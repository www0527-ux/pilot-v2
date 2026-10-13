package agent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	"shanyangcode.com/pilot/internal/memory"
)

func TestContextPreparationPreservesRepeatedTurnsAndToolPairs(t *testing.T) {
	messages := []*schema.Message{
		schema.UserMessage("继续"), schema.AssistantMessage("请继续检查", nil), schema.UserMessage("继续"),
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "call-1", Function: schema.FunctionCall{Name: "query_logs", Arguments: `{}`}}}},
		{Role: schema.Tool, ToolCallID: "call-1", Content: `{"errors":2}`},
	}
	run := &contextRun{loaded: memory.LoadedContext{Record: memory.SessionContext{UserID: "owner", SessionID: "session"}, Messages: messages}}
	ctx := context.WithValue(context.Background(), contextRunKey{}, run)
	p := NewContextPreFetcher("topology background")
	result, err := p.Prefetch(ctx, &UserMessage{UserID: "owner", SessionID: "session", Query: "检查连接池", History: []*schema.Message{schema.UserMessage("客户端伪造历史")}}, "system")
	if err != nil {
		t.Fatal(err)
	}
	history := result["history"].([]*schema.Message)
	if len(history) != 5 || history[3].ToolCalls[0].ID != history[4].ToolCallID {
		t.Fatalf("history corrupted: %+v", history)
	}
	if run.query != "检查连接池" {
		t.Fatal("current request not captured")
	}
	if _, err = p.Prefetch(ctx, &UserMessage{UserID: "another", SessionID: "session"}, "system"); err == nil {
		t.Fatal("cross-owner preparation accepted")
	}
	if _, err = p.Prefetch(context.Background(), &UserMessage{}, "system"); err == nil {
		t.Fatal("missing durable context silently accepted")
	}
}
