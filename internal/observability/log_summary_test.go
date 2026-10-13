package observability

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"shanyangcode.com/pilot/pkg/types"
)

func TestNormalizeLogLevelCompleteVocabulary(t *testing.T) {
	cases := map[string]string{
		"all": "all", "全部": "all", "DEBUG": "debug", "info": "info",
		"warning": "warn", "warn": "warn", "error": "error", "fatal": "fatal",
	}
	for input, want := range cases {
		got, err := NormalizeLogLevel(input, "")
		if err != nil || got != want {
			t.Fatalf("NormalizeLogLevel(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestSummarizeLogsIsRuneSafeAndReportsTruncation(t *testing.T) {
	now := time.Now()
	logs := []types.LogEntry{
		{Timestamp: now, Level: "warning", Service: "api", TraceID: "trace-a", Message: strings.Repeat("中文🙂", 200), Fields: map[string]string{"secret": "excluded"}},
		{Timestamp: now.Add(-time.Second), Level: "fatal", Service: "db", TraceID: "trace-b", Message: "fatal body"},
	}
	got := SummarizeLogs(logs, MinLogOutputRunes)
	if got.Summary.OriginalCount != 2 || !got.Summary.Truncated || got.Summary.LevelCounts["warn"] != 1 || got.Summary.LevelCounts["fatal"] != 1 {
		t.Fatalf("unexpected summary: %+v", got.Summary)
	}
	if got.Summary.ReturnedCount != len(got.Items) || got.Summary.OmittedCount != 2-len(got.Items) {
		t.Fatalf("inconsistent counts: %+v", got.Summary)
	}
	for _, entry := range got.Items {
		if !utf8.ValidString(entry.Message) {
			t.Fatalf("invalid UTF-8 message: %q", entry.Message)
		}
		if entry.Fields != nil {
			t.Fatalf("fields leaked into summary: %+v", entry.Fields)
		}
	}
}
