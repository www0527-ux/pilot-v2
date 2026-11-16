package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	es "github.com/elastic/go-elasticsearch/v8"
)

func TestExtractSearchHitsPreservesIDAndScore(t *testing.T) {
	result := map[string]interface{}{
		"hits": map[string]interface{}{
			"hits": []interface{}{
				map[string]interface{}{
					"_id":    "knowledge-1",
					"_score": 4.2,
					"_source": map[string]interface{}{
						"title": "runbook",
					},
				},
			},
		},
	}

	hits, err := extractSearchHits(result)
	if err != nil {
		t.Fatalf("extractSearchHits() error = %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "knowledge-1" || hits[0].Score != 4.2 {
		t.Fatalf("unexpected hits: %#v", hits)
	}
}

func newBulkTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := es.NewClient(es.Config{Addresses: []string{server.URL}, DisableRetry: true})
	if err != nil {
		t.Fatal(err)
	}
	return &Client{es: client}
}

func readBulkTestDocuments(t *testing.T, r *http.Request) []BulkDocument {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/_bulk" || r.URL.Query().Get("refresh") != "true" {
		t.Errorf("unexpected bulk request: %s %s", r.Method, r.URL)
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines)%2 != 0 {
		t.Fatalf("invalid bulk body: %s", data)
	}
	var documents []BulkDocument
	for i := 0; i < len(lines); i += 2 {
		var header struct {
			Index struct {
				ID    string `json:"_id"`
				Index string `json:"_index"`
			} `json:"index"`
		}
		if err := json.Unmarshal(lines[i], &header); err != nil {
			t.Fatal(err)
		}
		if header.Index.Index != "knowledge" || header.Index.ID == "" || !json.Valid(lines[i+1]) {
			t.Fatalf("invalid bulk operation: %s %s", lines[i], lines[i+1])
		}
		documents = append(documents, BulkDocument{ID: header.Index.ID, Document: json.RawMessage(lines[i+1])})
	}
	return documents
}

func writeBulkTestResponse(t *testing.T, w http.ResponseWriter, documents []BulkDocument, statuses []int) {
	t.Helper()
	items := make([]map[string]any, len(documents))
	hasErrors := false
	for i, document := range documents {
		result := map[string]any{"_id": document.ID, "status": statuses[i]}
		if statuses[i] >= 300 {
			hasErrors = true
			errorType := "es_rejected_execution_exception"
			if statuses[i] == http.StatusBadRequest {
				errorType = "mapper_parsing_exception"
			}
			result["error"] = map[string]string{"type": errorType, "reason": "injected failure"}
		}
		items[i] = map[string]any{"index": result}
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"errors": hasErrors, "items": items}); err != nil {
		t.Error(err)
	}
}

func TestBulkIndexDocumentsRetriesOnlyFailedRecordsWithOriginalContent(t *testing.T) {
	documents := []BulkDocument{
		{ID: "doc-0000", Document: map[string]any{"record_type": "chunk", "content": "first"}},
		{ID: "doc-0001", Document: map[string]any{"record_type": "chunk", "content": "second"}},
		{ID: "doc-section-0000", Document: map[string]any{"record_type": "section", "content": "whole section"}},
	}
	var requests [][]string
	original := map[string]string{}
	client := newBulkTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		batch := readBulkTestDocuments(t, r)
		ids := make([]string, len(batch))
		for i, document := range batch {
			ids[i] = document.ID
			payload := string(document.Document.(json.RawMessage))
			if len(requests) == 0 {
				original[document.ID] = payload
			} else if payload != original[document.ID] {
				t.Errorf("retry changed document %s: %s", document.ID, payload)
			}
		}
		requests = append(requests, ids)
		switch len(requests) {
		case 1:
			// Even a mutable caller payload must not change a later retry.
			documents[1].Document.(map[string]any)["content"] = "changed after submission"
			writeBulkTestResponse(t, w, batch, []int{201, 429, 503})
		case 2:
			writeBulkTestResponse(t, w, batch, []int{201, 429})
		case 3:
			writeBulkTestResponse(t, w, batch, []int{201})
		default:
			t.Errorf("unexpected attempt %d", len(requests))
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	indexed, err := client.BulkIndexDocuments(context.Background(), "knowledge", documents)
	wantRequests := [][]string{{"doc-0000", "doc-0001", "doc-section-0000"}, {"doc-0001", "doc-section-0000"}, {"doc-section-0000"}}
	if err != nil || !reflect.DeepEqual(requests, wantRequests) || !reflect.DeepEqual(indexed, wantRequests[0]) {
		t.Fatalf("requests=%v indexed=%v err=%v", requests, indexed, err)
	}
}

func TestBulkIndexDocumentsPermanentFailureAndExhaustion(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		attempts int
	}{
		{name: "mapping", status: 400, attempts: 1},
		{name: "exhausted", status: 429, attempts: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			client := newBulkTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				batch := readBulkTestDocuments(t, r)
				attempts++
				if attempts == 1 {
					writeBulkTestResponse(t, w, batch, []int{201, test.status})
				} else {
					if len(batch) != 1 || batch[0].ID != "failed" {
						t.Errorf("successful record was resubmitted: %+v", batch)
					}
					writeBulkTestResponse(t, w, batch, []int{test.status})
				}
			})
			indexed, err := client.BulkIndexDocuments(context.Background(), "knowledge", []BulkDocument{
				{ID: "accepted", Document: "first"}, {ID: "failed", Document: "second"},
			})
			if err == nil || attempts != test.attempts || !reflect.DeepEqual(indexed, []string{"accepted"}) || !strings.Contains(err.Error(), `"failed"`) || !strings.Contains(err.Error(), "injected failure") {
				t.Fatalf("attempts=%d indexed=%v err=%v", attempts, indexed, err)
			}
		})
	}
}

func TestBulkIndexDocumentsHTTPStatusRetries(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504, 400, 401, 501} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			attempts := 0
			client := newBulkTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				batch := readBulkTestDocuments(t, r)
				attempts++
				if attempts == 1 {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"reason":"request failed"}}`)
					return
				}
				writeBulkTestResponse(t, w, batch, []int{201})
			})
			indexed, err := client.BulkIndexDocuments(context.Background(), "knowledge", []BulkDocument{{ID: "doc", Document: "content"}})
			wantRetry := status == 429 || status == 500 || status == 502 || status == 503 || status == 504
			if wantRetry {
				if err != nil || attempts != 2 || len(indexed) != 1 {
					t.Fatalf("attempts=%d indexed=%v err=%v", attempts, indexed, err)
				}
			} else if err == nil || attempts != 1 {
				t.Fatalf("permanent HTTP error retried: attempts=%d err=%v", attempts, err)
			}
		})
	}
}

func TestBulkIndexDocumentsCancellationStopsRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	client := newBulkTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		batch := readBulkTestDocuments(t, r)
		attempts++
		writeBulkTestResponse(t, w, batch, []int{429})
		time.AfterFunc(10*time.Millisecond, cancel)
	})
	_, err := client.BulkIndexDocuments(ctx, "knowledge", []BulkDocument{{ID: "doc", Document: "content"}})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestBulkIndexDocumentsRejectsIncompleteOrMismatchedResults(t *testing.T) {
	for _, response := range []string{
		`{"errors":false,"items":[]}`,
		`{"errors":false,"items":[{"index":{"_id":"other","status":201}}]}`,
		`{"errors":false,"items":[{"delete":{"_id":"doc","status":200}}]}`,
		`{"errors":true,"items":[{"index":{"_id":"doc","status":201}}]}`,
		`{"errors":false,"items":[{"index":{"_id":"doc","status":400,"error":{"type":"mapper_parsing_exception"}}}]}`,
	} {
		t.Run(response, func(t *testing.T) {
			attempts := 0
			client := newBulkTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				attempts++
				_, _ = io.WriteString(w, response)
			})
			_, err := client.BulkIndexDocuments(context.Background(), "knowledge", []BulkDocument{{ID: "doc", Document: "content"}})
			if err == nil || attempts != 1 {
				t.Fatalf("attempts=%d err=%v", attempts, err)
			}
		})
	}
}
