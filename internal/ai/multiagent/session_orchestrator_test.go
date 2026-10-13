package multiagent

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

type sessionLoaderFunc func(context.Context, string, string) (string, error)

func (f sessionLoaderFunc) SessionSummary(ctx context.Context, userID, sessionID string) (string, error) {
	return f(ctx, userID, sessionID)
}

type episodicRecallFunc func(context.Context, string, string, int) ([]Finding, error)

func (f episodicRecallFunc) Recall(ctx context.Context, service, query string, limit int) ([]Finding, error) {
	return f(ctx, service, query, limit)
}

type resultRunner struct{ result Result }

func (r resultRunner) Execute(context.Context, string) (Result, error) { return r.result, nil }

type concurrentContextRunner struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	seen    map[string]string
}

func (r *concurrentContextRunner) Execute(ctx context.Context, query string) (Result, error) {
	r.entered <- struct{}{}
	<-r.release
	r.mu.Lock()
	r.seen[query] = sessionSummaryFromContext(ctx)
	r.mu.Unlock()
	return Result{Answer: "ok", Plan: Plan{Intent: "incident_triage"}}, nil
}

func TestSessionOrchestratorScopesContextPerConcurrentRequest(t *testing.T) {
	runner := &concurrentContextRunner{
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
		seen:    map[string]string{},
	}
	orchestrator := NewSessionOrchestrator(runner, sessionLoaderFunc(func(_ context.Context, userID, sessionID string) (string, error) {
		return fmt.Sprintf("summary-for-%s-%s", userID, sessionID), nil
	}), nil)

	var group sync.WaitGroup
	for _, input := range []struct {
		user    string
		session string
		query   string
	}{{"alice", "s1", "query-a"}, {"bob", "s2", "query-b"}} {
		input := input
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := orchestrator.Execute(context.Background(), input.user, input.session, input.query); err != nil {
				t.Errorf("Execute(%s): %v", input.query, err)
			}
		}()
	}
	<-runner.entered
	<-runner.entered
	close(runner.release)
	group.Wait()

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.seen["query-a"] != "summary-for-alice-s1" {
		t.Fatalf("query-a context = %q", runner.seen["query-a"])
	}
	if runner.seen["query-b"] != "summary-for-bob-s2" {
		t.Fatalf("query-b context = %q", runner.seen["query-b"])
	}
}

func TestRequestScopedEmptySummaryDoesNotFallBackToSharedState(t *testing.T) {
	orch, err := NewOrchestrator(plannerStub{plan: Plan{Intent: "knowledge_query", SelectedRoles: []Role{RoleSynthesis}}}, nil, &synthesizerStub{answer: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	orch.SetSessionSummary("shared-summary")
	if got := orch.sessionContext(contextWithSessionSummary(context.Background(), "")); got != "" {
		t.Fatalf("request-scoped empty summary leaked shared state: %q", got)
	}
}

func TestSessionOrchestratorAddsMemoryAsNonClaimableGraphContext(t *testing.T) {
	runner := resultRunner{result: Result{
		Answer: "runtime answer [alert:1]",
		Plan:   Plan{Intent: "incident_triage", Service: "checkout"},
		EvidenceGraph: &EvidenceGraph{Nodes: []EvidenceNode{{
			ID: "alert:1", Source: "alerts", Kind: "evidence", Runtime: true, Claimable: true,
		}}},
		Verification: &VerificationReport{Assessment: EvidenceAssessment{RuntimeEvidenceCount: 1}},
	}}
	recaller := episodicRecallFunc(func(context.Context, string, string, int) ([]Finding, error) {
		return []Finding{{
			Role:     RoleKnowledge,
			Summary:  "similar historical incident",
			Evidence: []EvidenceItem{{ID: "episodic:7", Source: "episodic_memory", Content: "past rollback"}},
		}}, nil
	})
	result, err := NewSessionOrchestrator(runner, nil, recaller).Execute(context.Background(), "u1", "s1", "checkout failed")
	if err != nil {
		t.Fatal(err)
	}
	if result.EvidenceGraph == nil || len(result.EvidenceGraph.Nodes) != 2 {
		t.Fatalf("graph = %+v", result.EvidenceGraph)
	}
	var memoryNode *EvidenceNode
	for index := range result.EvidenceGraph.Nodes {
		if result.EvidenceGraph.Nodes[index].ID == "episodic:7" {
			memoryNode = &result.EvidenceGraph.Nodes[index]
		}
	}
	if memoryNode == nil || memoryNode.Kind != "context" || memoryNode.Runtime || memoryNode.Claimable {
		t.Fatalf("episodic node must be non-claimable context: %+v", memoryNode)
	}
	if result.Verification.Assessment.RuntimeEvidenceCount != 1 {
		t.Fatalf("memory changed runtime assessment: %+v", result.Verification.Assessment)
	}
}

func TestSessionOrchestratorDoesNotAutoPromoteCompletedRun(t *testing.T) {
	runner := resultRunner{result: Result{Answer: "completed", Plan: Plan{Intent: "incident_triage"}}}
	result, err := NewSessionOrchestrator(runner, nil, nil).Execute(context.Background(), "u", "s", "q")
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata["episodic_promotion"] != "requires_explicit_thumbs_up" {
		t.Fatalf("promotion policy missing: %+v", result.Metadata)
	}
}
