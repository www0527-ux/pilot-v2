package retriever

import (
	"fmt"
	"sort"
	"strings"

	"shanyangcode.com/pilot/pkg/types"
)

const (
	recallModeBM25   = "bm25"
	recallModeVector = "vector"
	recallModeHybrid = "hybrid"
)

// RecallConfig 控制知识库的多路召回与融合策略。
type RecallConfig struct {
	Mode           string
	BM25TopK       int
	VectorTopK     int
	FinalTopK      int
	RRFK           int
	FallbackToBM25 bool
}

// RetrievedDocument 包含最终文档及其可解释的召回信息。
type RetrievedDocument struct {
	Document          types.KnowledgeDocument `json:"document"`
	Score             float64                 `json:"score"`
	BM25Score         *float64                `json:"bm25_score,omitempty"`
	VectorScore       *float64                `json:"vector_score,omitempty"`
	FusedScore        *float64                `json:"fused_score,omitempty"`
	RerankScore       *float64                `json:"rerank_score,omitempty"`
	RerankMode        string                  `json:"rerank_mode,omitempty"`
	RecallMethods     []string                `json:"recall_methods"`
	Degraded          bool                    `json:"degraded"`
	DegradationReason string                  `json:"degradation_reason,omitempty"`
}

type recallCandidate struct {
	document types.KnowledgeDocument
	score    float64
}

func (c RecallConfig) normalized(defaultTopK int) (RecallConfig, error) {
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	if c.Mode == "" {
		c.Mode = recallModeHybrid
	}
	if c.Mode != recallModeBM25 && c.Mode != recallModeVector && c.Mode != recallModeHybrid {
		return RecallConfig{}, fmt.Errorf("unsupported recall mode %q", c.Mode)
	}
	if defaultTopK <= 0 {
		defaultTopK = 5
	}
	if c.BM25TopK <= 0 {
		c.BM25TopK = defaultTopK
	}
	if c.VectorTopK <= 0 {
		c.VectorTopK = defaultTopK
	}
	if c.FinalTopK <= 0 {
		c.FinalTopK = defaultTopK
	}
	if c.RRFK <= 0 {
		c.RRFK = 60
	}
	return c, nil
}

func fuseRRF(bm25, vector []recallCandidate, rrfK, limit int) []RetrievedDocument {
	type fused struct {
		document    types.KnowledgeDocument
		bm25Score   *float64
		vectorScore *float64
		fusedScore  float64
		methods     map[string]struct{}
	}

	byDocument := make(map[string]*fused, len(bm25)+len(vector))
	add := func(candidates []recallCandidate, method string) {
		for rank, candidate := range candidates {
			identity := candidateIdentity(candidate.document)
			entry, exists := byDocument[identity]
			if !exists {
				entry = &fused{
					document: candidate.document,
					methods:  make(map[string]struct{}, 2),
				}
				byDocument[identity] = entry
			}
			entry.methods[method] = struct{}{}
			entry.fusedScore += 1 / float64(rrfK+rank+1)
			score := candidate.score
			if method == recallModeBM25 {
				entry.bm25Score = &score
			} else {
				entry.vectorScore = &score
			}
		}
	}

	add(bm25, recallModeBM25)
	add(vector, recallModeVector)

	results := make([]RetrievedDocument, 0, len(byDocument))
	for _, entry := range byDocument {
		fusedScore := entry.fusedScore
		methods := make([]string, 0, len(entry.methods))
		for method := range entry.methods {
			methods = append(methods, method)
		}
		sort.Strings(methods)
		results = append(results, RetrievedDocument{
			Document:      entry.document,
			Score:         fusedScore,
			BM25Score:     entry.bm25Score,
			VectorScore:   entry.vectorScore,
			FusedScore:    &fusedScore,
			RecallMethods: methods,
		})
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return candidateIdentity(results[i].Document) < candidateIdentity(results[j].Document)
		}
		return results[i].Score > results[j].Score
	})
	if limit > 0 && len(results) > limit {
		return results[:limit]
	}
	return results
}

func singleRouteResults(candidates []recallCandidate, method string, limit int) []RetrievedDocument {
	results := make([]RetrievedDocument, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		identity := candidateIdentity(candidate.document)
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		score := candidate.score
		result := RetrievedDocument{
			Document:      candidate.document,
			Score:         score,
			RecallMethods: []string{method},
		}
		if method == recallModeBM25 {
			result.BM25Score = &score
		} else {
			result.VectorScore = &score
		}
		results = append(results, result)
	}
	if limit > 0 && len(results) > limit {
		return results[:limit]
	}
	return results
}

func candidateIdentity(document types.KnowledgeDocument) string {
	if document.ChunkID != "" {
		return "chunk:" + document.ChunkID
	}
	if document.ID != "" {
		return "id:" + document.ID
	}
	return "content:" + document.Title + "\x00" + document.Content
}
