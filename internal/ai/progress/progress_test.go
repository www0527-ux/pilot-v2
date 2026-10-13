package progress

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

func TestToolFailureAndPreviewDoNotPublishCredentials(t *testing.T) {
	status, summary, preview := ToolResult(`{"success":false,"error":{"message":"bad selector"},"api_key":"sk-secret1234","result":"Authorization=Bearer-secret", "nested":{"password":"abc"}}`)
	if status != "failed" || summary != "bad selector" {
		t.Fatalf("status=%s summary=%s", status, summary)
	}
	if strings.Contains(preview, "sk-secret1234") || strings.Contains(preview, "Bearer-secret") || strings.Contains(preview, "abc") {
		t.Fatalf("credential published: %s", preview)
	}
	params := SafeParameters(`{"service":"pilot-api","query":"up","api_key":"private","reasoning_content":"private"}`)
	if len(params) != 2 || params["service"] != "pilot-api" {
		t.Fatalf("unexpected public parameters: %v", params)
	}
	params = SafeParameters(`{"query":{"api_key":"private"},"duration_minutes":30}`)
	if len(params) != 1 || params["duration_minutes"] != float64(30) {
		t.Fatalf("unexpected non-scalar parameter: %v", params)
	}
	status, summary, _ = ToolResult(`{"success":true,"count":3}`)
	if status != "completed" || !strings.Contains(summary, "3") {
		t.Fatalf("status=%s summary=%s", status, summary)
	}
}

func TestCompleteToolResultsRetainEvidenceAndRedactCredentials(t *testing.T) {
	var events []Event
	ctx := WithEmitter(context.Background(), func(event Event) { events = append(events, event) })
	raw := map[string]any{"content": strings.Repeat("evidence ", 500) + "end-marker", "nested": map[string]any{"apiKey": "private-value"}}
	data, _ := json.Marshal(raw)
	handler := ToolCallbacks()
	callCtx := handler.OnStart(ctx, nil, &tool.CallbackInput{ArgumentsInJSON: `{}`})
	handler.OnEnd(callCtx, nil, &tool.CallbackOutput{Response: string(data)})
	finish := StartTool(ctx, "search_knowledge", nil)
	finish("completed", "1 document", raw)
	for _, index := range []int{1, 3} {
		data, err := json.Marshal(events[index].Result)
		if err != nil || !strings.Contains(string(data), "end-marker") || strings.Contains(string(data), "private-value") {
			t.Fatalf("missing evidence or credential leaked: %s, err=%v", data, err)
		}
	}
	if strings.Contains(events[1].ResultPreview, "end-marker") {
		t.Fatal("fixture must exceed preview length")
	}
	if raw["nested"].(map[string]any)["apiKey"] != "private-value" {
		t.Fatal("publishing must not mutate the original tool result")
	}
}

func TestOperationsHaveSeparateIDsAndTerminalEvents(t *testing.T) {
	var events []Event
	ctx := WithEmitter(context.Background(), func(event Event) { events = append(events, event) })
	first := Start(ctx, "tool", "query_metrics", nil)
	first("failed", "invalid selector")
	second := Start(ctx, "tool", "query_metrics", nil)
	second("completed", "1 result")
	if len(events) != 4 || events[0].ID == events[2].ID || events[1].Status != "failed" || events[3].Status != "completed" {
		t.Fatalf("events=%v", events)
	}
	message := Message(events[1])
	if message.Content != "" {
		t.Fatal("progress must not become answer text")
	}
	if _, ok := FromMessage(message); !ok {
		t.Fatal("progress was not preserved")
	}
	data, err := json.Marshal(events)
	if err != nil || !strings.Contains(string(data), "timestamp") {
		t.Fatalf("marshal=%s err=%v", data, err)
	}
}
