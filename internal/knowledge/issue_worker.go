package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"shanyangcode.com/pilot/internal/memory"
)

func (w *Worker) processIssueJob(ctx context.Context, job Job, source Source, tracker IssueTracker) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("issue processor panic: %v", p)
		}
	}()
	s := w.service
	if job.SessionID == "" {
		job.SessionID = source.Run.SessionID
		if err := s.db.WithContext(ctx).Model(&Job{}).Where("id = ? AND lease_token = ?", job.ID, job.LeaseToken).Updates(map[string]any{"session_id": job.SessionID, "source_at": source.Run.CreatedAt}).Error; err != nil {
			return err
		}
	}
	if !job.Tracked {
		ledger, e := s.sessionLedger(ctx, job.OwnerID, job.SessionID)
		if e != nil {
			return e
		}
		issues, e := s.loadIssues(ctx, job.OwnerID, job.SessionID)
		if e != nil {
			return e
		}
		if err := s.loadUnassigned(ctx, &source); err != nil {
			return err
		}
		updates, e := tracker.Track(ctx, source, issues)
		if e != nil {
			return e
		}
		if e = validateTracking(source, issues, updates); e != nil {
			return e
		}
		if e = w.commitTracking(ctx, job, source, ledger, issues, updates); e != nil {
			return e
		}
	}
	// Refresh manual intent: a button/tool may have requested organization while
	// the tracking model was running. Never overwrite that request with a snapshot.
	if err := s.db.WithContext(ctx).Where("id = ? AND lease_token = ?", job.ID, job.LeaseToken).First(&job).Error; err != nil {
		return err
	}
	issues, err := s.loadIssues(ctx, job.OwnerID, job.SessionID)
	if err != nil {
		return err
	}
	settings, err := s.GetSettings(ctx, job.OwnerID)
	if err != nil {
		return err
	}
	var touched []string
	_ = json.Unmarshal([]byte(job.IssueIDsJSON), &touched)
	selected := map[string]bool{}
	for _, id := range touched {
		selected[id] = true
	}
	for _, issue := range issues {
		if issue.Revision <= issue.OrganizedRevision {
			continue
		}
		if !job.Manual && (!settings.Automatic || issue.Status != "resolved" || !selected[issue.ID]) {
			continue
		}
		source.Issues = append(source.Issues, issue)
	}
	if len(source.Issues) == 0 {
		return w.finishIssueJob(ctx, job, nil)
	}
	// Process one compact Issue at a time. Evidence is gathered by durable IDs,
	// and the curator pages it through get_source_record rather than replaying chat.
	for _, issue := range source.Issues {
		input, err := s.sourceForIssue(ctx, source, issue)
		if err != nil {
			return err
		}
		candidates, err := safeGenerate(ctx, w.generator, input)
		if err != nil {
			return err
		}
		if len(candidates) > 1 {
			return fmt.Errorf("%w: 一个问题只允许一个整理决策", ErrInvalid)
		}
		if len(candidates) == 0 {
			candidates = []Candidate{{Action: "NOOP", IssueID: issue.ID, Reason: "本次没有新的可复用知识"}}
		}
		for _, p := range candidates {
			if err := validatePlan(input, p); err != nil {
				return err
			}
			err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := lockJob(tx, job); err != nil {
					return err
				}
				var current Issue
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND merged_into = ?", p.IssueID, job.OwnerID, "").First(&current).Error; err != nil {
					return err
				}
				expected := issue.Revision
				if current.Revision != expected {
					return ErrConflict
				}
				if current.OrganizedRevision >= expected {
					return nil
				}
				input, err := s.sourceForIssueTx(tx, source, issue)
				if err != nil {
					return err
				}
				changed, err := applyPlan(tx, job, 0, input, p)
				if err != nil {
					return err
				}
				if changed {
					if err := tx.Model(&Job{}).Where("id = ? AND lease_token = ?", job.ID, job.LeaseToken).Update("draft_count", gorm.Expr("draft_count + 1")).Error; err != nil {
						return err
					}
				}
				if err := tx.Model(&Issue{}).Where("id = ? AND owner_id = ? AND revision = ?", issue.ID, job.OwnerID, expected).Update("organized_revision", expected).Error; err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	return w.finishIssueJob(ctx, job, nil)
}
func lockJob(tx *gorm.DB, job Job) error {
	var v Job
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND status = ? AND lease_token = ?", job.ID, "running", job.LeaseToken).First(&v).Error
	if err == nil && v.Manual && !job.Manual {
		return ErrConflict
	}
	return err
}
func finishIssueJobTx(tx *gorm.DB, job Job, count int) error {
	status := "succeeded"
	if count == 0 {
		status = "no_draft"
	}
	return tx.Model(&Job{}).Where("id = ? AND lease_token = ?", job.ID, job.LeaseToken).Updates(map[string]any{"status": status, "draft_count": count, "error": "", "lease_token": "", "lease_until": nil, "updated_at": time.Now()}).Error
}
func (w *Worker) finishIssueJob(ctx context.Context, job Job, _ []Candidate) error {
	return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockJob(tx, job); err != nil {
			return err
		}
		var current Job
		if err := tx.Where("id = ?", job.ID).First(&current).Error; err != nil {
			return err
		}
		return finishIssueJobTx(tx, job, current.DraftCount)
	})
}
func (s *Service) sourceForIssue(ctx context.Context, source Source, issue Issue) (Source, error) {
	input, err := s.sourceForIssueTx(s.db.WithContext(ctx), source, issue)
	if err != nil {
		return input, err
	}
	existing, err := s.SearchCurator(ctx, issue.OwnerID, issue.Title+" "+issue.Service, 8)
	if err != nil {
		return input, err
	}
	// Always include already linked knowledge, even if its title was manually changed.
	var links []IssueKnowledge
	if err := s.db.WithContext(ctx).Where("owner_id = ? AND issue_id = ?", issue.OwnerID, issue.ID).Find(&links).Error; err != nil {
		return input, err
	}
	seen := map[string]bool{}
	for _, d := range existing {
		seen[d.Entry.ID] = true
	}
	for _, l := range links {
		if !seen[l.EntryID] {
			d, err := s.Get(ctx, issue.OwnerID, l.EntryID, 0)
			if err != nil {
				return input, err
			}
			if d.Entry.Lifecycle != "archived" {
				existing = append(existing, d)
				seen[l.EntryID] = true
			}
		}
	}
	input.Existing = existing
	return input, nil
}
func (s *Service) sourceForIssueTx(tx *gorm.DB, source Source, issue Issue) (Source, error) {
	source.Issues = []Issue{issue}
	// The triggering Run can concern a different Issue (e.g. a manual session
	// audit). Do not present its unrelated question as this Issue's source.
	// Only associated messages, the compact memory and evidence are exposed.
	source.Run.Query, source.Run.Answer = "", ""
	source.Evidence = nil
	source.Conversation = nil
	var refs []IssueAssociation
	if err := tx.Where("owner_id = ? AND issue_id = ?", issue.OwnerID, issue.ID).Order("id DESC").Limit(101).Find(&refs).Error; err != nil {
		return source, err
	}
	if len(refs) > 100 {
		refs = refs[:100]
		source.Limitations = append(source.Limitations, "证据仅加载最近100项；更早证据保留在来源 Run 中")
	}
	wanted := map[string]bool{}
	runs := map[string]bool{}
	for _, r := range refs {
		wanted[r.EventID] = true
		runs[r.RunID] = true
	}
	for id := range runs {
		var j Job
		if err := tx.Select("snapshot_json").Where("owner_id = ? AND run_id = ?", issue.OwnerID, id).First(&j).Error; err != nil {
			return source, err
		}
		var raw Source
		if err := json.Unmarshal([]byte(j.SnapshotJSON), &raw); err != nil {
			return source, err
		}
		for _, e := range raw.Evidence {
			if wanted[e.ID] {
				source.Evidence = append(source.Evidence, e)
			}
		}
		if wanted[id+":user"] {
			source.Conversation = append(source.Conversation, memory.ConversationHistory{UserID: issue.OwnerID, SessionID: issue.SessionID, Role: "user", Content: raw.Run.Query, CreatedAt: raw.Run.CreatedAt})
		}
		if wanted[id+":assistant"] {
			source.Conversation = append(source.Conversation, memory.ConversationHistory{UserID: issue.OwnerID, SessionID: issue.SessionID, Role: "assistant", Content: raw.Run.Answer, CreatedAt: raw.Run.UpdatedAt})
		}
	}
	sort.Slice(source.Evidence, func(i, j int) bool { return source.Evidence[i].ObservedAt.Before(source.Evidence[j].ObservedAt) })
	sort.SliceStable(source.Conversation, func(i, j int) bool { return source.Conversation[i].CreatedAt.Before(source.Conversation[j].CreatedAt) })
	return source, nil
}
func validatePlan(source Source, p Candidate) error {
	if len(source.Issues) != 1 || p.IssueID != source.Issues[0].ID {
		return fmt.Errorf("%w: 整理问题不匹配", ErrInvalid)
	}
	if len([]rune(p.Reason)) > 2400 {
		return ErrInvalid
	}
	switch p.Action {
	case "CREATE", "ENRICH":
		if err := validateGenerated(source, []Candidate{p}); err != nil {
			return err
		}
	case "LINK", "NOOP":
	default:
		return fmt.Errorf("%w: 整理决策不合法", ErrInvalid)
	}
	if p.Action == "LINK" || p.Action == "ENRICH" {
		for _, d := range source.Existing {
			if d.Entry.ID == p.TargetID && d.Entry.Revision == p.TargetRevision && d.Entry.Lifecycle != "archived" {
				return nil
			}
		}
		return fmt.Errorf("%w: 修订目标未读取或版本过期", ErrInvalid)
	}
	if p.TargetID != "" {
		return ErrInvalid
	}
	if p.Action == "CREATE" {
		for _, d := range source.Existing {
			for _, l := range d.Sources {
				if l.IssueID == p.IssueID {
					return fmt.Errorf("%w: 同一问题已经关联知识，请关联或修订", ErrInvalid)
				}
			}
		}
	}
	return nil
}
func applyPlan(tx *gorm.DB, job Job, ordinal int, source Source, p Candidate) (bool, error) {
	if p.Action == "NOOP" {
		return false, nil
	}
	var entry Entry
	var old Version
	originRun := job.RunID
	if len(source.Issues) == 1 && source.Issues[0].LastRunID != "" {
		originRun = source.Issues[0].LastRunID
	}
	if p.Action == "CREATE" {
		var last int
		if err := tx.Model(&Entry{}).Where("job_id = ?", job.ID).Select("COALESCE(MAX(ordinal), -1)").Scan(&last).Error; err != nil {
			return false, err
		}
		ordinal = last + 1
		entry = Entry{ID: "KB-" + uuid.NewString(), OwnerID: job.OwnerID, JobID: job.ID, Ordinal: ordinal, Type: "incident_case", SourceRunID: originRun, SessionID: job.SessionID, Lifecycle: "draft", Revision: 1, LatestVersion: 1}
		if err := tx.Create(&entry).Error; err != nil {
			return false, err
		}
	} else {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ?", p.TargetID, job.OwnerID).First(&entry).Error; err != nil {
			return false, err
		}
		if entry.Revision != p.TargetRevision || entry.Lifecycle == "archived" {
			return false, ErrConflict
		}
		if err := tx.Where("entry_id = ? AND number = ?", entry.ID, entry.LatestVersion).First(&old).Error; err != nil {
			return false, err
		}
	}
	link := IssueKnowledge{OwnerID: job.OwnerID, IssueID: p.IssueID, EntryID: entry.ID, RunID: originRun, SessionID: job.SessionID, Action: p.Action, Reason: p.Reason}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&link).Error; err != nil {
		return false, err
	}
	if p.Action == "LINK" {
		return false, nil
	}
	p.Verification = "unverified"
	if p.CauseStatus == "confirmed" {
		p.CauseStatus = "hypothesis"
	}
	wanted := map[string]bool{}
	for _, id := range p.EvidenceIDs {
		wanted[id] = true
	}
	evidence := []Evidence{}
	for _, e := range source.Evidence {
		if wanted[e.ID] {
			evidence = append(evidence, e)
		}
	}
	v := versionFromCandidate(p, evidence)
	v.EntryID = entry.ID
	v.Number = entry.LatestVersion
	if p.Action == "ENRICH" {
		// Append a proposal rather than overwrite human draft edits or conflicting
		// statements. Published versions remain immutable until explicit review.
		if old.ReviewStatus == "rejected" {
			if entry.PublishedVersion == 0 {
				return false, fmt.Errorf("%w: 已拒绝草稿不能自动恢复，请人工检查", ErrInvalid)
			}
			if err := tx.Where("entry_id = ? AND number = ?", entry.ID, entry.PublishedVersion).First(&old).Error; err != nil {
				return false, err
			}
		}
		var prior []Evidence
		_ = json.Unmarshal([]byte(old.EvidenceJSON), &prior)
		seen := map[string]bool{}
		for _, e := range evidence {
			seen[e.ID] = true
		}
		for _, e := range prior {
			if !seen[e.ID] {
				evidence = append(evidence, e)
			}
		}
		data, _ := json.Marshal(evidence)
		v.EvidenceJSON = string(data)
		v.Title, v.Service, v.TagsJSON = old.Title, old.Service, old.TagsJSON
		v.Phenomenon = old.Phenomenon + "\n\n新增案例提案：" + p.Phenomenon
		v.RootCause = old.RootCause + "\n\n待审核补充（可能存在差异）：" + p.RootCause
		v.Solution = old.Solution + "\n\n修订提案：" + p.Solution
		v.Validation = old.Validation + "\n\n新增验证记录：" + p.Validation
		v.Conditions = old.Conditions + "\n\n新增适用条件：" + p.Conditions
		v.ReviewNote = old.ReviewNote + "\n修订理由：" + p.Reason
		if old.ReviewStatus == "pending" {
			v.ID = old.ID
			v.CreatedAt = old.CreatedAt
		} else {
			v.Number = entry.LatestVersion + 1
			entry.LatestVersion = v.Number
		}
		for _, body := range []string{v.Phenomenon, v.RootCause, v.Solution, v.Validation, v.Conditions} {
			if len([]rune(body)) > 16000 {
				return false, fmt.Errorf("%w: 修订过长，请人工精简已有草稿后重试", ErrInvalid)
			}
		}
		if err := tx.Save(&v).Error; err != nil {
			return false, err
		}
		result := tx.Model(&Entry{}).Where("id = ? AND owner_id = ? AND revision = ?", entry.ID, job.OwnerID, p.TargetRevision).Updates(map[string]any{"latest_version": entry.LatestVersion, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now()})
		if result.Error != nil {
			return false, result.Error
		}
		if result.RowsAffected != 1 {
			return false, ErrConflict
		}
	} else {
		if err := tx.Create(&v).Error; err != nil {
			return false, err
		}
	}
	return true, nil
}

// The curator can compare pending and published entries; the Main Agent only
// retrieves published approved versions through SearchPublished.
func (s *Service) SearchCurator(ctx context.Context, owner, query string, limit int) ([]Detail, error) {
	q := s.db.WithContext(ctx).Table("experience_entries e").Joins("JOIN experience_versions v ON v.entry_id = e.id AND v.number = e.latest_version").Where("e.owner_id = ? AND e.lifecycle != ? AND v.review_status != ?", owner, "archived", "rejected")
	terms := strings.Fields(query)
	if len(terms) > 8 {
		terms = terms[:8]
	}
	parts := []string{}
	args := []any{}
	for _, term := range terms {
		like := "%" + escapeLike(term) + "%"
		parts = append(parts, "(v.title LIKE ? ESCAPE '!' OR v.phenomenon LIKE ? ESCAPE '!' OR v.service LIKE ? ESCAPE '!' OR v.tags_json LIKE ? ESCAPE '!')")
		args = append(args, like, like, like, like)
	}
	if len(parts) == 0 {
		return []Detail{}, nil
	}
	q = q.Where(strings.Join(parts, " OR "), args...)
	var entries []Entry
	if err := q.Select("e.*").Order("e.updated_at DESC").Limit(limit).Find(&entries).Error; err != nil {
		return nil, err
	}
	result := []Detail{}
	for _, e := range entries {
		d, err := s.detail(ctx, e, e.LatestVersion)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, nil
}
