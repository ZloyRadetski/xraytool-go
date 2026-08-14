package support_chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Store struct {
	db            *gorm.DB
	crypto        *Crypto
	legacyCryptos []*Crypto
	keyVersion    uint16
}

type StoreOption func(*Store)

func WithLegacyCryptos(cryptos ...*Crypto) StoreOption {
	return func(s *Store) { s.legacyCryptos = append(s.legacyCryptos, cryptos...) }
}

func WithKeyVersion(version uint16) StoreOption {
	return func(s *Store) {
		if version > 0 {
			s.keyVersion = version
		}
	}
}

func NewStore(db *gorm.DB, crypto *Crypto, options ...StoreOption) *Store {
	store := &Store{db: db, crypto: crypto, keyVersion: 1}
	for _, option := range options {
		option(store)
	}
	return store
}

// Crypto returns the active encryption key. New writes must always use it;
// legacy keys are read-only and exist solely to migrate historic records.
func (s *Store) Crypto() *Crypto { return s.crypto }

func (s *Store) allCryptos() []*Crypto {
	cryptos := make([]*Crypto, 0, len(s.legacyCryptos)+1)
	if s.crypto != nil {
		cryptos = append(cryptos, s.crypto)
	}
	cryptos = append(cryptos, s.legacyCryptos...)
	return cryptos
}

func (s *Store) decryptField(purpose, recordID string, ciphertext, nonce []byte) (string, error) {
	var lastErr error
	for _, crypto := range s.allCryptos() {
		plaintext, err := crypto.DecryptField(purpose, recordID, ciphertext, nonce)
		if err == nil {
			return plaintext, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("decrypt %s: %w", purpose, lastErr)
}

func (s *Store) decryptLegacy(conversationID string, ciphertext, nonce []byte) (string, error) {
	var lastErr error
	for _, crypto := range s.allCryptos() {
		plaintext, err := crypto.Decrypt(conversationID, ciphertext, nonce)
		if err == nil {
			return plaintext, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("decrypt legacy record: %w", lastErr)
}

// Conversation Filters
type ConversationFilter struct {
	UserID *string
	Status *string
}

// CreateConversation stores user identity and subject only as encrypted data.
func (s *Store) CreateConversation(ctx context.Context, userID, subject string) (*Conversation, error) {
	conv := &Conversation{
		ID:                uuid.New().String(),
		Status:            "open",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
		EncryptionVersion: currentEncryptionVersion,
		KeyVersion:        s.keyVersion,
		UserID:            userID,
		Subject:           subject,
	}
	var err error
	if conv.UserIDCiphertext, conv.UserIDNonce, err = s.crypto.EncryptField("conversation-user-id", conv.ID, userID); err != nil {
		return nil, fmt.Errorf("encrypt conversation user: %w", err)
	}
	conv.UserIDHash = s.crypto.BlindIndex("conversation-user-id", userID)
	if conv.SubjectCiphertext, conv.SubjectNonce, err = s.crypto.EncryptField("conversation-subject", conv.ID, subject); err != nil {
		return nil, fmt.Errorf("encrypt conversation subject: %w", err)
	}
	if err := s.db.WithContext(ctx).Create(conv).Error; err != nil {
		return nil, fmt.Errorf("create conversation: %w", err)
	}
	return conv, nil
}

func (s *Store) hydrateConversation(conv *Conversation) error {
	if conv.EncryptionVersion < currentEncryptionVersion {
		conv.UserID = conv.LegacyUserID
		conv.Subject = conv.LegacySubject
		return nil
	}
	userID, err := s.decryptField("conversation-user-id", conv.ID, conv.UserIDCiphertext, conv.UserIDNonce)
	if err != nil {
		return err
	}
	subject, err := s.decryptField("conversation-subject", conv.ID, conv.SubjectCiphertext, conv.SubjectNonce)
	if err != nil {
		return err
	}
	conv.UserID = userID
	conv.Subject = subject
	return nil
}

// GetConversation retrieves and decrypts one conversation.
func (s *Store) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	var conv Conversation
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&conv).Error; err != nil {
		return nil, err
	}
	if err := s.hydrateConversation(&conv); err != nil {
		return nil, fmt.Errorf("decrypt conversation: %w", err)
	}
	return &conv, nil
}

// ListConversations lists conversations with optional filters. The user lookup
// uses a blind index for v2 records and a temporary plaintext fallback for rows
// that have not been migrated yet.
func (s *Store) ListConversations(ctx context.Context, filter ConversationFilter) ([]Conversation, error) {
	query := s.db.WithContext(ctx).Model(&Conversation{})
	if filter.UserID != nil {
		userIDHash := s.crypto.BlindIndex("conversation-user-id", *filter.UserID)
		query = query.Where("user_id_hash = ? OR (encryption_version < ? AND user_id = ?)", userIDHash, currentEncryptionVersion, *filter.UserID)
	}
	if filter.Status != nil {
		query = query.Where("status = ?", *filter.Status)
	}
	var convs []Conversation
	if err := query.Order("updated_at DESC").Find(&convs).Error; err != nil {
		return nil, err
	}
	for i := range convs {
		if err := s.hydrateConversation(&convs[i]); err != nil {
			return nil, fmt.Errorf("decrypt conversation %s: %w", convs[i].ID, err)
		}
	}
	return convs, nil
}

func (s *Store) UpdateStatus(ctx context.Context, id string, status string) error {
	updates := map[string]interface{}{"status": status, "updated_at": time.Now()}
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

// DeleteConversation removes a conversation, all its messages, and any
// unreferenced attachment blobs from database and disk.
func (s *Store) DeleteConversation(ctx context.Context, id string, mediaStoragePath string) error {
	var filesToDelete []string

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. Verify conversation exists
		var conv Conversation
		if err := tx.Where("id = ?", id).First(&conv).Error; err != nil {
			return err
		}

		// 2. Find all messages for this conversation
		var msgs []Message
		if err := tx.Where("conversation_id = ?", id).Find(&msgs).Error; err != nil {
			return err
		}

		if len(msgs) > 0 {
			msgIDs := make([]string, len(msgs))
			for i, m := range msgs {
				msgIDs[i] = m.ID
			}

			// 3. Find attachments linked to these messages
			var attachments []Attachment
			if err := tx.Where("message_id IN ?", msgIDs).Find(&attachments).Error; err != nil {
				return err
			}

			if len(attachments) > 0 {
				attIDs := make([]string, len(attachments))
				for i, a := range attachments {
					attIDs[i] = a.ID
				}

				// Check blob references for each attachment
				for _, att := range attachments {
					storageKey := attachmentStorageKey(&att)
					var remainingCount int64
					if err := tx.Model(&Attachment{}).
						Where("(storage_key = ? OR id = ?) AND id NOT IN ?", storageKey, storageKey, attIDs).
						Count(&remainingCount).Error; err != nil {
						return err
					}

					if remainingCount == 0 {
						// Safe to delete blob record and physical file
						if err := tx.Where("storage_key = ?", storageKey).Delete(&AttachmentBlob{}).Error; err != nil {
							return err
						}
						filePath := ""
						if att.EncryptionVersion >= currentEncryptionVersion && mediaStoragePath != "" {
							filePath = attachmentStoragePath(mediaStoragePath, storageKey)
						} else if att.LegacyStoragePath != "" {
							filePath = att.LegacyStoragePath
						}
						if filePath != "" {
							filesToDelete = append(filesToDelete, filePath)
						}
					}
				}

				// Delete attachment records
				if err := tx.Where("id IN ?", attIDs).Delete(&Attachment{}).Error; err != nil {
					return err
				}
			}

			// Delete message records
			if err := tx.Where("conversation_id = ?", id).Delete(&Message{}).Error; err != nil {
				return err
			}
		}

		// 4. Delete the conversation itself
		res := tx.Where("id = ?", id).Delete(&Conversation{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})

	if err != nil {
		return err
	}

	// Clean up unreferenced media files after successful DB transaction
	for _, filePath := range filesToDelete {
		_ = os.Remove(filePath)
	}

	return nil
}


// MessageOutput is the decrypted representation returned by the API.
type MessageOutput struct {
	ID             string       `json:"id"`
	ConversationID string       `json:"conversation_id"`
	SenderRole     string       `json:"sender_role"`
	Text           string       `json:"text"`
	ReadAt         *time.Time   `json:"read_at"`
	CreatedAt      time.Time    `json:"created_at"`
	Attachments    []Attachment `json:"attachments"`
}

func (s *Store) CreateMessage(ctx context.Context, convID, senderRole, text string, attachmentIDs []string) (*MessageOutput, error) {
	if _, err := s.GetConversation(ctx, convID); err != nil {
		return nil, fmt.Errorf("conversation not found: %w", err)
	}
	msg := &Message{
		ID:                uuid.New().String(),
		ConversationID:    convID,
		SenderRole:        senderRole,
		CreatedAt:         time.Now(),
		EncryptionVersion: currentEncryptionVersion,
		KeyVersion:        s.keyVersion,
	}
	var err error
	if msg.Ciphertext, msg.Nonce, err = s.crypto.EncryptField("message-body", msg.ID, text); err != nil {
		return nil, fmt.Errorf("encrypt message: %w", err)
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(msg).Error; err != nil {
			return err
		}
		if len(attachmentIDs) > 0 {
			if err := tx.Model(&Attachment{}).Where("id IN ?", attachmentIDs).Update("message_id", msg.ID).Error; err != nil {
				return err
			}
		}
		return tx.Model(&Conversation{}).Where("id = ?", convID).Update("updated_at", time.Now()).Error
	}); err != nil {
		return nil, fmt.Errorf("save message: %w", err)
	}

	var attachments []Attachment
	if len(attachmentIDs) > 0 {
		if err := s.db.WithContext(ctx).Where("message_id = ?", msg.ID).Find(&attachments).Error; err != nil {
			return nil, err
		}
		for i := range attachments {
			if err := s.hydrateAttachment(&attachments[i]); err != nil {
				return nil, err
			}
		}
	}
	return &MessageOutput{
		ID: msg.ID, ConversationID: msg.ConversationID, SenderRole: msg.SenderRole,
		Text: text, CreatedAt: msg.CreatedAt, Attachments: attachments,
	}, nil
}

func (s *Store) ListMessages(ctx context.Context, convID string) ([]MessageOutput, error) {
	var msgs []Message
	if err := s.db.WithContext(ctx).Preload("Attachments").Where("conversation_id = ?", convID).Order("created_at ASC").Find(&msgs).Error; err != nil {
		return nil, err
	}
	outputs := make([]MessageOutput, len(msgs))
	for i := range msgs {
		message := &msgs[i]
		var plaintext string
		var err error
		if message.EncryptionVersion >= currentEncryptionVersion {
			plaintext, err = s.decryptField("message-body", message.ID, message.Ciphertext, message.Nonce)
		} else {
			plaintext, err = s.decryptLegacy(message.ConversationID, message.Ciphertext, message.Nonce)
		}
		if err != nil {
			return nil, fmt.Errorf("decrypt message %s: %w", message.ID, err)
		}
		for j := range message.Attachments {
			if err := s.hydrateAttachment(&message.Attachments[j]); err != nil {
				return nil, fmt.Errorf("decrypt attachment %s: %w", message.Attachments[j].ID, err)
			}
		}
		outputs[i] = MessageOutput{
			ID: message.ID, ConversationID: message.ConversationID, SenderRole: message.SenderRole,
			Text: plaintext, ReadAt: message.ReadAt, CreatedAt: message.CreatedAt, Attachments: message.Attachments,
		}
	}
	return outputs, nil
}

func (s *Store) hydrateAttachment(att *Attachment) error {
	if att.EncryptionVersion < currentEncryptionVersion {
		att.UploaderID = att.LegacyUploaderID
		att.FileName = att.LegacyFileName
		att.MimeType = att.LegacyMimeType
		att.Size = att.LegacySize
		return nil
	}
	var err error
	if att.UploaderID, err = s.decryptField("attachment-uploader-id", att.ID, att.UploaderIDCiphertext, att.UploaderIDNonce); err != nil {
		return err
	}
	if att.FileName, err = s.decryptField("attachment-file-name", att.ID, att.FileNameCiphertext, att.FileNameNonce); err != nil {
		return err
	}
	if att.MimeType, err = s.decryptField("attachment-mime-type", att.ID, att.MimeTypeCiphertext, att.MimeTypeNonce); err != nil {
		return err
	}
	sizeText, err := s.decryptField("attachment-size", att.ID, att.SizeCiphertext, att.SizeNonce)
	if err != nil {
		return err
	}
	if att.Size, err = strconv.ParseInt(sizeText, 10, 64); err != nil {
		return fmt.Errorf("parse attachment size: %w", err)
	}
	return nil
}

// AttachmentContentDigest returns a non-reversible, sender-scoped key for
// comparing equal file contents without exposing a plain SHA-256 in the DB.
func (s *Store) AttachmentContentDigest(uploaderID, contentHash string) string {
	return s.crypto.BlindIndex("attachment-content-digest:"+uploaderID, contentHash)
}

func (s *Store) FindAttachmentBlob(ctx context.Context, uploaderID, contentDigest string) (*AttachmentBlob, error) {
	var blob AttachmentBlob
	uploaderHash := s.crypto.BlindIndex("attachment-uploader-id", uploaderID)
	err := s.db.WithContext(ctx).Where("uploader_id_hash = ? AND content_digest = ?", uploaderHash, contentDigest).First(&blob).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &blob, nil
}

func (s *Store) CreateAttachmentBlob(ctx context.Context, storageKey, uploaderID, contentDigest string) (*AttachmentBlob, error) {
	blob := &AttachmentBlob{
		StorageKey:     storageKey,
		UploaderIDHash: s.crypto.BlindIndex("attachment-uploader-id", uploaderID),
		ContentDigest:  contentDigest,
		CreatedAt:      time.Now(),
	}
	if err := s.db.WithContext(ctx).Create(blob).Error; err != nil {
		return nil, err
	}
	return blob, nil
}

// CreateAttachment stores encrypted attachment metadata. storageKey identifies
// the shared encrypted blob and is deliberately independent of attachmentID.
func (s *Store) CreateAttachment(ctx context.Context, attachmentID, storageKey, uploaderID, fileName, mimeType string, size int64) (*Attachment, error) {
	att := &Attachment{
		ID: attachmentID, StorageKey: storageKey, UploaderID: uploaderID, FileName: fileName, MimeType: mimeType, Size: size,
		CreatedAt: time.Now(), EncryptionVersion: currentEncryptionVersion, KeyVersion: s.keyVersion,
	}
	var err error
	if att.UploaderIDCiphertext, att.UploaderIDNonce, err = s.crypto.EncryptField("attachment-uploader-id", att.ID, uploaderID); err != nil {
		return nil, err
	}
	att.UploaderIDHash = s.crypto.BlindIndex("attachment-uploader-id", uploaderID)
	if att.FileNameCiphertext, att.FileNameNonce, err = s.crypto.EncryptField("attachment-file-name", att.ID, fileName); err != nil {
		return nil, err
	}
	if att.MimeTypeCiphertext, att.MimeTypeNonce, err = s.crypto.EncryptField("attachment-mime-type", att.ID, mimeType); err != nil {
		return nil, err
	}
	if att.SizeCiphertext, att.SizeNonce, err = s.crypto.EncryptField("attachment-size", att.ID, strconv.FormatInt(size, 10)); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Create(att).Error; err != nil {
		return nil, err
	}
	return att, nil
}

func (s *Store) GetAttachment(ctx context.Context, id string) (*Attachment, error) {
	var att Attachment
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&att).Error; err != nil {
		return nil, err
	}
	if err := s.hydrateAttachment(&att); err != nil {
		return nil, fmt.Errorf("decrypt attachment: %w", err)
	}
	return &att, nil
}

func (s *Store) UserOwnsAttachment(ctx context.Context, userID string, att *Attachment) bool {
	if att.UploaderID == "" {
		if err := s.hydrateAttachment(att); err != nil {
			return false
		}
	}
	if att.MessageID == nil || *att.MessageID == "" {
		return att.UploaderID == userID
	}
	var msg Message
	if err := s.db.WithContext(ctx).Where("id = ?", *att.MessageID).First(&msg).Error; err != nil {
		return false
	}
	conv, err := s.GetConversation(ctx, msg.ConversationID)
	return err == nil && conv.UserID == userID
}

func (s *Store) MarkMessagesRead(ctx context.Context, convID, readerRole string) error {
	targetRole := "admin"
	if readerRole == "admin" {
		targetRole = "client"
	}
	return s.db.WithContext(ctx).Model(&Message{}).
		Where("conversation_id = ? AND sender_role = ? AND read_at IS NULL", convID, targetRole).
		Update("read_at", time.Now()).Error
}

// BanUser creates or replaces a support-only ban for the given user.
func (s *Store) BanUser(ctx context.Context, userID, reason, bannedBy string, expiresAt *time.Time) (*SupportBan, error) {
	ban := &SupportBan{
		ID:                uuid.New().String(),
		UserID:            userID,
		Reason:            reason,
		BannedBy:          bannedBy,
		ExpiresAt:         expiresAt,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
		EncryptionVersion: currentEncryptionVersion,
		KeyVersion:        s.keyVersion,
		UserIDHash:        s.crypto.BlindIndex("support-ban-user-id", userID),
	}

	var err error
	if ban.UserIDCiphertext, ban.UserIDNonce, err = s.crypto.EncryptField("support-ban-user-id", ban.ID, userID); err != nil {
		return nil, fmt.Errorf("encrypt ban user id: %w", err)
	}
	if reason != "" {
		if ban.ReasonCiphertext, ban.ReasonNonce, err = s.crypto.EncryptField("support-ban-reason", ban.ID, reason); err != nil {
			return nil, fmt.Errorf("encrypt ban reason: %w", err)
		}
	}
	if bannedBy != "" {
		if ban.BannedByCiphertext, ban.BannedByNonce, err = s.crypto.EncryptField("support-ban-banned-by", ban.ID, bannedBy); err != nil {
			return nil, fmt.Errorf("encrypt ban author: %w", err)
		}
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Clean up any existing ban for this user
		if err := tx.Where("user_id_hash = ?", ban.UserIDHash).Delete(&SupportBan{}).Error; err != nil {
			return err
		}
		return tx.Create(ban).Error
	})
	if err != nil {
		return nil, fmt.Errorf("save support ban: %w", err)
	}

	return ban, nil
}

// UnbanUser removes any support-only ban for the user.
func (s *Store) UnbanUser(ctx context.Context, userID string) error {
	userIDHash := s.crypto.BlindIndex("support-ban-user-id", userID)
	return s.db.WithContext(ctx).Where("user_id_hash = ?", userIDHash).Delete(&SupportBan{}).Error
}

func (s *Store) hydrateBan(ban *SupportBan) error {
	if len(ban.UserIDCiphertext) > 0 {
		userID, err := s.decryptField("support-ban-user-id", ban.ID, ban.UserIDCiphertext, ban.UserIDNonce)
		if err != nil {
			return fmt.Errorf("decrypt ban user id: %w", err)
		}
		ban.UserID = userID
	}
	if len(ban.ReasonCiphertext) > 0 {
		reason, err := s.decryptField("support-ban-reason", ban.ID, ban.ReasonCiphertext, ban.ReasonNonce)
		if err != nil {
			return fmt.Errorf("decrypt ban reason: %w", err)
		}
		ban.Reason = reason
	}
	if len(ban.BannedByCiphertext) > 0 {
		bannedBy, err := s.decryptField("support-ban-banned-by", ban.ID, ban.BannedByCiphertext, ban.BannedByNonce)
		if err != nil {
			return fmt.Errorf("decrypt ban author: %w", err)
		}
		ban.BannedBy = bannedBy
	}
	return nil
}

// IsUserBanned checks whether the user is actively banned from support.
func (s *Store) IsUserBanned(ctx context.Context, userID string) (bool, *SupportBan, error) {
	userIDHash := s.crypto.BlindIndex("support-ban-user-id", userID)
	var ban SupportBan
	err := s.db.WithContext(ctx).
		Where("user_id_hash = ? AND (expires_at IS NULL OR expires_at > ?)", userIDHash, time.Now()).
		First(&ban).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if err := s.hydrateBan(&ban); err != nil {
		return false, nil, err
	}
	return true, &ban, nil
}

// GetBan returns the active ban for the user if one exists.
func (s *Store) GetBan(ctx context.Context, userID string) (*SupportBan, error) {
	banned, ban, err := s.IsUserBanned(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !banned {
		return nil, nil
	}
	return ban, nil
}

// ListBans returns all active support bans.
func (s *Store) ListBans(ctx context.Context) ([]SupportBan, error) {
	var bans []SupportBan
	err := s.db.WithContext(ctx).
		Where("expires_at IS NULL OR expires_at > ?", time.Now()).
		Order("created_at DESC").
		Find(&bans).Error
	if err != nil {
		return nil, err
	}
	for i := range bans {
		if err := s.hydrateBan(&bans[i]); err != nil {
			return nil, fmt.Errorf("decrypt ban %s: %w", bans[i].ID, err)
		}
	}
	return bans, nil
}

func attachmentStoragePath(mediaRoot, storageKey string) string {
	return filepath.Join(mediaRoot, storageKey+".bin")
}

func attachmentStorageKey(att *Attachment) string {
	if att.StorageKey != "" {
		return att.StorageKey
	}
	return att.ID
}

