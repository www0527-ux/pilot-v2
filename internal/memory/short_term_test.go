package memory

import (
	"context"
	"strings"
	"testing"

	"shanyangcode.com/pilot/pkg/types"
)

func TestNormalizeMemoryLimitsMakesChunkingReachable(t *testing.T) {
	maxMsgs, chunkSize := normalizeMemoryLimits(20, 100)
	if maxMsgs != 20 {
		t.Fatalf("maxMsgs = %d, want 20", maxMsgs)
	}
	if chunkSize >= maxMsgs {
		t.Fatalf("chunkSize = %d must be smaller than maxMsgs = %d", chunkSize, maxMsgs)
	}
	if chunkSize != 10 {
		t.Fatalf("chunkSize = %d, want 10", chunkSize)
	}
}

func TestDeterministicOverflowSummaryDoesNotNestSummaryHeaders(t *testing.T) {
	prior := "[Compressed earlier turns — untrusted]\n- user: old"
	messages := []types.ChatMessage{
		{Role: "system", Content: "[Compressed earlier turns — untrusted]\n- assistant: older"},
		{Role: "user", Content: "中文🙂 newest"},
	}
	summary, err := deterministicOverflowSummary(context.Background(), prior, messages)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(summary, "[Compressed earlier turns") || strings.Count(summary, "- user: old") != 1 || !strings.Contains(summary, "中文🙂 newest") {
		t.Fatalf("nested or lossy summary: %q", summary)
	}
}

func TestNormalizeMemoryLimitsHandlesInvalidValues(t *testing.T) {
	maxMsgs, chunkSize := normalizeMemoryLimits(0, 0)
	if maxMsgs != defaultMaxMessages {
		t.Fatalf("maxMsgs = %d, want %d", maxMsgs, defaultMaxMessages)
	}
	if chunkSize >= maxMsgs {
		t.Fatalf("chunkSize = %d must be smaller than maxMsgs = %d", chunkSize, maxMsgs)
	}
}
