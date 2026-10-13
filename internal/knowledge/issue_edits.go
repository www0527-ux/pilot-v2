package knowledge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type IssueEdit struct {
	Action         string   `json:"action"`
	Revision       uint64   `json:"revision"`
	TargetRevision uint64   `json:"target_revision"`
	TargetID       string   `json:"target_id"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	Basis          string   `json:"basis"`
	EventIDs       []string `json:"event_ids"`
}

// All corrections are owner scoped, optimistic, and atomic with the session
// revision so a model processing an earlier snapshot cannot undo the edit.
func (s *Service) EditIssue(ctx context.Context, owner, id string, input IssueEdit) (Issue, error) {
	var issue Issue
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND merged_into = ?", id, owner, "").First(&issue).Error; err != nil {
			return err
		}
		if issue.Revision != input.Revision {
			return ErrConflict
		}
		if err := tx.Model(&IssueSession{}).Where("owner_id = ? AND session_id = ?", owner, issue.SessionID).Update("revision", gorm.Expr("revision + 1")).Error; err != nil {
			return err
		}
		switch input.Action {
		case "assign":
			if len(input.EventIDs) == 0 || len(input.EventIDs) > 100 {
				return ErrInvalid
			}
			var refs []IssueAssociation
			if err := tx.Table("experience_issue_associations a").Select("a.*").Joins("JOIN experience_jobs j ON j.owner_id = a.owner_id AND j.run_id = a.run_id").Where("a.owner_id = ? AND a.issue_id = ? AND j.session_id = ? AND a.event_id IN ?", owner, "", issue.SessionID, input.EventIDs).Find(&refs).Error; err != nil {
				return err
			}
			if len(refs) != len(input.EventIDs) {
				return ErrInvalid
			}
			for _, r := range refs {
				oldID := r.ID
				r.ID = 0
				r.IssueID = issue.ID
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&r).Error; err != nil {
					return err
				}
				if err := tx.Where("id = ? AND owner_id = ?", oldID, owner).Delete(&IssueAssociation{}).Error; err != nil {
					return err
				}
			}
			issue.Status = "investigating"
			issue.ResolutionBasis = ""
			issue.Summary += "\n用户补充了此前未归属的事件；需要结合来源核实。"
		case "rename":
			if strings.TrimSpace(input.Title) == "" || len([]rune(input.Title)) > 200 {
				return ErrInvalid
			}
			issue.Title = strings.TrimSpace(input.Title)
		case "status":
			if input.Status != "investigating" && input.Status != "resolved" && input.Status != "unresolved" {
				return ErrInvalid
			}
			if input.Status == "resolved" && (len([]rune(strings.TrimSpace(input.Basis))) < 4 || len([]rune(input.Basis)) > 2400) {
				return fmt.Errorf("%w: 请说明解决依据", ErrInvalid)
			}
			issue.Status = input.Status
			issue.ResolutionBasis = input.Basis
		case "merge", "split", "move":
			if input.TargetID == id {
				return ErrInvalid
			}
			var refs []IssueAssociation
			query := tx.Where("owner_id = ? AND issue_id = ?", owner, id)
			if input.Action != "merge" {
				if len(input.EventIDs) == 0 || len(input.EventIDs) > 200 {
					return ErrInvalid
				}
				query = query.Where("event_id IN ?", input.EventIDs)
			}
			if err := query.Find(&refs).Error; err != nil {
				return err
			}
			if len(refs) == 0 || (input.Action != "merge" && len(refs) != len(input.EventIDs)) {
				return fmt.Errorf("%w: 事件不属于这个问题", ErrInvalid)
			}
			target := Issue{}
			if input.Action == "split" {
				if strings.TrimSpace(input.Title) == "" || len([]rune(input.Title)) > 200 {
					return ErrInvalid
				}
				target = Issue{ID: "ISS-" + uuid.NewString(), OwnerID: owner, SessionID: issue.SessionID, Title: input.Title, Status: "investigating", Revision: 1, LastRunID: issue.LastRunID, Summary: "用户从另一问题拆分了部分事件；请根据独立来源重新核实根因，原问题结论不可直接套用。"}
				if err := tx.Create(&target).Error; err != nil {
					return err
				}
			} else {
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ? AND session_id = ? AND merged_into = ?", input.TargetID, owner, issue.SessionID, "").First(&target).Error; err != nil {
					return err
				}
				if target.Revision != input.TargetRevision {
					return ErrConflict
				}
				target.Revision++
				target.Status = "investigating"
				target.ResolutionBasis = ""
				if input.Action == "merge" {
					target.Summary = target.Summary + "\n用户合并问题，保留来源差异：" + issue.Summary
				} else {
					target.Summary += "\n用户调整了证据归属；请重新核实。"
				}
				if len([]rune(target.Summary)) > 2400 {
					target.Summary = string([]rune(target.Summary)[:2400])
				}
				if err := tx.Save(&target).Error; err != nil {
					return err
				}
			}
			for _, r := range refs {
				oldID := r.ID
				r.ID = 0
				r.IssueID = target.ID
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&r).Error; err != nil {
					return err
				}
				if err := tx.Where("id = ? AND owner_id = ?", oldID, owner).Delete(&IssueAssociation{}).Error; err != nil {
					return err
				}
			}
			if input.Action == "merge" {
				issue.MergedInto = target.ID
				var links []IssueKnowledge
				if err := tx.Where("owner_id = ? AND issue_id = ?", owner, id).Find(&links).Error; err != nil {
					return err
				}
				for _, l := range links {
					l.IssueID = target.ID
					if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&l).Error; err != nil {
						return err
					}
				}
			} else {
				issue.Status = "investigating"
				issue.ResolutionBasis = ""
				issue.Summary += "\n用户移出了部分事件；之前的根因与验证需重新确认。"
			}
		default:
			return ErrInvalid
		}
		issue.Revision++
		return tx.Save(&issue).Error
	})
	if err != nil {
		return issue, err
	}
	return issue, nil
}

func (s *Service) RequeueSession(ctx context.Context, owner, session string) error {
	var j Job
	if err := s.db.WithContext(ctx).Where("owner_id = ? AND session_id = ?", owner, session).Order("created_at DESC").First(&j).Error; err != nil {
		return err
	}
	return s.db.WithContext(ctx).Model(&Job{}).Where("id = ? AND status IN ?", j.ID, []string{"succeeded", "no_draft", "failed"}).Updates(map[string]any{"status": "pending", "manual": true, "attempts": 0, "error": "", "next_attempt_at": time.Now()}).Error
}

// Lock against completion: a click just after the worker's last check must
// requeue a terminal job, rather than merely flip Manual on an idle record.
func (s *Service) markManualJob(ctx context.Context, owner, runID string) (Job, error) {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job Job
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("owner_id = ? AND run_id = ?", owner, runID).First(&job).Error; err != nil {
			return err
		}
		updates := map[string]any{"manual": true, "updated_at": time.Now()}
		if job.Status == "succeeded" || job.Status == "no_draft" || job.Status == "failed" {
			updates["status"] = "pending"
			updates["attempts"] = 0
			updates["next_attempt_at"] = time.Now()
			updates["error"] = ""
		}
		return tx.Model(&Job{}).Where("id = ?", job.ID).Updates(updates).Error
	})
	if err != nil {
		return Job{}, err
	}
	return s.GetJob(ctx, owner, runID)
}
