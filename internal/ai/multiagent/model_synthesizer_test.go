package multiagent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	callbackutils "github.com/cloudwego/eino/utils/callbacks"
)

type synthesisModelStub struct {
	response string
	calls    int
}

func (m *synthesisModelStub) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.calls++
	return &schema.Message{Role: schema.Assistant, Content: m.response}, nil
}

func (m *synthesisModelStub) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: m.response}}), nil
}

func TestModelSynthesizerUsesComposableModelChain(t *testing.T) {
	model := &synthesisModelStub{response: "Evidence-bound conclusion"}
	synthesizer, err := NewModelSynthesizer(context.Background(), model)
	if err != nil {
		t.Fatalf("NewModelSynthesizer() error = %v", err)
	}

	answer, err := synthesizer.Synthesize(context.Background(), "investigate checkout", Plan{Intent: "incident_triage"}, nil, nil)
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if answer != "Evidence-bound conclusion" {
		t.Fatalf("answer = %q", answer)
	}
	if model.calls != 1 {
		t.Fatalf("model calls = %d, want 1", model.calls)
	}
}

func TestNewModelSynthesizerRejectsNilModel(t *testing.T) {
	if _, err := NewModelSynthesizer(context.Background(), nil); err == nil {
		t.Fatal("expected nil model error")
	}
}

func TestModelSynthesizerChainEmitsModelCallbacks(t *testing.T) {
	synthesizer, err := NewModelSynthesizer(context.Background(), &synthesisModelStub{response: "answer"})
	if err != nil {
		t.Fatalf("NewModelSynthesizer() error = %v", err)
	}

	var modelEnds atomic.Int32
	handler := callbackutils.NewHandlerHelper().ChatModel(&callbackutils.ModelCallbackHandler{
		OnEnd: func(ctx context.Context, _ *callbacks.RunInfo, _ *model.CallbackOutput) context.Context {
			modelEnds.Add(1)
			return ctx
		},
	}).Handler()

	_, err = synthesizer.chain.Invoke(
		context.Background(),
		[]*schema.Message{{Role: schema.User, Content: "investigate"}},
		compose.WithCallbacks(handler),
	)
	if err != nil {
		t.Fatalf("chain invoke: %v", err)
	}
	if got := modelEnds.Load(); got != 1 {
		t.Fatalf("model callback ends = %d, want 1", got)
	}
}
