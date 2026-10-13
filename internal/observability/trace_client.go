package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"shanyangcode.com/pilot/pkg/types"
)

// TraceClient 链路追踪客户端
type TraceClient struct {
	baseURL string
	client  *http.Client
	tracer  trace.Tracer
}

// NewTraceClient 创建链路追踪客户端
func NewTraceClient(baseURL string) *TraceClient {
	return &TraceClient{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
		tracer:  otel.Tracer("pilot/observability/traces"),
	}
}

// GetTrace 获取链路追踪
func (tc *TraceClient) GetTrace(ctx context.Context, traceID string) ([]types.TraceSpan, error) {
	traceID, normalizeErr := NormalizeTraceID(traceID)
	ctx, span := startClientSpan(ctx, tc.tracer, "jaeger.get_trace", attribute.String("trace.id", traceID))
	defer span.End()

	if normalizeErr != nil {
		span.RecordError(normalizeErr)
		span.SetStatus(codes.Error, normalizeErr.Error())
		return nil, normalizeErr
	}

	traceURL := fmt.Sprintf("%s/api/traces/%s", tc.baseURL, traceID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, traceURL, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("create trace request failed: %w", err)
	}

	resp, err := tc.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("get trace failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("read trace response failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		span.SetStatus(codes.Error, resp.Status)
		return nil, formatHTTPStatusError("get trace failed", resp.StatusCode, body)
	}

	var result struct {
		Data []struct {
			Spans []struct {
				TraceID       string `json:"traceID"`
				SpanID        string `json:"spanID"`
				OperationName string `json:"operationName"`
				StartTime     int64  `json:"startTime"`
				Duration      int64  `json:"duration"`
				ProcessID     string `json:"processID"`
				References    []struct {
					RefType string `json:"refType"`
					SpanID  string `json:"spanID"`
				} `json:"references"`
				Tags []struct {
					Key   string      `json:"key"`
					Value interface{} `json:"value"`
				} `json:"tags"`
				Logs []struct {
					Timestamp int64 `json:"timestamp"`
					Fields    []struct {
						Key   string      `json:"key"`
						Value interface{} `json:"value"`
					} `json:"fields"`
				} `json:"logs"`
			} `json:"spans"`
			Processes map[string]struct {
				ServiceName string `json:"serviceName"`
			} `json:"processes"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("decode trace response failed: %w", err)
	}

	spans := make([]types.TraceSpan, 0)
	for _, traceData := range result.Data {
		for _, rawSpan := range traceData.Spans {
			parentID := ""
			for _, ref := range rawSpan.References {
				if strings.EqualFold(ref.RefType, "CHILD_OF") || parentID == "" {
					parentID = ref.SpanID
				}
			}

			serviceName := rawSpan.ProcessID
			if process, ok := traceData.Processes[rawSpan.ProcessID]; ok && process.ServiceName != "" {
				serviceName = process.ServiceName
			}

			tags := make(map[string]string, len(rawSpan.Tags))
			for _, tag := range rawSpan.Tags {
				tags[tag.Key] = toString(tag.Value)
			}

			logs := make([]types.TraceLog, 0, len(rawSpan.Logs))
			for _, rawLog := range rawSpan.Logs {
				fields := make(map[string]string, len(rawLog.Fields))
				for _, field := range rawLog.Fields {
					fields[field.Key] = toString(field.Value)
				}
				logs = append(logs, types.TraceLog{
					Timestamp: parseUnixMicroOrMilli(rawLog.Timestamp),
					Fields:    fields,
				})
			}

			spans = append(spans, types.TraceSpan{
				TraceID:   rawSpan.TraceID,
				SpanID:    rawSpan.SpanID,
				ParentID:  parentID,
				Operation: rawSpan.OperationName,
				Service:   serviceName,
				StartTime: parseUnixMicroOrMilli(rawSpan.StartTime),
				Duration:  rawSpan.Duration,
				Tags:      tags,
				Logs:      logs,
			})
		}
	}

	return spans, nil
}
