package controller

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"shanyangcode.com/pilot/internal/httpapi"
	"shanyangcode.com/pilot/internal/observability"
	"shanyangcode.com/pilot/internal/observability/correlate"
	"shanyangcode.com/pilot/pkg/types"
)

// ObservabilityCollector 是可观测性控制器依赖的数据源查询能力。
type ObservabilityCollector interface {
	GetAlerts(ctx context.Context, filters map[string]string) ([]types.AlertInfo, error)
	QueryMetrics(ctx context.Context, query string, startTime, endTime time.Time) ([]types.MetricData, error)
	QueryLogs(ctx context.Context, service, level, keyword string, startTime, endTime time.Time, limit int) ([]types.LogEntry, error)
}

// ObservabilityController 可观测性数据控制器
type ObservabilityController struct {
	collector      ObservabilityCollector
	correlate      *correlate.Engine
	defaultService string
}

// NewObservabilityController 创建可观测性控制器
func NewObservabilityController(collector ObservabilityCollector, defaultService string) *ObservabilityController {
	if defaultService == "" {
		defaultService = "pilot-api"
	}
	return &ObservabilityController{
		collector:      collector,
		defaultService: defaultService,
	}
}

// WithCorrelate enables POST /observability/correlate.
func (c *ObservabilityController) WithCorrelate(engine *correlate.Engine) *ObservabilityController {
	c.correlate = engine
	return c
}

// CorrelateRequest is the cross-signal correlation request body.
type CorrelateRequest struct {
	Service         string `json:"service"`
	DurationMinutes int    `json:"duration_minutes"`
	MetricQuery     string `json:"metric_query"`
}

// Correlate runs the heuristic correlation engine.
func (c *ObservabilityController) Correlate(g *gin.Context) {
	if c.correlate == nil {
		httpapi.Fail(g, httpapi.FeatureDisabled("correlation 未启用"))
		return
	}
	var req CorrelateRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("请求体不是合法 JSON"))
		return
	}
	var err error
	req.Service, err = observability.NormalizeService(req.Service, c.defaultService)
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	req.DurationMinutes = observability.NormalizeDurationMinutes(req.DurationMinutes)
	if strings.TrimSpace(req.MetricQuery) == "" {
		req.MetricQuery = "up"
	}
	result, err := c.correlate.Correlate(g.Request.Context(), correlate.Request{
		Service:     req.Service,
		Window:      time.Duration(req.DurationMinutes) * time.Minute,
		MetricQuery: req.MetricQuery,
	})
	if err != nil {
		slog.ErrorContext(g.Request.Context(), "correlate failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("关联分析失败，请稍后重试"))
		return
	}
	httpapi.OK(g, "关联分析完成", result)
}

// AlertsRequest 查询告警请求
type AlertsRequest struct {
	Service  string `json:"service"`
	Severity string `json:"severity"`
}

// MetricsRequest 查询指标请求
type MetricsRequest struct {
	Query           string `json:"query"`
	DurationMinutes int    `json:"duration_minutes"`
}

// LogsRequest 查询日志请求
type LogsRequest struct {
	Service          string `json:"service"`
	Level            string `json:"level"`
	DurationMinutes  int    `json:"duration_minutes"`
	Limit            int    `json:"limit"`
	OutputRuneBudget int    `json:"output_rune_budget"`
}

// OverviewRequest 看板概览请求
type OverviewRequest struct {
	Service             string `json:"service"`
	MetricQuery         string `json:"metric_query"`
	DurationMinutes     int    `json:"duration_minutes"`
	LogLevel            string `json:"log_level"`
	LogLimit            int    `json:"log_limit"`
	LogOutputRuneBudget int    `json:"log_output_rune_budget"`
}

// GetAlerts 查询告警
func (c *ObservabilityController) GetAlerts(g *gin.Context) {
	var req AlertsRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("请求体不是合法 JSON"))
		return
	}

	var err error
	req.Service, err = observability.NormalizeService(req.Service, "")
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	req.Severity, err = observability.NormalizeSeverity(req.Severity)
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}

	filters := make(map[string]string)
	if req.Service != "" {
		filters["service"] = req.Service
	}
	if req.Severity != "" {
		filters["severity"] = req.Severity
	}

	alerts, err := c.collector.GetAlerts(g.Request.Context(), filters)
	if err != nil {
		slog.ErrorContext(g.Request.Context(), "query alerts failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("查询告警失败，请稍后重试"))
		return
	}

	if strings.TrimSpace(req.Severity) != "" {
		filtered := make([]types.AlertInfo, 0, len(alerts))
		for _, item := range alerts {
			if strings.EqualFold(item.Severity, req.Severity) {
				filtered = append(filtered, item)
			}
		}
		alerts = filtered
	}

	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].StartTime.After(alerts[j].StartTime)
	})

	httpapi.OK(g, "查询成功", alerts)
}

// GetMetrics 查询指标
func (c *ObservabilityController) GetMetrics(g *gin.Context) {
	var req MetricsRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("请求体不是合法 JSON"))
		return
	}

	var err error
	req.Query, err = observability.NormalizePromQL(req.Query)
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	req.DurationMinutes = observability.NormalizeDurationMinutes(req.DurationMinutes)

	end := time.Now()
	start := end.Add(-time.Duration(req.DurationMinutes) * time.Minute)

	metrics, err := c.collector.QueryMetrics(g.Request.Context(), req.Query, start, end)
	if err != nil {
		slog.ErrorContext(g.Request.Context(), "query metrics failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("查询指标失败，请稍后重试"))
		return
	}

	sort.Slice(metrics, func(i, j int) bool {
		return metrics[i].Timestamp.Before(metrics[j].Timestamp)
	})

	latest := 0.0
	avg := 0.0
	if len(metrics) > 0 {
		latest = metrics[len(metrics)-1].Value
		sum := 0.0
		for _, item := range metrics {
			sum += item.Value
		}
		avg = sum / float64(len(metrics))
	}

	httpapi.OK(g, "查询成功", gin.H{
		"items": metrics,
		"summary": gin.H{
			"query":          req.Query,
			"duration_min":   req.DurationMinutes,
			"point_count":    len(metrics),
			"latest_value":   latest,
			"avg_value":      avg,
			"window_start":   start,
			"window_end":     end,
			"collection_now": time.Now(),
		},
	})
}

// GetLogs 查询日志
func (c *ObservabilityController) GetLogs(g *gin.Context) {
	var req LogsRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("请求体不是合法 JSON"))
		return
	}

	var err error
	req.Service, err = observability.NormalizeService(req.Service, c.defaultService)
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	req.Level, err = observability.NormalizeLogLevel(req.Level, "error")
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	req.DurationMinutes = observability.NormalizeDurationMinutes(req.DurationMinutes)
	req.Limit = observability.NormalizeLimit(req.Limit)
	req.OutputRuneBudget = observability.NormalizeLogOutputRunes(req.OutputRuneBudget)

	end := time.Now()
	start := end.Add(-time.Duration(req.DurationMinutes) * time.Minute)

	logs, err := c.collector.QueryLogs(g.Request.Context(), req.Service, req.Level, "", start, end, req.Limit)
	if err != nil {
		slog.ErrorContext(g.Request.Context(), "query logs failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("查询日志失败，请稍后重试"))
		return
	}

	sort.Slice(logs, func(i, j int) bool {
		return logs[i].Timestamp.After(logs[j].Timestamp)
	})

	summarized := observability.SummarizeLogs(logs, req.OutputRuneBudget)
	httpapi.OK(g, "查询成功", gin.H{
		"items": summarized.Items,
		"summary": gin.H{
			"service":             req.Service,
			"level":               req.Level,
			"duration_min":        req.DurationMinutes,
			"limit":               req.Limit,
			"original_count":      summarized.Summary.OriginalCount,
			"returned_count":      summarized.Summary.ReturnedCount,
			"omitted_count":       summarized.Summary.OmittedCount,
			"truncated":           summarized.Summary.Truncated,
			"rune_budget":         summarized.Summary.RuneBudget,
			"runes_used":          summarized.Summary.RunesUsed,
			"level_counts":        summarized.Summary.LevelCounts,
			"service_counts":      summarized.Summary.ServiceCounts,
			"trace_counts":        summarized.Summary.TraceCounts,
			"representative_logs": summarized.Summary.RepresentativeLogs,
			"collection_now":      time.Now(),
		},
	})
}

// GetOverview 查询可观测性看板概览
func (c *ObservabilityController) GetOverview(g *gin.Context) {
	var req OverviewRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("请求体不是合法 JSON"))
		return
	}

	var err error
	req.Service, err = observability.NormalizeService(req.Service, c.defaultService)
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	if req.MetricQuery == "" {
		req.MetricQuery = "up"
	}
	req.MetricQuery, err = observability.NormalizePromQL(req.MetricQuery)
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	req.LogLevel, err = observability.NormalizeLogLevel(req.LogLevel, "error")
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	req.DurationMinutes = observability.NormalizeDurationMinutes(req.DurationMinutes)
	req.LogLimit = observability.NormalizeLimit(req.LogLimit)
	req.LogOutputRuneBudget = observability.NormalizeLogOutputRunes(req.LogOutputRuneBudget)

	end := time.Now()
	start := end.Add(-time.Duration(req.DurationMinutes) * time.Minute)
	reqCtx := g.Request.Context()

	alerts, err := c.collector.GetAlerts(reqCtx, map[string]string{"service": req.Service})
	if err != nil {
		slog.ErrorContext(reqCtx, "get alerts failed in overview", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("获取告警失败，请稍后重试"))
		return
	}

	metrics, err := c.collector.QueryMetrics(reqCtx, req.MetricQuery, start, end)
	if err != nil {
		slog.ErrorContext(reqCtx, "get metrics failed in overview", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("获取指标失败，请稍后重试"))
		return
	}

	logs, err := c.collector.QueryLogs(reqCtx, req.Service, req.LogLevel, "", start, end, req.LogLimit)
	if err != nil {
		slog.ErrorContext(reqCtx, "get logs failed in overview", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("获取日志失败，请稍后重试"))
		return
	}

	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].StartTime.After(alerts[j].StartTime)
	})
	sort.Slice(metrics, func(i, j int) bool {
		return metrics[i].Timestamp.Before(metrics[j].Timestamp)
	})
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].Timestamp.After(logs[j].Timestamp)
	})

	activeAlerts := 0
	for _, item := range alerts {
		if item.Status == "firing" {
			activeAlerts++
		}
	}

	latestMetric := 0.0
	if len(metrics) > 0 {
		latestMetric = metrics[len(metrics)-1].Value
	}

	summarizedLogs := observability.SummarizeLogs(logs, req.LogOutputRuneBudget)
	httpapi.OK(g, "查询成功", gin.H{
		"summary": gin.H{
			"service":          req.Service,
			"metric_query":     req.MetricQuery,
			"duration_min":     req.DurationMinutes,
			"alerts_total":     len(alerts),
			"alerts_active":    activeAlerts,
			"metrics_points":   len(metrics),
			"metrics_latest":   latestMetric,
			"logs_total":       summarizedLogs.Summary.OriginalCount,
			"logs_returned":    summarizedLogs.Summary.ReturnedCount,
			"logs_omitted":     summarizedLogs.Summary.OmittedCount,
			"logs_truncated":   summarizedLogs.Summary.Truncated,
			"collection_start": start,
			"collection_end":   end,
		},
		"alerts":      alerts,
		"metrics":     metrics,
		"logs":        summarizedLogs.Items,
		"log_summary": summarizedLogs.Summary,
	})
}
