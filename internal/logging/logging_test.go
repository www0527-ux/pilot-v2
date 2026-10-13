package logging

import (
	"log/slog"
	"testing"
)

func TestParseLevelUsesSharedAliases(t *testing.T) {
	cases := map[string]slog.Level{"all": slog.LevelDebug, "全部": slog.LevelDebug, "debug": slog.LevelDebug, "info": slog.LevelInfo, "warning": slog.LevelWarn, "warn": slog.LevelWarn, "error": slog.LevelError, "fatal": slog.LevelError}
	for input, want := range cases {
		if got := ParseLevel(input); got != want {
			t.Fatalf("ParseLevel(%q)=%v want=%v", input, got, want)
		}
	}
}
