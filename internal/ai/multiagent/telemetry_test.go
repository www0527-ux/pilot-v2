package multiagent

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestOrchestratorEmitsGenAIAgentSpansWithoutInputContent(t *testing.T) {
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})

	orch, err := NewOrchestrator(plannerStub{plan: Plan{
		Intent:        "incident_triage",
		Service:       "checkout",
		SelectedRoles: []Role{RoleEvidence, RoleSynthesis},
	}}, map[Role]Executor{
		RoleEvidence: executorStub{finding: Finding{Summary: "alert", Evidence: []EvidenceItem{{ID: "alert:1", Source: "alerts", Content: "sensitive-log-body"}}}},
	}, &synthesizerStub{answer: "Observed [alert:1]."})
	if err != nil {
		t.Fatal(err)
	}
	orch.verification = VerificationPolicy{Mode: VerificationModeEvidenceGraph, MinConfidence: 0.65, MinIndependentSources: 1}
	before := testutil.ToFloat64(agentRunsTotal.WithLabelValues(RuntimeLegacy, "accept"))
	if _, err := orch.Execute(context.Background(), "secret-user-query"); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(agentRunsTotal.WithLabelValues(RuntimeLegacy, "accept")); got != before+1 {
		t.Fatalf("agent run metric = %v, want %v", got, before+1)
	}

	spans := recorder.Ended()
	wanted := map[string]bool{
		"invoke_agent pilot.oncall":   false,
		"plan pilot.oncall":           false,
		"invoke_agent pilot.evidence": false,
	}
	for _, span := range spans {
		if _, ok := wanted[span.Name()]; ok {
			wanted[span.Name()] = true
		}
		for _, attr := range span.Attributes() {
			value := attr.Value.Emit()
			if value == "secret-user-query" || value == "sensitive-log-body" {
				t.Fatalf("span %q leaked input content in %s", span.Name(), attr.Key)
			}
		}
	}
	for name, found := range wanted {
		if !found {
			t.Fatalf("span %q missing; ended=%d", name, len(spans))
		}
	}
	invocation := findRecordedSpan(spans, "invoke_agent pilot.oncall")
	if invocation == nil || spanAttribute(invocation.Attributes(), "gen_ai.operation.name") != "invoke_agent" {
		t.Fatalf("GenAI invocation attributes missing: %+v", invocation)
	}
	if provider := spanAttribute(invocation.Attributes(), "gen_ai.provider.name"); provider != "" {
		t.Fatalf("in-process framework span must not pretend to be a model provider: %q", provider)
	}
}

func findRecordedSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	return nil
}

func spanAttribute(attributes []attribute.KeyValue, key attribute.Key) string {
	for _, item := range attributes {
		if item.Key == key {
			return item.Value.Emit()
		}
	}
	return ""
}
