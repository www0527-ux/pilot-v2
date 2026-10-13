package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"shanyangcode.com/pilot/internal/ai/models"
	"shanyangcode.com/pilot/internal/config"
)

func TestSummaryRequestDisablesThinkingAndRetriesEmptyText(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Thinking struct {
				Type string `json:"type"`
			} `json:"thinking"`
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Thinking.Type != "disabled" || input.MaxTokens != 1800 {
			t.Errorf("invalid summary request: %+v", input)
		}
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"internal reasoning"},"finish_reason":"length"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"已讨论 Redis 延迟，待确认持久化耗时；用户要求只读检查。"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	ctx := context.WithValue(context.Background(), contextRunKey{}, &contextRun{query: "继续检查 Redis"})
	cm, err := models.NewChatModel(ctx, config.Model{APIKey: "test", BaseURL: server.URL + "/v1", Model: "deepseek-flash"})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &summaryChatModel{BaseChatModel: cm.GetModel(), options: models.BackgroundOptions(config.Model{BaseURL: "https://api.deepseek.com"})}
	mw, err := summarization.New(ctx, &summarization.Config{Model: wrapped, Retry: summaryRetryConfig(), Trigger: &summarization.TriggerCondition{ContextMessages: 1}, ModelOptions: []model.Option{model.WithMaxTokens(1800)}, Finalize: finalizeSessionSummary})
	if err != nil {
		t.Fatal(err)
	}
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{schema.SystemMessage("system"), schema.UserMessage("之前的问题"), schema.AssistantMessage("之前的回答", nil), schema.UserMessage("继续检查 Redis")}}
	_, updated, err := mw.BeforeModelRewriteState(ctx, state, nil)
	if err != nil || calls.Load() != 2 || len(updated.Messages) != 3 || updated.Messages[2].Content != "继续检查 Redis" {
		t.Fatalf("calls=%d updated=%+v err=%v", calls.Load(), updated, err)
	}
	if strings.Contains(updated.Messages[1].Content, "internal reasoning") {
		t.Fatal("reasoning substituted for summary")
	}
	if len(state.Messages) != 4 {
		t.Fatal("original history mutated")
	}
}

type emptySummaryModel struct {
	model.BaseChatModel
	calls int
}

func (m *emptySummaryModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	m.calls++
	return schema.AssistantMessage("", nil), nil
}

func TestEmptySummaryExhaustionPreservesHistory(t *testing.T) {
	cm := &emptySummaryModel{}
	mw, err := summarization.New(context.Background(), &summarization.Config{Model: &summaryChatModel{BaseChatModel: cm}, Retry: summaryRetryConfig(), Trigger: &summarization.TriggerCondition{ContextMessages: 1}})
	if err != nil {
		t.Fatal(err)
	}
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{schema.UserMessage("original history")}}
	_, _, err = mw.BeforeModelRewriteState(context.Background(), state, nil)
	if err == nil || !strings.Contains(err.Error(), errEmptySummary.Error()) || cm.calls != 3 || state.Messages[0].Content != "original history" {
		t.Fatalf("calls=%d state=%+v err=%v", cm.calls, state, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if summaryRetryConfig().ShouldRetry(ctx, nil, errEmptySummary) {
		t.Fatal("cancelled summary retried")
	}
	if summaryRetryConfig().ShouldRetry(context.Background(), nil, errors.New("invalid request")) {
		t.Fatal("permanent summary error retried")
	}
}
