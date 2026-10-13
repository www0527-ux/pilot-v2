package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/internal/memory"
	"shanyangcode.com/pilot/internal/observability"
	"shanyangcode.com/pilot/pkg/envloader"
	"shanyangcode.com/pilot/pkg/redis"
	"shanyangcode.com/pilot/pkg/types"
)

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type report struct {
	CheckedAt string        `json:"checked_at"`
	BaseURL   string        `json:"base_url"`
	Summary   string        `json:"summary"`
	Results   []checkResult `json:"results"`
}

type knowledgeUploadResponse struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		ID string `json:"id"`
	} `json:"data"`
}

type healthResponse struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Status string `json:"status"`
	} `json:"data"`
}

type chatResponse struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

type historyResponse struct {
	Success bool                `json:"success"`
	Code    string              `json:"code"`
	Message string              `json:"message"`
	Data    []types.ChatMessage `json:"data"`
}

type knowledgeSearchResponse struct {
	Success bool                      `json:"success"`
	Code    string                    `json:"code"`
	Message string                    `json:"message"`
	Data    []types.KnowledgeDocument `json:"data"`
}

func main() {
	ctx := context.Background()

	if err := envloader.Load(); err != nil {
		failAndExit(report{
			CheckedAt: time.Now().Format(time.RFC3339),
			Summary:   "failed",
			Results: []checkResult{{
				Name:   "env_loader",
				Status: "fail",
				Detail: err.Error(),
			}},
		})
	}

	cfg, err := config.Load(config.ResolvePath(""))
	if err != nil {
		failAndExit(singleFailure("config", err))
	}

	redisClient, err := redis.New(ctx, cfg.Redis)
	if err != nil {
		failAndExit(singleFailure("redis_init", err))
	}
	db, err := memory.OpenDB(ctx, cfg.Database)
	if err != nil {
		failAndExit(singleFailure("mysql_init", err))
	}

	baseURL := strings.TrimRight(cfg.Server.BaseURL, "/")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	now := time.Now()
	suffix := now.Format("20060102150405")
	userID := "e2e_user_" + suffix
	sessionID := "e2e_session_" + suffix
	docTitle := "Redis 超时排查 E2E " + suffix

	results := make([]checkResult, 0, 10)
	client := &http.Client{Timeout: 90 * time.Second}

	if err := checkHealth(ctx, client, baseURL); err != nil {
		results = append(results, checkResult{Name: "health", Status: "fail", Detail: err.Error()})
		printReportAndExit(baseURL, results, true)
	}
	results = append(results, checkResult{Name: "health", Status: "pass", Detail: "server is reachable"})

	if err := checkReady(ctx, client, baseURL); err != nil {
		results = append(results, checkResult{Name: "ready", Status: "fail", Detail: err.Error()})
		printReportAndExit(baseURL, results, true)
	}
	results = append(results, checkResult{Name: "ready", Status: "pass", Detail: "core dependencies are ready"})

	if err := uploadKnowledge(ctx, client, baseURL, docTitle); err != nil {
		results = append(results, checkResult{Name: "knowledge_upload", Status: "fail", Detail: err.Error()})
		printReportAndExit(baseURL, results, true)
	}
	results = append(results, checkResult{Name: "knowledge_upload", Status: "pass", Detail: docTitle})

	if err := searchKnowledge(ctx, client, baseURL, docTitle); err != nil {
		results = append(results, checkResult{Name: "knowledge_search", Status: "fail", Detail: err.Error()})
		printReportAndExit(baseURL, results, true)
	}
	results = append(results, checkResult{Name: "knowledge_search", Status: "pass", Detail: "knowledge index query returned the uploaded document"})

	chatText, err := sendChat(ctx, client, baseURL, userID, sessionID)
	if err != nil {
		results = append(results, checkResult{Name: "chat", Status: "fail", Detail: err.Error()})
		printReportAndExit(baseURL, results, true)
	}
	results = append(results, checkResult{Name: "chat", Status: "pass", Detail: truncate(chatText, 120)})

	historyMessages, err := getHistory(ctx, client, baseURL, userID, sessionID)
	if err != nil {
		results = append(results, checkResult{Name: "chat_history_api", Status: "fail", Detail: err.Error()})
		printReportAndExit(baseURL, results, true)
	}
	results = append(results, checkResult{
		Name:   "chat_history_api",
		Status: "pass",
		Detail: fmt.Sprintf("history messages=%d", len(historyMessages)),
	})

	shortTermMemory := memory.NewShortTermMemory(redisClient, cfg.Redis.Memory)
	redisMessages, err := shortTermMemory.GetMessages(ctx, userID, sessionID)
	if err != nil {
		results = append(results, checkResult{Name: "redis_short_memory", Status: "fail", Detail: err.Error()})
		printReportAndExit(baseURL, results, true)
	}
	if len(redisMessages) < 2 {
		results = append(results, checkResult{Name: "redis_short_memory", Status: "fail", Detail: fmt.Sprintf("redis messages=%d, want at least 2", len(redisMessages))})
		printReportAndExit(baseURL, results, true)
	}
	results = append(results, checkResult{
		Name:   "redis_short_memory",
		Status: "pass",
		Detail: fmt.Sprintf("redis messages=%d", len(redisMessages)),
	})

	historyManager := memory.NewConversationHistoryManager(db)
	mysqlHistory, err := historyManager.GetSessionHistory(ctx, userID, sessionID, 10)
	if err != nil {
		results = append(results, checkResult{Name: "mysql_history", Status: "fail", Detail: err.Error()})
		printReportAndExit(baseURL, results, true)
	}
	if len(mysqlHistory) < 2 {
		results = append(results, checkResult{Name: "mysql_history", Status: "fail", Detail: fmt.Sprintf("mysql history rows=%d, want at least 2", len(mysqlHistory))})
		printReportAndExit(baseURL, results, true)
	}
	results = append(results, checkResult{
		Name:   "mysql_history",
		Status: "pass",
		Detail: fmt.Sprintf("mysql history rows=%d", len(mysqlHistory)),
	})

	logClient := observability.NewLogClient(cfg.Observability.Log.URL, cfg.Observability.Log.Index)

	var chatLog types.LogEntry
	if err := retry(6, 2*time.Second, func() error {
		entry, findErr := findChatLog(ctx, logClient, cfg.Observability.ServiceName, now.Add(-5*time.Minute))
		if findErr != nil {
			return findErr
		}
		chatLog = entry
		return nil
	}); err != nil {
		results = append(results, checkResult{Name: "elasticsearch_runtime_logs", Status: "fail", Detail: err.Error()})
		printReportAndExit(baseURL, results, true)
	}
	results = append(results, checkResult{
		Name:   "elasticsearch_runtime_logs",
		Status: "pass",
		Detail: fmt.Sprintf("trace_id=%s level=%s", chatLog.TraceID, chatLog.Level),
	})

	promClient := observability.NewPrometheusClient(cfg.Observability.Prometheus.URL, 30*time.Second)

	metricQuery := `pilot_http_requests_total{route="/api/v1/chat",status="200"}`
	var metricPoints []types.MetricData
	if err := retry(8, 4*time.Second, func() error {
		end := time.Now()
		start := end.Add(-10 * time.Minute)
		points, queryErr := promClient.QueryRange(ctx, metricQuery, start, end)
		if queryErr != nil {
			return queryErr
		}
		if len(points) == 0 {
			return fmt.Errorf("prometheus has not scraped %s yet", metricQuery)
		}
		metricPoints = points
		return nil
	}); err != nil {
		results = append(results, checkResult{Name: "prometheus_metrics", Status: "fail", Detail: err.Error()})
		printReportAndExit(baseURL, results, true)
	}
	results = append(results, checkResult{
		Name:   "prometheus_metrics",
		Status: "pass",
		Detail: fmt.Sprintf("metric points=%d latest=%.0f", len(metricPoints), metricPoints[len(metricPoints)-1].Value),
	})

	traceClient := observability.NewTraceClient(cfg.Observability.Trace.URL)
	traceSpans, err := traceClient.GetTrace(ctx, chatLog.TraceID)
	if err != nil {
		results = append(results, checkResult{Name: "jaeger_trace", Status: "fail", Detail: err.Error()})
		printReportAndExit(baseURL, results, true)
	}
	if len(traceSpans) == 0 {
		results = append(results, checkResult{Name: "jaeger_trace", Status: "fail", Detail: "trace query returned no spans"})
		printReportAndExit(baseURL, results, true)
	}
	results = append(results, checkResult{
		Name:   "jaeger_trace",
		Status: "pass",
		Detail: fmt.Sprintf("trace spans=%d trace_id=%s", len(traceSpans), chatLog.TraceID),
	})

	printReportAndExit(baseURL, results, false)
}

func singleFailure(name string, err error) report {
	return report{
		CheckedAt: time.Now().Format(time.RFC3339),
		Summary:   "failed",
		Results: []checkResult{{
			Name:   name,
			Status: "fail",
			Detail: err.Error(),
		}},
	}
}

func checkHealth(ctx context.Context, client *http.Client, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("health status=%d body=%s", resp.StatusCode, string(body))
	}
	var payload healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("decode health response: %w", err)
	}
	if !payload.Success || payload.Code != "OK" || payload.Data.Status != "ok" {
		return fmt.Errorf("invalid health envelope: success=%t code=%s status=%s", payload.Success, payload.Code, payload.Data.Status)
	}
	return nil
}

func checkReady(ctx context.Context, client *http.Client, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/health/ready", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ready status=%d body=%s", resp.StatusCode, string(body))
	}
	var payload healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("decode ready response: %w", err)
	}
	if !payload.Success || payload.Code != "OK" || payload.Data.Status != "ok" {
		return fmt.Errorf("invalid ready envelope: success=%t code=%s status=%s", payload.Success, payload.Code, payload.Data.Status)
	}
	return nil
}

func uploadKnowledge(ctx context.Context, client *http.Client, baseURL, docTitle string) error {
	payload := map[string]string{
		"title":    docTitle,
		"content":  "排查步骤：优先检查连接池、慢命令、网络抖动、实例负载和 key 大小。",
		"category": "middleware",
	}
	var respBody knowledgeUploadResponse
	if err := doJSON(ctx, client, http.MethodPost, baseURL+"/api/v1/knowledge/upload", payload, &respBody); err != nil {
		return err
	}
	if err := requireSuccessEnvelope("knowledge upload", respBody.Success, respBody.Code, respBody.Message); err != nil {
		return err
	}
	if strings.TrimSpace(respBody.Data.ID) == "" {
		return fmt.Errorf("knowledge upload response is missing data.id")
	}
	return nil
}

func searchKnowledge(ctx context.Context, client *http.Client, baseURL, docTitle string) error {
	payload := map[string]string{"query": docTitle}
	var respBody knowledgeSearchResponse
	if err := doJSON(ctx, client, http.MethodPost, baseURL+"/api/v1/knowledge/search", payload, &respBody); err != nil {
		return err
	}
	if err := requireSuccessEnvelope("knowledge search", respBody.Success, respBody.Code, respBody.Message); err != nil {
		return err
	}
	for _, doc := range respBody.Data {
		if doc.Title == docTitle {
			return nil
		}
	}
	return fmt.Errorf("uploaded document not found in search results")
}

func sendChat(ctx context.Context, client *http.Client, baseURL, userID, sessionID string) (string, error) {
	payload := map[string]string{
		"user_id":    userID,
		"session_id": sessionID,
		"query":      "请根据刚上传的知识库内容，总结 Redis 超时排查步骤，并告诉我优先观察哪些可观测性数据。",
	}
	var respBody chatResponse
	if err := doJSON(ctx, client, http.MethodPost, baseURL+"/api/v1/chat", payload, &respBody); err != nil {
		return "", err
	}
	if err := requireSuccessEnvelope("chat", respBody.Success, respBody.Code, respBody.Message); err != nil {
		return "", err
	}
	if strings.TrimSpace(respBody.Data) == "" {
		return "", fmt.Errorf("chat response is empty")
	}
	return respBody.Data, nil
}

func getHistory(ctx context.Context, client *http.Client, baseURL, userID, sessionID string) ([]types.ChatMessage, error) {
	url := fmt.Sprintf("%s/api/v1/chat/history?user_id=%s&session_id=%s&limit=10", baseURL, userID, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("history status=%d body=%s", resp.StatusCode, string(body))
	}

	var history historyResponse
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		return nil, err
	}
	if err := requireSuccessEnvelope("history", history.Success, history.Code, history.Message); err != nil {
		return nil, err
	}
	if len(history.Data) < 2 {
		return nil, fmt.Errorf("history message count=%d, want at least 2", len(history.Data))
	}
	return history.Data, nil
}

func findChatLog(ctx context.Context, logClient *observability.LogClient, service string, start time.Time) (types.LogEntry, error) {
	logs, err := logClient.QueryLogs(ctx, service, "", "/api/v1/chat", start, time.Now(), 20)
	if err != nil {
		return types.LogEntry{}, err
	}
	for _, entry := range logs {
		if entry.Fields["path"] == "/api/v1/chat" && entry.TraceID != "" {
			return entry, nil
		}
	}
	return types.LogEntry{}, fmt.Errorf("chat runtime log not found yet")
}

func doJSON(ctx context.Context, client *http.Client, method, url string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("request %s %s failed: status=%d body=%s", method, url, resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response failed: %w body=%s", err, string(raw))
	}
	return nil
}

func requireSuccessEnvelope(operation string, success bool, code, message string) error {
	if success && code == "OK" {
		return nil
	}
	return fmt.Errorf("%s response failed: success=%t code=%s message=%s", operation, success, code, message)
}

func retry(attempts int, interval time.Duration, fn func() error) error {
	if attempts <= 0 {
		attempts = 1
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if i < attempts-1 {
			time.Sleep(interval)
		}
	}
	return lastErr
}

func truncate(text string, max int) string {
	text = strings.TrimSpace(text)
	if max <= 0 || len([]rune(text)) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max]) + "..."
}

func printReportAndExit(baseURL string, results []checkResult, failed bool) {
	summary := "passed"
	if failed {
		summary = "failed"
	}

	payload := report{
		CheckedAt: time.Now().Format(time.RFC3339),
		BaseURL:   baseURL,
		Summary:   summary,
		Results:   results,
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Println(string(data))
	if failed {
		os.Exit(1)
	}
}

func failAndExit(payload report) {
	data, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Println(string(data))
	os.Exit(1)
}
