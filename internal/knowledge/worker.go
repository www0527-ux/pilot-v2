package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/internal/identity"
	"shanyangcode.com/pilot/internal/memory"
)

type Generator interface {
	Generate(context.Context, Source) ([]Candidate, error)
}
type Worker struct {
	service   *Service
	generator Generator
}

func NewWorker(s *Service, g Generator) *Worker { return &Worker{service: s, generator: g} }

// Durable polling queue; conditional lease + fenced completion allow restart
// recovery and multiple workers without duplicate drafts. No additional MQ.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	reconcile := time.NewTicker(20 * time.Second)
	defer reconcile.Stop()
	for {
		if err := w.ProcessOne(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("knowledge worker failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-reconcile.C:
			if err := w.Reconcile(ctx); err != nil {
				slog.Warn("knowledge reconciliation failed", "error", err)
			}
		case <-ticker.C:
		}
	}
}
func (w *Worker) Reconcile(ctx context.Context) error {
	if w.service.issueTracking {
		var runs []memory.AgentRun
		if err := w.service.db.WithContext(ctx).Table("agent_runs r").Select("r.*").Joins("LEFT JOIN experience_jobs j ON j.owner_id = r.user_id AND j.run_id = r.id").Where("(j.id IS NULL OR j.status = ?) AND r.created_at >= ? AND r.status IN ?", "waiting_snapshot", w.service.trackingSince, []string{"completed", "failed", "cancelled", "interrupted"}).Order("r.created_at ASC").Limit(20).Find(&runs).Error; err != nil {
			return err
		}
		for _, r := range runs {
			if _, err := w.service.Enqueue(ctx, r.UserID, r.ID, false); err != nil {
				return err
			}
		}
		return nil
	}
	var runs []memory.AgentRun
	err := w.service.db.WithContext(ctx).Table("agent_runs r").Select("r.*").Joins("JOIN experience_settings s ON s.owner_id = r.user_id AND s.automatic = ?", true).Joins("LEFT JOIN experience_jobs j ON j.owner_id = r.user_id AND j.run_id = r.id").Where("j.id IS NULL AND r.created_at >= s.enabled_at AND r.status IN ?", []string{"completed", "failed", "cancelled", "interrupted"}).Order("r.created_at ASC").Limit(20).Find(&runs).Error
	if err != nil {
		return err
	}
	for _, r := range runs {
		if _, err := w.service.Enqueue(ctx, r.UserID, r.ID, false); err != nil {
			return err
		}
	}
	return nil
}
func (w *Worker) ProcessOne(ctx context.Context) error {
	now := time.Now()
	var job Job
	queue := w.service.db.WithContext(ctx).Model(&Job{}).Where("(status = ? AND next_attempt_at <= ?) OR (status = ? AND lease_until < ?)", "pending", now, "running", now)
	if w.service.issueTracking {
		// Never update a Session from a later Run while an earlier Run still
		// needs tracking. A failed predecessor is visible/retryable, not skipped.
		queue = queue.Where("NOT EXISTS (SELECT 1 FROM experience_jobs older WHERE older.owner_id = experience_jobs.owner_id AND older.session_id = experience_jobs.session_id AND older.tracked = ? AND older.status IN ? AND (older.source_at < experience_jobs.source_at OR (older.source_at = experience_jobs.source_at AND older.id < experience_jobs.id)))", false, []string{"pending", "running", "failed"})
		queue = queue.Where("NOT EXISTS (SELECT 1 FROM agent_runs previous LEFT JOIN experience_jobs pj ON pj.run_id = previous.id AND pj.owner_id = previous.user_id WHERE previous.user_id = experience_jobs.owner_id AND previous.session_id = experience_jobs.session_id AND previous.created_at >= ? AND previous.created_at < experience_jobs.source_at AND (pj.id IS NULL OR pj.tracked = ?))", w.service.trackingSince, false)
	}
	resultFind := queue.Order("created_at ASC").Limit(1).Find(&job)
	err := resultFind.Error
	if resultFind.RowsAffected == 0 && err == nil {
		return nil
	}
	if err != nil {
		return err
	}
	token := uuid.NewString()
	lease := now.Add(3 * time.Minute)
	result := w.service.db.WithContext(ctx).Model(&Job{}).Where("id = ? AND ((status = ? AND next_attempt_at <= ?) OR (status = ? AND lease_until < ?))", job.ID, "pending", now, "running", now).Updates(map[string]any{"status": "running", "lease_token": token, "lease_until": lease, "attempts": gorm.Expr("attempts + 1"), "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return nil
	}
	job.Attempts++
	job.LeaseToken = token
	var source Source
	if err := json.Unmarshal([]byte(job.SnapshotJSON), &source); err != nil {
		return w.fail(ctx, job, err)
	}
	workCtx, cancel := context.WithTimeout(identity.WithOwner(ctx, job.OwnerID), 2*time.Minute)
	defer cancel()
	if tracker, ok := w.generator.(IssueTracker); ok && w.service.issueTracking {
		err := w.processIssueJob(workCtx, job, source, tracker)
		if err != nil {
			return w.fail(ctx, job, err)
		}
		return nil
	}
	candidates, err := safeGenerate(workCtx, w.generator, source)
	if err == nil {
		err = validateGenerated(source, candidates)
	}
	if err != nil {
		return w.fail(ctx, job, err)
	}
	return w.complete(ctx, job, source, candidates)
}
func safeGenerate(ctx context.Context, g Generator, source Source) (candidates []Candidate, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("knowledge generator panic: %v", p)
		}
	}()
	return g.Generate(ctx, source)
}
func validateGenerated(source Source, candidates []Candidate) error {
	if len(candidates) > 3 {
		return fmt.Errorf("%w: 一次最多生成 3 条案例", ErrInvalid)
	}
	available := map[string]Evidence{}
	for _, e := range source.Evidence {
		available[e.ID] = e
	}
	for _, c := range candidates {
		if err := validateCandidate(c); err != nil {
			return err
		}
		for _, id := range c.EvidenceIDs {
			if _, ok := available[id]; !ok {
				return fmt.Errorf("%w: 引用不存在的证据 %s", ErrInvalid, id)
			}
		}
		// Publishing is always human controlled. A diagnostic answer cannot prove
		// the repair was executed. AI drafts start unverified/hypothesis.
		if c.CauseStatus == "confirmed" && len(c.EvidenceIDs) == 0 {
			return fmt.Errorf("%w: 确认根因需要原始证据引用", ErrInvalid)
		}
	}
	return nil
}
func (w *Worker) complete(ctx context.Context, job Job, source Source, candidates []Candidate) error {
	return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Job
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND status = ? AND lease_token = ?", job.ID, "running", job.LeaseToken).First(&current).Error; err != nil {
			return err
		}
		for i, c := range candidates {
			// Keep AI claims conservative until a reviewer supplies verification.
			c.Verification = "unverified"
			if c.CauseStatus == "confirmed" {
				c.CauseStatus = "hypothesis"
			}
			entry := Entry{ID: "KB-" + uuid.NewString(), OwnerID: job.OwnerID, JobID: job.ID, Ordinal: i, Type: "incident_case", SourceRunID: job.RunID, SessionID: source.Run.SessionID, Lifecycle: "draft", Revision: 1, LatestVersion: 1}
			if err := tx.Create(&entry).Error; err != nil {
				return err
			}
			evidence := []Evidence{}
			wanted := map[string]bool{}
			for _, id := range c.EvidenceIDs {
				wanted[id] = true
			}
			for _, e := range source.Evidence {
				if wanted[e.ID] {
					evidence = append(evidence, e)
				}
			}
			v := versionFromCandidate(c, evidence)
			v.EntryID = entry.ID
			v.Number = 1
			if err := tx.Create(&v).Error; err != nil {
				return err
			}
		}
		status := "succeeded"
		if len(candidates) == 0 {
			status = "no_draft"
		}
		return tx.Model(&Job{}).Where("id = ? AND lease_token = ?", job.ID, job.LeaseToken).Updates(map[string]any{"status": status, "draft_count": len(candidates), "error": "", "lease_token": "", "lease_until": nil, "updated_at": time.Now()}).Error
	})
}
func (w *Worker) fail(ctx context.Context, job Job, failure error) error {
	// Shutdown leaves the lease recoverable instead of exhausting attempts.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	status := "pending"
	if job.Attempts >= 3 {
		status = "failed"
	}
	return w.service.db.WithContext(ctx).Model(&Job{}).Where("id = ? AND status = ? AND lease_token = ?", job.ID, "running", job.LeaseToken).Updates(map[string]any{"status": status, "error": progress.Clip(progress.Redact(failure.Error()), 800), "lease_token": "", "lease_until": nil, "next_attempt_at": time.Now().Add(time.Duration(job.Attempts*5) * time.Second), "updated_at": time.Now()}).Error
}
