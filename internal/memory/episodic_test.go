package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"shanyangcode.com/pilot/internal/config"
	pkgelastic "shanyangcode.com/pilot/pkg/elasticsearch"
)

func episodicTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&EpisodicIncident{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestEpisodicSQLUsesRecentFallbackAndMarksMemoryBoundary(t *testing.T) {
	db := episodicTestDB(t)
	row := EpisodicIncident{UserID: "u", SessionID: "s", Service: "api", Intent: "incident_triage", Query: "old issue", Answer: "old answer", EvidenceIDs: "[]", Limitations: "[]", CreatedAt: time.Now()}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	findings, err := NewEpisodicMemory(db).Recall(context.Background(), "api", "no lexical match", 3)
	if err != nil || len(findings) != 1 || len(findings[0].Evidence) != 1 {
		t.Fatalf("findings=%+v err=%v", findings, err)
	}
	metadata := findings[0].Evidence[0].Metadata
	if metadata["mode"] != "recent" || metadata["memory"] != true || metadata["not_runtime_evidence"] != true {
		t.Fatalf("recent memory provenance missing: %+v", metadata)
	}
}

type episodicEmbedderStub struct{}

func (episodicEmbedderStub) Embed(context.Context, string) ([]float64, error) {
	return []float64{.1, .9}, nil
}

func TestEpisodicHybridRunsKeywordAndVectorAndReturnsSources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(`{"version":{"number":"8.12.0"},"tagline":"You Know, for Search"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		text, _ := json.Marshal(body)
		score := 4.0
		if strings.Contains(string(text), "script_score") {
			score = 1.8
		}
		_, _ = w.Write([]byte(`{"hits":{"hits":[{"_id":"1","_score":` + jsonNumber(score) + `,"_source":{"incident_id":1,"service":"api","intent":"incident_triage","query":"ERR_deadbeef","answer":"fixed","created_at":"2026-08-26T00:00:00Z"}}]}}`))
	}))
	defer server.Close()
	es, err := pkgelastic.New(context.Background(), config.Elasticsearch{Addresses: []string{server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	db := episodicTestDB(t)
	if err := db.Create(&EpisodicIncident{ID: 1, UserID: "u", SessionID: "s", Service: "api", Intent: "incident_triage", Query: "ERR_deadbeef", Answer: "fixed", EvidenceIDs: "[]", Limitations: "[]", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	memory := NewEpisodicMemoryWithOptions(db, EpisodicOptions{Mode: "hybrid", Index: "episodic", TopK: 3, ES: es, Embedder: episodicEmbedderStub{}, Dim: 2})
	findings, err := memory.Recall(context.Background(), "api", "ERR_deadbeef", 3)
	if err != nil || len(findings) != 1 || len(findings[0].Evidence) == 0 {
		t.Fatalf("findings=%+v err=%v", findings, err)
	}
	sources, ok := findings[0].Evidence[0].Metadata["recall_sources"].([]string)
	if !ok || len(sources) != 2 || sources[0] != "keyword" || sources[1] != "vector" {
		t.Fatalf("hybrid sources=%#v metadata=%+v", sources, findings[0].Evidence[0].Metadata)
	}
}

func jsonNumber(value float64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
