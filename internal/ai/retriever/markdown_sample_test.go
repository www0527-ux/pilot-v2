package retriever

import (
	"context"
	"os"
	"testing"
)

// Exercise the real guide used for manual upload without making embedding calls.
func TestRedisMarkdownSample(t *testing.T) {
	content, err := os.ReadFile("../../../docs/samples/redis-latency.md")
	if os.IsNotExist(err) {
		t.Skip("optional manual-upload sample is not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	sections, err := MarkdownChunks(context.Background(), string(content), 800, 120)
	if err != nil {
		t.Fatal(err)
	}
	chunks := 0
	for _, section := range sections {
		chunks += len(section.Chunks)
	}
	if len(sections) < 5 || chunks < len(sections) {
		t.Fatalf("unexpected sections=%d chunks=%d", len(sections), chunks)
	}
	t.Logf("Redis guide: %d sections, %d child chunks", len(sections), chunks)
}
