package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"shanyangcode.com/pilot/pkg/types"
)

// PrometheusClient Prometheus 客户端
type PrometheusClient struct {
	baseURL string
	client  *http.Client
	tracer  trace.Tracer
}

// NewPrometheusClient 创建 Prometheus 客户端
func NewPrometheusClient(baseURL string, timeout time.Duration) *PrometheusClient {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return &PrometheusClient{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: timeout,
		},
		tracer: otel.Tracer("pilot/observability/prometheus"),
	}
}

// GetAlerts 获取告警。filters 支持 service、severity、status（对应 Prometheus alert state）。
func (pc *PrometheusClient) GetAlerts(ctx context.Context, filters map[string]string) ([]types.AlertInfo, error) {
	normalizedFilters, err := normalizeAlertFilters(filters)
	if err != nil {
		return nil, err
	}

	ctx, span := startClientSpan(ctx, pc.tracer, "prometheus.alerts", attribute.String("prometheus.url", pc.baseURL))
	defer span.End()

	alertsURL := fmt.Sprintf("%s/api/v1/alerts", pc.baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, alertsURL, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("create alerts request failed: %w", err)
	}

	resp, err := pc.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get alerts failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("read response failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		span.SetStatus(codes.Error, resp.Status)
		return nil, formatHTTPStatusError("get alerts failed", resp.StatusCode, body)
	}

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Alerts []struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"`
				ActiveAt    time.Time         `json:"activeAt"`
			} `json:"alerts"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("unmarshal response failed: %w", err)
	}
	if result.Status != "success" {
		span.SetStatus(codes.Error, result.Status)
		return nil, fmt.Errorf("get alerts failed: status=%s", result.Status)
	}

	alerts := make([]types.AlertInfo, 0)
	for _, alert := range result.Data.Alerts {
		if !matchAlertFilters(alert.Labels, alert.State, normalizedFilters) {
			continue
		}

		alertInfo := types.AlertInfo{
			ID:          alert.Labels["alertname"],
			Name:        alert.Labels["alertname"],
			Severity:    alert.Labels["severity"],
			Status:      alert.State,
			Description: alert.Annotations["description"],
			Labels:      alert.Labels,
			Annotations: alert.Annotations,
			StartTime:   alert.ActiveAt,
		}
		alerts = append(alerts, alertInfo)
	}

	return alerts, nil
}

// QueryRange 范围查询
func (pc *PrometheusClient) QueryRange(ctx context.Context, query string, startTime, endTime time.Time) ([]types.MetricData, error) {
	query, err := NormalizePromQL(query)
	if err != nil {
		return nil, err
	}
	startTime, endTime = NormalizeTimeRange(startTime, endTime)

	ctx, span := startClientSpan(
		ctx,
		pc.tracer,
		"prometheus.query_range",
		attribute.String("prometheus.query", query),
	)
	defer span.End()

	queryURL := fmt.Sprintf("%s/api/v1/query_range", pc.baseURL)

	params := url.Values{}
	params.Add("query", query)
	params.Add("start", fmt.Sprintf("%d", startTime.Unix()))
	params.Add("end", fmt.Sprintf("%d", endTime.Unix()))
	params.Add("step", "60s")

	fullURL := fmt.Sprintf("%s?%s", queryURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("create query range request failed: %w", err)
	}

	resp, err := pc.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("query range failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("read response failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		span.SetStatus(codes.Error, resp.Status)
		return nil, formatHTTPStatusError("query range failed", resp.StatusCode, body)
	}

	var result struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]interface{}   `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("unmarshal response failed: %w", err)
	}
	if result.Status != "success" {
		span.SetStatus(codes.Error, result.Status)
		return nil, fmt.Errorf("query range failed: status=%s", result.Status)
	}

	metrics := make([]types.MetricData, 0)
	for _, r := range result.Data.Result {
		for _, value := range r.Values {
			timestamp, val, ok := parseMetricValuePoint(value)
			if !ok {
				continue
			}

			metrics = append(metrics, types.MetricData{
				Name:      query,
				Value:     val,
				Timestamp: timestamp,
				Labels:    r.Metric,
			})
		}
	}

	return metrics, nil
}

func normalizeAlertFilters(filters map[string]string) (map[string]string, error) {
	normalizedFilters := make(map[string]string, len(filters))
	for key, value := range filters {
		switch key {
		case "service":
			service, err := NormalizeService(value, "")
			if err != nil {
				return nil, err
			}
			if service != "" {
				normalizedFilters[key] = service
			}
		case "severity":
			severity, err := NormalizeSeverity(value)
			if err != nil {
				return nil, err
			}
			if severity != "" {
				normalizedFilters[key] = severity
			}
		case "status":
			status := strings.ToLower(strings.TrimSpace(value))
			if status != "" {
				normalizedFilters[key] = status
			}
		}
	}
	return normalizedFilters, nil
}

// matchAlertFilters 要求 labels 命中全部非 status 条件，且 state 命中可选 status。
func matchAlertFilters(labels map[string]string, state string, filters map[string]string) bool {
	for key, value := range filters {
		if key == "status" {
			if value != "" && !strings.EqualFold(state, value) {
				return false
			}
			continue
		}
		if labels[key] != value {
			return false
		}
	}
	return true
}
