package multiagent

import (
	"context"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"shanyangcode.com/pilot/internal/ai/progress"
)

var agentTracer = otel.Tracer("pilot/ai/multiagent")

var (
	agentMetricsOnce sync.Once
	agentRunsTotal   = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pilot_agent_runs_total",
		Help: "Completed multi-agent runs by runtime and verification decision.",
	}, []string{"runtime", "decision"})
	agentEvidenceConfidence = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pilot_agent_evidence_confidence",
		Help:    "Deterministic evidence sufficiency score for completed multi-agent runs.",
		Buckets: []float64{0, 0.25, 0.5, 0.65, 0.8, 0.9, 1},
	}, []string{"runtime"})
	agentReplansTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pilot_agent_replans_total",
		Help: "Bounded evidence replan rounds executed by runtime.",
	}, []string{"runtime"})
)

func installAgentMetrics() {
	agentMetricsOnce.Do(func() {
		prometheus.MustRegister(agentRunsTotal, agentEvidenceConfidence, agentReplansTotal)
	})
}

// startAgentInvocation follows the OpenTelemetry GenAI agent-span naming and
// attribute vocabulary without recording user input, prompts, or tool payloads.
func startAgentInvocation(ctx context.Context, runtime string) (context.Context, trace.Span) {
	installAgentMetrics()
	return agentTracer.Start(
		ctx,
		"invoke_agent pilot.oncall",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "invoke_agent"),
			attribute.String("gen_ai.agent.name", "pilot.oncall"),
			attribute.String("gen_ai.workflow.name", "oncall_evidence_graph"),
			attribute.String("pilot.agent.runtime", runtime),
		),
	)
}

func startPlanningSpan(ctx context.Context, runtime string) (context.Context, trace.Span) {
	ctx, span := agentTracer.Start(
		ctx,
		"plan pilot.oncall",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "plan"),
			attribute.String("gen_ai.agent.name", "pilot.oncall"),
			attribute.String("pilot.agent.runtime", runtime),
		),
	)
	return ctx, &progressSpan{Span: span, complete: progress.Start(ctx, "role", "triage", nil), status: "completed", summary: "执行计划已生成"}
}

func startRoleInvocation(ctx context.Context, role Role, runtime string) (context.Context, trace.Span) {
	ctx = progress.WithGroup(ctx, string(role), string(role))
	name := "pilot." + string(role)
	ctx, span := agentTracer.Start(
		ctx,
		"invoke_agent "+name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "invoke_agent"),
			attribute.String("gen_ai.agent.name", name),
			attribute.String("pilot.agent.role", string(role)),
			attribute.String("pilot.agent.runtime", runtime),
		),
	)
	return ctx, &progressSpan{Span: span, complete: progress.Start(ctx, "role", string(role), nil), status: "completed"}
}

type progressSpan struct {
	trace.Span
	complete        func(string, string)
	status, summary string
}

func (span *progressSpan) End(options ...trace.SpanEndOption) {
	span.complete(span.status, span.summary)
	span.Span.End(options...)
}

func finishRoleSpan(span trace.Span, finding Finding, err error) {
	if span == nil {
		return
	}
	if observed, ok := span.(*progressSpan); ok {
		observed.summary = finding.Summary
		if err != nil {
			observed.status, observed.summary = "failed", progress.Redact(err.Error())
		}
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "role execution failed")
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
	} else {
		span.SetAttributes(attribute.Int("pilot.agent.evidence.count", len(finding.Evidence)))
	}
	span.End()
}

func finishAgentSpan(span trace.Span, result Result) {
	if span == nil {
		return
	}
	span.SetAttributes(
		attribute.Int("pilot.agent.finding.count", len(result.Findings)),
		attribute.Int("pilot.agent.limitation.count", len(result.Limitations)),
	)
	runtime := RuntimeLegacy
	if result.Metadata != nil {
		if configured, _ := result.Metadata["runtime"].(string); configured != "" {
			runtime = configured
		}
	}
	decision := "unverified"
	if result.Verification != nil {
		decision = result.Verification.Decision
		span.SetAttributes(
			attribute.Float64("pilot.agent.evidence.confidence", result.Verification.Assessment.Score),
			attribute.String("pilot.agent.verification.decision", result.Verification.Decision),
			attribute.Bool("pilot.agent.citation.passed", result.Verification.Citations.Passed),
		)
		agentEvidenceConfidence.WithLabelValues(runtime).Observe(result.Verification.Assessment.Score)
	}
	agentRunsTotal.WithLabelValues(runtime, decision).Inc()
	if replans := metadataInt(result.Metadata, "replan_rounds"); replans > 0 {
		agentReplansTotal.WithLabelValues(runtime).Add(float64(replans))
	}
	span.End()
}
