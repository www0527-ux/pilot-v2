package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudwego/eino/schema"
	"shanyangcode.com/pilot/internal/ai/models"
	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/internal/identity"
	"shanyangcode.com/pilot/internal/memory"
)

func TestCuratorSubmitsTypedDecisionAndReturnsWithoutAnotherModelCall(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var in struct {
			Messages []*schema.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		name, args := "get_source_record", `{"section":"overview"}`
		if requests == 2 {
			name, args = "submit_knowledge", `{"candidates":[{"action":"NOOP","issue_id":"ISS-1","reason":"no reusable facts"}]}`
		} else if requests > 2 {
			t.Error("ReturnDirectly continued the model loop after validated submission")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": name, "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}, "finish_reason": "tool_calls"}}})
	}))
	defer server.Close()
	ctx := identity.WithOwner(context.Background(), "owner")
	cm, err := models.NewChatModel(ctx, config.Model{APIKey: "test", BaseURL: server.URL + "/v1", Model: "mock", MaxTokens: 6000})
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewAgentGenerator(ctx, cm.GetModel(), &Service{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := g.Generate(ctx, Source{Run: memory.AgentRun{ID: "run", UserID: "owner"}, Issues: []Issue{{ID: "ISS-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(result) != 1 || result[0].Action != "NOOP" || result[0].IssueID != "ISS-1" {
		t.Fatalf("typed submission not returned: calls=%d candidates=%v", requests, result)
	}
}

func TestCuratorCorrectionIsBoundedAndCannotBypassReferenceChecks(t *testing.T) {
	for _, issueID := range []string{"ISS-1", "invented"} {
		t.Run(issueID, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var in struct {
					Messages []*schema.Message `json:"messages"`
					Tools    []any             `json:"tools"`
					Choice   string            `json:"tool_choice"`
				}
				json.NewDecoder(r.Body).Decode(&in)
				message := map[string]any{"role": "assistant"}
				finish := "tool_calls"
				if requests == 1 {
					message["tool_calls"] = []any{map[string]any{"id": "submit", "type": "function", "function": map[string]any{"name": "submit_knowledge", "arguments": `{"candidates":[{"action":"NOOP","issue_id":"invented","reason":"not useful"}]}`}}}
				} else {
					if requests > 2 || len(in.Tools) != 0 || in.Choice != "none" {
						t.Error("correction restarted tools or exceeded its budget")
					}
					message["content"] = `{"candidates":[{"action":"NOOP","issue_id":"` + issueID + `","reason":"not useful"}]}`
					finish = "stop"
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
			}))
			defer server.Close()
			ctx := identity.WithOwner(context.Background(), "owner")
			cm, err := models.NewChatModel(ctx, config.Model{APIKey: "test", BaseURL: server.URL + "/v1", Model: "mock", MaxTokens: 6000})
			if err != nil {
				t.Fatal(err)
			}
			g, err := NewAgentGenerator(ctx, cm.GetModel(), &Service{})
			if err != nil {
				t.Fatal(err)
			}
			out, err := g.Generate(ctx, Source{Run: memory.AgentRun{ID: "run", UserID: "owner"}, Issues: []Issue{{ID: "ISS-1"}}})
			if requests != 2 {
				t.Fatalf("correction calls=%d", requests)
			}
			if issueID == "ISS-1" && (err != nil || len(out) != 1) {
				t.Fatalf("valid correction rejected: %v %v", out, err)
			}
			if issueID == "invented" && err == nil {
				t.Fatal("correction bypassed source validation")
			}
		})
	}
}
