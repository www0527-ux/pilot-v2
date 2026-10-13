package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"shanyangcode.com/pilot/internal/ai/multiagent"
)

const (
	FeedbackUp   = "up"
	FeedbackDown = "down"
)

var ErrInteractionNotFound = errors.New("interaction not found")

// InteractionRecord gives every generated answer a stable feedback target.
type InteractionRecord struct {
	ID        string `gorm:"type:varchar(64);primaryKey"`
	UserID    string `gorm:"type:varchar(128);index;not null"`
	SessionID string `gorm:"type:varchar(128);index;not null"`
	Mode      string `gorm:"type:varchar(32);index;not null"`
	Status    string `gorm:"type:varchar(32);index;not null"`
	Query     string `gorm:"type:text"`
	Answer    string `gorm:"type:text"`
	Snapshot  string `gorm:"type:longtext"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (InteractionRecord) TableName() string { return "interactions" }

type InteractionFeedback struct {
	InteractionID   string `gorm:"type:varchar(64);primaryKey"`
	Vote            string `gorm:"type:varchar(16);not null"`
	Improvement     string `gorm:"type:text"`
	PromotionStatus string `gorm:"type:varchar(32);index;not null"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (InteractionFeedback) TableName() string { return "interaction_feedback" }

type FeedbackOutcome struct {
	InteractionID   string `json:"interaction_id"`
	Vote            string `json:"vote"`
	PromotionStatus string `json:"promotion_status"`
	Idempotent      bool   `json:"idempotent"`
}

type FeedbackManager struct {
	db       *gorm.DB
	promoter multiagent.EpisodicRecorder
}

func NewFeedbackManager(db *gorm.DB, promoter multiagent.EpisodicRecorder) *FeedbackManager {
	return &FeedbackManager{db: db, promoter: promoter}
}

func EnsureFeedbackSchema(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errDBNotInitialized
	}
	return db.WithContext(ctx).AutoMigrate(&InteractionRecord{}, &InteractionFeedback{})
}

func (m *FeedbackManager) RecordInteraction(ctx context.Context, record InteractionRecord) error {
	if m == nil || m.db == nil {
		return errDBNotInitialized
	}
	if strings.TrimSpace(record.ID) == "" {
		return fmt.Errorf("interaction id is required")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	return m.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error
}

func (m *FeedbackManager) Submit(ctx context.Context, interactionID, userID, sessionID, vote, improvement string) (FeedbackOutcome, error) {
	if m == nil || m.db == nil {
		return FeedbackOutcome{}, errDBNotInitialized
	}
	vote = strings.ToLower(strings.TrimSpace(vote))
	if vote != FeedbackUp && vote != FeedbackDown {
		return FeedbackOutcome{}, fmt.Errorf("vote must be up or down")
	}
	var interaction InteractionRecord
	var prior InteractionFeedback
	idempotent := false
	promotionStatus := "not_eligible"
	shouldPromote := false
	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND session_id = ?", interactionID, userID, sessionID).First(&interaction).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrInteractionNotFound
			}
			return err
		}
		foundPrior := tx.Where("interaction_id = ?", interactionID).First(&prior).Error == nil
		idempotent = foundPrior && prior.Vote == vote && prior.Improvement == improvement
		promotionStatus = "not_eligible"
		if foundPrior && (prior.PromotionStatus == "promoted" || prior.PromotionStatus == "processing") {
			promotionStatus = prior.PromotionStatus
		}
		if vote == FeedbackUp && interactionEligibleForPromotion(interaction) {
			promotionStatus = "processing"
			if foundPrior && (prior.PromotionStatus == "promoted" || prior.PromotionStatus == "processing") {
				promotionStatus = prior.PromotionStatus
			} else {
				shouldPromote = true
			}
		}
		feedback := InteractionFeedback{InteractionID: interactionID, Vote: vote, Improvement: truncateRunes(improvement, 2000), PromotionStatus: promotionStatus}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "interaction_id"}}, DoUpdates: clause.Assignments(map[string]any{
			"vote": feedback.Vote, "improvement": feedback.Improvement, "promotion_status": feedback.PromotionStatus, "updated_at": time.Now(),
		})}).Create(&feedback).Error
	})
	if err != nil {
		return FeedbackOutcome{}, err
	}

	if shouldPromote {
		promotionStatus = m.promote(ctx, interaction)
	}
	return FeedbackOutcome{InteractionID: interactionID, Vote: vote, PromotionStatus: promotionStatus, Idempotent: idempotent}, nil
}

func interactionEligibleForPromotion(record InteractionRecord) bool {
	status := strings.ToLower(strings.TrimSpace(record.Status))
	if strings.TrimSpace(record.Answer) == "" {
		return false
	}
	switch status {
	case "success", "completed", "accepted":
		return true
	default:
		return false
	}
}

func (m *FeedbackManager) promote(ctx context.Context, interaction InteractionRecord) string {
	if interaction.Mode != "multi-agent" || m.promoter == nil {
		_ = m.updatePromotionStatus(ctx, interaction.ID, "long_term_only")
		return "long_term_only"
	}
	var result multiagent.Result
	if err := json.Unmarshal([]byte(interaction.Snapshot), &result); err != nil {
		slog.WarnContext(ctx, "decode interaction snapshot for promotion failed", "error", err, "interaction_id", interaction.ID)
		_ = m.updatePromotionStatus(ctx, interaction.ID, "failed")
		return "failed"
	}
	ids := make([]string, 0)
	for _, finding := range result.Findings {
		if finding.Metadata != nil && finding.Metadata["memory"] == true {
			continue
		}
		for _, item := range finding.Evidence {
			ids = append(ids, item.ID)
		}
	}
	limitations := make([]string, 0, len(result.Limitations))
	for _, limitation := range result.Limitations {
		limitations = append(limitations, limitation.Message)
	}
	err := m.promoter.Record(ctx, multiagent.EpisodicRecord{
		UserID: interaction.UserID, SessionID: interaction.SessionID, Service: result.Plan.Service,
		Intent: result.Plan.Intent, Query: interaction.Query, Answer: interaction.Answer,
		EvidenceIDs: ids, Limitations: limitations,
	})
	if err != nil {
		slog.WarnContext(ctx, "episodic promotion failed; feedback remains stored", "error", err, "interaction_id", interaction.ID)
		_ = m.updatePromotionStatus(ctx, interaction.ID, "failed")
		return "failed"
	}
	_ = m.updatePromotionStatus(ctx, interaction.ID, "promoted")
	return "promoted"
}

func (m *FeedbackManager) updatePromotionStatus(ctx context.Context, interactionID, status string) error {
	return m.db.WithContext(ctx).Model(&InteractionFeedback{}).Where("interaction_id = ?", interactionID).Update("promotion_status", status).Error
}
