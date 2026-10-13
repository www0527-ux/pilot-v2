package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"
)

func TestAgentRunJournalUpsertOwnershipAndConversationAssociation(t *testing.T) {
	db := openFeedbackTestDB(t)
	if err := db.AutoMigrate(&AgentRun{}, &ConversationHistory{}, &SessionContext{}); err != nil {
		t.Fatal(err)
	}
	manager := NewConversationHistoryManager(db)
	ctx := context.Background()
	record := AgentRun{ID: "run-one", UserID: "u", SessionID: "s", Query: "q", Status: "executing", EventsJSON: `[{"sequence":1}]`}
	if err := manager.SaveAgentRun(ctx, record); err != nil {
		t.Fatal(err)
	}
	record.Status, record.Answer, record.EventsJSON = "completed", "final answer", `[{"sequence":1},{"sequence":2}]`
	if err := manager.SaveAgentRun(ctx, record); err != nil {
		t.Fatal(err)
	}
	loaded, err := manager.GetAgentRun(ctx, record.ID, "u", "s")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != "completed" || loaded.Answer != "final answer" || loaded.EventsJSON != record.EventsJSON {
		t.Fatalf("loaded=%+v", loaded)
	}
	if _, err := manager.GetAgentRun(ctx, record.ID, "other", "s"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("ownership err=%v", err)
	}
	stolen := record
	stolen.UserID = "other"
	stolen.Answer = "overwritten"
	if err := manager.SaveAgentRun(ctx, stolen); err == nil {
		t.Fatal("another account could overwrite the run UUID")
	}
	unchanged, err := manager.GetAgentRun(ctx, record.ID, "u", "s")
	if err != nil || unchanged.Answer != "final answer" {
		t.Fatal("ownership conflict lost the original run")
	}
	runs, err := manager.GetSessionRuns(ctx, "u", "s")
	if err != nil || len(runs) != 1 {
		t.Fatalf("records=%v err=%v", runs, err)
	}
	if err := manager.SaveMessageForRun(ctx, "u", "s", "assistant", "final answer", record.ID); err != nil {
		t.Fatal(err)
	}
	history, err := manager.GetSessionHistory(ctx, "u", "s", 10)
	if err != nil || len(history) != 1 || !strings.Contains(history[0].Metadata, record.ID) {
		t.Fatalf("history=%v err=%v", history, err)
	}
	if err := manager.DeleteSessionHistory(ctx, "u", "s"); err != nil {
		t.Fatal(err)
	}
	runs, err = manager.GetSessionRuns(ctx, "u", "s")
	if err != nil || len(runs) != 0 {
		t.Fatalf("deleted records=%v err=%v", runs, err)
	}
}
