package retriever

import (
	"testing"

	"shanyangcode.com/pilot/pkg/types"
)

func TestFuseRRFCombinesIndependentRecallChannels(t *testing.T) {
	bm25 := []recallCandidate{
		{document: types.KnowledgeDocument{ID: "bm25-only", Title: "Connection pool"}, score: 9},
		{document: types.KnowledgeDocument{ID: "shared", Title: "Timeout runbook"}, score: 8},
	}
	vector := []recallCandidate{
		{document: types.KnowledgeDocument{ID: "shared", Title: "Timeout runbook"}, score: 0.95},
		{document: types.KnowledgeDocument{ID: "vector-only", Title: "Network timeout"}, score: 0.90},
	}

	results := fuseRRF(bm25, vector, 60, 3)
	if len(results) != 3 {
		t.Fatalf("result count = %d, want 3", len(results))
	}
	if results[0].Document.ID != "shared" {
		t.Fatalf("first result = %s, want shared", results[0].Document.ID)
	}
	if results[0].BM25Score == nil || results[0].VectorScore == nil || results[0].FusedScore == nil {
		t.Fatalf("shared result does not preserve channel scores: %#v", results[0])
	}
	if got := results[0].RecallMethods; len(got) != 2 || got[0] != recallModeBM25 || got[1] != recallModeVector {
		t.Fatalf("recall methods = %#v", got)
	}
}

func TestSingleRouteResultsDeduplicatesAndKeepsProvenance(t *testing.T) {
	candidates := []recallCandidate{
		{document: types.KnowledgeDocument{ID: "doc-1"}, score: 5},
		{document: types.KnowledgeDocument{ID: "doc-1"}, score: 4},
		{document: types.KnowledgeDocument{ID: "doc-2"}, score: 3},
	}

	results := singleRouteResults(candidates, recallModeBM25, 5)
	if len(results) != 2 {
		t.Fatalf("result count = %d, want 2", len(results))
	}
	if results[0].BM25Score == nil || results[0].VectorScore != nil || results[0].FusedScore != nil {
		t.Fatalf("unexpected BM25 provenance: %#v", results[0])
	}
}

func TestRecallConfigRejectsUnsupportedMode(t *testing.T) {
	_, err := (RecallConfig{Mode: "unsupported"}).normalized(5)
	if err == nil {
		t.Fatal("expected unsupported mode error")
	}
}
