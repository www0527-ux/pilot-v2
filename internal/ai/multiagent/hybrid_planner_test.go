package multiagent

import (
	"context"
	"fmt"
	"testing"

	"shanyangcode.com/pilot/internal/ai/multiagent/intent"
)

type classifierStub struct {
	result intent.Classification
	err    error
}

func (c classifierStub) Classify(context.Context, string) (intent.Classification, error) {
	return c.result, c.err
}

func TestHybridPlannerAdoptsHighConfidenceLLM(t *testing.T) {
	planner, err := NewHybridPlanner(HybridPlannerConfig{
		DefaultService: "pilot-api",
		MinConfidence:  0.6,
		Classifier: classifierStub{result: intent.Classification{
			Intent:        intent.IntentIncidentTriage,
			Service:       "checkout",
			SelectedRoles: []string{"triage", "evidence", "knowledge", "synthesis"},
			Confidence:    0.92,
			Reason:        "timeout incident",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := planner.Plan(context.Background(), "something is slow")
	if plan.Intent != intent.IntentIncidentTriage || plan.Service != "checkout" {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Metadata["planner_source"] != intent.SourceLLM {
		t.Fatalf("metadata = %#v", plan.Metadata)
	}
	hasEvidence := false
	for _, role := range plan.SelectedRoles {
		if role == RoleEvidence {
			hasEvidence = true
		}
	}
	if !hasEvidence {
		t.Fatalf("expected evidence role, got %#v", plan.SelectedRoles)
	}
}

func TestHybridPlannerWidensOnLowConfidence(t *testing.T) {
	planner, err := NewHybridPlanner(HybridPlannerConfig{
		DefaultService: "pilot-api",
		MinConfidence:  0.6,
		Classifier: classifierStub{result: intent.Classification{
			Intent:     intent.IntentKnowledgeQuery,
			Confidence: 0.2,
			Reason:     "unsure",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := planner.Plan(context.Background(), "maybe docs?")
	hasEvidence := false
	for _, role := range plan.SelectedRoles {
		if role == RoleEvidence {
			hasEvidence = true
		}
	}
	if !hasEvidence {
		t.Fatalf("expected widened evidence role, got %#v", plan.SelectedRoles)
	}
}

func TestHybridPlannerFallsBackOnClassifierError(t *testing.T) {
	planner, err := NewHybridPlanner(HybridPlannerConfig{
		DefaultService: "pilot-api",
		Classifier:     classifierStub{err: fmt.Errorf("model down")},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := planner.Plan(context.Background(), "find the database timeout runbook")
	if plan.Intent != "knowledge_query" {
		t.Fatalf("intent = %q", plan.Intent)
	}
	if plan.Metadata["planner_source"] != intent.SourceFallback {
		t.Fatalf("metadata = %#v", plan.Metadata)
	}
	hasEvidence, hasKnowledge := false, false
	for _, role := range plan.SelectedRoles {
		hasEvidence = hasEvidence || role == RoleEvidence
		hasKnowledge = hasKnowledge || role == RoleKnowledge
	}
	if !hasEvidence || !hasKnowledge || plan.Metadata["forced_evidence_knowledge"] != true {
		t.Fatalf("classifier failure did not force evidence+knowledge: %#v", plan)
	}
}

func TestHybridPlannerPrefersExplicitServiceOverLLM(t *testing.T) {
	planner, err := NewHybridPlanner(HybridPlannerConfig{
		DefaultService: "pilot-api",
		MinConfidence:  0.5,
		Classifier: classifierStub{result: intent.Classification{
			Intent:     intent.IntentIncidentTriage,
			Service:    "wrong-service",
			Confidence: 0.99,
			Reason:     "ok",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := planner.Plan(context.Background(), "timeout service=checkout-api")
	if plan.Service != "checkout-api" {
		t.Fatalf("service = %q", plan.Service)
	}
}
