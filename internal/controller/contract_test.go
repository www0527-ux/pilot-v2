package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"

	"shanyangcode.com/pilot/internal/ai/multiagent"
	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/internal/ai/retriever"
	"shanyangcode.com/pilot/internal/httpapi"
	"shanyangcode.com/pilot/internal/memory"
	"shanyangcode.com/pilot/pkg/types"
)

// ---- 假依赖 ----

type fakeAgent struct {
	generateResult string
	generateErr    error
	streamChunks   []string
	streamErr      error
}

func (f *fakeAgent) Generate(ctx context.Context, userID, sessionID, query string) (*schema.Message, error) {
	if f.generateErr != nil {
		return nil, f.generateErr
	}
	return &schema.Message{Role: schema.Assistant, Content: f.generateResult}, nil
}

func (f *fakeAgent) Stream(ctx context.Context, userID, sessionID, query string) (*schema.StreamReader[*schema.Message], error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	messages := []*schema.Message{progress.Message(progress.Event{Kind: "answer", Status: "started"})}
	for _, chunk := range f.streamChunks {
		messages = append(messages, progress.Message(progress.Event{Kind: "answer", Status: "delta", Text: chunk}))
	}
	messages = append(messages, progress.Message(progress.Event{Kind: "answer", Status: "ended"}))
	reader, writer := schema.Pipe[*schema.Message](len(messages))
	go func() {
		defer writer.Close()
		for _, message := range messages {
			writer.Send(message, nil)
		}
	}()
	return reader, nil
}

type fakeMultiAgent struct {
	result     multiagent.Result
	err        error
	seenWindow int
}

func (f *fakeMultiAgent) Execute(ctx context.Context, userID, sessionID, query string) (multiagent.Result, error) {
	f.seenWindow = multiagent.RequestWindowMinutes(ctx)
	return f.result, f.err
}

type fakeShortTerm struct {
	saved    []types.ChatMessage
	messages []types.ChatMessage
	err      error
}

func (f *fakeShortTerm) SaveMessage(ctx context.Context, userID, sessionID string, message types.ChatMessage) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, message)
	return nil
}

func (f *fakeShortTerm) GetMessages(ctx context.Context, userID, sessionID string) ([]types.ChatMessage, error) {
	return f.messages, f.err
}

func (f *fakeShortTerm) ClearMemory(ctx context.Context, userID, sessionID string) error {
	return f.err
}

type fakeHistory struct {
	records []memory.ConversationHistory
	saveErr error
	getErr  error
}

func (f *fakeHistory) SaveMessage(ctx context.Context, userID, sessionID, role, content string) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.records = append(f.records, memory.ConversationHistory{
		UserID: userID, SessionID: sessionID, Role: role, Content: content, CreatedAt: time.Now(),
	})
	return nil
}

func (f *fakeHistory) GetSessionHistory(ctx context.Context, userID, sessionID string, limit int) ([]memory.ConversationHistory, error) {
	return f.records, f.getErr
}

func (f *fakeHistory) DeleteSessionHistory(ctx context.Context, userID, sessionID string) error {
	return f.getErr
}

type fakeKnowledge struct {
	docs      []types.KnowledgeDocument
	detailed  []retriever.RetrievedDocument
	indexErr  error
	searchErr error
}

func (f *fakeKnowledge) IndexDocument(ctx context.Context, doc *types.KnowledgeDocument) error {
	if f.indexErr != nil {
		return f.indexErr
	}
	doc.ID = "doc-1"
	doc.DocID = "doc-1"
	doc.ChunkCount = 1
	return nil
}

func (f *fakeKnowledge) Search(ctx context.Context, query string) ([]types.KnowledgeDocument, error) {
	return f.docs, f.searchErr
}

func (f *fakeKnowledge) SearchDetailed(ctx context.Context, query string) ([]retriever.RetrievedDocument, error) {
	return f.detailed, f.searchErr
}

func (f *fakeKnowledge) DeleteDocument(ctx context.Context, docID string) (int, error) {
	if f.searchErr != nil {
		return 0, f.searchErr
	}
	return 1, nil
}

type fakeCollector struct {
	alerts  []types.AlertInfo
	metrics []types.MetricData
	logs    []types.LogEntry
	err     error
}

type fakeFeedback struct {
	recorded []memory.InteractionRecord
	submits  int
}

func (f *fakeFeedback) RecordInteraction(_ context.Context, record memory.InteractionRecord) error {
	f.recorded = append(f.recorded, record)
	return nil
}

func (f *fakeFeedback) Submit(_ context.Context, interactionID, userID, sessionID, vote, improvement string) (memory.FeedbackOutcome, error) {
	f.submits++
	return memory.FeedbackOutcome{InteractionID: interactionID, Vote: vote, PromotionStatus: "promoted", Idempotent: f.submits > 1}, nil
}

func (f *fakeCollector) GetAlerts(ctx context.Context, filters map[string]string) ([]types.AlertInfo, error) {
	return f.alerts, f.err
}

func (f *fakeCollector) QueryMetrics(ctx context.Context, query string, startTime, endTime time.Time) ([]types.MetricData, error) {
	return f.metrics, f.err
}

func (f *fakeCollector) QueryLogs(ctx context.Context, service, level, keyword string, startTime, endTime time.Time, limit int) ([]types.LogEntry, error) {
	return f.logs, f.err
}

// ---- 测试基础设施 ----

type envelope struct {
	Success bool            `json:"success"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func newChatRouter(agent ChatAgent, multiAgent MultiAgentExecutor, shortTerm ShortTermStore, history HistoryStore) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	chat := NewChatController(agent, multiAgent, shortTerm, history)
	engine.POST("/api/v1/chat", chat.Chat)
	engine.POST("/api/v1/chat/", chat.Chat)
	engine.POST("/api/v1/chat/multi-agent", chat.MultiAgentChat)
	engine.POST("/api/v1/chat/stream", chat.StreamChat)
	engine.GET("/api/v1/chat/history", chat.GetHistory)
	engine.DELETE("/api/v1/chat/history", chat.ClearHistory)
	return engine
}

func doJSONRequest(t *testing.T, engine *gin.Engine, method, path string, body string) (*httptest.ResponseRecorder, envelope) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	var resp envelope
	if recorder.Header().Get("Content-Type") == "application/json; charset=utf-8" {
		if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode envelope failed: %v body=%s", err, recorder.Body.String())
		}
	}
	return recorder, resp
}

const validChatBody = `{"user_id":"u1","session_id":"s1","query":"排查 pilot-api 错误"}`

// ---- Chat 契约 ----

func TestChatSuccessEnvelope(t *testing.T) {
	engine := newChatRouter(&fakeAgent{generateResult: "结论：正常"}, nil, &fakeShortTerm{}, &fakeHistory{})

	for _, path := range []string{"/api/v1/chat", "/api/v1/chat/"} {
		recorder, resp := doJSONRequest(t, engine, http.MethodPost, path, validChatBody)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body=%s", path, recorder.Code, recorder.Body.String())
		}
		if !resp.Success || resp.Code != httpapi.CodeOK {
			t.Fatalf("%s: unexpected envelope %+v", path, resp)
		}
		var answer string
		if err := json.Unmarshal(resp.Data, &answer); err != nil || answer != "结论：正常" {
			t.Fatalf("%s: unexpected data %s", path, string(resp.Data))
		}
	}
}

func TestChatPersistsHistory(t *testing.T) {
	history := &fakeHistory{}
	engine := newChatRouter(&fakeAgent{generateResult: "ok"}, nil, &fakeShortTerm{}, history)

	doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat", validChatBody)
	if len(history.records) != 2 {
		t.Fatalf("history records = %d, want 2 (user + assistant)", len(history.records))
	}
	if history.records[0].Role != "user" || history.records[1].Role != "assistant" {
		t.Fatalf("unexpected roles: %+v", history.records)
	}
}

func TestChatValidationError(t *testing.T) {
	engine := newChatRouter(&fakeAgent{}, nil, &fakeShortTerm{}, &fakeHistory{})

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat", `{"user_id":"u1","session_id":"s1","query":"  "}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if resp.Code != httpapi.CodeInvalidArgument {
		t.Fatalf("code = %s, want INVALID_ARGUMENT", resp.Code)
	}
}

func TestChatMalformedJSON(t *testing.T) {
	engine := newChatRouter(&fakeAgent{}, nil, &fakeShortTerm{}, &fakeHistory{})

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat", `{"user_id":`)
	if recorder.Code != http.StatusBadRequest || resp.Code != httpapi.CodeInvalidArgument {
		t.Fatalf("status=%d code=%s, want 400 INVALID_ARGUMENT", recorder.Code, resp.Code)
	}
}

func TestChatAgentFailureMapsToUpstreamError(t *testing.T) {
	engine := newChatRouter(&fakeAgent{generateErr: errors.New("llm 500")}, nil, &fakeShortTerm{}, &fakeHistory{})

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat", validChatBody)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}
	if resp.Code != httpapi.CodeUpstreamError {
		t.Fatalf("code = %s, want UPSTREAM_ERROR", resp.Code)
	}
	if strings.Contains(resp.Message, "llm 500") {
		t.Fatal("internal error detail leaked to client")
	}
}

func TestMultiAgentDisabledReturnsFeatureDisabled(t *testing.T) {
	engine := newChatRouter(&fakeAgent{}, nil, &fakeShortTerm{}, &fakeHistory{})

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat/multi-agent", validChatBody)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if resp.Code != httpapi.CodeFeatureDisabled {
		t.Fatalf("code = %s, want FEATURE_DISABLED", resp.Code)
	}
}

func TestMultiAgentSuccessReturnsAuditableResult(t *testing.T) {
	result := multiagent.Result{
		Answer:        "结论",
		Plan:          multiagent.Plan{Intent: "incident_triage"},
		Steps:         []multiagent.Step{{Role: multiagent.RoleTriage, Status: multiagent.StatusCompleted}},
		EvidenceGraph: &multiagent.EvidenceGraph{Nodes: []multiagent.EvidenceNode{{ID: "alert:1", Source: "alerts", Kind: "evidence"}}},
		Verification: &multiagent.VerificationReport{
			Decision:   "accept",
			Assessment: multiagent.EvidenceAssessment{Score: 0.82, Band: "high"},
			Citations:  multiagent.CitationAudit{Passed: true, ValidIDs: []string{"alert:1"}},
		},
	}
	engine := newChatRouter(&fakeAgent{}, &fakeMultiAgent{result: result}, &fakeShortTerm{}, &fakeHistory{})

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat/multi-agent", validChatBody)
	if recorder.Code != http.StatusOK || !resp.Success {
		t.Fatalf("unexpected response: %d %+v", recorder.Code, resp)
	}
	var got multiagent.Result
	if err := json.Unmarshal(resp.Data, &got); err != nil {
		t.Fatalf("decode result failed: %v", err)
	}
	if got.Answer != "结论" || got.Plan.Intent != "incident_triage" || len(got.Steps) != 1 ||
		got.Verification == nil || got.Verification.Decision != "accept" || got.EvidenceGraph == nil {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestMultiAgentPersistsShortTermAndHistory(t *testing.T) {
	shortTerm := &fakeShortTerm{}
	history := &fakeHistory{}
	engine := newChatRouter(
		&fakeAgent{},
		&fakeMultiAgent{result: multiagent.Result{Answer: "结论"}},
		shortTerm,
		history,
	)

	doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat/multi-agent", validChatBody)

	if len(shortTerm.saved) != 2 {
		t.Fatalf("short-term saved = %d, want 2", len(shortTerm.saved))
	}
	if shortTerm.saved[0].Role != "user" || shortTerm.saved[1].Role != "assistant" {
		t.Fatalf("unexpected short-term roles: %+v", shortTerm.saved)
	}
	if shortTerm.saved[1].Content != "结论" {
		t.Fatalf("assistant content = %q, want 结论", shortTerm.saved[1].Content)
	}
	if len(history.records) != 2 {
		t.Fatalf("history records = %d, want 2", len(history.records))
	}
}

func TestMultiAgentPersistsDespiteShortTermFailure(t *testing.T) {
	history := &fakeHistory{}
	engine := newChatRouter(
		&fakeAgent{},
		&fakeMultiAgent{result: multiagent.Result{Answer: "结论"}},
		&fakeShortTerm{err: errors.New("redis down")},
		history,
	)

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat/multi-agent", validChatBody)
	if recorder.Code != http.StatusOK || !resp.Success {
		t.Fatalf("response should still succeed: %d %+v", recorder.Code, resp)
	}
	if len(history.records) != 2 {
		t.Fatalf("history records = %d, want 2 even when short-term fails", len(history.records))
	}
}

func TestStreamChatSSEProtocol(t *testing.T) {
	engine := newChatRouter(&fakeAgent{streamChunks: []string{"你好", "，世界"}}, nil, &fakeShortTerm{}, &fakeHistory{})

	recorder, _ := doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat/stream", validChatBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type = %q", got)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"text":"你好"`) || !strings.Contains(body, `"text":"，世界"`) || !strings.Contains(body, "event: agent\n") {
		t.Fatalf("missing chunks in SSE body: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("missing [DONE] terminator: %q", body)
	}
}

func TestStreamChatPersistsAssistantMessage(t *testing.T) {
	shortTerm := &fakeShortTerm{}
	engine := newChatRouter(&fakeAgent{streamChunks: []string{"a", "b"}}, nil, shortTerm, &fakeHistory{})

	doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat/stream", validChatBody)
	if len(shortTerm.saved) == 0 {
		t.Fatal("assistant message not saved to short-term memory")
	}
	last := shortTerm.saved[len(shortTerm.saved)-1]
	if last.Role != "assistant" || last.Content != "ab" {
		t.Fatalf("unexpected saved message: %+v", last)
	}
}

func TestGetHistoryFallsBackToShortTerm(t *testing.T) {
	shortTerm := &fakeShortTerm{messages: []types.ChatMessage{{Role: "user", Content: "hi"}}}
	engine := newChatRouter(&fakeAgent{}, nil, shortTerm, &fakeHistory{})

	recorder, resp := doJSONRequest(t, engine, http.MethodGet, "/api/v1/chat/history?user_id=u1&session_id=s1", "")
	if recorder.Code != http.StatusOK || !resp.Success {
		t.Fatalf("unexpected response: %d %+v", recorder.Code, resp)
	}
	var messages []types.ChatMessage
	if err := json.Unmarshal(resp.Data, &messages); err != nil || len(messages) != 1 {
		t.Fatalf("unexpected history payload: %s", string(resp.Data))
	}
}

func TestGetHistoryDependencyFailure(t *testing.T) {
	engine := newChatRouter(&fakeAgent{}, nil, &fakeShortTerm{}, &fakeHistory{getErr: errors.New("mysql down")})

	recorder, resp := doJSONRequest(t, engine, http.MethodGet, "/api/v1/chat/history?user_id=u1&session_id=s1", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if resp.Code != httpapi.CodeDependencyUnavailable {
		t.Fatalf("code = %s, want DEPENDENCY_UNAVAILABLE", resp.Code)
	}
}

// ---- Knowledge 契约 ----

func newKnowledgeRouter(service KnowledgeService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	knowledge := NewKnowledgeController(service)
	engine.POST("/api/v1/knowledge/upload", knowledge.Upload)
	engine.POST("/api/v1/knowledge/search", knowledge.Search)
	engine.POST("/api/v1/knowledge/search/detailed", knowledge.SearchDetailed)
	engine.DELETE("/api/v1/knowledge", knowledge.Delete)
	return engine
}

func TestKnowledgeUploadReturnsDocID(t *testing.T) {
	engine := newKnowledgeRouter(&fakeKnowledge{})

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/knowledge/upload",
		`{"title":"t","content":"c","category":"ops"}`)
	if recorder.Code != http.StatusOK || !resp.Success {
		t.Fatalf("unexpected response: %d %+v", recorder.Code, resp)
	}
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil || data.ID != "doc-1" {
		t.Fatalf("unexpected data: %s", string(resp.Data))
	}
}

func TestKnowledgeUploadValidation(t *testing.T) {
	engine := newKnowledgeRouter(&fakeKnowledge{})

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/knowledge/upload",
		`{"title":"t","content":"","category":"ops"}`)
	if recorder.Code != http.StatusBadRequest || resp.Code != httpapi.CodeInvalidArgument {
		t.Fatalf("status=%d code=%s", recorder.Code, resp.Code)
	}
}

func TestKnowledgeSearchSuccess(t *testing.T) {
	engine := newKnowledgeRouter(&fakeKnowledge{docs: []types.KnowledgeDocument{{ID: "1", Title: "runbook"}}})

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/knowledge/search", `{"query":"redis"}`)
	if recorder.Code != http.StatusOK || !resp.Success {
		t.Fatalf("unexpected response: %d %+v", recorder.Code, resp)
	}
	var docs []types.KnowledgeDocument
	if err := json.Unmarshal(resp.Data, &docs); err != nil || len(docs) != 1 || docs[0].Title != "runbook" {
		t.Fatalf("unexpected docs payload: %s", string(resp.Data))
	}
}

func TestKnowledgeSearchDependencyFailure(t *testing.T) {
	engine := newKnowledgeRouter(&fakeKnowledge{searchErr: errors.New("es down")})

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/knowledge/search", `{"query":"redis"}`)
	if recorder.Code != http.StatusServiceUnavailable || resp.Code != httpapi.CodeDependencyUnavailable {
		t.Fatalf("status=%d code=%s", recorder.Code, resp.Code)
	}
}

func TestKnowledgeDeleteRejectsUnsafeID(t *testing.T) {
	engine := newKnowledgeRouter(&fakeKnowledge{})

	recorder, resp := doJSONRequest(t, engine, http.MethodDelete, "/api/v1/knowledge?id=../secret", "")
	if recorder.Code != http.StatusBadRequest || resp.Code != httpapi.CodeInvalidArgument {
		t.Fatalf("status=%d code=%s", recorder.Code, resp.Code)
	}
}

// ---- Observability 契约 ----

func newObservabilityRouter(collector ObservabilityCollector) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	ob := NewObservabilityController(collector, "pilot-api")
	engine.POST("/api/v1/observability/overview", ob.GetOverview)
	engine.POST("/api/v1/observability/alerts", ob.GetAlerts)
	engine.POST("/api/v1/observability/metrics", ob.GetMetrics)
	engine.POST("/api/v1/observability/logs", ob.GetLogs)
	return engine
}

func TestObservabilityOverviewShape(t *testing.T) {
	now := time.Now()
	collector := &fakeCollector{
		alerts:  []types.AlertInfo{{Name: "A1", Status: "firing", StartTime: now}},
		metrics: []types.MetricData{{Name: "up", Value: 1, Timestamp: now}},
		logs:    []types.LogEntry{{Level: "error", Message: "boom", Timestamp: now}},
	}
	engine := newObservabilityRouter(collector)

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/observability/overview",
		`{"service":"pilot-api","metric_query":"up","duration_minutes":60,"log_level":"error"}`)
	if recorder.Code != http.StatusOK || !resp.Success {
		t.Fatalf("unexpected response: %d %+v body=%s", recorder.Code, resp, recorder.Body.String())
	}

	var data struct {
		Summary map[string]any     `json:"summary"`
		Alerts  []types.AlertInfo  `json:"alerts"`
		Metrics []types.MetricData `json:"metrics"`
		Logs    []types.LogEntry   `json:"logs"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("decode overview failed: %v", err)
	}
	if len(data.Alerts) != 1 || len(data.Metrics) != 1 || len(data.Logs) != 1 {
		t.Fatalf("unexpected overview payload: %s", string(resp.Data))
	}
	if data.Summary["alerts_active"].(float64) != 1 {
		t.Fatalf("unexpected summary: %+v", data.Summary)
	}
}

func TestObservabilityMetricsRejectsInvalidPromQL(t *testing.T) {
	engine := newObservabilityRouter(&fakeCollector{})

	longQuery := strings.Repeat("a", 501)
	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/observability/metrics",
		fmt.Sprintf(`{"query":"%s"}`, longQuery))
	if recorder.Code != http.StatusBadRequest || resp.Code != httpapi.CodeInvalidArgument {
		t.Fatalf("status=%d code=%s", recorder.Code, resp.Code)
	}
}

func TestObservabilityLogsDependencyFailure(t *testing.T) {
	engine := newObservabilityRouter(&fakeCollector{err: errors.New("prom down")})

	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/observability/logs", `{"service":"pilot-api"}`)
	if recorder.Code != http.StatusServiceUnavailable || resp.Code != httpapi.CodeDependencyUnavailable {
		t.Fatalf("status=%d code=%s", recorder.Code, resp.Code)
	}
}

func TestChatInteractionIDAndFeedbackContract(t *testing.T) {
	feedback := &fakeFeedback{}
	controller := NewChatController(&fakeAgent{generateResult: "answer"}, nil, &fakeShortTerm{}, &fakeHistory{}).WithFeedbackStore(feedback)
	engine := gin.New()
	engine.POST("/api/v1/chat", controller.Chat)
	engine.POST("/api/v1/chat/feedback", controller.SubmitFeedback)
	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat", validChatBody)
	interactionID := recorder.Header().Get("X-Pilot-Interaction-ID")
	if !resp.Success || interactionID == "" || len(feedback.recorded) != 1 || feedback.recorded[0].ID != interactionID {
		t.Fatalf("interaction header/record missing: id=%q records=%+v", interactionID, feedback.recorded)
	}
	body := fmt.Sprintf(`{"interaction_id":%q,"user_id":"u1","session_id":"s1","vote":"up"}`, interactionID)
	_, feedbackResp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat/feedback", body)
	if !feedbackResp.Success || feedback.submits != 1 {
		t.Fatalf("feedback contract failed: %+v submits=%d", feedbackResp, feedback.submits)
	}
}

func TestMultiAgentHTTPDurationFlowsThroughContext(t *testing.T) {
	multi := &fakeMultiAgent{result: multiagent.Result{Answer: "ok", Metadata: map[string]any{}}}
	engine := newChatRouter(&fakeAgent{}, multi, &fakeShortTerm{}, &fakeHistory{})
	recorder, resp := doJSONRequest(t, engine, http.MethodPost, "/api/v1/chat/multi-agent", `{"user_id":"u","session_id":"s","query":"why","duration_minutes":180}`)
	if recorder.Code != http.StatusOK || !resp.Success || multi.seenWindow != 180 {
		t.Fatalf("status=%d response=%+v window=%d", recorder.Code, resp, multi.seenWindow)
	}
}
