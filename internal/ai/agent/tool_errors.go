package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cloudwego/eino/compose"
	"shanyangcode.com/pilot/internal/ai/progress"
)

// An invented tool name never executes anything. Return an explicit observation
// through the official ToolsNode extension so the model can correct its call.
func unknownToolResult(ctx context.Context, name, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := json.Marshal(map[string]any{
		"success": false, "tool": progress.Clip(name, 128),
		"error": map[string]string{"code": "UNKNOWN_TOOL", "message": "工具未注册，本次调用没有执行"},
		"hint":  "只能调用本轮 tools Schema 列出的准确工具名，不要生成占位名称。历史经验工具的准确名称为 search_experience；若没有可用工具，应说明限制。",
	})
	return string(data), err
}

// A failed read-only query is an observation the model can correct or report.
// Cancellation still terminates the run instead of being disguised as data.
func recoverableToolErrors() compose.ToolMiddleware {
	return compose.ToolMiddleware{
		Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
			return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
				output, err := next(ctx, input)
				if err == nil {
					return output, nil
				}
				if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, err
				}
				hint := "本次工具查询失败，没有获得可用数据。可修正参数后重试；仍失败时明确说明数据缺口，不得把错误当作证据。"
				switch input.Name {
				case "query_metrics":
					hint += " PromQL 必须使用有效的指标名或至少一个不匹配空值的标签条件；禁止仅使用匹配空值的选择器，如 {__name__=~\".*\"}。"
				case "query_traces":
					hint += " 必须提供已知的非空 trace_id；可先查询日志获取，无法获取时说明不能查询链路，不要编造 ID。"
				}
				payload, _ := json.Marshal(map[string]any{
					"success": false,
					"tool":    input.Name,
					"error": map[string]string{
						"code":    "TOOL_QUERY_FAILED",
						"message": truncatePromptText(err.Error(), 500),
					},
					"hint": hint,
				})
				return &compose.ToolOutput{Result: string(payload)}, nil
			}
		},
	}
}
