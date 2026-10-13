package multiagent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"shanyangcode.com/pilot/internal/ai/retriever"
	"shanyangcode.com/pilot/pkg/types"
)

type scriptedEvidenceModel struct {
	mu        sync.Mutex
	responses []*schema.Message
	err       error
	loop      bool
	calls     int
	toolNames []string
}

func (m *scriptedEvidenceModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	if m.loop {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: fmt.Sprintf("loop-%d", m.calls),
			Function: schema.FunctionCall{
				Name:      "query_metrics",
				Arguments: `{"query":"up"}`,
			},
		}}), nil
	}
	index := m.calls - 1
	if index >= len(m.responses) {
		index = len(m.responses) - 1
	}
	if index < 0 {
		return schema.AssistantMessage("no evidence", nil), nil
	}
	return m.responses[index], nil
}

func (m *scriptedEvidenceModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (m *scriptedEvidenceModel) WithTools(toolInfos []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolNames = m.toolNames[:0]
	for _, info := range toolInfos {
		m.toolNames = append(m.toolNames, info.Name)
	}
	return m, nil
}

func (m *scriptedEvidenceModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *scriptedEvidenceModel) registeredTools() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.toolNames...)
}

type reactEvidenceSourceStub struct {
	mu            sync.Mutex
	alertFilters  []map[string]string
	logServices   []string
	metricQueries []string
	alertsErr     error
	logsErr       error
	metricsErr    error
}

func (s *reactEvidenceSourceStub) GetAlerts(_ context.Context, filters map[string]string) ([]types.AlertInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copyFilters := make(map[string]string, len(filters))
	for key, value := range filters {
		copyFilters[key] = value
	}
	s.alertFilters = append(s.alertFilters, copyFilters)
	if s.alertsErr != nil {
		return nil, s.alertsErr
	}
	return []types.AlertInfo{{Name: "HighErrorRate", Severity: "critical", Status: "firing", StartTime: time.Now()}}, nil
}

func (s *reactEvidenceSourceStub) QueryMetrics(_ context.Context, query string, _, _ time.Time) ([]types.MetricData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metricQueries = append(s.metricQueries, query)
	if s.metricsErr != nil {
		return nil, s.metricsErr
	}
	return []types.MetricData{{Name: query, Value: 1, Timestamp: time.Now()}}, nil
}

func (s *reactEvidenceSourceStub) QueryLogs(_ context.Context, service, _, _ string, _, _ time.Time, _ int) ([]types.LogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logServices = append(s.logServices, service)
	if s.logsErr != nil {
		return nil, s.logsErr
	}
	return []types.LogEntry{{Timestamp: time.Now(), Service: service, Level: "error", Message: "request timeout"}}, nil
}

func TestReactEvidenceExecutorCollectsOnlyRestrictedToolEvidence(t *testing.T) {
	model := &scriptedEvidenceModel{responses: []*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{
			{ID: "alerts-1", Function: schema.FunctionCall{Name: "query_alerts", Arguments: `{"service":"checkout","status":"firing"}`}},
			{ID: "logs-1", Function: schema.FunctionCall{Name: "query_logs", Arguments: `{"service":"checkout","level":"error"}`}},
			{ID: "metrics-1", Function: schema.FunctionCall{Name: "query_metrics", Arguments: `{"query":"up"}`}},
		}),
		schema.AssistantMessage("Observed alert, error log, and metric evidence for checkout.", nil),
	}}
	source := &reactEvidenceSourceStub{}
	executor, err := NewReactEvidenceExecutor(context.Background(), source, model, 8)
	if err != nil {
		t.Fatalf("NewReactEvidenceExecutor() error = %v", err)
	}

	finding, err := executor.Execute(context.Background(), "investigate checkout", Plan{Service: "checkout"})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if finding.Summary != "Observed alert, error log, and metric evidence for checkout." {
		t.Fatalf("summary = %q", finding.Summary)
	}
	if got := finding.Metadata["mode"]; got != EvidenceModeReact {
		t.Fatalf("mode = %#v, want %q", got, EvidenceModeReact)
	}
	if len(finding.Evidence) != 3 {
		t.Fatalf("evidence count = %d, want 3: %#v", len(finding.Evidence), finding.Evidence)
	}

	actualTools := model.registeredTools()
	sort.Strings(actualTools)
	wantTools := []string{"query_alerts", "query_logs", "query_metrics"}
	if fmt.Sprint(actualTools) != fmt.Sprint(wantTools) {
		t.Fatalf("registered tools = %#v, want %#v", actualTools, wantTools)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if len(source.alertFilters) != 1 || source.alertFilters[0]["service"] != "checkout" {
		t.Fatalf("alert filters = %#v", source.alertFilters)
	}
	if len(source.logServices) != 1 || source.logServices[0] != "checkout" {
		t.Fatalf("log services = %#v", source.logServices)
	}
	if len(source.metricQueries) != 1 || source.metricQueries[0] != "up" {
		t.Fatalf("metric queries = %#v", source.metricQueries)
	}
	for _, item := range finding.Evidence {
		if item.Source != "tool:query_alerts" && item.Source != "tool:query_logs" && item.Source != "tool:query_metrics" {
			t.Fatalf("unexpected evidence source %q", item.Source)
		}
		if _, ok := item.Metadata["arguments"].(string); !ok {
			t.Fatalf("arguments metadata missing from %#v", item)
		}
	}
}

func TestReactEvidenceExecutorHonorsStepLimit(t *testing.T) {
	model := &scriptedEvidenceModel{loop: true}
	executor, err := NewReactEvidenceExecutor(context.Background(), &reactEvidenceSourceStub{}, model, 2)
	if err != nil {
		t.Fatalf("NewReactEvidenceExecutor() error = %v", err)
	}

	if _, err := executor.Execute(context.Background(), "inspect metrics", Plan{Service: "checkout"}); err == nil {
		t.Fatal("expected step limit to terminate looping model")
	}
	if calls := model.callCount(); calls > 2 {
		t.Fatalf("model calls = %d, exceeded configured max steps", calls)
	}
}

func TestReactEvidenceExecutorRejectsAllFailedToolCalls(t *testing.T) {
	model := &scriptedEvidenceModel{responses: []*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{
			{ID: "alerts-1", Function: schema.FunctionCall{Name: "query_alerts", Arguments: `{"service":"checkout"}`}},
			{ID: "logs-1", Function: schema.FunctionCall{Name: "query_logs", Arguments: `{"service":"checkout"}`}},
		}),
		schema.AssistantMessage("The tools failed, but this text claims a root cause.", nil),
	}}
	source := &reactEvidenceSourceStub{
		alertsErr: errors.New("alerts unavailable"),
		logsErr:   errors.New("logs unavailable"),
	}
	executor, err := NewReactEvidenceExecutor(context.Background(), source, model, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), "investigate", Plan{Service: "checkout"}); err == nil {
		t.Fatal("failed tool responses must not become runtime evidence")
	}
}

func TestReactEvidenceExecutorMarksPartialToolFailure(t *testing.T) {
	model := &scriptedEvidenceModel{responses: []*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{
			{ID: "alerts-1", Function: schema.FunctionCall{Name: "query_alerts", Arguments: `{"service":"checkout"}`}},
			{ID: "logs-1", Function: schema.FunctionCall{Name: "query_logs", Arguments: `{"service":"checkout"}`}},
		}),
		schema.AssistantMessage("Alert evidence is available; logs failed.", nil),
	}}
	executor, err := NewReactEvidenceExecutor(context.Background(), &reactEvidenceSourceStub{logsErr: errors.New("logs unavailable")}, model, 8)
	if err != nil {
		t.Fatal(err)
	}
	finding, err := executor.Execute(context.Background(), "investigate", Plan{Service: "checkout"})
	if err != nil {
		t.Fatal(err)
	}
	if !metadataBool(finding.Metadata, "partial_failure") || metadataInt(finding.Metadata, "tool_error_count") != 1 {
		t.Fatalf("metadata = %+v", finding.Metadata)
	}
}

func TestReactEvidenceExecutorRejectsTypedNilSource(t *testing.T) {
	var source *reactEvidenceSourceStub
	if _, err := NewReactEvidenceExecutor(context.Background(), source, &scriptedEvidenceModel{}, 8); err == nil {
		t.Fatal("expected typed nil source to be rejected")
	}
}

func TestDefaultOrchestratorFallsBackToDeterministicEvidence(t *testing.T) {
	model := &scriptedEvidenceModel{err: errors.New("model unavailable")}
	orchestrator, err := NewDefaultOrchestrator(context.Background(), OrchestratorDeps{
		Collector:          &reactEvidenceSourceStub{},
		KnowledgeRetriever: emptyKnowledgeSource{},
		SynthesisModel:     model,
		ServiceName:        "pilot-api",
		EvidenceMode:       EvidenceModeReact,
		ReactMaxSteps:      2,
	})
	if err != nil {
		t.Fatalf("NewDefaultOrchestrator() error = %v", err)
	}

	result, err := orchestrator.Execute(context.Background(), "investigate timeout service=checkout")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, finding := range result.Findings {
		if finding.Role != RoleEvidence {
			continue
		}
		if got := finding.Metadata["mode"]; got != "deterministic_fallback" {
			t.Fatalf("fallback mode = %#v", got)
		}
		if _, ok := finding.Metadata["react_error"].(string); !ok {
			t.Fatalf("fallback metadata = %#v", finding.Metadata)
		}
		return
	}
	t.Fatalf("evidence finding missing: %#v", result.Findings)
}

func TestDefaultOrchestratorUsesDeterministicEvidenceByDefault(t *testing.T) {
	model := &scriptedEvidenceModel{responses: []*schema.Message{schema.AssistantMessage("Deterministic evidence summary", nil)}}
	orchestrator, err := NewDefaultOrchestrator(context.Background(), OrchestratorDeps{
		Collector:          &reactEvidenceSourceStub{},
		KnowledgeRetriever: emptyKnowledgeSource{},
		SynthesisModel:     model,
		ServiceName:        "pilot-api",
	})
	if err != nil {
		t.Fatalf("NewDefaultOrchestrator() error = %v", err)
	}

	result, err := orchestrator.Execute(context.Background(), "investigate timeout service=checkout")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, finding := range result.Findings {
		if finding.Role == RoleEvidence {
			if got := finding.Metadata["mode"]; got != EvidenceModeDeterministic {
				t.Fatalf("evidence mode = %#v, want %q", got, EvidenceModeDeterministic)
			}
			if len(model.registeredTools()) != 0 {
				t.Fatalf("deterministic mode registered react tools: %#v", model.registeredTools())
			}
			return
		}
	}
	t.Fatalf("evidence finding missing: %#v", result.Findings)
}

type emptyKnowledgeSource struct{}

func (emptyKnowledgeSource) SearchDetailed(context.Context, string) ([]retriever.RetrievedDocument, error) {
	return nil, nil
}
