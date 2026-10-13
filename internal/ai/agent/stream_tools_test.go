package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"shanyangcode.com/pilot/internal/ai/models"
	"shanyangcode.com/pilot/internal/ai/retriever"
	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/internal/observability"
)

func newStreamingTestRunnable(t *testing.T, ctx context.Context, serverURL string) compose.Runnable[[]*schema.Message, *schema.Message] {
	t.Helper()
	chatModel, err := models.NewChatModel(ctx, config.Model{APIKey: "test-key", BaseURL: serverURL + "/v1", Model: "mock", MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	agent := &FullPilotAgent{knowledgeRetriever: &retriever.KnowledgeRetriever{}}
	lambda, err := agent.newADKAgentLambda(ctx, Deps{ChatModel: chatModel, Collector: &observability.DataCollector{}, MaxIterations: 8})
	if err != nil {
		t.Fatal(err)
	}
	graph := compose.NewGraph[[]*schema.Message, *schema.Message]()
	if err = graph.AddLambdaNode("agent", lambda); err != nil {
		t.Fatal(err)
	}
	if err = graph.AddEdge(compose.START, "agent"); err != nil {
		t.Fatal(err)
	}
	if err = graph.AddEdge("agent", compose.END); err != nil {
		t.Fatal(err)
	}
	runnable, err := graph.Compile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return runnable
}

func TestStreamToolCheckerPreservesErrorsAndPlainText(t *testing.T) {
	plain := schema.StreamReaderFromArray([]*schema.Message{nil, schema.AssistantMessage("完整回答", nil)})
	if found, err := streamContainsToolCalls(context.Background(), plain); found || err != nil {
		t.Fatalf("plain text: found=%v err=%v", found, err)
	}
	failure := errors.New("provider stream failed")
	reader, writer := schema.Pipe[*schema.Message](2)
	writer.Send(schema.AssistantMessage("准备查询", nil), nil)
	writer.Send(nil, failure)
	writer.Close()
	if found, err := streamContainsToolCalls(context.Background(), reader); found || !errors.Is(err, failure) {
		t.Fatalf("stream error swallowed: found=%v err=%v", found, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if found, err := streamContainsToolCalls(ctx, schema.StreamReaderFromArray([]*schema.Message{})); found || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation swallowed: found=%v err=%v", found, err)
	}
}

func TestStreamToolCheckerFindsCallsAfterPreface(t *testing.T) {
	reader := schema.StreamReaderFromArray([]*schema.Message{
		schema.AssistantMessage("I will query logs first.", nil),
		nil,
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "logs", Function: schema.FunctionCall{Name: "query_logs", Arguments: "{}"}}}},
	})
	if found, err := streamContainsToolCalls(context.Background(), reader); !found || err != nil {
		t.Fatalf("late tool call lost: found=%v err=%v", found, err)
	}
}
