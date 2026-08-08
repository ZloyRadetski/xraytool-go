package support_chat

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Store struct {
	db     *gorm.DB
	crypto *Crypto
}

func NewStore(db *gorm.DB, crypto *Crypto) *Store {
	return &Store{db: db, crypto: crypto}
}

// Crypto returns the crypto instance.
func (s *Store) Crypto() *Crypto {
	return s.crypto
}

// Conversation Filters
type ConversationFilter struct {
	UserID *string
	Status *string
}

// CreateConversation creates a new conversation.
func (s *Store) CreateConversation(ctx context.Context, userID, subject string) (*Conversation, error) {
	conv := &Conversation{
		ID:        uuid.New().String(),
		UserID:    userID,
		Subject:   subject,
		Status:    "open",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := s.db.WithContext(ctx).Create(conv).Error; err != nil {
		return nil, fmt.Errorf("failed to create conversation: %w", err)
	}
	return conv, nil
}

// GetConversation retrieves a conversation by ID.
func (s *Store) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	var convs []Conversation
	if err := s.db.WithContext(ctx).Where("id = ?", id).Limit(1).Find(&convs).Error; err != nil {
		return nil, err
	}
	if len(convs) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &convs[0], nil
}

// ListConversations lists conversations with optional filters.
func (s *Store) ListConversations(ctx context.Context, filter ConversationFilter) ([]Conversation, error) {
	query := s.db.WithContext(ctx).Model(&Conversation{})

	if filter.UserID != nil {
		query = query.Where("user_id = ?", *filter.UserID)
	}
	if filter.Status != nil {
		query = query.Where("status = ?", *filter.Status)
	}

	var convs []Conversation
	if err := query.Order("updated_at DESC").Find(&convs).Error; err != nil {
		return nil, err
	}
	return convs, nil
}

// UpdateStatus changes the status of a conversation.
func (s *Store) UpdateStatus(ctx context.Context, id string, status string) error {
	updates := map[string]interface{}{
		"status":     status,
		"updated_at": time.Now(),
	}
	if status == "closed" || status == "resolved" {
		t := time.Now()
		updates["closed_at"] = &t
	} else {
		updates["closed_at"] = nil
	}

	res := s.db.WithContext(ctx).Model(&Conversation{}).Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// MessageOutput is the decrypted representation of a message.
type MessageOutput struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	SenderRole     string     `json:"sender_role"`
	Text           string     `json:"text"`
	ReadAt         *time.Time   `json:"read_at"`
	CreatedAt      time.Time    `json:"created_at"`
	Attachments    []Attachment `json:"attachments"`
}

// CreateMessage encrypts and stores a new message.
func (s *Store) CreateMessage(ctx context.Context, convID, senderRole, text string, attachmentIDs []string) (*MessageOutput, error) {
	// Verify conversation exists
	if _, err := s.GetConversation(ctx, convID); err != nil {
		return nil, fmt.Errorf("conversation not found: %w", err)
	}

	ciphertext, nonce, err := s.crypto.Encrypt(convID, text)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt message: %w", err)
	}

	msg := &Message{
		ID:             uuid.New().String(),
		ConversationID: convID,
		SenderRole:     senderRole,
		Ciphertext:     ciphertext,
		Nonce:          nonce,
		CreatedAt:      time.Now(),
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(msg).Error; err != nil {
			return err
		}
		if len(attachmentIDs) > 0 {
			if err := tx.Model(&Attachment{}).Where("id IN ?", attachmentIDs).Update("message_id", msg.ID).Error; err != nil {
				return err
			}
		}
		return tx.Model(&Conversation{}).Where("id = ?", convID).Update("updated_at", time.Now()).Error
	})
	if err != nil {
		return nil, fmt.Errorf("failed to save message: %w", err)
	}

	var finalAttachments []Attachment
	if len(attachmentIDs) > 0 {
		s.db.WithContext(ctx).Where("message_id = ?", msg.ID).Find(&finalAttachments)
	}

	return &MessageOutput{
		ID:             msg.ID,
		ConversationID: msg.ConversationID,
		SenderRole:     msg.SenderRole,
		Text:           text,
		ReadAt:         nil,
		CreatedAt:      msg.CreatedAt,
		Attachments:    finalAttachments,
	}, nil
}

// ListMessages retrieves and decrypts all messages for a conversation.
func (s *Store) ListMessages(ctx context.Context, convID string) ([]MessageOutput, error) {
	var msgs []Message
	if err := s.db.WithContext(ctx).Preload("Attachments").Where("conversation_id = ?", convID).Order("created_at ASC").Find(&msgs).Error; err != nil {
		return nil, err
	}

	outputs := make([]MessageOutput, len(msgs))
	for i, m := range msgs {
		plaintext, err := s.crypto.Decrypt(convID, m.Ciphertext, m.Nonce)
		if err != nil {
			// In production, we might want to log this and return a placeholder instead of failing the whole list,
			// but failing is safer to detect master key changes.
			return nil, fmt.Errorf("failed to decrypt message %s: %w", m.ID, err)
		}
		outputs[i] = MessageOutput{
			ID:             m.ID,
			ConversationID: m.ConversationID,
			SenderRole:     m.SenderRole,
			Text:           plaintext,
			ReadAt:         m.ReadAt,
			CreatedAt:      m.CreatedAt,
			Attachments:    m.Attachments,
		}
	}
	return outputs, nil
}

// CreateAttachment creates a new unlinked attachment record.
func (s *Store) CreateAttachment(ctx context.Context, uploaderID, fileName, mimeType string, size int64, storagePath string, nonce []byte) (*Attachment, error) {
	att := &Attachment{
		ID:          uuid.New().String(),
		UploaderID:  uploaderID,
		FileName:    fileName,
		MimeType:    mimeType,
		Size:        size,
		StoragePath: storagePath,
		Nonce:       nonce,
		CreatedAt:   time.Now(),
	}
	if err := s.db.WithContext(ctx).Create(att).Error; err != nil {
		return nil, err
	}
	return att, nil
}

// GetAttachment retrieves an attachment by ID.
func (s *Store) GetAttachment(ctx context.Context, id string) (*Attachment, error) {
	var att Attachment
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&att).Error; err != nil {
		return nil, err
	}
	return &att, nil
}

// UserOwnsAttachment checks if the user has access to the attachment's conversation.
func (s *Store) UserOwnsAttachment(ctx context.Context, userID string, att *Attachment) bool {
	if att.MessageID == nil || *att.MessageID == "" {
		return att.UploaderID == userID
	}

	var msg Message
	if err := s.db.WithContext(ctx).Where("id = ?", *att.MessageID).First(&msg).Error; err != nil {
		return false
	}

	var conv Conversation
	if err := s.db.WithContext(ctx).Where("id = ?", msg.ConversationID).First(&conv).Error; err != nil {
		return false
	}

	return conv.UserID == userID
}

// MarkMessagesRead marks messages as read for a given conversation.
// If readerRole is "client", it marks "admin" messages as read, and vice versa.
func (s *Store) MarkMessagesRead(ctx context.Context, convID, readerRole string) error {
	targetRole := "admin"
	if readerRole == "admin" {
		targetRole = "client"
	}

	return s.db.WithContext(ctx).Model(&Message{}).
		Where("conversation_id = ? AND sender_role = ? AND read_at IS NULL", convID, targetRole).
		Update("read_at", time.Now()).Error
}
