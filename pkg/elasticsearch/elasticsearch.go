package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"

	"shanyangcode.com/pilot/internal/config"
)

const (
	maxErrorBodyBytes = 2048
	maxBulkRetries    = 2
	bulkRetryDelay    = 100 * time.Millisecond
)

// Client 是可注入的 Elasticsearch 客户端封装。
type Client struct {
	es *elasticsearch.Client
}

// SearchHit 保留 Elasticsearch 命中的原始文档和相关性分数，供需要融合多个召回链路的调用方使用。
type SearchHit struct {
	ID     string                 `json:"id"`
	Score  float64                `json:"score"`
	Source map[string]interface{} `json:"source"`
}

// BulkDocument is one explicitly identified document in a bulk index request.
type BulkDocument struct {
	ID       string
	Document interface{}
}

// New 创建 Elasticsearch 客户端并验证连通性。
func New(ctx context.Context, cfg config.Elasticsearch) (*Client, error) {
	es, err := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: cfg.Addresses,
		Username:  cfg.Username,
		Password:  cfg.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("create elasticsearch client failed: %w", err)
	}

	res, err := es.Info(es.Info.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("elasticsearch connection failed: %w", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		return nil, responseStatusError("elasticsearch error", res)
	}

	return &Client{es: es}, nil
}

// Ping 探测 Elasticsearch 连通性，供 readiness 使用。
func (c *Client) Ping(ctx context.Context) error {
	if c == nil || c.es == nil {
		return fmt.Errorf("elasticsearch client is nil")
	}
	res, err := c.es.Info(c.es.Info.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("elasticsearch ping failed: %w", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		return responseStatusError("elasticsearch ping failed", res)
	}
	return nil
}

// IndexDocument 索引文档并立即刷新（写后可读）。
func (c *Client) IndexDocument(ctx context.Context, index, docID string, document interface{}) error {
	return c.IndexDocumentWithRefresh(ctx, index, docID, document, "true")
}

// IndexDocumentWithRefresh 索引文档并允许调用方控制 refresh 策略。
func (c *Client) IndexDocumentWithRefresh(ctx context.Context, index, docID string, document interface{}, refresh string) error {
	data, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("marshal document failed: %w", err)
	}
	if refresh == "" {
		refresh = "true"
	}

	req := esapi.IndexRequest{
		Index:      index,
		DocumentID: docID,
		Body:       strings.NewReader(string(data)),
		Refresh:    refresh,
	}

	res, err := req.Do(ctx, c.es)
	if err != nil {
		return fmt.Errorf("index document failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		return responseStatusError("index document error", res)
	}

	return nil
}

type bulkIndexOperation struct {
	id   string
	body []byte
}

// BulkIndexDocuments retries transient failures with the original IDs and bytes.
// Accepted items are excluded from subsequent attempts and returned even when
// another item ultimately fails, allowing the caller to compensate the import.
func (c *Client) BulkIndexDocuments(ctx context.Context, index string, documents []BulkDocument) ([]string, error) {
	if len(documents) == 0 {
		return []string{}, nil
	}
	operations := make([]bulkIndexOperation, 0, len(documents))
	for _, document := range documents {
		if strings.TrimSpace(document.ID) == "" {
			return nil, fmt.Errorf("bulk document id cannot be empty")
		}
		header, err := json.Marshal(map[string]any{"index": map[string]any{"_index": index, "_id": document.ID}})
		if err != nil {
			return nil, fmt.Errorf("marshal bulk header failed: %w", err)
		}
		payload, err := json.Marshal(document.Document)
		if err != nil {
			return nil, fmt.Errorf("marshal bulk document %s failed: %w", document.ID, err)
		}
		var body bytes.Buffer
		body.Write(header)
		body.WriteByte('\n')
		body.Write(payload)
		body.WriteByte('\n')
		operations = append(operations, bulkIndexOperation{id: document.ID, body: body.Bytes()})
	}

	indexed := make([]string, 0, len(documents))
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return indexed, errors.Join(lastErr, err)
		}
		accepted, retry, err := c.bulkIndexAttempt(ctx, operations)
		indexed = append(indexed, accepted...)
		if err == nil {
			return indexed, nil
		}
		if len(retry) == 0 || attempt == maxBulkRetries {
			return indexed, fmt.Errorf("bulk index failed after %d attempts (accepted=%d): %w", attempt+1, len(indexed), err)
		}
		lastErr = err
		timer := time.NewTimer(bulkRetryDelay << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return indexed, fmt.Errorf("bulk index retry canceled: %w", errors.Join(err, ctx.Err()))
		case <-timer.C:
		}
		operations = retry
	}
}

func (c *Client) bulkIndexAttempt(ctx context.Context, operations []bulkIndexOperation) ([]string, []bulkIndexOperation, error) {
	var body bytes.Buffer
	for _, operation := range operations {
		body.Write(operation.body)
	}
	res, err := c.es.Bulk(bytes.NewReader(body.Bytes()), c.es.Bulk.WithContext(ctx), c.es.Bulk.WithRefresh("true"))
	if err != nil {
		return nil, nil, fmt.Errorf("bulk index request failed: %w", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		err := responseError("bulk index error", res)
		if retryableBulkStatus(res.StatusCode) {
			return nil, operations, err
		}
		return nil, nil, err
	}
	var response struct {
		Errors bool `json:"errors"`
		Items  []map[string]struct {
			ID     string          `json:"_id"`
			Status int             `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, nil, fmt.Errorf("decode bulk index response failed: %w", err)
	}
	if len(response.Items) != len(operations) {
		return nil, nil, fmt.Errorf("bulk index response item count %d, want %d", len(response.Items), len(operations))
	}
	indexed := make([]string, 0, len(response.Items))
	var retry []bulkIndexOperation
	var failures []error
	permanentFailure := false
	for i, item := range response.Items {
		result, ok := item["index"]
		if !ok || len(item) != 1 || result.ID != operations[i].id || result.Status < 100 || result.Status > 599 {
			return indexed, nil, fmt.Errorf("invalid bulk index response for document %q", operations[i].id)
		}
		if result.Status >= 200 && result.Status < 300 && (len(result.Error) == 0 || string(result.Error) == "null") {
			indexed = append(indexed, result.ID)
			continue
		}
		failures = append(failures, fmt.Errorf("bulk document %q failed: status=%d error=%s", result.ID, result.Status, result.Error))
		if retryableBulkStatus(result.Status) {
			retry = append(retry, operations[i])
		} else {
			permanentFailure = true
		}
	}
	if response.Errors && len(failures) == 0 {
		return indexed, nil, fmt.Errorf("bulk index response reports errors without failed items")
	}
	if permanentFailure {
		retry = nil
	}
	return indexed, retry, errors.Join(failures...)
}

func retryableBulkStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// SearchHits 搜索文档并保留命中 ID 与分数。
func (c *Client) SearchHits(ctx context.Context, index string, query map[string]interface{}) ([]SearchHit, error) {
	queryJSON, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("marshal query failed: %w", err)
	}

	res, err := c.es.Search(
		c.es.Search.WithContext(ctx),
		c.es.Search.WithIndex(index),
		c.es.Search.WithBody(strings.NewReader(string(queryJSON))),
		c.es.Search.WithTrackTotalHits(true),
	)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		return nil, responseStatusError("search error", res)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}

	return extractSearchHits(result)
}

// DeleteDocument 删除文档
func (c *Client) DeleteDocument(ctx context.Context, index, docID string) error {
	req := esapi.DeleteRequest{
		Index:      index,
		DocumentID: docID,
		Refresh:    "true",
	}

	res, err := req.Do(ctx, c.es)
	if err != nil {
		return fmt.Errorf("delete document failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		return responseStatusError("delete document error", res)
	}

	return nil
}

// DeleteByQuery removes every matching document and returns Elasticsearch's
// deleted count. Zero matches are successful, making logical deletes idempotent.
func (c *Client) DeleteByQuery(ctx context.Context, index string, query map[string]interface{}) (int, error) {
	payload, err := json.Marshal(map[string]interface{}{"query": query})
	if err != nil {
		return 0, fmt.Errorf("marshal delete-by-query failed: %w", err)
	}
	res, err := c.es.DeleteByQuery(
		[]string{index},
		strings.NewReader(string(payload)),
		c.es.DeleteByQuery.WithContext(ctx),
		c.es.DeleteByQuery.WithRefresh(true),
		c.es.DeleteByQuery.WithConflicts("proceed"),
	)
	if err != nil {
		return 0, fmt.Errorf("delete by query failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return 0, nil
	}
	if res.IsError() {
		return 0, responseError("delete by query error", res)
	}
	var response struct {
		Deleted  int   `json:"deleted"`
		Failures []any `json:"failures"`
	}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return 0, fmt.Errorf("decode delete-by-query response failed: %w", err)
	}
	if len(response.Failures) > 0 {
		return response.Deleted, fmt.Errorf("delete by query partially failed: deleted=%d failures=%d", response.Deleted, len(response.Failures))
	}
	return response.Deleted, nil
}

// CreateIndex 创建索引；索引已存在时视为成功。
func (c *Client) CreateIndex(ctx context.Context, index string, mapping map[string]interface{}) error {
	mappingJSON, err := json.Marshal(mapping)
	if err != nil {
		return fmt.Errorf("marshal mapping failed: %w", err)
	}

	req := esapi.IndicesCreateRequest{
		Index: index,
		Body:  strings.NewReader(string(mappingJSON)),
	}

	res, err := req.Do(ctx, c.es)
	if err != nil {
		return fmt.Errorf("create index failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body := readErrorBody(res)
		if strings.Contains(body, "resource_already_exists_exception") {
			return nil
		}
		return fmt.Errorf("create index error: status=%d body=%s", res.StatusCode, body)
	}

	return nil
}

// PutMapping 更新索引映射（可用于给已存在索引补充字段）
func (c *Client) PutMapping(ctx context.Context, index string, mapping map[string]interface{}) error {
	mappingJSON, err := json.Marshal(mapping)
	if err != nil {
		return fmt.Errorf("marshal put mapping failed: %w", err)
	}

	req := esapi.IndicesPutMappingRequest{
		Index: []string{index},
		Body:  strings.NewReader(string(mappingJSON)),
	}

	res, err := req.Do(ctx, c.es)
	if err != nil {
		return fmt.Errorf("put mapping failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		return responseError("put mapping error", res)
	}

	return nil
}

func extractSearchHits(result map[string]interface{}) ([]SearchHit, error) {
	hitsRoot, ok := result["hits"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected search response: missing hits object")
	}

	rawHits, ok := hitsRoot["hits"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected search response: missing hits array")
	}

	hits := make([]SearchHit, 0, len(rawHits))
	for _, hit := range rawHits {
		hitMap, ok := hit.(map[string]interface{})
		if !ok {
			continue
		}
		source, ok := hitMap["_source"].(map[string]interface{})
		if !ok {
			continue
		}
		searchHit := SearchHit{Source: source}
		if id, ok := hitMap["_id"].(string); ok {
			searchHit.ID = id
		}
		if score, ok := hitMap["_score"].(float64); ok {
			searchHit.Score = score
		}
		hits = append(hits, searchHit)
	}

	return hits, nil
}

func responseError(prefix string, res *esapi.Response) error {
	body := readErrorBody(res)
	if body == "" {
		return fmt.Errorf("%s: status=%d", prefix, res.StatusCode)
	}
	return fmt.Errorf("%s: status=%d body=%s", prefix, res.StatusCode, body)
}

func responseStatusError(prefix string, res *esapi.Response) error {
	if res == nil {
		return fmt.Errorf("%s", prefix)
	}
	return fmt.Errorf("%s: status=%d", prefix, res.StatusCode)
}

func readErrorBody(res *esapi.Response) string {
	if res == nil || res.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBodyBytes+1))
	text := strings.TrimSpace(string(body))
	if len(body) > maxErrorBodyBytes {
		text += "..."
	}
	return text
}
