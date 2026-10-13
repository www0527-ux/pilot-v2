package agent

import (
	"context"
	"errors"
	"io"

	"github.com/cloudwego/eino/schema"
)

// Some providers emit assistant text before tool calls. Text alone is not an
// end-of-turn signal: scan this branch's stream copy until a call or EOF. Eino
// retains a separate copy for tool execution or the final response.
func streamContainsToolCalls(ctx context.Context, output *schema.StreamReader[*schema.Message]) (bool, error) {
	defer output.Close()
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		chunk, err := output.Recv()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if chunk != nil && len(chunk.ToolCalls) > 0 {
			return true, nil
		}
	}
}
