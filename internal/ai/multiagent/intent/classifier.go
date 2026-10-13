package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"shanyangcode.com/pilot/internal/prompts"
)

// Classifier produces a structured intent classification for multi-agent routing.
type Classifier interface {
	Classify(ctx context.Context, query string) (Classification, error)
}

// LLMClassifier calls a chat model once and parses JSON classification.
type LLMClassifier struct {
	model   model.BaseChatModel
	prompts *prompts.Catalog
}

// NewLLMClassifier constructs a classifier bound to the given chat model.
func NewLLMClassifier(chatModel model.BaseChatModel) (*LLMClassifier, error) {
	if chatModel == nil {
		return nil, fmt.Errorf("intent classifier model is required")
	}
	catalog, err := prompts.Default()
	if err != nil {
		return nil, fmt.Errorf("load intent prompts: %w", err)
	}
	return &LLMClassifier{model: chatModel, prompts: catalog}, nil
}

// Classify returns a validated classification or an error on model/parse failure.
func (c *LLMClassifier) Classify(ctx context.Context, query string) (Classification, error) {
	if c == nil || c.model == nil {
		return Classification{}, fmt.Errorf("intent classifier is unavailable")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return Classification{}, fmt.Errorf("query cannot be empty")
	}

	systemPrompt, err := c.prompts.Render("intent_classifier_system", nil)
	if err != nil {
		return Classification{}, err
	}
	userPrompt, err := c.prompts.Render("intent_classifier_user", map[string]any{"query": query})
	if err != nil {
		return Classification{}, err
	}
	result, err := c.model.Generate(ctx, []*schema.Message{
		{Role: schema.System, Content: systemPrompt},
		{Role: schema.User, Content: userPrompt},
	})
	if err != nil {
		return Classification{}, fmt.Errorf("classify intent: %w", err)
	}
	if result == nil || strings.TrimSpace(result.Content) == "" {
		return Classification{}, fmt.Errorf("intent classifier returned an empty response")
	}

	parsed, err := parseClassificationJSON(result.Content)
	if err != nil {
		return Classification{}, err
	}
	parsed.Source = SourceLLM
	return parsed, nil
}

func parseClassificationJSON(raw string) (Classification, error) {
	payload := extractJSONObject(raw)
	var parsed Classification
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return Classification{}, fmt.Errorf("parse intent json: %w", err)
	}
	parsed.Intent = strings.ToLower(strings.TrimSpace(parsed.Intent))
	parsed.Service = strings.TrimSpace(parsed.Service)
	parsed.Reason = strings.TrimSpace(parsed.Reason)
	if !ValidIntent(parsed.Intent) {
		return Classification{}, fmt.Errorf("unsupported intent %q", parsed.Intent)
	}
	if parsed.Confidence < 0 {
		parsed.Confidence = 0
	}
	if parsed.Confidence > 1 {
		parsed.Confidence = 1
	}
	roles := make([]string, 0, len(parsed.SelectedRoles))
	for _, role := range parsed.SelectedRoles {
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "" {
			continue
		}
		roles = append(roles, role)
	}
	parsed.SelectedRoles = roles
	return parsed, nil
}

func extractJSONObject(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```JSON")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		return raw[start : end+1]
	}
	return raw
}
