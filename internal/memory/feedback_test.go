package memory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"shanyangcode.com/pilot/internal/ai/multiagent"
)

type promotionRecorder struct {
	calls int
	err   error
}

func (r *promotionRecorder) Record(context.Context, multiagent.EpisodicRecord) error {
	r.calls++
	return r.err
}

func openFeedbackTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureFeedbackSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestFeedbackIsIdempotentAndOnlyUpPromotesEligibleSuccess(t *testing.T) {
	db := openFeedbackTestDB(t)
	recorder := &promotionRecorder{}
	manager := NewFeedbackManager(db, recorder)
	result := multiagent.Result{Answer: "answer", Plan: multiagent.Plan{Intent: "incident_triage", Service: "api"}}
	snapshot, _ := json.Marshal(result)
	for _, record := range []InteractionRecord{
		{ID: "ok", UserID: "u", SessionID: "s", Mode: "multi-agent", Status: "completed", Query: "q", Answer: "answer", Snapshot: string(snapshot)},
		{ID: "interrupted", UserID: "u", SessionID: "s", Mode: "multi-agent", Status: "interrupted", Query: "q", Answer: "partial", Snapshot: string(snapshot)},
		{ID: "rejected", UserID: "u", SessionID: "s", Mode: "multi-agent", Status: "rejected", Query: "q", Answer: "fallback", Snapshot: string(snapshot)},
		{ID: "abstain", UserID: "u", SessionID: "s", Mode: "multi-agent", Status: "abstain", Query: "q", Answer: "cannot conclude", Snapshot: string(snapshot)},
	} {
		if err := manager.RecordInteraction(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	down, err := manager.Submit(context.Background(), "ok", "u", "s", FeedbackDown, "improve")
	if err != nil || down.PromotionStatus != "not_eligible" || recorder.calls != 0 {
		t.Fatalf("down outcome=%+v err=%v calls=%d", down, err, recorder.calls)
	}
	up, err := manager.Submit(context.Background(), "ok", "u", "s", FeedbackUp, "")
	if err != nil || up.PromotionStatus != "promoted" || recorder.calls != 1 {
		t.Fatalf("up outcome=%+v err=%v calls=%d", up, err, recorder.calls)
	}
	again, err := manager.Submit(context.Background(), "ok", "u", "s", FeedbackUp, "")
	if err != nil || !again.Idempotent || recorder.calls != 1 {
		t.Fatalf("idempotent outcome=%+v err=%v calls=%d", again, err, recorder.calls)
	}
	changed, err := manager.Submit(context.Background(), "ok", "u", "s", FeedbackDown, "later concern")
	if err != nil || changed.PromotionStatus != "promoted" || recorder.calls != 1 {
		t.Fatalf("vote update duplicated/lost promotion: %+v err=%v calls=%d", changed, err, recorder.calls)
	}
	upAgain, err := manager.Submit(context.Background(), "ok", "u", "s", FeedbackUp, "")
	if err != nil || upAgain.PromotionStatus != "promoted" || recorder.calls != 1 {
		t.Fatalf("re-up duplicated promotion: %+v err=%v calls=%d", upAgain, err, recorder.calls)
	}
	blocked, err := manager.Submit(context.Background(), "interrupted", "u", "s", FeedbackUp, "")
	if err != nil || blocked.PromotionStatus != "not_eligible" || recorder.calls != 1 {
		t.Fatalf("interrupted outcome=%+v err=%v calls=%d", blocked, err, recorder.calls)
	}
	for _, id := range []string{"rejected", "abstain"} {
		blocked, err = manager.Submit(context.Background(), id, "u", "s", FeedbackUp, "")
		if err != nil || blocked.PromotionStatus != "not_eligible" || recorder.calls != 1 {
			t.Fatalf("%s outcome=%+v err=%v calls=%d", id, blocked, err, recorder.calls)
		}
	}
}

func TestPromotionFailureDoesNotLoseFeedback(t *testing.T) {
	db := openFeedbackTestDB(t)
	recorder := &promotionRecorder{err: errors.New("es unavailable")}
	manager := NewFeedbackManager(db, recorder)
	result := multiagent.Result{Answer: "answer", Plan: multiagent.Plan{Intent: "incident_triage"}}
	snapshot, _ := json.Marshal(result)
	if err := manager.RecordInteraction(context.Background(), InteractionRecord{ID: "x", UserID: "u", SessionID: "s", Mode: "multi-agent", Status: "completed", Query: "q", Answer: "answer", Snapshot: string(snapshot)}); err != nil {
		t.Fatal(err)
	}
	outcome, err := manager.Submit(context.Background(), "x", "u", "s", FeedbackUp, "")
	if err != nil || outcome.PromotionStatus != "failed" {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	var feedback InteractionFeedback
	if err := db.First(&feedback, "interaction_id = ?", "x").Error; err != nil || feedback.Vote != FeedbackUp {
		t.Fatalf("feedback not retained: %+v err=%v", feedback, err)
	}
}
