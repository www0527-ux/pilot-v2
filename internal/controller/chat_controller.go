package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"shanyangcode.com/pilot/internal/ai/multiagent"
	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/internal/httpapi"
	"shanyangcode.com/pilot/internal/identity"
	"shanyangcode.com/pilot/internal/memory"
	"shanyangcode.com/pilot/internal/observability"
	"shanyangcode.com/pilot/pkg/types"
)

// ChatAgent 是聊天控制器依赖的对话 Agent 能力。
type ChatAgent interface {
	Generate(ctx context.Context, userID, sessionID, query string) (*schema.Message, error)
	Stream(ctx context.Context, userID, sessionID, query string) (*schema.StreamReader[*schema.Message], error)
}

// MultiAgentExecutor 是多 Agent 排障工作流入口。
type MultiAgentExecutor interface {
	Execute(ctx context.Context, userID, sessionID, query string) (multiagent.Result, error)
}

// MultiAgentResumer optionally resumes a HITL-interrupted multi-agent run.
type MultiAgentResumer interface {
	Resume(ctx context.Context, checkpointID string, approve bool) (multiagent.Result, error)
}

// ShortTermStore 是短期记忆读写能力。
type ShortTermStore interface {
	SaveMessage(ctx context.Context, userID, sessionID string, message types.ChatMessage) error
	GetMessages(ctx context.Context, userID, sessionID string) ([]types.ChatMessage, error)
	ClearMemory(ctx context.Context, userID, sessionID string) error
}

// HistoryStore 是长期会话历史读写能力。
type HistoryStore interface {
	SaveMessage(ctx context.Context, userID, sessionID, role, content string) error
	GetSessionHistory(ctx context.Context, userID, sessionID string, limit int) ([]memory.ConversationHistory, error)
	DeleteSessionHistory(ctx context.Context, userID, sessionID string) error
}

// ChatController 聊天控制器
type ChatController struct {
	agent            ChatAgent
	multiAgent       MultiAgentExecutor
	multiAgentResume MultiAgentResumer
	shortTermMemory  ShortTermStore
	historyManager   HistoryStore
	agentTimeout     AgentTimeout
	feedback         FeedbackStore
	runs             *runRegistry
	onRunFinished    func(context.Context, memory.AgentRun) error
}

type FeedbackStore interface {
	RecordInteraction(ctx context.Context, record memory.InteractionRecord) error
	Submit(ctx context.Context, interactionID, userID, sessionID, vote, improvement string) (memory.FeedbackOutcome, error)
}

// AgentTimeout 约束单次 Agent 执行的最长时间；零值表示不限制。
type AgentTimeout = func(ctx context.Context) (context.Context, context.CancelFunc)

// NewChatController 创建聊天控制器；multiAgent 传 nil 表示多 Agent 模式未启用。
func NewChatController(agent ChatAgent, multiAgent MultiAgentExecutor, shortTerm ShortTermStore, history HistoryStore) *ChatController {
	return &ChatController{
		agent:           agent,
		multiAgent:      multiAgent,
		shortTermMemory: shortTerm,
		historyManager:  history,
		runs:            newRunRegistry(),
	}
}

// WithAgentTimeout 配置 Agent 执行超时包装器（由装配方基于配置注入）。
func (c *ChatController) WithAgentTimeout(wrap AgentTimeout) *ChatController {
	c.agentTimeout = wrap
	return c
}

// WithMultiAgentResumer enables HITL resume endpoint wiring.
func (c *ChatController) WithMultiAgentResumer(resumer MultiAgentResumer) *ChatController {
	c.multiAgentResume = resumer
	return c
}

func (c *ChatController) WithFeedbackStore(store FeedbackStore) *ChatController {
	c.feedback = store
	return c
}

func (c *ChatController) WithRunFinished(fn func(context.Context, memory.AgentRun) error) *ChatController {
	c.onRunFinished = fn
	return c
}

func (c *ChatController) agentContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.agentTimeout == nil {
		return ctx, func() {}
	}
	return c.agentTimeout(ctx)
}

// ChatRequest 聊天请求
type ChatRequest struct {
	UserID          string `json:"user_id"`
	SessionID       string `json:"session_id"`
	Query           string `json:"query"`
	DurationMinutes int    `json:"duration_minutes"`
	IncludeProgress bool   `json:"include_progress"`
	RunID           string `json:"run_id"`
	ParentRunID     string `json:"parent_run_id"`
}

// bindChatRequest 解析并规范化聊天请求。
func bindChatRequest(c *gin.Context) (*ChatRequest, bool) {
	var req ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpapi.Fail(c, httpapi.InvalidArgument("请求体不是合法 JSON"))
		return nil, false
	}
	if owner := identity.Owner(c.Request.Context()); owner != "" {
		req.UserID = owner
	}
	if err := normalizeChatRequest(&req); err != nil {
		httpapi.Fail(c, httpapi.InvalidArgument(err.Error()))
		return nil, false
	}
	if req.DurationMinutes > 0 {
		req.DurationMinutes = observability.NormalizeDurationMinutes(req.DurationMinutes)
	}
	return &req, true
}

// Chat 普通对话
func (c *ChatController) Chat(g *gin.Context) {
	req, ok := bindChatRequest(g)
	if !ok {
		return
	}

	reqCtx := g.Request.Context()
	slog.InfoContext(reqCtx, "chat request",
		"user_id", req.UserID, "session_id", req.SessionID, "query_length", len([]rune(req.Query)))

	agentCtx, cancel := c.agentContext(reqCtx)
	defer cancel()
	agentCtx = multiagent.WithRequestWindowMinutes(agentCtx, req.DurationMinutes)
	interactionID := uuid.NewString()
	agentCtx = progress.WithRunID(agentCtx, interactionID)

	// 调用 Agent 生成响应
	response, err := c.agent.Generate(agentCtx, req.UserID, req.SessionID, req.Query)
	if err != nil {
		slog.ErrorContext(reqCtx, "agent generate failed", "error", err)
		httpapi.Fail(g, httpapi.UpstreamError("生成响应失败，请稍后重试"))
		return
	}

	// 短期记忆由 FullPilotAgent 写入；此处只落长期历史。
	c.persistTurn(reqCtx, req.UserID, req.SessionID, req.Query, response.Content, persistTurnOptions{
		SaveHistory: true,
		RunID:       interactionID,
	})
	c.recordInteraction(reqCtx, memory.InteractionRecord{ID: interactionID, UserID: req.UserID, SessionID: req.SessionID, Mode: "chat", Status: "success", Query: req.Query, Answer: response.Content})
	g.Header("X-Pilot-Interaction-ID", interactionID)
	g.Header("X-Pilot-Run-ID", interactionID)
	if store := c.store(); store != nil {
		journal := memory.AgentRun{ID: interactionID, UserID: req.UserID, SessionID: req.SessionID, Query: req.Query, Answer: response.Content, Mode: "chat", Status: "completed", EventsJSON: "[]", CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if err := store.SaveAgentRun(reqCtx, journal); err != nil {
			slog.WarnContext(reqCtx, "save synchronous run failed", "error", err)
		} else if c.onRunFinished != nil {
			if err := c.onRunFinished(reqCtx, journal); err != nil {
				slog.WarnContext(reqCtx, "enqueue synchronous knowledge failed", "error", err)
			}
		}
	}

	httpapi.OK(g, "对话成功", response.Content)
}

// MultiAgentChat executes the optional role-based incident-analysis workflow.
func (c *ChatController) MultiAgentChat(g *gin.Context) {
	if c.multiAgent == nil {
		httpapi.Fail(g, httpapi.FeatureDisabled("多 Agent 模式未启用"))
		return
	}

	req, ok := bindChatRequest(g)
	if !ok {
		return
	}
	if req.IncludeProgress {
		c.startRun(g, req, "multi-agent", nil)
		return
	}

	reqCtx := g.Request.Context()
	slog.InfoContext(reqCtx, "multi-agent chat request",
		"user_id", req.UserID, "session_id", req.SessionID, "query_length", len([]rune(req.Query)))

	agentCtx, cancel := c.agentContext(reqCtx)
	defer cancel()
	agentCtx = multiagent.WithRequestWindowMinutes(agentCtx, req.DurationMinutes)

	result, err := c.multiAgent.Execute(agentCtx, req.UserID, req.SessionID, req.Query)
	if err != nil {
		slog.ErrorContext(reqCtx, "multi-agent execute failed", "error", err)
		httpapi.Fail(g, httpapi.UpstreamError("多 Agent 分析失败，请稍后重试"))
		return
	}

	interactionID := uuid.NewString()
	g.Header("X-Pilot-Interaction-ID", interactionID)
	if c.saveMultiAgentResult(reqCtx, interactionID, req, &result) {
		httpapi.OK(g, "多 Agent 等待人工审批", result)
		return
	}

	httpapi.OK(g, "多 Agent 分析完成", result)
}

// ResumeRequest resumes a HITL checkpoint.
type ResumeRequest struct {
	CheckpointID    string `json:"checkpoint_id"`
	Approve         bool   `json:"approve"`
	IncludeProgress bool   `json:"include_progress"`
	RunID           string `json:"run_id"`
	UserID          string `json:"user_id"`
	SessionID       string `json:"session_id"`
	ParentRunID     string `json:"parent_run_id"`
}

// MultiAgentResume continues an interrupted multi-agent run.
func (c *ChatController) MultiAgentResume(g *gin.Context) {
	if c.multiAgentResume == nil {
		httpapi.Fail(g, httpapi.FeatureDisabled("HITL resume 未启用"))
		return
	}
	var req ResumeRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("请求体不是合法 JSON"))
		return
	}
	if owner := identity.Owner(g.Request.Context()); owner != "" {
		req.UserID = owner
	}
	req.CheckpointID = strings.TrimSpace(req.CheckpointID)
	if req.CheckpointID == "" {
		httpapi.Fail(g, httpapi.InvalidArgument("checkpoint_id 不能为空"))
		return
	}
	if req.IncludeProgress {
		c.startRun(g, &ChatRequest{UserID: req.UserID, SessionID: req.SessionID, Query: "继续已审批的排障", RunID: req.RunID, ParentRunID: req.ParentRunID}, "resume", &req)
		return
	}
	reqCtx := g.Request.Context()
	agentCtx, cancel := c.agentContext(reqCtx)
	defer cancel()
	result, err := c.multiAgentResume.Resume(agentCtx, req.CheckpointID, req.Approve)
	if err != nil {
		slog.ErrorContext(reqCtx, "multi-agent resume failed", "error", err)
		httpapi.Fail(g, httpapi.UpstreamError("恢复多 Agent 执行失败，请稍后重试"))
		return
	}
	status, _ := result.Metadata["status"].(string)
	if status == multiagent.CheckpointRejected || status == multiagent.CheckpointProcessing {
		message := "多 Agent 审批已拒绝"
		if status == multiagent.CheckpointProcessing {
			message = "多 Agent 正在由另一个恢复请求处理"
		}
		httpapi.OK(g, message, result)
		return
	}
	interactionID := uuid.NewString()
	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	result.Metadata["interaction_id"] = interactionID
	g.Header("X-Pilot-Interaction-ID", interactionID)
	if strings.TrimSpace(result.Answer) != "" {
		userID, _ := result.Metadata["original_user_id"].(string)
		sessionID, _ := result.Metadata["original_session_id"].(string)
		query, _ := result.Metadata["original_query"].(string)
		c.persistTurn(reqCtx, userID, sessionID, query, result.Answer, persistTurnOptions{
			SaveShortTermUser:      true,
			SaveShortTermAssistant: true,
			SaveHistory:            true,
		})
		c.recordMultiAgentInteraction(reqCtx, interactionID, &ChatRequest{UserID: userID, SessionID: sessionID, Query: query}, result, "completed")
	}
	httpapi.OK(g, "多 Agent 已恢复", result)
}

// StreamChat starts a task and serves its structured SSE events.
func (c *ChatController) StreamChat(g *gin.Context) {
	req, ok := bindChatRequest(g)
	if !ok {
		return
	}
	c.startRun(g, req, "stream", nil)
}

type FeedbackRequest struct {
	InteractionID string `json:"interaction_id"`
	UserID        string `json:"user_id"`
	SessionID     string `json:"session_id"`
	Vote          string `json:"vote"`
	Improvement   string `json:"improvement"`
}

func (c *ChatController) SubmitFeedback(g *gin.Context) {
	if c.feedback == nil {
		httpapi.Fail(g, httpapi.FeatureDisabled("反馈功能未启用"))
		return
	}
	var req FeedbackRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("请求体不是合法 JSON"))
		return
	}
	if owner := identity.Owner(g.Request.Context()); owner != "" {
		req.UserID = owner
	}
	req.InteractionID = strings.TrimSpace(req.InteractionID)
	req.UserID = strings.TrimSpace(req.UserID)
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Vote = strings.ToLower(strings.TrimSpace(req.Vote))
	if req.InteractionID == "" || req.UserID == "" || req.SessionID == "" || (req.Vote != memory.FeedbackUp && req.Vote != memory.FeedbackDown) {
		httpapi.Fail(g, httpapi.InvalidArgument("interaction_id、user_id、session_id 必填，vote 仅支持 up/down"))
		return
	}
	outcome, err := c.feedback.Submit(g.Request.Context(), req.InteractionID, req.UserID, req.SessionID, req.Vote, req.Improvement)
	if err != nil {
		slog.WarnContext(g.Request.Context(), "submit interaction feedback failed", "error", err, "interaction_id", req.InteractionID)
		if errors.Is(err, memory.ErrInteractionNotFound) {
			httpapi.Fail(g, httpapi.NotFound("interaction 不存在或不属于当前会话"))
		} else {
			httpapi.Fail(g, httpapi.DependencyUnavailable("反馈保存失败，请稍后重试"))
		}
		return
	}
	httpapi.OK(g, "反馈已保存", outcome)
}

func (c *ChatController) recordInteraction(ctx context.Context, record memory.InteractionRecord) {
	if c.feedback == nil {
		return
	}
	if err := c.feedback.RecordInteraction(ctx, record); err != nil {
		slog.WarnContext(ctx, "record interaction failed; response remains available", "error", err, "interaction_id", record.ID)
	}
}

func (c *ChatController) recordMultiAgentInteraction(ctx context.Context, interactionID string, req *ChatRequest, result multiagent.Result, status string) {
	snapshot, err := json.Marshal(result)
	if err != nil {
		slog.WarnContext(ctx, "marshal multi-agent interaction snapshot failed", "error", err, "interaction_id", interactionID)
	}
	c.recordInteraction(ctx, memory.InteractionRecord{ID: interactionID, UserID: req.UserID, SessionID: req.SessionID, Mode: "multi-agent", Status: status, Query: req.Query, Answer: result.Answer, Snapshot: string(snapshot)})
}

// ClearHistory 清除对话历史
func (c *ChatController) ClearHistory(g *gin.Context) {
	userID, sessionID, err := normalizeHistoryParams(g.Query("user_id"), g.Query("session_id"))
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	reqCtx := g.Request.Context()

	// 清除短期记忆
	c.dropSessionRuns(userID, sessionID)
	if err := c.shortTermMemory.ClearMemory(reqCtx, userID, sessionID); err != nil {
		slog.ErrorContext(reqCtx, "clear short-term memory failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("清除短期历史失败，请稍后重试"))
		return
	}

	if err := c.historyManager.DeleteSessionHistory(reqCtx, userID, sessionID); err != nil {
		slog.ErrorContext(reqCtx, "clear mysql session history failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("清除长期历史失败，请稍后重试"))
		return
	}

	httpapi.OK(g, "清除历史成功", nil)
}

// GetHistory 获取对话历史
func (c *ChatController) GetHistory(g *gin.Context) {
	userID, sessionID, err := normalizeHistoryParams(g.Query("user_id"), g.Query("session_id"))
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	limit := normalizeHistoryLimit(parseIntDefault(g.Query("limit"), 0))
	reqCtx := g.Request.Context()

	records, err := c.historyManager.GetSessionHistory(reqCtx, userID, sessionID, limit)
	if err != nil {
		slog.ErrorContext(reqCtx, "get mysql session history failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("获取长期历史失败，请稍后重试"))
		return
	}

	messages := buildChatMessagesFromHistory(records)
	if len(messages) == 0 {
		messages, err = c.shortTermMemory.GetMessages(reqCtx, userID, sessionID)
		if err != nil {
			slog.ErrorContext(reqCtx, "get short-term session history failed", "error", err)
			httpapi.Fail(g, httpapi.DependencyUnavailable("获取短期历史失败，请稍后重试"))
			return
		}
		if len(messages) > limit {
			messages = messages[len(messages)-limit:]
		}
	}

	httpapi.OK(g, "获取历史成功", messages)
}
