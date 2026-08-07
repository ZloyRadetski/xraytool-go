package support_chat

import (
	"time"
)

// Conversation represents a support chat between a user and admins.
type Conversation struct {
	ID        string    `gorm:"primaryKey;type:varchar(36)"`
	UserID    string    `gorm:"index;type:varchar(36);not null"`
	Subject   string    `gorm:"type:text"`
	Status    string    `gorm:"index;type:varchar(20);not null;default:'open'"`
	CreatedAt time.Time `gorm:"autoCreateTime;not null"`
	UpdatedAt time.Time `gorm:"autoUpdateTime;not null"`
	ClosedAt  *time.Time
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
