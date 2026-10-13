package main

import (
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
	"shanyangcode.com/pilot/pkg/elasticsearch"
	"shanyangcode.com/pilot/pkg/envloader"
	"shanyangcode.com/pilot/pkg/redis"
)

// checkItem 检查项，包含名称、状态和详细信息
type checkItem struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// checkReport 检查报告，包含检查时间、总结和检查项列表
type checkReport struct {
	CheckedAt string      `json:"checked_at"`
	Summary   string      `json:"summary"`
	Items     []checkItem `json:"items"`
}

func main() {
	ctx := context.Background()
	items := make([]checkItem, 0, 8)
	failed := false

	if err := envloader.Load(); err != nil {
		appendItem(&items, "env_loader", "fail", err.Error())
		printReport(items, true)
		os.Exit(1)
	}
	appendItem(&items, "env_loader", "pass", "loaded .env (if exists)")

	cfg, err := config.Load(config.ResolvePath(""))
	if err != nil {
		appendItem(&items, "config", "fail", err.Error())
		printReport(items, true)
		os.Exit(1)
	}
	appendItem(&items, "config", "pass", "typed config loaded and validated")

	if redisClient, err := redis.New(ctx, cfg.Redis); err != nil {
		appendItem(&items, "redis", "fail", err.Error())
		failed = true
	} else {
		_ = redisClient.Close()
		appendItem(&items, "redis", "pass", cfg.Redis.Addr)
	}

	if _, err := elasticsearch.New(ctx, cfg.Elasticsearch); err != nil {
		appendItem(&items, "elasticsearch", "fail", err.Error())
		failed = true
	} else {
		appendItem(&items, "elasticsearch", "pass", strings.Join(cfg.Elasticsearch.Addresses, ","))
	}

	if db, err := memory.OpenDB(ctx, cfg.Database); err != nil {
		appendItem(&items, "mysql", "fail", err.Error())
		failed = true
	} else {
		_ = memory.CloseDB(db)
		appendItem(&items, "mysql", "pass", fmt.Sprintf("%s:%d/%s", cfg.Database.Host, cfg.Database.Port, cfg.Database.Name))
	}

	if err := checkPrometheus(ctx, cfg.Observability.Prometheus.URL); err != nil {
		appendItem(&items, "prometheus", "fail", err.Error())
		failed = true
	} else {
		appendItem(&items, "prometheus", "pass", cfg.Observability.Prometheus.URL)
	}

	if err := checkTraceQuery(ctx, cfg.Observability.Trace.URL); err != nil {
		appendItem(&items, "jaeger", "fail", err.Error())
		failed = true
	} else {
		appendItem(&items, "jaeger", "pass", cfg.Observability.Trace.URL)
	}

	if err := checkDashScopeKey(cfg); err != nil {
		appendItem(&items, "dashscope_key", "fail", err.Error())
		failed = true
	} else {
		appendItem(&items, "dashscope_key", "pass", "llm.main_model / embedding api_key ready")
	}

	if status, detail := checkLogIndex(ctx, cfg.Observability.Log.URL, cfg.Observability.Log.Index); status != "pass" {
		appendItem(&items, "log_index", status, detail)
	} else {
		appendItem(&items, "log_index", "pass", detail)
	}

	printReport(items, failed)
	if failed {
		os.Exit(1)
	}
}

// appendItem 向检查项列表中添加一个新的检查项
func appendItem(items *[]checkItem, name, status, detail string) {
	*items = append(*items, checkItem{
		Name:   name,
		Status: status,
		Detail: detail,
	})
}

// printReport 输出检查报告，包含所有检查项和总结
func printReport(items []checkItem, failed bool) {
	summary := "all_passed"
	if failed {
		summary = "has_failure"
	}

	report := checkReport{
		CheckedAt: time.Now().Format(time.RFC3339),
		Summary:   summary,
		Items:     items,
	}

	data, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(data))
}

// checkPrometheus 通过访问 Prometheus 的 readiness endpoint 来检查其是否可用
func checkPrometheus(ctx context.Context, baseURL string) error {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return fmt.Errorf("observability.prometheus.url is empty")
	}
	return checkHTTPEndpoint(ctx, baseURL+"/-/ready")
}

// checkTraceQuery 检查 Jaeger Query UI 是否可达。
func checkTraceQuery(ctx context.Context, baseURL string) error {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return fmt.Errorf("observability.trace.url is empty")
	}
	return checkHTTPEndpoint(ctx, baseURL+"/api/services")
}

func checkHTTPEndpoint(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// checkDashScopeKey 检查 DashScope API Key 是否已通过配置或环境变量注入
func checkDashScopeKey(cfg *config.Config) error {
	if strings.TrimSpace(cfg.LLM.MainModel.APIKey) == "" {
		return fmt.Errorf("missing llm.main_model.api_key or DASHSCOPE_API_KEY")
	}
	if strings.TrimSpace(cfg.Embedding.APIKey) == "" {
		return fmt.Errorf("missing embedding.api_key or DASHSCOPE_API_KEY")
	}
	return nil
}

// checkLogIndex 通过访问 Elasticsearch 的 _count API 来检查日志索引是否存在以及可用
func checkLogIndex(ctx context.Context, logURL, index string) (string, string) {
	logURL = strings.TrimRight(logURL, "/")
	if logURL == "" || index == "" {
		return "warn", "observability.log.url or observability.log.index is empty"
	}

	countURL := fmt.Sprintf("%s/%s/_count", logURL, index)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, countURL, nil)
	if err != nil {
		return "warn", err.Error()
	}

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "warn", err.Error()
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return "warn", fmt.Sprintf("index %s not found (import sample logs or create index)", index)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "warn", fmt.Sprintf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var countResult struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal(body, &countResult); err != nil {
		return "warn", fmt.Sprintf("cannot parse count response: %v", err)
	}
	return "pass", fmt.Sprintf("index=%s count=%d", index, countResult.Count)
}
