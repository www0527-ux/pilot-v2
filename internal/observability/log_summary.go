package observability

import (
	"sort"
	"strings"

	"shanyangcode.com/pilot/pkg/types"
)

const defaultRepresentativeLogLimit = 6

// LogSummary describes both the original result set and the bounded payload.
// Aggregations are computed from the original entries, while representatives
// and items contain only rune-budget-safe copies.
type LogSummary struct {
	OriginalCount      int              `json:"original_count"`
	ReturnedCount      int              `json:"returned_count"`
	OmittedCount       int              `json:"omitted_count"`
	Truncated          bool             `json:"truncated"`
	RuneBudget         int              `json:"rune_budget"`
	RunesUsed          int              `json:"runes_used"`
	LevelCounts        map[string]int   `json:"level_counts"`
	ServiceCounts      map[string]int   `json:"service_counts"`
	TraceCounts        map[string]int   `json:"trace_counts"`
	RepresentativeLogs []types.LogEntry `json:"representative_logs"`
}

// SummarizedLogs is the common response contract used by HTTP, Agent tools,
// and MCP. Items preserve newest-first input order.
type SummarizedLogs struct {
	Items   []types.LogEntry `json:"items"`
	Summary LogSummary       `json:"summary"`
}

// SummarizeLogs returns bounded copies of logs and aggregates over the full
// input. The rune budget applies to user-controlled textual fields and is
// deliberately conservative; JSON framing is bounded separately by item and
// field limits at callers.
func SummarizeLogs(logs []types.LogEntry, runeBudget int) SummarizedLogs {
	runeBudget = NormalizeLogOutputRunes(runeBudget)
	result := SummarizedLogs{
		Items: make([]types.LogEntry, 0, len(logs)),
		Summary: LogSummary{
			OriginalCount: len(logs),
			RuneBudget:    runeBudget,
			LevelCounts:   make(map[string]int),
			ServiceCounts: make(map[string]int),
			TraceCounts:   make(map[string]int),
		},
	}

	for _, entry := range logs {
		level := canonicalLogLevel(entry.Level)
		service := strings.TrimSpace(entry.Service)
		if service == "" {
			service = "unknown"
		}
		result.Summary.LevelCounts[level]++
		result.Summary.ServiceCounts[service]++
		if traceID := strings.TrimSpace(entry.TraceID); traceID != "" {
			result.Summary.TraceCounts[traceID]++
		}
	}

	remaining := runeBudget
	for _, entry := range logs {
		copyEntry, used, ok := fitLogEntry(entry, remaining)
		if !ok {
			break
		}
		result.Items = append(result.Items, copyEntry)
		result.Summary.RunesUsed += used
		remaining -= used
	}

	result.Summary.ReturnedCount = len(result.Items)
	result.Summary.OmittedCount = len(logs) - len(result.Items)
	result.Summary.Truncated = result.Summary.OmittedCount > 0 || entriesDiffer(logs, result.Items)
	result.Summary.RepresentativeLogs = representativeLogs(result.Items, defaultRepresentativeLogLimit)
	return result
}

func fitLogEntry(entry types.LogEntry, budget int) (types.LogEntry, int, bool) {
	copyEntry := entry
	copyEntry.Level = canonicalLogLevel(entry.Level)
	copyEntry.Service = strings.TrimSpace(entry.Service)
	if copyEntry.Service == "" {
		copyEntry.Service = "unknown"
	}
	copyEntry.TraceID = strings.TrimSpace(entry.TraceID)
	copyEntry.Fields = nil // arbitrary fields are excluded from bounded summaries

	fixed := len([]rune(copyEntry.Level)) + len([]rune(copyEntry.Service)) + len([]rune(copyEntry.TraceID)) + 8
	if budget <= fixed {
		return types.LogEntry{}, 0, false
	}
	messageRunes := []rune(entry.Message)
	allowed := budget - fixed
	if allowed < len(messageRunes) {
		if allowed <= 1 {
			return types.LogEntry{}, 0, false
		}
		messageRunes = append(messageRunes[:allowed-1], '…')
	}
	copyEntry.Message = string(messageRunes)
	used := fixed + len(messageRunes)
	return copyEntry, used, true
}

func canonicalLogLevel(value string) string {
	level, err := NormalizeLogLevel(value, "info")
	if err != nil || level == "all" || level == "" {
		return "info"
	}
	return level
}

func entriesDiffer(original, bounded []types.LogEntry) bool {
	if len(original) != len(bounded) {
		return true
	}
	for i := range original {
		if original[i].Message != bounded[i].Message || len(original[i].Fields) != len(bounded[i].Fields) {
			return true
		}
	}
	return false
}

func representativeLogs(items []types.LogEntry, limit int) []types.LogEntry {
	if limit <= 0 || len(items) == 0 {
		return []types.LogEntry{}
	}
	representatives := make([]types.LogEntry, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, entry := range items {
		key := canonicalLogLevel(entry.Level) + "\x00" + entry.Service + "\x00" + entry.TraceID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		representatives = append(representatives, entry)
		if len(representatives) == limit {
			return representatives
		}
	}
	if len(representatives) < limit {
		// Fill deterministically when all entries share the same aggregation key.
		already := make(map[string]struct{}, len(representatives))
		for _, entry := range representatives {
			already[entry.Timestamp.String()+"\x00"+entry.Message] = struct{}{}
		}
		for _, entry := range items {
			key := entry.Timestamp.String() + "\x00" + entry.Message
			if _, ok := already[key]; ok {
				continue
			}
			representatives = append(representatives, entry)
			if len(representatives) == limit {
				break
			}
		}
	}
	return representatives
}

// SortedCountKeys is used by text renderers that need deterministic output.
func SortedCountKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
