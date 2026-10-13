package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

// bulkAction 定义了 Elasticsearch Bulk API 中的操作行格式，这里只使用了 index 操作
type bulkAction struct {
	Index map[string]string `json:"index"`
}

// logEntry 定义了日志条目的结构，包括时间戳、日志级别、服务名称、日志消息、Trace ID 和一些额外字段
type logEntry struct {
	Timestamp string            `json:"timestamp"`
	Level     string            `json:"level"`
	Service   string            `json:"service"`
	Message   string            `json:"message"`
	TraceID   string            `json:"trace_id"`
	Fields    map[string]string `json:"fields"`
}

func main() {
	output := flag.String("output", "/tmp/pilot_logs_bulk.ndjson", "output ndjson file path")
	index := flag.String("index", "pilot_logs", "elasticsearch index name")
	count := flag.Int("count", 40, "number of logs")
	flag.Parse()

	if *count <= 0 {
		*count = 40
	}

	now := time.Now().UTC()
	levels := []string{"info", "warn", "error", "info", "error", "warn", "info", "debug"}
	services := []string{"pilot-api", "pilot-worker"}

	file, err := os.Create(*output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create output file failed: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	for i := 0; i < *count; i++ {
		level := levels[i%len(levels)]
		service := services[i%len(services)]
		ts := now.Add(-time.Duration(*count-i) * 90 * time.Second)

		action := bulkAction{
			Index: map[string]string{"_index": *index},
		}
		if err := encoder.Encode(action); err != nil {
			fmt.Fprintf(os.Stderr, "encode bulk action failed: %v\n", err)
			os.Exit(1)
		}

		entry := logEntry{
			Timestamp: ts.Format(time.RFC3339),
			Level:     level,
			Service:   service,
			Message:   messageFor(service, level, i),
			TraceID:   fmt.Sprintf("trace_%d", now.UnixNano()+int64(i)),
			Fields: map[string]string{
				"env":       "local-demo",
				"namespace": "pilot",
				"pod":       fmt.Sprintf("%s-%d", service, i%3),
				"instance":  fmt.Sprintf("10.20.%d.%d", (i%2)+1, (i%20)+10),
			},
		}
		if err := encoder.Encode(entry); err != nil {
			fmt.Fprintf(os.Stderr, "encode log entry failed: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Printf("generated %d logs: %s\n", *count, *output)
}

// messageFor 根据服务、日志级别和索引生成相应的日志消息
func messageFor(service, level string, i int) string {
	switch level {
	case "error":
		msgs := []string{
			"HTTP 500: upstream timeout in %s",
			"redis timeout in %s",
			"database connection pool exhausted in %s",
		}
		return fmt.Sprintf(msgs[i%len(msgs)], service)
	case "warn":
		msgs := []string{
			"high latency detected on %s, p95=780ms",
			"retrying request in %s, attempt=2",
			"memory usage above threshold in %s",
		}
		return fmt.Sprintf(msgs[i%len(msgs)], service)
	case "debug":
		return fmt.Sprintf("debug trace enabled for %s", service)
	default:
		msgs := []string{
			"request completed successfully in %s",
			"scheduled job heartbeat ok in %s",
		}
		return fmt.Sprintf(msgs[i%len(msgs)], service)
	}
}
