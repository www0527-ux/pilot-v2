// Package progress publishes request-scoped execution observations for the UI.
package progress

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/schema"
)

const MessageKey = "pilot_progress"

type Event struct {
	Kind          string         `json:"kind"`
	ID            string         `json:"id,omitempty"`
	Name          string         `json:"name,omitempty"`
	Status        string         `json:"status"`
	Summary       string         `json:"summary,omitempty"`
	Parameters    map[string]any `json:"parameters,omitempty"`
	ResultPreview string         `json:"result_preview,omitempty"`
	DurationMS    int64          `json:"duration_ms,omitempty"`
	Timestamp     time.Time      `json:"timestamp"`
	RunID         string         `json:"run_id,omitempty"`
	Sequence      uint64         `json:"sequence,omitempty"`
	GroupID       string         `json:"group_id,omitempty"`
	GroupLabel    string         `json:"group_label,omitempty"`
	Round         uint64         `json:"round,omitempty"`
	Order         int            `json:"order,omitempty"`
	Text          string         `json:"text,omitempty"`
	Result        any            `json:"result,omitempty"`
}

func RunEvent(status, summary string) Event {
	return Event{Kind: "run", Status: status, Summary: summary, Timestamp: time.Now()}
}

type enabledKey struct{}
type streamingKey struct{}
type groupKey struct{}
type runKey struct{}
type sessionKey struct{}

func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}
func SessionID(ctx context.Context) string { id, _ := ctx.Value(sessionKey{}).(string); return id }

func WithRunID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runKey{}, id)
}
func RunID(ctx context.Context) string { id, _ := ctx.Value(runKey{}).(string); return id }

type emitterKey struct{}
type emitter struct {
	send     func(Event)
	sequence atomic.Uint64
	round    atomic.Uint64
}

// EnableStreaming selects incremental synthesis instead of synchronous synthesis.
func EnableStreaming(ctx context.Context) context.Context {
	return context.WithValue(Enable(ctx), streamingKey{}, true)
}
func Streaming(ctx context.Context) bool {
	streaming, _ := ctx.Value(streamingKey{}).(bool)
	return streaming
}
func WithGroup(ctx context.Context, id, label string) context.Context {
	return context.WithValue(ctx, groupKey{}, [2]string{id, label})
}

func Enable(ctx context.Context) context.Context { return context.WithValue(ctx, enabledKey{}, true) }
func Enabled(ctx context.Context) bool           { enabled, _ := ctx.Value(enabledKey{}).(bool); return enabled }
func WithEmitter(ctx context.Context, send func(Event)) context.Context {
	return context.WithValue(Enable(ctx), emitterKey{}, &emitter{send: send})
}
func Emit(ctx context.Context, event Event) {
	if sink, _ := ctx.Value(emitterKey{}).(*emitter); sink != nil {
		if event.Timestamp.IsZero() {
			event.Timestamp = time.Now()
		}
		if event.GroupID == "" {
			group, _ := ctx.Value(groupKey{}).([2]string)
			event.GroupID, event.GroupLabel = group[0], group[1]
		}
		if event.Round == 0 {
			event.Round = sink.round.Load()
		}
		event.Summary = Clip(event.Summary, 600)
		sink.send(event)
	}
}

func NextRound(ctx context.Context) {
	if sink, _ := ctx.Value(emitterKey{}).(*emitter); sink != nil {
		sink.round.Add(1)
	}
}
func Message(event Event) *schema.Message {
	return &schema.Message{Role: schema.Assistant, Extra: map[string]any{MessageKey: event}}
}
func FromMessage(message *schema.Message) (Event, bool) {
	if message == nil {
		return Event{}, false
	}
	event, ok := message.Extra[MessageKey].(Event)
	return event, ok
}

type action struct {
	event   Event
	started time.Time
}

func begin(ctx context.Context, kind, name, id string, parameters map[string]any) *action {
	sink, _ := ctx.Value(emitterKey{}).(*emitter)
	if sink == nil {
		return nil
	}
	if id == "" {
		id = fmt.Sprintf("%s-%d", kind, sink.sequence.Add(1))
	} else {
		// Providers can reuse call IDs in later model rounds.
		id = fmt.Sprintf("%s-%d", id, sink.sequence.Add(1))
	}
	state := &action{event: Event{Kind: kind, Name: name, ID: id, Status: "running", Parameters: parameters}, started: time.Now()}
	state.event.Round = sink.round.Load()
	Emit(ctx, state.event)
	return state
}
func (state *action) finish(ctx context.Context, status, summary, preview string, result ...any) {
	if state == nil {
		return
	}
	event := state.event
	event.Status, event.Summary, event.ResultPreview = status, summary, preview
	if len(result) > 0 {
		event.Result = PublicValue(result[0])
	}
	event.DurationMS = time.Since(state.started).Milliseconds()
	Emit(ctx, event)
}

// Start records an actual operation. A retry receives a new ID.
func Start(ctx context.Context, kind, name string, parameters map[string]any) func(string, string) {
	state := begin(ctx, kind, name, "", parameters)
	return func(status, summary string) { state.finish(ctx, status, summary, "") }
}

// StartTool retains the complete public result, independently of the short summary.
func StartTool(ctx context.Context, name string, parameters map[string]any) func(string, string, any) {
	state := begin(ctx, "tool", name, "", parameters)
	return func(status, summary string, result any) { state.finish(ctx, status, summary, "", result) }
}
func Clip(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return string(runes)
}
