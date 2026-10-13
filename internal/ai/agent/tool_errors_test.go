package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"shanyangcode.com/pilot/internal/ai/tools"
	"shanyangcode.com/pilot/pkg/types"
)

type metricErrorSource struct{}

func (metricErrorSource) QueryMetrics(_ context.Context, query string, _, _ time.Time) ([]types.MetricData, error) {
	if query != "up" {
		return nil, fmt.Errorf("invalid parameter query: vector selector must contain at least one non-empty matcher")
	}
	return []types.MetricData{{Name: "up", Value: 1, Timestamp: time.Now()}}, nil
}

// Exercise the actual Eino tools node in both modes: one invalid query must
// preserve the other result and become a tool response, rather than aborting.
func TestToolQueryFailurePreservesOtherResults(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			ctx := context.Background()
			metrics, err := tools.NewQueryMetricsTool(metricErrorSource{})
			if err != nil {
				t.Fatal(err)
			}
			node, err := compose.NewToolNode(ctx, &compose.ToolsNodeConfig{
				Tools:               []tool.BaseTool{metrics},
				ToolCallMiddlewares: []compose.ToolMiddleware{recoverableToolErrors()},
			})
			if err != nil {
				t.Fatal(err)
			}
			input := schema.AssistantMessage("", []schema.ToolCall{
				{ID: "invalid", Function: schema.FunctionCall{Name: "query_metrics", Arguments: `{"query":"{__name__=~\".*\"}"}`}},
				{ID: "valid", Function: schema.FunctionCall{Name: "query_metrics", Arguments: `{"query":"up"}`}},
			})
			var messages []*schema.Message
			if streaming {
				stream, streamErr := node.Stream(ctx, input)
				if streamErr != nil {
					t.Fatal(streamErr)
				}
				defer stream.Close()
				var chunks [][]*schema.Message
				for {
					chunk, recvErr := stream.Recv()
					if recvErr == io.EOF {
						break
					}
					if recvErr != nil {
						t.Fatal(recvErr)
					}
					chunks = append(chunks, chunk)
				}
				messages, err = schema.ConcatMessageArray(chunks)
			} else {
				messages, err = node.Invoke(ctx, input)
			}
			if err != nil || len(messages) != 2 {
				t.Fatalf("messages=%v err=%v", messages, err)
			}
			results := map[string]bool{}
			for _, message := range messages {
				var payload struct {
					Success bool `json:"success"`
					Error   struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(message.Content), &payload); err != nil {
					t.Fatal(err)
				}
				results[message.ToolCallID] = payload.Success
				if message.ToolCallID == "invalid" && (payload.Error.Code != "TOOL_QUERY_FAILED" || !strings.Contains(message.Content, "PromQL")) {
					t.Fatalf("missing corrective error response: %s", message.Content)
				}
			}
			if results["invalid"] || !results["valid"] {
				t.Fatalf("tool failures must remain distinct from successful evidence: %v", results)
			}
		})
	}
}

func TestToolRecoveryPreservesCancellationAndDeadlines(t *testing.T) {
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded} {
		endpoint := recoverableToolErrors().Invokable(func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			return nil, failure
		})
		output, err := endpoint(context.Background(), &compose.ToolInput{Name: "query_metrics"})
		if output != nil || !errors.Is(err, failure) {
			t.Fatalf("cancellation swallowed: output=%v err=%v", output, err)
		}
	}
}

func TestUnknownToolDoesNotAbortValidParallelCall(t *testing.T) {
	ctx := context.Background()
	clock, err := tools.NewGetCurrentTimeTool()
	if err != nil {
		t.Fatal(err)
	}
	node, err := compose.NewToolNode(ctx, &compose.ToolsNodeConfig{Tools: []tool.BaseTool{clock}, UnknownToolsHandler: unknownToolResult})
	if err != nil {
		t.Fatal(err)
	}
	output, err := node.Invoke(ctx, schema.AssistantMessage("", []schema.ToolCall{
		{ID: "invented", Function: schema.FunctionCall{Name: "search_exp_placeholder", Arguments: `{}`}},
		{ID: "valid", Function: schema.FunctionCall{Name: "get_current_time", Arguments: `{}`}},
	}))
	if err != nil || len(output) != 2 {
		t.Fatalf("output=%v err=%v", output, err)
	}
	for _, m := range output {
		if m.ToolCallID == "invented" && !strings.Contains(m.Content, "UNKNOWN_TOOL") {
			t.Fatal("unknown tool became fake success")
		}
		if m.ToolCallID == "valid" {
			var result struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal([]byte(m.Content), &result); err != nil || !result.Success {
				t.Fatalf("valid call discarded: %s", m.Content)
			}
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := unknownToolResult(cancelled, "invented", ""); !errors.Is(err, context.Canceled) {
		t.Fatal("unknown-tool recovery swallowed cancellation")
	}
}
