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
	Attachments    []Attachment `gorm:"foreignKey:MessageID"`
}

// Attachment represents a file (image/video) attached to a message.
type Attachment struct {
	ID          string    `gorm:"primaryKey;type:varchar(36)" json:"id"`
	MessageID   *string   `gorm:"index;type:varchar(36)" json:"message_id,omitempty"` // null until linked
	UploaderID  string    `gorm:"index;type:varchar(128);not null" json:"-"`          // user_id or admin who uploaded it
	FileName    string    `gorm:"type:text;not null" json:"file_name"`
	MimeType    string    `gorm:"type:varchar(128);not null" json:"mime_type"`
	Size        int64     `gorm:"not null" json:"size"`
	StoragePath string    `gorm:"type:text;not null" json:"-"`
	Nonce       []byte    `gorm:"type:blob;not null" json:"-"`
	CreatedAt   time.Time `gorm:"autoCreateTime;not null" json:"created_at"`
}
