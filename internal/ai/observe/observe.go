// Package observe instruments Eino model and tool execution with the project's
// existing OpenTelemetry and Prometheus providers.
package observe

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	callbackutils "github.com/cloudwego/eino/utils/callbacks"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

var (
	registerOnce sync.Once

	llmRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pilot_llm_requests_total",
			Help: "Total number of Eino chat model requests.",
		},
		[]string{"model", "status"},
	)
	llmRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pilot_llm_request_duration_seconds",
			Help:    "Eino chat model request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"model"},
	)
	llmTokensTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pilot_llm_tokens_total",
			Help: "Tokens reported by Eino chat model responses.",
		},
		[]string{"model", "kind"},
	)
	toolCallsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pilot_tool_calls_total",
			Help: "Total number of Eino tool calls.",
		},
		[]string{"tool", "status"},
	)
	toolCallDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pilot_tool_call_duration_seconds",
			Help:    "Eino tool call duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"tool"},
	)
	embeddingRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pilot_embedding_requests_total",
			Help: "Total number of embedding model requests.",
		},
		[]string{"model", "status"},
	)
	embeddingRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pilot_embedding_request_duration_seconds",
			Help:    "Embedding model request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"model"},
	)
)

// Install registers metrics and appends an Eino callback handler once during
// process initialization. It must be called after the trace provider is ready.
func Install() {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			llmRequestsTotal,
			llmRequestDuration,
			llmTokensTotal,
			toolCallsTotal,
			toolCallDuration,
			embeddingRequestsTotal,
			embeddingRequestDuration,
		)
		callbacks.AppendGlobalHandlers(newObserver().handler())
	})
}

// RecordEmbedding 记录一次 Embedding 调用（不写入原文）。
func RecordEmbedding(modelName string, duration time.Duration, err error) {
	if modelName == "" {
		modelName = "unknown"
	}
	status := "success"
	if err != nil {
		status = "error"
	}
	embeddingRequestsTotal.WithLabelValues(modelName, status).Inc()
	embeddingRequestDuration.WithLabelValues(modelName).Observe(duration.Seconds())
}

type modelRunState struct {
	model     string
	startedAt time.Time
}

type toolRunState struct {
	name      string
	startedAt time.Time
}

type modelRunStateKey struct{}
type toolRunStateKey struct{}

type observer struct {
	tracer oteltrace.Tracer
}

func newObserver() *observer {
	return &observer{tracer: otel.Tracer("pilot/ai/eino")}
}

func (o *observer) handler() callbacks.Handler {
	return callbackutils.NewHandlerHelper().
		ChatModel(&callbackutils.ModelCallbackHandler{
			OnStart:               o.onModelStart,
			OnEnd:                 o.onModelEnd,
			OnEndWithStreamOutput: o.onModelEndWithStreamOutput,
			OnError:               o.onModelError,
		}).
		Tool(&callbackutils.ToolCallbackHandler{
			OnStart:               o.onToolStart,
			OnEnd:                 o.onToolEnd,
			OnEndWithStreamOutput: o.onToolEndWithStreamOutput,
			OnError:               o.onToolError,
		}).
		Handler()
}

func (o *observer) onModelStart(ctx context.Context, runInfo *callbacks.RunInfo, input *model.CallbackInput) context.Context {
	modelName := modelName(input)
	ctx, span := o.tracer.Start(ctx, "chat "+modelName, oteltrace.WithSpanKind(oteltrace.SpanKindClient))
	span.SetAttributes(
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.provider.name", "openai_compatible"),
		attribute.String("gen_ai.request.model", modelName),
		attribute.String("eino.run.name", runName(runInfo)),
	)
	return context.WithValue(ctx, modelRunStateKey{}, modelRunState{model: modelName, startedAt: time.Now()})
}

func (o *observer) onModelEnd(ctx context.Context, _ *callbacks.RunInfo, output *model.CallbackOutput) context.Context {
	o.finishModel(ctx, output, nil)
	return ctx
}

func (o *observer) onModelEndWithStreamOutput(ctx context.Context, _ *callbacks.RunInfo, output *schema.StreamReader[*model.CallbackOutput]) context.Context {
	go func() {
		defer output.Close()

		var latest *model.CallbackOutput
		for {
			item, err := output.Recv()
			if err == io.EOF {
				o.finishModel(ctx, latest, nil)
				return
			}
			if err != nil {
				o.finishModel(ctx, latest, err)
				return
			}
			if item != nil {
				latest = item
			}
		}
	}()
	return ctx
}

func (o *observer) onModelError(ctx context.Context, _ *callbacks.RunInfo, err error) context.Context {
	o.finishModel(ctx, nil, err)
	return ctx
}

func (o *observer) finishModel(ctx context.Context, output *model.CallbackOutput, err error) {
	state, ok := ctx.Value(modelRunStateKey{}).(modelRunState)
	if !ok {
		return
	}

	status := "success"
	span := oteltrace.SpanFromContext(ctx)
	if err != nil {
		status = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, "model invocation failed")
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
	}

	llmRequestsTotal.WithLabelValues(state.model, status).Inc()
	llmRequestDuration.WithLabelValues(state.model).Observe(time.Since(state.startedAt).Seconds())
	if output != nil && output.TokenUsage != nil {
		usage := output.TokenUsage
		llmTokensTotal.WithLabelValues(state.model, "prompt").Add(float64(usage.PromptTokens))
		llmTokensTotal.WithLabelValues(state.model, "completion").Add(float64(usage.CompletionTokens))
		llmTokensTotal.WithLabelValues(state.model, "total").Add(float64(usage.TotalTokens))
		span.SetAttributes(
			attribute.Int("gen_ai.usage.input_tokens", usage.PromptTokens),
			attribute.Int("gen_ai.usage.output_tokens", usage.CompletionTokens),
			attribute.Int("gen_ai.usage.cache_read.input_tokens", usage.PromptTokenDetails.CachedTokens),
			attribute.Int("gen_ai.usage.reasoning.output_tokens", usage.CompletionTokensDetails.ReasoningTokens),
		)
	}
	span.End()
}

func (o *observer) onToolStart(ctx context.Context, runInfo *callbacks.RunInfo, _ *tool.CallbackInput) context.Context {
	toolName := runName(runInfo)
	ctx, span := o.tracer.Start(ctx, "execute_tool "+toolName, oteltrace.WithSpanKind(oteltrace.SpanKindInternal))
	span.SetAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", toolName),
		attribute.String("gen_ai.tool.type", "extension"),
		attribute.String("eino.run.name", toolName),
	)
	return context.WithValue(ctx, toolRunStateKey{}, toolRunState{name: toolName, startedAt: time.Now()})
}

func (o *observer) onToolEnd(ctx context.Context, _ *callbacks.RunInfo, _ *tool.CallbackOutput) context.Context {
	o.finishTool(ctx, nil)
	return ctx
}

func (o *observer) onToolEndWithStreamOutput(ctx context.Context, _ *callbacks.RunInfo, output *schema.StreamReader[*tool.CallbackOutput]) context.Context {
	go func() {
		defer output.Close()

		for {
			_, err := output.Recv()
			if err == io.EOF {
				o.finishTool(ctx, nil)
				return
			}
			if err != nil {
				o.finishTool(ctx, err)
				return
			}
		}
	}()
	return ctx
}

func (o *observer) onToolError(ctx context.Context, _ *callbacks.RunInfo, err error) context.Context {
	o.finishTool(ctx, err)
	return ctx
}

func (o *observer) finishTool(ctx context.Context, err error) {
	state, ok := ctx.Value(toolRunStateKey{}).(toolRunState)
	if !ok {
		return
	}

	status := "success"
	span := oteltrace.SpanFromContext(ctx)
	if err != nil {
		status = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, "tool execution failed")
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
	}

	toolCallsTotal.WithLabelValues(state.name, status).Inc()
	toolCallDuration.WithLabelValues(state.name).Observe(time.Since(state.startedAt).Seconds())
	span.End()
}

func modelName(input *model.CallbackInput) string {
	if input != nil && input.Config != nil && input.Config.Model != "" {
		return input.Config.Model
	}
	return "unknown"
}

func runName(runInfo *callbacks.RunInfo) string {
	if runInfo != nil && runInfo.Name != "" {
		return runInfo.Name
	}
	return "unknown"
}
