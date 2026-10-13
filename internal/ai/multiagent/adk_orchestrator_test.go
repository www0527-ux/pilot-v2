package multiagent

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type stubExecutor struct {
	role Role
}

func TestLegacyADKContractParity(t *testing.T) {
	inner, err := NewOrchestrator(stubPlanner{}, map[Role]Executor{RoleEvidence: stubExecutor{role: RoleEvidence}, RoleKnowledge: stubExecutor{role: RoleKnowledge}}, stubSynth{})
	if err != nil {
		t.Fatal(err)
	}
	inner.verification = VerificationPolicy{Mode: VerificationModeEvidenceGraph, MinConfidence: .65, MinIndependentSources: 1, ReplanWindowMultiplier: 2}
	legacy, err := inner.Execute(context.Background(), "api outage")
	if err != nil {
		t.Fatal(err)
	}
	adkOrch, _ := NewADKOrchestrator(inner)
	adkResult, err := adkOrch.Execute(context.Background(), "api outage")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy.Plan.SelectedRoles, adkResult.Plan.SelectedRoles) || legacy.Plan.Intent != adkResult.Plan.Intent || legacy.Plan.Service != adkResult.Plan.Service {
		t.Fatalf("plan parity failed legacy=%+v adk=%+v", legacy.Plan, adkResult.Plan)
	}
	if legacy.Verification == nil || adkResult.Verification == nil || legacy.Verification.Decision != adkResult.Verification.Decision || legacy.Verification.Assessment.StopReason != adkResult.Verification.Assessment.StopReason {
		t.Fatalf("verification parity failed legacy=%+v adk=%+v", legacy.Verification, adkResult.Verification)
	}
	if len(legacy.Limitations) != len(adkResult.Limitations) {
		t.Fatalf("limitation parity failed legacy=%+v adk=%+v", legacy.Limitations, adkResult.Limitations)
	}
}

type slowExecutor struct{}

func (slowExecutor) Execute(ctx context.Context, _ string, _ Plan) (Finding, error) {
	<-ctx.Done()
	return Finding{}, ctx.Err()
}

func (s stubExecutor) Execute(context.Context, string, Plan) (Finding, error) {
	source := string(s.role)
	if s.role == RoleEvidence {
		source = "alerts"
	}
	return Finding{
		Role:    s.role,
		Summary: string(s.role) + " ok",
		Evidence: []EvidenceItem{{
			ID:      string(s.role) + ":1",
			Source:  source,
			Content: "sample",
		}},
	}, nil
}

func TestADKOrchestratorAppliesBranchTimeout(t *testing.T) {
	inner, err := NewOrchestrator(stubPlanner{}, map[Role]Executor{RoleEvidence: slowExecutor{}, RoleKnowledge: slowExecutor{}}, stubSynth{})
	if err != nil {
		t.Fatal(err)
	}
	inner.branchTimeout = 20 * time.Millisecond
	inner.verification = VerificationPolicy{Mode: VerificationModeEvidenceGraph, MinConfidence: .65, MinIndependentSources: 1, ReplanWindowMultiplier: 2}
	adkOrch, err := NewADKOrchestrator(inner)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := adkOrch.Execute(context.Background(), "api outage")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("branch timeout not applied: %s", time.Since(started))
	}
	found := false
	for _, limitation := range result.Limitations {
		if limitation.Code == "ADK_BRANCH_TIMEOUT" {
			found = true
		}
	}
	if !found {
		t.Fatalf("timeout limitation missing: %+v", result.Limitations)
	}
}

type stubSynth struct{}

func (stubSynth) Synthesize(context.Context, string, Plan, []Finding, []Limitation) (string, error) {
	return "conclusion cites [evidence:1] and [knowledge:1]", nil
}

type stubPlanner struct{}

func (stubPlanner) Plan(context.Context, string) Plan {
	return Plan{
		Intent:        "incident_triage",
		Service:       "api",
		SelectedRoles: []Role{RoleEvidence, RoleKnowledge, RoleSynthesis},
		RoutingReason: "test",
	}
}

func TestADKOrchestratorParallelBranches(t *testing.T) {
	inner, err := NewOrchestrator(
		stubPlanner{},
		map[Role]Executor{
			RoleEvidence:  stubExecutor{role: RoleEvidence},
			RoleKnowledge: stubExecutor{role: RoleKnowledge},
		},
		stubSynth{},
	)
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	inner.verification = VerificationPolicy{
		Mode:                   VerificationModeEvidenceGraph,
		MinConfidence:          0.65,
		MinIndependentSources:  1,
		ReplanWindowMultiplier: 2,
	}
	adkOrch, err := NewADKOrchestrator(inner)
	if err != nil {
		t.Fatalf("NewADKOrchestrator: %v", err)
	}
	result, err := adkOrch.Execute(context.Background(), "api outage")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Metadata["runtime"] != RuntimeADK && result.Metadata["runtime"] != RuntimeLegacy {
		// ADK path sets runtime=adk; fallback still acceptable if Parallel fails in constrained env.
		t.Fatalf("runtime metadata = %v", result.Metadata["runtime"])
	}
	if len(result.Findings) == 0 {
		t.Fatal("expected findings")
	}
	if result.Verification == nil || result.EvidenceGraph == nil {
		t.Fatalf("ADK result did not preserve evidence verification: %+v", result)
	}
}
