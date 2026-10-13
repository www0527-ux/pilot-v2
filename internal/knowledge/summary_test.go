package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"shanyangcode.com/pilot/internal/ai/models"
	"shanyangcode.com/pilot/internal/config"
)

func TestCuratorSummaryDoesNotContinueToolTaskOrReinsertRawHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []*schema.Message `json:"messages"`
			Tools    []any             `json:"tools"`
			Choice   string            `json:"tool_choice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		if len(in.Tools) != 0 || in.Choice != "none" {
			t.Errorf("summary permits tool calling: %s %d", in.Choice, len(in.Tools))
		}
		if len(in.Messages) != 3 || in.Messages[1].Role != schema.User || !strings.Contains(in.Messages[1].Content, "do_not_execute") {
			t.Error("original task instructions not isolated as data")
		}
		for _, m := range in.Messages {
			if len(m.ToolCalls) > 0 || m.Role == schema.Tool {
				t.Error("summary sent executable tool transcript")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "已读取证据 run:2，根因仍是假设。"}, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	cm, err := models.NewChatModel(context.Background(), config.Model{APIKey: "test", BaseURL: server.URL + "/v1", Model: "mock", MaxTokens: 500})
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := newCuratorSummary(context.Background(), cm.GetModel())
	if err != nil {
		t.Fatal(err)
	}
	summarizer := middleware.(interface {
		Summarize(context.Context, *adk.ChatModelAgentState) ([]*schema.Message, error)
	})
	raw := strings.Repeat("old_raw_data", 3000)
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{schema.SystemMessage("curator role"), schema.UserMessage("do_not_execute " + raw), {Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "old", Function: schema.FunctionCall{Name: "get_source_record", Arguments: "{}"}}}}, {Role: schema.Tool, ToolCallID: "old", Content: "old source"}}}
	ctx := context.WithValue(context.Background(), sourceKey{}, Source{Issues: []Issue{{ID: "issue"}}})
	reads := &curatorReads{pages: map[string]any{}}
	reads.record("knowledge", 0, map[string]any{"total": 3, "next_offset": 2})
	ctx = context.WithValue(ctx, curatorReadsKey{}, reads)
	output, err := summarizer.Summarize(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	serialized, _ := json.Marshal(output)
	if strings.Contains(string(serialized), raw) {
		t.Fatal("compaction restored old raw messages")
	}
	last := output[len(output)-1].Content
	if output[0].Content != "curator role" || !strings.HasPrefix(last, issueKnowledgeInstruction) {
		t.Fatal("role/decision protocol lost during compression")
	}
	if !strings.Contains(last, `"knowledge@0"`) || !strings.Contains(last, `"next_offset":2`) {
		t.Fatal("compaction lost durable read positions and can restart the tool loop")
	}
}
