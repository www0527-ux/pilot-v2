package progress

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// The producer explicitly chooses a tool-free model invocation before emitting
// started. No text, Markdown, timing, or tool-count heuristics classify a delta.
func StartAnswer(ctx context.Context) {
	Emit(ctx, Event{Kind: "answer", Status: "started", Summary: "正在生成最终回答"})
}

func ReadAnswer(ctx context.Context, reader *schema.StreamReader[*schema.Message]) (string, error) {
	defer reader.Close()
	var text strings.Builder
	for {
		if err := ctx.Err(); err != nil {
			return text.String(), err
		}
		chunk, err := reader.Recv()
		if errors.Is(err, io.EOF) {
			if strings.TrimSpace(text.String()) == "" {
				return "", errors.New("final model returned an empty answer")
			}
			Emit(ctx, Event{Kind: "answer", Status: "ended"})
			return text.String(), nil
		}
		if err != nil {
			return text.String(), err
		}
		if chunk == nil {
			continue
		}
		if len(chunk.ToolCalls) > 0 {
			return text.String(), errors.New("final answer model unexpectedly called a tool")
		}
		if chunk.Content != "" {
			text.WriteString(chunk.Content)
			Emit(ctx, Event{Kind: "answer", Status: "delta", Text: chunk.Content})
		}
	}
}
