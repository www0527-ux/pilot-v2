package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
)

// migrateSessionContexts runs at startup before accepting requests. Legacy
// writers must be stopped during upgrade. The old columns are retained on disk
// for migration only; normal context loading never reads raw history.
func migrateSessionContexts(ctx context.Context, db *gorm.DB) error {
	legacyColumns := db.Migrator().HasColumn("session_contexts", "history_cursor") && db.Migrator().HasColumn("session_contexts", "covered_runs_json")
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var records []SessionContext
		if err := tx.Where("context_version = ?", 0).Find(&records).Error; err != nil {
			return err
		}
		for _, record := range records {
			var messages []*schema.Message
			if record.MessagesJSON != "" {
				if err := json.Unmarshal([]byte(record.MessagesJSON), &messages); err != nil {
					return fmt.Errorf("decode legacy session context: %w", err)
				}
			}
			var legacy struct {
				HistoryCursor   uint
				CoveredRunsJSON string
			}
			if legacyColumns {
				if err := tx.Table("session_contexts").Select("history_cursor, covered_runs_json").Where("user_id = ? AND session_id = ?", record.UserID, record.SessionID).Take(&legacy).Error; err != nil {
					return err
				}
			}
			covered := map[string]bool{}
			var runIDs []string
			if legacy.CoveredRunsJSON != "" {
				if err := json.Unmarshal([]byte(legacy.CoveredRunsJSON), &runIDs); err != nil {
					return err
				}
			}
			for _, id := range runIDs {
				covered[id] = true
			}
			if record.LastRunID != "" {
				covered[record.LastRunID] = true
			}
			var rows []ConversationHistory
			if err := tx.Where("user_id = ? AND session_id = ? AND id > ?", record.UserID, record.SessionID, legacy.HistoryCursor).Order("id ASC").Find(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				var metadata struct {
					RunID string `json:"run_id"`
				}
				_ = json.Unmarshal([]byte(row.Metadata), &metadata)
				if metadata.RunID != "" && covered[metadata.RunID] {
					continue
				}
				messages = append(messages, &schema.Message{Role: schema.RoleType(row.Role), Content: row.Content})
			}
			data, err := json.Marshal(messages)
			if err != nil {
				return err
			}
			result := tx.Model(&SessionContext{}).Where("user_id = ? AND session_id = ? AND revision = ? AND context_version = 0", record.UserID, record.SessionID, record.Revision).
				Updates(map[string]any{"messages_json": string(data), "context_version": sessionContextVersion, "revision": record.Revision + 1, "updated_at": time.Now()})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrContextConflict
			}
		}

		// Import history-only legacy sessions once. Existing snapshots, including
		// empty revision tombstones, are authoritative and are never rebuilt here.
		var sessions []struct{ UserID, SessionID string }
		if err := tx.Table("conversation_histories AS h").Select("DISTINCT h.user_id, h.session_id").
			Joins("LEFT JOIN session_contexts AS c ON c.user_id = h.user_id AND c.session_id = h.session_id").
			Where("c.user_id IS NULL").Scan(&sessions).Error; err != nil {
			return err
		}
		for _, session := range sessions {
			var rows []ConversationHistory
			if err := tx.Where("user_id = ? AND session_id = ?", session.UserID, session.SessionID).Order("id ASC").Find(&rows).Error; err != nil {
				return err
			}
			messages := make([]*schema.Message, 0, len(rows))
			for _, row := range rows {
				messages = append(messages, &schema.Message{Role: schema.RoleType(row.Role), Content: row.Content})
			}
			data, err := json.Marshal(messages)
			if err != nil {
				return err
			}
			if err := tx.Create(&SessionContext{UserID: session.UserID, SessionID: session.SessionID, Revision: 1, ContextVersion: sessionContextVersion, MessagesJSON: string(data), UpdatedAt: time.Now()}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
