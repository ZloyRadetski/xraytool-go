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
	ReadAt         *time.Time `json:"read_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// CreateMessage encrypts and stores a new message.
func (s *Store) CreateMessage(ctx context.Context, convID, senderRole, text string) (*MessageOutput, error) {
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
		return tx.Model(&Conversation{}).Where("id = ?", convID).Update("updated_at", time.Now()).Error
	})
	if err != nil {
		return nil, fmt.Errorf("failed to save message: %w", err)
	}

	return &MessageOutput{
		ID:             msg.ID,
		ConversationID: msg.ConversationID,
		SenderRole:     msg.SenderRole,
		Text:           text,
		ReadAt:         nil,
		CreatedAt:      msg.CreatedAt,
	}, nil
}

// ListMessages retrieves and decrypts all messages for a conversation.
func (s *Store) ListMessages(ctx context.Context, convID string) ([]MessageOutput, error) {
	var msgs []Message
	if err := s.db.WithContext(ctx).Where("conversation_id = ?", convID).Order("created_at ASC").Find(&msgs).Error; err != nil {
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
		}
	}
	return outputs, nil
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
