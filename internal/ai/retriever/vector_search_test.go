package retriever

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"shanyangcode.com/pilot/pkg/elasticsearch"
)

func TestVectorSearchUsesIndexedKNNAndPreservesHits(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limit      int
		candidates int
	}{
		{"small", 2, 100},
		{"default_pool", 15, 150},
		{"capped_candidates", 2000, 10000},
		{"largest_pool", 9999, 10000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeKnowledgeStore{searchResults: [][]elasticsearch.SearchHit{{
				{ID: "d-0001", Score: 1.9, Source: map[string]interface{}{"doc_id": "d", "content": "first"}},
				{ID: "d-0002", Score: 1.6, Source: map[string]interface{}{"doc_id": "d", "content": "second"}},
			}}}
			r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
			if err != nil {
				t.Fatal(err)
			}
			vector := []float64{0.2, 0.8}
			hits, err := r.searchVector(context.Background(), vector, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			// The adapter must preserve ES order, scores and the _id fallback.
			if len(hits) != 2 || hits[0].document.ChunkID != "d-0001" || hits[0].score != 1.9 || hits[1].document.ChunkID != "d-0002" {
				t.Fatalf("unexpected converted hits: %+v", hits)
			}
			query := store.searchQueries[0]
			if query["size"] != tc.limit || query["query"] != nil {
				t.Fatalf("expected standalone kNN, not a script or disjunctive query: %#v", query)
			}
			knn, ok := query["knn"].(map[string]interface{})
			if !ok || knn["field"] != "vector" || knn["k"] != tc.limit || knn["num_candidates"] != tc.candidates || knn["boost"] != 2.0 || !reflect.DeepEqual(knn["query_vector"], vector) {
				t.Fatalf("invalid kNN contract: %#v", knn)
			}
			wantFilter := map[string]interface{}{"bool": map[string]interface{}{
				"filter":   []interface{}{map[string]interface{}{"exists": map[string]interface{}{"field": "vector"}}},
				"must_not": []interface{}{map[string]interface{}{"term": map[string]interface{}{"record_type": "section"}}},
			}}
			if !reflect.DeepEqual(knn["filter"], wantFilter) {
				t.Fatalf("parent exclusion must be a kNN pre-filter: %#v", knn["filter"])
			}
		})
	}
}

func TestVectorSearchRejectsUnsupportedLimitsBeforeES(t *testing.T) {
	store := &fakeKnowledgeStore{}
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{-1, 0, 10000, 10001} {
		if _, err := r.searchVector(context.Background(), []float64{0.2, 0.8}, limit); err == nil {
			t.Fatalf("limit %d should be rejected", limit)
		}
	}
	if len(store.searchQueries) != 0 {
		t.Fatal("invalid limits should not reach Elasticsearch")
	}
}

func TestVectorSearchEmptyAndErrorResults(t *testing.T) {
	for _, searchErr := range []error{nil, errors.New("vector field is not indexed"), context.Canceled, context.DeadlineExceeded} {
		store := &fakeKnowledgeStore{searchErrs: []error{searchErr}}
		r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
		if err != nil {
			t.Fatal(err)
		}
		hits, err := r.searchVector(context.Background(), []float64{0.2, 0.8}, 5)
		if len(hits) != 0 || !errors.Is(err, searchErr) {
			t.Fatalf("search error %v: hits=%+v err=%v", searchErr, hits, err)
		}
	}
}

// Route by DSL rather than call order: hybrid BM25 runs concurrently with kNN.
type failingKNNStore struct {
	fakeKnowledgeStore
	err error
}

func (s *failingKNNStore) SearchHits(_ context.Context, _ string, query map[string]interface{}) ([]elasticsearch.SearchHit, error) {
	if query["knn"] != nil {
		return nil, s.err
	}
	return []elasticsearch.SearchHit{{ID: "d-0000", Score: 3, Source: map[string]interface{}{
		"doc_id": "d", "content": "Redis timeout", "record_type": "chunk",
	}}}, nil
}

func TestKNNFailureKeepsExistingRecallFallbackPolicy(t *testing.T) {
	knnErr := errors.New("vector field is not indexed")
	for _, tc := range []struct {
		name     string
		mode     string
		fallback bool
		wantErr  bool
	}{
		{"hybrid_fallback", recallModeHybrid, true, false},
		{"hybrid_strict", recallModeHybrid, false, true},
		{"vector_only", recallModeVector, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &failingKNNStore{err: knnErr}
			cfg := testRetrieverConfig()
			cfg.RAG.RetrievalMode, cfg.RAG.FallbackToBM25 = tc.mode, tc.fallback
			r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			results, err := r.SearchDetailed(context.Background(), "Redis timeout")
			if tc.wantErr {
				if !errors.Is(err, knnErr) {
					t.Fatalf("expected original kNN error, got %v", err)
				}
				return
			}
			if err != nil || len(results) != 1 || !results[0].Degraded || results[0].BM25Score == nil || results[0].VectorScore != nil {
				t.Fatalf("expected explicit BM25 fallback: results=%+v err=%v", results, err)
			}
		})
	}
}
