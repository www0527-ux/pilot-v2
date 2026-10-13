package multiagent

import (
	"strings"
	"testing"
)

func TestEvidenceGraphMeetsIndependentRuntimeQuorum(t *testing.T) {
	plan := Plan{SelectedRoles: []Role{RoleEvidence, RoleSynthesis}}
	findings := []Finding{{
		Role: RoleEvidence,
		Evidence: []EvidenceItem{
			{ID: "alert:1", Source: "alerts", Content: "checkout error rate firing"},
			{ID: "log:1", Source: "logs", Content: "checkout request timed out", Metadata: map[string]any{"trace_id": "trace-1"}},
			{ID: "corr-hyp:0:checkout", Source: "correlation", Content: "multi-signal hypothesis", Metadata: map[string]any{
				"kind":        "hypothesis",
				"support_ids": []string{"alert:1", "log:1"},
				"confidence":  0.84,
			}},
		},
	}}

	graph, assessment := buildEvidenceGraph(plan, findings, VerificationPolicy{
		Mode:                  VerificationModeEvidenceGraph,
		MinConfidence:         0.65,
		MinIndependentSources: 2,
	})

	if assessment.NeedsMoreEvidence {
		t.Fatalf("assessment unexpectedly requests more evidence: %+v", assessment)
	}
	if assessment.Score < 0.65 || assessment.StopReason != "evidence_quorum_met" {
		t.Fatalf("assessment = %+v", assessment)
	}
	if got := strings.Join(assessment.RuntimeSources, ","); got != "alerts,logs" {
		t.Fatalf("runtime sources = %q", got)
	}
	if assessment.HypothesisCount != 1 {
		t.Fatalf("hypothesis count = %d", assessment.HypothesisCount)
	}
	if countEdges(graph.Edges, "supported_by") != 2 {
		t.Fatalf("graph edges = %+v", graph.Edges)
	}
}

func TestEvidenceGraphRequestsAdaptiveAcquisitionForSingleSource(t *testing.T) {
	plan := Plan{SelectedRoles: []Role{RoleEvidence, RoleSynthesis}}
	graph, assessment := buildEvidenceGraph(plan, []Finding{{
		Role: RoleEvidence,
		Evidence: []EvidenceItem{
			{ID: "log:1", Source: "logs", Content: "request timeout"},
			{ID: "log:1", Source: "logs", Content: "request timeout repeated"},
		},
	}}, VerificationPolicy{Mode: VerificationModeEvidenceGraph, MinConfidence: 0.65, MinIndependentSources: 2})

	if !assessment.NeedsMoreEvidence || assessment.StopReason != "insufficient_source_diversity" {
		t.Fatalf("assessment = %+v", assessment)
	}
	if assessment.RuntimeEvidenceCount != 1 || len(graph.Nodes) != 1 {
		t.Fatalf("stable ID should deduplicate repeats: assessment=%+v graph=%+v", assessment, graph)
	}
}

func TestEvidenceGraphModelsCounterEvidence(t *testing.T) {
	plan := Plan{SelectedRoles: []Role{RoleEvidence, RoleSynthesis}}
	graph, assessment := buildEvidenceGraph(plan, []Finding{{
		Role: RoleEvidence,
		Evidence: []EvidenceItem{
			{ID: "alert:1", Source: "alerts", Content: "service down"},
			{ID: "metric:up:1", Source: "metrics", Content: "up=1", Metadata: map[string]any{"stance": "counter"}},
			{ID: "corr-hyp:0:checkout", Source: "correlation", Content: "outage hypothesis", Metadata: map[string]any{
				"kind":        "hypothesis",
				"support_ids": []string{"alert:1"},
				"counter_ids": []string{"metric:up:1"},
				"confidence":  0.51,
			}},
		},
	}}, VerificationPolicy{Mode: VerificationModeEvidenceGraph, MinConfidence: 0.65, MinIndependentSources: 2})

	if assessment.ContradictionCount != 1 || !assessment.NeedsMoreEvidence {
		t.Fatalf("assessment = %+v", assessment)
	}
	if countEdges(graph.Edges, "contradicted_by") != 1 {
		t.Fatalf("graph edges = %+v", graph.Edges)
	}
}

func TestEvidenceGraphIgnoresUnclaimableCounterEdge(t *testing.T) {
	plan := Plan{SelectedRoles: []Role{RoleEvidence, RoleSynthesis}}
	graph, assessment := buildEvidenceGraph(plan, []Finding{{
		Role: RoleEvidence,
		Evidence: []EvidenceItem{
			{ID: "alert:1", Source: "alerts", Content: "service unavailable"},
			{ID: "tool:failed", Source: "tool:query_metrics", Content: "permission denied", Metadata: map[string]any{"status": "error"}},
			{ID: "corr-hyp:0:checkout", Source: "correlation", Content: "availability hypothesis", Metadata: map[string]any{
				"kind":        "hypothesis",
				"support_ids": []string{"alert:1"},
				"counter_ids": []string{"tool:failed"},
			}},
		},
	}}, VerificationPolicy{Mode: VerificationModeEvidenceGraph, MinConfidence: 0.65, MinIndependentSources: 1})

	if assessment.ContradictionCount != 0 || countEdges(graph.Edges, "contradicted_by") != 0 {
		t.Fatalf("failed tool output must not form a contradiction: assessment=%+v graph=%+v", assessment, graph)
	}
}

func TestCitationAuditRejectsHallucinatedEvidenceID(t *testing.T) {
	findings := []Finding{{
		Role: RoleEvidence,
		Evidence: []EvidenceItem{
			{ID: "alert:real", Source: "alerts", Content: "real alert"},
			{ID: "tool:query_logs:2", Source: "tool:query_logs", Content: "source unavailable", Metadata: map[string]any{"status": "error"}},
		},
	}}

	audit := auditCitations("Conclusion [alert:real], fabricated support [log:missing].", findings)
	if audit.Passed {
		t.Fatalf("audit should reject hallucinated citation: %+v", audit)
	}
	if len(audit.ValidIDs) != 1 || audit.ValidIDs[0] != "alert:real" {
		t.Fatalf("valid IDs = %+v", audit.ValidIDs)
	}
	if len(audit.InvalidIDs) != 1 || audit.InvalidIDs[0] != "log:missing" {
		t.Fatalf("invalid IDs = %+v", audit.InvalidIDs)
	}
	if audit.ClaimableCount != 1 {
		t.Fatalf("failed tool output must not be claimable: %+v", audit)
	}
}

func TestEvidenceGraphExcludesEpisodicMemoryFromRuntimeConfidence(t *testing.T) {
	plan := Plan{SelectedRoles: []Role{RoleEvidence, RoleKnowledge, RoleSynthesis}}
	_, assessment := buildEvidenceGraph(plan, []Finding{{
		Role:     RoleKnowledge,
		Metadata: map[string]any{"memory": true, "not_runtime_evidence": true},
		Evidence: []EvidenceItem{{ID: "episodic:7", Source: "episodic_memory", Content: "prior incident"}},
	}}, VerificationPolicy{Mode: VerificationModeEvidenceGraph, MinConfidence: 0.65, MinIndependentSources: 2})

	if assessment.Score != 0 || !assessment.NeedsMoreEvidence || assessment.RuntimeEvidenceCount != 0 {
		t.Fatalf("memory must not increase runtime confidence: %+v", assessment)
	}
}

func TestUngroundedHypothesisIsNotClaimableOrRendered(t *testing.T) {
	plan := Plan{Service: "checkout", SelectedRoles: []Role{RoleEvidence, RoleSynthesis}}
	findings := []Finding{{
		Role:    RoleEvidence,
		Summary: "database saturation is the root cause",
		Evidence: []EvidenceItem{{
			ID:      "corr-hyp:0:checkout",
			Source:  "correlation",
			Content: "database saturation is the root cause",
			Metadata: map[string]any{
				"kind":        "hypothesis",
				"support_ids": []string{"metric:missing"},
			},
		}},
	}}

	graph, assessment := buildEvidenceGraph(plan, findings, VerificationPolicy{
		Mode:                  VerificationModeEvidenceGraph,
		MinConfidence:         0.65,
		MinIndependentSources: 2,
	})
	if len(graph.Nodes) != 1 || graph.Nodes[0].Claimable {
		t.Fatalf("ungrounded hypothesis must be retained for audit but not claimable: %+v", graph.Nodes)
	}
	if assessment.HypothesisCount != 0 || assessment.EvidenceCount != 0 {
		t.Fatalf("ungrounded hypothesis must not affect assessment: %+v", assessment)
	}
	if audit := auditCitations("conclusion [corr-hyp:0:checkout]", findings); audit.Passed || len(audit.InvalidIDs) != 1 {
		t.Fatalf("ungrounded hypothesis citation must be rejected: %+v", audit)
	}
	if prompt := synthesisPrompt("why", plan, findings, nil); strings.Contains(prompt, "database saturation") {
		t.Fatalf("synthesis prompt leaked ungrounded hypothesis: %q", prompt)
	}
	if answer := fallbackAnswer(plan, findings, nil); strings.Contains(answer, "database saturation") || strings.Contains(answer, "corr-hyp:") {
		t.Fatalf("fallback rendered ungrounded hypothesis: %q", answer)
	}
}

func countEdges(edges []EvidenceEdge, relation string) int {
	count := 0
	for _, edge := range edges {
		if edge.Relation == relation {
			count++
		}
	}
	return count
}
