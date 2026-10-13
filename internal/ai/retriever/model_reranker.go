package retriever

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"shanyangcode.com/pilot/internal/prompts"
	"shanyangcode.com/pilot/pkg/types"
)

// ChatModelReranker is the production model-backed reranker. It asks for a
// narrow JSON score contract and never accepts model-generated chunk IDs that
// were not in the supplied candidate set.
type ChatModelReranker struct {
	model   model.BaseChatModel
	prompts *prompts.Catalog
}

func NewChatModelReranker(chatModel model.BaseChatModel) (*ChatModelReranker, error) {
	if chatModel == nil {
		return nil, fmt.Errorf("reranker chat model is required")
	}
	catalog, err := prompts.Default()
	if err != nil {
		return nil, fmt.Errorf("load reranker prompts: %w", err)
	}
	return &ChatModelReranker{model: chatModel, prompts: catalog}, nil
}

func (r *ChatModelReranker) Rerank(ctx context.Context, query string, candidates []types.KnowledgeDocument) (map[string]float64, error) {
	if r == nil || r.model == nil {
		return nil, fmt.Errorf("reranker chat model is unavailable")
	}
	if len(candidates) == 0 {
		return map[string]float64{}, nil
	}
	var prompt strings.Builder
	prompt.WriteString("Query:\n")
	prompt.WriteString(query)
	prompt.WriteString("\n\nCandidates:\n")
	allowed := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		allowed[candidate.ChunkID] = struct{}{}
		prompt.WriteString("- chunk_id: ")
		prompt.WriteString(candidate.ChunkID)
		prompt.WriteString("\n  title: ")
		prompt.WriteString(truncateError(candidate.Title, 300))
		prompt.WriteString("\n  content: ")
		prompt.WriteString(truncateError(candidate.Content, 1200))
		prompt.WriteByte('\n')
	}
	systemPrompt, err := r.prompts.Render("rag_reranker_system", nil)
	if err != nil {
		return nil, err
	}
	userPrompt, err := r.prompts.Render("rag_reranker_user", map[string]any{"query": query, "candidates": strings.TrimPrefix(prompt.String(), "Query:\n"+query+"\n\nCandidates:\n")})
	if err != nil {
		return nil, err
	}
	response, err := r.model.Generate(ctx, []*schema.Message{
		{Role: schema.System, Content: systemPrompt},
		{Role: schema.User, Content: userPrompt},
	})
	if err != nil {
		return nil, fmt.Errorf("model rerank failed: %w", err)
	}
	if response == nil || strings.TrimSpace(response.Content) == "" {
		return nil, fmt.Errorf("model reranker returned an empty response")
	}
	raw := strings.TrimSpace(response.Content)
	start, end := strings.Index(raw, "["), strings.LastIndex(raw, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("model reranker response is not a JSON array")
	}
	var items []struct {
		ChunkID string  `json:"chunk_id"`
		Score   float64 `json:"score"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &items); err != nil {
		return nil, fmt.Errorf("parse model reranker response: %w", err)
	}
	scores := make(map[string]float64, len(items))
	for _, item := range items {
		if _, ok := allowed[item.ChunkID]; !ok {
			continue
		}
		scores[item.ChunkID] = clamp01(item.Score)
	}
	if len(scores) == 0 {
		return nil, fmt.Errorf("model reranker returned no valid candidate scores")
	}
	return scores, nil
}
