package model

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// ResponsesReplaySnapshot is an immutable, versioned local replay record. It
// does not store channel credentials or tool declarations. Conversation content
// may contain user-provided sensitive data. Top-level instructions and legacy
// Chat-only histories are not inherited.
// Each snapshot is capped at 1 MiB by the service. Expired records are removed
// in bounded batches on writes; expiry is enforced independently on every read.
type ResponsesReplaySnapshot struct {
	ID         int64  `gorm:"primaryKey"`
	ResponseID string `gorm:"type:varchar(128);uniqueIndex"`
	UserID     int    `gorm:"index"`
	TokenID    int
	Route      string `gorm:"type:varchar(64)"`
	Version    int
	Depth      int
	Input      ReplayText
	Output     ReplayText
	Status     string `gorm:"type:varchar(16)"`
	CreatedAt  int64
	ExpiresAt  int64 `gorm:"index"`
}

// ReplayText uses a portable large-text column rather than MySQL's 64 KiB TEXT.
type ReplayText string

func (ReplayText) GormDataType() string { return "string" }
func (ReplayText) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	if db.Dialector.Name() == "mysql" {
		return "LONGTEXT"
	}
	return "TEXT"
}

var ErrResponsesReplayNotFound = errors.New("previous_response_id is unavailable for this request")

func FindResponsesReplay(ctx context.Context, responseID string, userID, tokenID int, route string, now int64) (*ResponsesReplaySnapshot, error) {
	if DB == nil {
		return nil, errors.New("local responses replay storage unavailable")
	}
	var snapshot ResponsesReplaySnapshot
	err := DB.WithContext(ctx).Where("response_id = ? AND user_id = ? AND token_id = ? AND route = ? AND expires_at > ?", responseID, userID, tokenID, route, now).First(&snapshot).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrResponsesReplayNotFound
	}
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func InsertResponsesReplay(ctx context.Context, snapshot *ResponsesReplaySnapshot) error {
	if DB == nil {
		return errors.New("local responses replay storage unavailable")
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var expiredIDs []int64
		if err := tx.Model(&ResponsesReplaySnapshot{}).Where("expires_at <= ?", snapshot.CreatedAt).Order("expires_at").Limit(256).Pluck("id", &expiredIDs).Error; err != nil {
			return err
		}
		if len(expiredIDs) > 0 {
			if err := tx.Where("id IN ?", expiredIDs).Delete(&ResponsesReplaySnapshot{}).Error; err != nil {
				return err
			}
		}
		return tx.Create(snapshot).Error
	})
}
