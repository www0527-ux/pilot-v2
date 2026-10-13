package embedder

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino-ext/components/embedding/openai"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"shanyangcode.com/pilot/internal/ai/observe"
	"shanyangcode.com/pilot/internal/config"
)

// Embedder Embedding 模型封装
type Embedder struct {
	model     *openai.Embedder
	modelName string
	tracer    trace.Tracer
}

// NewEmbedder 创建 Embedder
func NewEmbedder(ctx context.Context, cfg config.Embedding) (*Embedder, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("embedding api_key is required (set embedding.api_key or DASHSCOPE_API_KEY)")
	}

	model, err := openai.NewEmbedder(ctx, &openai.EmbeddingConfig{
		APIKey:  cfg.APIKey,
		BaseURL: cfg.BaseURL,
		Model:   cfg.Model,
	})
	if err != nil {
		return nil, fmt.Errorf("create embedder failed: %w", err)
	}

	return &Embedder{
		model:     model,
		modelName: cfg.Model,
		tracer:    otel.Tracer("pilot/ai/embedder"),
	}, nil
}

// Embed 生成文本向量。只记录模型名与结果维度，不写入原文，避免泄露检索内容。
func (e *Embedder) Embed(ctx context.Context, text string) ([]float64, error) {
	started := time.Now()
	modelName := e.modelName
	if modelName == "" {
		modelName = "unknown"
	}
	ctx, span := e.tracer.Start(ctx, "embeddings "+modelName, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	span.SetAttributes(
		attribute.String("gen_ai.operation.name", "embeddings"),
		attribute.String("gen_ai.provider.name", "openai_compatible"),
		attribute.String("gen_ai.request.model", modelName),
		attribute.Int("embedding.input_count", 1),
	)

	result, err := e.model.EmbedStrings(ctx, []string{text})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "embedding invocation failed")
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
		observe.RecordEmbedding(modelName, time.Since(started), err)
		return nil, fmt.Errorf("embed text failed: %w", err)
	}

	if len(result) == 0 {
		err := fmt.Errorf("empty embedding result")
		span.RecordError(err)
		span.SetStatus(codes.Error, "embedding invocation failed")
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
		observe.RecordEmbedding(modelName, time.Since(started), err)
		return nil, err
	}

	span.SetAttributes(
		attribute.Int("gen_ai.embeddings.dimension.count", len(result[0])),
		attribute.Int("embedding.dimensions", len(result[0])),
	)
	observe.RecordEmbedding(modelName, time.Since(started), nil)
	return result[0], nil
}
