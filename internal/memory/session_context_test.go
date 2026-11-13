package memory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
)

func contextTestStore(t *testing.T) (*gorm.DB, *SessionContextManager) {
	t.Helper()
	db := openFeedbackTestDB(t)
	if err := db.AutoMigrate(&SessionContext{}, &ConversationHistory{}, &AgentRun{}); err != nil {
		t.Fatal(err)
	}
	return db, NewSessionContextManager(db)
}

func testTurn(query, answer string) []*schema.Message {
	return []*schema.Message{schema.UserMessage(query), schema.AssistantMessage(answer, nil)}
}

func historyCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	if err := db.Model(&ConversationHistory{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSnapshotOnlyLoadAndAtomicTurnRetry(t *testing.T) {
	db, store := contextTestStore(t)
	ctx := context.Background()
	initial, err := store.Load(ctx, "a", "s")
	if err != nil {
		t.Fatal(err)
	}
	working := []*schema.Message{
		schema.UserMessage("expanded continue instruction"),
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "call-1", Type: "function", Function: schema.FunctionCall{Name: "clock", Arguments: "{}"}}}},
		{Role: schema.Tool, ToolCallID: "call-1", Content: "actual evidence"},
		schema.AssistantMessage("answer", nil),
	}
	if err := store.CommitTurn(ctx, initial, working, "run-a", "继续", "answer"); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx, "a", "s")
	if err != nil || loaded.Record.Revision != 1 || len(loaded.Messages) != 4 || loaded.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	rows, err := NewConversationHistoryManager(db).GetSessionHistory(ctx, "a", "s", 10)
	if err != nil || len(rows) != 2 || rows[0].Content != "继续" {
		t.Fatalf("raw history=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		var metadata map[string]string
		if json.Unmarshal([]byte(row.Metadata), &metadata) != nil || metadata["run_id"] != "run-a" {
			t.Fatal("run association lost")
		}
	}
	for _, retry := range []LoadedContext{initial, loaded} {
		if err := store.CommitTurn(ctx, retry, working, "run-a", "继续", "answer"); err != nil {
			t.Fatal(err)
		}
	}
	if historyCount(t, db) != 2 {
		t.Fatal("retry duplicated history")
	}
	if err := store.CommitTurn(ctx, loaded, working, "run-a", "different raw query", "answer"); !errors.Is(err, ErrContextConflict) {
		t.Fatal(err)
	}
	// A raw-only record is deliberately not merged into the model snapshot.
	if err := NewConversationHistoryManager(db).SaveMessageForRun(ctx, "a", "s", "user", "raw-only", "external"); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.Load(ctx, "a", "s")
	if err != nil || len(loaded.Messages) != 4 || loaded.Record.Revision != 1 {
		t.Fatal("load scanned raw history or retry changed revision")
	}
	// Even an unavailable history table does not affect normal snapshot loads.
	if err := db.Migrator().DropTable(&ConversationHistory{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, "a", "s"); err != nil {
		t.Fatal(err)
	}
	other, err := store.Load(ctx, "other", "s")
	if err != nil || len(other.Messages) != 0 {
		t.Fatal("account context leaked")
	}
}

func TestTurnRollbackDoesNotLeaveSnapshotOrHalfHistory(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new session"
		if existing {
			name = "existing session"
		}
		t.Run(name, func(t *testing.T) {
			db, store := contextTestStore(t)
			ctx := context.Background()
			loaded, _ := store.Load(ctx, "a", "s")
			if existing {
				if err := store.CommitTurn(ctx, loaded, testTurn("old", "old answer"), "old", "old", "old answer"); err != nil {
					t.Fatal(err)
				}
				loaded, _ = store.Load(ctx, "a", "s")
			}
			if err := db.Exec("CREATE TRIGGER reject_answer BEFORE INSERT ON conversation_histories WHEN NEW.role = 'assistant' BEGIN SELECT RAISE(ABORT, 'history write failed'); END").Error; err != nil {
				t.Fatal(err)
			}
			messages := append(append([]*schema.Message(nil), loaded.Messages...), testTurn("new", "new answer")...)
			if err := store.CommitTurn(ctx, loaded, messages, "new", "new", "new answer"); err == nil {
				t.Fatal("expected injected write failure")
			}
			after, err := store.Load(ctx, "a", "s")
			if err != nil || after.Record.Revision != loaded.Record.Revision || len(after.Messages) != len(loaded.Messages) {
				t.Fatalf("snapshot was not rolled back: %+v %v", after, err)
			}
			want := int64(0)
			if existing {
				want = 2
			}
			if historyCount(t, db) != want {
				t.Fatal("transaction left partial raw history")
			}
			if !existing {
				var count int64
				db.Model(&SessionContext{}).Count(&count)
				if count != 0 {
					t.Fatal("failed first turn left a snapshot row")
				}
			}
		})
	}
}

func TestTurnRevisionFenceAndClearRollbackHistory(t *testing.T) {
	db, store := contextTestStore(t)
	ctx := context.Background()
	first, _ := store.Load(ctx, "a", "s")
	if err := store.CommitTurn(ctx, first, testTurn("A", "answer A"), "run-a", "A", "answer A"); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitTurn(ctx, first, testTurn("stale", "answer"), "stale", "stale", "answer"); !errors.Is(err, ErrContextConflict) {
		t.Fatal(err)
	}
	if historyCount(t, db) != 2 {
		t.Fatal("stale writer inserted history")
	}
	loaded, _ := store.Load(ctx, "a", "s")
	if err := NewConversationHistoryManager(db).DeleteSessionHistory(ctx, "a", "s"); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []LoadedContext{first, loaded} {
		if err := store.CommitTurn(ctx, stale, testTurn("late", "answer"), "late", "late", "answer"); !errors.Is(err, ErrContextConflict) {
			t.Fatal(err)
		}
	}
	blank, err := store.Load(ctx, "a", "s")
	if err != nil || len(blank.Messages) != 0 || historyCount(t, db) != 0 {
		t.Fatal("clear resurrected conversation")
	}
	if err := store.CommitTurn(ctx, blank, testTurn("fresh", "answer"), "fresh", "fresh", "answer"); err != nil {
		t.Fatal(err)
	}
}

type legacySessionContext struct {
	UserID          string `gorm:"primaryKey"`
	SessionID       string `gorm:"primaryKey"`
	Revision        uint64
	HistoryCursor   uint
	LastRunID       string
	CoveredRunsJSON string
	MessagesJSON    string
}

func (legacySessionContext) TableName() string { return "session_contexts" }

func TestStartupMigratesLegacyContextExactlyOnce(t *testing.T) {
	db := openFeedbackTestDB(t)
	ctx := context.Background()
	if err := db.AutoMigrate(&legacySessionContext{}, &ConversationHistory{}); err != nil {
		t.Fatal(err)
	}
	oldMessages := []*schema.Message{schema.UserMessage("A"), {Role: schema.Tool, ToolCallID: "saved-tool", Content: "preserved tool result"}, schema.AssistantMessage("answer A", nil)}
	data, _ := json.Marshal(oldMessages)
	if err := db.Create(&legacySessionContext{UserID: "a", SessionID: "s", Revision: 3, LastRunID: "run-a", CoveredRunsJSON: `["run-a"]`, MessagesJSON: string(data)}).Error; err != nil {
		t.Fatal(err)
	}
	history := NewConversationHistoryManager(db)
	for _, turn := range []struct{ session, run, query, answer string }{{"s", "run-a", "A", "answer A"}, {"s", "run-b", "B", "answer B"}, {"history-only", "run-c", "C", "answer C"}} {
		if err := history.SaveMessageForRun(ctx, "a", turn.session, "user", turn.query, turn.run); err != nil {
			t.Fatal(err)
		}
		if err := history.SaveMessageForRun(ctx, "a", turn.session, "assistant", turn.answer, turn.run); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(&SessionContext{}); err != nil {
		t.Fatal(err)
	}
	store := NewSessionContextManager(db)
	if _, err := store.Load(ctx, "a", "s"); !errors.Is(err, ErrLegacyContext) {
		t.Fatal("legacy data loaded without migration", err)
	}
	if err := migrateSessionContexts(ctx, db); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx, "a", "s")
	if err != nil || loaded.Record.Revision != 4 || len(loaded.Messages) != 5 || loaded.Messages[1].ToolCallID != "saved-tool" {
		t.Fatalf("migration=%+v err=%v", loaded, err)
	}
	bootstrap, err := store.Load(ctx, "a", "history-only")
	if err != nil || len(bootstrap.Messages) != 2 {
		t.Fatal("history-only migration failed", err)
	}
	if err := migrateSessionContexts(ctx, db); err != nil {
		t.Fatal(err)
	}
	again, _ := store.Load(ctx, "a", "s")
	if again.Record.Revision != loaded.Record.Revision || len(again.Messages) != len(loaded.Messages) || historyCount(t, db) != 6 {
		t.Fatal("migration was not idempotent")
	}
}

func TestMigrationPreservesCurrentSnapshotAndRollsBackMalformedLegacy(t *testing.T) {
	db, store := contextTestStore(t)
	ctx := context.Background()
	initial, _ := store.Load(ctx, "a", "s")
	if err := store.CommitTurn(ctx, initial, testTurn("A", "answer"), "run-a", "A", "answer"); err != nil {
		t.Fatal(err)
	}
	if err := NewConversationHistoryManager(db).SaveMessageForRun(ctx, "a", "s", "user", "do not import", "external"); err != nil {
		t.Fatal(err)
	}
	if err := migrateSessionContexts(ctx, db); err != nil {
		t.Fatal(err)
	}
	current, err := store.Load(ctx, "a", "s")
	if err != nil || current.Record.Revision != 1 || len(current.Messages) != 2 {
		t.Fatal("startup modified current snapshot", err)
	}
	if err := NewConversationHistoryManager(db).DeleteSessionHistory(ctx, "a", "s"); err != nil {
		t.Fatal(err)
	}
	if err := migrateSessionContexts(ctx, db); err != nil {
		t.Fatal(err)
	}
	blank, err := store.Load(ctx, "a", "s")
	if err != nil || len(blank.Messages) != 0 || blank.Record.Revision != 2 {
		t.Fatal("startup changed clear tombstone", err)
	}
	for _, record := range []SessionContext{
		{UserID: "legacy", SessionID: "1-good", Revision: 1, MessagesJSON: `[{"role":"user","content":"preserve"}]`},
		{UserID: "legacy", SessionID: "2-bad", Revision: 1, MessagesJSON: `not-json`},
	} {
		if err := db.Create(&record).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateSessionContexts(ctx, db); err == nil {
		t.Fatal("malformed snapshot was silently discarded")
	}
	var converted int64
	if err := db.Model(&SessionContext{}).Where("user_id = ? AND context_version = ?", "legacy", sessionContextVersion).Count(&converted).Error; err != nil {
		t.Fatal(err)
	}
	if converted != 0 {
		t.Fatal("failed migration did not roll back")
	}
}
