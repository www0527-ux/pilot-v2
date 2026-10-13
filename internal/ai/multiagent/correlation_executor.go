package multiagent

import (
	"context"
	"fmt"
	"time"

	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/internal/observability/correlate"
)

// CorrelationExecutor runs heuristic cross-signal correlation as an evidence-compatible finding.
type CorrelationExecutor struct {
	engine *correlate.Engine
	window time.Duration
	now    func() time.Time
}

// NewCorrelationExecutor wraps a correlate.Engine.
func NewCorrelationExecutor(engine *correlate.Engine) *CorrelationExecutor {
	return &CorrelationExecutor{engine: engine, window: 30 * time.Minute, now: time.Now}
}

// WithWindow overrides the lookback window.
func (e *CorrelationExecutor) WithWindow(window time.Duration) *CorrelationExecutor {
	if e != nil && window > 0 {
		e.window = window
	}
	return e
}

// Execute implements Executor.
func (e *CorrelationExecutor) Execute(ctx context.Context, _ string, plan Plan) (Finding, error) {
	if e == nil || e.engine == nil {
		return Finding{}, fmt.Errorf("correlation engine is unavailable")
	}
	window := e.window
	if window <= 0 {
		window = 30 * time.Minute
	}
	if requested := metadataInt(plan.Metadata, "duration_minutes"); requested > 0 {
		window = time.Duration(requested) * time.Minute
	}
	windowMultiplier := evidenceWindowMultiplier(plan)
	window *= time.Duration(windowMultiplier)
	finish := progress.StartTool(ctx, "correlate_signals", map[string]any{"service": plan.Service, "duration_minutes": int(window / time.Minute)})
	result, err := e.engine.Correlate(ctx, correlate.Request{
		Service: plan.Service,
		Window:  window,
		Now:     e.now(),
	})
	if err != nil {
		finish("failed", progress.Redact(err.Error()), nil)
		return Finding{}, err
	}
	finish("completed", fmt.Sprintf("关联到 %d 条证据，%d 个候选原因", len(result.Evidence), len(result.Hypotheses)), result)
	hypotheses := make(map[string]correlate.Hypothesis, len(result.Hypotheses))
	for _, hypothesis := range result.Hypotheses {
		hypotheses[hypothesis.ID] = hypothesis
	}
	evidence := make([]EvidenceItem, 0, len(result.Evidence))
	for _, item := range result.Evidence {
		score := item.Score
		metadata := map[string]any{
			"correlation":  true,
			"kind":         item.Kind,
			"stance":       item.Stance,
			"claim_family": item.Claim,
			"service":      item.Service,
			"observed_at":  item.ObservedAt,
		}
		if hypothesis, ok := hypotheses[item.ID]; ok {
			metadata["kind"] = "hypothesis"
			metadata["confidence"] = hypothesis.Confidence
			metadata["support_ids"] = hypothesis.SupportIDs
			metadata["counter_ids"] = hypothesis.CounterIDs
			metadata["signal_kinds"] = hypothesis.SignalKinds
			metadata["claim_families"] = hypothesis.ClaimFamilies
			metadata["lead_signal"] = hypothesis.LeadSignal
			metadata["lag_seconds"] = hypothesis.LagSeconds
			metadata["method"] = hypothesis.Method
			metadata["topology_prior"] = hypothesis.NeighborSpread
		}
		evidence = append(evidence, EvidenceItem{
			ID:       item.ID,
			Source:   item.Source,
			Content:  item.Content,
			Score:    &score,
			Metadata: metadata,
		})
	}
	metadata := map[string]any{
		"mode":              "correlation",
		"method":            "robust_temporal_evidence_graph_v1",
		"hypothesis_count":  len(result.Hypotheses),
		"window_minutes":    int(window / time.Minute),
		"window_multiplier": windowMultiplier,
		"data_gaps":         result.DataGaps,
	}
	if len(result.DataGaps) > 0 {
		metadata["partial_failure"] = true
		metadata["correlation_data_gaps"] = result.DataGaps
	}
	return Finding{
		Role:     RoleEvidence,
		Summary:  fmt.Sprintf("Correlated %d temporal evidence hypothesis(es) over %s window (robust MAD + lead/lag graph; not a causal model).", len(result.Hypotheses), window),
		Evidence: evidence,
		Metadata: metadata,
	}, nil
}

// MergingEvidenceExecutor runs primary evidence then appends correlation evidence when enabled.
type MergingEvidenceExecutor struct {
	primary     Executor
	correlation Executor
}

// NewMergingEvidenceExecutor combines deterministic/react evidence with correlation.
func NewMergingEvidenceExecutor(primary, correlation Executor) *MergingEvidenceExecutor {
	return &MergingEvidenceExecutor{primary: primary, correlation: correlation}
}

// Execute implements Executor.
func (e *MergingEvidenceExecutor) Execute(ctx context.Context, query string, plan Plan) (Finding, error) {
	if e == nil || e.primary == nil {
		return Finding{}, fmt.Errorf("primary evidence executor is required")
	}
	primary, err := e.primary.Execute(ctx, query, plan)
	if err != nil {
		return Finding{}, err
	}
	if e.correlation == nil {
		return primary, nil
	}
	corr, corrErr := e.correlation.Execute(ctx, query, plan)
	if corrErr != nil {
		if primary.Metadata == nil {
			primary.Metadata = map[string]any{}
		}
		primary.Metadata["correlation_error"] = truncate(corrErr.Error(), 200)
		primary.Metadata["partial_failure"] = true
		return primary, nil
	}
	primary.Evidence = append(primary.Evidence, corr.Evidence...)
	primary.Summary = primary.Summary + " " + corr.Summary
	if primary.Metadata == nil {
		primary.Metadata = map[string]any{}
	}
	primary.Metadata["correlation"] = corr.Metadata
	if metadataBool(corr.Metadata, "partial_failure") {
		primary.Metadata["partial_failure"] = true
		primary.Metadata["correlation_data_gaps"] = corr.Metadata["correlation_data_gaps"]
	}
	return primary, nil
}
