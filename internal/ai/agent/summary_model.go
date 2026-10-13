package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"shanyangcode.com/pilot/internal/ai/models"
	"shanyangcode.com/pilot/internal/ai/progress"
)

var errEmptySummary = errors.New("summary model returned no usable text")

type summaryChatModel struct {
	model.BaseChatModel
	options []model.Option
}

func newSummaryModel(cm *models.ChatModel) model.BaseChatModel {
	return &summaryChatModel{BaseChatModel: cm.GetModel(), options: cm.BackgroundOptions()}
}

func (m *summaryChatModel) Generate(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.Message, error) {
	// A bounded summary needs text, not a reasoning-only response. Apply this
	// only to the summary invocation; never expose reasoning as a substitute.
	options = append(append([]model.Option(nil), options...), m.options...)
	response, err := m.BaseChatModel.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	if response == nil || strings.TrimSpace(response.Content) == "" {
		return nil, errEmptySummary
	}
	return response, nil
}

func summaryRetryConfig() *summarization.RetryConfig {
	limit := maxModelRetries
	return &summarization.RetryConfig{
		MaxRetries: &limit,
		ShouldRetry: func(ctx context.Context, _ *schema.Message, err error) bool {
			return ctx.Err() == nil && (errors.Is(err, errEmptySummary) || retryableModelError(ctx, err))
		},
		BackoffFunc: func(ctx context.Context, attempt int, _ *schema.Message, _ error) time.Duration {
			progress.Emit(ctx, progress.Event{Kind: "notice", Status: "warning", Summary: "历史摘要暂未生成，正在重试"})
			return modelRetryBackoff(ctx, attempt)
		},
	}
}
