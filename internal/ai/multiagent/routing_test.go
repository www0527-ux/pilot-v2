package multiagent

import "testing"

func TestRulePlannerRoutesKnowledgeRequestsWithoutEvidenceAgent(t *testing.T) {
	plan := NewRulePlanner("pilot-api").Plan("find the database timeout runbook")
	if plan.Intent != "knowledge_query" {
		t.Fatalf("intent = %q", plan.Intent)
	}
	if len(plan.SelectedRoles) != 2 || plan.SelectedRoles[0] != RoleKnowledge || plan.SelectedRoles[1] != RoleSynthesis {
		t.Fatalf("selected roles = %#v", plan.SelectedRoles)
	}
}

func TestRulePlannerExtractsExplicitService(t *testing.T) {
	plan := NewRulePlanner("pilot-api").Plan("investigate timeout service=checkout-api")
	if plan.Service != "checkout-api" {
		t.Fatalf("service = %q", plan.Service)
	}
	if plan.Intent != "incident_triage" {
		t.Fatalf("intent = %q", plan.Intent)
	}
}

func TestRulePlannerExtractsServiceSyntaxVariantsAndChinese(t *testing.T) {
	cases := map[string]string{
		"排查 service:checkout-api 超时": "checkout-api",
		"排查 service:=payment_v2 报错":  "payment_v2",
		"服务名称：order.api 出现故障":        "order.api",
		"checkout-api 服务发生异常":        "checkout-api",
	}
	for query, want := range cases {
		if got := NewRulePlanner("default").Plan(query).Service; got != want {
			t.Fatalf("query=%q service=%q want=%q", query, got, want)
		}
	}
}
