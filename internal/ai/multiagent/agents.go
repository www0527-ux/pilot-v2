package multiagent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"shanyangcode.com/pilot/internal/ai/multiagent/intent"
	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/internal/ai/retriever"
	"shanyangcode.com/pilot/internal/prompts"
	"shanyangcode.com/pilot/pkg/types"
)

// KnowledgeSource is the retrieval surface needed by the knowledge role.
type KnowledgeSource interface {
	SearchDetailed(ctx context.Context, query string) ([]retriever.RetrievedDocument, error)
}

// KnowledgeExecutor 将融合后的 RAG 结果转为有界、可溯源的知识证据。
type KnowledgeExecutor struct {
	retriever KnowledgeSource
}

func NewKnowledgeExecutor(knowledgeRetriever KnowledgeSource) *KnowledgeExecutor {
	return &KnowledgeExecutor{retriever: knowledgeRetriever}
}

func (e *KnowledgeExecutor) Execute(ctx context.Context, query string, _ Plan) (Finding, error) {
	if e == nil || dependencyMissing(e.retriever) {
		return Finding{}, fmt.Errorf("knowledge retriever is unavailable")
	}
	finishSearch := progress.StartTool(ctx, "search_knowledge", map[string]any{"query": progress.Clip(progress.Redact(query), 400)})
	results, err := e.retriever.SearchDetailed(ctx, query)
	if err != nil {
		finishSearch("failed", progress.Redact(err.Error()), nil)
	} else {
		finishSearch("completed", fmt.Sprintf("检索到 %d 条知识文档", len(results)), results)
	}
	if err != nil {
		return Finding{}, fmt.Errorf("retrieve knowledge: %w", err)
	}
	if len(results) == 0 {
		return Finding{Summary: "No matching knowledge document was found."}, nil
	}

	evidence := make([]EvidenceItem, 0, len(results))
	for _, result := range results {
		content := strings.TrimSpace(result.Document.Content)
		evidence = append(evidence, EvidenceItem{
			ID:      "knowledge:" + result.Document.ID,
			Source:  "knowledge",
			Content: truncate(result.Document.Title+": "+content, 900),
			Score:   floatPointer(result.Score),
			Metadata: map[string]any{
				"category":       result.Document.Category,
				"recall_methods": result.RecallMethods,
				"bm25_score":     result.BM25Score,
				"vector_score":   result.VectorScore,
				"fused_score":    result.FusedScore,
			},
		})
	}
	return Finding{
		Summary:  fmt.Sprintf("Retrieved %d knowledge document(s) through multi-route RAG.", len(results)),
		Evidence: evidence,
		Metadata: map[string]any{"retrieved_count": len(results)},
	}, nil
}

type deterministicEvidenceSource interface {
	GetAlerts(ctx context.Context, filters map[string]string) ([]types.AlertInfo, error)
	QueryLogs(ctx context.Context, service, level, keyword string, startTime, endTime time.Time, limit int) ([]types.LogEntry, error)
}

// EvidenceExecutor 收集目标服务的近期告警与错误日志。
type EvidenceExecutor struct {
	collector deterministicEvidenceSource
	now       func() time.Time
	window    time.Duration
	logLimit  int
}

func NewEvidenceExecutor(collector deterministicEvidenceSource) *EvidenceExecutor {
	return &EvidenceExecutor{
		collector: collector,
		now:       time.Now,
		window:    30 * time.Minute,
		logLimit:  10,
	}
}

// WithWindow overrides the lookback window for alerts/logs collection.
func (e *EvidenceExecutor) WithWindow(window time.Duration) *EvidenceExecutor {
	if e == nil {
		return nil
	}
	if window > 0 {
		e.window = window
	}
	return e
}

func (e *EvidenceExecutor) Execute(ctx context.Context, _ string, plan Plan) (Finding, error) {
	if e == nil || dependencyMissing(e.collector) {
		return Finding{}, fmt.Errorf("observability collector is unavailable")
	}
	now := e.now()
	window := e.window
	if window <= 0 {
		window = 30 * time.Minute
	}
	if requested := metadataInt(plan.Metadata, "duration_minutes"); requested > 0 {
		window = time.Duration(requested) * time.Minute
	}
	windowMultiplier := evidenceWindowMultiplier(plan)
	window *= time.Duration(windowMultiplier)
	logLimit := e.logLimit
	if logLimit <= 0 {
		logLimit = 10
	}
	logLimit *= windowMultiplier
	if logLimit > 100 {
		logLimit = 100
	}
	var (
		alerts    []types.AlertInfo
		logs      []types.LogEntry
		alertsErr error
		logsErr   error
		group     sync.WaitGroup
	)
	group.Add(2)
	go func() {
		defer group.Done()
		finish := progress.StartTool(ctx, "query_alerts", map[string]any{"service": plan.Service, "status": "firing"})
		alerts, alertsErr = e.collector.GetAlerts(ctx, map[string]string{"service": plan.Service, "status": "firing"})
		if alertsErr != nil {
			finish("failed", progress.Redact(alertsErr.Error()), nil)
		} else {
			finish("completed", fmt.Sprintf("查询到 %d 条告警", len(alerts)), alerts)
		}
	}()
	go func() {
		defer group.Done()
		finish := progress.StartTool(ctx, "query_logs", map[string]any{"service": plan.Service, "level": "error", "duration_minutes": int(window / time.Minute)})
		logs, logsErr = e.collector.QueryLogs(ctx, plan.Service, "error", "", now.Add(-window), now, logLimit)
		if logsErr != nil {
			finish("failed", progress.Redact(logsErr.Error()), nil)
		} else {
			finish("completed", fmt.Sprintf("查询到 %d 条错误日志", len(logs)), logs)
		}
	}()
	group.Wait()
	if alertsErr != nil && logsErr != nil {
		return Finding{}, fmt.Errorf("alerts: %v; error logs: %v", alertsErr, logsErr)
	}

	evidence := make([]EvidenceItem, 0, len(alerts)+len(logs))
	for index, alert := range alerts {
		evidence = append(evidence, EvidenceItem{
			ID:      fmt.Sprintf("alert:%d:%s", index, alert.Name),
			Source:  "alerts",
			Content: fmt.Sprintf("%s [%s/%s]: %s", alert.Name, alert.Severity, alert.Status, alert.Description),
		})
	}
	for index, log := range logs {
		evidence = append(evidence, EvidenceItem{
			ID:       fmt.Sprintf("log:%d:%s", index, log.Timestamp.UTC().Format(time.RFC3339Nano)),
			Source:   "logs",
			Content:  truncate(fmt.Sprintf("%s %s: %s", log.Timestamp.Format(time.RFC3339), log.Level, log.Message), 900),
			Metadata: map[string]any{"trace_id": log.TraceID, "service": log.Service},
		})
	}

	summary := fmt.Sprintf("Collected %d firing alert(s) and %d recent error log(s) for %s.", len(alerts), len(logs), serviceLabel(plan.Service))
	limitationsHint := alertsErr != nil || logsErr != nil
	if limitationsHint {
		summary += " One data source was unavailable."
	}
	meta := map[string]any{
		"mode":              EvidenceModeDeterministic,
		"alert_count":       len(alerts),
		"error_log_count":   len(logs),
		"alerts_available":  alertsErr == nil,
		"logs_available":    logsErr == nil,
		"window_minutes":    int(window / time.Minute),
		"window_multiplier": windowMultiplier,
	}
	if alertsErr != nil {
		meta["alerts_error"] = truncate(alertsErr.Error(), 200)
		meta["partial_failure"] = true
	}
	if logsErr != nil {
		meta["logs_error"] = truncate(logsErr.Error(), 200)
		meta["partial_failure"] = true
	}
	return Finding{
		Summary:  summary,
		Evidence: evidence,
		Metadata: meta,
	}, nil
}

func evidenceWindowMultiplier(plan Plan) int {
	multiplier := metadataInt(plan.Metadata, "evidence_window_multiplier")
	if multiplier < 1 {
		return 1
	}
	if multiplier > 8 {
		return 8
	}
	return multiplier
}

// ModelSynthesizer 仅基于合并后的角色结论与限制项生成答案。
type ModelSynthesizer struct {
	chain   compose.Runnable[[]*schema.Message, *schema.Message]
	prompts *prompts.Catalog
}

// NewModelSynthesizer 基于注入的模型创建综合器。
func NewModelSynthesizer(ctx context.Context, chatModel model.BaseChatModel) (*ModelSynthesizer, error) {
	if dependencyMissing(chatModel) {
		return nil, fmt.Errorf("synthesis model is required")
	}

	chain := compose.NewChain[[]*schema.Message, *schema.Message]()
	chain.AppendChatModel(chatModel, compose.WithNodeName("MultiAgentSynthesisModel"))
	runnable, err := chain.Compile(ctx, compose.WithGraphName("MultiAgentSynthesis"))
	if err != nil {
		return nil, fmt.Errorf("compile synthesis chain: %w", err)
	}
	catalog, err := prompts.Default()
	if err != nil {
		return nil, fmt.Errorf("load synthesis prompts: %w", err)
	}
	return &ModelSynthesizer{chain: runnable, prompts: catalog}, nil
}

func (s *ModelSynthesizer) Synthesize(ctx context.Context, query string, plan Plan, findings []Finding, limitations []Limitation) (string, error) {
	if s == nil || s.chain == nil {
		return "", fmt.Errorf("synthesis model is unavailable")
	}
	systemPrompt, err := s.prompts.Render("multi_agent_synthesis_system", nil)
	if err != nil {
		return "", err
	}
	userPrompt, err := renderSynthesisPrompt(s.prompts, query, plan, findings, limitations)
	if err != nil {
		return "", err
	}
	messages := []*schema.Message{{Role: schema.System, Content: systemPrompt}, {Role: schema.User, Content: userPrompt}}
	if progress.Streaming(ctx) {
		progress.StartAnswer(ctx)
		stream, err := s.chain.Stream(ctx, messages)
		if err != nil {
			return "", fmt.Errorf("stream synthesis: %w", err)
		}
		return progress.ReadAnswer(ctx, stream)
	}
	result, err := s.chain.Invoke(ctx, messages, compose.WithCallbacks(progress.CallbackHandler()))
	if err != nil {
		return "", fmt.Errorf("generate synthesis: %w", err)
	}
	answer := strings.TrimSpace(result.Content)
	if answer == "" {
		return "", fmt.Errorf("synthesis model returned an empty answer")
	}
	return answer, nil
}

// OrchestratorDeps keeps production assembly explicit while allowing every role
// dependency to be replaced in tests.
type OrchestratorDeps struct {
	Collector           EvidenceSource
	KnowledgeRetriever  KnowledgeSource
	SynthesisModel      model.ToolCallingChatModel
	EvidenceModel       model.ToolCallingChatModel // optional; react evidence falls back to SynthesisModel
	ServiceName         string
	EvidenceMode        string
	ReactMaxSteps       int
	PlannerMode         string
	IntentClassifier    intent.Classifier
	IntentMinConfidence float64
	IntentTimeout       time.Duration
	EvidenceWindow      time.Duration
	BranchTimeout       time.Duration
	MaxReplanRounds     int
	SessionSummary      string
	CorrelationEnabled  bool
	CorrelationExecutor Executor // optional; when CorrelationEnabled and non-nil, merged into evidence
	Runtime             string   // legacy | adk
	Verification        VerificationPolicy
}

// NewDefaultOrchestrator 接入生产数据源，同时保持依赖可在测试中替换。
func NewDefaultOrchestrator(ctx context.Context, deps OrchestratorDeps) (*Orchestrator, error) {
	if dependencyMissing(deps.Collector) {
		return nil, fmt.Errorf("evidence source is required")
	}
	if dependencyMissing(deps.KnowledgeRetriever) {
		return nil, fmt.Errorf("knowledge retriever is required")
	}
	if dependencyMissing(deps.SynthesisModel) {
		return nil, fmt.Errorf("synthesis model is required")
	}

	synthesizer, err := NewModelSynthesizer(ctx, deps.SynthesisModel)
	if err != nil {
		return nil, err
	}

	var evidence Executor
	reactModel := deps.EvidenceModel
	if dependencyMissing(reactModel) {
		reactModel = deps.SynthesisModel
	}
	switch mode := strings.ToLower(strings.TrimSpace(deps.EvidenceMode)); mode {
	case "", EvidenceModeDeterministic:
		evidence = NewEvidenceExecutor(deps.Collector).WithWindow(deps.EvidenceWindow)
	case EvidenceModeReact:
		reactEvidence, err := NewReactEvidenceExecutor(ctx, deps.Collector, reactModel, deps.ReactMaxSteps)
		if err != nil {
			return nil, err
		}
		evidence = newFallbackEvidenceExecutor(reactEvidence, NewEvidenceExecutor(deps.Collector).WithWindow(deps.EvidenceWindow))
	default:
		return nil, fmt.Errorf("unsupported evidence mode %q", mode)
	}
	if deps.CorrelationEnabled && !dependencyMissing(deps.CorrelationExecutor) {
		evidence = NewMergingEvidenceExecutor(evidence, deps.CorrelationExecutor)
	}

	planner, err := buildPlanner(deps)
	if err != nil {
		return nil, err
	}

	orch, err := NewOrchestrator(
		planner,
		map[Role]Executor{
			RoleEvidence:  evidence,
			RoleKnowledge: NewKnowledgeExecutor(deps.KnowledgeRetriever),
		},
		synthesizer,
	)
	if err != nil {
		return nil, err
	}
	orch.branchTimeout = deps.BranchTimeout
	orch.maxReplanRounds = deps.MaxReplanRounds
	if orch.maxReplanRounds < 0 {
		orch.maxReplanRounds = 0
	}
	if orch.maxReplanRounds > 1 {
		orch.maxReplanRounds = 1
	}
	orch.sessionSummary = strings.TrimSpace(deps.SessionSummary)
	orch.verification = deps.Verification.normalized()
	return orch, nil
}

func buildPlanner(deps OrchestratorDeps) (Planner, error) {
	mode := strings.ToLower(strings.TrimSpace(deps.PlannerMode))
	if mode == "" {
		mode = PlannerModeRule
	}
	rules := NewRulePlanner(deps.ServiceName)
	switch mode {
	case PlannerModeRule:
		return NewContextPlanner(rules), nil
	case PlannerModeHybrid:
		if dependencyMissing(deps.IntentClassifier) {
			return nil, fmt.Errorf("hybrid planner requires an intent classifier")
		}
		return NewHybridPlanner(HybridPlannerConfig{
			DefaultService: deps.ServiceName,
			Classifier:     deps.IntentClassifier,
			MinConfidence:  deps.IntentMinConfidence,
			Timeout:        deps.IntentTimeout,
		})
	default:
		return nil, fmt.Errorf("unsupported planner mode %q", mode)
	}
}

func dependencyMissing(dependency any) bool {
	if dependency == nil {
		return true
	}

	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func synthesisPrompt(query string, plan Plan, findings []Finding, limitations []Limitation) string {
	catalog, err := prompts.Default()
	if err != nil {
		return "[prompt catalog unavailable: " + err.Error() + "]"
	}
	rendered, err := renderSynthesisPrompt(catalog, query, plan, findings, limitations)
	if err != nil {
		return "[prompt render failed: " + err.Error() + "]"
	}
	return rendered
}

func renderSynthesisPrompt(catalog *prompts.Catalog, query string, plan Plan, findings []Finding, limitations []Limitation) (string, error) {
	var assessment strings.Builder
	var findingText strings.Builder
	var limitationText strings.Builder
	claimableIDs := claimableEvidenceIDs(findings)
	if confidence, ok := metadataFloat(plan.Metadata, "evidence_confidence"); ok {
		assessment.WriteString("Evidence sufficiency score (not root-cause probability): ")
		assessment.WriteString(fmt.Sprintf("%.3f", confidence))
		assessment.WriteString("\nEvidence confidence band: ")
		assessment.WriteString(metadataString(plan.Metadata, "evidence_confidence_band"))
		assessment.WriteString("\nEvidence stop reason: ")
		assessment.WriteString(metadataString(plan.Metadata, "evidence_stop_reason"))
		assessment.WriteString("\nEvidence needs more acquisition: ")
		assessment.WriteString(fmt.Sprint(metadataBool(plan.Metadata, "evidence_needs_more")))
	}
	for _, finding := range findings {
		if !findingHasClaimableEvidence(finding, claimableIDs) {
			continue
		}
		findingText.WriteString("- ")
		findingText.WriteString(string(finding.Role))
		findingText.WriteString(" [UNTRUSTED SUMMARY]: ")
		findingText.WriteString(finding.Summary)
		findingText.WriteString("\n")
		for _, evidence := range finding.Evidence {
			if !claimableIDs[strings.TrimSpace(evidence.ID)] {
				continue
			}
			findingText.WriteString("  - [")
			findingText.WriteString(evidence.ID)
			findingText.WriteString("] ")
			if strings.EqualFold(metadataString(evidence.Metadata, "stance"), "counter") {
				findingText.WriteString("[COUNTER-EVIDENCE] ")
			}
			findingText.WriteString("[UNTRUSTED] ")
			findingText.WriteString(truncate(evidence.Content, 500))
			findingText.WriteString("\n")
		}
	}
	if len(limitations) > 0 {
		for _, limitation := range limitations {
			limitationText.WriteString("- ")
			limitationText.WriteString(limitation.Message)
			limitationText.WriteString("\n")
		}
	}
	if findingText.Len() == 0 {
		findingText.WriteString("(none)")
	}
	if limitationText.Len() == 0 {
		limitationText.WriteString("(none)")
	}
	return catalog.Render("multi_agent_synthesis_user", map[string]any{
		"query": query, "intent": plan.Intent, "service": serviceLabel(plan.Service),
		"assessment": assessment.String(), "findings": findingText.String(), "limitations": limitationText.String(),
	})
}

func truncate(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "..."
}

func floatPointer(value float64) *float64 {
	return &value
}

func serviceLabel(service string) string {
	if strings.TrimSpace(service) == "" {
		return "the configured service"
	}
	return service
}
