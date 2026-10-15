package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"shanyangcode.com/pilot/internal/observability"
)

// QueryAlertsInput 查询告警输入
type QueryAlertsInput struct {
	Service  string `json:"service" jsonschema:"description=服务名称，可选；为空时查询所有服务"`
	Status   string `json:"status" jsonschema:"description=告警状态：firing、resolved、all"`
	Severity string `json:"severity" jsonschema:"description=告警级别：critical、warning、info"`
}

// QueryAlertsOutput 查询告警输出
type QueryAlertsOutput struct {
	Success bool   `json:"success"`
	Count   int    `json:"count"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

// NewQueryAlertsTool 创建查询告警工具。
func NewQueryAlertsTool(source AlertsSource) (tool.InvokableTool, error) {
	if err := requireSource("alerts source", source); err != nil {
		return nil, err
	}

	t, err := utils.InferOptionableTool(
		"query_alerts",
		"查询告警信息，获取当前活跃的告警或历史告警",
		func(ctx context.Context, input *QueryAlertsInput, opts ...tool.Option) (string, error) {
			filters := make(map[string]string)
			if input != nil {
				service, err := observability.NormalizeService(input.Service, "")
				if err != nil {
					return "", err
				}
				if service != "" {
					filters["service"] = service
				}
				if strings.TrimSpace(input.Severity) != "" {
					filters["severity"] = strings.ToLower(strings.TrimSpace(input.Severity))
				}
				if strings.TrimSpace(input.Status) != "" && !strings.EqualFold(input.Status, "all") {
					filters["status"] = strings.ToLower(strings.TrimSpace(input.Status))
				}
			}

			alerts, err := source.GetAlerts(ctx, filters)
			if err != nil {
				return "", err
			}
			sort.Slice(alerts, func(i, j int) bool {
				return alerts[i].StartTime.After(alerts[j].StartTime)
			})

			result := "告警查询结果（Real）:\n"
			if len(alerts) == 0 {
				result += "未匹配到告警"
			}
			for i, alert := range alerts {
				result += fmt.Sprintf(
					"\n告警 %d:\n  名称: %s\n  级别: %s\n  状态: %s\n  时间: %s\n  描述: %s\n",
					i+1,
					alert.Name,
					alert.Severity,
					alert.Status,
					alert.StartTime.Format("2006-01-02 15:04:05"),
					alert.Description,
				)
			}

			output := QueryAlertsOutput{
				Success: true,
				Count:   len(alerts),
				Result:  result,
				Message: "查询完成",
			}
			jsonBytes, _ := json.MarshalIndent(output, "", "  ")
			return string(jsonBytes), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create query alerts tool: %w", err)
	}

	return t, nil
}

// QueryMetricsInput 查询指标输入
type QueryMetricsInput struct {
	Query           string `json:"query" jsonschema:"description=PromQL查询语句,required"`
	DurationMinutes int    `json:"duration_minutes" jsonschema:"description=查询时间窗分钟数，默认30，范围1-1440"`
}

// QueryMetricsOutput 查询指标输出
type QueryMetricsOutput struct {
	Success bool   `json:"success"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

// NewQueryMetricsTool 创建查询指标工具。
func NewQueryMetricsTool(source MetricsSource) (tool.InvokableTool, error) {
	if err := requireSource("metrics source", source); err != nil {
		return nil, err
	}

	t, err := utils.InferOptionableTool(
		"query_metrics",
		"查询 Prometheus 指标数据，支持 PromQL 查询",
		func(ctx context.Context, input *QueryMetricsInput, opts ...tool.Option) (string, error) {
			query := "up"
			durationMinutes := 30
			if input != nil {
				if strings.TrimSpace(input.Query) != "" {
					query = strings.TrimSpace(input.Query)
				}
				durationMinutes = normalizeDurationMinutes(input.DurationMinutes, 30)
			}
			var err error
			query, err = observability.NormalizePromQL(query)
			if err != nil {
				return "", err
			}

			end := time.Now()
			start := end.Add(-time.Duration(durationMinutes) * time.Minute)
			metrics, err := source.QueryMetrics(ctx, query, start, end)
			if err != nil {
				return "", err
			}
			sort.Slice(metrics, func(i, j int) bool {
				return metrics[i].Timestamp.Before(metrics[j].Timestamp)
			})

			latest := 0.0
			avg := 0.0
			if len(metrics) > 0 {
				latest = metrics[len(metrics)-1].Value
				for _, item := range metrics {
					avg += item.Value
				}
				avg /= float64(len(metrics))
			}

			startIdx := len(metrics) - 8
			if startIdx < 0 {
				startIdx = 0
			}
			sample := ""
			for i := startIdx; i < len(metrics); i++ {
				sample += fmt.Sprintf("  - %s => %.4f\n", metrics[i].Timestamp.Format("15:04:05"), metrics[i].Value)
			}

			output := QueryMetricsOutput{
				Success: true,
				Result: fmt.Sprintf(
					"指标查询结果（Real）:\nquery: %s\n窗口: 最近%d分钟\n数据点: %d\n最新值: %.4f\n平均值: %.4f\n最近样本:\n%s",
					query,
					durationMinutes,
					len(metrics),
					latest,
					avg,
					sample,
				),
				Message: "查询完成",
			}
			jsonBytes, _ := json.MarshalIndent(output, "", "  ")
			return string(jsonBytes), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create query metrics tool: %w", err)
	}

	return t, nil
}

// QueryLogsInput 查询日志输入
type QueryLogsInput struct {
	Service          string `json:"service" jsonschema:"description=服务名称，可选；为空时查询所有服务"`
	Query            string `json:"query" jsonschema:"description=日志查询关键词"`
	Level            string `json:"level" jsonschema:"description=日志级别：all/全部、debug、info、warn（warning 别名）、error、fatal"`
	DurationMinutes  int    `json:"duration_minutes" jsonschema:"description=查询时间窗分钟数，默认30，范围1-1440"`
	Limit            int    `json:"limit" jsonschema:"description=最多采集日志条数，默认20，范围1-500"`
	OutputRuneBudget int    `json:"output_rune_budget" jsonschema:"description=返回日志正文的 rune 预算，默认12000，范围256-100000"`
}

// QueryLogsOutput 查询日志输出
type QueryLogsOutput struct {
	Success bool                         `json:"success"`
	Result  observability.SummarizedLogs `json:"result"`
	Message string                       `json:"message"`
}

// NewQueryLogsTool 创建查询日志工具。
func NewQueryLogsTool(source LogsSource) (tool.InvokableTool, error) {
	if err := requireSource("logs source", source); err != nil {
		return nil, err
	}

	t, err := utils.InferOptionableTool(
		"query_logs",
		"查询日志，支持关键词搜索和过滤",
		func(ctx context.Context, input *QueryLogsInput, opts ...tool.Option) (string, error) {
			service := ""
			level := ""
			keyword := ""
			durationMinutes := 30
			limit := 20
			//返回日志文本的字符预算
			outputRuneBudget := observability.DefaultLogOutputRunes
			if input != nil {
				var err error
				service, err = observability.NormalizeService(input.Service, "")
				if err != nil {
					return "", err
				}
				level, err = observability.NormalizeLogLevel(input.Level, "")
				if err != nil {
					return "", err
				}
				keyword, err = observability.NormalizeLogKeyword(input.Query)
				if err != nil {
					return "", err
				}
				durationMinutes = normalizeDurationMinutes(input.DurationMinutes, 30)
				if input.Limit > 0 {
					limit = observability.NormalizeLimit(input.Limit)
				}
				outputRuneBudget = observability.NormalizeLogOutputRunes(input.OutputRuneBudget)
			}

			end := time.Now()
			start := end.Add(-time.Duration(durationMinutes) * time.Minute)
			logs, err := source.QueryLogs(ctx, service, level, keyword, start, end, limit)
			if err != nil {
				return "", err
			}

			output := QueryLogsOutput{
				Success: true,
				Result:  observability.SummarizeLogs(logs, outputRuneBudget),
				Message: "查询完成；result.summary 明确给出原始/返回/省略数量、截断状态和聚合，items 仅包含预算内日志",
			}
			jsonBytes, _ := json.MarshalIndent(output, "", "  ")
			return string(jsonBytes), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create query logs tool: %w", err)
	}

	return t, nil
}

// QueryTracesInput 查询链路输入
type QueryTracesInput struct {
	Service string `json:"service" jsonschema:"description=服务名称"`
	TraceID string `json:"trace_id" jsonschema:"required,description=必须提供已知的非空 Trace ID，可从日志获取，不可编造"`
}

// QueryTracesOutput 查询链路输出
type QueryTracesOutput struct {
	Success bool   `json:"success"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

// NewQueryTracesTool 创建查询链路工具。
func NewQueryTracesTool(source TraceSource) (tool.InvokableTool, error) {
	if err := requireSource("trace source", source); err != nil {
		return nil, err
	}

	t, err := utils.InferOptionableTool(
		"query_traces",
		"根据已知且非空的 trace_id 查询分布式链路追踪数据；没有 trace_id 时先查询日志，不能仅凭服务名查询",
		func(ctx context.Context, input *QueryTracesInput, opts ...tool.Option) (string, error) {
			traceID := ""
			if input != nil {
				var err error
				traceID, err = observability.NormalizeTraceID(input.TraceID)
				if err != nil {
					return "", err
				}
			}

			traces, err := source.GetTrace(ctx, traceID)
			if err != nil {
				return "", err
			}

			result := "链路查询结果（Real）:\n"
			if len(traces) == 0 {
				result += "未查询到链路数据"
			}
			for i, span := range traces {
				result += fmt.Sprintf(
					"\nSpan %d:\n  TraceID: %s\n  SpanID: %s\n  ParentID: %s\n  服务: %s\n  操作: %s\n  耗时: %dμs\n  时间: %s\n",
					i+1,
					span.TraceID,
					span.SpanID,
					span.ParentID,
					span.Service,
					span.Operation,
					span.Duration,
					span.StartTime.Format("2006-01-02 15:04:05"),
				)
			}

			output := QueryTracesOutput{
				Success: true,
				Result:  result,
				Message: "查询完成",
			}
			jsonBytes, _ := json.MarshalIndent(output, "", "  ")
			return string(jsonBytes), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create query traces tool: %w", err)
	}

	return t, nil
}

// AnalyzeAnomalyInput 异常分析输入
type AnalyzeAnomalyInput struct {
	Metric          string `json:"metric" jsonschema:"description=要分析的指标名称,required"`
	DurationMinutes int    `json:"duration_minutes" jsonschema:"description=分析时间窗分钟数，默认30，范围1-1440"`
}

// AnalyzeAnomalyOutput 异常分析输出
type AnalyzeAnomalyOutput struct {
	Success bool   `json:"success"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

// NewAnalyzeAnomalyTool 创建异常分析工具。
func NewAnalyzeAnomalyTool(source MetricsSource) (tool.InvokableTool, error) {
	if err := requireSource("metrics source", source); err != nil {
		return nil, err
	}

	t, err := utils.InferOptionableTool(
		"analyze_anomaly",
		"基于固定阈值的启发式异常扫描（非 ML 检测），识别指标窗口内超阈值点",
		func(ctx context.Context, input *AnalyzeAnomalyInput, opts ...tool.Option) (string, error) {
			metric := "pilot_http_request_duration_seconds"
			durationMinutes := 30
			if input != nil {
				if strings.TrimSpace(input.Metric) != "" {
					metric = strings.TrimSpace(input.Metric)
				}
				durationMinutes = normalizeDurationMinutes(input.DurationMinutes, 30)
			}
			var err error
			metric, err = observability.NormalizePromQL(metric)
			if err != nil {
				return "", err
			}

			end := time.Now()
			start := end.Add(-time.Duration(durationMinutes) * time.Minute)
			metrics, err := source.QueryMetrics(ctx, metric, start, end)
			if err != nil {
				return "", err
			}

			threshold := anomalyThreshold(metric)
			anomalies := make([]string, 0)
			for _, m := range metrics {
				if m.Value > threshold {
					anomalies = append(anomalies, fmt.Sprintf("[%s] %.4f > %.4f", m.Timestamp.Format("15:04:05"), m.Value, threshold))
				}
			}

			result := ""
			if len(anomalies) == 0 {
				result = fmt.Sprintf("异常分析结果（启发式阈值，非ML）:\nmetric: %s\n窗口: %d分钟\n未检测到超过阈值 %.4f 的异常点", metric, durationMinutes, threshold)
			} else {
				result = fmt.Sprintf("异常分析结果（启发式阈值，非ML）:\nmetric: %s\n窗口: %d分钟\n阈值: %.4f\n异常点数量: %d\n%s", metric, durationMinutes, threshold, len(anomalies), strings.Join(anomalies, "\n"))
			}

			output := AnalyzeAnomalyOutput{
				Success: true,
				Result:  result,
				Message: "分析完成",
			}
			jsonBytes, _ := json.MarshalIndent(output, "", "  ")
			return string(jsonBytes), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create analyze anomaly tool: %w", err)
	}

	return t, nil
}

// GetServiceTopologyInput 获取服务拓扑输入
type GetServiceTopologyInput struct {
	Service string `json:"service" jsonschema:"description=服务名称"`
}

// GetServiceTopologyOutput 获取服务拓扑输出
type GetServiceTopologyOutput struct {
	Success bool   `json:"success"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

// NewGetServiceTopologyTool 创建获取服务拓扑工具。
func NewGetServiceTopologyTool(source TopologySource) (tool.InvokableTool, error) {
	if err := requireSource("topology source", source); err != nil {
		return nil, err
	}

	t, err := utils.InferOptionableTool(
		"get_service_topology",
		"获取配置依赖图（由本地配置拼装的 Redis/MySQL/ES/Prometheus/Jaeger/LLM 依赖，不是服务网格发现拓扑）",
		func(ctx context.Context, input *GetServiceTopologyInput, opts ...tool.Option) (string, error) {
			service := ""
			if input != nil {
				var err error
				service, err = observability.NormalizeService(input.Service, "")
				if err != nil {
					return "", err
				}
			}

			topology, err := source.GetServiceTopology(ctx, service)
			if err != nil {
				return "", err
			}

			output := GetServiceTopologyOutput{
				Success: true,
				Result:  topology,
				Message: "查询完成",
			}
			jsonBytes, _ := json.MarshalIndent(output, "", "  ")
			return string(jsonBytes), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create get service topology tool: %w", err)
	}

	return t, nil
}

func anomalyThreshold(metric string) float64 {
	lowerMetric := strings.ToLower(metric)
	switch {
	case strings.Contains(lowerMetric, "error"), strings.Contains(lowerMetric, "5xx"):
		return 0.01
	case strings.Contains(lowerMetric, "latency"), strings.Contains(lowerMetric, "duration"):
		return 1.0
	default:
		return 0.8
	}
}
