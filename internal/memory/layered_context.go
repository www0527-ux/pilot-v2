package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

type sessionSummarySource interface {
	SessionSummary(ctx context.Context, userID, sessionID string) (string, error)
}

type sessionHistorySource interface {
	GetSessionHistory(ctx context.Context, userID, sessionID string, limit int) ([]ConversationHistory, error)
}

// LayeredSessionContext loads Redis short-term and MySQL long-term context in
// parallel. Either source may satisfy the request, enabling cold-session
// recovery without making context failures fatal to the main answer.
type LayeredSessionContext struct {
	short sessionSummarySource
	long  sessionHistorySource
	limit int
}

func NewLayeredSessionContext(short sessionSummarySource, long sessionHistorySource, limit int) *LayeredSessionContext {
	if limit <= 0 {
		limit = 8
	}
	return &LayeredSessionContext{short: short, long: long, limit: limit}
}

func (l *LayeredSessionContext) SessionSummary(ctx context.Context, userID, sessionID string) (string, error) {
	if l == nil {
		return "", fmt.Errorf("layered session context unavailable")
	}
	type result struct {
		name, text string
		err        error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	if l.short != nil {
		group.Add(1)
		go func() {
			defer group.Done()
			text, err := l.short.SessionSummary(ctx, userID, sessionID)
			results <- result{name: "redis", text: text, err: err}
		}()
	}
	if l.long != nil {
		group.Add(1)
		go func() {
			defer group.Done()
			rows, err := l.long.GetSessionHistory(ctx, userID, sessionID, l.limit)
			results <- result{name: "mysql", text: formatLongTermContext(rows), err: err}
		}()
	}
	go func() { group.Wait(); close(results) }()
	parts := make([]string, 0, 2)
	failures := make([]string, 0, 2)
	for loaded := range results {
		if loaded.err != nil {
			failures = append(failures, loaded.name+": "+loaded.err.Error())
			continue
		}
		if text := strings.TrimSpace(loaded.text); text != "" {
			parts = append(parts, "["+loaded.name+" memory/not_runtime_evidence]\n"+text)
		}
	}
	if len(parts) == 0 && len(failures) > 0 {
		return "", fmt.Errorf("load layered context failed: %s", strings.Join(failures, "; "))
	}
	return truncateRunes(strings.Join(parts, "\n\n"), 5000), nil
}

func formatLongTermContext(rows []ConversationHistory) string {
	var builder strings.Builder
	for _, row := range rows {
		builder.WriteString("- ")
		builder.WriteString(strings.TrimSpace(row.Role))
		builder.WriteString(": ")
		builder.WriteString(truncateRunes(row.Content, 500))
		builder.WriteByte('\n')
	}
	return strings.TrimSpace(builder.String())
}
