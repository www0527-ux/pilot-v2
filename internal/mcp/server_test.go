package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shanyangcode.com/pilot/pkg/types"
)

func TestToolsListAndCallReadOnly(t *testing.T) {
	server := NewServer(Deps{
		APIKey: "secret",
		Alerts: func(context.Context, map[string]string) ([]types.AlertInfo, error) {
			return []types.AlertInfo{{Name: "A", Status: "firing"}}, nil
		},
		Logs: func(context.Context, string, string, string, time.Time, time.Time, int) ([]types.LogEntry, error) {
			return nil, nil
		},
		Metrics: func(context.Context, string, time.Time, time.Time) ([]types.MetricData, error) {
			return nil, nil
		},
		Traces: func(context.Context, string) ([]types.TraceSpan, error) {
			return nil, nil
		},
		Knowledge: func(context.Context, string) ([]types.KnowledgeDocument, error) {
			return nil, nil
		},
	})

	listBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(listBody))
	req.Header.Set("X-API-Key", "secret")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query_alerts","arguments":{"service":"api"}}}`
	req = httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(callBody))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("call status = %d", rec.Code)
	}
	var resp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
}

func TestUnauthorized(t *testing.T) {
	server := NewServer(Deps{APIKey: "secret", Alerts: func(context.Context, map[string]string) ([]types.AlertInfo, error) { return nil, nil }})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestStandardInitializeSchemasAndFixedReadOnlyAllowlist(t *testing.T) {
	server := NewServer(Deps{})
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(initialize))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	var initResp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &initResp); err != nil || initResp.Error != nil {
		t.Fatalf("initialize response=%s err=%v", rec.Body.String(), err)
	}
	result := initResp.Result.(map[string]interface{})
	if result["protocolVersion"] == "" || result["capabilities"] == nil || result["serverInfo"] == nil {
		t.Fatalf("incomplete initialize result: %+v", result)
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	var list struct {
		Result struct {
			Tools []ToolSpec `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Result.Tools) != 6 {
		t.Fatalf("tool count=%d: %+v", len(list.Result.Tools), list.Result.Tools)
	}
	for index, spec := range list.Result.Tools {
		if spec.Name != readOnlyAllowlist[index] || spec.InputSchema["type"] != "object" || spec.Annotations["readOnlyHint"] != true || spec.Annotations["destructiveHint"] != false {
			t.Fatalf("invalid read-only schema at %d: %+v", index, spec)
		}
	}
}

func TestInvalidToolArgumentsReturnProtocolInvalidParams(t *testing.T) {
	server := NewServer(Deps{Logs: func(context.Context, string, string, string, time.Time, time.Time, int) ([]types.LogEntry, error) {
		return nil, nil
	}})
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"query_logs","arguments":{"level":"verbose","duration_minutes":99999}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	var response rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != -32602 {
		t.Fatalf("expected -32602: %s", rec.Body.String())
	}
}

func TestQueryLogsUsesSharedSummaryAndAllLevel(t *testing.T) {
	seenLevel := ""
	server := NewServer(Deps{Logs: func(_ context.Context, _, level, _ string, _, _ time.Time, _ int) ([]types.LogEntry, error) {
		seenLevel = level
		return []types.LogEntry{{Level: "warning", Service: "api", Message: strings.Repeat("中文🙂", 200)}}, nil
	}})
	body := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"query_logs","arguments":{"level":"all","output_rune_budget":256}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if seenLevel != "all" || !strings.Contains(rec.Body.String(), `\"truncated\":true`) || !strings.Contains(rec.Body.String(), `\"original_count\":1`) {
		t.Fatalf("shared log contract missing: level=%q body=%s", seenLevel, rec.Body.String())
	}
}
