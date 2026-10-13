package memory

import (
	"context"
	"fmt"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"shanyangcode.com/pilot/internal/config"
)

var errDBNotInitialized = fmt.Errorf("database is not initialized")

// OpenDB 建立 MySQL 连接池并自动迁移表结构。
func OpenDB(ctx context.Context, cfg config.Database) (*gorm.DB, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Name)

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("connect to database failed: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql db failed: %w", err)
	}
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetimeSeconds) * time.Second)
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping database failed: %w", err)
	}

	// 自动迁移表结构
	if err := db.WithContext(ctx).AutoMigrate(&ConversationHistory{}, &InteractionRecord{}, &InteractionFeedback{}, &AgentRun{}, &SessionContext{}); err != nil {
		return nil, fmt.Errorf("auto migrate failed: %w", err)
	}

	return db, nil
}

// PingDB 探测 MySQL 连通性，供 readiness 使用。
func PingDB(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errDBNotInitialized
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("get sql db failed: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database failed: %w", err)
	}
	return nil
}

// CloseDB 关闭数据库连接池。
func CloseDB(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// ConversationHistory 对话历史表
type ConversationHistory struct {
	ID        uint   `gorm:"primaryKey"`
	UserID    string `gorm:"type:varchar(128);index;not null"`
	SessionID string `gorm:"type:varchar(128);index;not null"`
	Role      string `gorm:"type:varchar(32);not null"` // user, assistant, system
	Content   string `gorm:"type:text"`
	Metadata  string `gorm:"type:json"` // 额外元数据
	CreatedAt time.Time
}

// BeforeCreate 兜底修正 JSON 字段，避免空字符串写入 JSON 列报错。
func (h *ConversationHistory) BeforeCreate(tx *gorm.DB) (err error) {
	if h.Metadata == "" {
		h.Metadata = "{}"
	}
	return nil
}

// ConversationHistoryManager 对话历史管理器
type ConversationHistoryManager struct {
	db *gorm.DB
}

// NewConversationHistoryManager 创建对话历史管理器
func NewConversationHistoryManager(db *gorm.DB) *ConversationHistoryManager {
	return &ConversationHistoryManager{db: db}
}

// SaveMessage 保存对话消息
func (m *ConversationHistoryManager) SaveMessage(ctx context.Context, userID, sessionID, role, content string) error {
	if m == nil || m.db == nil {
		return errDBNotInitialized
	}
	history := ConversationHistory{
		UserID:    userID,
		SessionID: sessionID,
		Role:      role,
		Content:   content,
		Metadata:  "{}",
		CreatedAt: time.Now(),
	}
	return m.db.WithContext(ctx).Create(&history).Error
}

// GetSessionHistory 获取会话历史
func (m *ConversationHistoryManager) GetSessionHistory(ctx context.Context, userID, sessionID string, limit int) ([]ConversationHistory, error) {
	if m == nil || m.db == nil {
		return nil, errDBNotInitialized
	}
	if limit <= 0 {
		limit = 100
	}

	var history []ConversationHistory
	err := m.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("created_at DESC").
		Limit(limit).
		Find(&history).Error
	if err != nil {
		return nil, err
	}

	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}
	return history, nil
}

// DeleteSessionHistory 删除会话历史
func (m *ConversationHistoryManager) DeleteSessionHistory(ctx context.Context, userID, sessionID string) error {
	if m == nil || m.db == nil {
		return errDBNotInitialized
	}
	return m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Keep a revision tombstone: an in-flight first request must not recreate
		// working context after the user cleared this session.
		blank := SessionContext{UserID: userID, SessionID: sessionID, Revision: 1, MessagesJSON: "[]", CoveredRunsJSON: "[]"}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "session_id"}}, DoUpdates: clause.Assignments(map[string]any{"revision": gorm.Expr("revision + 1"), "messages_json": "[]", "covered_runs_json": "[]", "history_cursor": 0, "last_run_id": "", "updated_at": time.Now()})}).Create(&blank).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND session_id = ?", userID, sessionID).Delete(&ConversationHistory{}).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ? AND session_id = ?", userID, sessionID).Delete(&AgentRun{}).Error
	})
}
