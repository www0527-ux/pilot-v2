package multiagent

import (
	"context"
	"regexp"
	"strings"
)

type requestWindowContextKey struct{}

func WithRequestWindowMinutes(ctx context.Context, minutes int) context.Context {
	if minutes <= 0 {
		return ctx
	}
	if minutes > 1440 {
		minutes = 1440
	}
	return context.WithValue(ctx, requestWindowContextKey{}, minutes)
}

func RequestWindowMinutes(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	minutes, _ := ctx.Value(requestWindowContextKey{}).(int)
	return minutes
}

func applyRequestWindow(ctx context.Context, plan Plan) Plan {
	if minutes := RequestWindowMinutes(ctx); minutes > 0 {
		plan.Metadata = copyMetadata(plan.Metadata)
		plan.Metadata["duration_minutes"] = minutes
	}
	return plan
}

var servicePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bservice\s*(?::=|[:=])\s*([a-z0-9][a-z0-9._-]*)`),
	regexp.MustCompile(`(?i)(?:服务名称|服务名|服务|应用)\s*(?::=|[:：=]|为|是)\s*([a-z0-9][a-z0-9._-]*)`),
	regexp.MustCompile(`(?i)([a-z0-9][a-z0-9._-]*)\s*(?:服务|应用)(?:出现|发生|正在|有)?(?:故障|异常|报错|超时|错误)`),
}

// RulePlanner 按规则做可预期路由，只启用能提供真实证据的角色。
type RulePlanner struct {
	defaultService string
}

func NewRulePlanner(defaultService string) *RulePlanner {
	return &RulePlanner{defaultService: strings.TrimSpace(defaultService)}
}

func (p *RulePlanner) Plan(query string) Plan {
	query = strings.TrimSpace(query)
	lower := strings.ToLower(query)
	service := p.defaultService
	explicitService := false
	if extracted, ok := explicitServiceFromQuery(query); ok {
		service = extracted
		explicitService = true
	}

	intent := "general_analysis"
	routingReason := "general operational request benefits from triage and knowledge context"
	switch {
	case containsAny(lower, "runbook", "knowledge", "document", "best practice", "文档", "知识库", "手册"):
		intent = "knowledge_query"
		routingReason = "knowledge-oriented request"
	case containsAny(lower, "metric", "metrics", "log", "logs", "trace", "traces", "alert", "dashboard", "指标", "日志", "链路", "告警", "监控"):
		intent = "observability_query"
		routingReason = "request refers to observable runtime evidence"
	case containsAny(lower, "incident", "outage", "error", "timeout", "failure", "故障", "异常", "超时", "报错", "排查"):
		intent = "incident_triage"
		routingReason = "incident request requires runtime evidence and operational guidance"
	}

	selected := ApplyIntentRoles(intent)
	plan := Plan{
		Intent:        intent,
		Service:       service,
		SelectedRoles: selected,
		SkippedRoles:  skippedRoles(selected),
		RoutingReason: routingReason,
		Metadata: map[string]any{
			"planner_source":   "rule",
			"explicit_service": explicitService,
			"rule_intent":      intent,
		},
	}
	return plan
}

// ApplyIntentRoles maps a known intent to the roles that may produce real evidence.
func ApplyIntentRoles(intent string) []Role {
	switch strings.ToLower(strings.TrimSpace(intent)) {
	case "knowledge_query":
		return []Role{RoleKnowledge, RoleSynthesis}
	case "observability_query", "incident_triage":
		return []Role{RoleTriage, RoleEvidence, RoleKnowledge, RoleSynthesis}
	default:
		return []Role{RoleTriage, RoleKnowledge, RoleSynthesis}
	}
}

// WidenEvidenceRoles forces evidence + knowledge collection for low-confidence routing.
func WidenEvidenceRoles() []Role {
	return []Role{RoleTriage, RoleEvidence, RoleKnowledge, RoleSynthesis}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func skippedRoles(selected []Role) []Role {
	selectedSet := make(map[Role]struct{}, len(selected))
	for _, role := range selected {
		selectedSet[role] = struct{}{}
	}
	skipped := make([]Role, 0, len(roleOrder()))
	for _, role := range roleOrder() {
		if _, ok := selectedSet[role]; !ok {
			skipped = append(skipped, role)
		}
	}
	return skipped
}

func explicitServiceFromQuery(query string) (string, bool) {
	for _, pattern := range servicePatterns {
		if matches := pattern.FindStringSubmatch(query); len(matches) == 2 {
			return matches[1], true
		}
	}
	return "", false
}
