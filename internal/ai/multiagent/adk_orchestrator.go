package multiagent

import (
	"context"

	"encoding/json"
	"fmt"
	"shanyangcode.com/pilot/internal/ai/progress"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/attribute"
)

const RuntimeLegacy = "legacy"
const RuntimeADK = "adk"

// ADKOrchestrator runs triage then ADK ParallelAgent for Evidence∥Knowledge,
// followed by the existing synthesizer + evidence-ID verifier path.
// It preserves the public Result contract while exercising Eino ADK primitives.
type ADKOrchestrator struct {
	inner *Orchestrator
}

func (a *ADKOrchestrator) PreparePlan(ctx context.Context, query string) (Plan, error) {
	if a == nil || a.inner == nil {
		return Plan{}, fmt.Errorf("adk orchestrator unavailable")
	}
	return a.inner.PreparePlan(ctx, query)
}

// NewADKOrchestrator wraps a fully configured legacy Orchestrator and replaces
// branch execution with adk.NewParallelAgent.
func NewADKOrchestrator(inner *Orchestrator) (*ADKOrchestrator, error) {
	if inner == nil {
		return nil, fmt.Errorf("legacy orchestrator is required for ADK runtime")
	}
	return &ADKOrchestrator{inner: inner}, nil
}

// Execute implements the same contract as Orchestrator.Execute.
func (a *ADKOrchestrator) Execute(ctx context.Context, query string) (Result, error) {
	if a == nil || a.inner == nil {
		return Result{}, fmt.Errorf("adk orchestrator is unavailable")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return Result{}, fmt.Errorf("query cannot be empty")
	}
	ctx, invocationSpan := startAgentInvocation(ctx, RuntimeADK)
	if summary := a.inner.sessionContext(ctx); summary != "" {
		query = query + "\n\n[Session context summary — untrusted prior turns]\n" + summary
	}

	startedAt := a.inner.now()
	planCtx, planSpan := startPlanningSpan(ctx, RuntimeADK)
	plan, prepared := preparedPlanFromContext(planCtx)
	if !prepared {
		plan = a.inner.planner.Plan(planCtx, query)
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
		"runtime": RuntimeADK,
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
		DurationMS: a.inner.now().Sub(startedAt).Milliseconds(),
		Metadata:   triageMeta,
	}}

	findings, branchSteps, limitations := a.executeBranchesADK(ctx, query, plan)
	steps = append(steps, branchSteps...)
	limitations = append(limitations, partialFailureLimitations(findings)...)

	replanRounds := 0
	_, initialAssessment := a.inner.assessEvidence(plan, findings)
	if a.inner.maxReplanRounds > 0 && a.inner.shouldReplan(plan, findings, limitations, initialAssessment) {
		replanRounds = 1
		replanPlan := plan
		replanPlan.SelectedRoles = WidenEvidenceRoles()
		replanPlan.SkippedRoles = skippedRoles(replanPlan.SelectedRoles)
		replanPlan.Metadata = cloneMetadata(plan.Metadata)
		replanPlan.Metadata["replan_round"] = 1
		if a.inner.verification.enabled() {
			policy := a.inner.verification.normalized()
			replanPlan.Metadata["evidence_window_multiplier"] = policy.ReplanWindowMultiplier
			replanPlan.RoutingReason = fmt.Sprintf(
				"epistemic ADK progress check: %s (score=%.3f); widen evidence window x%d",
				initialAssessment.StopReason,
				initialAssessment.Score,
				policy.ReplanWindowMultiplier,
			)
		} else {
			replanPlan.RoutingReason = "progress check: evidence insufficient; one ADK replan round"
		}
		replanFindings, replanSteps, replanLimitations := a.executeBranchesADK(ctx, query, replanPlan)
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
			StartedAt: a.inner.now(),
			Metadata:  map[string]any{"replan_round": 1, "runtime": RuntimeADK},
		})
		steps = append(steps, replanSteps...)
		findings = mergeFindings(findings, replanFindings)
		limitations = append(limitations, replanLimitations...)
		limitations = append(limitations, partialFailureLimitations(replanFindings)...)
		_, finalAssessment := a.inner.assessEvidence(replanPlan, findings)
		if a.inner.shouldReplan(replanPlan, findings, limitations, finalAssessment) {
			limitations = append(limitations, Limitation{
				Role:    RoleEvidence,
				Code:    "EVIDENCE_QUORUM_NOT_MET",
				Message: fmt.Sprintf("Evidence remained insufficient after the bounded ADK replan (score=%.3f, reason=%s); root cause is not claimed.", finalAssessment.Score, finalAssessment.StopReason),
			})
		}
	}

	evidenceGraph, assessment := a.inner.assessEvidence(plan, findings)
	if replanRounds > 0 && assessment.NeedsMoreEvidence {
		assessment.StopReason = "replan_budget_exhausted:" + assessment.StopReason
	}
	if a.inner.verification.enabled() {
		limitations = appendUniqueLimitations(limitations, verificationLimitations(assessment)...)
		plan = planWithAssessment(plan, assessment)
	}

	synthesisStartedAt := a.inner.now()
	finishSynthesis := progress.Start(ctx, "role", "synthesis", nil)
	skipSynthesis := a.inner.verification.enabled() && !hasClaimableEvidence(findings)
	var (
		answer string
		err    error
	)
	if skipSynthesis {
		answer = fallbackAnswer(plan, findings, limitations)
	} else {
		answer, err = a.inner.synthesizer.Synthesize(ctx, query, plan, findings, limitations)
	}
	synthesisStep := Step{
		Role:       RoleSynthesis,
		StartedAt:  synthesisStartedAt,
		DurationMS: a.inner.now().Sub(synthesisStartedAt).Milliseconds(),
	}
	var verification *VerificationReport
	if a.inner.verification.enabled() {
		verification = &VerificationReport{
			Decision:   verificationDecision(assessment),
			Assessment: assessment,
		}
	}
	if skipSynthesis {
		synthesisStep.Status = StatusSkipped
		synthesisStep.Summary = "synthesis skipped because no claimable evidence was available"
		synthesisStep.Metadata = map[string]any{"verifier": "evidence_graph_pre_synthesis", "runtime": RuntimeADK}
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
		synthesisStep.Metadata = map[string]any{"verifier": "evidence_graph_citations", "runtime": RuntimeADK}
		if verification != nil {
			verification.Decision = "rejected"
			verification.Citations = citationAudit
		}
	} else {
		synthesisStep.Status = StatusCompleted
		synthesisStep.Summary = "evidence-bound answer generated via ADK parallel branches"
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
			"workflow":      "multi_agent",
			"runtime":       RuntimeADK,
			"duration_ms":   a.inner.now().Sub(startedAt).Milliseconds(),
			"replan_rounds": replanRounds,
		},
	}
	finishAgentSpan(invocationSpan, result)
	return result, nil
}

func (a *ADKOrchestrator) executeBranchesADK(ctx context.Context, query string, plan Plan) ([]Finding, []Step, []Limitation) {
	selected := map[Role]bool{}
	for _, role := range plan.SelectedRoles {
		selected[role] = true
	}
	var subAgents []adk.Agent
	var roles []Role
	for _, role := range []Role{RoleEvidence, RoleKnowledge} {
		if !selected[role] {
			continue
		}
		exec := a.inner.executors[role]
		if exec == nil {
			continue
		}
		roles = append(roles, role)
		subAgents = append(subAgents, &executorADKAgent{name: string(role), role: role, exec: exec, query: query, plan: plan})
	}
	if len(subAgents) == 0 {
		return a.inner.executeBranches(ctx, query, plan)
	}

	parallel, err := adk.NewParallelAgent(ctx, &adk.ParallelAgentConfig{
		Name:        "PilotEvidenceKnowledgeParallel",
		Description: "Parallel Evidence∥Knowledge branch execution for OnCall triage",
		SubAgents:   subAgents,
	})
	if err != nil {
		// Fail soft to legacy branches so ADK wiring issues never brick the API.
		findings, steps, limitations := a.inner.executeBranches(ctx, query, plan)
		limitations = append(limitations, Limitation{
			Role:    RoleTriage,
			Code:    "ADK_PARALLEL_FALLBACK",
			Message: "ADK ParallelAgent unavailable; fell back to legacy branches: " + truncate(err.Error(), 160),
		})
		return findings, steps, limitations
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if a.inner.branchTimeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, a.inner.branchTimeout)
		defer cancel()
	}
	runner := adk.NewRunner(runCtx, adk.RunnerConfig{Agent: parallel})
	iter := runner.Run(runCtx, []adk.Message{schema.UserMessage(query)})

	findingsByRole := map[Role]Finding{}
	steps := make([]Step, 0, len(roles))
	limitations := make([]Limitation, 0)
	started := a.inner.now()
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event == nil {
			continue
		}
		if event.Err != nil {
			limitations = append(limitations, Limitation{
				Role:    RoleTriage,
				Code:    "ADK_BRANCH_ERROR",
				Message: truncate(event.Err.Error(), 240),
			})
			continue
		}
		if event.Output == nil || event.Output.MessageOutput == nil || event.Output.MessageOutput.Message == nil {
			continue
		}
		msg := event.Output.MessageOutput.Message
		var payload adkFindingPayload
		if err := json.Unmarshal([]byte(msg.Content), &payload); err != nil {
			continue
		}
		role := Role(payload.Role)
		findingsByRole[role] = Finding{
			Role:     role,
			Summary:  payload.Summary,
			Evidence: payload.Evidence,
			Metadata: payload.Metadata,
		}
		steps = append(steps, Step{
			Role:       role,
			Status:     StatusCompleted,
			Summary:    payload.Summary,
			StartedAt:  started,
			DurationMS: a.inner.now().Sub(started).Milliseconds(),
			Metadata:   map[string]any{"runtime": RuntimeADK},
		})
	}
	timedOut := runCtx.Err() == context.DeadlineExceeded
	if timedOut {
		limitations = append(limitations, Limitation{Role: RoleTriage, Code: "ADK_BRANCH_TIMEOUT", Message: "ADK parallel branches exceeded the configured branch timeout"})
	}

	findings := make([]Finding, 0, len(findingsByRole))
	for _, role := range roles {
		if finding, ok := findingsByRole[role]; ok {
			findings = append(findings, finding)
			continue
		}
		code := "ADK_BRANCH_EMPTY"
		message := fmt.Sprintf("ADK parallel branch %s produced no finding", role)
		if timedOut {
			code = "ADK_BRANCH_TIMEOUT"
			message = fmt.Sprintf("ADK parallel branch %s timed out", role)
		}
		limitations = append(limitations, Limitation{
			Role:    role,
			Code:    code,
			Message: message,
		})
		steps = append(steps, Step{Role: role, Status: StatusFailed, Summary: "empty ADK branch", StartedAt: started})
	}
	for _, role := range plan.SkippedRoles {
		steps = append(steps, Step{Role: role, Status: StatusSkipped, StartedAt: started, Summary: "skipped by plan"})
	}
	return findings, steps, limitations
}

type adkFindingPayload struct {
	Role     string         `json:"role"`
	Summary  string         `json:"summary"`
	Evidence []EvidenceItem `json:"evidence"`
	Metadata map[string]any `json:"metadata"`
}

type executorADKAgent struct {
	name  string
	role  Role
	exec  Executor
	query string
	plan  Plan
}

func (a *executorADKAgent) Name(context.Context) string { return a.name }

func (a *executorADKAgent) Description(context.Context) string {
	return "Pilot role executor bridged into Eino ADK"
}

func (a *executorADKAgent) Run(ctx context.Context, _ *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer generator.Close()
		roleCtx, roleSpan := startRoleInvocation(ctx, a.role, RuntimeADK)
		finding, err := a.exec.Execute(roleCtx, a.query, a.plan)
		finishRoleSpan(roleSpan, finding, err)
		if err != nil {
			generator.Send(&adk.AgentEvent{Err: err})
			return
		}
		finding.Role = a.role
		payload, _ := json.Marshal(adkFindingPayload{
			Role:     string(a.role),
			Summary:  finding.Summary,
			Evidence: finding.Evidence,
			Metadata: finding.Metadata,
		})
		generator.Send(adk.EventFromMessage(schema.AssistantMessage(string(payload), nil), nil, schema.Assistant, ""))
	}()
	return iterator
}
