package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/internal/memory"
)

func testService(t *testing.T) *Service {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.AutoMigrate(&memory.AgentRun{}, &memory.ConversationHistory{}); err != nil {
		t.Fatal(err)
	}
	s, err := NewService(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func seedRun(t *testing.T, s *Service, owner string) memory.AgentRun {
	t.Helper()
	now := time.Now()
	id := uuid.NewString()
	events, _ := json.Marshal([]progress.Event{{Kind: "tool", Name: "query_metrics", Status: "completed", Sequence: 2, Timestamp: now, Result: map[string]any{"success": true, "latency_ms": 14000}}})
	r := memory.AgentRun{ID: id, UserID: owner, SessionID: "session", Query: "pilot-api 慢请求", Answer: "模型调用耗时增加，尚未验证修复", Status: "completed", EventsJSON: string(events), CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	return r
}

type fakeGenerator struct {
	calls   int
	fail    bool
	noDraft bool
	invalid bool
}

func (g *fakeGenerator) Generate(_ context.Context, source Source) ([]Candidate, error) {
	g.calls++
	if g.fail {
		return nil, errors.New("provider failure")
	}
	if g.noDraft {
		return []Candidate{}, nil
	}
	id := source.Evidence[0].ID
	if g.invalid {
		id = "invented:900"
	}
	return []Candidate{{Title: "pilot-api 模型调用延迟", Service: "pilot-api", Tags: []string{"LLM"}, Phenomenon: "调用延迟 14s", RootCause: "待查下游延迟原因", CauseStatus: "hypothesis", Solution: "建议缩小观测窗口；尚未实施修复", Validation: "尚未验证", Verification: "unverified", EvidenceIDs: []string{id}}}, nil
}
func candidateOf(d Detail) Candidate {
	return Candidate{Title: d.Version.Title, Service: d.Version.Service, Tags: d.Tags, Phenomenon: d.Version.Phenomenon, RootCause: d.Version.RootCause, CauseStatus: d.Version.CauseStatus, Solution: d.Version.Solution, Validation: d.Version.Validation, Verification: d.Version.Verification}
}

func TestExperienceDraftPublishRevisionAndAccountIsolation(t *testing.T) {
	ctx := context.Background()
	s := testService(t)
	run := seedRun(t, s, "owner-a")
	job, err := s.Enqueue(ctx, "owner-a", run.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	dup, err := s.Enqueue(ctx, "owner-a", run.ID, true)
	if err != nil || dup.ID != job.ID {
		t.Fatalf("idempotence: %+v %v", dup, err)
	}
	if _, err := s.Enqueue(ctx, "owner-b", run.ID, false); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("cross-account enqueue: %v", err)
	}
	g := &fakeGenerator{}
	w := NewWorker(s, g)
	if err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := s.List(ctx, "owner-a", Filter{Tab: "pending"})
	if err != nil || len(items.Items) != 1 || items.Counts["pending"] != 1 {
		t.Fatalf("draft list: %+v %v", items, err)
	}
	d := items.Items[0]
	if d.Version.ReviewStatus != "pending" || d.Entry.Lifecycle != "draft" || d.Version.Verification != "unverified" || len(d.Evidence) != 1 {
		t.Fatalf("draft: %+v", d)
	}
	if docs, _ := s.SearchPublished(ctx, "owner-a", "pilot-api", "", 3); len(docs) != 0 {
		t.Fatal("draft leaked into retrieval")
	}
	edit := Edit{Revision: d.Entry.Revision, Action: "publish", Candidate: candidateOf(d)}
	edit.RootCause = "已修改的假设"
	published, err := s.Update(ctx, "owner-a", d.Entry.ID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if published.Version.Verification != "unverified" || published.Version.ReviewStatus != "approved" {
		t.Fatal("approval changed technical verification")
	}
	if _, err := s.Update(ctx, "owner-a", d.Entry.ID, edit); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate/stale submit: %v", err)
	}
	for _, action := range []string{"save", "publish", "revise", "archive", "reject", "restore"} {
		if _, err := s.Update(ctx, "owner-b", d.Entry.ID, Edit{Revision: published.Entry.Revision, Action: action}); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("cross-account %s: %v", action, err)
		}
	}
	if docs, _ := s.SearchPublished(ctx, "owner-b", "pilot-api", "", 3); len(docs) != 0 {
		t.Fatal("another account retrieved knowledge")
	}
	revision, err := s.Update(ctx, "owner-a", d.Entry.ID, Edit{Revision: published.Entry.Revision, Action: "revise"})
	if err != nil {
		t.Fatal(err)
	}
	if revision.Version.Number != 2 || revision.Entry.PublishedVersion != 1 || revision.Version.ReviewStatus != "pending" {
		t.Fatalf("new version: %+v", revision)
	}
	change := Edit{Revision: revision.Entry.Revision, Action: "save", Candidate: candidateOf(revision)}
	change.Title = "修改中的新版本"
	revision, err = s.Update(ctx, "owner-a", d.Entry.ID, change)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := s.SearchPublished(ctx, "owner-a", "pilot-api", "", 3)
	if err != nil || len(docs) != 1 || docs[0].Version.Number != 1 || docs[0].Version.Title == change.Title {
		t.Fatalf("unpublished revision affected retrieval: %+v %v", docs, err)
	}
	rejected, err := s.Update(ctx, "owner-a", d.Entry.ID, Edit{Revision: revision.Entry.Revision, Action: "reject", Note: "信息不足"})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Entry.Lifecycle != "published" || rejected.Entry.PublishedVersion != 1 {
		t.Fatal("rejecting revision archived published knowledge")
	}
	archived, err := s.Update(ctx, "owner-a", d.Entry.ID, Edit{Revision: rejected.Entry.Revision, Action: "archive", Note: "环境已迁移"})
	if err != nil {
		t.Fatal(err)
	}
	if archived.Entry.Lifecycle != "archived" {
		t.Fatal("archive missing")
	}
	if docs, _ := s.SearchPublished(ctx, "owner-a", "pilot-api", "", 3); len(docs) != 0 {
		t.Fatal("archived knowledge retrieved")
	}
	versions, err := s.Versions(ctx, "owner-a", d.Entry.ID)
	if err != nil || len(versions) != 2 || versions[1].Version.ReviewStatus != "approved" {
		t.Fatalf("history lost: %+v %v", versions, err)
	}
	if err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if g.calls != 1 {
		t.Fatal("same run regenerated unnecessarily")
	}
	// Source snapshots survive deletion of the UI run journal.
	if err := s.db.Delete(&run).Error; err != nil {
		t.Fatal(err)
	}
	again, err := s.Get(ctx, "owner-a", d.Entry.ID, 1)
	if err != nil || len(again.Evidence) != 1 {
		t.Fatal("source cleanup lost evidence")
	}
}

func TestRestorePublishedKnowledgePreservesPendingRevision(t *testing.T) {
	ctx := context.Background()
	s := testService(t)
	run := seedRun(t, s, "owner")
	if _, err := s.Enqueue(ctx, "owner", run.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := NewWorker(s, &fakeGenerator{}).ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, "owner", Filter{Tab: "pending"})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("draft: %+v %v", list, err)
	}
	d := list.Items[0]
	published, err := s.Update(ctx, "owner", d.Entry.ID, Edit{Revision: d.Entry.Revision, Action: "publish", Candidate: candidateOf(d)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, "owner", d.Entry.ID, Edit{Revision: published.Entry.Revision, Action: "restore"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-archived restore: %v", err)
	}
	pending, err := s.Update(ctx, "owner", d.Entry.ID, Edit{Revision: published.Entry.Revision, Action: "revise"})
	if err != nil {
		t.Fatal(err)
	}
	edit := Edit{Revision: pending.Entry.Revision, Action: "save", Candidate: candidateOf(pending)}
	edit.Title = "尚未审核的新结论"
	pending, err = s.Update(ctx, "owner", d.Entry.ID, edit)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := s.Update(ctx, "owner", d.Entry.ID, Edit{Revision: pending.Entry.Revision, Action: "archive", Note: "暂时停用"})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"save", "publish", "revise", "reject"} {
		if _, err := s.Update(ctx, "owner", d.Entry.ID, Edit{Revision: archived.Entry.Revision, Action: action, Candidate: candidateOf(pending)}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("archived %s bypass: %v", action, err)
		}
	}
	if _, err := s.Update(ctx, "other", d.Entry.ID, Edit{Revision: archived.Entry.Revision, Action: "restore"}); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("cross-account restore: %v", err)
	}
	if _, err := s.Update(ctx, "owner", d.Entry.ID, Edit{Revision: archived.Entry.Revision - 1, Action: "restore"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale restore: %v", err)
	}
	if docs, err := s.SearchPublished(ctx, "owner", "pilot-api", "", 3); err != nil || len(docs) != 0 {
		t.Fatalf("archived retrieval: %+v %v", docs, err)
	}
	restored, err := s.Update(ctx, "owner", d.Entry.ID, Edit{Revision: archived.Entry.Revision, Action: "restore", Candidate: Candidate{Title: "不能通过恢复修改内容", Verification: "verified"}})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Entry.Lifecycle != "published" || restored.Entry.PublishedVersion != 1 || restored.Entry.LatestVersion != 2 || restored.Entry.Revision != archived.Entry.Revision+1 || restored.Entry.ArchiveReason != "暂时停用" {
		t.Fatalf("restored state: %+v", restored.Entry)
	}
	if !reflect.DeepEqual(restored.Version, pending.Version) {
		t.Fatal("restore mutated or approved the pending revision")
	}
	docs, err := s.SearchPublished(ctx, "owner", "pilot-api", "", 3)
	if err != nil || len(docs) != 1 || !reflect.DeepEqual(docs[0].Version, published.Version) {
		t.Fatalf("restore must retrieve unchanged approved v1: %+v %v", docs, err)
	}
	if _, err := s.Update(ctx, "owner", d.Entry.ID, Edit{Revision: archived.Entry.Revision, Action: "restore"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate restore: %v", err)
	}
	versions, err := s.Versions(ctx, "owner", d.Entry.ID)
	if err != nil || len(versions) != 2 {
		t.Fatalf("restore changed version history: %+v %v", versions, err)
	}
}

func TestRestoreRequiresAnApprovedPublishedVersion(t *testing.T) {
	for _, test := range []struct {
		name   string
		number int
		status string
	}{
		{"no published version", 0, ""},
		{"missing published version", 1, ""},
		{"pending published version", 1, "pending"},
		{"rejected published version", 1, "rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := testService(t)
			e := Entry{ID: uuid.NewString(), OwnerID: "owner", Lifecycle: "archived", Revision: 1, LatestVersion: 2, PublishedVersion: test.number}
			if err := s.db.Create(&e).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.db.Create(&Version{EntryID: e.ID, Number: 2, ReviewStatus: "pending"}).Error; err != nil {
				t.Fatal(err)
			}
			if test.status != "" {
				if err := s.db.Create(&Version{EntryID: e.ID, Number: 1, ReviewStatus: test.status}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Update(context.Background(), "owner", e.ID, Edit{Revision: 1, Action: "restore"}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("unsafe restore accepted: %v", err)
			}
			var after Entry
			if err := s.db.First(&after, "id = ?", e.ID).Error; err != nil {
				t.Fatal(err)
			}
			if after.Lifecycle != "archived" || after.Revision != 1 {
				t.Fatal("failed restore changed entry")
			}
		})
	}
}

func TestKnowledgeRetriesLeaseRecoveryAndEvidenceValidation(t *testing.T) {
	ctx := context.Background()
	s := testService(t)
	run := seedRun(t, s, "owner")
	job, err := s.Enqueue(ctx, "owner", run.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGenerator{invalid: true}
	w := NewWorker(s, g)
	for i := 0; i < 3; i++ {
		if err := w.ProcessOne(ctx); err != nil {
			t.Fatal(err)
		}
		s.db.Model(&Job{}).Where("id = ?", job.ID).Update("next_attempt_at", time.Now().Add(-time.Second))
	}
	failed, _ := s.GetJob(ctx, "owner", run.ID)
	if failed.Status != "failed" || failed.DraftCount != 0 {
		t.Fatalf("invalid evidence accepted: %+v", failed)
	}
	if list, _ := s.List(ctx, "owner", Filter{}); len(list.Items) != 0 {
		t.Fatal("failed extraction saved partial drafts")
	}
	g.invalid = false
	if _, err := s.Enqueue(ctx, "owner", run.ID, true); err != nil {
		t.Fatal(err)
	}
	// Simulate a worker dying after taking a lease.
	past := time.Now().Add(-time.Minute)
	if err := s.db.Model(&Job{}).Where("id = ?", job.ID).Updates(map[string]any{"status": "running", "lease_token": "stale", "lease_until": past}).Error; err != nil {
		t.Fatal(err)
	}
	if err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	done, _ := s.GetJob(ctx, "owner", run.ID)
	if done.Status != "succeeded" || done.DraftCount != 1 {
		t.Fatalf("restart recovery: %+v", done)
	}
	source, err := s.BuildSource(ctx, "owner", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = w.complete(ctx, Job{ID: job.ID, OwnerID: "owner", RunID: run.ID, LeaseToken: "stale"}, source, []Candidate{})
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("stale worker completion unfenced: %v", err)
	}
	if list, _ := s.List(ctx, "owner", Filter{}); len(list.Items) != 1 {
		t.Fatal("recovery duplicated drafts")
	}
}

func TestAutomaticReconciliationAndNoDraft(t *testing.T) {
	ctx := context.Background()
	s := testService(t)
	r := seedRun(t, s, "owner")
	// Keep the pre-enable run strictly before the setting's timestamp even
	// when the wall clock returns the same value for consecutive operations.
	r.CreatedAt = time.Now().Add(-time.Minute)
	if err := s.db.Model(&r).UpdateColumn("created_at", r.CreatedAt).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.MaybeEnqueue(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetJob(ctx, "owner", r.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal("disabled automatic collection created job")
	}
	if _, err := s.SetAutomatic(ctx, "owner", true); err != nil {
		t.Fatal(err)
	}
	r = seedRun(t, s, "owner")
	w := NewWorker(s, &fakeGenerator{noDraft: true})
	if err := w.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	job, err := s.GetJob(ctx, "owner", r.ID)
	if err != nil || job.Status != "no_draft" {
		t.Fatalf("empty candidate: %+v %v", job, err)
	}
	if err := w.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var count int64
	s.db.Model(&Job{}).Count(&count)
	if count != 1 {
		t.Fatal(fmt.Sprintf("retroactive or duplicate jobs: %d", count))
	}
}
