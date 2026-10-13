package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"shanyangcode.com/pilot/internal/ai/progress"
	"shanyangcode.com/pilot/internal/memory"
)

var ErrConflict = errors.New("knowledge changed; reload before submitting")
var ErrInvalid = errors.New("invalid knowledge input")

type Service struct {
	db            *gorm.DB
	issueTracking bool
	trackingSince time.Time
}

// EnableIssueTracking switches the running application to incremental issue jobs.
func (s *Service) EnableIssueTracking() error {
	marker := IssueSession{OwnerID: "__system__", SessionID: "issue_tracking_start", StartedAt: time.Now()}
	if err := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&marker).Error; err != nil {
		return err
	}
	if err := s.db.Where("owner_id = ? AND session_id = ?", marker.OwnerID, marker.SessionID).First(&marker).Error; err != nil {
		return err
	}
	s.issueTracking = true
	s.trackingSince = marker.StartedAt
	return nil
}

func NewService(ctx context.Context, db *gorm.DB) (*Service, error) {
	if err := db.WithContext(ctx).AutoMigrate(&Entry{}, &Version{}, &Job{}, &Settings{}, &Reference{}, &Issue{}, &IssueAssociation{}, &IssueKnowledge{}, &IssueSession{}); err != nil {
		return nil, err
	}
	return &Service{db: db}, nil
}

func (s *Service) GetSettings(ctx context.Context, owner string) (Settings, error) {
	v := Settings{OwnerID: owner, EnabledAt: time.Now()}
	err := s.db.WithContext(ctx).Where("owner_id = ?", owner).First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		v.Automatic = s.issueTracking
		err = nil
	}
	return v, err
}
func (s *Service) SetAutomatic(ctx context.Context, owner string, enabled bool) (Settings, error) {
	v, err := s.GetSettings(ctx, owner)
	if err != nil {
		return v, err
	}
	if enabled && !v.Automatic {
		v.EnabledAt = time.Now()
	}
	v.Automatic = enabled
	err = s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&v).Error
	return v, err
}

func terminalRun(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled" || status == "interrupted"
}

func (s *Service) BuildSource(ctx context.Context, owner, runID string) (Source, error) {
	var source Source
	if err := s.db.WithContext(ctx).Where("id = ? AND user_id = ?", runID, owner).First(&source.Run).Error; err != nil {
		return source, err
	}
	if !terminalRun(source.Run.Status) {
		return source, fmt.Errorf("%w: 排障尚未结束", ErrInvalid)
	}
	// Bind a snapshot to this run's time, excluding later turns in the session.
	q := s.db.WithContext(ctx).Where("user_id = ? AND session_id = ? AND created_at <= ?", owner, source.Run.SessionID, source.Run.UpdatedAt)
	if s.issueTracking {
		q = q.Where("created_at >= ?", source.Run.CreatedAt)
	}
	var err error
	if !s.issueTracking {
		err = q.Order("id DESC").Limit(201).Find(&source.Conversation).Error
	}
	if err != nil {
		return source, err
	}
	if len(source.Conversation) > 200 {
		source.Conversation = source.Conversation[:200]
		source.Limitations = append(source.Limitations, "仅包含最近 200 条对话；更早的原始记录未加载")
	}
	for i, j := 0, len(source.Conversation)-1; i < j; i, j = i+1, j-1 {
		source.Conversation[i], source.Conversation[j] = source.Conversation[j], source.Conversation[i]
	}
	if s.issueTracking {
		// Query/answer and journal below are authoritative for this Run. Delayed
		// history persistence must not accidentally associate another Run's turn.
		source.Conversation = nil
	}
	var events []progress.Event
	if err := json.Unmarshal([]byte(source.Run.EventsJSON), &events); err != nil {
		return source, err
	}
	for _, e := range events {
		if e.Kind == "tool" && e.Status != "running" && e.Result != nil {
			result, err := json.Marshal(progress.PublicValue(e.Result))
			if err != nil {
				return source, err
			}
			source.Evidence = append(source.Evidence, Evidence{ID: fmt.Sprintf("%s:%d", runID, e.Sequence), RunID: runID, Sequence: e.Sequence, Tool: e.Name, ObservedAt: e.Timestamp, Status: e.Status, Result: result})
		}
	}
	// UI journals contain bounded/redacted tool results, not the full provider
	// transcript. Make that limitation explicit to the knowledge agent.
	source.Limitations = append(source.Limitations, "证据来自已保存的公开工具结果；可能已截断或脱敏，不是完整模型内部上下文")
	return source, nil
}
func (s *Service) Enqueue(ctx context.Context, owner, runID string, retry bool) (Job, error) {
	// Authorize before returning an existing job as well as before creating one.
	var run memory.AgentRun
	if err := s.db.WithContext(ctx).Select("id", "status", "session_id", "created_at").Where("id = ? AND user_id = ?", runID, owner).First(&run).Error; err != nil {
		return Job{}, err
	}
	var existing Job
	err := s.db.WithContext(ctx).Where("owner_id = ? AND run_id = ?", owner, runID).First(&existing).Error
	if err == nil {
		if s.issueTracking && existing.SessionID == "" {
			if err := s.db.WithContext(ctx).Model(&Job{}).Where("id = ? AND owner_id = ?", existing.ID, owner).Updates(map[string]any{"session_id": run.SessionID, "source_at": run.CreatedAt}).Error; err != nil {
				return existing, err
			}
			existing.SessionID = run.SessionID
			existing.SourceAt = run.CreatedAt
		}
		if existing.Status == "waiting_snapshot" && terminalRun(run.Status) {
			source, e := s.BuildSource(ctx, owner, runID)
			if e != nil {
				return existing, e
			}
			data, e := json.Marshal(source)
			if e != nil {
				return existing, e
			}
			if e := s.db.WithContext(ctx).Model(&Job{}).Where("id = ? AND status = ?", existing.ID, "waiting_snapshot").Updates(map[string]any{"status": "pending", "snapshot_json": string(data), "next_attempt_at": time.Now()}).Error; e != nil {
				return existing, e
			}
			return s.GetJob(ctx, owner, runID)
		}
		if retry && s.issueTracking {
			return s.markManualJob(ctx, owner, runID)
		}
		if retry && (existing.Status == "failed" || (s.issueTracking && !existing.Manual && (existing.Status == "succeeded" || existing.Status == "no_draft"))) {
			result := s.db.WithContext(ctx).Model(&Job{}).Where("id = ? AND status = ?", existing.ID, existing.Status).Updates(map[string]any{"status": "pending", "manual": true, "attempts": 0, "error": "", "next_attempt_at": time.Now(), "updated_at": time.Now()})
			if result.Error != nil {
				return existing, result.Error
			}
			return s.GetJob(ctx, owner, runID)
		}
		if retry && s.issueTracking && !existing.Manual {
			return existing, s.db.WithContext(ctx).Model(&Job{}).Where("id = ?", existing.ID).Update("manual", true).Error
		}
		return existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return Job{}, err
	}
	source, err := s.BuildSource(ctx, owner, runID)
	if err != nil {
		return Job{}, err
	}
	snapshot, err := json.Marshal(source)
	if err != nil {
		return Job{}, err
	}
	job := Job{ID: uuid.NewString(), OwnerID: owner, RunID: runID, SessionID: source.Run.SessionID, SourceAt: source.Run.CreatedAt, Manual: retry, Status: "pending", SnapshotJSON: string(snapshot), NextAttemptAt: time.Now()}
	result := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&job)
	if result.Error != nil {
		return job, result.Error
	}
	return s.GetJob(ctx, owner, runID)
}
func (s *Service) GetJob(ctx context.Context, owner, runID string) (Job, error) {
	var job Job
	err := s.db.WithContext(ctx).Where("owner_id = ? AND run_id = ?", owner, runID).First(&job).Error
	return job, err
}
func (s *Service) MaybeEnqueue(ctx context.Context, run memory.AgentRun) error {
	if s.issueTracking {
		if !terminalRun(run.Status) {
			return nil
		}
		_, err := s.Enqueue(ctx, run.UserID, run.ID, false)
		return err
	}
	settings, err := s.GetSettings(ctx, run.UserID)
	if err != nil {
		return err
	}
	if !settings.Automatic || !terminalRun(run.Status) || run.CreatedAt.Before(settings.EnabledAt) {
		return nil
	}
	_, err = s.Enqueue(ctx, run.UserID, run.ID, false)
	return err
}

type Filter struct {
	Query, Service, Type, Tab, Review, Verification, From, To, RunID string
	Page                                                             int
}
type ListResult struct {
	Items  []Detail         `json:"items"`
	Total  int64            `json:"total"`
	Counts map[string]int64 `json:"counts"`
}

func (s *Service) List(ctx context.Context, owner string, f Filter) (ListResult, error) {
	result := ListResult{Items: []Detail{}, Counts: map[string]int64{"pending": 0, "published": 0, "archived": 0}}
	base := s.db.WithContext(ctx).Table("experience_entries e").Joins("JOIN experience_versions v ON v.entry_id = e.id AND v.number = e.latest_version").Where("e.owner_id = ?", owner)
	for _, tab := range []string{"pending", "published", "archived"} {
		q := base.Session(&gorm.Session{})
		switch tab {
		case "pending":
			q = q.Where("e.lifecycle != ? AND v.review_status = ?", "archived", "pending")
		case "published":
			q = q.Where("e.lifecycle = ?", "published")
		case "archived":
			q = q.Where("e.lifecycle = ?", "archived")
		}
		var count int64
		if err := q.Count(&count).Error; err != nil {
			return result, err
		}
		result.Counts[tab] = count
	}
	q := base.Session(&gorm.Session{})
	switch f.Tab {
	case "pending":
		q = q.Where("e.lifecycle != ? AND v.review_status = ?", "archived", "pending")
	case "published":
		q = q.Where("e.lifecycle = ?", "published")
	case "archived":
		q = q.Where("e.lifecycle = ?", "archived")
	}
	if f.Query != "" {
		like := "%" + escapeLike(strings.TrimSpace(f.Query)) + "%"
		q = q.Where("(v.title LIKE ? ESCAPE '!' OR v.phenomenon LIKE ? ESCAPE '!' OR v.tags_json LIKE ? ESCAPE '!')", like, like, like)
	}
	if f.RunID != "" {
		q = q.Where("e.source_run_id = ? OR e.job_id IN (SELECT j.id FROM experience_jobs j WHERE j.owner_id = e.owner_id AND j.run_id = ?) OR EXISTS (SELECT 1 FROM experience_issue_knowledge k JOIN experience_issue_associations a ON a.issue_id = k.issue_id AND a.owner_id = k.owner_id WHERE k.entry_id = e.id AND k.owner_id = e.owner_id AND a.run_id = ?)", f.RunID, f.RunID, f.RunID)
	}
	for _, p := range []struct{ column, value string }{{"v.service", f.Service}, {"e.type", f.Type}, {"v.review_status", f.Review}, {"v.verification", f.Verification}} {
		if p.value != "" {
			q = q.Where(p.column+" = ?", p.value)
		}
	}
	if f.From != "" {
		date, err := time.Parse("2006-01-02", f.From)
		if err != nil {
			return result, ErrInvalid
		}
		q = q.Where("e.created_at >= ?", date)
	}
	if f.To != "" {
		date, err := time.Parse("2006-01-02", f.To)
		if err != nil {
			return result, ErrInvalid
		}
		q = q.Where("e.created_at < ?", date.AddDate(0, 0, 1))
	}
	if err := q.Count(&result.Total).Error; err != nil {
		return result, err
	}
	page := f.Page
	if page < 1 {
		page = 1
	}
	var entries []Entry
	if err := q.Select("e.*").Order("e.updated_at DESC").Limit(30).Offset((page - 1) * 30).Find(&entries).Error; err != nil {
		return result, err
	}
	for _, entry := range entries {
		detail, err := s.detail(ctx, entry, entry.LatestVersion)
		if err != nil {
			return result, err
		}
		result.Items = append(result.Items, detail)
	}
	return result, nil
}
func escapeLike(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}
func (s *Service) detail(ctx context.Context, entry Entry, number int) (Detail, error) {
	d := Detail{Entry: entry, Tags: []string{}, Evidence: []Evidence{}}
	err := s.db.WithContext(ctx).Where("entry_id = ? AND number = ?", entry.ID, number).First(&d.Version).Error
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal([]byte(d.Version.TagsJSON), &d.Tags); err != nil {
		return d, err
	}
	if err := json.Unmarshal([]byte(d.Version.EvidenceJSON), &d.Evidence); err != nil {
		return d, err
	}
	if err := s.db.WithContext(ctx).Where("entry_id = ? AND owner_id = ?", entry.ID, entry.OwnerID).Find(&d.Sources).Error; err != nil {
		return d, err
	}
	for i := range d.Sources {
		if err := s.db.WithContext(ctx).Model(&IssueAssociation{}).Where("owner_id = ? AND issue_id = ?", entry.OwnerID, d.Sources[i].IssueID).Distinct("run_id").Limit(100).Pluck("run_id", &d.Sources[i].RunIDs).Error; err != nil {
			return d, err
		}
	}
	return d, nil
}
func (s *Service) Get(ctx context.Context, owner, id string, number int) (Detail, error) {
	var entry Entry
	if err := s.db.WithContext(ctx).Where("id = ? AND owner_id = ?", id, owner).First(&entry).Error; err != nil {
		return Detail{}, err
	}
	if number == 0 {
		number = entry.LatestVersion
	}
	return s.detail(ctx, entry, number)
}
func (s *Service) Versions(ctx context.Context, owner, id string) ([]Detail, error) {
	d, err := s.Get(ctx, owner, id, 0)
	if err != nil {
		return nil, err
	}
	items := make([]Detail, 0, d.Entry.LatestVersion)
	for n := d.Entry.LatestVersion; n > 0; n-- {
		v, err := s.detail(ctx, d.Entry, n)
		if err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, nil
}

type Edit struct {
	Revision uint64 `json:"revision"`
	Action   string `json:"action"`
	Note     string `json:"note"`
	Candidate
}

func validateCandidate(c Candidate) error {
	if strings.TrimSpace(c.Title) == "" || strings.TrimSpace(c.Phenomenon) == "" || len([]rune(c.Title)) > 200 || len([]rune(c.Service)) > 128 || len(c.Tags) > 12 {
		return fmt.Errorf("%w: 标题、现象必填，标题最多 200 字，标签最多 12 个", ErrInvalid)
	}
	for _, tag := range c.Tags {
		if len([]rune(tag)) > 80 {
			return ErrInvalid
		}
	}
	for _, text := range []string{c.Phenomenon, c.RootCause, c.Solution, c.Validation, c.Conditions} {
		if len([]rune(text)) > 16000 {
			return fmt.Errorf("%w: 内容过长", ErrInvalid)
		}
	}
	if c.Verification != "unverified" && c.Verification != "partially_verified" && c.Verification != "verified" {
		return fmt.Errorf("%w: 验证状态不合法", ErrInvalid)
	}
	if c.CauseStatus != "hypothesis" && c.CauseStatus != "confirmed" && c.CauseStatus != "unknown" {
		return fmt.Errorf("%w: 根因状态不合法", ErrInvalid)
	}
	if c.Verification != "unverified" && strings.TrimSpace(c.Validation) == "" {
		return fmt.Errorf("%w: 请填写验证依据", ErrInvalid)
	}
	return nil
}
func versionFromCandidate(c Candidate, evidence []Evidence) Version {
	if c.Tags == nil {
		c.Tags = []string{}
	}
	if evidence == nil {
		evidence = []Evidence{}
	}
	tags, _ := json.Marshal(c.Tags)
	ev, _ := json.Marshal(evidence)
	return Version{Title: strings.TrimSpace(c.Title), Service: strings.TrimSpace(c.Service), TagsJSON: string(tags), Phenomenon: c.Phenomenon, RootCause: c.RootCause, CauseStatus: c.CauseStatus, Solution: c.Solution, Validation: c.Validation, Conditions: c.Conditions, Verification: c.Verification, ReviewStatus: "pending", EvidenceJSON: string(ev)}
}
func (s *Service) Update(ctx context.Context, owner, id string, input Edit) (Detail, error) {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var entry Entry
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner_id = ?", id, owner).First(&entry).Error; err != nil {
			return err
		}
		if entry.Revision != input.Revision {
			return ErrConflict
		}
		if entry.Lifecycle == "archived" && input.Action != "restore" {
			return fmt.Errorf("%w: 已归档知识不可编辑", ErrInvalid)
		}
		var v Version
		if err := tx.Where("entry_id = ? AND number = ?", id, entry.LatestVersion).First(&v).Error; err != nil {
			return err
		}
		switch input.Action {
		case "restore":
			if entry.Lifecycle != "archived" || entry.PublishedVersion == 0 {
				return fmt.Errorf("%w: 只有归档且有已发布版本的知识可以恢复", ErrInvalid)
			}
			var published Version
			if err := tx.Where("entry_id = ? AND number = ?", id, entry.PublishedVersion).First(&published).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return fmt.Errorf("%w: 原发布版本不存在，无法恢复", ErrInvalid)
				}
				return err
			}
			if published.ReviewStatus != "approved" {
				return fmt.Errorf("%w: 原发布版本未通过审核，无法恢复", ErrInvalid)
			}
			// Restore availability only. Never publish the latest pending revision
			// or modify immutable content, verification, or source evidence.
			entry.Lifecycle = "published"
		case "revise":
			if entry.PublishedVersion == 0 || v.ReviewStatus == "pending" {
				return fmt.Errorf("%w: 当前已有待审核版本或没有已发布版本", ErrInvalid)
			}
			v.ID = 0
			v.Number++
			v.ReviewStatus = "pending"
			v.ReviewerID = ""
			v.ReviewedAt = nil
			v.ReviewNote = ""
			v.CreatedAt = time.Time{}
			v.UpdatedAt = time.Time{}
			if err := tx.Create(&v).Error; err != nil {
				return err
			}
			entry.LatestVersion = v.Number
		case "archive":
			if entry.PublishedVersion == 0 || strings.TrimSpace(input.Note) == "" {
				return fmt.Errorf("%w: 归档需要已发布版本和原因", ErrInvalid)
			}
			entry.Lifecycle = "archived"
			entry.ArchiveReason = strings.TrimSpace(input.Note)
		case "reject":
			if v.ReviewStatus != "pending" {
				return ErrConflict
			}
			now := time.Now()
			v.ReviewStatus = "rejected"
			v.ReviewerID = owner
			v.ReviewedAt = &now
			v.ReviewNote = input.Note
			if err := tx.Save(&v).Error; err != nil {
				return err
			}
		case "save", "publish":
			if v.ReviewStatus != "pending" {
				return ErrConflict
			}
			if err := validateCandidate(input.Candidate); err != nil {
				return err
			}
			// Evidence snapshots are server-owned; edits cannot replace source refs.
			updated := versionFromCandidate(input.Candidate, nil)
			updated.ID = v.ID
			updated.EntryID = id
			updated.Number = v.Number
			updated.CreatedAt = v.CreatedAt
			updated.EvidenceJSON = v.EvidenceJSON
			updated.ReviewNote = input.Note
			if input.Action == "publish" {
				now := time.Now()
				updated.ReviewStatus = "approved"
				updated.ReviewerID = owner
				updated.ReviewedAt = &now
				updated.ReviewNote = input.Note
				entry.PublishedVersion = v.Number
				entry.Lifecycle = "published"
			}
			if err := tx.Save(&updated).Error; err != nil {
				return err
			}
		default:
			return ErrInvalid
		}
		result := tx.Model(&Entry{}).Where("id = ? AND owner_id = ? AND revision = ?", id, owner, input.Revision).Updates(map[string]any{"revision": input.Revision + 1, "latest_version": entry.LatestVersion, "published_version": entry.PublishedVersion, "lifecycle": entry.Lifecycle, "archive_reason": entry.ArchiveReason, "updated_at": time.Now()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrConflict
		}
		return nil
	})
	if err != nil {
		return Detail{}, err
	}
	return s.Get(ctx, owner, id, 0)
}

func (s *Service) SearchPublished(ctx context.Context, owner, query, service string, limit int) ([]Detail, error) {
	if owner == "" {
		return nil, ErrInvalid
	}
	if limit < 1 || limit > 8 {
		limit = 3
	}
	q := s.db.WithContext(ctx).Table("experience_entries e").Joins("JOIN experience_versions v ON v.entry_id = e.id AND v.number = e.published_version").Where("e.owner_id = ? AND e.lifecycle = ? AND v.review_status = ?", owner, "published", "approved")
	if service != "" {
		q = q.Where("v.service = ?", service)
	}
	terms := strings.Fields(strings.TrimSpace(query))
	if len(terms) > 8 {
		terms = terms[:8]
	}
	if len(terms) == 0 && service == "" {
		return []Detail{}, nil
	}
	if len(terms) > 0 {
		parts := []string{}
		args := []any{}
		scores := []string{}
		scoreArgs := []any{}
		for _, term := range terms {
			parts = append(parts, "(v.title LIKE ? ESCAPE '!' OR v.phenomenon LIKE ? ESCAPE '!' OR v.root_cause LIKE ? ESCAPE '!' OR v.tags_json LIKE ? ESCAPE '!')")
			like := "%" + escapeLike(term) + "%"
			args = append(args, like, like, like, like)
			scores = append(scores, "(CASE WHEN v.title LIKE ? ESCAPE '!' THEN 4 ELSE 0 END + CASE WHEN v.phenomenon LIKE ? ESCAPE '!' THEN 2 ELSE 0 END + CASE WHEN v.root_cause LIKE ? ESCAPE '!' THEN 2 ELSE 0 END + CASE WHEN v.tags_json LIKE ? ESCAPE '!' THEN 1 ELSE 0 END)")
			scoreArgs = append(scoreArgs, like, like, like, like)
		}
		q = q.Where("("+strings.Join(parts, " OR ")+")", args...)
		q = q.Select("e.*, ("+strings.Join(scores, " + ")+") AS relevance", scoreArgs...).Order("relevance DESC")
	} else {
		q = q.Select("e.*")
	}
	var entries []Entry
	if err := q.Order("e.updated_at DESC").Limit(limit).Find(&entries).Error; err != nil {
		return nil, err
	}
	items := []Detail{}
	for _, e := range entries {
		d, err := s.detail(ctx, e, e.PublishedVersion)
		if err != nil {
			return nil, err
		}
		items = append(items, d)
	}
	return items, nil
}
func (s *Service) RecordReference(ctx context.Context, owner, runID string, d Detail) error {
	if runID == "" {
		return nil
	}
	r := Reference{OwnerID: owner, RunID: runID, EntryID: d.Entry.ID, Version: d.Version.Number}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&r).Error
}
