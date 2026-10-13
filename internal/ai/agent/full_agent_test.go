package agent

import (
	"testing"
)

func TestPrepareAgentQueryRewritesContinue(t *testing.T) {
	got := prepareAgentQuery("继续")
	if got == "继续" {
		t.Fatal("expected continue rewrite")
	}
	if prepareAgentQuery("hello") != "hello" {
		t.Fatal("expected passthrough")
	}
}

func TestFormatKnowledgeDocumentsMarksUntrusted(t *testing.T) {
	text := formatKnowledgeDocumentsForPrompt(nil)
	if text != "（无）" {
		t.Fatalf("empty = %q", text)
	}
}

func TestTruncatePromptText(t *testing.T) {
	if truncatePromptText("abc", 10) != "abc" {
		t.Fatal("short")
	}
	got := truncatePromptText("abcdefghij", 3)
	if got != "abc..." {
		t.Fatalf("got %q", got)
	}
}
