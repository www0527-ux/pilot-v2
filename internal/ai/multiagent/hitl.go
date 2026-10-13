package multiagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	pkgredis "shanyangcode.com/pilot/pkg/redis"
)

type InterruptDecision string

const (
	InterruptApprove InterruptDecision = "approve"
	InterruptReject  InterruptDecision = "reject"
	InterruptDefer   InterruptDecision = "defer"

	CheckpointPending    = "pending"
	CheckpointProcessing = "processing"
	CheckpointCompleted  = "completed"
	CheckpointRejected   = "rejected"
	CheckpointFailed     = "failed"
)

type InterruptRequest struct {
	Reason   string         `json:"reason"`
	ToolName string         `json:"tool_name,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type InterruptGate interface {
	Decide(req InterruptRequest) InterruptDecision
}
type AutoApproveGate struct{}

func (AutoApproveGate) Decide(InterruptRequest) InterruptDecision { return InterruptApprove }

type ThresholdGate struct{ WideWindowMinutes int }

func (g ThresholdGate) Decide(req InterruptRequest) InterruptDecision {
	wide := g.WideWindowMinutes
	if wide <= 0 {
		wide = 120
	}
	if req.Metadata == nil {
		return InterruptApprove
	}
	if metadataInt(req.Metadata, "duration_minutes") >= wide {
		return InterruptDefer
	}
	if require, _ := req.Metadata["require_approval"].(bool); require {
		return InterruptDefer
	}
	return InterruptApprove
}

func metadataInt(meta map[string]any, key string) int {
	if meta == nil {
		return 0
	}
	switch value := meta[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

// Checkpoint is a resumable execution ledger. CompletedSteps and Findings may
// be empty at a pre-branch approval gate, but the prepared Plan is always saved
// and reused so resume does not repeat classification/triage.
type Checkpoint struct {
	ID             string           `json:"id"`
	UserID         string           `json:"user_id"`
	SessionID      string           `json:"session_id"`
	Query          string           `json:"query"`
	Request        InterruptRequest `json:"request"`
	Plan           Plan             `json:"plan"`
	CompletedSteps []Step           `json:"completed_steps"`
	Findings       []Finding        `json:"findings"`
	NextStage      string           `json:"next_stage"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	Status         string           `json:"status"`
	Attempts       int              `json:"attempts"`
	LastError      string           `json:"last_error,omitempty"`
	Result         *Result          `json:"result,omitempty"`
}

type CheckpointStore interface {
	Put(ctx context.Context, cp Checkpoint) error
	Get(ctx context.Context, id string) (Checkpoint, error)
	CompareAndSwapStatus(ctx context.Context, id, expected, replacement string) (bool, error)
}

type MemoryCheckpointStore struct {
	mu   sync.Mutex
	data map[string]Checkpoint
}

func NewMemoryCheckpointStore() *MemoryCheckpointStore {
	return &MemoryCheckpointStore{data: map[string]Checkpoint{}}
}
func (s *MemoryCheckpointStore) Put(_ context.Context, cp Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp.UpdatedAt = time.Now().UTC()
	s.data[cp.ID] = cp
	return nil
}
func (s *MemoryCheckpointStore) Get(_ context.Context, id string) (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.data[id]
	if !ok {
		return Checkpoint{}, fmt.Errorf("checkpoint %s not found", id)
	}
	return cp, nil
}
func (s *MemoryCheckpointStore) CompareAndSwapStatus(_ context.Context, id, expected, replacement string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.data[id]
	if !ok {
		return false, fmt.Errorf("checkpoint %s not found", id)
	}
	if cp.Status != expected {
		return false, nil
	}
	cp.Status = replacement
	cp.UpdatedAt = time.Now().UTC()
	s.data[id] = cp
	return true, nil
}

type RedisCheckpointStore struct {
	client *pkgredis.Client
	ttl    time.Duration
	prefix string
}

func NewRedisCheckpointStore(client *pkgredis.Client) *RedisCheckpointStore {
	return &RedisCheckpointStore{client: client, ttl: 24 * time.Hour, prefix: "pilot:hitl:cp:"}
}
func (s *RedisCheckpointStore) Put(ctx context.Context, cp Checkpoint) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis checkpoint store unavailable")
	}
	cp.UpdatedAt = time.Now().UTC()
	return s.client.SetJSON(ctx, s.prefix+cp.ID, cp, s.ttl)
}
func (s *RedisCheckpointStore) Get(ctx context.Context, id string) (Checkpoint, error) {
	if s == nil || s.client == nil {
		return Checkpoint{}, fmt.Errorf("redis checkpoint store unavailable")
	}
	var cp Checkpoint
	if err := s.client.GetJSON(ctx, s.prefix+id, &cp); err != nil {
		return Checkpoint{}, fmt.Errorf("checkpoint %s not found: %w", id, err)
	}
	return cp, nil
}
func (s *RedisCheckpointStore) CompareAndSwapStatus(ctx context.Context, id, expected, replacement string) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis checkpoint store unavailable")
	}
	return s.client.CompareAndSwapJSONStatus(ctx, s.prefix+id, expected, replacement, s.ttl)
}

type SessionExecutor interface {
	Execute(ctx context.Context, userID, sessionID, query string) (Result, error)
}
type CheckpointPreparer interface {
	PrepareCheckpoint(ctx context.Context, userID, sessionID, query string) (Plan, error)
}
type CheckpointResumer interface {
	ResumeCheckpoint(ctx context.Context, checkpoint Checkpoint) (Result, error)
}

type HITLOrchestrator struct {
	inner   SessionExecutor
	gate    InterruptGate
	store   CheckpointStore
	wideMin int
}

func NewHITLOrchestrator(inner SessionExecutor, gate InterruptGate, store CheckpointStore, wideWindowMinutes int) *HITLOrchestrator {
	if gate == nil {
		gate = AutoApproveGate{}
	}
	if wideWindowMinutes <= 0 {
		wideWindowMinutes = 120
	}
	return &HITLOrchestrator{inner: inner, gate: gate, store: store, wideMin: wideWindowMinutes}
}

func (h *HITLOrchestrator) Execute(ctx context.Context, userID, sessionID, query string) (Result, error) {
	if h == nil || h.inner == nil {
		return Result{}, fmt.Errorf("hitl orchestrator unavailable")
	}
	minutes := RequestWindowMinutes(ctx)
	if minutes <= 0 {
		minutes = extractDurationMinutes(query)
	}
	if minutes > 1440 {
		minutes = 1440
	}
	require := minutes >= h.wideMin || strings.Contains(strings.ToLower(query), "correlate_approve")
	request := InterruptRequest{Reason: "wide evidence window or high-cost action requires approval", ToolName: "multi_agent", Metadata: map[string]any{"duration_minutes": minutes, "require_approval": require}}
	decision := h.gate.Decide(request)
	if decision == InterruptApprove {
		return h.inner.Execute(ctx, userID, sessionID, query)
	}
	if h.store == nil {
		return Result{}, fmt.Errorf("hitl interrupt requested but checkpoint store is nil")
	}
	plan := NewRulePlanner("").Plan(query)
	if preparer, ok := h.inner.(CheckpointPreparer); ok {
		prepared, err := preparer.PrepareCheckpoint(ctx, userID, sessionID, query)
		if err != nil {
			return Result{}, err
		}
		plan = prepared
	}
	now := time.Now().UTC()
	checkpoint := Checkpoint{ID: uuid.NewString(), UserID: userID, SessionID: sessionID, Query: query, Request: request, Plan: plan,
		CompletedSteps: []Step{{Role: RoleTriage, Status: StatusCompleted, Summary: plan.RoutingReason, StartedAt: now, Metadata: map[string]any{"checkpointed": true}}},
		Findings:       []Finding{}, NextStage: "execute_parallel_branches", CreatedAt: now, UpdatedAt: now, Status: CheckpointPending}
	if decision == InterruptReject {
		checkpoint.Status = CheckpointRejected
	}
	if err := h.store.Put(ctx, checkpoint); err != nil {
		return Result{}, err
	}
	status := "interrupted"
	if decision == InterruptReject {
		status = CheckpointRejected
	}
	return checkpointBusinessResult(checkpoint, status), nil
}

func (h *HITLOrchestrator) Resume(ctx context.Context, checkpointID string, approve bool) (Result, error) {
	if h == nil || h.inner == nil || h.store == nil {
		return Result{}, fmt.Errorf("hitl resume unavailable")
	}
	cp, err := h.store.Get(ctx, checkpointID)
	if err != nil {
		return Result{}, err
	}
	if cp.Status == CheckpointCompleted && cp.Result != nil {
		return *cp.Result, nil
	}
	if cp.Status == CheckpointRejected {
		return checkpointBusinessResult(cp, CheckpointRejected), nil
	}
	if !approve {
		swapped, err := h.swapPendingOrFailed(ctx, checkpointID, CheckpointRejected)
		if err != nil {
			return Result{}, err
		}
		if !swapped {
			current, getErr := h.store.Get(ctx, checkpointID)
			if getErr != nil {
				return Result{}, getErr
			}
			return checkpointBusinessResult(current, current.Status), nil
		}
		cp.Status = CheckpointRejected
		cp.NextStage = "none"
		_ = h.store.Put(ctx, cp)
		return checkpointBusinessResult(cp, CheckpointRejected), nil
	}
	swapped, err := h.swapPendingOrFailed(ctx, checkpointID, CheckpointProcessing)
	if err != nil {
		return Result{}, err
	}
	if !swapped {
		current, getErr := h.store.Get(ctx, checkpointID)
		if getErr != nil {
			return Result{}, getErr
		}
		if current.Status == CheckpointCompleted && current.Result != nil {
			return *current.Result, nil
		}
		return checkpointBusinessResult(current, current.Status), nil
	}
	cp, _ = h.store.Get(ctx, checkpointID)
	cp.Attempts++
	if err := h.store.Put(ctx, cp); err != nil {
		return Result{}, err
	}
	var result Result
	if resumer, ok := h.inner.(CheckpointResumer); ok {
		result, err = resumer.ResumeCheckpoint(ctx, cp)
	} else {
		result, err = h.inner.Execute(contextWithPreparedPlan(ctx, cp.Plan), cp.UserID, cp.SessionID, cp.Query)
	}
	if err != nil {
		_, _ = h.store.CompareAndSwapStatus(ctx, checkpointID, CheckpointProcessing, CheckpointFailed)
		cp.Status = CheckpointFailed
		cp.LastError = truncate(err.Error(), 300)
		cp.NextStage = "retry_execute_parallel_branches"
		_ = h.store.Put(ctx, cp)
		return Result{}, err
	}
	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	result.Metadata["resumed_from"] = checkpointID
	result.Metadata["status"] = CheckpointCompleted
	result.Metadata["original_user_id"] = cp.UserID
	result.Metadata["original_session_id"] = cp.SessionID
	result.Metadata["original_query"] = cp.Query
	cp.Status = CheckpointCompleted
	cp.NextStage = "done"
	cp.LastError = ""
	cp.Result = &result
	cp.Findings = result.Findings
	cp.CompletedSteps = result.Steps
	if err := h.store.Put(ctx, cp); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (h *HITLOrchestrator) swapPendingOrFailed(ctx context.Context, id, replacement string) (bool, error) {
	if swapped, err := h.store.CompareAndSwapStatus(ctx, id, CheckpointPending, replacement); err != nil || swapped {
		return swapped, err
	}
	cp, err := h.store.Get(ctx, id)
	if err != nil {
		return false, err
	}
	if cp.Status == CheckpointFailed && cp.Attempts < 3 {
		return h.store.CompareAndSwapStatus(ctx, id, CheckpointFailed, replacement)
	}
	return false, nil
}

func checkpointBusinessResult(cp Checkpoint, status string) Result {
	return Result{Plan: cp.Plan, Steps: cp.CompletedSteps, Findings: cp.Findings, Metadata: map[string]any{
		"status": status, "checkpoint_id": cp.ID, "reason": cp.Request.Reason, "request": cp.Request,
		"next_stage": cp.NextStage, "original_user_id": cp.UserID, "original_session_id": cp.SessionID, "original_query": cp.Query,
	}}
}

func extractDurationMinutes(query string) int {
	q := strings.ToLower(query)
	if i := strings.Index(q, "duration_minutes="); i >= 0 {
		var n int
		_, _ = fmt.Sscanf(q[i:], "duration_minutes=%d", &n)
		if n > 0 {
			return n
		}
	}
	var n int
	if _, err := fmt.Sscanf(q, "%d minutes", &n); err == nil && n > 0 {
		return n
	}
	if _, err := fmt.Sscanf(q, "%d分钟", &n); err == nil && n > 0 {
		return n
	}
	return 0
}
