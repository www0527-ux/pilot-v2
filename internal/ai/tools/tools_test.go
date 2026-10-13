package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"shanyangcode.com/pilot/pkg/types"
)

type toolSourceStub struct{}

func (*toolSourceStub) GetAlerts(context.Context, map[string]string) ([]types.AlertInfo, error) {
	return []types.AlertInfo{{Name: "HighErrorRate", Severity: "critical", Status: "firing", StartTime: time.Now()}}, nil
}

func (*toolSourceStub) QueryMetrics(context.Context, string, time.Time, time.Time) ([]types.MetricData, error) {
	return []types.MetricData{{Name: "up", Value: 1, Timestamp: time.Now()}}, nil
}

func (*toolSourceStub) QueryLogs(context.Context, string, string, string, time.Time, time.Time, int) ([]types.LogEntry, error) {
	return []types.LogEntry{{Timestamp: time.Now(), Level: "error", Service: "pilot-api", Message: "request failed"}}, nil
}

func (*toolSourceStub) GetTrace(context.Context, string) ([]types.TraceSpan, error) {
	return []types.TraceSpan{{TraceID: "trace-1", SpanID: "span-1", Service: "pilot-api", Operation: "request"}}, nil
}

func (*toolSourceStub) GetServiceTopology(context.Context, string) (string, error) {
	return "pilot-api -> redis", nil
}

type knowledgeSourceStub struct{}

func (knowledgeSourceStub) Search(context.Context, string) ([]types.KnowledgeDocument, error) {
	return []types.KnowledgeDocument{{ID: "runbook-1", Title: "Timeout runbook", Content: "Inspect the connection pool.", Category: "database"}}, nil
}

func TestToolBuildersUseNarrowInjectedSources(t *testing.T) {
	source := &toolSourceStub{}
	cases := []struct {
		name  string
		input string
		build func() (tool.InvokableTool, error)
	}{
		{name: "query_alerts", input: `{"status":"firing"}`, build: func() (tool.InvokableTool, error) { return NewQueryAlertsTool(source) }},
		{name: "query_metrics", input: `{"query":"up"}`, build: func() (tool.InvokableTool, error) { return NewQueryMetricsTool(source) }},
		{name: "query_logs", input: `{"level":"error"}`, build: func() (tool.InvokableTool, error) { return NewQueryLogsTool(source) }},
		{name: "query_traces", input: `{"trace_id":"0123456789abcdef0123456789abcdef"}`, build: func() (tool.InvokableTool, error) { return NewQueryTracesTool(source) }},
		{name: "analyze_anomaly", input: `{"metric":"up"}`, build: func() (tool.InvokableTool, error) { return NewAnalyzeAnomalyTool(source) }},
		{name: "get_service_topology", input: `{}`, build: func() (tool.InvokableTool, error) { return NewGetServiceTopologyTool(source) }},
		{name: "get_current_time", input: `{}`, build: NewGetCurrentTimeTool},
		{name: "search_knowledge", input: `{"query":"timeout"}`, build: func() (tool.InvokableTool, error) { return NewSearchKnowledgeTool(knowledgeSourceStub{}) }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			built, err := testCase.build()
			if err != nil {
				t.Fatalf("build tool: %v", err)
			}

			info, err := built.Info(context.Background())
			if err != nil {
				t.Fatalf("tool info: %v", err)
			}
			if info.Name != testCase.name {
				t.Fatalf("tool name = %q, want %q", info.Name, testCase.name)
			}

			response, err := built.InvokableRun(context.Background(), testCase.input)
			if err != nil {
				t.Fatalf("invoke tool: %v", err)
			}
			var output struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal([]byte(response), &output); err != nil {
				t.Fatalf("decode response %q: %v", response, err)
			}
			if !output.Success {
				t.Fatalf("tool response is not successful: %s", response)
			}
		})
	}
}

func TestToolBuildersRejectMissingSources(t *testing.T) {
	var nilSource *toolSourceStub
	cases := []struct {
		name  string
		build func() (tool.InvokableTool, error)
	}{
		{name: "query_alerts", build: func() (tool.InvokableTool, error) { return NewQueryAlertsTool(nilSource) }},
		{name: "query_metrics", build: func() (tool.InvokableTool, error) { return NewQueryMetricsTool(nilSource) }},
		{name: "query_logs", build: func() (tool.InvokableTool, error) { return NewQueryLogsTool(nilSource) }},
		{name: "query_traces", build: func() (tool.InvokableTool, error) { return NewQueryTracesTool(nilSource) }},
		{name: "analyze_anomaly", build: func() (tool.InvokableTool, error) { return NewAnalyzeAnomalyTool(nilSource) }},
		{name: "get_service_topology", build: func() (tool.InvokableTool, error) { return NewGetServiceTopologyTool(nilSource) }},
		{name: "search_knowledge", build: func() (tool.InvokableTool, error) { return NewSearchKnowledgeTool(nil) }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := testCase.build(); err == nil {
				t.Fatal("expected missing source error")
			}
		})
	}
}
