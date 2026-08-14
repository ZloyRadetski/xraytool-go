package support_chat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupTestStore(t *testing.T) *Store {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("Failed to open db: %v", err)
	}

	if err := db.AutoMigrate(&Conversation{}, &Message{}, &Attachment{}, &AttachmentBlob{}, &SupportBan{}); err != nil {
		t.Fatalf("Failed to migrate: %v", err)
	}

	masterKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	crypto, _ := NewCrypto(masterKey)

	return NewStore(db, crypto)
}

func TestStore_EncryptsConversationAndAttachmentMetadata(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "telegram:12345", "Billing question for order 42")
	if err != nil {
		t.Fatal(err)
	}
	att, err := s.CreateAttachment(ctx, "attachment-1", "attachment-1", "telegram:12345", "passport.png", "image/png", 321)
	if err != nil {
		t.Fatal(err)
	}

	var rawConv Conversation
	if err := s.db.Where("id = ?", conv.ID).First(&rawConv).Error; err != nil {
		t.Fatal(err)
	}
	if rawConv.LegacyUserID != "" || rawConv.LegacySubject != "" {
		t.Fatal("conversation plaintext metadata was written")
	}
	if bytes.Contains(rawConv.UserIDCiphertext, []byte("telegram:12345")) || bytes.Contains(rawConv.SubjectCiphertext, []byte("Billing question")) {
		t.Fatal("conversation ciphertext contains plaintext")
	}

	var rawAtt Attachment
	if err := s.db.Where("id = ?", att.ID).First(&rawAtt).Error; err != nil {
		t.Fatal(err)
	}
	if rawAtt.LegacyUploaderID != "" || rawAtt.LegacyFileName != "" || rawAtt.LegacyMimeType != "" || rawAtt.LegacySize != 0 {
		t.Fatal("attachment plaintext metadata was written")
	}
	if bytes.Contains(rawAtt.FileNameCiphertext, []byte("passport.png")) {
		t.Fatal("attachment file name was not encrypted")
	}
}

func TestStore_DeduplicatesAttachmentBlobPerUploader(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	const contentHash = "c1fbfbcd1c4ca0cf15d2c2e7438a2f5f5069fdef939eaca7b4790f418bb75472"
	firstDigest := s.AttachmentContentDigest("telegram:12345", contentHash)
	secondDigest := s.AttachmentContentDigest("telegram:67890", contentHash)
	if firstDigest == secondDigest {
		t.Fatal("content digest must be scoped to the uploader")
	}

	firstBlob, err := s.CreateAttachmentBlob(ctx, "blob-1", "telegram:12345", firstDigest)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.FindAttachmentBlob(ctx, "telegram:12345", firstDigest)
	if err != nil || duplicate == nil || duplicate.StorageKey != firstBlob.StorageKey {
		t.Fatalf("expected existing blob, got %#v, %v", duplicate, err)
	}
	if _, err := s.CreateAttachmentBlob(ctx, "blob-2", "telegram:12345", firstDigest); err == nil {
		t.Fatal("same uploader and file must have exactly one blob")
	}
	if _, err := s.CreateAttachmentBlob(ctx, "blob-2", "telegram:67890", secondDigest); err != nil {
		t.Fatalf("another uploader must be able to store the same file independently: %v", err)
	}

	first, err := s.CreateAttachment(ctx, "attachment-1", firstBlob.StorageKey, "telegram:12345", "first.png", "image/png", 10)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateAttachment(ctx, "attachment-2", firstBlob.StorageKey, "telegram:12345", "second.png", "image/png", 10)
	if err != nil {
		t.Fatal(err)
	}
	if first.StorageKey != second.StorageKey {
		t.Fatalf("duplicate attachments reference different blobs: %q and %q", first.StorageKey, second.StorageKey)
	}
}

func TestUploadAttachment_ReusesEncryptedBlobForSameUploader(t *testing.T) {
	mediaRoot := t.TempDir()
	p := &Plugin{
		store: setupTestStore(t),
		cfg: pluginConfig{Media: MediaConfig{
			StoragePath:   mediaRoot,
			MaxFileSizeMB: 1,
		}},
	}
	upload := p.handleUploadAttachment()
	content := []byte("same image bytes")

	uploadFile := func(fileName string) string {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodPost, "/support/attachments", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.Header.Set("X-User-ID", "telegram:12345")
		rec := httptest.NewRecorder()
		upload(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var response struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.ID == "" {
			t.Fatal("upload response has no attachment ID")
		}
		return response.ID
	}

	firstID := uploadFile("first.png")
	secondID := uploadFile("second.png")
	first, err := p.store.GetAttachment(context.Background(), firstID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.store.GetAttachment(context.Background(), secondID)
	if err != nil {
		t.Fatal(err)
	}
	if first.StorageKey == "" || first.StorageKey != second.StorageKey {
		t.Fatalf("identical uploads use different storage keys: %q and %q", first.StorageKey, second.StorageKey)
	}
	entries, err := os.ReadDir(mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != first.StorageKey+".bin" {
		t.Fatalf("expected exactly one encrypted binary, got %#v", entries)
	}

	download := p.handleDownloadAttachment()
	for _, id := range []string{firstID, secondID} {
		req := httptest.NewRequest(http.MethodGet, "/support/attachments/"+id, nil)
		req.Header.Set("X-User-ID", "telegram:12345")
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		download(rec, req)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), content) {
			t.Fatalf("download %s returned status %d and body %q", id, rec.Code, rec.Body.Bytes())
		}
	}
}

func TestStore_MigratesLegacyConversationAndMessage(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	legacyConv := &Conversation{ID: "legacy-conversation", LegacyUserID: "legacy-user", LegacySubject: "legacy subject", Status: "open"}
	if err := s.db.Create(legacyConv).Error; err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := s.Crypto().Encrypt(legacyConv.ID, "legacy message")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&Message{ID: "legacy-message", ConversationID: legacyConv.ID, SenderRole: "client", Ciphertext: ciphertext, Nonce: nonce}).Error; err != nil {
		t.Fatal(err)
	}
	stats, err := s.MigrateLegacyData(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Conversations != 1 || stats.Messages != 1 || stats.Attachments != 0 {
		t.Fatalf("unexpected migration stats: %#v", stats)
	}
	conv, err := s.GetConversation(ctx, legacyConv.ID)
	if err != nil || conv.UserID != "legacy-user" || conv.Subject != "legacy subject" {
		t.Fatalf("legacy conversation did not survive migration: %#v, %v", conv, err)
	}
	messages, err := s.ListMessages(ctx, legacyConv.ID)
	if err != nil || len(messages) != 1 || messages[0].Text != "legacy message" {
		t.Fatalf("legacy message did not survive migration: %#v, %v", messages, err)
	}
	var rawConv Conversation
	if err := s.db.Where("id = ?", legacyConv.ID).First(&rawConv).Error; err != nil {
		t.Fatal(err)
	}
	if rawConv.LegacyUserID != "" || rawConv.LegacySubject != "" || rawConv.EncryptionVersion != currentEncryptionVersion {
		t.Fatal("legacy plaintext conversation values were not cleared")
	}
}

func TestStore_MigratesLegacyAttachmentToAuthenticatedFormat(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	mediaRoot := t.TempDir()
	oldPath := filepath.Join(mediaRoot, "legacy.bin")
	oldFile, err := os.OpenFile(oldPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("legacy attachment contents")
	nonce, err := s.Crypto().EncryptStream("global_attachments", oldFile, bytes.NewReader(content))
	if closeErr := oldFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	legacy := &Attachment{
		ID: "legacy-attachment", LegacyUploaderID: "legacy-user", LegacyFileName: "secret.png",
		LegacyMimeType: "image/png", LegacySize: int64(len(content)), LegacyFileHash: "plain-hash",
		LegacyStoragePath: oldPath, Nonce: nonce,
	}
	if err := s.db.Create(legacy).Error; err != nil {
		t.Fatal(err)
	}
	stats, err := s.MigrateLegacyData(ctx, mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Attachments != 1 {
		t.Fatalf("expected one migrated attachment, got %#v", stats)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("legacy attachment file was not removed")
	}
	att, err := s.GetAttachment(ctx, legacy.ID)
	if err != nil || att.FileName != "secret.png" || att.UploaderID != "legacy-user" {
		t.Fatalf("attachment metadata did not survive migration: %#v, %v", att, err)
	}
	newFile, err := os.Open(attachmentStoragePath(mediaRoot, legacy.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer newFile.Close()
	var decrypted bytes.Buffer
	if err := s.Crypto().DecryptAttachmentStream(legacy.ID, &decrypted, newFile); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted.Bytes(), content) {
		t.Fatal("migrated attachment content changed")
	}
	var raw Attachment
	if err := s.db.Where("id = ?", legacy.ID).First(&raw).Error; err != nil {
		t.Fatal(err)
	}
	if raw.LegacyFileName != "" || raw.LegacyStoragePath != "" || raw.LegacyFileHash != "" {
		t.Fatal("legacy attachment plaintext was not cleared")
	}
}

func TestStore_CreateAndListConversations(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	userID := "user-1"
	conv, err := s.CreateConversation(ctx, userID, "Test subject")
	if err != nil {
		t.Fatalf("Failed to create conversation: %v", err)
	}

	if conv.ID == "" || conv.UserID != userID || conv.Subject != "Test subject" {
		t.Errorf("Unexpected conversation values: %+v", conv)
	}

	filter := ConversationFilter{UserID: &userID}
	list, err := s.ListConversations(ctx, filter)
	if err != nil {
		t.Fatalf("Failed to list: %v", err)
	}
	if len(list) != 1 || list[0].ID != conv.ID {
		t.Errorf("Expected 1 conversation, got %d", len(list))
	}
}

func TestStore_Messages(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	conv, _ := s.CreateConversation(ctx, "user-1", "Subject")

	_, err := s.CreateMessage(ctx, conv.ID, "client", "Hello admin", nil)
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}

	_, err = s.CreateMessage(ctx, conv.ID, "admin", "Hello client", nil)
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}

	msgs, err := s.ListMessages(ctx, conv.ID)
	if err != nil {
		t.Fatalf("ListMessages failed: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("Expected 2 messages, got %d", len(msgs))
	}

	if msgs[0].Text != "Hello admin" || msgs[1].Text != "Hello client" {
		t.Errorf("Unexpected decrypted texts: %s, %s", msgs[0].Text, msgs[1].Text)
	}

	// Verify encryption in DB
	var rawMsgs []Message
	s.db.Find(&rawMsgs)
	if len(rawMsgs) != 2 {
		t.Fatalf("Expected 2 raw messages, got %d", len(rawMsgs))
	}
	if string(rawMsgs[0].Ciphertext) == "Hello admin" {
		t.Fatal("Message was not encrypted in the database!")
	}

	// Test read tracking
	if err := s.MarkMessagesRead(ctx, conv.ID, "client"); err != nil {
		t.Fatalf("MarkMessagesRead failed: %v", err)
	}

	msgsAfterRead, _ := s.ListMessages(ctx, conv.ID)
	if msgsAfterRead[0].ReadAt != nil {
		t.Errorf("Client message should not be marked read by client")
	}
	if msgsAfterRead[1].ReadAt == nil {
		t.Errorf("Admin message should be marked read by client")
	}
}
