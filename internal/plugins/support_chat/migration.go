package support_chat

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const migrationBatchSize = 100

// MigrationStats reports how many legacy records were converted during one
// startup. The migration is idempotent: completed rows have version 2 and are
// never processed again.
type MigrationStats struct {
	Conversations int
	Messages      int
	Attachments   int
}

// MigrateLegacyData removes plaintext metadata from records written by older
// versions and converts legacy AES-GCM/AES-CTR payloads to the v2 formats.
func (s *Store) MigrateLegacyData(ctx context.Context, mediaRoot string) (MigrationStats, error) {
	var stats MigrationStats
	for {
		var conversations []Conversation
		if err := s.db.WithContext(ctx).
			Where("encryption_version < ? AND (user_id <> '' OR subject <> '')", currentEncryptionVersion).
			Limit(migrationBatchSize).Find(&conversations).Error; err != nil {
			return stats, fmt.Errorf("load legacy conversations: %w", err)
		}
		if len(conversations) == 0 {
			break
		}
		for i := range conversations {
			if err := s.migrateConversation(ctx, &conversations[i]); err != nil {
				return stats, err
			}
			stats.Conversations++
		}
	}

	for {
		var messages []Message
		if err := s.db.WithContext(ctx).Where("encryption_version < ?", currentEncryptionVersion).
			Limit(migrationBatchSize).Find(&messages).Error; err != nil {
			return stats, fmt.Errorf("load legacy messages: %w", err)
		}
		if len(messages) == 0 {
			break
		}
		for i := range messages {
			if err := s.migrateMessage(ctx, &messages[i]); err != nil {
				return stats, err
			}
			stats.Messages++
		}
	}

	for {
		var attachments []Attachment
		if err := s.db.WithContext(ctx).Where("encryption_version < ?", currentEncryptionVersion).
			Limit(migrationBatchSize).Find(&attachments).Error; err != nil {
			return stats, fmt.Errorf("load legacy attachments: %w", err)
		}
		if len(attachments) == 0 {
			break
		}
		for i := range attachments {
			if err := s.migrateAttachment(ctx, &attachments[i], mediaRoot); err != nil {
				return stats, err
			}
			stats.Attachments++
		}
	}
	return stats, nil
}

func (s *Store) migrateConversation(ctx context.Context, conv *Conversation) error {
	userCiphertext, userNonce, err := s.crypto.EncryptField("conversation-user-id", conv.ID, conv.LegacyUserID)
	if err != nil {
		return fmt.Errorf("encrypt legacy conversation user %s: %w", conv.ID, err)
	}
	subjectCiphertext, subjectNonce, err := s.crypto.EncryptField("conversation-subject", conv.ID, conv.LegacySubject)
	if err != nil {
		return fmt.Errorf("encrypt legacy conversation subject %s: %w", conv.ID, err)
	}
	updates := map[string]any{
		"user_id_ciphertext": userCiphertext,
		"user_id_nonce":      userNonce,
		"user_id_hash":       s.crypto.BlindIndex("conversation-user-id", conv.LegacyUserID),
		"subject_ciphertext": subjectCiphertext,
		"subject_nonce":      subjectNonce,
		"encryption_version": currentEncryptionVersion,
		"key_version":        s.keyVersion,
		"user_id":            "",
		"subject":            "",
	}
	if err := s.db.WithContext(ctx).Model(&Conversation{}).Where("id = ?", conv.ID).Updates(updates).Error; err != nil {
		return fmt.Errorf("migrate conversation %s: %w", conv.ID, err)
	}
	return nil
}

func (s *Store) migrateMessage(ctx context.Context, message *Message) error {
	plaintext, err := s.decryptLegacy(message.ConversationID, message.Ciphertext, message.Nonce)
	if err != nil {
		return fmt.Errorf("decrypt legacy message %s: %w", message.ID, err)
	}
	ciphertext, nonce, err := s.crypto.EncryptField("message-body", message.ID, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt message %s: %w", message.ID, err)
	}
	updates := map[string]any{
		"ciphertext":         ciphertext,
		"nonce":              nonce,
		"encryption_version": currentEncryptionVersion,
		"key_version":        s.keyVersion,
	}
	if err := s.db.WithContext(ctx).Model(&Message{}).Where("id = ?", message.ID).Updates(updates).Error; err != nil {
		return fmt.Errorf("migrate message %s: %w", message.ID, err)
	}
	return nil
}

func (s *Store) migrateAttachment(ctx context.Context, att *Attachment, mediaRoot string) error {
	if att.LegacyStoragePath == "" {
		return fmt.Errorf("legacy attachment %s has no storage path", att.ID)
	}
	if err := migrateAttachmentFile(s, att, mediaRoot); err != nil {
		return err
	}

	uploaderCiphertext, uploaderNonce, err := s.crypto.EncryptField("attachment-uploader-id", att.ID, att.LegacyUploaderID)
	if err != nil {
		return err
	}
	fileNameCiphertext, fileNameNonce, err := s.crypto.EncryptField("attachment-file-name", att.ID, att.LegacyFileName)
	if err != nil {
		return err
	}
	mimeTypeCiphertext, mimeTypeNonce, err := s.crypto.EncryptField("attachment-mime-type", att.ID, att.LegacyMimeType)
	if err != nil {
		return err
	}
	sizeCiphertext, sizeNonce, err := s.crypto.EncryptField("attachment-size", att.ID, fmt.Sprintf("%d", att.LegacySize))
	if err != nil {
		return err
	}
	updates := map[string]any{
		"uploader_id_ciphertext": uploaderCiphertext,
		"uploader_id_nonce":      uploaderNonce,
		"uploader_id_hash":       s.crypto.BlindIndex("attachment-uploader-id", att.LegacyUploaderID),
		"file_name_ciphertext":   fileNameCiphertext,
		"file_name_nonce":        fileNameNonce,
		"mime_type_ciphertext":   mimeTypeCiphertext,
		"mime_type_nonce":        mimeTypeNonce,
		"size_ciphertext":        sizeCiphertext,
		"size_nonce":             sizeNonce,
		"encryption_version":     currentEncryptionVersion,
		"key_version":            s.keyVersion,
		"uploader_id":            "",
		"file_name":              "",
		"mime_type":              "",
		"size":                   0,
		"file_hash":              "",
		"storage_path":           "",
		"storage_key":            att.ID,
		"nonce":                  []byte{},
	}
	if err := s.db.WithContext(ctx).Model(&Attachment{}).Where("id = ?", att.ID).Updates(updates).Error; err != nil {
		return fmt.Errorf("migrate attachment %s: %w", att.ID, err)
	}
	if oldPath := filepath.Clean(att.LegacyStoragePath); oldPath != filepath.Clean(attachmentStoragePath(mediaRoot, att.ID)) {
		if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove legacy attachment %s: %w", att.ID, err)
		}
	}
	return nil
}

func migrateAttachmentFile(s *Store, att *Attachment, mediaRoot string) error {
	if err := os.MkdirAll(mediaRoot, 0700); err != nil {
		return fmt.Errorf("create protected media directory: %w", err)
	}
	if err := os.Chmod(mediaRoot, 0700); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("protect media directory: %w", err)
	}

	in, err := os.Open(att.LegacyStoragePath)
	if err != nil {
		return fmt.Errorf("open legacy attachment %s: %w", att.ID, err)
	}
	defer in.Close()

	temp, err := os.CreateTemp(mediaRoot, "."+att.ID+".*.migrating")
	if err != nil {
		return fmt.Errorf("create attachment migration temp file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return fmt.Errorf("protect attachment migration temp file: %w", err)
	}

	plainReader, plainWriter := io.Pipe()
	encryptResult := make(chan error, 1)
	go func() {
		encryptResult <- s.crypto.EncryptAttachmentStream(att.ID, temp, plainReader)
		_ = plainReader.Close()
	}()
	decryptErr := s.legacyAttachmentCrypto().DecryptStream("global_attachments", plainWriter, in, att.Nonce)
	_ = plainWriter.CloseWithError(decryptErr)
	if encryptErr := <-encryptResult; decryptErr != nil || encryptErr != nil {
		temp.Close()
		if decryptErr != nil {
			return fmt.Errorf("decrypt legacy attachment %s: %w", att.ID, decryptErr)
		}
		return fmt.Errorf("encrypt migrated attachment %s: %w", att.ID, encryptErr)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close attachment migration temp file: %w", err)
	}

	newPath := attachmentStoragePath(mediaRoot, att.ID)
	if err := os.Rename(tempPath, newPath); err != nil {
		return fmt.Errorf("publish encrypted attachment %s: %w", att.ID, err)
	}
	return nil
}

func (s *Store) legacyAttachmentCrypto() *Crypto {
	if len(s.legacyCryptos) > 0 && s.legacyCryptos[0] != nil {
		return s.legacyCryptos[0]
	}
	return s.crypto
}
