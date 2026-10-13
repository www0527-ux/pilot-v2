// Package mcp implements Pilot's standard, structurally read-only MCP server.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"shanyangcode.com/pilot/internal/observability"
	"shanyangcode.com/pilot/internal/observability/correlate"
	"shanyangcode.com/pilot/pkg/types"
)

const protocolVersion = "2025-03-26"

var readOnlyAllowlist = []string{"correlate_signals", "query_alerts", "query_logs", "query_metrics", "query_traces", "search_knowledge"}

type ToolFunc func(ctx context.Context, args map[string]any) (any, error)

type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
}

type Server struct {
	tools map[string]toolEntry
	key   string
}

type toolEntry struct {
	spec ToolSpec
	fn   ToolFunc
}

type Deps struct {
	APIKey      string
	Alerts      func(ctx context.Context, filters map[string]string) ([]types.AlertInfo, error)
	Logs        func(ctx context.Context, service, level, keyword string, start, end time.Time, limit int) ([]types.LogEntry, error)
	Metrics     func(ctx context.Context, query string, start, end time.Time) ([]types.MetricData, error)
	Traces      func(ctx context.Context, traceID string) ([]types.TraceSpan, error)
	Knowledge   func(ctx context.Context, query string) ([]types.KnowledgeDocument, error)
	Correlate   *correlate.Engine
	ServiceName string
}

func NewServer(deps Deps) *Server {
	s := &Server{tools: map[string]toolEntry{}, key: strings.TrimSpace(deps.APIKey)}
	s.register("query_alerts", "Query current alerts (read-only).", objectSchema(map[string]any{
		"service": stringProperty("Service filter"), "severity": stringEnumProperty("Severity filter", "critical", "warning", "info"),
	}, nil), func(ctx context.Context, args map[string]any) (any, error) {
		if deps.Alerts == nil {
			return nil, errors.New("alerts source unavailable")
		}
		service, err := observability.NormalizeService(stringArg(args, "service"), "")
		if err != nil {
			return nil, invalidParams(err.Error())
		}
		severity, err := observability.NormalizeSeverity(stringArg(args, "severity"))
		if err != nil {
			return nil, invalidParams(err.Error())
		}
		filters := map[string]string{}
		if service != "" {
			filters["service"] = service
		}
		if severity != "" {
			filters["severity"] = severity
		}
		return deps.Alerts(ctx, filters)
	})
	s.register("query_logs", "Query and summarize logs with a bounded output budget (read-only).", objectSchema(map[string]any{
		"service":            stringProperty("Service filter"),
		"level":              stringEnumProperty("Log level; warning aliases warn", "all", "debug", "info", "warn", "warning", "error", "fatal"),
		"keyword":            stringProperty("Optional log keyword"),
		"duration_minutes":   integerProperty("Time window in minutes", 1, observability.MaxDurationMins),
		"limit":              integerProperty("Maximum collected entries", 1, observability.MaxLogLimit),
		"output_rune_budget": integerProperty("Rune budget for returned log text", observability.MinLogOutputRunes, observability.MaxLogOutputRunes),
	}, nil), func(ctx context.Context, args map[string]any) (any, error) {
		if deps.Logs == nil {
			return nil, errors.New("logs source unavailable")
		}
		service, err := observability.NormalizeService(stringArg(args, "service"), deps.ServiceName)
		if err != nil {
			return nil, invalidParams(err.Error())
		}
		level, err := observability.NormalizeLogLevel(stringArg(args, "level"), "all")
		if err != nil {
			return nil, invalidParams(err.Error())
		}
		keyword, err := observability.NormalizeLogKeyword(stringArg(args, "keyword"))
		if err != nil {
			return nil, invalidParams(err.Error())
		}
		duration, err := boundedIntArg(args, "duration_minutes", 30, 1, observability.MaxDurationMins)
		if err != nil {
			return nil, err
		}
		limit, err := boundedIntArg(args, "limit", 20, 1, observability.MaxLogLimit)
		if err != nil {
			return nil, err
		}
		budget, err := boundedIntArg(args, "output_rune_budget", observability.DefaultLogOutputRunes, observability.MinLogOutputRunes, observability.MaxLogOutputRunes)
		if err != nil {
			return nil, err
		}
		end := time.Now()
		start := end.Add(-time.Duration(duration) * time.Minute)
		logs, err := deps.Logs(ctx, service, level, keyword, start, end, limit)
		if err != nil {
			return nil, err
		}
		return observability.SummarizeLogs(logs, budget), nil
	})
	s.register("query_metrics", "Query Prometheus metrics (read-only).", objectSchema(map[string]any{
		"query": stringProperty("PromQL query"), "duration_minutes": integerProperty("Time window in minutes", 1, observability.MaxDurationMins),
	}, []string{"query"}), func(ctx context.Context, args map[string]any) (any, error) {
		if deps.Metrics == nil {
			return nil, errors.New("metrics source unavailable")
		}
		query, err := observability.NormalizePromQL(stringArg(args, "query"))
		if err != nil {
			return nil, invalidParams(err.Error())
		}
		duration, err := boundedIntArg(args, "duration_minutes", 30, 1, observability.MaxDurationMins)
		if err != nil {
			return nil, err
		}
		end := time.Now()
		start := end.Add(-time.Duration(duration) * time.Minute)
		return deps.Metrics(ctx, query, start, end)
	})
	s.register("query_traces", "Query one distributed trace (read-only).", objectSchema(map[string]any{
		"trace_id": stringProperty("Trace ID"),
	}, []string{"trace_id"}), func(ctx context.Context, args map[string]any) (any, error) {
		if deps.Traces == nil {
			return nil, errors.New("trace source unavailable")
		}
		traceID, err := observability.NormalizeTraceID(stringArg(args, "trace_id"))
		if err != nil {
			return nil, invalidParams(err.Error())
		}
		return deps.Traces(ctx, traceID)
	})
	s.register("search_knowledge", "Search relevant knowledge chunks (read-only).", objectSchema(map[string]any{
		"query": stringProperty("Knowledge search query"),
	}, []string{"query"}), func(ctx context.Context, args map[string]any) (any, error) {
		if deps.Knowledge == nil {
			return nil, errors.New("knowledge source unavailable")
		}
		query := strings.TrimSpace(stringArg(args, "query"))
		if query == "" {
			return nil, invalidParams("query is required")
		}
		return deps.Knowledge(ctx, query)
	})
	s.register("correlate_signals", "Correlate observed alerts/logs/metrics (read-only heuristic).", objectSchema(map[string]any{
		"service": stringProperty("Service"), "duration_minutes": integerProperty("Time window in minutes", 1, observability.MaxDurationMins),
		"metric_query": stringProperty("PromQL query; defaults to up"),
	}, nil), func(ctx context.Context, args map[string]any) (any, error) {
		if deps.Correlate == nil {
			return nil, errors.New("correlation source unavailable")
		}
		service, err := observability.NormalizeService(stringArg(args, "service"), deps.ServiceName)
		if err != nil {
			return nil, invalidParams(err.Error())
		}
		duration, err := boundedIntArg(args, "duration_minutes", 30, 1, observability.MaxDurationMins)
		if err != nil {
			return nil, err
		}
		metric := stringArg(args, "metric_query")
		if metric == "" {
			metric = "up"
		}
		metric, err = observability.NormalizePromQL(metric)
		if err != nil {
			return nil, invalidParams(err.Error())
		}
		return deps.Correlate.Correlate(ctx, correlate.Request{Service: service, Window: time.Duration(duration) * time.Minute, MetricQuery: metric})
	})
	return s
}

func (s *Server) register(name, description string, schema map[string]any, fn ToolFunc) {
	if !isAllowedTool(name) {
		panic("attempted to register non-read-only MCP tool: " + name)
	}
	s.tools[name] = toolEntry{spec: ToolSpec{Name: name, Description: description, InputSchema: schema, Annotations: map[string]any{
		"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false,
	}}, fn: fn}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}
type paramsError struct{ message string }

func (e paramsError) Error() string      { return e.message }
func invalidParams(message string) error { return paramsError{message: message} }

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			return
		}
		var req rpcRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			return
		}
		if req.JSONRPC != "2.0" || strings.TrimSpace(req.Method) == "" {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32600, Message: "invalid request"}})
			return
		}
		if req.Method == "notifications/initialized" && req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, s.dispatch(r.Context(), req))
	})
}

func (s *Server) authorized(r *http.Request) bool {
	if s.key == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.Header.Get("X-API-Key")
	}
	return got == s.key
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string         `json:"protocolVersion"`
			ClientInfo      map[string]any `json:"clientInfo"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.ProtocolVersion == "" {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			return resp
		}
		resp.Result = map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "pilot-readonly", "version": "1.0.0"}}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		list := make([]ToolSpec, 0, len(s.tools))
		for _, name := range readOnlyAllowlist {
			list = append(list, s.tools[name].spec)
		}
		resp.Result = map[string]any{"tools": list}
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			return resp
		}
		entry, ok := s.tools[params.Name]
		if !ok || !isAllowedTool(params.Name) {
			resp.Error = &rpcError{Code: -32601, Message: "tool not found"}
			return resp
		}
		if params.Arguments == nil {
			params.Arguments = map[string]any{}
		}
		out, err := entry.fn(ctx, params.Arguments)
		if err != nil {
			var invalid paramsError
			if errors.As(err, &invalid) {
				resp.Error = &rpcError{Code: -32602, Message: "invalid params", Data: invalid.Error()}
				return resp
			}
			resp.Result = toolErrorResult("tool execution failed")
			return resp
		}
		raw, err := json.Marshal(out)
		if err != nil {
			resp.Result = toolErrorResult("tool output encoding failed")
			return resp
		}
		resp.Result = map[string]any{"content": []map[string]string{{"type": "text", "text": string(raw)}}, "isError": false}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	return resp
}

func toolErrorResult(message string) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": message}}, "isError": true}
}
func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return http.ListenAndServe(addr, mux)
}

func isAllowedTool(name string) bool {
	index := sort.SearchStrings(readOnlyAllowlist, name)
	return index < len(readOnlyAllowlist) && readOnlyAllowlist[index] == name
}
func objectSchema(properties map[string]any, required []string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
func stringEnumProperty(description string, values ...string) map[string]any {
	result := stringProperty(description)
	result["enum"] = values
	return result
}
func integerProperty(description string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": description, "minimum": min, "maximum": max}
}
func stringArg(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}
func boundedIntArg(args map[string]any, key string, defaultValue, min, max int) (int, error) {
	raw, ok := args[key]
	if !ok {
		return defaultValue, nil
	}
	var value int
	switch typed := raw.(type) {
	case float64:
		if typed != float64(int(typed)) {
			return 0, invalidParams(key + " must be an integer")
		}
		value = int(typed)
	case int:
		value = typed
	default:
		return 0, invalidParams(key + " must be an integer")
	}
	if value < min || value > max {
		return 0, invalidParams(fmt.Sprintf("%s must be in [%d,%d]", key, min, max))
	}
	return value, nil
}
