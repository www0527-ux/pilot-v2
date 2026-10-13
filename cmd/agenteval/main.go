package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"shanyangcode.com/pilot/internal/ai/multiagent"
	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/pkg/envloader"
)

type evalCase struct {
	ID                  string   `json:"id"`
	Query               string   `json:"query"`
	ExpectIntent        string   `json:"expect_intent"`
	ExpectEvidence      bool     `json:"expect_evidence"`
	ExpectCorr          bool     `json:"expect_corr"`
	MaxLatencyMS        int64    `json:"max_latency_ms"`
	RequireCitation     bool     `json:"require_citation"`
	RequireVerification bool     `json:"require_verification"`
	MinConfidence       float64  `json:"min_confidence"`
	MaxContradictions   *int     `json:"max_contradictions"`
	Tags                []string `json:"tags"`
	ExpectService       string   `json:"expect_service"`
	ExpectRuntime       string   `json:"expect_runtime"`
	ExpectFallback      *bool    `json:"expect_fallback"`
}

type caseResult struct {
	ID                 string  `json:"id"`
	Pass               bool    `json:"pass"`
	Intent             string  `json:"intent"`
	HasEvidence        bool    `json:"has_evidence"`
	HasCorr            bool    `json:"has_corr"`
	Cited              bool    `json:"cited"`
	CitationPassed     bool    `json:"citation_passed"`
	Verified           bool    `json:"verified"`
	Confidence         float64 `json:"confidence"`
	ConfidenceBand     string  `json:"confidence_band,omitempty"`
	Decision           string  `json:"decision,omitempty"`
	StopReason         string  `json:"stop_reason,omitempty"`
	Contradictions     int     `json:"contradictions"`
	EvidenceGraphNodes int     `json:"evidence_graph_nodes"`
	LatencyMS          int64   `json:"latency_ms"`
	Limitations        int     `json:"limitations"`
	Detail             string  `json:"detail,omitempty"`
	Runtime            string  `json:"runtime,omitempty"`
	Service            string  `json:"service,omitempty"`
	Fallback           bool    `json:"fallback"`
}

type reportSummary struct {
	Total               int     `json:"total"`
	IntentAccuracy      float64 `json:"intent_accuracy"`
	EvidenceMissRate    float64 `json:"evidence_miss_rate"`
	CorrelationAccuracy float64 `json:"correlation_accuracy"`
	CitationPassRate    float64 `json:"citation_pass_rate"`
	FallbackRate        float64 `json:"fallback_rate"`
	P95LatencyMS        int64   `json:"p95_latency_ms"`
	RuntimeParityKeys   int     `json:"runtime_parity_keys"`
}

type report struct {
	SchemaVersion string        `json:"schema_version"`
	CheckedAt     string        `json:"checked_at"`
	BaseURL       string        `json:"base_url,omitempty"`
	Mode          string        `json:"mode"`
	Passed        int           `json:"passed"`
	Failed        int           `json:"failed"`
	Results       []caseResult  `json:"results"`
	Summary       reportSummary `json:"summary"`
}

func main() {
	casesPath := flag.String("cases", "testdata/eval/canaries.json", "eval cases JSON")
	outDir := flag.String("out", "", "report output directory (default artifacts/eval/<mode>)")
	mode := flag.String("mode", "offline", "offline (rule planner only) or live (HTTP multi-agent)")
	baseURL := flag.String("base-url", "", "live mode base URL")
	flag.Parse()

	_ = envloader.Load()
	raw, err := os.ReadFile(*casesPath)
	if err != nil {
		fail(err)
	}
	var cases []evalCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		fail(err)
	}

	rep := report{
		SchemaVersion: "pilot.agent_eval.v2",
		CheckedAt:     time.Now().UTC().Format(time.RFC3339),
		Mode:          *mode,
		BaseURL:       *baseURL,
		Results:       make([]caseResult, 0, len(cases)),
	}

	switch strings.ToLower(*mode) {
	case "offline":
		runOffline(&rep, filterCases(cases, "offline"))
	case "live":
		url := strings.TrimSpace(*baseURL)
		if url == "" {
			cfg, err := config.Load(config.ResolvePath(""))
			if err != nil {
				fail(err)
			}
			url = cfg.Server.BaseURL
		}
		rep.BaseURL = url
		runLive(context.Background(), &rep, filterCases(cases, "live"), url)
	default:
		fail(fmt.Errorf("unsupported mode %q", *mode))
	}

	resolvedOut := strings.TrimSpace(*outDir)
	if resolvedOut == "" {
		resolvedOut = filepath.Join("artifacts", "eval", strings.ToLower(*mode))
	}
	rep.Summary = summarizeReport(rep.Results)
	if err := os.MkdirAll(resolvedOut, 0o755); err != nil {
		fail(err)
	}
	jsonPath := filepath.Join(resolvedOut, "report.json")
	mdPath := filepath.Join(resolvedOut, "report.md")
	if err := writeJSON(jsonPath, rep); err != nil {
		fail(err)
	}
	if err := writeMarkdown(mdPath, rep); err != nil {
		fail(err)
	}
	fmt.Printf("agenteval: passed=%d failed=%d out=%s\n", rep.Passed, rep.Failed, resolvedOut)
	if rep.Failed > 0 {
		os.Exit(1)
	}
}

func filterCases(cases []evalCase, tag string) []evalCase {
	out := make([]evalCase, 0, len(cases))
	for _, c := range cases {
		if len(c.Tags) == 0 {
			out = append(out, c)
			continue
		}
		for _, t := range c.Tags {
			if strings.EqualFold(t, tag) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func runOffline(rep *report, cases []evalCase) {
	planner := multiagent.NewRulePlanner("pilot-api")
	for _, c := range cases {
		started := time.Now()
		plan := planner.Plan(c.Query)
		latency := time.Since(started).Milliseconds()
		hasEvidence := false
		for _, role := range plan.SelectedRoles {
			if role == multiagent.RoleEvidence {
				hasEvidence = true
				break
			}
		}
		cr := caseResult{
			ID:          c.ID,
			Intent:      plan.Intent,
			HasEvidence: hasEvidence,
			LatencyMS:   latency,
			Runtime:     "offline-rule",
			Service:     plan.Service,
		}
		var problems []string
		if c.ExpectIntent != "" && plan.Intent != c.ExpectIntent {
			problems = append(problems, fmt.Sprintf("intent want %s got %s", c.ExpectIntent, plan.Intent))
		}
		if c.ExpectEvidence && !hasEvidence {
			problems = append(problems, "expected evidence role")
		}
		if c.ExpectService != "" && plan.Service != c.ExpectService {
			problems = append(problems, fmt.Sprintf("service want %s got %s", c.ExpectService, plan.Service))
		}
		if c.MaxLatencyMS > 0 && latency > c.MaxLatencyMS {
			problems = append(problems, fmt.Sprintf("latency %dms > %dms", latency, c.MaxLatencyMS))
		}
		cr.Pass = len(problems) == 0
		cr.Detail = strings.Join(problems, "; ")
		if cr.Pass {
			rep.Passed++
		} else {
			rep.Failed++
		}
		rep.Results = append(rep.Results, cr)
	}
}

func runLive(ctx context.Context, rep *report, cases []evalCase, baseURL string) {
	client := &httpClient{baseURL: strings.TrimRight(baseURL, "/")}
	for _, c := range cases {
		started := time.Now()
		result, err := client.multiAgent(ctx, c.Query)
		latency := time.Since(started).Milliseconds()
		cr := caseResult{ID: c.ID, LatencyMS: latency}
		if err != nil {
			cr.Pass = false
			cr.Detail = err.Error()
			rep.Failed++
			rep.Results = append(rep.Results, cr)
			continue
		}
		cr.Intent = result.Plan.Intent
		cr.Service = result.Plan.Service
		cr.Limitations = len(result.Limitations)
		if result.Metadata != nil {
			if rt, ok := result.Metadata["runtime"].(string); ok {
				cr.Runtime = rt
			}
		}
		if result.Verification != nil {
			cr.Verified = true
			cr.Confidence = result.Verification.Assessment.Score
			cr.ConfidenceBand = result.Verification.Assessment.Band
			cr.Decision = result.Verification.Decision
			cr.StopReason = result.Verification.Assessment.StopReason
			cr.Contradictions = result.Verification.Assessment.ContradictionCount
			cr.CitationPassed = result.Verification.Citations.Passed
			cr.Cited = result.Verification.Citations.CitationCount > 0
		}
		if result.EvidenceGraph != nil {
			cr.EvidenceGraphNodes = len(result.EvidenceGraph.Nodes)
		}
		for _, finding := range result.Findings {
			if finding.Role == multiagent.RoleEvidence && len(finding.Evidence) > 0 {
				cr.HasEvidence = true
			}
			if finding.Metadata != nil {
				mode, _ := finding.Metadata["mode"].(string)
				if strings.Contains(mode, "fallback") {
					cr.Fallback = true
				}
			}
			for _, item := range finding.Evidence {
				if strings.HasPrefix(item.ID, "corr-") || item.Source == "correlation" {
					cr.HasCorr = true
				}
				if strings.Contains(result.Answer, item.ID) {
					cr.Cited = true
				}
			}
		}
		var problems []string
		if c.ExpectIntent != "" && cr.Intent != c.ExpectIntent {
			problems = append(problems, fmt.Sprintf("intent want %s got %s", c.ExpectIntent, cr.Intent))
		}
		if c.ExpectEvidence && !cr.HasEvidence {
			problems = append(problems, "expected evidence")
		}
		if c.ExpectService != "" && cr.Service != c.ExpectService {
			problems = append(problems, fmt.Sprintf("service want %s got %s", c.ExpectService, cr.Service))
		}
		if c.ExpectRuntime != "" && cr.Runtime != c.ExpectRuntime {
			problems = append(problems, fmt.Sprintf("runtime want %s got %s", c.ExpectRuntime, cr.Runtime))
		}
		if c.ExpectFallback != nil && cr.Fallback != *c.ExpectFallback {
			problems = append(problems, fmt.Sprintf("fallback want %v got %v", *c.ExpectFallback, cr.Fallback))
		}
		if c.ExpectCorr && !cr.HasCorr {
			problems = append(problems, "expected corr-* evidence")
		}
		if c.RequireCitation && cr.HasEvidence && (!cr.Cited || (cr.Verified && !cr.CitationPassed)) && !answerLooksFallback(result.Answer) {
			problems = append(problems, "expected citation")
		}
		if c.RequireVerification && !cr.Verified {
			problems = append(problems, "expected evidence verification report")
		}
		if c.MinConfidence > 0 && cr.Confidence < c.MinConfidence {
			problems = append(problems, fmt.Sprintf("confidence %.3f < %.3f", cr.Confidence, c.MinConfidence))
		}
		if c.MaxContradictions != nil && cr.Contradictions > *c.MaxContradictions {
			problems = append(problems, fmt.Sprintf("contradictions %d > %d", cr.Contradictions, *c.MaxContradictions))
		}
		if c.MaxLatencyMS > 0 && latency > c.MaxLatencyMS {
			problems = append(problems, fmt.Sprintf("latency %dms > %dms", latency, c.MaxLatencyMS))
		}
		cr.Pass = len(problems) == 0
		cr.Detail = strings.Join(problems, "; ")
		if cr.Pass {
			rep.Passed++
		} else {
			rep.Failed++
		}
		rep.Results = append(rep.Results, cr)
	}
}

func answerLooksFallback(answer string) bool {
	return strings.Contains(answer, "Evidence summary") || strings.Contains(answer, "Current conclusion") || strings.Contains(answer, "deterministic")
}

func writeJSON(path string, rep report) error {
	raw, _ := json.MarshalIndent(rep, "", "  ")
	return os.WriteFile(path, raw, 0o644)
}

func writeMarkdown(path string, rep report) error {
	var b strings.Builder
	b.WriteString("# Agent Eval Report\n\n")
	b.WriteString(fmt.Sprintf("- checked_at: %s\n- mode: %s\n- passed: %d\n- failed: %d\n\n", rep.CheckedAt, rep.Mode, rep.Passed, rep.Failed))
	b.WriteString("| ID | Pass | Intent | Evidence | Corr | Confidence | Decision | Contradictions | Latency |\n|---|---|---|---|---|---:|---|---:|---:|\n")
	for _, r := range rep.Results {
		b.WriteString(fmt.Sprintf("| %s | %v | %s | %v | %v | %.3f | %s | %d | %d |\n", r.ID, r.Pass, r.Intent, r.HasEvidence, r.HasCorr, r.Confidence, r.Decision, r.Contradictions, r.LatencyMS))
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func summarizeReport(results []caseResult) reportSummary {
	summary := reportSummary{Total: len(results)}
	if len(results) == 0 {
		return summary
	}
	intentCompared, intentPassed := 0, 0
	evidenceExpected, evidenceMissed := 0, 0
	corrCompared, corrPassed := 0, 0
	citationCompared, citationPassed := 0, 0
	fallbacks := 0
	latencies := make([]int64, 0, len(results))
	parity := map[string]map[string]struct{}{}
	for _, result := range results {
		if result.Intent != "" {
			intentCompared++
			if result.Pass {
				intentPassed++
			}
		}
		if !result.HasEvidence && strings.Contains(result.Detail, "expected evidence") {
			evidenceMissed++
		}
		if result.HasEvidence || strings.Contains(result.Detail, "expected evidence") {
			evidenceExpected++
		}
		if result.HasCorr || strings.Contains(result.Detail, "expected corr") {
			corrCompared++
			if result.HasCorr {
				corrPassed++
			}
		}
		if result.Cited || result.Verified {
			citationCompared++
			if result.CitationPassed || result.Cited {
				citationPassed++
			}
		}
		if result.Fallback {
			fallbacks++
		}
		latencies = append(latencies, result.LatencyMS)
		if result.Runtime != "" {
			if parity[result.ID] == nil {
				parity[result.ID] = map[string]struct{}{}
			}
			parity[result.ID][result.Runtime] = struct{}{}
		}
	}
	if intentCompared > 0 {
		summary.IntentAccuracy = float64(intentPassed) / float64(intentCompared)
	}
	if evidenceExpected > 0 {
		summary.EvidenceMissRate = float64(evidenceMissed) / float64(evidenceExpected)
	}
	if corrCompared > 0 {
		summary.CorrelationAccuracy = float64(corrPassed) / float64(corrCompared)
	}
	if citationCompared > 0 {
		summary.CitationPassRate = float64(citationPassed) / float64(citationCompared)
	}
	summary.FallbackRate = float64(fallbacks) / float64(len(results))
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	index := int(float64(len(latencies)-1)*.95 + .5)
	if index >= len(latencies) {
		index = len(latencies) - 1
	}
	summary.P95LatencyMS = latencies[index]
	for _, runtimes := range parity {
		if len(runtimes) > 1 {
			summary.RuntimeParityKeys++
		}
	}
	return summary
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "agenteval failed: %v\n", err)
	os.Exit(1)
}
