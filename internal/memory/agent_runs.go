package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AgentRun is a replayable UI journal, separate from model conversation memory.
type AgentRun struct {
	ID          string    `gorm:"primaryKey;type:varchar(64)" json:"id"`
	ParentRunID string    `gorm:"type:varchar(64);index" json:"parent_run_id,omitempty"`
	UserID      string    `gorm:"type:varchar(128);index" json:"user_id"`
	SessionID   string    `gorm:"type:varchar(128);index" json:"session_id"`
	Query       string    `gorm:"type:text" json:"query"`
	Mode        string    `json:"mode"`
	Status      string    `json:"status"`
	Answer      string    `gorm:"type:longtext" json:"answer"`
	EventsJSON  string    `gorm:"type:longtext" json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (m *ConversationHistoryManager) SaveAgentRun(ctx context.Context, record AgentRun) error {
	return m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Do not let a caller reuse another account's UUID to take over its run.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error; err != nil {
			return err
		}
		var existing AgentRun
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", record.ID).First(&existing).Error; err != nil {
			return err
		}
		if existing.UserID != record.UserID || existing.SessionID != record.SessionID {
			return fmt.Errorf("run ownership conflict")
		}
		record.CreatedAt = existing.CreatedAt
		return tx.Model(&AgentRun{}).Where("id = ? AND user_id = ? AND session_id = ?", record.ID, record.UserID, record.SessionID).Select("parent_run_id", "query", "mode", "status", "answer", "events_json", "updated_at").Updates(&record).Error
	})
}
func (m *ConversationHistoryManager) GetAgentRun(ctx context.Context, id, userID, sessionID string) (AgentRun, error) {
	var record AgentRun
	err := m.db.WithContext(ctx).Where("id = ? AND user_id = ? AND session_id = ?", id, userID, sessionID).First(&record).Error
	return record, err
}
func (m *ConversationHistoryManager) GetSessionRuns(ctx context.Context, userID, sessionID string) ([]AgentRun, error) {
	var records []AgentRun
	err := m.db.WithContext(ctx).Where("user_id = ? AND session_id = ?", userID, sessionID).Order("created_at DESC").Limit(100).Find(&records).Error
	return records, err
}
func (m *ConversationHistoryManager) SaveMessageForRun(ctx context.Context, userID, sessionID, role, content, runID string) error {
	metadata, _ := json.Marshal(map[string]string{"run_id": runID})
	return m.db.WithContext(ctx).Create(&ConversationHistory{UserID: userID, SessionID: sessionID, Role: role, Content: content, Metadata: string(metadata), CreatedAt: time.Now()}).Error
}
