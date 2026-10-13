package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"shanyangcode.com/pilot/internal/ai/multiagent"
	"shanyangcode.com/pilot/pkg/elasticsearch"
)

// EpisodicIncident is a compact cross-session incident memory row.
type EpisodicIncident struct {
	ID          uint   `gorm:"primaryKey"`
	UserID      string `gorm:"type:varchar(128);index;not null"`
	SessionID   string `gorm:"type:varchar(128);index;not null"`
	Service     string `gorm:"type:varchar(128);index"`
	Intent      string `gorm:"type:varchar(64);index"`
	Query       string `gorm:"type:text"`
	Answer      string `gorm:"type:text"`
	EvidenceIDs string `gorm:"type:json"`
	Limitations string `gorm:"type:json"`
	CreatedAt   time.Time
}

func (EpisodicIncident) TableName() string { return "episodic_incidents" }

// TextEmbedder generates vectors for hybrid episodic recall.
type TextEmbedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

// EpisodicMemory stores and recalls incident summaries for multi-agent runs.
type EpisodicMemory struct {
	db       *gorm.DB
	es       *elasticsearch.Client
	embedder TextEmbedder
	mode     string // sql | hybrid
	index    string
	topK     int
	dim      int
}

// EpisodicOptions configures hybrid episodic memory.
type EpisodicOptions struct {
	Mode     string
	Index    string
	TopK     int
	ES       *elasticsearch.Client
	Embedder TextEmbedder
	Dim      int
}

// NewEpisodicMemory creates a SQL-only episodic store.
func NewEpisodicMemory(db *gorm.DB) *EpisodicMemory {
	return &EpisodicMemory{db: db, mode: "sql", topK: 3}
}

// NewEpisodicMemoryWithOptions creates sql or hybrid episodic memory.
func NewEpisodicMemoryWithOptions(db *gorm.DB, opts EpisodicOptions) *EpisodicMemory {
	m := NewEpisodicMemory(db)
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = "sql"
	}
	m.mode = mode
	m.index = opts.Index
	if m.index == "" {
		m.index = "pilot_episodic"
	}
	m.topK = opts.TopK
	if m.topK <= 0 {
		m.topK = 3
	}
	m.es = opts.ES
	m.embedder = opts.Embedder
	m.dim = opts.Dim
	if m.dim <= 0 {
		m.dim = 1024
	}
	return m
}

// EnsureEpisodicSchema migrates the episodic table and optionally ES index.
func EnsureEpisodicSchema(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errDBNotInitialized
	}
	return db.WithContext(ctx).AutoMigrate(&EpisodicIncident{})
}

// EnsureHybridIndex creates the ES episodic index mapping when hybrid mode is active.
func (m *EpisodicMemory) EnsureHybridIndex(ctx context.Context) error {
	if m == nil || m.mode != "hybrid" || m.es == nil {
		return nil
	}
	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"incident_id": map[string]interface{}{"type": "long"},
				"service":     map[string]interface{}{"type": "keyword"},
				"intent":      map[string]interface{}{"type": "keyword"},
				"query":       map[string]interface{}{"type": "text"},
				"answer":      map[string]interface{}{"type": "text"},
				"content":     map[string]interface{}{"type": "text"},
				"vector": map[string]interface{}{
					"type":       "dense_vector",
					"dims":       m.dim,
					"index":      true,
					"similarity": "cosine",
				},
				"created_at": map[string]interface{}{"type": "date"},
			},
		},
	}
	return m.es.CreateIndex(ctx, m.index, mapping)
}

// Record implements multiagent.EpisodicRecorder.
func (m *EpisodicMemory) Record(ctx context.Context, record multiagent.EpisodicRecord) error {
	if m == nil || m.db == nil {
		return errDBNotInitialized
	}
	evidenceJSON, _ := json.Marshal(record.EvidenceIDs)
	limitJSON, _ := json.Marshal(record.Limitations)
	row := EpisodicIncident{
		UserID:      record.UserID,
		SessionID:   record.SessionID,
		Service:     record.Service,
		Intent:      record.Intent,
		Query:       record.Query,
		Answer:      truncateRunes(record.Answer, 4000),
		EvidenceIDs: string(evidenceJSON),
		Limitations: string(limitJSON),
		CreatedAt:   time.Now(),
	}
	if err := m.db.WithContext(ctx).Create(&row).Error; err != nil {
		return err
	}
	if m.mode == "hybrid" {
		if err := m.indexHybrid(ctx, row); err != nil {
			slog.WarnContext(ctx, "episodic hybrid index failed; sql row kept", "error", err, "id", row.ID)
		}
	}
	return nil
}

func (m *EpisodicMemory) indexHybrid(ctx context.Context, row EpisodicIncident) error {
	if m.es == nil || m.embedder == nil {
		return fmt.Errorf("hybrid episodic requires es and embedder")
	}
	content := strings.TrimSpace(row.Query + "\n" + row.Answer)
	vector, err := m.embedder.Embed(ctx, content)
	if err != nil {
		return err
	}
	doc := map[string]any{
		"incident_id": row.ID,
		"service":     row.Service,
		"intent":      row.Intent,
		"query":       row.Query,
		"answer":      row.Answer,
		"content":     content,
		"vector":      vector,
		"created_at":  row.CreatedAt,
	}
	return m.es.IndexDocument(ctx, m.index, fmt.Sprintf("%d", row.ID), doc)
}

// Recall implements multiagent.EpisodicRecaller.
func (m *EpisodicMemory) Recall(ctx context.Context, service, query string, limit int) ([]multiagent.Finding, error) {
	if m == nil || m.db == nil {
		return nil, errDBNotInitialized
	}
	if limit <= 0 {
		limit = m.topK
	}
	if limit <= 0 {
		limit = 3
	}

	if m.mode == "hybrid" {
		findings, err := m.recallHybrid(ctx, service, query, limit)
		if err == nil && len(findings) > 0 {
			return findings, nil
		}
		if err != nil {
			slog.WarnContext(ctx, "episodic hybrid recall failed; falling back to sql", "error", err)
		}
	}
	return m.recallSQL(ctx, service, query, limit)
}

// RecallContext returns a bounded, explicitly non-runtime context block for
// ordinary Chat's ContextPreFetcher.
func (m *EpisodicMemory) RecallContext(ctx context.Context, query string, limit int) (string, error) {
	findings, err := m.Recall(ctx, "", query, limit)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, finding := range findings {
		for _, item := range finding.Evidence {
			builder.WriteString("[memory/not_runtime_evidence] ")
			builder.WriteString(truncateRunes(item.Content, 700))
			builder.WriteByte('\n')
		}
	}
	return strings.TrimSpace(builder.String()), nil
}

func (m *EpisodicMemory) recallSQL(ctx context.Context, service, query string, limit int) ([]multiagent.Finding, error) {
	rows, err := m.recallSQLRows(ctx, service, query, limit)
	if err != nil {
		return nil, err
	}
	mode := "sql_keyword"
	if len(rows) == 0 && strings.TrimSpace(query) != "" {
		rows, err = m.recallSQLRows(ctx, service, "", limit)
		mode = "recent"
	}
	return findingsFromRows(rows, mode), err
}

func (m *EpisodicMemory) recallSQLRows(ctx context.Context, service, query string, limit int) ([]EpisodicIncident, error) {
	var rows []EpisodicIncident
	q := m.db.WithContext(ctx).Order("id desc").Limit(limit)
	service = strings.TrimSpace(service)
	if service != "" {
		q = q.Where("service = ?", service)
	}
	query = strings.TrimSpace(query)
	if query != "" {
		like := "%" + truncateRunes(query, 80) + "%"
		q = q.Where("query LIKE ? OR answer LIKE ?", like, like)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (m *EpisodicMemory) recallHybrid(ctx context.Context, service, query string, limit int) ([]multiagent.Finding, error) {
	if m.es == nil || m.embedder == nil {
		return nil, fmt.Errorf("hybrid episodic unavailable")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return m.recallSQL(ctx, service, query, limit)
	}
	filter := []map[string]any{}
	if strings.TrimSpace(service) != "" {
		filter = append(filter, map[string]any{"term": map[string]any{"service": service}})
	}
	type channelResult struct {
		name string
		hits []elasticsearch.SearchHit
		err  error
	}
	results := make(chan channelResult, 2)
	go func() {
		body := map[string]any{
			"size": limit * 2,
			"query": map[string]any{
				"bool": map[string]any{
					"must":   []map[string]any{{"multi_match": map[string]any{"query": query, "fields": []string{"query^2", "answer", "content"}}}},
					"filter": filter,
				},
			},
		}
		hits, err := m.es.SearchHits(ctx, m.index, body)
		results <- channelResult{name: "keyword", hits: hits, err: err}
	}()
	go func() {
		vector, err := m.embedder.Embed(ctx, query)
		if err != nil {
			results <- channelResult{name: "vector", err: err}
			return
		}
		body := map[string]any{
			"size": limit * 2,
			"query": map[string]any{
				"script_score": map[string]any{
					"query":  map[string]any{"bool": map[string]any{"must": []map[string]any{{"exists": map[string]any{"field": "vector"}}}, "filter": filter}},
					"script": map[string]any{"source": "cosineSimilarity(params.query_vector, 'vector') + 1.0", "params": map[string]any{"query_vector": vector}},
				},
			},
		}
		hits, err := m.es.SearchHits(ctx, m.index, body)
		results <- channelResult{name: "vector", hits: hits, err: err}
	}()
	channels := map[string]channelResult{}
	for i := 0; i < 2; i++ {
		result := <-results
		channels[result.name] = result
	}
	if channels["keyword"].err != nil && channels["vector"].err != nil {
		return nil, fmt.Errorf("episodic keyword recall failed: %v; vector recall failed: %w", channels["keyword"].err, channels["vector"].err)
	}
	type fused struct {
		row     EpisodicIncident
		score   float64
		sources map[string]struct{}
	}
	byID := map[uint]*fused{}
	for _, name := range []string{"keyword", "vector"} {
		for rank, hit := range channels[name].hits {
			row := episodicRowFromHit(hit)
			if row.ID == 0 {
				continue
			}
			entry := byID[row.ID]
			if entry == nil {
				entry = &fused{row: row, sources: map[string]struct{}{}}
				byID[row.ID] = entry
			}
			entry.score += 1 / float64(60+rank+1)
			entry.sources[name] = struct{}{}
		}
	}
	recent, recentErr := m.recallSQLRows(ctx, service, "", limit)
	if recentErr != nil && len(byID) == 0 {
		return nil, recentErr
	}
	for _, row := range recent {
		if byID[row.ID] == nil {
			byID[row.ID] = &fused{row: row, sources: map[string]struct{}{"recent": {}}}
		}
	}
	items := make([]*fused, 0, len(byID))
	for _, item := range byID {
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score == items[j].score {
			return items[i].row.CreatedAt.After(items[j].row.CreatedAt)
		}
		return items[i].score > items[j].score
	})
	if len(items) > limit {
		items = items[:limit]
	}
	rows := make([]EpisodicIncident, 0, len(items))
	sources := make([][]string, 0, len(items))
	scores := make([]float64, 0, len(items))
	for _, item := range items {
		rows = append(rows, item.row)
		keys := make([]string, 0, len(item.sources))
		for source := range item.sources {
			keys = append(keys, source)
		}
		sort.Strings(keys)
		sources = append(sources, keys)
		scores = append(scores, item.score)
	}
	findings := findingsFromRows(rows, "hybrid")
	if len(findings) == 1 {
		for i := range findings[0].Evidence {
			findings[0].Evidence[i].Metadata["recall_sources"] = sources[i]
			score := scores[i]
			findings[0].Evidence[i].Score = &score
		}
		if channels["keyword"].err != nil || channels["vector"].err != nil {
			findings[0].Metadata["degraded"] = true
		}
	}
	return findings, nil
}

func episodicRowFromHit(hit elasticsearch.SearchHit) EpisodicIncident {
	src := hit.Source
	incidentID, _ := toUint(src["incident_id"])
	createdAt, _ := src["created_at"].(string)
	parsed, _ := time.Parse(time.RFC3339Nano, createdAt)
	if parsed.IsZero() {
		parsed, _ = time.Parse(time.RFC3339, createdAt)
	}
	return EpisodicIncident{ID: incidentID, Service: fmt.Sprint(src["service"]), Intent: fmt.Sprint(src["intent"]), Query: fmt.Sprint(src["query"]), Answer: fmt.Sprint(src["answer"]), CreatedAt: parsed}
}

func toUint(v any) (uint, bool) {
	switch n := v.(type) {
	case float64:
		return uint(n), true
	case int:
		return uint(n), true
	case int64:
		return uint(n), true
	case json.Number:
		i, err := n.Int64()
		return uint(i), err == nil
	default:
		return 0, false
	}
}

func findingsFromRows(rows []EpisodicIncident, mode string) []multiagent.Finding {
	if len(rows) == 0 {
		return nil
	}
	evidence := make([]multiagent.EvidenceItem, 0, len(rows))
	for i, row := range rows {
		evidence = append(evidence, multiagent.EvidenceItem{
			ID:      fmt.Sprintf("episodic:%d", row.ID),
			Source:  "episodic_memory",
			Content: truncateRunes(fmt.Sprintf("prior intent=%s query=%s conclusion=%s", row.Intent, row.Query, row.Answer), 900),
			Metadata: map[string]any{
				"service":              row.Service,
				"session_id":           row.SessionID,
				"created_at":           row.CreatedAt,
				"index":                i,
				"mode":                 mode,
				"memory":               true,
				"not_runtime_evidence": true,
			},
		})
	}
	return []multiagent.Finding{{
		Role:     multiagent.RoleKnowledge,
		Summary:  fmt.Sprintf("Recalled %d similar prior incident summary(ies) via %s.", len(rows), mode),
		Evidence: evidence,
		Metadata: map[string]any{"episodic": true, "memory": true, "mode": mode, "not_runtime_evidence": true},
	}}
}

func truncateRunes(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "..."
}
