package observability

import (
	"bytes"
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

// LogClient 日志客户端（基于 Elasticsearch）
type LogClient struct {
	baseURL string
	index   string
	client  *http.Client
	tracer  trace.Tracer
}

// NewLogClient 创建日志客户端
func NewLogClient(baseURL, index string) *LogClient {
	if index == "" {
		index = "pilot_logs"
	}

	return &LogClient{
		baseURL: baseURL,
		index:   index,
		client:  &http.Client{Timeout: 30 * time.Second},
		tracer:  otel.Tracer("pilot/observability/logs"),
	}
}

// QueryLogs 查询日志
func (lc *LogClient) QueryLogs(ctx context.Context, service, level, keyword string, startTime, endTime time.Time, limit int) ([]types.LogEntry, error) {
	var err error
	service, err = NormalizeService(service, "")
	if err != nil {
		return nil, err
	}
	level, err = NormalizeLogLevel(level, "")
	if err != nil {
		return nil, err
	}
	keyword, err = NormalizeLogKeyword(keyword)
	if err != nil {
		return nil, err
	}
	startTime, endTime = NormalizeTimeRange(startTime, endTime)
	limit = NormalizeLimit(limit)

	ctx, span := startClientSpan(
		ctx,
		lc.tracer,
		"elasticsearch.query_logs",
		attribute.String("elasticsearch.index", lc.index),
		attribute.String("log.service", service),
		attribute.String("log.level", level),
		attribute.Bool("log.keyword_present", keyword != ""),
	)
	defer span.End()

	// 构建 ES 查询（可选 service/level 过滤 + 时间范围）
	mustQuery := []interface{}{
		map[string]interface{}{
			"range": map[string]interface{}{
				"timestamp": map[string]interface{}{
					"gte": startTime.Format(time.RFC3339),
					"lte": endTime.Format(time.RFC3339),
				},
			},
		},
	}
	//存在条件则追加查询要求
	if strings.TrimSpace(service) != "" {
		mustQuery = append(mustQuery, map[string]interface{}{
			//bool：把多个查询条件按逻辑关系组合起来
			//should候选条件，至少满足几个由 minimum_should_match 控制
			"bool": map[string]interface{}{
				"should": []interface{}{
					map[string]interface{}{
						//term：精确匹配，keyword字段是ES的关键字类型，适合做精确匹配
						"term": map[string]interface{}{
							"service.keyword": service,
						},
					},
					map[string]interface{}{
						"term": map[string]interface{}{
							"service": service,
						},
					},
				},
				"minimum_should_match": 1,
			},
		})
	}
	if strings.TrimSpace(level) != "" && level != "all" {
		mustQuery = append(mustQuery, map[string]interface{}{
			"bool": map[string]interface{}{
				"should": []interface{}{
					map[string]interface{}{
						"term": map[string]interface{}{
							"level.keyword": map[string]interface{}{"value": strings.ToLower(level), "case_insensitive": true},
						},
					},
					map[string]interface{}{
						"term": map[string]interface{}{
							"level": map[string]interface{}{"value": strings.ToLower(level), "case_insensitive": true},
						},
					},
				},
				"minimum_should_match": 1,
			},
		})
	}
	// 非空关键词参与多字段匹配
	if strings.TrimSpace(keyword) != "" {
		mustQuery = append(mustQuery, map[string]interface{}{
			"multi_match": map[string]interface{}{
				"query":  keyword,
				"fields": []string{"message", "service", "trace_id"},
				"type":   "best_fields",
			},
		})
	}

	query := map[string]interface{}{
		"size": limit,
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				"must": mustQuery,
			},
		},
		"sort": []interface{}{
			map[string]interface{}{
				"timestamp": map[string]string{
					"order": "desc",
				},
			},
		},
	}

	body, err := json.Marshal(query)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("marshal log query failed: %w", err)
	}

	searchURL := fmt.Sprintf("%s/%s/_search", strings.TrimRight(lc.baseURL, "/"), lc.index)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, searchURL, bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("create log search request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := lc.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("query logs from elasticsearch failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("read log search response failed: %w", err)
	}

	// 索引不存在时返回空结果，避免影响整体功能。
	if resp.StatusCode == http.StatusNotFound && strings.Contains(string(respBody), "index_not_found_exception") {
		return make([]types.LogEntry, 0), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		span.SetStatus(codes.Error, resp.Status)
		return nil, formatHTTPStatusError("query logs failed", resp.StatusCode, respBody)
	}

	var result struct {
		Hits struct {
			Hits []struct {
				Source map[string]interface{} `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("decode log search response failed: %w", err)
	}

	logs := make([]types.LogEntry, 0, len(result.Hits.Hits))
	for _, hit := range result.Hits.Hits {
		src := hit.Source

		entry := types.LogEntry{
			Timestamp: parseLogTimestamp(src["timestamp"]),
			Level:     toString(src["level"]),
			Message:   toString(src["message"]),
			Service:   toString(src["service"]),
			TraceID:   toString(src["trace_id"]),
		}

		if rawFields, ok := src["fields"].(map[string]interface{}); ok {
			fields := make(map[string]string, len(rawFields))
			for k, v := range rawFields {
				fields[k] = toString(v)
			}
			entry.Fields = fields
		}

		if entry.Level == "" {
			entry.Level = "info"
		}
		if entry.Service == "" {
			entry.Service = "unknown"
		}
		if entry.Message == "" {
			entry.Message = "(empty log message)"
		}
		if entry.Timestamp.IsZero() {
			entry.Timestamp = time.Now()
		}

		logs = append(logs, entry)
	}

	return logs, nil
}
