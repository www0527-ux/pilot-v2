package multiagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"shanyangcode.com/pilot/internal/ai/multiagent/intent"
)

const (
	PlannerModeRule   = "rule"
	PlannerModeHybrid = "hybrid"
)

// HybridPlanner merges RulePlanner with an optional LLM intent classifier.
type HybridPlanner struct {
	rules          *RulePlanner
	classifier     intent.Classifier
	minConfidence  float64
	timeout        time.Duration
	defaultService string
}

// HybridPlannerConfig configures hybrid routing behaviour.
type HybridPlannerConfig struct {
	DefaultService string
	Classifier     intent.Classifier
	MinConfidence  float64
	Timeout        time.Duration
}

// NewHybridPlanner builds a planner that always computes a rule baseline and
// optionally overlays LLM classification with safe fallbacks.
func NewHybridPlanner(cfg HybridPlannerConfig) (*HybridPlanner, error) {
	if cfg.Classifier == nil {
		return nil, fmt.Errorf("hybrid planner requires an intent classifier")
	}
	minConfidence := cfg.MinConfidence
	if minConfidence <= 0 || minConfidence > 1 {
		minConfidence = 0.6
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &HybridPlanner{
		rules:          NewRulePlanner(cfg.DefaultService),
		classifier:     cfg.Classifier,
		minConfidence:  minConfidence,
		timeout:        timeout,
		defaultService: strings.TrimSpace(cfg.DefaultService),
	}, nil
}

// Plan implements Planner with rule fallback and low-confidence widening.
func (p *HybridPlanner) Plan(ctx context.Context, query string) Plan {
	rulePlan := p.rules.Plan(query)
	if p == nil || p.classifier == nil {
		return rulePlan
	}
	if ctx == nil {
		ctx = context.Background()
	}

	classifyCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	classification, err := p.classifier.Classify(classifyCtx, query)
	if err != nil {
		plan := rulePlan
		plan.RoutingReason = rulePlan.RoutingReason + "; llm classification failed, using rule fallback"
		plan.SelectedRoles = WidenEvidenceRoles()
		plan.SkippedRoles = skippedRoles(plan.SelectedRoles)
		plan.Metadata = copyMetadata(rulePlan.Metadata)
		plan.Metadata["planner_source"] = intent.SourceFallback
		plan.Metadata["rule_intent"] = rulePlan.Intent
		plan.Metadata["classify_error"] = truncate(err.Error(), 200)
		plan.Metadata["forced_evidence_knowledge"] = true
		return plan
	}

	explicitService, hasExplicit := explicitServiceFromQuery(query)
	service := rulePlan.Service
	if hasExplicit {
		service = explicitService
	} else if strings.TrimSpace(classification.Service) != "" {
		service = strings.TrimSpace(classification.Service)
	} else if service == "" {
		service = p.defaultService
	}

	meta := copyMetadata(rulePlan.Metadata)
	meta["planner_source"] = intent.SourceLLM
	meta["rule_intent"] = rulePlan.Intent
	meta["llm_intent"] = classification.Intent
	meta["confidence"] = classification.Confidence
	meta["llm_reason"] = classification.Reason

	if classification.Confidence < p.minConfidence {
		roles := WidenEvidenceRoles()
		return Plan{
			Intent:        classification.Intent,
			Service:       service,
			SelectedRoles: roles,
			SkippedRoles:  skippedRoles(roles),
			RoutingReason: fmt.Sprintf("low confidence %.2f (min %.2f); widened to evidence+knowledge", classification.Confidence, p.minConfidence),
			Metadata:      meta,
		}
	}

	roles := rolesFromClassification(classification)
	return Plan{
		Intent:        classification.Intent,
		Service:       service,
		SelectedRoles: roles,
		SkippedRoles:  skippedRoles(roles),
		RoutingReason: classification.Reason,
		Metadata:      meta,
	}
}

func rolesFromClassification(c intent.Classification) []Role {
	if len(c.SelectedRoles) == 0 {
		return ApplyIntentRoles(c.Intent)
	}
	roles := make([]Role, 0, len(c.SelectedRoles))
	seen := map[Role]struct{}{}
	for _, raw := range c.SelectedRoles {
		role := Role(strings.ToLower(strings.TrimSpace(raw)))
		switch role {
		case RoleTriage, RoleEvidence, RoleKnowledge, RoleSynthesis:
			if _, ok := seen[role]; ok {
				continue
			}
			seen[role] = struct{}{}
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return ApplyIntentRoles(c.Intent)
	}
	// Ensure synthesis is always present for a usable answer path.
	if _, ok := seen[RoleSynthesis]; !ok {
		roles = append(roles, RoleSynthesis)
	}
	return roles
}

func copyMetadata(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+4)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ContextPlanner adapts a query-only planner to the context-aware Planner interface.
type ContextPlanner struct {
	inner interface{ Plan(query string) Plan }
}

// NewContextPlanner wraps RulePlanner (or any query-only planner).
func NewContextPlanner(inner interface{ Plan(query string) Plan }) *ContextPlanner {
	return &ContextPlanner{inner: inner}
}

func (p *ContextPlanner) Plan(_ context.Context, query string) Plan {
	return p.inner.Plan(query)
}
