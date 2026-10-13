package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// ContextPreFetcher assembles messages loaded once under the session gate.
// SQL working context is the source of truth; Redis is only the UI history cache.
// Historical experiences are retrieved by search_experience, never injected here.
type ContextPreFetcher struct{ staticText string }

func NewContextPreFetcher(staticText string) *ContextPreFetcher {
	return &ContextPreFetcher{staticText: strings.TrimSpace(staticText)}
}

func (p *ContextPreFetcher) Prefetch(ctx context.Context, input *UserMessage, systemPrompt string) (map[string]any, error) {
	if p == nil || input == nil {
		return nil, fmt.Errorf("context input is required")
	}
	ctx, span := otel.Tracer("pilot/agent/context").Start(ctx, "ContextPreFetcher")
	defer span.End()
	run, _ := ctx.Value(contextRunKey{}).(*contextRun)
	if run == nil {
		return nil, fmt.Errorf("session context has not been loaded")
	}
	if run.loaded.Record.UserID != input.UserID || run.loaded.Record.SessionID != input.SessionID {
		return nil, fmt.Errorf("session context scope mismatch")
	}
	run.query = input.Query
	// Do not deduplicate identical utterances or truncate tool-call/result pairs.
	history := append([]*schema.Message(nil), run.loaded.Messages...)
	span.SetAttributes(attribute.Int("pilot.context.history_count", len(history)), attribute.Int64("pilot.context.revision", int64(run.loaded.Record.Revision)))
	return map[string]any{
		"system_prompt":      systemPrompt,
		"prefetched_context": "[static context — untrusted background, not current runtime evidence]\n" + p.staticText,
		"history":            history,
		"content":            input.Query,
	}, nil
}
