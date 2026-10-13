package observe

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestModelCallbacksRecordMetricsAndSpan(t *testing.T) {
	recorder := installTestTracer(t)
	observer := newObserver()
	const modelName = "callback-unit-model"

	before := testutil.ToFloat64(llmRequestsTotal.WithLabelValues(modelName, "success"))
	ctx := observer.onModelStart(context.Background(), &callbacks.RunInfo{Name: "Synthesis"}, &model.CallbackInput{
		Config: &model.Config{Model: modelName},
	})
	observer.onModelEnd(ctx, nil, &model.CallbackOutput{
		TokenUsage: &model.TokenUsage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
	})

	if got := testutil.ToFloat64(llmRequestsTotal.WithLabelValues(modelName, "success")); got != before+1 {
		t.Fatalf("successful model requests = %v, want %v", got, before+1)
	}
	if got := testutil.ToFloat64(llmTokensTotal.WithLabelValues(modelName, "total")); got < 8 {
		t.Fatalf("total tokens = %v, want at least 8", got)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if got := spans[0].Name(); got != "chat "+modelName {
		t.Fatalf("span name = %q, want chat model", got)
	}
	if got := traceAttribute(spans[0].Attributes(), "gen_ai.operation.name"); got != "chat" {
		t.Fatalf("gen_ai.operation.name = %q", got)
	}
}

func TestToolCallbacksRecordMetricsAndSpan(t *testing.T) {
	recorder := installTestTracer(t)
	observer := newObserver()
	const toolName = "query_alerts"

	before := testutil.ToFloat64(toolCallsTotal.WithLabelValues(toolName, "success"))
	ctx := observer.onToolStart(context.Background(), &callbacks.RunInfo{Name: toolName}, &tool.CallbackInput{ArgumentsInJSON: `{}`})
	observer.onToolEnd(ctx, nil, &tool.CallbackOutput{Response: `{"success":true}`})

	if got := testutil.ToFloat64(toolCallsTotal.WithLabelValues(toolName, "success")); got != before+1 {
		t.Fatalf("successful tool calls = %v, want %v", got, before+1)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if got := spans[0].Name(); got != "execute_tool "+toolName {
		t.Fatalf("span name = %q, want execute_tool", got)
	}
	if got := traceAttribute(spans[0].Attributes(), "gen_ai.tool.type"); got != "extension" {
		t.Fatalf("gen_ai.tool.type = %q", got)
	}
}

func TestModelStreamCallbackDrainsOutput(t *testing.T) {
	_ = installTestTracer(t)
	observer := newObserver()
	const modelName = "callback-stream-model"

	before := testutil.ToFloat64(llmRequestsTotal.WithLabelValues(modelName, "success"))
	ctx := observer.onModelStart(context.Background(), nil, &model.CallbackInput{Config: &model.Config{Model: modelName}})
	output := schema.StreamReaderFromArray([]*model.CallbackOutput{
		{TokenUsage: &model.TokenUsage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}},
	})
	observer.onModelEndWithStreamOutput(ctx, nil, output)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := testutil.ToFloat64(llmRequestsTotal.WithLabelValues(modelName, "success")); got == before+1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stream callback did not finish; requests = %v, want %v", testutil.ToFloat64(llmRequestsTotal.WithLabelValues(modelName, "success")), before+1)
}

func TestRecordEmbeddingMetrics(t *testing.T) {
	const modelName = "text-embedding-unit"
	beforeOK := testutil.ToFloat64(embeddingRequestsTotal.WithLabelValues(modelName, "success"))
	beforeErr := testutil.ToFloat64(embeddingRequestsTotal.WithLabelValues(modelName, "error"))

	RecordEmbedding(modelName, 12*time.Millisecond, nil)
	RecordEmbedding(modelName, 8*time.Millisecond, context.Canceled)

	if got := testutil.ToFloat64(embeddingRequestsTotal.WithLabelValues(modelName, "success")); got != beforeOK+1 {
		t.Fatalf("success embeddings = %v, want %v", got, beforeOK+1)
	}
	if got := testutil.ToFloat64(embeddingRequestsTotal.WithLabelValues(modelName, "error")); got != beforeErr+1 {
		t.Fatalf("error embeddings = %v, want %v", got, beforeErr+1)
	}
}

func installTestTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	return recorder
}

func traceAttribute(attributes []attribute.KeyValue, key attribute.Key) string {
	for _, item := range attributes {
		if item.Key == key {
			return item.Value.Emit()
		}
	}
	return ""
}
