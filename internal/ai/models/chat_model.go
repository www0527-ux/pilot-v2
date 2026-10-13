package models

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"shanyangcode.com/pilot/internal/config"
)

// ChatModel LLM 模型封装
type ChatModel struct {
	model             *openai.ChatModel
	modelName         string
	backgroundOptions []model.Option
}

// Short background classification/extraction has a bounded output budget.
// DeepSeek thinking tokens share that budget; disable thinking for these tasks
// only, preserving the chosen model and the Main Agent's runtime configuration.
func BackgroundOptions(cfg config.Model) []model.Option {
	endpoint, err := url.Parse(cfg.BaseURL)
	if err == nil && strings.EqualFold(endpoint.Hostname(), "api.deepseek.com") {
		return []model.Option{openai.WithExtraFields(map[string]any{"thinking": map[string]any{"type": "disabled"}})}
	}
	return nil
}

// NewChatModel 按配置创建 LLM 模型。
func NewChatModel(ctx context.Context, cfg config.Model) (*ChatModel, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("llm api_key is required (set llm.main_model.api_key or DASHSCOPE_API_KEY)")
	}

	temperature := float32(cfg.Temperature)
	maxTokens := cfg.MaxTokens
	modelConfig := &openai.ChatModelConfig{
		APIKey:      cfg.APIKey,
		BaseURL:     cfg.BaseURL,
		Model:       cfg.Model,
		Temperature: &temperature,
		MaxTokens:   &maxTokens,
	}

	chatModel, err := openai.NewChatModel(ctx, modelConfig)
	if err != nil {
		return nil, fmt.Errorf("create chat model failed: %w", err)
	}

	return &ChatModel{
		model:             chatModel,
		modelName:         cfg.Model,
		backgroundOptions: BackgroundOptions(cfg),
	}, nil
}

// GetModel 获取底层模型
func (m *ChatModel) GetModel() *openai.ChatModel {
	return m.model
}

// Name 返回配置中的模型名，用于指标与日志标注。
func (m *ChatModel) Name() string {
	return m.modelName
}

// BackgroundOptions returns provider-specific options for bounded summary /
// extraction tasks, without changing the main Agent's model configuration.
func (m *ChatModel) BackgroundOptions() []model.Option {
	return append([]model.Option(nil), m.backgroundOptions...)
}
