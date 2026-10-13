package types

import "time"

// AlertInfo 告警信息
type AlertInfo struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Severity    string            `json:"severity"`
	Status      string            `json:"status"`
	Description string            `json:"description"`
	Labels      map[string]string `json:"labels"`
	StartTime   time.Time         `json:"start_time"`
	EndTime     *time.Time        `json:"end_time,omitempty"`
	Annotations map[string]string `json:"annotations"`
}

// LogEntry 日志条目
type LogEntry struct {
	Timestamp time.Time         `json:"timestamp"`
	Level     string            `json:"level"`
	Message   string            `json:"message"`
	Service   string            `json:"service"`
	TraceID   string            `json:"trace_id,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
}

// MetricData 指标数据
type MetricData struct {
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Timestamp time.Time         `json:"timestamp"`
	Labels    map[string]string `json:"labels"`
}

// TraceSpan 链路追踪 Span
type TraceSpan struct {
	TraceID   string            `json:"trace_id"`
	SpanID    string            `json:"span_id"`
	ParentID  string            `json:"parent_id,omitempty"`
	Operation string            `json:"operation"`
	Service   string            `json:"service"`
	StartTime time.Time         `json:"start_time"`
	Duration  int64             `json:"duration"` // 微秒
	Tags      map[string]string `json:"tags"`
	Logs      []TraceLog        `json:"logs,omitempty"`
}

// TraceLog 链路日志
type TraceLog struct {
	Timestamp time.Time         `json:"timestamp"`
	Fields    map[string]string `json:"fields"`
}

// KnowledgeDocument 知识库文档
type KnowledgeDocument struct {
	ID              string            `json:"id"` // backward-compatible alias of DocID
	DocID           string            `json:"doc_id"`
	ChunkID         string            `json:"chunk_id"`
	ChunkIndex      int               `json:"chunk_index"`
	ChunkCount      int               `json:"chunk_count"`
	SectionID       string            `json:"section_id,omitempty"`
	SectionPath     string            `json:"section_path,omitempty"`
	RecordType      string            `json:"record_type,omitempty"`
	SplitterVersion string            `json:"splitter_version,omitempty"`
	StartRune       int               `json:"start_rune"`
	EndRune         int               `json:"end_rune"`
	Title           string            `json:"title"`
	Content         string            `json:"content"`
	Category        string            `json:"category"`
	Tags            []string          `json:"tags"`
	Metadata        map[string]string `json:"metadata"`
	Vector          []float64         `json:"vector,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// ChatMessage 对话消息
type ChatMessage struct {
	RunID     string    `json:"run_id,omitempty"`
	Role      string    `json:"role"` // user, assistant, system
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}
