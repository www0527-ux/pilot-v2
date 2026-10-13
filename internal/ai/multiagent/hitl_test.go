package multiagent

import (
	"context"
	"sync"
	"testing"
)

type stubSession struct {
	calls int
}

func (s *stubSession) Execute(context.Context, string, string, string) (Result, error) {
	s.calls++
	return Result{Answer: "ok", Metadata: map[string]any{"status": "completed"}}, nil
}

func TestHITLInterruptAndResume(t *testing.T) {
	inner := &stubSession{}
	store := NewMemoryCheckpointStore()
	hitl := NewHITLOrchestrator(inner, ThresholdGate{WideWindowMinutes: 60}, store, 60)

	result, err := hitl.Execute(context.Background(), "u1", "s1", "duration_minutes=180 investigate outage")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Metadata["status"] != "interrupted" {
		t.Fatalf("status = %v", result.Metadata["status"])
	}
	cpID, _ := result.Metadata["checkpoint_id"].(string)
	if cpID == "" {
		t.Fatal("missing checkpoint_id")
	}
	if inner.calls != 0 {
		t.Fatalf("inner should not run before resume, calls=%d", inner.calls)
	}
	cp, err := store.Get(context.Background(), cpID)
	if err != nil || cp.Plan.Intent == "" || cp.NextStage != "execute_parallel_branches" || len(cp.CompletedSteps) != 1 || cp.Status != CheckpointPending {
		t.Fatalf("incomplete checkpoint: %+v err=%v", cp, err)
	}

	resumed, err := hitl.Resume(context.Background(), cpID, true)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Answer != "ok" {
		t.Fatalf("answer = %q", resumed.Answer)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d", inner.calls)
	}
}

type checkpointSession struct {
	mu           sync.Mutex
	prepareCalls int
	resumeCalls  int
	entered      chan struct{}
	release      chan struct{}
}

func (s *checkpointSession) Execute(context.Context, string, string, string) (Result, error) {
	return Result{Answer: "direct"}, nil
}
func (s *checkpointSession) PrepareCheckpoint(context.Context, string, string, string) (Plan, error) {
	s.mu.Lock()
	s.prepareCalls++
	s.mu.Unlock()
	return Plan{Intent: "incident_triage", Service: "api", SelectedRoles: WidenEvidenceRoles(), RoutingReason: "prepared once"}, nil
}
func (s *checkpointSession) ResumeCheckpoint(_ context.Context, cp Checkpoint) (Result, error) {
	s.mu.Lock()
	s.resumeCalls++
	s.mu.Unlock()
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.release != nil {
		<-s.release
	}
	return Result{Answer: "resumed", Plan: cp.Plan, Metadata: map[string]any{}}, nil
}

func TestHITLConcurrentResumeUsesCASAndPreparedPlan(t *testing.T) {
	session := &checkpointSession{entered: make(chan struct{}, 1), release: make(chan struct{})}
	store := NewMemoryCheckpointStore()
	hitl := NewHITLOrchestrator(session, ThresholdGate{WideWindowMinutes: 60}, store, 60)
	interrupted, err := hitl.Execute(context.Background(), "u", "s", "duration_minutes=180")
	if err != nil {
		t.Fatal(err)
	}
	id := interrupted.Metadata["checkpoint_id"].(string)
	firstDone := make(chan Result, 1)
	go func() { result, _ := hitl.Resume(context.Background(), id, true); firstDone <- result }()
	<-session.entered
	second, err := hitl.Resume(context.Background(), id, true)
	if err != nil || second.Metadata["status"] != CheckpointProcessing {
		t.Fatalf("second resume=%+v err=%v", second, err)
	}
	close(session.release)
	first := <-firstDone
	if first.Answer != "resumed" {
		t.Fatalf("first=%+v", first)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.prepareCalls != 1 || session.resumeCalls != 1 {
		t.Fatalf("prepare=%d resume=%d", session.prepareCalls, session.resumeCalls)
	}
}

func TestHITLRejectIsNormalBusinessResultAndPersistsAcrossWrapperRestart(t *testing.T) {
	session := &checkpointSession{}
	store := NewMemoryCheckpointStore()
	firstWrapper := NewHITLOrchestrator(session, ThresholdGate{WideWindowMinutes: 60}, store, 60)
	interrupted, err := firstWrapper.Execute(context.Background(), "original-user", "original-session", "duration_minutes=180")
	if err != nil {
		t.Fatal(err)
	}
	id := interrupted.Metadata["checkpoint_id"].(string)
	secondWrapper := NewHITLOrchestrator(session, ThresholdGate{WideWindowMinutes: 60}, store, 60)
	rejected, err := secondWrapper.Resume(context.Background(), id, false)
	if err != nil || rejected.Metadata["status"] != CheckpointRejected || rejected.Metadata["original_user_id"] != "original-user" || rejected.Metadata["original_session_id"] != "original-session" {
		t.Fatalf("rejected=%+v err=%v", rejected, err)
	}
	cp, _ := store.Get(context.Background(), id)
	if cp.Status != CheckpointRejected {
		t.Fatalf("checkpoint=%+v", cp)
	}
}

func TestHITLAutoApprove(t *testing.T) {
	inner := &stubSession{}
	hitl := NewHITLOrchestrator(inner, AutoApproveGate{}, NewMemoryCheckpointStore(), 120)
	result, err := hitl.Execute(context.Background(), "u1", "s1", "duration_minutes=180")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Answer != "ok" {
		t.Fatalf("answer = %q", result.Answer)
	}
	if inner.calls != 1 {
		t.Fatalf("calls = %d", inner.calls)
	}
}
