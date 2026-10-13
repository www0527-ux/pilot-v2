package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"shanyangcode.com/pilot/internal/ai/multiagent"
	"shanyangcode.com/pilot/internal/ai/progress"
)

type eventAgent struct{ fakeAgent }

func (a *eventAgent) Stream(context.Context, string, string, string) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{
		progress.Message(progress.Event{Kind: "tool", ID: "one", Name: "query_logs", Status: "running"}),
		progress.Message(progress.Event{Kind: "tool", ID: "one", Name: "query_logs", Status: "completed", Summary: "3条日志"}),
		progress.Message(progress.Event{Kind: "answer", Status: "started"}),
		progress.Message(progress.Event{Kind: "answer", Status: "delta", Text: "完整回答"}),
		progress.Message(progress.Event{Kind: "answer", Status: "ended"}),
	}), nil
}

func streamEvents(t *testing.T, body string) []progress.Event {
	t.Helper()
	var events []progress.Event
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var event progress.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("non-event SSE payload: %s: %v", line, err)
		}
		events = append(events, event)
	}
	if len(events) == 0 || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("missing events or terminal sentinel: %s", body)
	}
	for i, event := range events {
		if event.Sequence != uint64(i+1) || event.RunID != events[0].RunID {
			t.Fatalf("unordered event: %+v", event)
		}
	}
	return events
}

func TestStreamAlwaysUsesTaskEventsAndGeneratedRunID(t *testing.T) {
	for _, suffix := range []string{"", `,"include_progress":false`, `,"include_progress":true`} {
		history := &fakeHistory{}
		router := newChatRouter(&eventAgent{}, nil, &fakeShortTerm{}, history)
		body := strings.TrimSuffix(validChatBody, "}") + suffix + "}"
		response, _ := doJSONRequest(t, router, http.MethodPost, "/api/v1/chat/stream", body)
		events := streamEvents(t, response.Body.String())
		id := response.Header().Get("X-Pilot-Run-ID")
		if _, err := uuid.Parse(id); err != nil || events[0].RunID != id {
			t.Fatalf("missing generated task identity: %q", id)
		}
		if events[len(events)-1].Status != "completed" || len(history.records) != 2 || history.records[1].Content != "完整回答" {
			t.Fatalf("wrong status or answer: events=%v history=%v", events, history.records)
		}
		if strings.Contains(response.Body.String(), "event: progress") {
			t.Fatal("legacy stream protocol remains")
		}
	}
}

type eventMultiAgent struct {
	failure     bool
	interrupted bool
}

func (a *eventMultiAgent) Execute(ctx context.Context, _, _, _ string) (multiagent.Result, error) {
	finish := progress.Start(ctx, "role", "evidence", nil)
	if a.failure {
		finish("failed", "query failed")
		return multiagent.Result{}, errors.New("query failed")
	}
	finish("completed", "查到3条证据")
	result := multiagent.Result{Answer: "多 Agent 完整回答"}
	if a.interrupted {
		result.Answer = ""
		result.Metadata = map[string]any{"status": "interrupted", "checkpoint_id": "checkpoint"}
	} else if progress.Streaming(ctx) {
		progress.StartAnswer(ctx)
		progress.Emit(ctx, progress.Event{Kind: "answer", Status: "delta", Text: result.Answer})
		progress.Emit(ctx, progress.Event{Kind: "answer", Status: "ended"})
	}
	return result, nil
}

func TestMultiAgentTaskEventsCompletionFailureAndApproval(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agent  eventMultiAgent
		saved  int
		status string
	}{
		{"complete", eventMultiAgent{}, 2, "completed"},
		{"failure", eventMultiAgent{failure: true}, 0, "failed"},
		{"approval", eventMultiAgent{interrupted: true}, 0, "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := &fakeHistory{}
			router := newChatRouter(&fakeAgent{}, &tc.agent, &fakeShortTerm{}, history)
			body := strings.TrimSuffix(validChatBody, "}") + `,"include_progress":true}`
			response, _ := doJSONRequest(t, router, http.MethodPost, "/api/v1/chat/multi-agent", body)
			events := streamEvents(t, response.Body.String())
			if response.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(response.Body.String(), `"name":"evidence"`) {
				t.Fatalf("missing actual role events: %s", response.Body.String())
			}
			if events[len(events)-1].Status != tc.status || len(history.records) != tc.saved {
				t.Fatalf("wrong state or persistence: %v records=%v", events, history.records)
			}
		})
	}
}

type eventResumer struct{ eventMultiAgent }

func (a *eventResumer) Resume(ctx context.Context, _ string, _ bool) (multiagent.Result, error) {
	return a.Execute(ctx, "u1", "s1", "resume")
}

func TestResumeSelectsTaskEventsOrSynchronousJSON(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		c := NewChatController(&fakeAgent{}, nil, &fakeShortTerm{}, &fakeHistory{}).WithMultiAgentResumer(&eventResumer{})
		router := newChatRouter(&fakeAgent{}, nil, &fakeShortTerm{}, &fakeHistory{})
		router.POST("/resume", c.MultiAgentResume)
		body, _ := json.Marshal(map[string]any{"checkpoint_id": "checkpoint", "approve": true, "user_id": "u1", "session_id": "s1", "include_progress": streaming})
		response, envelope := doJSONRequest(t, router, http.MethodPost, "/resume", string(body))
		if streaming {
			events := streamEvents(t, response.Body.String())
			if events[len(events)-1].Status != "completed" {
				t.Fatal(events)
			}
		} else if !envelope.Success || !strings.Contains(response.Body.String(), `"answer":"多 Agent 完整回答"`) {
			t.Fatalf("synchronous resume failed: %s", response.Body.String())
		}
	}
}
