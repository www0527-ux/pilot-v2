package correlate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shanyangcode.com/pilot/pkg/types"
)

type fakeSource struct {
	alerts  []types.AlertInfo
	logs    []types.LogEntry
	metrics []types.MetricData
}

func (f *fakeSource) GetAlerts(context.Context, map[string]string) ([]types.AlertInfo, error) {
	return f.alerts, nil
}
func (f *fakeSource) QueryMetrics(context.Context, string, time.Time, time.Time) ([]types.MetricData, error) {
	return f.metrics, nil
}
func (f *fakeSource) QueryLogs(context.Context, string, string, string, time.Time, time.Time, int) ([]types.LogEntry, error) {
	return f.logs, nil
}

func TestCorrelateMultiSignalCooccurrence(t *testing.T) {
	now := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	src := &fakeSource{
		alerts: []types.AlertInfo{{
			Name: "HighErrorRate", Severity: "critical", Status: "firing",
			Description: "error rate > 5%", StartTime: now.Add(-3 * time.Minute),
			Labels: map[string]string{"service": "checkout"},
		}},
		logs: []types.LogEntry{{
			Timestamp: now.Add(-2 * time.Minute), Level: "error", Message: "payment timeout",
			Service: "checkout",
		}},
		metrics: []types.MetricData{{
			Name: "http_error_ratio", Value: 12, Timestamp: now.Add(-2 * time.Minute),
			Labels: map[string]string{"service": "checkout"},
		}},
	}
	engine := NewEngine(src, StaticNeighbors{"checkout": {"mysql", "redis"}})
	result, err := engine.Correlate(context.Background(), Request{
		Service: "checkout", Window: 30 * time.Minute, Bucket: 2 * time.Minute, Now: now,
	})
	if err != nil {
		t.Fatalf("Correlate: %v", err)
	}
	if len(result.Hypotheses) == 0 {
		t.Fatal("expected hypotheses")
	}
	top := result.Hypotheses[0]
	if top.Service != "checkout" {
		t.Fatalf("top service = %q", top.Service)
	}
	if len(top.SignalKinds) < 2 {
		t.Fatalf("expected multi-signal kinds, got %v", top.SignalKinds)
	}
	foundHyp := false
	for _, ev := range result.Evidence {
		if ev.ID == top.ID {
			foundHyp = true
			break
		}
	}
	if !foundHyp {
		t.Fatal("hypothesis evidence id missing from evidence list")
	}
}

func TestCorrelateNeighborSpread(t *testing.T) {
	now := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	src := &fakeSource{
		logs: []types.LogEntry{{
			Timestamp: now.Add(-1 * time.Minute), Level: "error", Message: "db fail",
			Service: "api",
		}},
		metrics: []types.MetricData{{
			Name: "db_latency_seconds", Value: 4.2, Timestamp: now.Add(-2 * time.Minute),
			Labels: map[string]string{"service": "mysql", "anomaly": "true"},
		}},
	}
	engine := NewEngine(src, StaticNeighbors{"api": {"mysql"}})
	result, err := engine.Correlate(context.Background(), Request{
		Service: "api", Window: 10 * time.Minute, Now: now,
	})
	if err != nil {
		t.Fatalf("Correlate: %v", err)
	}
	var sawNeighbor bool
	for _, h := range result.Hypotheses {
		if h.Service == "mysql" && h.NeighborSpread {
			sawNeighbor = true
		}
	}
	if !sawNeighbor {
		t.Fatalf("expected observed neighbor hypothesis with topology prior, got %+v", result.Hypotheses)
	}
}

func TestCorrelateDoesNotFabricateUnobservedNeighbor(t *testing.T) {
	now := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	src := &fakeSource{logs: []types.LogEntry{{
		Timestamp: now.Add(-time.Minute), Level: "error", Message: "request failed", Service: "api",
	}}}
	result, err := NewEngine(src, StaticNeighbors{"api": {"mysql"}}).Correlate(context.Background(), Request{
		Service: "api", Window: 10 * time.Minute, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, hypothesis := range result.Hypotheses {
		if hypothesis.Service == "mysql" {
			t.Fatalf("unobserved dependency must not become a hypothesis: %+v", result.Hypotheses)
		}
	}
}

func TestCorrelateUsesRobustMetricAnomalyWithoutCrossClaimCounter(t *testing.T) {
	now := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	metrics := []types.MetricData{
		{Name: "latency", Value: 10, Timestamp: now.Add(-5 * time.Minute), Labels: map[string]string{"service": "checkout", "instance": "a"}},
		{Name: "latency", Value: 10, Timestamp: now.Add(-4 * time.Minute), Labels: map[string]string{"service": "checkout", "instance": "a"}},
		{Name: "latency", Value: 11, Timestamp: now.Add(-3 * time.Minute), Labels: map[string]string{"service": "checkout", "instance": "a"}},
		{Name: "latency", Value: 10, Timestamp: now.Add(-2 * time.Minute), Labels: map[string]string{"service": "checkout", "instance": "a"}},
		{Name: "latency", Value: 100, Timestamp: now.Add(-time.Minute), Labels: map[string]string{"service": "checkout", "instance": "a"}},
		{Name: "up", Value: 1, Timestamp: now, Labels: map[string]string{"service": "checkout", "instance": "a"}},
	}
	result, err := NewEngine(&fakeSource{
		alerts:  []types.AlertInfo{{Name: "HighLatency", Severity: "warning", StartTime: now.Add(-2 * time.Minute), Labels: map[string]string{"service": "checkout"}}},
		metrics: metrics,
	}, nil).Correlate(context.Background(), Request{Service: "checkout", Window: 10 * time.Minute, Now: now, MetricQuery: "latency"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hypotheses) == 0 {
		t.Fatal("expected hypothesis")
	}
	top := result.Hypotheses[0]
	if len(top.CounterIDs) != 0 {
		t.Fatalf("healthy availability must not refute a latency hypothesis: %+v", top)
	}
	if top.Confidence <= 0 || top.Confidence >= 1 || top.Method != "robust_temporal_evidence_graph_v1" {
		t.Fatalf("confidence metadata = %+v", top)
	}
	var robustMetric bool
	for _, item := range result.Evidence {
		if item.Source == "metrics" && item.Stance == "support" && item.Content != "" {
			robustMetric = true
		}
	}
	if !robustMetric {
		t.Fatalf("robust metric anomaly missing: %+v", result.Evidence)
	}
}

func TestCorrelateScopesHealthyAvailabilityCounterToAvailabilityClaim(t *testing.T) {
	now := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	result, err := NewEngine(&fakeSource{
		alerts: []types.AlertInfo{{
			Name: "ServiceDown", Severity: "critical", Description: "checkout service unavailable",
			StartTime: now.Add(-2 * time.Minute), Labels: map[string]string{"service": "checkout"},
		}},
		metrics: []types.MetricData{{
			Name: "up", Value: 1, Timestamp: now, Labels: map[string]string{"service": "checkout", "instance": "a"},
		}},
	}, nil).Correlate(context.Background(), Request{Service: "checkout", Window: 10 * time.Minute, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hypotheses) == 0 {
		t.Fatal("expected availability hypothesis")
	}
	top := result.Hypotheses[0]
	if len(top.CounterIDs) != 1 || len(top.ClaimFamilies) != 1 || top.ClaimFamilies[0] != "availability" {
		t.Fatalf("claim-scoped availability counter missing: %+v", top)
	}
}

func TestResultJSONUsesStableSnakeCaseContract(t *testing.T) {
	raw, err := json.Marshal(Result{Hypotheses: []Hypothesis{{ID: "corr-hyp:0:api"}}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"hypotheses"`) || strings.Contains(text, `"Hypotheses"`) {
		t.Fatalf("unexpected JSON contract: %s", text)
	}
}
