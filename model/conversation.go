package model

import (
	"encoding/json"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

type ConversationResponse struct {
	ID                 int64           `json:"id" gorm:"primary_key;AUTO_INCREMENT"`
	ResponseID         string          `json:"response_id" gorm:"type:varchar(64);uniqueIndex"`
	UserID             int             `json:"user_id" gorm:"index"`
	PreviousResponseID string          `json:"previous_response_id" gorm:"type:varchar(64);index"`
	Model              string          `json:"model" gorm:"type:varchar(64)"`
	Messages           json.RawMessage `json:"messages" gorm:"type:json"`
	CreatedAt          int64           `json:"created_at"`
	UpdatedAt          int64           `json:"updated_at"`
}

func (c *ConversationResponse) Insert() error {
	c.CreatedAt = time.Now().Unix()
	c.UpdatedAt = c.CreatedAt
	return DB.Create(c).Error
}

func GetConversationByResponseID(userID int, responseID string) (*ConversationResponse, bool, error) {
	var conv ConversationResponse
	err := DB.Where("user_id = ? AND response_id = ?", userID, responseID).First(&conv).Error
	if err != nil {
		if err.Error() == "record not found" {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &conv, true, nil
}

func (c *ConversationResponse) SetMessages(messages []dto.Message) error {
	b, err := common.Marshal(messages)
	if err != nil {
		return err
	}
	c.Messages = json.RawMessage(b)
	return nil
}

func (c *ConversationResponse) GetMessages() ([]dto.Message, error) {
	if len(c.Messages) == 0 {
		return nil, nil
	}
	var msgs []dto.Message
	if err := common.Unmarshal(c.Messages, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}
