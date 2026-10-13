package tools

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"shanyangcode.com/pilot/pkg/types"
)

// AlertsSource provides the alert data needed by query_alerts.
type AlertsSource interface {
	GetAlerts(ctx context.Context, filters map[string]string) ([]types.AlertInfo, error)
}

// MetricsSource provides the metric data needed by metric tools.
type MetricsSource interface {
	QueryMetrics(ctx context.Context, query string, start, end time.Time) ([]types.MetricData, error)
}

// LogsSource provides the log data needed by query_logs.
type LogsSource interface {
	QueryLogs(ctx context.Context, service, level, keyword string, start, end time.Time, limit int) ([]types.LogEntry, error)
}

// TraceSource provides the trace data needed by query_traces.
type TraceSource interface {
	GetTrace(ctx context.Context, traceID string) ([]types.TraceSpan, error)
}

// TopologySource provides the topology data needed by get_service_topology.
type TopologySource interface {
	GetServiceTopology(ctx context.Context, service string) (string, error)
}

// KnowledgeSource provides the documents needed by search_knowledge.
type KnowledgeSource interface {
	Search(ctx context.Context, query string) ([]types.KnowledgeDocument, error)
}

func requireSource(name string, source any) error {
	if source == nil {
		return fmt.Errorf("%s is required", name)
	}

	value := reflect.ValueOf(source)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}
