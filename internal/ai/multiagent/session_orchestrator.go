package multiagent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// SessionContextLoader loads optional prior-turn context for multi-agent runs.
type SessionContextLoader interface {
	SessionSummary(ctx context.Context, userID, sessionID string) (string, error)
}

// EpisodicRecaller optionally recalls similar past incidents.
type EpisodicRecaller interface {
	Recall(ctx context.Context, service, query string, limit int) ([]Finding, error)
}

// EpisodicRecorder persists a completed multi-agent incident summary.
type EpisodicRecorder interface {
	Record(ctx context.Context, record EpisodicRecord) error
}

// EpisodicRecord is the minimal incident snapshot stored after a run.
type EpisodicRecord struct {
	UserID      string
	SessionID   string
	Service     string
	Intent      string
	Query       string
	Answer      string
	EvidenceIDs []string
	Limitations []string
}

// CoreQueryRunner executes a planned multi-agent query (legacy or ADK).
type CoreQueryRunner interface {
	Execute(ctx context.Context, query string) (Result, error)
}

type CorePlanPreparer interface {
	PreparePlan(ctx context.Context, query string) (Plan, error)
}

type sessionSummaryContextKey struct{}

// contextWithSessionSummary scopes prior-turn context to one request. Keeping
// it in context avoids cross-session leakage when the shared orchestrator is
// invoked concurrently.
func contextWithSessionSummary(ctx context.Context, summary string) context.Context {
	return context.WithValue(ctx, sessionSummaryContextKey{}, strings.TrimSpace(summary))
}

func sessionSummaryFromContext(ctx context.Context) string {
	summary, _ := sessionSummaryContextValue(ctx)
	return summary
}

func sessionSummaryContextValue(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	value, ok := ctx.Value(sessionSummaryContextKey{}).(string)
	return strings.TrimSpace(value), ok
}

// SessionOrchestrator binds user/session context onto a core query runner.
type SessionOrchestrator struct {
	inner    CoreQueryRunner
	sessions SessionContextLoader
	recall   EpisodicRecaller
	topK     int
}

// NewSessionOrchestrator wraps a core runner with optional session/episodic hooks.
func NewSessionOrchestrator(inner CoreQueryRunner, sessions SessionContextLoader, recall EpisodicRecaller) *SessionOrchestrator {
	return &SessionOrchestrator{inner: inner, sessions: sessions, recall: recall, topK: 3}
}

// WithTopK sets episodic recall size.
func (s *SessionOrchestrator) WithTopK(topK int) *SessionOrchestrator {
	if s != nil && topK > 0 {
		s.topK = topK
	}
	return s
}

// Execute loads session summary and episodic recalls, then runs the orchestrator.
func (s *SessionOrchestrator) Execute(ctx context.Context, userID, sessionID, query string) (Result, error) {
	if s == nil || s.inner == nil {
		return Result{}, fmt.Errorf("session orchestrator is unavailable")
	}
	summaryParts := make([]string, 0, 2)
	memoryFindings := make([]Finding, 0, 1)
	if s.sessions != nil {
		if loaded, err := s.sessions.SessionSummary(ctx, userID, sessionID); err == nil {
			if trimmed := strings.TrimSpace(loaded); trimmed != "" {
				summaryParts = append(summaryParts, trimmed)
			}
		} else {
			slog.WarnContext(ctx, "load layered session context failed; continuing without it", "error", err)
		}
	}
	serviceHint := NewRulePlanner("").Plan(query).Service
	if s.recall != nil {
		if recalled, err := s.recall.Recall(ctx, serviceHint, query, s.topK); err == nil {
			for _, finding := range recalled {
				// Mark as memory — never treat as runtime evidence for Verifier.
				if finding.Metadata == nil {
					finding.Metadata = map[string]any{}
				}
				finding.Metadata["memory"] = true
				finding.Metadata["not_runtime_evidence"] = true
				for i := range finding.Evidence {
					if finding.Evidence[i].Metadata == nil {
						finding.Evidence[i].Metadata = map[string]any{}
					}
					finding.Evidence[i].Metadata["memory"] = true
					finding.Evidence[i].Source = "episodic_memory"
				}
				memoryFindings = append(memoryFindings, finding)
				for _, item := range finding.Evidence {
					summaryParts = append(summaryParts, "[memory — not runtime evidence] "+item.Content)
				}
			}
		} else {
			slog.WarnContext(ctx, "recall episodic memory failed; continuing without it", "error", err)
		}
	}

	runCtx := contextWithSessionSummary(ctx, strings.Join(summaryParts, "\n"))
	result, err := s.inner.Execute(runCtx, query)
	if err != nil {
		return Result{}, err
	}
	if len(memoryFindings) > 0 {
		result.Findings = append(result.Findings, memoryFindings...)
		appendContextEvidenceNodes(result.EvidenceGraph, memoryFindings)
	}

	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	if len(summaryParts) > 0 {
		result.Metadata["session_context_parts"] = len(summaryParts)
	}
	if len(memoryFindings) > 0 {
		result.Metadata["episodic_memory_findings"] = len(memoryFindings)
	}
	result.Metadata["episodic_promotion"] = "requires_explicit_thumbs_up"
	return result, nil
}

func (s *SessionOrchestrator) PrepareCheckpoint(ctx context.Context, userID, sessionID, query string) (Plan, error) {
	preparer, ok := s.inner.(CorePlanPreparer)
	if !ok {
		return Plan{}, fmt.Errorf("core runner cannot prepare a checkpoint plan")
	}
	return preparer.PreparePlan(ctx, query)
}

func (s *SessionOrchestrator) ResumeCheckpoint(ctx context.Context, checkpoint Checkpoint) (Result, error) {
	return s.Execute(contextWithPreparedPlan(ctx, checkpoint.Plan), checkpoint.UserID, checkpoint.SessionID, checkpoint.Query)
}

// SetSessionSummary implements CoreQueryRunner for Orchestrator.
func (o *Orchestrator) SetSessionSummary(summary string) {
	if o != nil {
		o.sessionMu.Lock()
		defer o.sessionMu.Unlock()
		o.sessionSummary = strings.TrimSpace(summary)
	}
}

func (o *Orchestrator) sessionContext(ctx context.Context) string {
	if summary, scoped := sessionSummaryContextValue(ctx); scoped {
		return summary
	}
	if o == nil {
		return ""
	}
	o.sessionMu.RLock()
	defer o.sessionMu.RUnlock()
	return o.sessionSummary
}

// SetSessionSummary implements CoreQueryRunner for ADKOrchestrator.
func (a *ADKOrchestrator) SetSessionSummary(summary string) {
	if a != nil && a.inner != nil {
		a.inner.SetSessionSummary(summary)
	}
}
