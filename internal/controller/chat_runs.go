package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"shanyangcode.com/pilot/internal/ai/multiagent"
	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/internal/httpapi"
	"shanyangcode.com/pilot/internal/memory"
)

type runStore interface {
	SaveAgentRun(context.Context, memory.AgentRun) error
	GetAgentRun(context.Context, string, string, string) (memory.AgentRun, error)
	GetSessionRuns(context.Context, string, string) ([]memory.AgentRun, error)
}
type runRegistry struct {
	mu    sync.Mutex
	items map[string]*chatRun
}

func newRunRegistry() *runRegistry { return &runRegistry{items: make(map[string]*chatRun)} }

type chatRun struct {
	mu                         sync.Mutex
	persistMu                  sync.Mutex
	record                     memory.AgentRun
	events                     []progress.Event
	notify                     chan struct{}
	cancel                     context.CancelFunc
	answerStarted, answerEnded bool
	deleted                    bool
}

func terminal(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "interrupted", "rejected", "processing":
		return true
	}
	return false
}
func (r *chatRun) append(event progress.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleted || terminal(r.record.Status) {
		return
	}
	event.Summary = progress.Clip(progress.Redact(event.Summary), 600)
	if event.Parameters != nil {
		event.Parameters, _ = progress.PublicValue(event.Parameters).(map[string]any)
	}
	if event.ResultPreview != "" {
		_, _, event.ResultPreview = progress.ToolResult(event.ResultPreview)
	}
	if event.Result != nil {
		event.Result = progress.PublicValue(event.Result)
	}
	if event.Kind == "run" && event.Status == "completed" && (!r.answerEnded || strings.TrimSpace(r.record.Answer) == "") {
		event.Status, event.Summary = "failed", "未收到完整的最终回答"
	}
	if event.Kind == "answer" {
		switch event.Status {
		case "started":
			r.answerStarted = true
			r.answerEnded = false
			r.record.Status = "final_streaming"
		case "delta":
			if !r.answerStarted || r.answerEnded {
				return
			}
			r.record.Answer += event.Text
		case "replace":
			r.record.Answer = event.Text
		case "ended":
			if !r.answerStarted {
				return
			}
			r.answerEnded = true
		}
	}
	if event.Kind == "run" {
		r.record.Status = event.Status
	}
	event.RunID, event.Sequence = r.record.ID, uint64(len(r.events)+1)
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	r.events = append(r.events, event)
	r.record.UpdatedAt = time.Now()
	close(r.notify)
	r.notify = make(chan struct{})
}
func (r *chatRun) snapshot() memory.AgentRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.record
	data, _ := json.Marshal(r.events)
	record.EventsJSON = string(data)
	return record
}
func (r *chatRun) replay(after uint64) ([]progress.Event, <-chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if after > uint64(len(r.events)) {
		after = uint64(len(r.events))
	}
	return append([]progress.Event(nil), r.events[after:]...), r.notify, terminal(r.record.Status)
}
func (c *ChatController) store() runStore { store, _ := c.historyManager.(runStore); return store }
func (c *ChatController) saveRun(r *chatRun) error {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	r.mu.Lock()
	deleted := r.deleted
	r.mu.Unlock()
	if deleted {
		return nil
	}
	if store := c.store(); store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return store.SaveAgentRun(ctx, r.snapshot())
	}
	return nil
}

// Persist the terminal journal before publishing it. The persistence mutex also
// prevents an older periodic snapshot from overwriting this terminal record.
func (c *ChatController) finishRun(r *chatRun, status, summary string) {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleted || terminal(r.record.Status) {
		return
	}
	event := progress.RunEvent(status, summary)
	event.RunID, event.Sequence = r.record.ID, uint64(len(r.events)+1)
	r.record.Status, r.record.UpdatedAt = status, time.Now()
	r.events = append(r.events, event)
	if store := c.store(); store != nil {
		data, _ := json.Marshal(r.events)
		record := r.record
		record.EventsJSON = string(data)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := store.SaveAgentRun(ctx, record)
		cancel()
		if err != nil {
			slog.Error("save terminal run failed", "run_id", record.ID, "error", err)
			r.record.Status = "failed"
			r.events[len(r.events)-1].Status = "failed"
			r.events[len(r.events)-1].Summary = "执行记录保存失败，已保留页面中的回答"
		}
	}
	close(r.notify)
	r.notify = make(chan struct{})
}

func (c *ChatController) dropSessionRuns(userID, sessionID string) {
	c.runs.mu.Lock()
	defer c.runs.mu.Unlock()
	for id, r := range c.runs.items {
		record := r.snapshot()
		if record.UserID != userID || record.SessionID != sessionID {
			continue
		}
		r.persistMu.Lock()
		if r.cancel != nil {
			r.cancel()
		}
		r.append(progress.RunEvent("cancelled", "会话已清除"))
		r.mu.Lock()
		r.deleted = true
		r.mu.Unlock()
		r.persistMu.Unlock()
		delete(c.runs.items, id)
	}
}
func (c *ChatController) findRun(ctx context.Context, id, userID, sessionID string) (*chatRun, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, gorm.ErrRecordNotFound
	}
	id = parsed.String()
	c.runs.mu.Lock()
	r := c.runs.items[id]
	c.runs.mu.Unlock()
	if r != nil {
		record := r.snapshot()
		if record.UserID != userID || record.SessionID != sessionID {
			return nil, gorm.ErrRecordNotFound
		}
		return r, nil
	}
	if c.store() == nil {
		return nil, gorm.ErrRecordNotFound
	}
	record, err := c.store().GetAgentRun(ctx, id, userID, sessionID)
	if err != nil {
		return nil, err
	}
	if record.UserID != userID || record.SessionID != sessionID {
		return nil, gorm.ErrRecordNotFound
	}
	r = &chatRun{record: record, notify: make(chan struct{})}
	if err := json.Unmarshal([]byte(record.EventsJSON), &r.events); err != nil {
		return nil, err
	}
	if !terminal(record.Status) {
		// A journal does not resurrect an execution engine after a server restart.
		r.append(progress.RunEvent("failed", "服务已重启，本次执行中断；已保留接收到的内容"))
		if err := c.saveRun(r); err != nil {
			return nil, err
		}
	}
	return r, nil
}
func (c *ChatController) startRun(g *gin.Context, req *ChatRequest, mode string, resume *ResumeRequest) {
	//校验请求
	if err := normalizeChatRequest(req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	if req.RunID == "" {
		req.RunID = uuid.NewString()
	}
	if _, err := uuid.Parse(req.RunID); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("run_id 必须是 UUID"))
		return
	}
	parsedID, _ := uuid.Parse(req.RunID)
	req.RunID = parsedID.String()
	//用于审批恢复
	if req.ParentRunID != "" {
		parent, err := c.findRun(g.Request.Context(), req.ParentRunID, req.UserID, req.SessionID)
		if err != nil || mode != "resume" || resume == nil {
			httpapi.Fail(g, httpapi.InvalidArgument("审批记录与当前会话不匹配"))
			return
		}
		events, _, _ := parent.replay(0)
		matched := false
		for _, event := range events {
			if event.Kind == "result" {
				if result, ok := event.Result.(map[string]any); ok {
					if meta, ok := result["metadata"].(map[string]any); ok && meta["checkpoint_id"] == resume.CheckpointID {
						matched = true
					}
				}
			}
		}
		if !matched {
			httpapi.Fail(g, httpapi.InvalidArgument("checkpoint_id 与审批记录不匹配"))
			return
		}
	}
	// POST retries are idempotent. Reconnects normally use the replay GET endpoint.
	r, err := c.findRun(g.Request.Context(), req.RunID, req.UserID, req.SessionID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		httpapi.Fail(g, httpapi.DependencyUnavailable("执行记录读取失败"))
		return
	}
	if r != nil {
		record := r.snapshot()
		if record.Query != req.Query || record.Mode != mode {
			httpapi.Fail(g, httpapi.InvalidArgument("run_id 已用于另一个请求"))
			return
		}
		//接入原任务
		c.serveRun(g, r)
		return
	}
	base, stop := context.WithTimeout(context.Background(), 10*time.Minute)
	ctx, cancel := c.agentContext(base)
	//增加ctx信息
	ctx = multiagent.WithRequestWindowMinutes(
		progress.WithRunID(progress.EnableStreaming(ctx), req.RunID),
		req.DurationMinutes,
	)
	//任务的内存对象
	r = &chatRun{
		record: memory.AgentRun{ID: req.RunID, ParentRunID: req.ParentRunID, UserID: req.UserID, SessionID: req.SessionID, Query: req.Query, Mode: mode, Status: "executing", CreatedAt: time.Now()},
		notify: make(chan struct{}),
		cancel: func() {
			cancel()
			stop()
		}}
	//增加执行状态
	r.append(progress.RunEvent("executing", "正在分析请求"))
	c.runs.mu.Lock()
	if existing := c.runs.items[req.RunID]; existing != nil {
		c.runs.mu.Unlock()
		r.cancel()
		existingRecord := existing.snapshot()
		if existingRecord.UserID != req.UserID || existingRecord.SessionID != req.SessionID || existingRecord.Query != req.Query || existingRecord.Mode != mode {
			httpapi.Fail(g, httpapi.InvalidArgument("run_id 冲突"))
			return
		}
		c.serveRun(g, existing)
		return
	}
	//保存任务记录
	if err := c.saveRun(r); err != nil {
		c.runs.mu.Unlock()
		r.cancel()
		httpapi.Fail(g, httpapi.DependencyUnavailable("执行记录保存失败"))
		return
	}
	c.runs.items[req.RunID] = r
	// Keep active runs; terminal records remain available from durable storage.
	if c.store() != nil {
		for id, item := range c.runs.items {
			if id != req.RunID && terminal(item.snapshot().Status) && time.Since(item.snapshot().UpdatedAt) > 30*time.Minute {
				delete(c.runs.items, id)
			}
		}
	}
	c.runs.mu.Unlock()
	ctx = progress.WithEmitter(ctx, r.append)
	copyReq := *req
	go c.executeRun(ctx, r, &copyReq, mode, resume)
	c.serveRun(g, r)
}
func (c *ChatController) executeRun(ctx context.Context, r *chatRun, req *ChatRequest, mode string, resume *ResumeRequest) {
	defer r.cancel()
	stopSave, saved := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(saved)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopSave:
				return
			case <-ticker.C:
				if err := c.saveRun(r); err != nil {
					slog.Error("save active run failed", "run_id", req.RunID, "error", err)
				}
			}
		}
	}()
	defer func() {
		if failure := recover(); failure != nil {
			slog.Error("agent run panic", "error", failure)
			r.append(progress.RunEvent("failed", "执行发生异常"))
		}
		close(stopSave)
		<-saved
		if err := c.saveRun(r); err != nil {
			slog.Error("save final run failed", "run_id", req.RunID, "error", err)
		} else if c.onRunFinished != nil {
			// This hook enqueues a durable job only. Never run the knowledge LLM
			// under a run mutex or inside the response event stream.
			if err := c.onRunFinished(context.Background(), r.snapshot()); err != nil {
				slog.Warn("enqueue knowledge failed; periodic recovery will retry", "run_id", req.RunID, "error", err)
			}
		}
	}()
	fail := func(err error) {
		status, summary := "failed", "执行失败，已保留接收到的内容"
		if errors.Is(err, context.Canceled) {
			status, summary = "cancelled", "已取消"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			summary = "执行超时，已保留接收到的内容"
		}
		slog.Error("agent run failed", "run_id", req.RunID, "error", err)
		r.append(progress.Event{Kind: "model", ID: "execution-error", Name: "execution", Status: "failed", Summary: "执行未能继续", ResultPreview: progress.Clip(progress.Redact(err.Error()), 1200), GroupID: "synthesis"})
		r.append(progress.RunEvent(status, summary))
	}
	var multiResult *multiagent.Result
	finalFailed := false
	if mode == "stream" {
		reader, err := c.agent.Stream(ctx, req.UserID, req.SessionID, req.Query)
		if err != nil {
			fail(err)
			return
		}
		defer reader.Close()
		//循环读取agent输出
		for {
			chunk, err := reader.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fail(err)
				return
			}
			if event, ok := progress.FromMessage(chunk); ok {
				r.append(event)
				continue
			}
			// Stream producers must explicitly label their final answer. An old or
			// broken adapter is an error, never a text-based guess.
			if chunk != nil && chunk.Content != "" {
				fail(errors.New("unclassified text in task stream"))
				return
			}
		}
	} else {
		var result multiagent.Result
		var err error
		if mode == "resume" {
			result, err = c.multiAgentResume.Resume(ctx, resume.CheckpointID, resume.Approve)
		} else {
			result, err = c.multiAgent.Execute(ctx, req.UserID, req.SessionID, req.Query)
		}
		if err != nil {
			fail(err)
			return
		}
		if result.Metadata == nil {
			result.Metadata = map[string]any{}
		}
		result.Metadata["interaction_id"] = req.RunID
		multiResult = &result
		if mode == "resume" {
			if query, _ := result.Metadata["original_query"].(string); query != "" {
				req.Query = query
			}
		}
		status, _ := result.Metadata["status"].(string)
		r.append(progress.Event{Kind: "result", Status: status, Result: progress.PublicValue(map[string]any{"metadata": result.Metadata, "verification": result.Verification, "limitations": result.Limitations})})
		if status == "interrupted" || status == "rejected" || status == "processing" {
			c.recordMultiAgentInteraction(ctx, req.RunID, req, result, status)
			r.append(progress.RunEvent(status, map[string]string{"interrupted": "等待审批", "rejected": "已拒绝继续执行", "processing": "另一请求正在处理审批"}[status]))
			return
		}
		if r.snapshot().Answer != result.Answer {
			// Preserve the evidence verifier: replace a rejected draft with the
			// existing deterministic summary, visibly label that correction.
			if !r.answerStarted {
				progress.StartAnswer(ctx)
			}
			r.append(progress.Event{Kind: "answer", Status: "replace", Text: result.Answer, Summary: "证据校验后返回确认的结果"})
			r.append(progress.Event{Kind: "answer", Status: "ended"})
		}
		if result.Verification != nil && result.Verification.Decision == "rejected" {
			finalFailed = true
			r.append(progress.Event{Kind: "notice", Status: "warning", Summary: "生成的诊断未通过证据校验，已显示证据摘要"})
		}
		for _, limitation := range result.Limitations {
			if limitation.Code == "SYNTHESIS_FAILED" {
				finalFailed = true
				r.append(progress.Event{Kind: "notice", Status: "warning", Summary: "最终回答生成失败，已显示确定性的证据摘要"})
			}
		}
	}
	if ctx.Err() != nil {
		fail(ctx.Err())
		return
	}
	record := r.snapshot()
	if terminal(record.Status) {
		return
	}
	if !r.answerEnded || strings.TrimSpace(record.Answer) == "" {
		fail(errors.New("answer end missing"))
		return
	}
	c.persistTurn(ctx, req.UserID, req.SessionID, req.Query, record.Answer, persistTurnOptions{RunID: req.RunID, SaveHistory: true, SaveShortTermUser: mode != "stream", SaveShortTermAssistant: true})
	if multiResult != nil {
		interactionStatus := "completed"
		if finalFailed {
			interactionStatus = "failed"
		}
		if multiResult.Verification != nil {
			switch multiResult.Verification.Decision {
			case "abstain", "rejected":
				interactionStatus = multiResult.Verification.Decision
			}
		}
		c.recordMultiAgentInteraction(ctx, req.RunID, req, *multiResult, interactionStatus)
	} else {
		c.recordInteraction(ctx, memory.InteractionRecord{ID: req.RunID, UserID: req.UserID, SessionID: req.SessionID, Mode: mode, Status: "success", Query: req.Query, Answer: record.Answer})
	}
	if finalFailed {
		c.finishRun(r, "failed", "生成或校验未通过 · 已保留证据摘要")
	} else {
		c.finishRun(r, "completed", "已完成")
	}
}
func (c *ChatController) serveRun(g *gin.Context, r *chatRun) {
	g.Header("X-Pilot-Run-ID", r.record.ID)
	g.Header("X-Pilot-Interaction-ID", r.record.ID)
	httpapi.SetSSEHeaders(g)
	after, _ := strconv.ParseUint(g.Query("after"), 10, 64)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		events, notify, done := r.replay(after)
		for _, event := range events {
			data, _ := json.Marshal(event)
			if _, err := g.Writer.WriteString(fmt.Sprintf("id: %s:%d\nevent: agent\n", event.RunID, event.Sequence) + httpapi.FormatSSEDataBlock(string(data))); err != nil {
				return
			}
			g.Writer.Flush()
			after = event.Sequence
		}
		if done {
			httpapi.WriteSSEData(g, httpapi.SSEDoneSentinel)
			return
		}
		select {
		case <-g.Request.Context().Done():
			return
		case <-notify:
		case <-heartbeat.C:
			_, _ = g.Writer.WriteString(": heartbeat\n\n")
			g.Writer.Flush()
		}
	}
}
func (c *ChatController) RunEvents(g *gin.Context) {
	r, err := c.findRun(g.Request.Context(), g.Param("run_id"), g.Query("user_id"), g.Query("session_id"))
	if err != nil {
		httpapi.Fail(g, httpapi.NotFound("执行记录不存在"))
		return
	}
	c.serveRun(g, r)
}
func (c *ChatController) CancelRun(g *gin.Context) {
	r, err := c.findRun(g.Request.Context(), g.Param("run_id"), g.Query("user_id"), g.Query("session_id"))
	if err != nil {
		httpapi.Fail(g, httpapi.NotFound("执行记录不存在"))
		return
	}
	if r.cancel != nil {
		r.cancel()
	}
	r.append(progress.RunEvent("cancelled", "已取消"))
	if err := c.saveRun(r); err != nil {
		httpapi.Fail(g, httpapi.DependencyUnavailable("取消状态保存失败"))
		return
	}
	httpapi.OK(g, "已停止执行", r.snapshot().Status)
}
func (c *ChatController) RunHistory(g *gin.Context) {
	userID, sessionID, err := normalizeHistoryParams(g.Query("user_id"), g.Query("session_id"))
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	var records []memory.AgentRun
	if store := c.store(); store != nil {
		records, err = store.GetSessionRuns(g.Request.Context(), userID, sessionID)
		if err != nil {
			httpapi.Fail(g, httpapi.DependencyUnavailable("执行历史读取失败"))
			return
		}
	} else {
		c.runs.mu.Lock()
		for _, r := range c.runs.items {
			record := r.snapshot()
			if record.UserID == userID && record.SessionID == sessionID {
				records = append(records, record)
			}
		}
		c.runs.mu.Unlock()
	}
	items := make([]map[string]any, 0, len(records))
	for _, record := range records {
		r, err := c.findRun(g.Request.Context(), record.ID, userID, sessionID)
		if err != nil {
			httpapi.Fail(g, httpapi.DependencyUnavailable("执行记录损坏或读取失败"))
			return
		}
		events, _, _ := r.replay(0)
		snap := r.snapshot()
		items = append(items, map[string]any{"id": record.ID, "parent_run_id": record.ParentRunID, "query": record.Query, "answer": snap.Answer, "status": snap.Status, "mode": record.Mode, "created_at": record.CreatedAt, "events": events})
	}
	httpapi.OK(g, "获取执行记录成功", items)
}
