package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"shanyangcode.com/pilot/internal/observability"
	"shanyangcode.com/pilot/internal/observability/correlate"
)

// CorrelateSignalsInput is the read-only correlation tool input.
type CorrelateSignalsInput struct {
	Service         string `json:"service" jsonschema:"description=服务名称，可选"`
	DurationMinutes int    `json:"duration_minutes" jsonschema:"description=回看分钟数，默认 30，最大受 HITL 策略约束"`
	MetricQuery     string `json:"metric_query" jsonschema:"description=可选 PromQL，默认 up"`
}

// NewCorrelateSignalsTool creates the correlate_signals InvokableTool.
func NewCorrelateSignalsTool(engine *correlate.Engine) (tool.InvokableTool, error) {
	if err := requireSource("correlation engine", engine); err != nil {
		return nil, err
	}
	return utils.InferOptionableTool(
		"correlate_signals",
		"只读跨信号关联：对告警、错误日志与指标做时间桶共现打分；静态依赖图只能为已经有运行信号的依赖节点增加弱先验，不生成未观测根因。输出可引用的 corr-* 证据。这是可解释启发式，不是因果模型。",
		func(ctx context.Context, input *CorrelateSignalsInput, opts ...tool.Option) (string, error) {
			service := ""
			window := 30 * time.Minute
			metricQuery := "up"
			if input != nil {
				normalized, err := observability.NormalizeService(input.Service, "")
				if err != nil {
					return "", err
				}
				service = normalized
				if input.DurationMinutes > 0 {
					window = time.Duration(input.DurationMinutes) * time.Minute
				}
				if strings.TrimSpace(input.MetricQuery) != "" {
					metricQuery = strings.TrimSpace(input.MetricQuery)
				}
			}
			result, err := engine.Correlate(ctx, correlate.Request{
				Service:     service,
				Window:      window,
				MetricQuery: metricQuery,
			})
			if err != nil {
				return "", err
			}
			if len(result.Hypotheses) == 0 {
				return "关联结果（Real）: 窗口内未形成多信号共现假设", nil
			}
			var b strings.Builder
			b.WriteString("关联结果（Real / heuristic）:\n")
			for i, h := range result.Hypotheses {
				b.WriteString(fmt.Sprintf("\n假设 %d [%s] service=%s score=%.2f signals=%s\n  %s\n  support=%v\n",
					i+1, h.ID, h.Service, h.Score, strings.Join(h.SignalKinds, "+"), h.Summary, h.SupportIDs))
			}
			return b.String(), nil
		},
	)
}
