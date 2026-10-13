package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

const (
	defaultESURL  = "http://localhost:9200"
	defaultIndex  = "pilot_logs"
	defaultCount  = 40
	defaultOutput = ""
)

type logEntry struct {
	Timestamp  string `json:"@timestamp"`
	Level      string `json:"level"`
	Message    string `json:"message"`
	Service    string `json:"service"`
	TraceID    string `json:"trace_id,omitempty"`
	SpanID     string `json:"span_id,omitempty"`
	Duration   int64  `json:"duration_ms,omitempty"`
	Status     string `json:"status,omitempty"`
	Method     string `json:"method,omitempty"`
	Path       string `json:"path,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
}

func main() {
	esURL := flag.String("es", defaultESURL, "Elasticsearch URL")
	index := flag.String("index", defaultIndex, "Target index name")
	count := flag.Int("count", defaultCount, "Number of log entries to generate")
	output := flag.String("output", defaultOutput, "Output file path (default: system temp dir)")
	flag.Parse()

	filePath := *output
	if filePath == "" {
		filePath = filepath.Join(os.TempDir(), "pilot_logs_bulk.ndjson")
	}

	if err := generateLogs(filePath, *index, *count); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate logs: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Generated %d log entries to: %s\n", *count, filePath)

	if err := pushToElasticsearch(*esURL, filePath); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to push to Elasticsearch: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Successfully pushed logs to Elasticsearch")

	if err := os.Remove(filePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove temp file: %v\n", err)
		return
	}
	fmt.Printf("Cleaned up: %s\n", filePath)
}

func generateLogs(filePath, index string, count int) error {
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer file.Close()

	levels := []string{"INFO", "WARN", "ERROR", "DEBUG"}
	methods := []string{"GET", "POST", "PUT", "DELETE"}
	paths := []string{"/api/v1/chat", "/api/v1/health", "/api/v1/users", "/api/v1/memory"}
	services := []string{"pilot-api", "pilot-worker", "pilot-scheduler"}
	now := time.Now()

	for i := 0; i < count; i++ {
		indexLine := map[string]map[string]string{
			"index": {"_index": index},
		}
		indexJSON, _ := json.Marshal(indexLine)
		fmt.Fprintln(file, string(indexJSON))

		entry := logEntry{
			Timestamp:  now.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339),
			Level:      levels[i%len(levels)],
			Message:    fmt.Sprintf("Sample log message #%d", i+1),
			Service:    services[i%len(services)],
			TraceID:    uuid.New().String()[:32],
			Method:     methods[i%len(methods)],
			Path:       paths[i%len(paths)],
			StatusCode: 200 + (i%4)*100,
			Duration:   int64(10 + i%100),
			Status:     "success",
		}

		entryJSON, _ := json.Marshal(entry)
		fmt.Fprintln(file, string(entryJSON))
	}

	return nil
}

func pushToElasticsearch(esURL, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	content, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/_bulk?refresh=true", esURL), bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("elasticsearch error (status %d): %s", resp.StatusCode, string(body))
	}

	return nil
}
