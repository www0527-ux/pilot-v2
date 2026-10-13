package controller

import (
	"strings"
	"testing"
	"time"

	"shanyangcode.com/pilot/internal/memory"
)

func TestNormalizeChatRequestTrimsInput(t *testing.T) {
	req := ChatRequest{
		UserID:    " user-1 ",
		SessionID: " session-1 ",
		Query:     " hello pilot ",
	}

	if err := normalizeChatRequest(&req); err != nil {
		t.Fatalf("normalizeChatRequest() error = %v", err)
	}
	if req.UserID != "user-1" {
		t.Fatalf("unexpected user id: %q", req.UserID)
	}
	if req.SessionID != "session-1" {
		t.Fatalf("unexpected session id: %q", req.SessionID)
	}
	if req.Query != "hello pilot" {
		t.Fatalf("unexpected query: %q", req.Query)
	}
}

func TestNormalizeChatRequestRejectsLongQuery(t *testing.T) {
	req := ChatRequest{
		UserID:    "user-1",
		SessionID: "session-1",
		Query:     strings.Repeat("a", maxChatQueryLength+1),
	}

	if err := normalizeChatRequest(&req); err == nil {
		t.Fatal("expected error for oversized query")
	}
}

func TestNormalizeKnowledgeUploadRejectsOversizedContent(t *testing.T) {
	req := UploadRequest{
		Title:    "runbook",
		Category: "ops",
		Content:  strings.Repeat("a", maxKnowledgeContentLength+1),
	}

	if err := normalizeKnowledgeUpload(&req); err == nil {
		t.Fatal("expected oversized knowledge content to be rejected")
	}
}

func TestNormalizeKnowledgeUploadBoundsAndDeduplicatesChunkMetadata(t *testing.T) {
	req := UploadRequest{Title: "runbook", Category: "ops", Content: "content", Tags: []string{" api ", "api", ""}, Metadata: map[string]string{"owner": "ops"}}
	if err := normalizeKnowledgeUpload(&req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tags) != 1 || req.Tags[0] != "api" {
		t.Fatalf("tags=%+v", req.Tags)
	}
	req.Metadata = map[string]string{"": "invalid"}
	if err := normalizeKnowledgeUpload(&req); err == nil {
		t.Fatal("expected invalid metadata key")
	}
}

func TestNormalizeKnowledgeDocIDRejectsUnsafeCharacters(t *testing.T) {
	if _, err := normalizeKnowledgeDocID("../secret"); err == nil {
		t.Fatal("expected unsafe document id to be rejected")
	}
}

func TestBuildChatMessagesFromHistory(t *testing.T) {
	now := time.Now()
	records := []memory.ConversationHistory{
		{Role: "user", Content: "hello", CreatedAt: now},
	}

	messages := buildChatMessagesFromHistory(records)
	if len(messages) != 1 {
		t.Fatalf("unexpected message count: %d", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Content != "hello" || !messages[0].Timestamp.Equal(now) {
		t.Fatalf("unexpected message payload: %+v", messages[0])
	}
}
