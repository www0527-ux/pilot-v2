// Package correlate implements an explainable cross-signal correlation engine.
// It concurrently collects alerts, error-log bursts, and metric samples,
// detects robust metric outliers, aligns signals into time buckets, and uses
// static dependency edges only as priors for already observed nodes. This is
// heuristic temporal graph scoring — not a learned causal model.
package correlate

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"shanyangcode.com/pilot/pkg/types"
)

// SignalSource is the narrow observability surface required for correlation.
type SignalSource interface {
	GetAlerts(ctx context.Context, filters map[string]string) ([]types.AlertInfo, error)
	QueryMetrics(ctx context.Context, query string, start, end time.Time) ([]types.MetricData, error)
	QueryLogs(ctx context.Context, service, level, keyword string, start, end time.Time, limit int) ([]types.LogEntry, error)
}

// DependencyNeighbor maps a service to configured dependency labels (static graph).
type DependencyNeighbor interface {
	Neighbors(service string) []string
}

// Request is a correlation window query.
type Request struct {
	Service       string        `json:"service"`
	Window        time.Duration `json:"window"`
	Bucket        time.Duration `json:"bucket"`
	Now           time.Time     `json:"now"`
	MetricQuery   string        `json:"metric_query"`
	LogLimit      int           `json:"log_limit"`
	MaxHypotheses int           `json:"max_hypotheses"`
}

// Hypothesis is a ranked, citeable correlation finding.
type Hypothesis struct {
	ID             string   `json:"id"`
	Service        string   `json:"service"`
	Score          float64  `json:"score"`
	Confidence     float64  `json:"confidence"`
	Summary        string   `json:"summary"`
	SupportIDs     []string `json:"support_ids"`
	CounterIDs     []string `json:"counter_ids,omitempty"`
	BucketHits     int      `json:"bucket_hits"`
	SignalKinds    []string `json:"signal_kinds"`
	ClaimFamilies  []string `json:"claim_families"`
	LeadSignal     string   `json:"lead_signal,omitempty"`
	LagSeconds     int64    `json:"lag_seconds"`
	Method         string   `json:"method"`
	NeighborSpread bool     `json:"topology_prior"`
}

// Result is the full correlation output with supporting raw evidence snippets.
type Result struct {
	Hypotheses []Hypothesis  `json:"hypotheses"`
	Evidence   []Evidence    `json:"evidence"`
	Window     time.Duration `json:"window"`
	Service    string        `json:"service"`
	DataGaps   []string      `json:"data_gaps,omitempty"`
}

// Evidence is a citeable runtime snippet produced during correlation.
type Evidence struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	Content    string    `json:"content"`
	Score      float64   `json:"score"`
	ObservedAt time.Time `json:"observed_at"`
	Service    string    `json:"service"`
	Kind       string    `json:"kind"`   // signal | hypothesis
	Stance     string    `json:"stance"` // support | counter
	Claim      string    `json:"claim_family,omitempty"`
}

// Engine performs robust temporal evidence-graph scoring.
type Engine struct {
	source    SignalSource
	neighbors DependencyNeighbor
}

// NewEngine constructs a correlator. neighbors may be nil (no graph spread).
func NewEngine(source SignalSource, neighbors DependencyNeighbor) *Engine {
	return &Engine{source: source, neighbors: neighbors}
}

// Correlate collects signals and returns ranked hypotheses with evidence IDs.
func (e *Engine) Correlate(ctx context.Context, req Request) (Result, error) {
	if e == nil || e.source == nil {
		return Result{}, fmt.Errorf("correlate: signal source is required")
	}
	req = normalizeRequest(req)
	end := req.Now
	start := end.Add(-req.Window)

	// The three independent read-only sources are acquired concurrently. This
	// keeps the correlation critical path bounded by the slowest source instead
	// of their sum while preserving partial-result semantics.
	var (
		alerts     []types.AlertInfo
		logs       []types.LogEntry
		metrics    []types.MetricData
		alertsErr  error
		logsErr    error
		metricsErr error
		group      sync.WaitGroup
	)
	group.Add(3)
	go func() {
		defer group.Done()
		alerts, alertsErr = e.source.GetAlerts(ctx, map[string]string{"service": req.Service, "status": "firing"})
	}()
	go func() {
		defer group.Done()
		logs, logsErr = e.source.QueryLogs(ctx, req.Service, "error", "", start, end, req.LogLimit)
	}()
	go func() {
		defer group.Done()
		metrics, metricsErr = e.source.QueryMetrics(ctx, req.MetricQuery, start, end)
	}()
	group.Wait()

	if alertsErr != nil && logsErr != nil && metricsErr != nil {
		return Result{}, fmt.Errorf("correlate: alerts: %v; logs: %v; metrics: %v", alertsErr, logsErr, metricsErr)
	}
	dataGaps := make([]string, 0, 3)
	if alertsErr != nil {
		dataGaps = append(dataGaps, "alerts")
	}
	if logsErr != nil {
		dataGaps = append(dataGaps, "logs")
	}
	if metricsErr != nil {
		dataGaps = append(dataGaps, "metrics")
	}

	type bucketKey struct {
		service string
		bucket  int64
	}
	type bucketAgg struct {
		counts map[string]int
		claims map[string]bool
		ids    []string
		weight float64
		first  map[string]time.Time
	}

	evidence := make([]Evidence, 0, 32)
	buckets := map[bucketKey]*bucketAgg{}
	countersByService := map[string]map[string][]string{}
	bump := func(service string, ts time.Time, kind, stance, claim, id, content string, score float64) {
		service = firstNonEmpty(service, req.Service, "unknown")
		claim = firstNonEmpty(claim, "incident")
		ts = observationTime(ts, start, end)
		evidence = append(evidence, Evidence{
			ID:         id,
			Source:     kind,
			Content:    content,
			Score:      round3(score),
			ObservedAt: ts,
			Service:    service,
			Kind:       "signal",
			Stance:     stance,
			Claim:      claim,
		})
		if stance == "counter" {
			if countersByService[service] == nil {
				countersByService[service] = map[string][]string{}
			}
			countersByService[service][claim] = appendUnique(countersByService[service][claim], id)
			return
		}
		b := ts.Unix() / int64(req.Bucket.Seconds())
		key := bucketKey{service: service, bucket: b}
		agg := buckets[key]
		if agg == nil {
			agg = &bucketAgg{counts: map[string]int{}, claims: map[string]bool{}, first: map[string]time.Time{}}
			buckets[key] = agg
		}
		agg.counts[kind]++
		agg.claims[claim] = true
		agg.weight += clamp(score, 0, 1)
		if previous := agg.first[kind]; previous.IsZero() || ts.Before(previous) {
			agg.first[kind] = ts
		}
		agg.ids = append(agg.ids, id)
	}

	for i, alert := range alerts {
		svc := firstNonEmpty(alert.Labels["service"], req.Service, "unknown")
		id := fmt.Sprintf("corr-alert:%d:%s", i, alert.Name)
		content := fmt.Sprintf("%s [%s]: %s", alert.Name, alert.Severity, alert.Description)
		bump(svc, alert.StartTime, "alerts", "support", signalClaimFamily("alerts", content), id, content, alertReliability(alert.Severity))
	}
	for i, log := range logs {
		svc := firstNonEmpty(log.Service, req.Service, "unknown")
		id := fmt.Sprintf("corr-log:%d:%s", i, log.Timestamp.UTC().Format(time.RFC3339Nano))
		score := 0.72
		if strings.TrimSpace(log.TraceID) != "" {
			score = 0.82
		}
		bump(svc, log.Timestamp, "logs", "support", signalClaimFamily("logs", log.Message), id, truncate(fmt.Sprintf("%s %s: %s", log.Timestamp.Format(time.RFC3339), log.Level, log.Message), 400), score)
	}
	for _, signal := range classifyMetricSignals(metrics, req.Service) {
		bump(
			signal.service,
			signal.timestamp,
			"metrics",
			signal.stance,
			signal.claim,
			signal.id,
			fmt.Sprintf("%s=%.4f @ %s (%s)", signal.name, signal.value, signal.timestamp.Format(time.RFC3339), signal.reason),
			signal.score,
		)
	}

	type serviceScore struct {
		service        string
		score          float64
		hits           int
		kinds          map[string]bool
		claims         map[string]bool
		support        []string
		first          map[string]time.Time
		supportWeight  float64
		supportCount   int
		neighborSpread bool
	}
	byService := map[string]*serviceScore{}
	for key, agg := range buckets {
		kinds := len(agg.counts)
		// Co-occurrence: reward multi-signal buckets; single-signal still contributes weakly.
		cooccur := float64(kinds)
		if kinds >= 2 {
			cooccur += 1.5
		}
		intensity := 0
		for _, count := range agg.counts {
			intensity += count
		}
		delta := cooccur*2 + math.Log1p(float64(intensity)) + agg.weight/math.Max(float64(intensity), 1)
		ss := byService[key.service]
		if ss == nil {
			ss = &serviceScore{service: key.service, kinds: map[string]bool{}, claims: map[string]bool{}, first: map[string]time.Time{}}
			byService[key.service] = ss
		}
		ss.score += delta
		ss.hits++
		for kind, first := range agg.first {
			ss.kinds[kind] = true
			if previous := ss.first[kind]; previous.IsZero() || first.Before(previous) {
				ss.first[kind] = first
			}
		}
		for claim := range agg.claims {
			ss.claims[claim] = true
		}
		ss.support = appendUnique(ss.support, agg.ids...)
		ss.supportWeight += agg.weight
		ss.supportCount += intensity
	}

	// A topology edge acts only as a prior over an already observed neighbor.
	// Never fabricate a dependency hypothesis by spreading a target service's
	// score onto a neighbor for which no runtime signal was observed.
	if e.neighbors != nil && strings.TrimSpace(req.Service) != "" {
		for _, n := range e.neighbors.Neighbors(req.Service) {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			ss := byService[n]
			if ss != nil && ss.score > 0 {
				ss.score *= 1.10
				ss.neighborSpread = true
			}
		}
	}

	ranked := make([]*serviceScore, 0, len(byService))
	for _, ss := range byService {
		if ss.score <= 0 {
			continue
		}
		ranked = append(ranked, ss)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].service < ranked[j].service
		}
		return ranked[i].score > ranked[j].score
	})
	if len(ranked) > req.MaxHypotheses {
		ranked = ranked[:req.MaxHypotheses]
	}

	hypotheses := make([]Hypothesis, 0, len(ranked))
	for i, ss := range ranked {
		kinds := sortedKeys(ss.kinds)
		claims := sortedKeys(ss.claims)
		sort.Strings(ss.support)
		id := fmt.Sprintf("corr-hyp:%d:%s", i, ss.service)
		leadSignal, lagSeconds := temporalOrder(ss.first)
		counterIDs := make([]string, 0)
		for _, claim := range claims {
			counterIDs = appendUnique(counterIDs, countersByService[ss.service][claim]...)
		}
		sort.Strings(counterIDs)
		confidence := hypothesisConfidence(ss.score, kinds, ss.supportWeight, ss.supportCount, lagSeconds, req.Window, len(counterIDs), ss.neighborSpread)
		hypotheses = append(hypotheses, Hypothesis{
			ID:             id,
			Service:        ss.service,
			Score:          round2(ss.score),
			Confidence:     round3(confidence),
			Summary:        fmt.Sprintf("Temporal evidence hypothesis for %s (confidence=%.3f, score=%.2f, buckets=%d, signals=%s, claims=%s, lead=%s, lag=%ds)", ss.service, confidence, ss.score, ss.hits, strings.Join(kinds, "+"), strings.Join(claims, "+"), firstNonEmpty(leadSignal, "unknown"), lagSeconds),
			SupportIDs:     ss.support,
			CounterIDs:     counterIDs,
			BucketHits:     ss.hits,
			SignalKinds:    kinds,
			ClaimFamilies:  claims,
			LeadSignal:     leadSignal,
			LagSeconds:     lagSeconds,
			Method:         "robust_temporal_evidence_graph_v1",
			NeighborSpread: ss.neighborSpread,
		})
		evidence = append(evidence, Evidence{
			ID:         id,
			Source:     "correlation",
			Content:    fmt.Sprintf("%s supports=%d counters=%d topology_prior=%v", hypotheses[len(hypotheses)-1].Summary, len(ss.support), len(counterIDs), ss.neighborSpread),
			Score:      confidence,
			ObservedAt: end,
			Service:    ss.service,
			Kind:       "hypothesis",
			Stance:     "support",
			Claim:      strings.Join(claims, "+"),
		})
	}

	return Result{
		Hypotheses: hypotheses,
		Evidence:   evidence,
		Window:     req.Window,
		Service:    req.Service,
		DataGaps:   dataGaps,
	}, nil
}

type metricSignal struct {
	id        string
	service   string
	name      string
	value     float64
	timestamp time.Time
	score     float64
	stance    string
	claim     string
	reason    string
}

func classifyMetricSignals(metrics []types.MetricData, defaultService string) []metricSignal {
	series := make(map[string][]types.MetricData)
	for _, metric := range metrics {
		if math.IsNaN(metric.Value) || math.IsInf(metric.Value, 0) {
			continue
		}
		key := metricSeriesKey(metric, defaultService)
		series[key] = append(series[key], metric)
	}
	keys := make([]string, 0, len(series))
	for key := range series {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]metricSignal, 0)
	sequence := 0
	for _, key := range keys {
		points := series[key]
		sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
		if len(points) == 0 {
			continue
		}
		latest := points[len(points)-1]
		service := firstNonEmpty(latest.Labels["service"], defaultService, "unknown")
		if isAvailabilityMetric(latest.Name) {
			sequence++
			stance := "counter"
			score := 0.85
			reason := "availability healthy; counter-evidence"
			if latest.Value < 0.5 {
				stance = "support"
				score = 0.95
				reason = "availability below 0.5"
			}
			out = append(out, metricSignal{
				id:        fmt.Sprintf("corr-metric:%d:%s", sequence, latest.Timestamp.UTC().Format(time.RFC3339Nano)),
				service:   service,
				name:      latest.Name,
				value:     latest.Value,
				timestamp: latest.Timestamp,
				score:     score,
				stance:    stance,
				claim:     "availability",
				reason:    reason,
			})
			continue
		}

		values := make([]float64, 0, len(points))
		for _, point := range points {
			values = append(values, point.Value)
		}
		center := median(values)
		deviations := make([]float64, 0, len(values))
		for _, value := range values {
			deviations = append(deviations, math.Abs(value-center))
		}
		mad := median(deviations)
		scale := 1.4826 * mad
		if scale < 1e-9 {
			scale = math.Max(math.Abs(center)*0.10, 1e-6)
		}

		type candidate struct {
			point types.MetricData
			z     float64
		}
		candidates := make([]candidate, 0)
		for _, point := range points {
			z := math.Abs(point.Value-center) / scale
			explicit, _ := strconv.ParseBool(strings.TrimSpace(point.Labels["anomaly"]))
			if (len(points) >= 4 && z >= 3.5) || explicit {
				if explicit && z < 3.5 {
					z = 3.5
				}
				candidates = append(candidates, candidate{point: point, z: z})
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].z == candidates[j].z {
				return candidates[i].point.Timestamp.Before(candidates[j].point.Timestamp)
			}
			return candidates[i].z > candidates[j].z
		})
		if len(candidates) > 3 {
			candidates = candidates[:3]
		}
		for _, candidate := range candidates {
			sequence++
			point := candidate.point
			service = firstNonEmpty(point.Labels["service"], defaultService, "unknown")
			out = append(out, metricSignal{
				id:        fmt.Sprintf("corr-metric:%d:%s", sequence, point.Timestamp.UTC().Format(time.RFC3339Nano)),
				service:   service,
				name:      point.Name,
				value:     point.Value,
				timestamp: point.Timestamp,
				score:     clamp(0.55+0.05*candidate.z, 0.55, 0.98),
				stance:    "support",
				claim:     signalClaimFamily("metrics", point.Name),
				reason:    fmt.Sprintf("robust_z=%.2f median=%.4f mad=%.4f", candidate.z, center, mad),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].timestamp.Equal(out[j].timestamp) {
			return out[i].id < out[j].id
		}
		return out[i].timestamp.Before(out[j].timestamp)
	})
	return out
}

func metricSeriesKey(metric types.MetricData, defaultService string) string {
	labels := make([]string, 0, len(metric.Labels))
	for key, value := range metric.Labels {
		if key == "__name__" {
			continue
		}
		labels = append(labels, key+"="+value)
	}
	sort.Strings(labels)
	return metric.Name + "\x00" + firstNonEmpty(metric.Labels["service"], defaultService, "unknown") + "\x00" + strings.Join(labels, "\x00")
}

func isAvailabilityMetric(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "up" || strings.HasPrefix(name, "up{")
}

// signalClaimFamily gives counter-evidence a narrow semantic target. In
// particular, a healthy availability probe must not refute an unrelated
// latency, error-rate, or saturation hypothesis for the same service.
func signalClaimFamily(kind, text string) string {
	value := strings.ToLower(strings.TrimSpace(text))
	for _, token := range []string{"unavailable", "outage", "unreachable", "connection refused", "no healthy upstream", "healthcheck", "health check", "service down"} {
		if strings.Contains(value, token) {
			return "availability"
		}
	}
	for _, token := range []string{"latency", "slow", "timeout", "duration"} {
		if strings.Contains(value, token) {
			return "latency"
		}
	}
	for _, token := range []string{"saturation", "cpu", "memory", "queue", "throttle", "capacity"} {
		if strings.Contains(value, token) {
			return "saturation"
		}
	}
	for _, token := range []string{"error", "failed", "failure", "exception", "5xx"} {
		if strings.Contains(value, token) {
			return "errors"
		}
	}
	if kind == "metrics" {
		return "performance"
	}
	return "incident"
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	middle := len(copyValues) / 2
	if len(copyValues)%2 == 1 {
		return copyValues[middle]
	}
	return (copyValues[middle-1] + copyValues[middle]) / 2
}

func observationTime(value, start, end time.Time) time.Time {
	if value.IsZero() || value.After(end) {
		return end
	}
	if value.Before(start) {
		return start
	}
	return value
}

func alertReliability(severity string) float64 {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "page", "p0", "p1":
		return 0.95
	case "warning", "warn", "p2":
		return 0.85
	default:
		return 0.78
	}
}

func temporalOrder(first map[string]time.Time) (string, int64) {
	if len(first) == 0 {
		return "", 0
	}
	var earliest, latest time.Time
	lead := ""
	for kind, timestamp := range first {
		if timestamp.IsZero() {
			continue
		}
		if earliest.IsZero() || timestamp.Before(earliest) || (timestamp.Equal(earliest) && kind < lead) {
			earliest = timestamp
			lead = kind
		}
		if latest.IsZero() || timestamp.After(latest) {
			latest = timestamp
		}
	}
	if earliest.IsZero() || latest.IsZero() {
		return lead, 0
	}
	return lead, maxInt64(0, int64(latest.Sub(earliest).Seconds()))
}

func hypothesisConfidence(score float64, kinds []string, supportWeight float64, supportCount int, lagSeconds int64, window time.Duration, counterCount int, topologyPrior bool) float64 {
	diversity := math.Min(1, float64(len(kinds))/3)
	strength := 1 - math.Exp(-score/10)
	quality := 0.0
	if supportCount > 0 {
		quality = clamp(supportWeight/float64(supportCount), 0, 1)
	}
	temporalCoherence := 1.0
	if window > 0 && lagSeconds > 0 {
		temporalCoherence = math.Exp(-float64(lagSeconds) / math.Max(window.Seconds()/2, 1))
	}
	confidence := 0.45*diversity + 0.25*strength + 0.20*quality + 0.10*temporalCoherence
	confidence -= math.Min(0.45, float64(counterCount)*0.15)
	if topologyPrior {
		confidence -= 0.05
	}
	return clamp(confidence, 0, 1)
}

func clamp(value, minimum, maximum float64) float64 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func round3(value float64) float64 { return math.Round(value*1000) / 1000 }

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func normalizeRequest(req Request) Request {
	if req.Now.IsZero() {
		req.Now = time.Now().UTC()
	}
	if req.Window <= 0 {
		req.Window = 30 * time.Minute
	}
	if req.Bucket <= 0 {
		req.Bucket = 2 * time.Minute
	}
	if req.LogLimit <= 0 {
		req.LogLimit = 50
	}
	if req.MaxHypotheses <= 0 {
		req.MaxHypotheses = 5
	}
	if strings.TrimSpace(req.MetricQuery) == "" {
		req.MetricQuery = `up`
	}
	req.Service = strings.TrimSpace(req.Service)
	return req
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func appendUnique(dst []string, ids ...string) []string {
	seen := map[string]bool{}
	for _, id := range dst {
		seen[id] = true
	}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		dst = append(dst, id)
	}
	return dst
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, ok := range m {
		if ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func truncate(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "..."
}

// StaticNeighbors adapts a service->deps map for limited hop spread.
type StaticNeighbors map[string][]string

// Neighbors implements DependencyNeighbor.
func (s StaticNeighbors) Neighbors(service string) []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s[service]...)
}
