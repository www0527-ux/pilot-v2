package retriever

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"

	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/pkg/elasticsearch"
	"shanyangcode.com/pilot/pkg/types"
)

type fakeKnowledgeStore struct {
	bulk          []elasticsearch.BulkDocument
	bulkAccepted  []string
	bulkErr       error
	deleteCount   int
	deleteErr     error
	deleteQuery   map[string]interface{}
	searchQueries []map[string]interface{}
	searchResults [][]elasticsearch.SearchHit
	searchErrs    []error
	putMappingErr error
}

func (f *fakeKnowledgeStore) BulkIndexDocuments(_ context.Context, _ string, docs []elasticsearch.BulkDocument) ([]string, error) {
	f.bulk = append([]elasticsearch.BulkDocument(nil), docs...)
	return f.bulkAccepted, f.bulkErr
}
func (f *fakeKnowledgeStore) SearchHits(_ context.Context, _ string, query map[string]interface{}) ([]elasticsearch.SearchHit, error) {
	f.searchQueries = append(f.searchQueries, query)
	index := len(f.searchQueries) - 1
	if index < len(f.searchErrs) && f.searchErrs[index] != nil {
		return nil, f.searchErrs[index]
	}
	if index < len(f.searchResults) {
		return f.searchResults[index], nil
	}
	return []elasticsearch.SearchHit{}, nil
}
func (f *fakeKnowledgeStore) DeleteByQuery(_ context.Context, _ string, query map[string]interface{}) (int, error) {
	f.deleteQuery = query
	return f.deleteCount, f.deleteErr
}
func (*fakeKnowledgeStore) CreateIndex(context.Context, string, map[string]interface{}) error {
	return nil
}
func (f *fakeKnowledgeStore) PutMapping(context.Context, string, map[string]interface{}) error {
	return f.putMappingErr
}

type fakeEmbedding struct {
	failOn string
}

func (f fakeEmbedding) Embed(_ context.Context, text string) ([]float64, error) {
	if f.failOn != "" && strings.Contains(text, f.failOn) {
		return nil, errors.New("embedding unavailable")
	}
	return []float64{0.2, 0.8}, nil
}

func testRetrieverConfig() *config.Config {
	return &config.Config{
		Elasticsearch: config.Elasticsearch{KnowledgeIndex: "knowledge", MaxResults: 5},
		Embedding:     config.Embedding{Dimension: 2},
		RAG: config.RAG{RetrievalMode: "hybrid", BM25TopK: 6, VectorTopK: 6, FinalTopK: 4, RRFK: 60,
			FallbackToBM25: true, ChunkSize: 8, ChunkOverlap: 2, CandidateMultiplier: 2, MaxChunksPerDoc: 2, RerankerMode: "lexical"},
	}
}

func TestIndexDocumentWritesIndependentChunksWithStableDocID(t *testing.T) {
	store := &fakeKnowledgeStore{}
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	doc := &types.KnowledgeDocument{Title: "手册", Content: "中文🙂abcdefghijk组合e\u0301结束", Category: "runbook", Tags: []string{"api"}, Metadata: map[string]string{"owner": "ops"}}
	if err := r.IndexDocument(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if doc.DocID == "" || doc.ID != doc.DocID || doc.ChunkCount != len(store.bulk) || len(store.bulk) < 2 {
		t.Fatalf("logical document/chunks inconsistent: doc=%+v bulk=%d", doc, len(store.bulk))
	}
	for index, item := range store.bulk {
		chunk := item.Document.(types.KnowledgeDocument)
		if chunk.DocID != doc.DocID || chunk.ID != doc.DocID || chunk.ChunkID != item.ID || chunk.ChunkIndex != index || chunk.ChunkCount != len(store.bulk) {
			t.Fatalf("bad chunk %d: %+v", index, chunk)
		}
		if chunk.Content == doc.Content {
			t.Fatalf("whole document was indexed as one chunk: %+v", chunk)
		}
		if len(chunk.Vector) != 2 || chunk.Metadata["owner"] != "ops" {
			t.Fatalf("chunk metadata/vector missing: %+v", chunk)
		}
	}
	second := &types.KnowledgeDocument{Title: doc.Title, Content: doc.Content}
	store.bulk = nil
	if err := r.IndexDocument(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if second.DocID != doc.DocID {
		t.Fatalf("doc_id is not stable: %q != %q", second.DocID, doc.DocID)
	}
}

func TestIndexDocumentRollsBackPartialBulkFailure(t *testing.T) {
	store := &fakeKnowledgeStore{bulkAccepted: []string{"doc-0000"}, bulkErr: errors.New("partial bulk failure"), deleteCount: 1}
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	doc := &types.KnowledgeDocument{ID: "doc", Title: "t", Content: "abcdefghijklmnop"}
	err = r.IndexDocument(context.Background(), doc)
	if err == nil || !strings.Contains(err.Error(), "rollback deleted 1") || store.deleteQuery == nil {
		t.Fatalf("partial failure was not rolled back: err=%v query=%v", err, store.deleteQuery)
	}
}

func TestIndexDocumentBulkRetriesAndCompensation(t *testing.T) {
	for _, test := range []struct {
		name          string
		failureStatus int
		recover       bool
		deleteFails   bool
		wantAttempts  int
	}{
		{name: "retry succeeds", failureStatus: 429, recover: true, wantAttempts: 2},
		{name: "mapping failure", failureStatus: 400, wantAttempts: 1},
		{name: "retry exhausted", failureStatus: 503, wantAttempts: 3},
		{name: "compensation failure", failureStatus: 400, deleteFails: true, wantAttempts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts, deletes := 0, 0
			stored := map[string]bool{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("X-Elastic-Product", "Elasticsearch")
				w.Header().Set("Content-Type", "application/json")
				switch {
				case request.URL.Path == "/_bulk":
					attempts++
					decoder := json.NewDecoder(request.Body)
					items := []map[string]any{}
					hasErrors := false
					for {
						var operation struct {
							Index struct {
								ID string `json:"_id"`
							} `json:"index"`
						}
						if err := decoder.Decode(&operation); err == io.EOF {
							break
						} else if err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						var document types.KnowledgeDocument
						if err := decoder.Decode(&document); err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if stored[operation.Index.ID] {
							t.Errorf("successful document was resubmitted: %s", operation.Index.ID)
						}
						result := map[string]any{"_id": operation.Index.ID, "status": 201}
						if document.RecordType == "section" && !(test.recover && attempts > 1) {
							hasErrors = true
							result["status"] = test.failureStatus
							result["error"] = map[string]string{"reason": "injected write failure"}
						} else {
							stored[operation.Index.ID] = true
						}
						items = append(items, map[string]any{"index": result})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"errors": hasErrors, "items": items})
				case strings.HasSuffix(request.URL.Path, "/_delete_by_query"):
					deletes++
					body, _ := io.ReadAll(request.Body)
					if !strings.Contains(string(body), `"doc_id":"doc"`) {
						t.Errorf("compensation did not target logical document: %s", body)
					}
					if test.deleteFails {
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = io.WriteString(w, `{"error":{"reason":"injected compensation failure"}}`)
						return
					}
					count := len(stored)
					clear(stored)
					_ = json.NewEncoder(w).Encode(map[string]any{"deleted": count, "failures": []any{}})
				default:
					_, _ = io.WriteString(w, `{"acknowledged":true,"version":{"number":"8.12.0"}}`)
				}
			}))
			defer server.Close()
			ctx := context.Background()
			client, err := elasticsearch.New(ctx, config.Elasticsearch{Addresses: []string{server.URL}})
			if err != nil {
				t.Fatal(err)
			}
			cfg := testRetrieverConfig()
			cfg.RAG.ChunkSize, cfg.RAG.ChunkOverlap = 800, 120
			r, err := newKnowledgeRetriever(ctx, client, fakeEmbedding{}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			doc := &types.KnowledgeDocument{DocID: "doc", Title: "Runbook", Content: "# Runbook\n\nCheck the pool.\n", Metadata: map[string]string{"format": "markdown"}}
			err = r.IndexDocument(ctx, doc)
			if attempts != test.wantAttempts {
				t.Fatalf("bulk attempts=%d, want %d", attempts, test.wantAttempts)
			}
			if test.recover {
				if err != nil || deletes != 0 || len(stored) != 2 || doc.ChunkCount != 1 {
					t.Fatalf("successful retry: deletes=%d stored=%v doc=%+v err=%v", deletes, stored, doc, err)
				}
				return
			}
			if err == nil || deletes != 1 || !strings.Contains(err.Error(), "injected write failure") {
				t.Fatalf("compensation: deletes=%d stored=%v err=%v", deletes, stored, err)
			}
			if test.deleteFails {
				if len(stored) != 1 || !strings.Contains(err.Error(), "injected compensation failure") {
					t.Fatalf("compensation failure was lost: stored=%v err=%v", stored, err)
				}
			} else if len(stored) != 0 {
				t.Fatalf("partial import was not deleted: %v", stored)
			}
		})
	}
}

func TestIndexDocumentPreservesWriteAndCompensationErrors(t *testing.T) {
	writeErr := errors.New("write failure")
	deleteErr := errors.New("delete failure")
	store := &fakeKnowledgeStore{bulkErr: writeErr, deleteErr: deleteErr}
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	err = r.IndexDocument(context.Background(), &types.KnowledgeDocument{DocID: "doc", Content: "text"})
	if !errors.Is(err, writeErr) || !errors.Is(err, deleteErr) {
		t.Fatalf("write or compensation error was not wrapped: %v", err)
	}
}

type canceledBulkStore struct {
	fakeKnowledgeStore
	cancel context.CancelFunc
}

func (s *canceledBulkStore) BulkIndexDocuments(ctx context.Context, _ string, _ []elasticsearch.BulkDocument) ([]string, error) {
	s.cancel()
	return []string{"doc-0000"}, ctx.Err()
}

func (s *canceledBulkStore) DeleteByQuery(ctx context.Context, index string, query map[string]interface{}) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return 0, errors.New("compensation context has no deadline")
	}
	return s.fakeKnowledgeStore.DeleteByQuery(ctx, index, query)
}

func TestIndexDocumentCompensatesAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &canceledBulkStore{fakeKnowledgeStore: fakeKnowledgeStore{deleteCount: 1}, cancel: cancel}
	r, err := newKnowledgeRetriever(ctx, store, fakeEmbedding{}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	err = r.IndexDocument(ctx, &types.KnowledgeDocument{DocID: "doc", Content: "text"})
	if !errors.Is(err, context.Canceled) || store.deleteQuery == nil || !strings.Contains(err.Error(), "rollback deleted 1") {
		t.Fatalf("canceled import was not compensated: query=%v err=%v", store.deleteQuery, err)
	}
}

func TestDeleteDocumentDeletesAllChunksAndIsIdempotent(t *testing.T) {
	store := &fakeKnowledgeStore{deleteCount: 3}
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := r.DeleteDocument(context.Background(), "doc-42")
	if err != nil || deleted != 3 || !strings.Contains(fmt.Sprint(store.deleteQuery), "doc_id") {
		t.Fatalf("delete = %d, %v query=%v", deleted, err, store.deleteQuery)
	}
	store.deleteCount = 0
	deleted, err = r.DeleteDocument(context.Background(), "doc-42")
	if err != nil || deleted != 0 {
		t.Fatalf("idempotent delete = %d, %v", deleted, err)
	}
}

func TestHybridSearchMarksVectorFallbackAndBuildsExactBoost(t *testing.T) {
	store := &fakeKnowledgeStore{
		searchResults: [][]elasticsearch.SearchHit{
			{
				{ID: "d-0000", Score: 3, Source: map[string]interface{}{"id": "d", "doc_id": "d", "chunk_id": "d-0000", "content": "trace deadbeefcafebabe failed", "title": "runbook"}},
			},
		},
	}
	r, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{failOn: "ERR_deadbeef-cafe"}, testRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	results, err := r.SearchDetailed(context.Background(), "ERR_deadbeef-cafe")
	if err != nil || len(results) != 1 || !results[0].Degraded || !strings.Contains(results[0].DegradationReason, "vector") {
		t.Fatalf("fallback results=%+v err=%v", results, err)
	}
	queryText := fmt.Sprint(store.searchQueries[0])
	if !strings.Contains(queryText, "match_phrase") || !strings.Contains(queryText, "doc_id") || !strings.Contains(queryText, "ERR_deadbeef-cafe") {
		t.Fatalf("exact/phrase boost absent: %s", queryText)
	}
}

type fakeModelReranker struct {
	scores map[string]float64
	err    error
}

func (f fakeModelReranker) Rerank(context.Context, string, []types.KnowledgeDocument) (map[string]float64, error) {
	return f.scores, f.err
}

func TestLexicalModelRerankFallbackAndDocumentDiversity(t *testing.T) {
	candidates := []RetrievedDocument{
		{Document: types.KnowledgeDocument{DocID: "a", ChunkID: "a-0", Content: "generic"}, Score: .5},
		{Document: types.KnowledgeDocument{DocID: "a", ChunkID: "a-1", Content: "ERR_deadbeef exact"}, Score: .1},
		{Document: types.KnowledgeDocument{DocID: "a", ChunkID: "a-2", Content: "other"}, Score: .4},
		{Document: types.KnowledgeDocument{DocID: "b", ChunkID: "b-0", Content: "different"}, Score: .3},
	}
	lexicalRerank("ERR_deadbeef", candidates)
	if candidates[0].Document.ChunkID != "a-1" || candidates[0].RerankMode != "lexical" {
		t.Fatalf("exact identifier did not rerank first: %+v", candidates)
	}
	diverse := diversifyChunks(candidates, 3, 2)
	if len(diverse) != 3 {
		t.Fatalf("unexpected diverse results: %+v", diverse)
	}
	countA := 0
	for _, item := range diverse {
		if item.Document.DocID == "a" {
			countA++
		}
	}
	if countA > 2 {
		t.Fatalf("per-document chunk limit violated: %+v", diverse)
	}
	r := &KnowledgeRetriever{rerankerMode: "model", modelReranker: fakeModelReranker{err: errors.New("model down")}, tracer: otel.Tracer("test")}
	fallback := r.rerank(context.Background(), "x", candidates)
	if !fallback[0].Degraded || fallback[0].RerankMode != "lexical_fallback" {
		t.Fatalf("model fallback not explicit: %+v", fallback[0])
	}
	successCandidates := []RetrievedDocument{
		{Document: types.KnowledgeDocument{DocID: "a", ChunkID: "a-0", Content: "one"}, Score: .2},
		{Document: types.KnowledgeDocument{DocID: "b", ChunkID: "b-0", Content: "two"}, Score: .3},
	}
	r.modelReranker = fakeModelReranker{scores: map[string]float64{"a-0": .95, "b-0": .1}}
	success := r.rerank(context.Background(), "target", successCandidates)
	if success[0].Document.ChunkID != "a-0" || success[0].RerankMode != "model" || success[0].RerankScore == nil {
		t.Fatalf("model reranker was not applied: %+v", success)
	}
}

func TestIncompatibleMappingFailsWithMigrationInstruction(t *testing.T) {
	store := &fakeKnowledgeStore{putMappingErr: errors.New("cannot update parameter [dims]")}
	_, err := newKnowledgeRetriever(context.Background(), store, fakeEmbedding{}, testRetrieverConfig())
	if err == nil || !strings.Contains(err.Error(), "versioned chunk index") || !strings.Contains(err.Error(), "re-upload") {
		t.Fatalf("missing explicit migration failure: %v", err)
	}
}
