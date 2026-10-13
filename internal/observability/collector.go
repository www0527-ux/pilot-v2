package observability

import (
	"context"
	"fmt"
	"strings"
	"time"

	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/pkg/types"
)

// DataCollector 可观测性数据收集器
type DataCollector struct {
	prometheusClient *PrometheusClient
	logClient        *LogClient
	traceClient      *TraceClient
	topology         TopologySources
}

// TopologySources 描述当前部署真实依赖，用于生成拓扑文本。
type TopologySources struct {
	ServiceName   string
	RedisAddr     string
	MySQLHost     string
	MySQLPort     int
	ESAddresses   []string
	PrometheusURL string
	TraceQueryURL string
	LLMBaseURL    string
}

// NewTopologySources 从配置提取真实依赖端点。
func NewTopologySources(cfg *config.Config) TopologySources {
	return TopologySources{
		ServiceName:   cfg.Observability.ServiceName,
		RedisAddr:     cfg.Redis.Addr,
		MySQLHost:     cfg.Database.Host,
		MySQLPort:     cfg.Database.Port,
		ESAddresses:   cfg.Elasticsearch.Addresses,
		PrometheusURL: cfg.Observability.Prometheus.URL,
		TraceQueryURL: cfg.Observability.Trace.URL,
		LLMBaseURL:    cfg.LLM.MainModel.BaseURL,
	}
}

// NewDataCollector 创建数据收集器；全部数据源客户端由调用方注入。
func NewDataCollector(prom *PrometheusClient, logClient *LogClient, traceClient *TraceClient, topology TopologySources) *DataCollector {
	return &DataCollector{
		prometheusClient: prom,
		logClient:        logClient,
		traceClient:      traceClient,
		topology:         topology,
	}
}

// GetAlerts 获取告警信息
func (dc *DataCollector) GetAlerts(ctx context.Context, filters map[string]string) ([]types.AlertInfo, error) {
	return dc.prometheusClient.GetAlerts(ctx, filters)
}

// QueryMetrics 查询指标数据
func (dc *DataCollector) QueryMetrics(ctx context.Context, query string, startTime, endTime time.Time) ([]types.MetricData, error) {
	return dc.prometheusClient.QueryRange(ctx, query, startTime, endTime)
}

// QueryLogs 查询日志
func (dc *DataCollector) QueryLogs(ctx context.Context, service, level, keyword string, startTime, endTime time.Time, limit int) ([]types.LogEntry, error) {
	return dc.logClient.QueryLogs(ctx, service, level, keyword, startTime, endTime, limit)
}

// GetTrace 获取链路追踪
func (dc *DataCollector) GetTrace(ctx context.Context, traceID string) ([]types.TraceSpan, error) {
	return dc.traceClient.GetTrace(ctx, traceID)
}

// GetServiceTopology 根据当前配置生成真实依赖拓扑文本。
func (dc *DataCollector) GetServiceTopology(ctx context.Context, service string) (string, error) {
	service = strings.TrimSpace(service)
	if service == "" {
		service = dc.topology.ServiceName
	}

	lines := []string{
		fmt.Sprintf("服务拓扑（Real）:\n- service: %s", service),
		"  - downstream:",
	}

	if strings.TrimSpace(dc.topology.RedisAddr) != "" {
		lines = append(lines, fmt.Sprintf("    * %s -> redis (%s)", service, dc.topology.RedisAddr))
	}
	if strings.TrimSpace(dc.topology.MySQLHost) != "" {
		lines = append(lines, fmt.Sprintf("    * %s -> mysql (%s:%d)", service, dc.topology.MySQLHost, dc.topology.MySQLPort))
	}
	for _, address := range dc.topology.ESAddresses {
		if strings.TrimSpace(address) == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("    * %s -> elasticsearch (%s)", service, address))
	}
	if strings.TrimSpace(dc.topology.PrometheusURL) != "" {
		lines = append(lines, fmt.Sprintf("    * %s -> prometheus (%s)", service, dc.topology.PrometheusURL))
	}
	if strings.TrimSpace(dc.topology.TraceQueryURL) != "" {
		lines = append(lines, fmt.Sprintf("    * %s -> jaeger-query (%s)", service, dc.topology.TraceQueryURL))
	}
	if strings.TrimSpace(dc.topology.LLMBaseURL) != "" {
		lines = append(lines, fmt.Sprintf("    * %s -> llm-gateway (%s)", service, dc.topology.LLMBaseURL))
	}

	return strings.Join(lines, "\n"), nil
}

// DependencyNeighbors returns static dependency labels for correlation hop spread.
func (dc *DataCollector) DependencyNeighbors(service string) []string {
	if dc == nil {
		return nil
	}
	return dc.topology.DependencyNeighbors(service)
}

// DependencyNeighbors lists configured downstream dependency kinds for a service.
func (t TopologySources) DependencyNeighbors(service string) []string {
	_ = service
	out := make([]string, 0, 6)
	if strings.TrimSpace(t.RedisAddr) != "" {
		out = append(out, "redis")
	}
	if strings.TrimSpace(t.MySQLHost) != "" {
		out = append(out, "mysql")
	}
	if len(t.ESAddresses) > 0 {
		out = append(out, "elasticsearch")
	}
	if strings.TrimSpace(t.PrometheusURL) != "" {
		out = append(out, "prometheus")
	}
	if strings.TrimSpace(t.TraceQueryURL) != "" {
		out = append(out, "jaeger")
	}
	return out
}
