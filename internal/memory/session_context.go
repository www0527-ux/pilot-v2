package memory

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SessionContext is the model-facing working context, not the immutable UI history.
// HistoryCursor + LastRunID prevent re-injecting turns already in MessagesJSON.
type SessionContext struct {
	UserID          string `gorm:"primaryKey;size:128"`
	SessionID       string `gorm:"primaryKey;size:128"`
	Revision        uint64
	HistoryCursor   uint
	LastRunID       string `gorm:"size:64"`
	CoveredRunsJSON string `gorm:"type:text"`
	MessagesJSON    string `gorm:"type:longtext"`
	UpdatedAt       time.Time
}

type LoadedContext struct {
	Record   SessionContext
	Messages []*schema.Message
}

type SessionContextManager struct{ db *gorm.DB }

func NewSessionContextManager(db *gorm.DB) *SessionContextManager {
	return &SessionContextManager{db: db}
}

func (m *SessionContextManager) Load(ctx context.Context, owner, session string) (LoadedContext, error) {
	loaded := LoadedContext{Record: SessionContext{UserID: owner, SessionID: session}}
	err := m.db.WithContext(ctx).Where("user_id = ? AND session_id = ?", owner, session).First(&loaded.Record).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return loaded, err
	}
	if loaded.Record.MessagesJSON != "" {
		if err := json.Unmarshal([]byte(loaded.Record.MessagesJSON), &loaded.Messages); err != nil {
			return loaded, err
		}
	}
	var rows []ConversationHistory
	covered := map[string]bool{}
	var runIDs []string
	if loaded.Record.CoveredRunsJSON != "" {
		if err := json.Unmarshal([]byte(loaded.Record.CoveredRunsJSON), &runIDs); err != nil {
			return loaded, err
		}
	}
	for _, id := range runIDs {
		covered[id] = true
	}
	if loaded.Record.LastRunID != "" {
		covered[loaded.Record.LastRunID] = true
	}
	if err := m.db.WithContext(ctx).Where("user_id = ? AND session_id = ? AND id > ?", owner, session, loaded.Record.HistoryCursor).Order("id ASC").Find(&rows).Error; err != nil {
		return loaded, err
	}
	for _, row := range rows {
		loaded.Record.HistoryCursor = row.ID
		var metadata struct {
			RunID string `json:"run_id"`
		}
		_ = json.Unmarshal([]byte(row.Metadata), &metadata)
		if metadata.RunID != "" && covered[metadata.RunID] {
			continue
		}
		loaded.Messages = append(loaded.Messages, &schema.Message{Role: schema.RoleType(row.Role), Content: row.Content})
	}
	return loaded, nil
}

var ErrContextConflict = errors.New("session context changed concurrently")

// Save uses a revision fence so a slow/old request cannot overwrite newer memory.
func (m *SessionContextManager) Save(ctx context.Context, loaded LoadedContext, messages []*schema.Message, runID string) error {
	data, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	r := loaded.Record
	var runIDs []string
	if r.CoveredRunsJSON != "" {
		if err := json.Unmarshal([]byte(r.CoveredRunsJSON), &runIDs); err != nil {
			return err
		}
	}
	if runID != "" {
		runIDs = append(runIDs, runID)
	}
	if len(runIDs) > 128 {
		runIDs = runIDs[len(runIDs)-128:]
	}
	covered, _ := json.Marshal(runIDs)
	r.CoveredRunsJSON = string(covered)
	r.MessagesJSON, r.LastRunID, r.UpdatedAt = string(data), runID, time.Now()
	if r.Revision == 0 {
		r.Revision = 1
		result := m.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&r)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrContextConflict
		}
		return nil
	}
	result := m.db.WithContext(ctx).Model(&SessionContext{}).Where("user_id = ? AND session_id = ? AND revision = ?", r.UserID, r.SessionID, r.Revision).Updates(map[string]any{"revision": r.Revision + 1, "history_cursor": r.HistoryCursor, "last_run_id": r.LastRunID, "covered_runs_json": r.CoveredRunsJSON, "messages_json": r.MessagesJSON, "updated_at": r.UpdatedAt})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrContextConflict
	}
	return nil
}
