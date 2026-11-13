package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const sessionContextVersion = 1

// SessionContext is the model-facing snapshot, separate from raw UI history.
// ContextVersion marks the one-time migration from cursor/covered-runs loading.
type SessionContext struct {
	UserID         string `gorm:"primaryKey;size:128"`
	SessionID      string `gorm:"primaryKey;size:128"`
	Revision       uint64
	ContextVersion uint   `gorm:"not null;default:0"`
	LastRunID      string `gorm:"size:64"` // retry identifier for the latest commit
	MessagesJSON   string `gorm:"type:longtext"`
	UpdatedAt      time.Time
}

type LoadedContext struct {
	Record   SessionContext
	Messages []*schema.Message
}

type SessionContextManager struct{ db *gorm.DB }

func NewSessionContextManager(db *gorm.DB) *SessionContextManager {
	return &SessionContextManager{db: db}
}

var (
	ErrContextConflict = errors.New("session context changed concurrently")
	ErrLegacyContext   = errors.New("session context requires startup migration")
	ErrTurnPersistence = errors.New("conversation turn persistence failed")
)

func (m *SessionContextManager) Load(ctx context.Context, owner, session string) (LoadedContext, error) {
	loaded := LoadedContext{Record: SessionContext{UserID: owner, SessionID: session, ContextVersion: sessionContextVersion}}
	err := m.db.WithContext(ctx).Where("user_id = ? AND session_id = ?", owner, session).First(&loaded.Record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return loaded, nil
	}
	if err != nil {
		return loaded, err
	}
	if loaded.Record.ContextVersion != sessionContextVersion {
		return loaded, ErrLegacyContext
	}
	if loaded.Record.MessagesJSON != "" {
		err = json.Unmarshal([]byte(loaded.Record.MessagesJSON), &loaded.Messages)
	}
	return loaded, err
}

// CommitTurn atomically saves the working snapshot and both raw utterances.
// A stale revision, including one racing a history clear, rolls back the whole
// turn. Model calls happen before this short database transaction.
func (m *SessionContextManager) CommitTurn(ctx context.Context, loaded LoadedContext, messages []*schema.Message, runID, query, answer string) error {
	if loaded.Record.UserID == "" || loaded.Record.SessionID == "" || runID == "" || strings.TrimSpace(query) == "" || strings.TrimSpace(answer) == "" {
		return errors.New("turn requires an owner, session, run ID, question and answer")
	}
	data, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	metadata, _ := json.Marshal(map[string]string{"run_id": runID})
	return m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r := loaded.Record
		var current SessionContext
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND session_id = ?", r.UserID, r.SessionID).First(&current).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			if current.ContextVersion != sessionContextVersion {
				return ErrLegacyContext
			}
			if current.LastRunID == runID {
				if current.MessagesJSON != string(data) {
					return ErrContextConflict
				}
				return matchingLastTurn(tx, r.UserID, r.SessionID, runID, query, answer)
			}
			if current.Revision != r.Revision {
				return ErrContextConflict
			}
		} else if r.Revision != 0 {
			return ErrContextConflict
		}
		now := time.Now()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			r.Revision, r.ContextVersion = 1, sessionContextVersion
			r.LastRunID, r.MessagesJSON, r.UpdatedAt = runID, string(data), now
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&r)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrContextConflict
			}
		} else {
			result := tx.Model(&SessionContext{}).Where("user_id = ? AND session_id = ? AND revision = ?", r.UserID, r.SessionID, r.Revision).
				Updates(map[string]any{"revision": r.Revision + 1, "last_run_id": runID, "messages_json": string(data), "updated_at": now})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrContextConflict
			}
		}
		return tx.Create(&[]ConversationHistory{
			{UserID: r.UserID, SessionID: r.SessionID, Role: "user", Content: query, Metadata: string(metadata), CreatedAt: now},
			{UserID: r.UserID, SessionID: r.SessionID, Role: "assistant", Content: answer, Metadata: string(metadata), CreatedAt: now},
		}).Error
	})
}

// LastRunID only recognizes retries of the latest transaction; it is not used
// for context loading. An older retry fails the revision check instead.
func matchingLastTurn(tx *gorm.DB, owner, session, runID, query, answer string) error {
	var rows []ConversationHistory
	if err := tx.Where("user_id = ? AND session_id = ?", owner, session).Order("id DESC").Limit(2).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) != 2 || rows[0].Role != "assistant" || rows[0].Content != answer || rows[1].Role != "user" || rows[1].Content != query {
		return ErrContextConflict
	}
	for _, row := range rows {
		var metadata struct {
			RunID string `json:"run_id"`
		}
		if json.Unmarshal([]byte(row.Metadata), &metadata) != nil || metadata.RunID != runID {
			return ErrContextConflict
		}
	}
	return nil
}
