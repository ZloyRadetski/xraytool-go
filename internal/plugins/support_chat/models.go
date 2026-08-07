package support_chat

import (
	"time"
)

// Conversation represents a support chat between a user and admins.
type Conversation struct {
	ID        string    `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID    string    `gorm:"index;type:varchar(36);not null" json:"user_id"`
	Subject   string    `gorm:"type:text" json:"subject"`
	Status    string    `gorm:"index;type:varchar(20);not null;default:'open'" json:"status"`
	CreatedAt time.Time `gorm:"autoCreateTime;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime;not null" json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
}

// Message represents an encrypted message in a conversation.
type Message struct {
	ID             string    `gorm:"primaryKey;type:varchar(36)"`
	ConversationID string    `gorm:"index;type:varchar(36);not null"`
	SenderRole     string    `gorm:"type:varchar(20);not null"` // 'client' or 'admin'
	Ciphertext     []byte    `gorm:"type:blob;not null"`
	Nonce          []byte    `gorm:"type:blob;not null"`
	ReadAt         *time.Time `gorm:"index"`
	CreatedAt      time.Time `gorm:"autoCreateTime;not null"`
}
