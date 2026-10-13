package multiagent

import (
	"context"

	"errors"
	"fmt"
	"shanyangcode.com/pilot/internal/ai/progress"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	einoagent "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	callbackutils "github.com/cloudwego/eino/utils/callbacks"

	"shanyangcode.com/pilot/internal/ai/tools"
	"shanyangcode.com/pilot/internal/prompts"
	"shanyangcode.com/pilot/pkg/types"
)

const (
	EvidenceModeDeterministic = "deterministic"
	EvidenceModeReact         = "react"

	defaultReactEvidenceMaxSteps = 8
	maxEvidenceArgumentRunes     = 1000
	maxEvidenceResponseRunes     = 4000
	maxReactErrorRunes           = 300
)

// EvidenceSource is the narrow runtime data surface available to evidence agents.
// The ReAct variant intentionally excludes traces, topology, knowledge, and all
// mutating capabilities.
type EvidenceSource interface {
	GetAlerts(ctx context.Context, filters map[string]string) ([]types.AlertInfo, error)
	QueryMetrics(ctx context.Context, query string, start, end time.Time) ([]types.MetricData, error)
	QueryLogs(ctx context.Context, service, level, keyword string, start, end time.Time, limit int) ([]types.LogEntry, error)
}

// ReactEvidenceExecutor uses a constrained ReAct agent to choose which real
// evidence sources to query. It never treats the model response itself as evidence.
type ReactEvidenceExecutor struct {
	agent    *react.Agent
	maxSteps int
	prompts  *prompts.Catalog
}

// NewReactEvidenceExecutor constructs a ReAct executor with exactly the three
// read-only tools that can produce runtime evidence for an incident.
func NewReactEvidenceExecutor(ctx context.Context, source EvidenceSource, chatModel model.ToolCallingChatModel, maxSteps int) (*ReactEvidenceExecutor, error) {
	if dependencyMissing(source) {
		return nil, fmt.Errorf("evidence source is required")
	}
	if dependencyMissing(chatModel) {
		return nil, fmt.Errorf("react evidence model is required")
	}
	if maxSteps <= 0 {
		maxSteps = defaultReactEvidenceMaxSteps
	}

	toolset, err := newReactEvidenceTools(source)
	if err != nil {
		return nil, err
	}
	agent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: chatModel,
		MaxStep:          maxSteps,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: toolset,
		},
		GraphName: "ReactEvidenceCollector",
	})
	if err != nil {
		return nil, fmt.Errorf("create react evidence agent: %w", err)
	}

	catalog, err := prompts.Default()
	if err != nil {
		return nil, fmt.Errorf("load react evidence prompts: %w", err)
	}
	return &ReactEvidenceExecutor{agent: agent, maxSteps: maxSteps, prompts: catalog}, nil
}

func newReactEvidenceTools(source EvidenceSource) ([]tool.BaseTool, error) {
	registered := make([]tool.BaseTool, 0, 3)
	var constructionErrors []error
	add := func(name string, build func() (tool.InvokableTool, error)) {
		created, err := build()
		if err != nil {
			constructionErrors = append(constructionErrors, fmt.Errorf("%s: %w", name, err))
			return
		}
		registered = append(registered, created)
	}

	add("query_alerts", func() (tool.InvokableTool, error) { return tools.NewQueryAlertsTool(source) })
	add("query_logs", func() (tool.InvokableTool, error) { return tools.NewQueryLogsTool(source) })
	add("query_metrics", func() (tool.InvokableTool, error) { return tools.NewQueryMetricsTool(source) })
	if err := errors.Join(constructionErrors...); err != nil {
		return nil, fmt.Errorf("create react evidence tools: %w", err)
	}
	return registered, nil
}

// Execute gathers only callback-recorded tool responses. A model answer without
// a permitted tool call is rejected so it cannot be presented as runtime evidence.
func (e *ReactEvidenceExecutor) Execute(ctx context.Context, query string, plan Plan) (Finding, error) {
	if e == nil || e.agent == nil {
		return Finding{}, fmt.Errorf("react evidence executor is unavailable")
	}

	recorder := newEvidenceRecorder()
	messages, promptErr := e.evidenceMessages(query, plan)
	if promptErr != nil {
		return Finding{}, promptErr
	}
	response, runErr := e.agent.Generate(ctx, messages,
		einoagent.WithComposeOptions(compose.WithCallbacks(recorder.handler(), progress.CallbackHandler())),
	)

	evidence := recorder.evidence()
	successfulCalls := 0
	failedCalls := 0
	for _, item := range evidence {
		if metadataStatusError(item.Metadata) {
			failedCalls++
		} else {
			successfulCalls++
		}
	}
	if runErr != nil {
		if successfulCalls == 0 || failedCalls == 0 {
			return Finding{}, fmt.Errorf("run react evidence agent: %w", runErr)
		}
		return Finding{
			Summary:  fmt.Sprintf("React evidence collection stopped after a tool failure; preserved %d successful read-only tool result(s).", successfulCalls),
			Evidence: evidence,
			Metadata: map[string]any{
				"mode":             EvidenceModeReact,
				"react_max_steps":  e.maxSteps,
				"tool_call_count":  len(evidence),
				"tool_error_count": failedCalls,
				"partial_failure":  true,
				"react_error":      truncate(runErr.Error(), maxReactErrorRunes),
			},
		}, nil
	}
	if response == nil || strings.TrimSpace(response.Content) == "" {
		return Finding{}, fmt.Errorf("react evidence agent returned an empty summary")
	}
	if len(evidence) == 0 {
		return Finding{}, fmt.Errorf("react evidence agent completed without calling a permitted tool")
	}
	if successfulCalls == 0 {
		return Finding{}, fmt.Errorf("react evidence agent completed with %d failed tool call(s) and no usable runtime evidence", failedCalls)
	}
	metadata := map[string]any{
		"mode":             EvidenceModeReact,
		"react_max_steps":  e.maxSteps,
		"tool_call_count":  len(evidence),
		"tool_error_count": failedCalls,
	}
	if failedCalls > 0 {
		metadata["partial_failure"] = true
	}
	return Finding{
		Summary:  strings.TrimSpace(response.Content),
		Evidence: evidence,
		Metadata: metadata,
	}, nil
}

func (e *ReactEvidenceExecutor) evidenceMessages(query string, plan Plan) ([]*schema.Message, error) {
	systemPrompt, err := e.prompts.Render("react_evidence_system", nil)
	if err != nil {
		return nil, err
	}
	userPrompt, err := e.prompts.Render("react_evidence_user", map[string]any{"service": serviceLabel(plan.Service), "query": strings.TrimSpace(query)})
	if err != nil {
		return nil, err
	}
	return []*schema.Message{schema.SystemMessage(systemPrompt), schema.UserMessage(userPrompt)}, nil
}

type fallbackEvidenceExecutor struct {
	primary  Executor
	fallback Executor
}

func newFallbackEvidenceExecutor(primary, fallback Executor) *fallbackEvidenceExecutor {
	return &fallbackEvidenceExecutor{primary: primary, fallback: fallback}
}

func (e *fallbackEvidenceExecutor) Execute(ctx context.Context, query string, plan Plan) (Finding, error) {
	finding, err := e.primary.Execute(ctx, query, plan)
	if err == nil {
		return finding, nil
	}

	fallbackFinding, fallbackErr := e.fallback.Execute(ctx, query, plan)
	if fallbackErr != nil {
		return Finding{}, fmt.Errorf("react evidence failed: %v; deterministic fallback failed: %w", err, fallbackErr)
	}
	metadata := cloneMetadata(fallbackFinding.Metadata)
	metadata["mode"] = "deterministic_fallback"
	metadata["react_error"] = truncate(err.Error(), maxReactErrorRunes)
	fallbackFinding.Metadata = metadata
	return fallbackFinding, nil
}

func cloneMetadata(input map[string]any) map[string]any {
	output := make(map[string]any, len(input)+2)
	for key, value := range input {
		output[key] = value
	}
	return output
}

type evidenceToolCall struct {
	sequence  int
	name      string
	arguments string
}

type evidenceToolCallKey struct{}

type recordedEvidenceToolCall struct {
	evidenceToolCall
	response string
	err      error
}

// evidenceRecorder belongs to one Execute call and is safe when a tools node
// runs several independent tool calls concurrently.
type evidenceRecorder struct {
	mu       sync.Mutex
	next     int
	recorded []recordedEvidenceToolCall
}

func newEvidenceRecorder() *evidenceRecorder {
	return &evidenceRecorder{}
}

func (r *evidenceRecorder) handler() callbacks.Handler {
	return callbackutils.NewHandlerHelper().Tool(&callbackutils.ToolCallbackHandler{
		OnStart: r.onToolStart,
		OnEnd:   r.onToolEnd,
		OnError: r.onToolError,
	}).Handler()
}

func (r *evidenceRecorder) onToolStart(ctx context.Context, runInfo *callbacks.RunInfo, input *tool.CallbackInput) context.Context {
	arguments := ""
	if input != nil {
		arguments = input.ArgumentsInJSON
	}

	r.mu.Lock()
	r.next++
	call := evidenceToolCall{sequence: r.next, name: evidenceToolName(runInfo), arguments: arguments}
	r.mu.Unlock()
	return context.WithValue(ctx, evidenceToolCallKey{}, call)
}

func (r *evidenceRecorder) onToolEnd(ctx context.Context, _ *callbacks.RunInfo, output *tool.CallbackOutput) context.Context {
	response := ""
	if output != nil {
		response = output.Response
	}
	r.record(ctx, response, nil)
	return ctx
}

func (r *evidenceRecorder) onToolError(ctx context.Context, _ *callbacks.RunInfo, err error) context.Context {
	r.record(ctx, "", err)
	return ctx
}

func (r *evidenceRecorder) record(ctx context.Context, response string, err error) {
	call, ok := ctx.Value(evidenceToolCallKey{}).(evidenceToolCall)
	if !ok {
		return
	}
	r.mu.Lock()
	r.recorded = append(r.recorded, recordedEvidenceToolCall{evidenceToolCall: call, response: response, err: err})
	r.mu.Unlock()
}

func (r *evidenceRecorder) evidence() []EvidenceItem {
	r.mu.Lock()
	recorded := append([]recordedEvidenceToolCall(nil), r.recorded...)
	r.mu.Unlock()

	sort.Slice(recorded, func(i, j int) bool { return recorded[i].sequence < recorded[j].sequence })
	evidence := make([]EvidenceItem, 0, len(recorded))
	for _, call := range recorded {
		response := call.response
		status := "success"
		if call.err != nil {
			response = call.err.Error()
			status = "error"
		}
		evidence = append(evidence, EvidenceItem{
			ID:      fmt.Sprintf("tool:%s:%d", call.name, call.sequence),
			Source:  "tool:" + call.name,
			Content: truncate(response, maxEvidenceResponseRunes),
			Metadata: map[string]any{
				"arguments": truncate(call.arguments, maxEvidenceArgumentRunes),
				"status":    status,
			},
		})
	}
	return evidence
}

func evidenceToolName(runInfo *callbacks.RunInfo) string {
	if runInfo != nil && strings.TrimSpace(runInfo.Name) != "" {
		return runInfo.Name
	}
	return "unknown"
}
