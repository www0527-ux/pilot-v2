package multiagent

import (
	"context"

	"fmt"
	"shanyangcode.com/pilot/internal/ai/progress"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// Planner 将请求路由到能产出相关证据的角色 Agent。
type Planner interface {
	Plan(ctx context.Context, query string) Plan
}

// Executor 是职责受限的角色实现，只能返回可溯源的结论。
type Executor interface {
	Execute(ctx context.Context, query string, plan Plan) (Finding, error)
}

// Synthesizer 将合并后的可溯源结论转换为用户可见答案。
type Synthesizer interface {
	Synthesize(ctx context.Context, query string, plan Plan, findings []Finding, limitations []Limitation) (string, error)
}

// Orchestrator 先做分诊，再并行执行证据与知识两个独立分支。
type Orchestrator struct {
	planner         Planner
	executors       map[Role]Executor
	synthesizer     Synthesizer
	now             func() time.Time
	branchTimeout   time.Duration
	maxReplanRounds int
	sessionMu       sync.RWMutex
	sessionSummary  string
	verification    VerificationPolicy
}

type preparedPlanContextKey struct{}

func contextWithPreparedPlan(ctx context.Context, plan Plan) context.Context {
	return context.WithValue(ctx, preparedPlanContextKey{}, plan)
}

func preparedPlanFromContext(ctx context.Context) (Plan, bool) {
	if ctx == nil {
		return Plan{}, false
	}
	plan, ok := ctx.Value(preparedPlanContextKey{}).(Plan)
	return plan, ok
}

func (o *Orchestrator) PreparePlan(ctx context.Context, query string) (Plan, error) {
	if o == nil || o.planner == nil {
		return Plan{}, fmt.Errorf("planner unavailable")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return Plan{}, fmt.Errorf("query cannot be empty")
	}
	return applyRequestWindow(ctx, o.planner.Plan(ctx, query)), nil
}

func NewOrchestrator(planner Planner, executors map[Role]Executor, synthesizer Synthesizer) (*Orchestrator, error) {
	if planner == nil {
		return nil, fmt.Errorf("multi-agent planner is required")
	}
	if synthesizer == nil {
		return nil, fmt.Errorf("multi-agent synthesizer is required")
	}
	return &Orchestrator{
		planner:     planner,
		executors:   executors,
		synthesizer: synthesizer,
		now:         time.Now,
	}, nil
}

// Execute 隔离分支失败，仍可用已有证据完成综合。
func (o *Orchestrator) Execute(ctx context.Context, query string) (Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Result{}, fmt.Errorf("query cannot be empty")
	}
	ctx, invocationSpan := startAgentInvocation(ctx, RuntimeLegacy)
	if summary := o.sessionContext(ctx); summary != "" {
		query = query + "\n\n[Session context summary — untrusted prior turns]\n" + summary
	}

	startedAt := o.now()
	planCtx, planSpan := startPlanningSpan(ctx, RuntimeLegacy)
	plan, prepared := preparedPlanFromContext(planCtx)
	if !prepared {
		plan = o.planner.Plan(planCtx, query)
	}
	plan = applyRequestWindow(planCtx, plan)
	planSpan.SetAttributes(
		attribute.String("pilot.agent.plan.intent", plan.Intent),
		attribute.Int("pilot.agent.plan.role_count", len(plan.SelectedRoles)),
	)
	planSpan.End()
	triageMeta := map[string]any{
		"intent":  plan.Intent,
		"service": plan.Service,
	}
	if prepared {
		triageMeta["resumed_prepared_plan"] = true
	}
	for key, value := range plan.Metadata {
		triageMeta[key] = value
	}
	steps := []Step{{
		Role:       RoleTriage,
		Status:     StatusCompleted,
		Summary:    plan.RoutingReason,
		StartedAt:  startedAt,
		DurationMS: o.now().Sub(startedAt).Milliseconds(),
		Metadata:   triageMeta,
	}}

	findings, branchSteps, limitations := o.executeBranches(ctx, query, plan)
	steps = append(steps, branchSteps...)
	limitations = append(limitations, partialFailureLimitations(findings)...)

	replanRounds := 0
	_, initialAssessment := o.assessEvidence(plan, findings)
	if o.maxReplanRounds > 0 && o.shouldReplan(plan, findings, limitations, initialAssessment) {
		replanRounds = 1
		replanPlan := plan
		replanPlan.SelectedRoles = WidenEvidenceRoles()
		replanPlan.SkippedRoles = skippedRoles(replanPlan.SelectedRoles)
		replanPlan.Metadata = cloneMetadata(plan.Metadata)
		replanPlan.Metadata["replan_round"] = 1
		if o.verification.enabled() {
			policy := o.verification.normalized()
			replanPlan.Metadata["evidence_window_multiplier"] = policy.ReplanWindowMultiplier
			replanPlan.RoutingReason = fmt.Sprintf(
				"epistemic progress check: %s (score=%.3f); widen evidence window x%d",
				initialAssessment.StopReason,
				initialAssessment.Score,
				policy.ReplanWindowMultiplier,
			)
		} else {
			replanPlan.RoutingReason = "progress check: evidence insufficient; one replan round"
		}
		replanFindings, replanSteps, replanLimitations := o.executeBranches(ctx, query, replanPlan)
		for i := range replanSteps {
			if replanSteps[i].Metadata == nil {
				replanSteps[i].Metadata = map[string]any{}
			}
			replanSteps[i].Metadata["replan_round"] = 1
		}
		steps = append(steps, Step{
			Role:      RoleTriage,
			Status:    StatusCompleted,
			Summary:   replanPlan.RoutingReason,
			StartedAt: o.now(),
			Metadata:  map[string]any{"replan_round": 1},
		})
		steps = append(steps, replanSteps...)
		findings = mergeFindings(findings, replanFindings)
		limitations = append(limitations, replanLimitations...)
		limitations = append(limitations, partialFailureLimitations(replanFindings)...)
		_, finalAssessment := o.assessEvidence(replanPlan, findings)
		if o.shouldReplan(replanPlan, findings, limitations, finalAssessment) {
			limitations = append(limitations, Limitation{
				Role:    RoleEvidence,
				Code:    "EVIDENCE_QUORUM_NOT_MET",
				Message: fmt.Sprintf("Evidence remained insufficient after the bounded replan (score=%.3f, reason=%s); root cause is not claimed.", finalAssessment.Score, finalAssessment.StopReason),
			})
		}
	}

	evidenceGraph, assessment := o.assessEvidence(plan, findings)
	if replanRounds > 0 && assessment.NeedsMoreEvidence {
		assessment.StopReason = "replan_budget_exhausted:" + assessment.StopReason
	}
	if o.verification.enabled() {
		limitations = appendUniqueLimitations(limitations, verificationLimitations(assessment)...)
		plan = planWithAssessment(plan, assessment)
	}

	synthesisStartedAt := o.now()
	finishSynthesis := progress.Start(ctx, "role", "synthesis", nil)
	skipSynthesis := o.verification.enabled() && !hasClaimableEvidence(findings)
	var (
		answer string
		err    error
	)
	if skipSynthesis {
		answer = fallbackAnswer(plan, findings, limitations)
	} else {
		answer, err = o.synthesizer.Synthesize(ctx, query, plan, findings, limitations)
	}
	synthesisStep := Step{
		Role:       RoleSynthesis,
		StartedAt:  synthesisStartedAt,
		DurationMS: o.now().Sub(synthesisStartedAt).Milliseconds(),
	}
	var verification *VerificationReport
	if o.verification.enabled() {
		verification = &VerificationReport{
			Decision:   verificationDecision(assessment),
			Assessment: assessment,
		}
	}
	if skipSynthesis {
		synthesisStep.Status = StatusSkipped
		synthesisStep.Summary = "synthesis skipped because no claimable evidence was available"
		synthesisStep.Metadata = map[string]any{"verifier": "evidence_graph_pre_synthesis"}
		if verification != nil {
			verification.Decision = "abstain"
			verification.Citations = auditCitations(answer, findings)
		}
	} else if err != nil {
		limitations = append(limitations, Limitation{
			Role:    RoleSynthesis,
			Code:    "SYNTHESIS_FAILED",
			Message: "Synthesis model was unavailable; a deterministic evidence summary was returned.",
		})
		answer = fallbackAnswer(plan, findings, limitations)
		synthesisStep.Status = StatusFailed
		synthesisStep.Summary = "fallback evidence summary"
		if verification != nil {
			verification.Decision = "abstain"
			verification.Citations = auditCitations(answer, findings)
		}
	} else if citationAudit := auditCitations(answer, findings); !citationAudit.Passed {
		limitations = append(limitations, Limitation{
			Role:    RoleSynthesis,
			Code:    citationFailureCode(citationAudit),
			Message: citationFailureMessage(citationAudit),
		})
		answer = fallbackAnswer(plan, findings, limitations)
		synthesisStep.Status = StatusFailed
		synthesisStep.Summary = "verifier rejected answer with invalid evidence citations"
		synthesisStep.Metadata = map[string]any{"verifier": "evidence_graph_citations"}
		if verification != nil {
			verification.Decision = "rejected"
			verification.Citations = citationAudit
		}
	} else {
		synthesisStep.Status = StatusCompleted
		synthesisStep.Summary = "evidence-bound answer generated"
		if verification != nil {
			verification.Citations = citationAudit
		}
	}
	steps = append(steps, synthesisStep)
	finishSynthesis(synthesisStep.Status, synthesisStep.Summary)

	result := Result{
		Answer:        answer,
		Plan:          plan,
		Steps:         steps,
		Findings:      findings,
		Limitations:   limitations,
		EvidenceGraph: evidenceGraph,
		Verification:  verification,
		Metadata: map[string]any{
			"workflow":               "multi_agent",
			"runtime":                RuntimeLegacy,
			"duration_ms":            o.now().Sub(startedAt).Milliseconds(),
			"evidence_finding_count": len(findings),
			"limitation_count":       len(limitations),
			"replan_rounds":          replanRounds,
		},
	}
	finishAgentSpan(invocationSpan, result)
	return result, nil
}

func (o *Orchestrator) executeBranches(ctx context.Context, query string, plan Plan) ([]Finding, []Step, []Limitation) {
	type outcome struct {
		role       Role
		finding    Finding
		step       Step
		limitation *Limitation
	}

	selected := make(map[Role]struct{}, len(plan.SelectedRoles))
	for _, role := range plan.SelectedRoles {
		selected[role] = struct{}{}
	}

	roles := []Role{RoleEvidence, RoleKnowledge}
	outcomes := make(chan outcome, len(roles))
	var group sync.WaitGroup
	for _, role := range roles {
		if _, ok := selected[role]; !ok {
			outcomes <- outcome{role: role, step: Step{Role: role, Status: StatusSkipped, Summary: "not selected by intent router", StartedAt: o.now()}}
			continue
		}
		executor := o.executors[role]
		if executor == nil {
			limitation := Limitation{Role: role, Code: "EXECUTOR_UNAVAILABLE", Message: "The selected role is not configured."}
			outcomes <- outcome{role: role, step: Step{Role: role, Status: StatusFailed, Summary: limitation.Message, StartedAt: o.now()}, limitation: &limitation}
			continue
		}
		group.Add(1)
		go func(role Role, executor Executor) {
			defer group.Done()
			startedAt := o.now()
			execCtx := ctx
			var cancel context.CancelFunc
			if o.branchTimeout > 0 {
				execCtx, cancel = context.WithTimeout(ctx, o.branchTimeout)
				defer cancel()
			}
			execCtx, roleSpan := startRoleInvocation(execCtx, role, RuntimeLegacy)
			finding, err := executor.Execute(execCtx, query, plan)
			finishRoleSpan(roleSpan, finding, err)
			step := Step{Role: role, StartedAt: startedAt, DurationMS: o.now().Sub(startedAt).Milliseconds()}
			if err != nil {
				limitation := Limitation{Role: role, Code: strings.ToUpper(string(role)) + "_AGENT_FAILED", Message: err.Error()}
				step.Status = StatusFailed
				step.Summary = limitation.Message
				outcomes <- outcome{role: role, step: step, limitation: &limitation}
				return
			}
			finding.Role = role
			step.Status = StatusCompleted
			step.Summary = finding.Summary
			step.Metadata = map[string]any{"evidence_count": len(finding.Evidence)}
			outcomes <- outcome{role: role, finding: finding, step: step}
		}(role, executor)
	}
	group.Wait()
	close(outcomes)

	findings := make([]Finding, 0, len(roles))
	steps := make([]Step, 0, len(roles))
	limitations := make([]Limitation, 0)
	for item := range outcomes {
		steps = append(steps, item.step)
		if item.finding.Summary != "" || len(item.finding.Evidence) > 0 {
			findings = append(findings, item.finding)
		}
		if item.limitation != nil {
			limitations = append(limitations, *item.limitation)
		}
	}
	sort.Slice(steps, func(i, j int) bool { return rolePosition(steps[i].Role) < rolePosition(steps[j].Role) })
	sort.Slice(findings, func(i, j int) bool { return rolePosition(findings[i].Role) < rolePosition(findings[j].Role) })
	return findings, steps, limitations
}

func rolePosition(role Role) int {
	for index, expected := range roleOrder() {
		if role == expected {
			return index
		}
	}
	return len(roleOrder())
}

func partialFailureLimitations(findings []Finding) []Limitation {
	out := make([]Limitation, 0)
	for _, finding := range findings {
		if finding.Metadata == nil {
			continue
		}
		partial, _ := finding.Metadata["partial_failure"].(bool)
		if !partial {
			continue
		}
		msg := "One evidence data source was unavailable."
		if errText, ok := finding.Metadata["alerts_error"].(string); ok && errText != "" {
			msg = "Alerts source unavailable: " + errText
		} else if errText, ok := finding.Metadata["logs_error"].(string); ok && errText != "" {
			msg = "Logs source unavailable: " + errText
		} else if gaps := metadataStrings(finding.Metadata, "correlation_data_gaps"); len(gaps) > 0 {
			msg = "Correlation continued with unavailable source(s): " + strings.Join(gaps, ", ")
		} else if failedTools := metadataInt(finding.Metadata, "tool_error_count"); failedTools > 0 {
			msg = fmt.Sprintf("Restricted evidence collection continued after %d tool call failure(s).", failedTools)
		}
		out = append(out, Limitation{
			Role:    finding.Role,
			Code:    "PARTIAL_EVIDENCE_SOURCE_FAILURE",
			Message: msg,
		})
	}
	return out
}

func evidenceInsufficient(plan Plan, findings []Finding, limitations []Limitation) bool {
	needsEvidence := false
	for _, role := range plan.SelectedRoles {
		if role == RoleEvidence {
			needsEvidence = true
			break
		}
	}
	if !needsEvidence {
		return false
	}
	for _, finding := range findings {
		if finding.Role == RoleEvidence && len(finding.Evidence) > 0 {
			return false
		}
	}
	for _, limitation := range limitations {
		if limitation.Role == RoleEvidence {
			return true
		}
	}
	return true
}

func (o *Orchestrator) assessEvidence(plan Plan, findings []Finding) (*EvidenceGraph, EvidenceAssessment) {
	if o == nil || !o.verification.enabled() {
		return nil, EvidenceAssessment{}
	}
	return buildEvidenceGraph(plan, findings, o.verification)
}

func (o *Orchestrator) shouldReplan(plan Plan, findings []Finding, limitations []Limitation, assessment EvidenceAssessment) bool {
	if o != nil && o.verification.enabled() {
		return assessment.NeedsMoreEvidence
	}
	return evidenceInsufficient(plan, findings, limitations)
}

func verificationLimitations(assessment EvidenceAssessment) []Limitation {
	limitations := make([]Limitation, 0, 2)
	if assessment.ContradictionCount > 0 {
		limitations = append(limitations, Limitation{
			Role:    RoleEvidence,
			Code:    "CONTRADICTORY_EVIDENCE",
			Message: fmt.Sprintf("The evidence graph contains %d counter-evidence relation(s); competing explanations remain open.", assessment.ContradictionCount),
		})
	}
	if assessment.NeedsMoreEvidence {
		limitations = append(limitations, Limitation{
			Role:    RoleEvidence,
			Code:    "EVIDENCE_CONFIDENCE_LOW",
			Message: fmt.Sprintf("Evidence confidence is %.3f (%s); source quorum or confidence threshold was not met, so the agent abstains from a definitive root-cause claim.", assessment.Score, assessment.StopReason),
		})
	}
	return limitations
}

func appendUniqueLimitations(base []Limitation, extra ...Limitation) []Limitation {
	seen := make(map[string]bool, len(base)+len(extra))
	for _, limitation := range base {
		seen[string(limitation.Role)+"\x00"+limitation.Code] = true
	}
	for _, limitation := range extra {
		key := string(limitation.Role) + "\x00" + limitation.Code
		if seen[key] {
			continue
		}
		seen[key] = true
		base = append(base, limitation)
	}
	return base
}

func verificationDecision(assessment EvidenceAssessment) string {
	if assessment.EvidenceCount == 0 || assessment.Band == "abstain" {
		return "abstain"
	}
	if assessment.NeedsMoreEvidence || assessment.Band == "low" {
		return "degraded"
	}
	return "accept"
}

func citationFailureCode(audit CitationAudit) string {
	if len(audit.InvalidIDs) > 0 {
		return "EVIDENCE_CITATION_INVALID"
	}
	return "EVIDENCE_CITATION_MISSING"
}

func citationFailureMessage(audit CitationAudit) string {
	if len(audit.InvalidIDs) > 0 {
		return "Model answer referenced evidence IDs that do not exist in the evidence graph; a deterministic evidence summary was returned."
	}
	return "Model answer did not cite a valid bracketed evidence ID; a deterministic evidence summary was returned."
}

func mergeFindings(base, extra []Finding) []Finding {
	byRole := make(map[Role]Finding, len(base)+len(extra))
	order := make([]Role, 0, len(base)+len(extra))
	for _, finding := range base {
		if _, ok := byRole[finding.Role]; !ok {
			order = append(order, finding.Role)
		}
		finding.Evidence = mergeEvidenceItems(nil, finding.Evidence)
		byRole[finding.Role] = finding
	}
	for _, finding := range extra {
		existing, ok := byRole[finding.Role]
		if !ok {
			byRole[finding.Role] = finding
			order = append(order, finding.Role)
			continue
		}
		existing.Evidence = mergeEvidenceItems(existing.Evidence, finding.Evidence)
		if finding.Summary != "" {
			existing.Summary = existing.Summary + " | " + finding.Summary
		}
		byRole[finding.Role] = existing
	}
	out := make([]Finding, 0, len(order))
	for _, role := range order {
		out = append(out, byRole[role])
	}
	return out
}

func mergeEvidenceItems(base, extra []EvidenceItem) []EvidenceItem {
	out := make([]EvidenceItem, 0, len(base)+len(extra))
	byID := make(map[string]int, len(base)+len(extra))
	appendItem := func(item EvidenceItem) {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			out = append(out, item)
			return
		}
		if index, ok := byID[id]; ok {
			// Prefer the richer repeat without allowing a second acquisition of
			// the same stable evidence ID to increase confidence.
			if len(item.Content) > len(out[index].Content) {
				out[index] = item
			}
			return
		}
		byID[id] = len(out)
		out = append(out, item)
	}
	for _, item := range base {
		appendItem(item)
	}
	for _, item := range extra {
		appendItem(item)
	}
	return out
}

func hasClaimableEvidence(findings []Finding) bool {
	return len(claimableEvidenceIDs(findings)) > 0
}

func fallbackAnswer(plan Plan, findings []Finding, limitations []Limitation) string {
	var builder strings.Builder
	claimableIDs := claimableEvidenceIDs(findings)
	builder.WriteString("Current conclusion\n")
	if len(findings) == 0 {
		builder.WriteString("No verified runtime or knowledge evidence was available for this request.\n")
	} else {
		wroteFinding := false
		for _, finding := range findings {
			if metadataBool(finding.Metadata, "memory") || metadataBool(finding.Metadata, "not_runtime_evidence") {
				continue
			}
			if !findingHasClaimableEvidence(finding, claimableIDs) {
				continue
			}
			wroteFinding = true
			builder.WriteString("- ")
			if summary := strings.TrimSpace(finding.Summary); summary != "" {
				builder.WriteString(summary)
			} else {
				builder.WriteString(string(finding.Role))
				builder.WriteString(" evidence")
			}
			builder.WriteString("\n")
			for _, evidence := range finding.Evidence {
				if !claimableIDs[strings.TrimSpace(evidence.ID)] {
					continue
				}
				builder.WriteString("  - [")
				builder.WriteString(evidence.ID)
				builder.WriteString("] ")
				if strings.EqualFold(metadataString(evidence.Metadata, "stance"), "counter") {
					builder.WriteString("COUNTER-EVIDENCE: ")
				}
				builder.WriteString(truncate(evidence.Content, 200))
				builder.WriteString("\n")
			}
		}
		if !wroteFinding {
			builder.WriteString("No verified runtime or knowledge evidence was available for this request.\n")
		}
	}
	if len(limitations) > 0 {
		builder.WriteString("\nMissing data\n")
		for _, limitation := range limitations {
			builder.WriteString("- ")
			builder.WriteString(limitation.Message)
			builder.WriteString("\n")
		}
	}
	if plan.Service != "" {
		builder.WriteString("\nSuggested next step\n- Validate the latest metrics and error logs for service ")
		builder.WriteString(plan.Service)
		builder.WriteString(".\n")
	}
	return strings.TrimSpace(builder.String())
}

func findingHasClaimableEvidence(finding Finding, claimableIDs map[string]bool) bool {
	for _, evidence := range finding.Evidence {
		if claimableIDs[strings.TrimSpace(evidence.ID)] {
			return true
		}
	}
	return false
}
