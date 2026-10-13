package retriever

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/pkg/elasticsearch"
	"shanyangcode.com/pilot/pkg/types"
)

type Embedding interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

type knowledgeStore interface {
	BulkIndexDocuments(ctx context.Context, index string, documents []elasticsearch.BulkDocument) ([]string, error)
	SearchHits(ctx context.Context, index string, query map[string]interface{}) ([]elasticsearch.SearchHit, error)
	DeleteByQuery(ctx context.Context, index string, query map[string]interface{}) (int, error)
	CreateIndex(ctx context.Context, index string, mapping map[string]interface{}) error
	PutMapping(ctx context.Context, index string, mapping map[string]interface{}) error
}

// ModelReranker is injectable so production can use a real model while tests
// use deterministic fakes. Scores are keyed by chunk_id and expected in [0,1].
type ModelReranker interface {
	Rerank(ctx context.Context, query string, candidates []types.KnowledgeDocument) (map[string]float64, error)
}

type KnowledgeRetriever struct {
	es                  knowledgeStore
	indexName           string
	embedder            Embedding
	modelReranker       ModelReranker
	vectorDim           int
	topK                int
	recallConfig        RecallConfig
	chunkSize           int
	chunkOverlap        int
	candidateMultiplier int
	maxChunksPerDoc     int
	rerankerMode        string
	tracer              trace.Tracer
}

func NewKnowledgeRetriever(ctx context.Context, es *elasticsearch.Client, emb Embedding, cfg *config.Config) (*KnowledgeRetriever, error) {
	return newKnowledgeRetriever(ctx, es, emb, cfg)
}

func newKnowledgeRetriever(ctx context.Context, es knowledgeStore, emb Embedding, cfg *config.Config) (*KnowledgeRetriever, error) {
	if es == nil {
		return nil, fmt.Errorf("knowledge store is required")
	}
	if emb == nil {
		return nil, fmt.Errorf("knowledge embedder is required")
	}
	topK := cfg.Elasticsearch.MaxResults
	recallConfig, err := (RecallConfig{
		Mode: cfg.RAG.RetrievalMode, BM25TopK: cfg.RAG.BM25TopK, VectorTopK: cfg.RAG.VectorTopK,
		FinalTopK: cfg.RAG.FinalTopK, RRFK: cfg.RAG.RRFK, FallbackToBM25: cfg.RAG.FallbackToBM25,
	}).normalized(topK)
	if err != nil {
		return nil, fmt.Errorf("load rag recall config failed: %w", err)
	}
	r := &KnowledgeRetriever{
		es: es, indexName: cfg.Elasticsearch.KnowledgeIndex, embedder: emb, vectorDim: cfg.Embedding.Dimension,
		topK: topK, recallConfig: recallConfig, chunkSize: cfg.RAG.ChunkSize, chunkOverlap: cfg.RAG.ChunkOverlap,
		candidateMultiplier: cfg.RAG.CandidateMultiplier, maxChunksPerDoc: cfg.RAG.MaxChunksPerDoc,
		rerankerMode: cfg.RAG.RerankerMode, tracer: otel.Tracer("pilot/rag"),
	}
	if err := r.InitIndex(ctx); err != nil {
		return nil, fmt.Errorf("init knowledge index failed: %w", err)
	}
	return r, nil
}

func (r *KnowledgeRetriever) WithModelReranker(reranker ModelReranker) *KnowledgeRetriever {
	r.modelReranker = reranker
	return r
}

// IndexDocument chunks, embeds, and bulk-indexes a logical document. Any
// partial bulk failure triggers a doc_id delete-by-query rollback.
func (r *KnowledgeRetriever) IndexDocument(ctx context.Context, doc *types.KnowledgeDocument) (err error) {
	started := time.Now()
	ctx, span := r.tracer.Start(ctx, "rag.index", trace.WithAttributes(attribute.String("rag.mode", "chunk")))
	defer func() {
		count := 0
		if doc != nil {
			count = doc.ChunkCount
		}
		observeRAG("index", "chunk", started, err, count)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "index_failed")
		}
		span.End()
	}()
	if doc == nil || strings.TrimSpace(doc.Content) == "" {
		return fmt.Errorf("knowledge document content is required")
	}
	docID := strings.TrimSpace(doc.DocID)
	if docID == "" {
		docID = strings.TrimSpace(doc.ID)
	}
	if docID == "" {
		docID = generateDocID(doc.Title, doc.Content)
	}
	var chunks []TextChunk
	var sections []MarkdownSection
	var chunkSections []int
	if doc.Metadata["format"] == "markdown" {
		sections, err = MarkdownChunks(ctx, doc.Content, r.chunkSize, r.chunkOverlap)
		chunks = nil
		for sectionIndex, section := range sections {
			for _, chunk := range section.Chunks {
				chunks = append(chunks, chunk)
				chunkSections = append(chunkSections, sectionIndex)
			}
		}
		if doc.DocID == "" && doc.ID == "" {
			docID = generateDocID(doc.Title, fmt.Sprintf("%s:%d:%d\x00%s", markdownSplitterVersion, r.chunkSize, r.chunkOverlap, doc.Content))
		}
	} else {
		chunks, err = ChunkText(doc.Content, r.chunkSize, r.chunkOverlap)
	}
	if err != nil {
		return fmt.Errorf("chunk document failed: %w", err)
	}
	if len(chunks) == 0 {
		return fmt.Errorf("knowledge document produced no chunks")
	}
	now := time.Now().UTC()
	createdAt := doc.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	bulk := make([]elasticsearch.BulkDocument, 0, len(chunks))
	for index, chunk := range chunks {
		embeddingText := chunk.Content
		sectionID, sectionPath := "", ""
		if len(sections) > 0 {
			sectionIndex := chunkSections[index]
			sectionID = fmt.Sprintf("%s-section-%04d", docID, sectionIndex)
			sectionPath = sections[sectionIndex].Path
			embeddingText = sectionPath + "\n" + chunk.Content
		}
		vector, embedErr := r.embedder.Embed(ctx, embeddingText)
		if embedErr != nil {
			return fmt.Errorf("embed chunk %d/%d failed: %w", index+1, len(chunks), embedErr)
		}
		if r.vectorDim > 0 && len(vector) != r.vectorDim {
			return fmt.Errorf("embed chunk %d returned dimension %d, want %d", index, len(vector), r.vectorDim)
		}
		chunkID := fmt.Sprintf("%s-%04d", docID, index)
		chunkDoc := types.KnowledgeDocument{
			ID: docID, DocID: docID, ChunkID: chunkID, ChunkIndex: index, ChunkCount: len(chunks),
			StartRune: chunk.StartRune, EndRune: chunk.EndRune, Title: doc.Title, Content: chunk.Content,
			Category: doc.Category, Tags: append([]string(nil), doc.Tags...), Metadata: cloneMetadata(doc.Metadata),
			Vector: vector, CreatedAt: createdAt, UpdatedAt: now,
			SectionID: sectionID, SectionPath: sectionPath, RecordType: "chunk",
		}
		if sectionID != "" {
			chunkDoc.SplitterVersion = markdownSplitterVersion
		}
		bulk = append(bulk, elasticsearch.BulkDocument{ID: chunkID, Document: chunkDoc})
	}
	for i, section := range sections {
		sectionID := fmt.Sprintf("%s-section-%04d", docID, i)
		parent := types.KnowledgeDocument{ID: docID, DocID: docID, ChunkID: sectionID, SectionID: sectionID, SectionPath: section.Path, RecordType: "section", SplitterVersion: markdownSplitterVersion,
			Content: section.Content, Title: doc.Title, Category: doc.Category, Tags: append([]string(nil), doc.Tags...), Metadata: cloneMetadata(doc.Metadata), StartRune: section.StartRune, EndRune: section.EndRune, CreatedAt: createdAt, UpdatedAt: now}
		bulk = append(bulk, elasticsearch.BulkDocument{ID: sectionID, Document: parent})
	}
	span.SetAttributes(attribute.Int("rag.chunk_count", len(chunks)), attribute.Int("rag.section_count", len(sections)))
	indexed, err := r.es.BulkIndexDocuments(ctx, r.indexName, bulk)
	if err != nil {
		deleted, rollbackErr := r.deleteLogicalDocument(ctx, docID)
		if rollbackErr != nil {
			return fmt.Errorf("index logical document failed after accepting %d chunks; rollback failed after deleting %d: %v: %w", len(indexed), deleted, rollbackErr, err)
		}
		return fmt.Errorf("index logical document failed after accepting %d chunks; rollback deleted %d: %w", len(indexed), deleted, err)
	}
	doc.ID, doc.DocID, doc.ChunkCount, doc.CreatedAt, doc.UpdatedAt = docID, docID, len(chunks), createdAt, now
	return nil
}

func (r *KnowledgeRetriever) Search(ctx context.Context, query string) ([]types.KnowledgeDocument, error) {
	results, err := r.SearchDetailed(ctx, query)
	if err != nil {
		return nil, err
	}
	return r.expandSections(ctx, results)
}

func (r *KnowledgeRetriever) SearchDetailed(ctx context.Context, query string) (results []RetrievedDocument, err error) {
	started := time.Now()
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query cannot be empty")
	}
	ctx, span := r.tracer.Start(ctx, "rag.search", trace.WithAttributes(attribute.String("rag.mode", r.recallConfig.Mode)))
	defer func() {
		observeRAG("search", r.recallConfig.Mode, started, err, len(results))
		span.SetAttributes(attribute.Int("rag.result_count", len(results)))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "search_failed")
		}
		span.End()
	}()
	//两路查询
	candidates, err := r.recall(ctx, query)
	if err != nil {
		return nil, err
	}
	candidates = r.rerank(ctx, query, candidates)
	return diversifyChunks(candidates, r.recallConfig.FinalTopK, r.maxChunksPerDoc), nil
}

type recallSearchResult struct {
	candidates []recallCandidate
	err        error
}

func (r *KnowledgeRetriever) recall(ctx context.Context, query string) ([]RetrievedDocument, error) {
	cfg := r.recallConfig
	poolLimit := maxInt(cfg.FinalTopK*r.candidateMultiplier, cfg.FinalTopK)
	if cfg.Mode == recallModeBM25 {
		bm25, err := r.searchBM25(ctx, query, maxInt(cfg.BM25TopK, poolLimit))
		if err != nil {
			return nil, err
		}
		return singleRouteResults(bm25, recallModeBM25, poolLimit), nil
	}
	searchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	bm25Result := make(chan recallSearchResult, 1)
	if cfg.Mode == recallModeHybrid {
		go func() {
			items, searchErr := r.searchBM25(searchCtx, query, maxInt(cfg.BM25TopK, poolLimit))
			bm25Result <- recallSearchResult{candidates: items, err: searchErr}
		}()
	}
	queryVector, vectorErr := r.embedder.Embed(searchCtx, query)
	var vector []recallCandidate
	if vectorErr == nil {
		vector, vectorErr = r.searchVector(searchCtx, queryVector, maxInt(cfg.VectorTopK, poolLimit))
	}
	if cfg.Mode == recallModeVector {
		if vectorErr != nil {
			return nil, fmt.Errorf("vector recall failed: %w", vectorErr)
		}
		return singleRouteResults(vector, recallModeVector, poolLimit), nil
	}
	bm25 := <-bm25Result
	if vectorErr != nil {
		if bm25.err == nil && cfg.FallbackToBM25 {
			slog.WarnContext(ctx, "vector recall unavailable; using BM25 fallback", "error", vectorErr)
			observeRAGFallback("vector_to_bm25")
			return markDegraded(singleRouteResults(bm25.candidates, recallModeBM25, poolLimit), "vector", vectorErr), nil
		}
		return nil, fmt.Errorf("vector recall failed: %w", vectorErr)
	}
	if bm25.err != nil {
		if len(vector) > 0 {
			slog.WarnContext(ctx, "BM25 recall unavailable; using vector results", "error", bm25.err)
			observeRAGFallback("bm25_to_vector")
			return markDegraded(singleRouteResults(vector, recallModeVector, poolLimit), "bm25", bm25.err), nil
		}
		return nil, fmt.Errorf("BM25 recall failed: %w", bm25.err)
	}
	started := time.Now()
	fused := fuseRRF(bm25.candidates, vector, cfg.RRFK, poolLimit)
	observeRAG("rrf", "hybrid", started, nil, len(fused))
	return fused, nil
}

// BM25检索
func (r *KnowledgeRetriever) searchBM25(ctx context.Context, query string, limit int) ([]recallCandidate, error) {
	started := time.Now()
	_, span := r.tracer.Start(ctx, "rag.recall.bm25", trace.WithAttributes(attribute.Int("rag.candidate_limit", limit)))
	defer span.End()
	should := []interface{}{
		map[string]interface{}{"multi_match": map[string]interface{}{"query": query, "fields": []string{"title^3", "section_path^3", "content", "tags^2"}, "type": "best_fields"}},
		map[string]interface{}{"multi_match": map[string]interface{}{"query": query, "fields": []string{"title^5", "content^2"}, "type": "phrase", "boost": 3}},
	}
	for _, identifier := range exactIdentifiers(query) {
		should = append(should,
			map[string]interface{}{"term": map[string]interface{}{"doc_id": map[string]interface{}{"value": identifier, "boost": 12}}},
			map[string]interface{}{"term": map[string]interface{}{"chunk_id": map[string]interface{}{"value": identifier, "boost": 12}}},
			map[string]interface{}{"match_phrase": map[string]interface{}{"content": map[string]interface{}{"query": identifier, "boost": 8}}},
		)
	}
	esQuery := map[string]interface{}{"size": limit, "query": map[string]interface{}{"bool": map[string]interface{}{"should": should, "minimum_should_match": 1, "must_not": []interface{}{map[string]interface{}{"term": map[string]interface{}{"record_type": "section"}}}}}}
	hits, err := r.es.SearchHits(ctx, r.indexName, esQuery)
	if err != nil {
		observeRAG("recall", "bm25", started, err, 0)
		return nil, fmt.Errorf("BM25 search failed: %w", err)
	}
	candidates := r.convertHitsToCandidates(hits)
	observeRAG("recall", "bm25", started, nil, len(candidates))
	return candidates, nil
}

func (r *KnowledgeRetriever) searchVector(ctx context.Context, vector []float64, limit int) ([]recallCandidate, error) {
	started := time.Now()
	_, span := r.tracer.Start(ctx, "rag.recall.vector", trace.WithAttributes(attribute.Int("rag.candidate_limit", limit)))
	defer span.End()
	esQuery := map[string]interface{}{"size": limit, "query": map[string]interface{}{"script_score": map[string]interface{}{
		"query":  map[string]interface{}{"exists": map[string]interface{}{"field": "vector"}},
		"script": map[string]interface{}{"source": "cosineSimilarity(params.query_vector, 'vector') + 1.0", "params": map[string]interface{}{"query_vector": vector}},
	}}}
	hits, err := r.es.SearchHits(ctx, r.indexName, esQuery)
	if err != nil {
		observeRAG("recall", "vector", started, err, 0)
		return nil, fmt.Errorf("vector search failed: %w", err)
	}
	candidates := r.convertHitsToCandidates(hits)
	observeRAG("recall", "vector", started, nil, len(candidates))
	return candidates, nil
}

func (r *KnowledgeRetriever) convertHitsToCandidates(hits []elasticsearch.SearchHit) []recallCandidate {
	candidates := make([]recallCandidate, 0, len(hits))
	for _, hit := range hits {
		doc := r.convertToDocument(hit.Source)
		if doc.ChunkID == "" {
			doc.ChunkID = hit.ID
		}
		if doc.DocID == "" {
			doc.DocID = doc.ID
		}
		if doc.ID == "" {
			doc.ID = doc.DocID
		}
		candidates = append(candidates, recallCandidate{document: doc, score: hit.Score})
	}
	return candidates
}

func (r *KnowledgeRetriever) DeleteDocument(ctx context.Context, docID string) (deleted int, err error) {
	started := time.Now()
	ctx, span := r.tracer.Start(ctx, "rag.delete", trace.WithAttributes(attribute.String("rag.delete_mode", "doc_id")))
	defer func() {
		observeRAG("delete", "doc_id", started, err, deleted)
		span.SetAttributes(attribute.Int("rag.deleted_count", deleted))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "delete_failed")
		}
		span.End()
	}()
	return r.deleteLogicalDocument(ctx, strings.TrimSpace(docID))
}

func (r *KnowledgeRetriever) deleteLogicalDocument(ctx context.Context, docID string) (int, error) {
	if docID == "" {
		return 0, fmt.Errorf("doc_id cannot be empty")
	}
	query := map[string]interface{}{"bool": map[string]interface{}{"should": []interface{}{
		map[string]interface{}{"term": map[string]interface{}{"doc_id": docID}},
		map[string]interface{}{"term": map[string]interface{}{"id": docID}},
	}, "minimum_should_match": 1}}
	return r.es.DeleteByQuery(ctx, r.indexName, query)
}

func (r *KnowledgeRetriever) InitIndex(ctx context.Context) error {
	vectorDim := r.vectorDim
	if vectorDim <= 0 {
		vectorDim = 1024
	}
	mapping := map[string]interface{}{"mappings": map[string]interface{}{"properties": map[string]interface{}{
		"id": map[string]interface{}{"type": "keyword"}, "doc_id": map[string]interface{}{"type": "keyword"},
		"chunk_id": map[string]interface{}{"type": "keyword"}, "chunk_index": map[string]interface{}{"type": "integer"},
		"section_id": map[string]interface{}{"type": "keyword"}, "section_path": map[string]interface{}{"type": "text", "analyzer": "standard"},
		"record_type": map[string]interface{}{"type": "keyword"}, "splitter_version": map[string]interface{}{"type": "keyword"},
		"chunk_count": map[string]interface{}{"type": "integer"}, "start_rune": map[string]interface{}{"type": "integer"},
		"end_rune": map[string]interface{}{"type": "integer"},
		"title":    map[string]interface{}{"type": "text", "analyzer": "standard", "fields": map[string]interface{}{"raw": map[string]interface{}{"type": "keyword", "ignore_above": 512}}},
		"content":  map[string]interface{}{"type": "text", "analyzer": "standard"}, "category": map[string]interface{}{"type": "keyword"},
		"tags": map[string]interface{}{"type": "keyword"}, "metadata": map[string]interface{}{"type": "object", "enabled": false},
		"vector":     map[string]interface{}{"type": "dense_vector", "dims": vectorDim, "index": true, "similarity": "cosine"},
		"created_at": map[string]interface{}{"type": "date"}, "updated_at": map[string]interface{}{"type": "date"},
	}}}
	if err := r.es.CreateIndex(ctx, r.indexName, mapping); err != nil {
		return err
	}
	mappings := mapping["mappings"].(map[string]interface{})
	if err := r.es.PutMapping(ctx, r.indexName, mappings); err != nil {
		if isMappingConflict(err) {
			return fmt.Errorf("knowledge index %q has an incompatible vector mapping; create a versioned chunk index, re-upload source documents, then switch elasticsearch.knowledge_index: %w", r.indexName, err)
		}
		return err
	}
	return nil
}

func isMappingConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "cannot be changed") || strings.Contains(msg, "cannot update parameter [dims]") ||
		strings.Contains(msg, "different number of dimensions") || strings.Contains(msg, "more dimensions than defined")
}

func (r *KnowledgeRetriever) convertToDocument(result map[string]interface{}) types.KnowledgeDocument {
	doc := types.KnowledgeDocument{
		ID: stringField(result, "id"), DocID: stringField(result, "doc_id"), ChunkID: stringField(result, "chunk_id"),
		ChunkIndex: intField(result, "chunk_index"), ChunkCount: intField(result, "chunk_count"),
		StartRune: intField(result, "start_rune"), EndRune: intField(result, "end_rune"),
		SectionID: stringField(result, "section_id"), SectionPath: stringField(result, "section_path"), RecordType: stringField(result, "record_type"), SplitterVersion: stringField(result, "splitter_version"),
		Title: stringField(result, "title"), Content: stringField(result, "content"), Category: stringField(result, "category"),
	}
	doc.Tags = stringSliceField(result["tags"])
	if raw, ok := result["metadata"].(map[string]interface{}); ok {
		doc.Metadata = make(map[string]string, len(raw))
		for key, value := range raw {
			doc.Metadata[key] = fmt.Sprint(value)
		}
	}
	return doc
}

func (r *KnowledgeRetriever) rerank(ctx context.Context, query string, candidates []RetrievedDocument) []RetrievedDocument {
	started := time.Now()
	ctx, span := r.tracer.Start(ctx, "rag.rerank", trace.WithAttributes(attribute.String("rag.reranker_mode", r.rerankerMode), attribute.Int("rag.candidate_count", len(candidates))))
	defer func() { span.End(); observeRAG("rerank", r.rerankerMode, started, nil, len(candidates)) }()
	lexicalRerank(query, candidates)
	if r.rerankerMode != "model" {
		return candidates
	}
	if r.modelReranker == nil {
		observeRAGFallback("model_to_lexical")
		return markRerankerFallback(candidates, "model reranker is not configured")
	}
	docs := make([]types.KnowledgeDocument, 0, len(candidates))
	for _, candidate := range candidates {
		docs = append(docs, candidate.Document)
	}
	scores, err := r.modelReranker.Rerank(ctx, query, docs)
	if err != nil {
		span.RecordError(err)
		observeRAGFallback("model_to_lexical")
		return markRerankerFallback(candidates, err.Error())
	}
	for index := range candidates {
		score, ok := scores[candidates[index].Document.ChunkID]
		if !ok {
			continue
		}
		score = clamp01(score)
		candidates[index].RerankScore = &score
		candidates[index].RerankMode = "model"
		candidates[index].Score = 0.75*score + 0.25*candidates[index].Score
	}
	sortRetrieved(candidates)
	return candidates
}

func lexicalRerank(query string, candidates []RetrievedDocument) {
	queryTokens := tokenSet(query)
	identifiers := exactIdentifiers(query)
	for index := range candidates {
		doc := candidates[index].Document
		text := strings.ToLower(doc.Title + "\n" + doc.Content + "\n" + strings.Join(doc.Tags, " "))
		tokens := tokenSet(text)
		matches := 0
		for token := range queryTokens {
			if _, ok := tokens[token]; ok {
				matches++
			}
		}
		lexical := 0.0
		if len(queryTokens) > 0 {
			lexical = float64(matches) / float64(len(queryTokens))
		}
		for _, identifier := range identifiers {
			if strings.Contains(text, strings.ToLower(identifier)) || strings.EqualFold(doc.DocID, identifier) || strings.EqualFold(doc.ChunkID, identifier) {
				lexical += 0.75
			}
		}
		lexical = clamp01(lexical)
		candidates[index].RerankScore = &lexical
		candidates[index].RerankMode = "lexical"
		candidates[index].Score += lexical
	}
	sortRetrieved(candidates)
}

func diversifyChunks(candidates []RetrievedDocument, limit, maxPerDoc int) []RetrievedDocument {
	if maxPerDoc <= 0 {
		maxPerDoc = 1
	}
	result := make([]RetrievedDocument, 0, minInt(limit, len(candidates)))
	counts := make(map[string]int)
	seenSections := make(map[string]bool)
	for _, candidate := range candidates {
		if candidate.Document.SectionID != "" && seenSections[candidate.Document.SectionID] {
			continue
		}
		docID := candidate.Document.DocID
		if docID == "" {
			docID = candidate.Document.ID
		}
		if counts[docID] >= maxPerDoc {
			continue
		}
		counts[docID]++
		if candidate.Document.SectionID != "" {
			seenSections[candidate.Document.SectionID] = true
		}
		result = append(result, candidate)
		if limit > 0 && len(result) == limit {
			break
		}
	}
	return result
}

var identifierPattern = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_-]{3,}`)

func exactIdentifiers(query string) []string {
	matches := identifierPattern.FindAllString(query, -1)
	seen := make(map[string]struct{}, len(matches))
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		key := strings.ToLower(match)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, match)
	}
	return result
}

func tokenSet(text string) map[string]struct{} {
	result := make(map[string]struct{})
	var word []rune
	flush := func() {
		if len(word) > 0 {
			result[strings.ToLower(string(word))] = struct{}{}
			word = word[:0]
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			word = append(word, r)
			if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
				result[strings.ToLower(string(r))] = struct{}{}
			}
		} else {
			flush()
		}
	}
	flush()
	return result
}

func markDegraded(items []RetrievedDocument, channel string, err error) []RetrievedDocument {
	reason := channel + " recall failed"
	if err != nil {
		reason += ": " + truncateError(err.Error(), 240)
	}
	for index := range items {
		items[index].Degraded = true
		items[index].DegradationReason = reason
	}
	return items
}

func markRerankerFallback(items []RetrievedDocument, reason string) []RetrievedDocument {
	for index := range items {
		items[index].Degraded = true
		items[index].DegradationReason = "model reranker failed; lexical fallback: " + truncateError(reason, 240)
		items[index].RerankMode = "lexical_fallback"
	}
	return items
}

func sortRetrieved(items []RetrievedDocument) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score == items[j].Score {
			return candidateIdentity(items[i].Document) < candidateIdentity(items[j].Document)
		}
		return items[i].Score > items[j].Score
	})
}

func generateDocID(title, content string) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(title) + "\x00" + content))
	return hex.EncodeToString(hash[:16])
}

func cloneMetadata(input map[string]string) map[string]string {
	if len(input) == 0 {
		return map[string]string{}
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func stringField(values map[string]interface{}, key string) string {
	value, _ := values[key].(string)
	return value
}
func intField(values map[string]interface{}, key string) int {
	switch value := values[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}
func stringSliceField(value interface{}) []string {
	switch values := value.(type) {
	case []interface{}:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	case []string:
		return append([]string(nil), values...)
	default:
		return []string{}
	}
}
func truncateError(value string, max int) string {
	runes := []rune(value)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return value
}
func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
