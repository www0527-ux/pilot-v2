package observability

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizePromQLRejectsControlCharacters(t *testing.T) {
	if _, err := NormalizePromQL("up\nrate(http_requests_total[5m])"); err == nil {
		t.Fatal("expected control character to be rejected")
	}
}

func TestNormalizePromQLRejectsOversizedQuery(t *testing.T) {
	if _, err := NormalizePromQL(strings.Repeat("a", MaxPromQLLength+1)); err == nil {
		t.Fatal("expected oversized query to be rejected")
	}
}

func TestNormalizeServiceRejectsUnsafeValue(t *testing.T) {
	if _, err := NormalizeService("pilot/api", ""); err == nil {
		t.Fatal("expected unsafe service value to be rejected")
	}
}

func TestNormalizeLogLevelAliasesWarning(t *testing.T) {
	level, err := NormalizeLogLevel("WARNING", "")
	if err != nil {
		t.Fatalf("NormalizeLogLevel() error = %v", err)
	}
	if level != "warn" {
		t.Fatalf("level = %q, want warn", level)
	}
}

func TestNormalizeDurationMinutesClamps(t *testing.T) {
	if got := NormalizeDurationMinutes(MaxDurationMins + 10); got != MaxDurationMins {
		t.Fatalf("duration = %d, want %d", got, MaxDurationMins)
	}
}

func TestNormalizeLimitClampsOversizedValue(t *testing.T) {
	if got := NormalizeLimit(MaxLogLimit + 1); got != MaxLogLimit {
		t.Fatalf("limit = %d, want %d", got, MaxLogLimit)
	}
}

func TestNormalizeLimitUsesDefaultForInvalidValue(t *testing.T) {
	if got := NormalizeLimit(0); got != DefaultLogLimit {
		t.Fatalf("limit = %d, want %d", got, DefaultLogLimit)
	}
}

func TestNormalizeTimeRangeClampsLongWindow(t *testing.T) {
	end := time.Unix(2000, 0)
	start := end.Add(-2 * MaxQueryRange)

	gotStart, gotEnd := NormalizeTimeRange(start, end)
	if !gotEnd.Equal(end) {
		t.Fatalf("end changed: got %v want %v", gotEnd, end)
	}
	if gotEnd.Sub(gotStart) != MaxQueryRange {
		t.Fatalf("window = %v, want %v", gotEnd.Sub(gotStart), MaxQueryRange)
	}
}
