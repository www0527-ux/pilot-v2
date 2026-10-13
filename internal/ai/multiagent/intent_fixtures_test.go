package multiagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type intentCase struct {
	Query           string `json:"query"`
	ExpectedIntent  string `json:"expected_intent"`
	ExpectEvidence  bool   `json:"expect_evidence"`
	ExpectedService string `json:"expected_service"`
}

func TestRulePlannerIntentCanaryFixtures(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "testdata", "intent_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []intentCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}

	planner := NewRulePlanner("pilot-api")
	for _, tc := range cases {
		plan := planner.Plan(tc.Query)
		if plan.Intent != tc.ExpectedIntent {
			t.Fatalf("query %q intent = %q, want %q", tc.Query, plan.Intent, tc.ExpectedIntent)
		}
		hasEvidence := false
		for _, role := range plan.SelectedRoles {
			if role == RoleEvidence {
				hasEvidence = true
				break
			}
		}
		if hasEvidence != tc.ExpectEvidence {
			t.Fatalf("query %q expect_evidence=%v got roles %#v", tc.Query, tc.ExpectEvidence, plan.SelectedRoles)
		}
		if tc.ExpectedService != "" && plan.Service != tc.ExpectedService {
			t.Fatalf("query %q service = %q, want %q", tc.Query, plan.Service, tc.ExpectedService)
		}
	}
}
