package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type summaryModelStub struct {
	calls int
	fail  bool
	input []*schema.Message
}

func (m *summaryModelStub) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.calls++
	m.input = in
	if m.fail {
		return nil, errors.New("summary provider unavailable")
	}
	return schema.AssistantMessage("先讨论接口延迟，已排除全站宕机；曾考虑替换数据库，因缺乏证据放弃。连接池仍待核实。", nil), nil
}
func (*summaryModelStub) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("unexpected summary stream")
}

func TestOfficialCompactionPreservesCurrentRequestAndDoesNotRunEveryTurn(t *testing.T) {
	ctx := context.WithValue(context.Background(), contextRunKey{}, &contextRun{query: "继续检查连接池，先解释，不修改"})
	cm := &summaryModelStub{}
	mw, err := summarization.New(ctx, &summarization.Config{Model: cm, Trigger: &summarization.TriggerCondition{ContextTokens: 5000}, TokenCounter: conservativeTokenCounter, Finalize: finalizeSessionSummary, UserInstruction: sessionSummaryInstruction})
	if err != nil {
		t.Fatal(err)
	}
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{schema.SystemMessage(investigationInstruction), schema.SystemMessage("应用系统提示词"), schema.UserMessage(strings.Repeat("历史原文", 4000)), schema.AssistantMessage("之前的答复", nil), schema.UserMessage("继续检查连接池，先解释，不修改")}}
	_, compressed, err := mw.BeforeModelRewriteState(ctx, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cm.calls != 1 || len(compressed.Messages) != 4 {
		t.Fatalf("calls=%d messages=%d", cm.calls, len(compressed.Messages))
	}
	if compressed.Messages[0].Content != investigationInstruction || compressed.Messages[1].Role != schema.System || compressed.Messages[3].Content != "继续检查连接池，先解释，不修改" {
		t.Fatal("phase/system/current request lost")
	}
	if strings.Contains(compressed.Messages[2].Content, "历史原文") {
		t.Fatal("old raw messages reintroduced after compaction")
	}
	if !strings.Contains(compressed.Messages[2].Content, "因缺乏证据放弃") || len(cm.input) < 2 {
		t.Fatal("whole-conversation summary missing")
	}
	_, _, err = mw.BeforeModelRewriteState(ctx, compressed, nil)
	if err != nil || cm.calls != 1 {
		t.Fatalf("compressed context summarized again: calls=%d err=%v", cm.calls, err)
	}
	if len(state.Messages) != 5 {
		t.Fatal("original raw history mutated")
	}
	cm.fail = true
	_, _, err = mw.BeforeModelRewriteState(ctx, state, nil)
	if err == nil || len(state.Messages) != 5 {
		t.Fatal("failed summary silently destroys the original history")
	}
}
