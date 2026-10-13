package intent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type stubChatModel struct {
	content string
	err     error
}

func (m *stubChatModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &schema.Message{Role: schema.Assistant, Content: m.content}, nil
}

func (m *stubChatModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}

func TestLLMClassifierParsesValidJSON(t *testing.T) {
	classifier, err := NewLLMClassifier(&stubChatModel{
		content: `{"intent":"incident_triage","service":"checkout","selected_roles":["triage","evidence","knowledge","synthesis"],"confidence":0.91,"reason":"timeout outage"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := classifier.Classify(context.Background(), "checkout timeout")
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent != IntentIncidentTriage || got.Service != "checkout" || got.Confidence < 0.9 || got.Source != SourceLLM {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestLLMClassifierRejectsInvalidJSON(t *testing.T) {
	classifier, err := NewLLMClassifier(&stubChatModel{content: "not-json"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := classifier.Classify(context.Background(), "hello"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLLMClassifierRejectsUnknownIntent(t *testing.T) {
	classifier, err := NewLLMClassifier(&stubChatModel{
		content: `{"intent":"code_fix","service":"","selected_roles":[],"confidence":0.9,"reason":"x"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := classifier.Classify(context.Background(), "fix code"); err == nil {
		t.Fatal("expected unsupported intent error")
	}
}

func TestLLMClassifierStripsMarkdownFence(t *testing.T) {
	classifier, err := NewLLMClassifier(&stubChatModel{
		content: "```json\n{\"intent\":\"knowledge_query\",\"service\":\"\",\"selected_roles\":[\"knowledge\",\"synthesis\"],\"confidence\":0.8,\"reason\":\"docs\"}\n```",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := classifier.Classify(context.Background(), "runbook")
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent != IntentKnowledgeQuery {
		t.Fatalf("intent = %q", got.Intent)
	}
}
