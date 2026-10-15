package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Issue is compact, domain-specific working memory. Raw records remain in Run
// journals/job snapshots; a summary is never a replacement for source evidence.
type Issue struct {
	ID                string    `gorm:"primaryKey;size:64" json:"id"`
	OwnerID           string    `gorm:"size:128;index" json:"-"`
	SessionID         string    `gorm:"size:128;index" json:"session_id"`
	Title             string    `gorm:"size:240" json:"title"`
	Service           string    `gorm:"size:128" json:"service"`
	Summary           string    `gorm:"type:text" json:"summary"`
	Status            string    `gorm:"size:32" json:"status"`
	ResolutionBasis   string    `gorm:"type:text" json:"resolution_basis"`
	Revision          uint64    `json:"revision"`
	OrganizedRevision uint64    `json:"organized_revision"`
	LastRunID         string    `gorm:"size:64" json:"last_run_id"`
	MergedInto        string    `gorm:"size:64" json:"merged_into,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (Issue) TableName() string { return "experience_issues" }

type IssueSession struct {
	OwnerID   string `gorm:"primaryKey;size:128"`
	SessionID string `gorm:"primaryKey;size:128"`
	Revision  uint64
	LastRunID string `gorm:"size:64"`
	ClosedAt  *time.Time
	StartedAt time.Time
}

func (IssueSession) TableName() string { return "experience_issue_sessions" }

// Empty IssueID explicitly represents an event that cannot yet be attributed.
type IssueAssociation struct {
	ID               uint       `gorm:"primaryKey" json:"-"`
	OwnerID          string     `gorm:"size:128;uniqueIndex:issue_event" json:"-"`
	IssueID          string     `gorm:"size:64;uniqueIndex:issue_event;index" json:"issue_id"`
	EventID          string     `gorm:"size:100;uniqueIndex:issue_event" json:"event_id"`
	RunID            string     `gorm:"size:64;index" json:"run_id"`
	Kind             string     `gorm:"size:32" json:"kind"`
	LastConsideredAt *time.Time `json:"-"`
}

func (IssueAssociation) TableName() string { return "experience_issue_associations" }

type IssueKnowledge struct {
	OwnerID   string    `gorm:"size:128;primaryKey" json:"-"`
	IssueID   string    `gorm:"size:64;primaryKey" json:"issue_id"`
	EntryID   string    `gorm:"size:64;primaryKey" json:"entry_id"`
	RunID     string    `gorm:"size:64" json:"run_id"`
	RunIDs    []string  `gorm:"-" json:"run_ids,omitempty"`
	SessionID string    `gorm:"size:128" json:"session_id"`
	Action    string    `gorm:"size:16" json:"action"`
	Reason    string    `gorm:"type:text" json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

func (IssueKnowledge) TableName() string { return "experience_issue_knowledge" }

type IssueView struct {
	Issue
	EvidenceCount int64              `json:"evidence_count"`
	Knowledge     []IssueKnowledge   `json:"knowledge"`
	Events        []IssueAssociation `json:"events"`
}
type IssueUpdate struct {
	ID                string   `json:"id"` // Existing ID, or a local new key such as new_mysql.
	Title             string   `json:"title"`
	Service           string   `json:"service"`
	Summary           string   `json:"summary"`
	Status            string   `json:"status"`
	ConfirmationQuote string   `json:"confirmation_quote"`
	EventIDs          []string `json:"event_ids"`
}
type TrackResult struct {
	Issues     []IssueUpdate `json:"issues"`
	Unassigned []string      `json:"unassigned"`
}
type IssueTracker interface {
	Track(context.Context, Source, []Issue) (TrackResult, error)
}

func eventKinds(source Source) map[string]string {
	events := map[string]string{source.Run.ID + ":user": "message", source.Run.ID + ":assistant": "message"}
	for _, e := range source.Evidence {
		events[e.ID] = "evidence"
	}
	for _, e := range source.Unassigned {
		events[e.EventID] = e.Kind
	}
	return events
}
func validateTracking(source Source, existing []Issue, result TrackResult) error {
	if len(result.Issues) > 12 {
		return fmt.Errorf("%w: 本轮问题过多", ErrInvalid)
	}
	known := map[string]bool{}
	previousStatus := map[string]string{}
	for _, i := range existing {
		known[i.ID] = true
		previousStatus[i.ID] = i.Status
	}
	events := eventKinds(source)
	seen := map[string]bool{}
	keys := map[string]bool{}
	for _, u := range result.Issues {
		if u.ID == "" || keys[u.ID] || (!known[u.ID] && !strings.HasPrefix(u.ID, "new_")) || strings.TrimSpace(u.Title) == "" || len([]rune(u.Title)) > 200 || len([]rune(u.Service)) > 128 || len([]rune(u.Summary)) > 2400 || len(u.EventIDs) == 0 {
			return fmt.Errorf("%w: 问题 ID/摘要/事件不合法", ErrInvalid)
		}
		keys[u.ID] = true
		if u.Status != "investigating" && u.Status != "resolved" && u.Status != "unresolved" {
			return ErrInvalid
		}
		// Model prose alone cannot establish repair success. The MVP requires an
		// exact user confirmation; machine-only verification stays investigating.
		if u.Status == "resolved" && (previousStatus[u.ID] != "resolved" || u.ConfirmationQuote != "") {
			if !confirmedInQuery(source.Run.Query, u.ConfirmationQuote) {
				return fmt.Errorf("%w: 已解决状态需要用户原文确认", ErrInvalid)
			}
		}
		for _, id := range u.EventIDs {
			if _, ok := events[id]; !ok {
				return fmt.Errorf("%w: 未知事件 %s", ErrInvalid, id)
			}
			seen[id] = true
		}
	}
	for _, id := range result.Unassigned {
		if _, ok := events[id]; !ok || seen[id] {
			return ErrInvalid
		}
		seen[id] = true
	}
	for id := range events {
		if !seen[id] {
			return fmt.Errorf("%w: 事件未归属或明确暂存 %s", ErrInvalid, id)
		}
	}
	return nil
}
func confirmedInQuery(query, quote string) bool {
	if len([]rune(quote)) < 4 || !strings.Contains(query, quote) {
		return false
	}
	for _, deny := range []string{"没有解决", "未解决", "没解决", "未验证", "没有验证", "假设", "如果", "建议", "not resolved", "not verified"} {
		if strings.Contains(strings.ToLower(quote), deny) {
			return false
		}
	}
	for _, word := range []string{"已解决", "已经解决", "恢复正常", "验证通过", "测试通过", "已经通过", "已通过", "修复成功", "已恢复", "resolved", "verified", "passed"} {
		if strings.Contains(strings.ToLower(quote), word) {
			return true
		}
	}
	return false
}

func (s *Service) ListIssues(ctx context.Context, owner, session string) ([]IssueView, error) {
	issues, err := s.loadIssues(ctx, owner, session)
	if err != nil {
		return nil, err
	}
	result := []IssueView{}
	for _, i := range issues {
		v := IssueView{Issue: i, Knowledge: []IssueKnowledge{}, Events: []IssueAssociation{}}
		if err := s.db.WithContext(ctx).Where("owner_id = ? AND issue_id = ?", owner, i.ID).Order("id ASC").Find(&v.Events).Error; err != nil {
			return nil, err
		}
		for _, e := range v.Events {
			if e.Kind == "evidence" {
				v.EvidenceCount++
			}
		}
		if err := s.db.WithContext(ctx).Where("owner_id = ? AND issue_id = ?", owner, i.ID).Find(&v.Knowledge).Error; err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, nil
}
func (s *Service) loadIssues(ctx context.Context, owner, session string) ([]Issue, error) {
	result := []Issue{}
	err := s.db.WithContext(ctx).Where("owner_id = ? AND session_id = ? AND merged_into = ?", owner, session, "").Order("created_at ASC").Find(&result).Error
	return result, err
}
func (s *Service) sessionLedger(ctx context.Context, owner, session string) (IssueSession, error) {
	v := IssueSession{OwnerID: owner, SessionID: session, StartedAt: time.Now()}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&v).Error; err != nil {
		return v, err
	}
	err := s.db.WithContext(ctx).Where("owner_id = ? AND session_id = ?", owner, session).First(&v).Error
	return v, err
}

// Commit coverage, associations and compact memory together. A retry cannot
// advance a watermark without preserving the observations it covers.
func (w *Worker) commitTracking(ctx context.Context, job Job, source Source, ledger IssueSession, existing []Issue, result TrackResult) error {
	return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Job
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND status = ? AND lease_token = ? AND tracked = ?", job.ID, "running", job.LeaseToken, false).First(&current).Error; err != nil {
			return err
		}
		fence := tx.Model(&IssueSession{}).Where("owner_id = ? AND session_id = ? AND revision = ?", job.OwnerID, job.SessionID, ledger.Revision).Updates(map[string]any{"revision": ledger.Revision + 1, "last_run_id": job.RunID})
		if fence.Error != nil {
			return fence.Error
		}
		if fence.RowsAffected != 1 {
			return ErrConflict
		}
		known := map[string]Issue{}
		for _, i := range existing {
			known[i.ID] = i
		}
		kinds := eventKinds(source)
		touched := []string{}
		for _, u := range result.Issues {
			i, ok := known[u.ID]
			if !ok {
				i = Issue{ID: "ISS-" + uuid.NewString(), OwnerID: job.OwnerID, SessionID: job.SessionID}
			}
			if u.Status == "resolved" && u.ConfirmationQuote == "" && i.Status != "resolved" {
				return fmt.Errorf("%w: 新的解决状态缺少确认", ErrInvalid)
			}
			i.Title, i.Service, i.Summary, i.Status, i.LastRunID = u.Title, u.Service, u.Summary, u.Status, job.RunID
			if u.ConfirmationQuote != "" {
				i.ResolutionBasis = u.ConfirmationQuote
			}
			if i.Status != "resolved" {
				i.ResolutionBasis = ""
			}
			i.Revision++
			if err := tx.Save(&i).Error; err != nil {
				return err
			}
			touched = append(touched, i.ID)
			for _, event := range u.EventIDs {
				runID := job.RunID
				for _, prior := range source.Unassigned {
					if prior.EventID == event {
						runID = prior.RunID
					}
				}
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&IssueAssociation{OwnerID: job.OwnerID, IssueID: i.ID, EventID: event, RunID: runID, Kind: kinds[event]}).Error; err != nil {
					return err
				}
				if err := tx.Where("owner_id = ? AND issue_id = ? AND event_id = ?", job.OwnerID, "", event).Delete(&IssueAssociation{}).Error; err != nil {
					return err
				}
			}
		}
		for _, event := range result.Unassigned {
			runID := job.RunID
			for _, prior := range source.Unassigned {
				if prior.EventID == event {
					runID = prior.RunID
				}
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&IssueAssociation{OwnerID: job.OwnerID, EventID: event, RunID: runID, Kind: kinds[event]}).Error; err != nil {
				return err
			}
			if err := tx.Model(&IssueAssociation{}).Where("owner_id = ? AND issue_id = ? AND event_id = ?", job.OwnerID, "", event).Update("last_considered_at", time.Now()).Error; err != nil {
				return err
			}
		}
		ids, _ := json.Marshal(touched)
		return tx.Model(&Job{}).Where("id = ? AND lease_token = ?", job.ID, job.LeaseToken).Updates(map[string]any{"tracked": true, "issue_ids_json": string(ids)}).Error
	})
}

func (s *Service) UnassignedEvents(ctx context.Context, owner, session string) ([]IssueAssociation, error) {
	items := []IssueAssociation{}
	err := s.db.WithContext(ctx).Table("experience_issue_associations a").Select("a.*").Joins("JOIN experience_jobs j ON j.run_id = a.run_id AND j.owner_id = a.owner_id").Where("a.owner_id = ? AND j.session_id = ? AND a.issue_id = ?", owner, session, "").Order("a.last_considered_at ASC, a.id ASC").Limit(100).Find(&items).Error
	return items, err
}
func (s *Service) loadUnassigned(ctx context.Context, source *Source) error {
	refs, err := s.UnassignedEvents(ctx, source.Run.UserID, source.Run.SessionID)
	if err != nil {
		return err
	}
	source.Unassigned = refs
	for _, r := range refs {
		var j Job
		if err := s.db.WithContext(ctx).Where("owner_id = ? AND run_id = ?", r.OwnerID, r.RunID).First(&j).Error; err != nil {
			return err
		}
		var raw Source
		if err := json.Unmarshal([]byte(j.SnapshotJSON), &raw); err != nil {
			return err
		}
		text := ""
		if r.EventID == r.RunID+":user" {
			text = raw.Run.Query
		} else if r.EventID == r.RunID+":assistant" {
			text = raw.Run.Answer
		} else {
			for _, e := range raw.Evidence {
				if e.ID == r.EventID {
					text = string(e.Result)
				}
			}
		}
		source.PriorEvents = append(source.PriorEvents, map[string]any{"id": r.EventID, "run_id": r.RunID, "preview": string([]rune(text)[:min(len([]rune(text)), 800)])})
	}
	return nil
}

// Session close reconciles persisted Runs, not an unbounded chat replay.
func (s *Service) FinalizeSession(ctx context.Context, owner, session string) ([]Job, error) {
	if owner == "" || session == "" || len(session) > 128 {
		return nil, ErrInvalid
	}
	var runs []struct{ ID string }
	if err := s.db.WithContext(ctx).Table("agent_runs").Select("id").Where("user_id = ? AND session_id = ? AND status IN ?", owner, session, []string{"completed", "failed", "cancelled", "interrupted"}).Order("created_at ASC").Find(&runs).Error; err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	jobs := []Job{}
	for _, r := range runs {
		j, err := s.Enqueue(ctx, owner, r.ID, true)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	ledger, err := s.sessionLedger(ctx, owner, session)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	err = s.db.WithContext(ctx).Model(&ledger).Where("owner_id = ? AND session_id = ?", owner, session).Update("closed_at", now).Error
	return jobs, err
}
