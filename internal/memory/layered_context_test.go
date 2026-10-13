package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type summarySourceFunc func(context.Context, string, string) (string, error)

func (f summarySourceFunc) SessionSummary(ctx context.Context, userID, sessionID string) (string, error) {
	return f(ctx, userID, sessionID)
}

type historySourceFunc func(context.Context, string, string, int) ([]ConversationHistory, error)

func (f historySourceFunc) GetSessionHistory(ctx context.Context, userID, sessionID string, limit int) ([]ConversationHistory, error) {
	return f(ctx, userID, sessionID, limit)
}

func TestLayeredSessionContextRecoversColdSessionFromMySQL(t *testing.T) {
	short := summarySourceFunc(func(context.Context, string, string) (string, error) { return "", nil })
	long := historySourceFunc(func(context.Context, string, string, int) ([]ConversationHistory, error) {
		return []ConversationHistory{{Role: "user", Content: "cold question"}, {Role: "assistant", Content: "persisted answer"}}, nil
	})
	text, err := NewLayeredSessionContext(short, long, 8).SessionSummary(context.Background(), "u", "s")
	if err != nil || !strings.Contains(text, "mysql memory/not_runtime_evidence") || !strings.Contains(text, "persisted answer") {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestLayeredSessionContextRunsInParallelAndIsolatesOneFailure(t *testing.T) {
	short := summarySourceFunc(func(context.Context, string, string) (string, error) {
		time.Sleep(60 * time.Millisecond)
		return "redis recent", nil
	})
	long := historySourceFunc(func(context.Context, string, string, int) ([]ConversationHistory, error) {
		time.Sleep(60 * time.Millisecond)
		return nil, errors.New("mysql down")
	})
	started := time.Now()
	text, err := NewLayeredSessionContext(short, long, 8).SessionSummary(context.Background(), "u", "s")
	if err != nil || text == "" || time.Since(started) > 110*time.Millisecond {
		t.Fatalf("text=%q err=%v elapsed=%s", text, err, time.Since(started))
	}
}
