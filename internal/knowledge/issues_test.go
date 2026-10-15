package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/internal/memory"
)

type issueTestGenerator struct {
	tracks, generates int
	fail              bool
	action            string
	seen              []Source
}

func (g *issueTestGenerator) Track(_ context.Context, source Source, existing []Issue) (TrackResult, error) {
	g.tracks++
	g.seen = append(g.seen, source)
	name := "MySQL"
	if strings.Contains(source.Run.Query, "Redis") {
		name = "Redis"
	}
	id := "new_" + name
	summary := ""
	for _, i := range existing {
		if i.Title == name+" timeout" {
			id = i.ID
			summary = i.Summary
		}
	}
	ids := []string{}
	for id := range eventKinds(source) {
		ids = append(ids, id)
	}
	status, quote := "investigating", ""
	if strings.Contains(source.Run.Query, "已解决") {
		status = "resolved"
		quote = name + "故障已解决"
	}
	return TrackResult{Issues: []IssueUpdate{{ID: id, Title: name + " timeout", Service: name, Summary: summary + "|" + source.Run.Query, Status: status, ConfirmationQuote: quote, EventIDs: ids}}, Unassigned: []string{}}, nil
}
func (g *issueTestGenerator) Generate(_ context.Context, source Source) ([]Candidate, error) {
	g.generates++
	if source.Run.Query != "" || source.Run.Answer != "" {
		return nil, errors.New("knowledge source leaked the unrelated triggering Run")
	}
	if g.fail {
		return nil, errors.New("model unavailable")
	}
	i := source.Issues[0]
	p := Candidate{Action: "CREATE", IssueID: i.ID, Title: i.Title, Service: i.Service, Phenomenon: i.Summary, RootCause: "假设连接耗尽", CauseStatus: "hypothesis", Solution: "用户报告已修复；不能推断其他环境", Validation: i.ResolutionBasis, Conditions: "仅限本案例环境", Verification: "unverified"}
	for _, e := range source.Evidence {
		p.EvidenceIDs = append(p.EvidenceIDs, e.ID)
	}
	if g.action != "" {
		p.Action = g.action
	}
	for _, existing := range source.Existing {
		if existing.Version.Service == i.Service {
			p.TargetID = existing.Entry.ID
			p.TargetRevision = existing.Entry.Revision
			if g.action == "" {
				p.Action = "ENRICH"
			}
			break
		}
	}
	return []Candidate{p}, nil
}
func issueRun(t *testing.T, s *Service, session, query, tool string) memory.AgentRun {
	t.Helper()
	r := seedRun(t, s, "owner")
	r.SessionID = session
	r.Query = query
	events, _ := json.Marshal([]progress.Event{{Kind: "tool", Name: tool, Status: "completed", Sequence: 2, Timestamp: time.Now(), Result: map[string]any{"diagnostic": tool}}})
	r.EventsJSON = string(events)
	if err := s.db.Save(&r).Error; err != nil {
		t.Fatal(err)
	}
	return r
}
func queueIssue(t *testing.T, s *Service, w *Worker, r memory.AgentRun) {
	t.Helper()
	if _, err := s.Enqueue(context.Background(), "owner", r.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := w.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func enableIssues(t *testing.T, s *Service) {
	t.Helper()
	if err := s.EnableIssueTracking(); err != nil {
		t.Fatal(err)
	}
}

func TestInterleavedIssuesIncrementalAndVersionedKnowledge(t *testing.T) {
	ctx := context.Background()
	s := testService(t)
	enableIssues(t, s)
	g := &issueTestGenerator{}
	w := NewWorker(s, g)
	a := issueRun(t, s, "session", "MySQL连接超时，继续排查", "mysql_metrics")
	queueIssue(t, s, w, a)
	b := issueRun(t, s, "session", "Redis故障已解决，恢复正常", "redis_metrics")
	queueIssue(t, s, w, b)
	c := issueRun(t, s, "session", "回到 MySQL：MySQL故障已解决，验证通过", "mysql_logs")
	queueIssue(t, s, w, c)
	issues, err := s.ListIssues(ctx, "owner", "session")
	if err != nil || len(issues) != 2 {
		t.Fatalf("issues %+v %v", issues, err)
	}
	var mysql, redis IssueView
	for _, i := range issues {
		if i.Service == "MySQL" {
			mysql = i
		} else {
			redis = i
		}
	}
	if mysql.Revision != 2 || mysql.EvidenceCount != 2 || redis.Revision != 1 || redis.EvidenceCount != 1 || strings.Contains(mysql.Summary, "Redis") {
		t.Fatalf("incorrect partition %+v %+v", mysql, redis)
	}
	for _, source := range g.seen {
		if len(source.Conversation) != 0 || len(source.Evidence) != 1 {
			t.Fatal("tracker replayed history")
		}
	}
	if g.generates != 2 {
		t.Fatalf("premature drafts %d", g.generates)
	}
	for _, i := range issues {
		for _, ref := range i.Events {
			if ref.Kind == "evidence" && i.Service == "MySQL" && ref.RunID == b.ID {
				t.Fatal("redis evidence assigned to mysql")
			}
		}
	}
	pending, err := s.List(ctx, "owner", Filter{Tab: "pending"})
	if err != nil || len(pending.Items) != 2 {
		t.Fatalf("drafts %v %v", pending, err)
	}
	var original Detail
	for _, d := range pending.Items {
		published, e := s.Update(ctx, "owner", d.Entry.ID, Edit{Revision: d.Entry.Revision, Action: "publish", Candidate: candidateOf(d)})
		if e != nil {
			t.Fatal(e)
		}
		if d.Version.Service == "MySQL" {
			original = published
		}
	}
	g.action = "LINK"
	d := issueRun(t, s, "next", "MySQL故障已解决，症状和上次相同", "mysql_metrics")
	queueIssue(t, s, w, d)
	all, _ := s.List(ctx, "owner", Filter{})
	if len(all.Items) != 2 {
		t.Fatal("LINK duplicated knowledge")
	}
	linked, _ := s.ListIssues(ctx, "owner", "next")
	if len(linked) != 1 || len(linked[0].Knowledge) != 1 {
		t.Fatal("case provenance missing")
	}
	g.action = "ENRICH"
	e := issueRun(t, s, "next", "MySQL故障已解决，补充新环境差异", "mysql_logs")
	queueIssue(t, s, w, e)
	revised, err := s.Get(ctx, "owner", original.Entry.ID, 0)
	if err != nil || revised.Version.Number != 2 || revised.Version.ReviewStatus != "pending" || revised.Entry.PublishedVersion != 1 {
		t.Fatalf("revision %+v %v", revised, err)
	}
	docs, err := s.SearchPublished(ctx, "owner", "MySQL", "", 3)
	if err != nil || len(docs) != 1 || docs[0].Version.Number != 1 {
		t.Fatalf("published retrieval %+v %v", docs, err)
	}
	calls := g.generates
	if err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if g.generates != calls {
		t.Fatal("completed job reran")
	}
}

func TestManualSessionOrganizationWorksWithoutAutomaticDrafts(t *testing.T) {
	ctx := context.Background()
	s := testService(t)
	enableIssues(t, s)
	if _, err := s.SetAutomatic(ctx, "owner", false); err != nil {
		t.Fatal(err)
	}
	g := &issueTestGenerator{}
	w := NewWorker(s, g)
	r := issueRun(t, s, "manual-session", "MySQL连接超时，尚未解决", "mysql_logs")
	if err := s.MaybeEnqueue(ctx, r); err != nil {
		t.Fatal(err)
	}
	process := func() {
		t.Helper()
		// Make queue readiness independent of the database clock's precision.
		if err := s.db.Model(&Job{}).Where("owner_id = ? AND run_id = ?", "owner", r.ID).Update("next_attempt_at", time.Now().Add(-time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		if err := w.ProcessOne(ctx); err != nil {
			t.Fatal(err)
		}
	}
	process()
	job, err := s.GetJob(ctx, "owner", r.ID)
	if err != nil || !job.Tracked || job.Manual || g.tracks != 1 || g.generates != 0 {
		t.Fatalf("automatic-disabled path must only track: job=%+v tracks=%d generates=%d err=%v", job, g.tracks, g.generates, err)
	}
	// This is the service entry used by the page's organize/close endpoints.
	if _, err := s.FinalizeSession(ctx, "owner", r.SessionID); err != nil {
		t.Fatal(err)
	}
	process()
	drafts, err := s.List(ctx, "owner", Filter{Tab: "pending"})
	if err != nil || len(drafts.Items) != 1 || drafts.Items[0].Entry.PublishedVersion != 0 || g.tracks != 1 || g.generates != 1 {
		t.Fatalf("manual entry must produce one unpublished draft without retracking: drafts=%+v tracks=%d generates=%d err=%v", drafts, g.tracks, g.generates, err)
	}
	if _, err := s.FinalizeSession(ctx, "owner", r.SessionID); err != nil {
		t.Fatal(err)
	}
	process()
	if g.generates != 1 {
		t.Fatal("repeating the page action regenerated an already organized Issue")
	}
}

func TestKnowledgeFailurePreservesTrackingAndRetryDoesNotReattribute(t *testing.T) {
	ctx := context.Background()
	s := testService(t)
	enableIssues(t, s)
	g := &issueTestGenerator{fail: true}
	w := NewWorker(s, g)
	run := issueRun(t, s, "session", "MySQL故障已解决，恢复正常", "mysql_metrics")
	queueIssue(t, s, w, run)
	job, _ := s.GetJob(ctx, "owner", run.ID)
	if !job.Tracked || job.Status != "pending" {
		t.Fatalf("tracking lost %+v", job)
	}
	before, _ := s.ListIssues(ctx, "owner", "session")
	if len(before) != 1 || before[0].Revision != 1 {
		t.Fatal("tracking not durable before failure")
	}
	g.fail = false
	s.db.Model(&Job{}).Where("id = ?", job.ID).Update("next_attempt_at", time.Now().Add(-time.Second))
	if err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := s.ListIssues(ctx, "owner", "session")
	if g.tracks != 1 || len(after[0].Events) != len(before[0].Events) || after[0].Revision != 1 || len(after[0].Knowledge) != 1 {
		t.Fatal("retry repeated tracking or lost draft")
	}
	// Persisted cutover survives process restart, including missing completion hooks.
	restarted, err := NewService(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	enableIssues(t, restarted)
	if !restarted.trackingSince.Equal(s.trackingSince) {
		t.Fatal("restart loses recovery cutoff")
	}
	missed := issueRun(t, s, "another", "Redis故障已解决，恢复正常", "redis_metrics")
	other := NewWorker(restarted, g)
	if err := other.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetJob(ctx, "owner", missed.ID); err != nil {
		t.Fatal("missing hook not recovered")
	}
}

func TestIssueCorrectionsAndCoverageValidation(t *testing.T) {
	ctx := context.Background()
	s := testService(t)
	enableIssues(t, s)
	g := &issueTestGenerator{}
	w := NewWorker(s, g)
	r := issueRun(t, s, "session", "MySQL正在排查", "mysql_metrics")
	queueIssue(t, s, w, r)
	items, _ := s.ListIssues(ctx, "owner", "session")
	i := items[0]
	if _, err := s.EditIssue(ctx, "other", i.ID, IssueEdit{Action: "rename", Revision: i.Revision, Title: "偷改"}); err == nil {
		t.Fatal("cross-owner edit")
	}
	input := IssueEdit{Action: "split", Revision: i.Revision, Title: "连接池独立问题", EventIDs: []string{r.ID + ":2"}}
	if _, err := s.EditIssue(ctx, "owner", i.ID, input); err != nil {
		t.Fatal(err)
	}
	split, _ := s.ListIssues(ctx, "owner", "session")
	if len(split) != 2 || split[0].EvidenceCount != 0 || split[1].EvidenceCount != 1 {
		t.Fatalf("split incorrect %+v", split)
	}
	if _, err := s.EditIssue(ctx, "owner", i.ID, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale edit accepted %v", err)
	}
	if _, err := s.EditIssue(ctx, "owner", split[1].ID, IssueEdit{Action: "merge", Revision: split[1].Revision, TargetID: split[0].ID, TargetRevision: split[0].Revision}); err != nil {
		t.Fatal(err)
	}
	merged, _ := s.ListIssues(ctx, "owner", "session")
	if len(merged) != 1 || merged[0].EvidenceCount != 1 {
		t.Fatal("merge lost or duplicated evidence")
	}
	source, _ := s.BuildSource(ctx, "owner", r.ID)
	bad := TrackResult{Issues: []IssueUpdate{{ID: "new_a", Title: "a", Summary: "a", Status: "resolved", EventIDs: []string{r.ID + ":user"}}}, Unassigned: []string{r.ID + ":assistant", r.ID + ":2"}}
	if validateTracking(source, nil, bad) == nil {
		t.Fatal("unconfirmed repair accepted")
	}
	bad.Issues[0].ID = "ISS-existing"
	if validateTracking(source, []Issue{{ID: "ISS-existing", Status: "investigating"}}, bad) == nil {
		t.Fatal("existing investigating issue became resolved without confirmation")
	}
	if err := validateTracking(source, []Issue{{ID: "ISS-existing", Status: "resolved"}}, bad); err != nil {
		t.Fatalf("already-confirmed resolution should survive an unrelated round: %v", err)
	}
	bad.Issues[0].Status = "investigating"
	bad.Issues[0].ID = "new_a"
	bad.Unassigned = nil
	if validateTracking(source, nil, bad) == nil {
		t.Fatal("dropped observations accepted")
	}
}

func TestLaterRunWaitsForMissingEarlierTracking(t *testing.T) {
	ctx := context.Background()
	s := testService(t)
	enableIssues(t, s)
	g := &issueTestGenerator{}
	w := NewWorker(s, g)
	earlier := issueRun(t, s, "session", "MySQL初始问题", "mysql_metrics")
	later := issueRun(t, s, "session", "MySQL补充问题", "mysql_logs")
	if _, err := s.Enqueue(ctx, "owner", later.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if g.tracks != 0 {
		t.Fatal("newer run overtook missing earlier hook")
	}
	if _, err := s.Enqueue(ctx, "owner", earlier.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if g.tracks != 2 || g.seen[0].Run.ID != earlier.ID || g.seen[1].Run.ID != later.ID {
		t.Fatal("out-of-order incremental tracking")
	}
}

type multiIssueGenerator struct {
	issueTestGenerator
	failRedis bool
}

func (g *multiIssueGenerator) Track(_ context.Context, source Source, _ []Issue) (TrackResult, error) {
	g.tracks++
	return TrackResult{Issues: []IssueUpdate{
		{ID: "new_mysql", Title: "MySQL timeout", Service: "MySQL", Summary: "MySQL独立问题", Status: "resolved", ConfirmationQuote: "MySQL故障已解决", EventIDs: []string{source.Run.ID + ":user", source.Run.ID + ":assistant", source.Run.ID + ":2"}},
		{ID: "new_redis", Title: "Redis timeout", Service: "Redis", Summary: "Redis独立问题", Status: "resolved", ConfirmationQuote: "Redis故障已解决", EventIDs: []string{source.Run.ID + ":user", source.Run.ID + ":assistant", source.Run.ID + ":3"}},
	}, Unassigned: []string{}}, nil
}
func (g *multiIssueGenerator) Generate(ctx context.Context, source Source) ([]Candidate, error) {
	if source.Issues[0].Service == "Redis" && g.failRedis {
		return nil, errors.New("second issue provider failure")
	}
	return g.issueTestGenerator.Generate(ctx, source)
}
func TestPartialKnowledgeCompletionSurvivesRetryAndSharedMessages(t *testing.T) {
	ctx := context.Background()
	s := testService(t)
	enableIssues(t, s)
	g := &multiIssueGenerator{failRedis: true}
	w := NewWorker(s, g)
	r := issueRun(t, s, "session", "MySQL故障已解决，Redis故障已解决", "mysql_metrics")
	events, _ := json.Marshal([]progress.Event{{Kind: "tool", Name: "mysql_metrics", Status: "completed", Sequence: 2, Timestamp: time.Now(), Result: map[string]any{"service": "mysql"}}, {Kind: "tool", Name: "redis_metrics", Status: "completed", Sequence: 3, Timestamp: time.Now(), Result: map[string]any{"service": "redis"}}})
	r.EventsJSON = string(events)
	s.db.Save(&r)
	queueIssue(t, s, w, r)
	issues, _ := s.ListIssues(ctx, "owner", "session")
	if len(issues) != 2 || len(issues[0].Events) != 3 || len(issues[1].Events) != 3 {
		t.Fatal("shared messages/evidence attribution incorrect")
	}
	partial, _ := s.List(ctx, "owner", Filter{})
	if len(partial.Items) != 1 {
		t.Fatal("successful first proposal rolled back with second model failure")
	}
	j, _ := s.GetJob(ctx, "owner", r.ID)
	g.failRedis = false
	s.db.Model(&Job{}).Where("id = ?", j.ID).Update("next_attempt_at", time.Now().Add(-time.Second))
	if err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	done, _ := s.List(ctx, "owner", Filter{})
	if len(done.Items) != 2 || g.tracks != 1 || g.generates != 2 {
		t.Fatal("retry duplicated finished problem")
	}
	for _, d := range done.Items {
		if len(d.Evidence) != 1 {
			t.Fatal("cross-issue evidence combined")
		}
	}
}

func TestUnknownEventsCanBeAssignedWithoutLossOrCrossSession(t *testing.T) {
	ctx := context.Background()
	s := testService(t)
	enableIssues(t, s)
	g := &issueTestGenerator{}
	w := NewWorker(s, g)
	r := issueRun(t, s, "session", "MySQL继续排查", "mysql_metrics")
	queueIssue(t, s, w, r)
	extra := IssueAssociation{OwnerID: "owner", RunID: r.ID, EventID: r.ID + ":unknown", Kind: "message"}
	if err := s.db.Create(&extra).Error; err != nil {
		t.Fatal(err)
	}
	issues, _ := s.ListIssues(ctx, "owner", "session")
	if _, err := s.EditIssue(ctx, "owner", issues[0].ID, IssueEdit{Action: "assign", Revision: issues[0].Revision, EventIDs: []string{extra.EventID}}); err != nil {
		t.Fatal(err)
	}
	unknown, _ := s.UnassignedEvents(ctx, "owner", "session")
	if len(unknown) != 0 {
		t.Fatal("assigned event retained as unassigned")
	}
	after, _ := s.ListIssues(ctx, "owner", "session")
	if len(after[0].Events) != 4 {
		t.Fatal("assignment lost original events")
	}
	if events, _ := s.UnassignedEvents(ctx, "other", "session"); len(events) != 0 {
		t.Fatal("unknown events leaked")
	}
}
