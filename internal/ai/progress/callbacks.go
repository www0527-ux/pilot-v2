package progress

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	callbackutils "github.com/cloudwego/eino/utils/callbacks"
)

type modelKey struct{}
type toolKey struct{}

func ModelCallbacks() *callbackutils.ModelCallbackHandler {
	return &callbackutils.ModelCallbackHandler{
		OnStart: func(ctx context.Context, _ *callbacks.RunInfo, input *model.CallbackInput) context.Context {
			NextRound(ctx)
			name := "模型"
			if input != nil && input.Config != nil && input.Config.Model != "" {
				name = input.Config.Model
			}
			return context.WithValue(ctx, modelKey{}, begin(ctx, "model", name, "", nil))
		},
		OnEnd: func(ctx context.Context, _ *callbacks.RunInfo, _ *model.CallbackOutput) context.Context {
			FinishModel(ctx, "completed", "模型响应完成")
			return ctx
		},
		OnError: func(ctx context.Context, _ *callbacks.RunInfo, _ error) context.Context {
			FinishModel(ctx, "failed", "模型调用失败")
			return ctx
		},
	}
}
func FinishModel(ctx context.Context, status, summary string) {
	state, _ := ctx.Value(modelKey{}).(*action)
	state.finish(ctx, status, summary, "")
}
func ToolCallbacks() *callbackutils.ToolCallbackHandler {
	return &callbackutils.ToolCallbackHandler{
		OnStart: func(ctx context.Context, info *callbacks.RunInfo, input *tool.CallbackInput) context.Context {
			name := "tool"
			if info != nil && info.Name != "" {
				name = info.Name
			}
			parameters := map[string]any{}
			if input != nil {
				parameters = SafeParameters(input.ArgumentsInJSON)
			}
			id := compose.GetToolCallID(ctx)
			if id != "" {
				id = "tool:" + id
			}
			return context.WithValue(ctx, toolKey{}, begin(ctx, "tool", name, id, parameters))
		},
		OnEnd: func(ctx context.Context, _ *callbacks.RunInfo, output *tool.CallbackOutput) context.Context {
			state, _ := ctx.Value(toolKey{}).(*action)
			status, summary, preview := "completed", "查询完成", ""
			var result any
			if output != nil {
				status, summary, preview = ToolResult(output.Response)
				if json.Unmarshal([]byte(output.Response), &result) != nil {
					result = output.Response
				}
			}
			state.finish(ctx, status, summary, preview, result)
			return ctx
		},
		OnError: func(ctx context.Context, _ *callbacks.RunInfo, err error) context.Context {
			state, _ := ctx.Value(toolKey{}).(*action)
			state.finish(ctx, "failed", Redact(err.Error()), "")
			return ctx
		},
	}
}
func CallbackHandler() callbacks.Handler {
	return callbackutils.NewHandlerHelper().ChatModel(ModelCallbacks()).Tool(ToolCallbacks()).Handler()
}

// Only query parameters intended for display are published, never model input.
func SafeParameters(raw string) map[string]any {
	var input map[string]any
	if json.Unmarshal([]byte(raw), &input) != nil {
		return nil
	}
	output := map[string]any{}
	for _, key := range []string{"service", "query", "trace_id", "duration_minutes", "level", "keyword", "limit", "status", "severity", "format", "metric_name", "start_time", "end_time"} {
		if value, ok := input[key]; ok {
			switch typed := value.(type) {
			case string:
				value = Clip(Redact(typed), 400)
			case float64, bool, nil:
			default:
				// Tool query fields are scalar; malformed objects must not
				// smuggle credentials through an otherwise allowed key.
				continue
			}
			output[key] = value
		}
	}
	return output
}

var credentialPattern = regexp.MustCompile(`(?i)(bearer\s+\S+|sk-[a-z0-9_-]{8,}|(?:api[_-]?key|password|token|secret|authorization)\s*[=:]\s*[^\s,;]+)`)

func Redact(text string) string { return credentialPattern.ReplaceAllString(text, "[已隐藏]") }
func scrub(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			lower := strings.ToLower(key)
			compact := strings.NewReplacer("_", "", "-", "").Replace(lower)
			if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(compact, "apikey") || lower == "authorization" || lower == "cookie" {
				v[key] = "[已隐藏]"
			} else {
				v[key] = scrub(item)
			}
		}
	case []any:
		for i := range v {
			v[i] = scrub(v[i])
		}
	case string:
		return Redact(v)
	}
	return value
}

func PublicValue(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return nil
	}
	return scrub(decoded)
}
func ToolResult(raw string) (status, summary, preview string) {
	status, summary = "completed", "查询完成"
	var output map[string]any
	if json.Unmarshal([]byte(raw), &output) != nil {
		return status, summary, Clip(Redact(raw), 2400)
	}
	if success, ok := output["success"].(bool); ok && !success {
		status, summary = "failed", "查询未成功"
	}
	if count, ok := output["count"].(float64); ok && status == "completed" {
		summary = fmt.Sprintf("查询完成 · %.0f 条结果", count)
	}
	if message, ok := output["message"].(string); ok && status == "failed" {
		summary = message
	}
	if failure, ok := output["error"].(map[string]any); ok && status == "failed" {
		if message, ok := failure["message"].(string); ok {
			summary = message
		}
	}
	cleaned, _ := json.MarshalIndent(scrub(output), "", "  ")
	return status, Clip(Redact(summary), 300), Clip(string(cleaned), 2400)
}
