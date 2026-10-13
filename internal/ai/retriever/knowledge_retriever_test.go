package retriever

import (
	"context"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	"strings"
	"testing"

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
