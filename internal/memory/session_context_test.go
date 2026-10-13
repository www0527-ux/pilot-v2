package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestWorkingContextCursorDelayedHistoryAndRevisionFence(t *testing.T) {
	db := openFeedbackTestDB(t)
	if err := db.AutoMigrate(&SessionContext{}, &ConversationHistory{}, &AgentRun{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m := NewSessionContextManager(db)
	h := NewConversationHistoryManager(db)
	first, err := m.Load(ctx, "a", "s")
	if err != nil {
		t.Fatal(err)
	}
	messages := []*schema.Message{schema.UserMessage("A"), schema.AssistantMessage("回答 A", nil)}
	if err := m.Save(ctx, first, messages, "run-a"); err != nil {
		t.Fatal(err)
	}
	// Another request starts before the controller persists A's visible history.
	second, err := m.Load(ctx, "a", "s")
	if err != nil {
		t.Fatal(err)
	}
	messages = append(second.Messages, schema.UserMessage("B"), schema.AssistantMessage("回答 B", nil))
	if err := m.Save(ctx, second, messages, "run-b"); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []struct{ run, user, answer string }{{"run-a", "A", "回答 A"}, {"run-b", "B", "回答 B"}} {
		if err := h.SaveMessageForRun(ctx, "a", "s", "user", turn.user, turn.run); err != nil {
			t.Fatal(err)
		}
		if err := h.SaveMessageForRun(ctx, "a", "s", "assistant", turn.answer, turn.run); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := m.Load(ctx, "a", "s")
	if err != nil || len(loaded.Messages) != 4 {
		t.Fatalf("history duplication: %d %v", len(loaded.Messages), err)
	}
	if err := m.Save(ctx, first, []*schema.Message{schema.UserMessage("stale")}, "old"); !errors.Is(err, ErrContextConflict) {
		t.Fatalf("stale overwrite: %v", err)
	}
	other, err := m.Load(ctx, "other", "s")
	if err != nil || len(other.Messages) != 0 {
		t.Fatal("account context leaked")
	}
	if err := h.DeleteSessionHistory(ctx, "a", "s"); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(ctx, loaded, messages, "late"); !errors.Is(err, ErrContextConflict) {
		t.Fatalf("clear resurrected context: %v", err)
	}
	blank, err := m.Load(ctx, "a", "s")
	if err != nil || len(blank.Messages) != 0 {
		t.Fatal("clear didn't reset context")
	}
}
