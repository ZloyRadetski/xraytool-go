package support_chat

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"xraytool/internal/pluginapi"
)

func TestStore_DeleteConversationWithMessagesAndAttachments(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	mediaRoot := t.TempDir()
	const (
		userID   = "telegram:123"
		storage  = "storage-key-1"
		attachID = "attachment-1"
		content  = "confidential test file"
	)

	// Create conversation, attachment, message
	conv, err := s.CreateConversation(ctx, userID, "Delete test")
	if err != nil {
		t.Fatal(err)
	}

	contentDigest := s.AttachmentContentDigest(userID, "somehash")
	if _, err := s.CreateAttachmentBlob(ctx, storage, userID, contentDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAttachment(ctx, attachID, storage, userID, "test.txt", "text/plain", int64(len(content))); err != nil {
		t.Fatal(err)
	}

	// Write encrypted file on disk
	filePath := attachmentStoragePath(mediaRoot, storage)
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Crypto().EncryptAttachmentStream(storage, f, bytes.NewBufferString(content)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	if _, err := s.CreateMessage(ctx, conv.ID, "client", "hello", []string{attachID}); err != nil {
		t.Fatal(err)
	}

	// Verify file exists
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("expected file on disk: %v", err)
	}

	// Delete conversation
	if err := s.DeleteConversation(ctx, conv.ID, mediaRoot); err != nil {
		t.Fatalf("failed to delete conversation: %v", err)
	}

	// Verify conversation is gone
	if _, err := s.GetConversation(ctx, conv.ID); err == nil {
		t.Fatal("expected conversation to be deleted")
	}

	// Verify messages are gone
	msgs, err := s.ListMessages(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}

	// Verify attachment is gone
	if _, err := s.GetAttachment(ctx, attachID); err == nil {
		t.Fatal("expected attachment to be deleted")
	}

	// Verify blob is gone
	blob, err := s.FindAttachmentBlob(ctx, userID, contentDigest)
	if err != nil || blob != nil {
		t.Fatalf("expected blob to be deleted: %v, %+v", err, blob)
	}

	// Verify file is removed from disk
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatal("expected attachment file on disk to be removed")
	}
}

func TestStore_DeleteConversationPreservesSharedAttachmentBlobs(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	mediaRoot := t.TempDir()
	const (
		userID       = "telegram:123"
		storage      = "shared-storage-key"
		firstAttach  = "attachment-1"
		secondAttach = "attachment-2"
		content      = "shared file content"
	)

	// Create 2 conversations
	conv1, err := s.CreateConversation(ctx, userID, "Conv 1")
	if err != nil {
		t.Fatal(err)
	}
	conv2, err := s.CreateConversation(ctx, userID, "Conv 2")
	if err != nil {
		t.Fatal(err)
	}

	// Same blob shared by two attachments
	contentDigest := s.AttachmentContentDigest(userID, "sharedhash")
	if _, err := s.CreateAttachmentBlob(ctx, storage, userID, contentDigest); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateAttachment(ctx, firstAttach, storage, userID, "f1.txt", "text/plain", int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAttachment(ctx, secondAttach, storage, userID, "f2.txt", "text/plain", int64(len(content))); err != nil {
		t.Fatal(err)
	}

	// Write encrypted file on disk
	filePath := attachmentStoragePath(mediaRoot, storage)
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Crypto().EncryptAttachmentStream(storage, f, bytes.NewBufferString(content)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	if _, err := s.CreateMessage(ctx, conv1.ID, "client", "msg1", []string{firstAttach}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMessage(ctx, conv2.ID, "client", "msg2", []string{secondAttach}); err != nil {
		t.Fatal(err)
	}

	// Delete conv1
	if err := s.DeleteConversation(ctx, conv1.ID, mediaRoot); err != nil {
		t.Fatal(err)
	}

	// conv1 and firstAttach are deleted
	if _, err := s.GetConversation(ctx, conv1.ID); err == nil {
		t.Fatal("expected conv1 to be deleted")
	}
	if _, err := s.GetAttachment(ctx, firstAttach); err == nil {
		t.Fatal("expected firstAttach to be deleted")
	}

	// conv2, secondAttach, blob, and disk file MUST still exist
	if _, err := s.GetConversation(ctx, conv2.ID); err != nil {
		t.Fatalf("expected conv2 to still exist: %v", err)
	}
	if _, err := s.GetAttachment(ctx, secondAttach); err != nil {
		t.Fatalf("expected secondAttach to still exist: %v", err)
	}
	blob, err := s.FindAttachmentBlob(ctx, userID, contentDigest)
	if err != nil || blob == nil {
		t.Fatalf("expected shared blob to still exist: %v", err)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("expected physical file to still exist: %v", err)
	}
}

func TestHTTP_ClientDeleteConversation(t *testing.T) {
	store := setupTestStore(t)
	p := &Plugin{
		store: store,
		provider: &Provider{db: store.db, crypto: store.crypto},
		cfg: pluginConfig{
			Media: MediaConfig{StoragePath: t.TempDir(), MaxFileSizeMB: 10},
		},
		users: supportUserLookupStub{byEmail: map[string]*pluginapi.User{
			"owner@test.local":    {ID: "owner-1", IsAdmin: false},
			"stranger@test.local": {ID: "stranger-1", IsAdmin: false},
			"admin@test.local":    {ID: "admin-1", IsAdmin: true},
		}},
		authMiddleware: testProtectedMiddleware,
		hub:            newHub(nil),
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	conv, err := store.CreateConversation(context.Background(), "owner@test.local", "My Ticket")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Stranger tries to delete owner's conversation -> 403 Forbidden
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/support/conversations/"+conv.ID, nil)
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", "stranger@test.local")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("stranger delete status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	// 2. Owner deletes their own conversation -> 200 OK
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/support/conversations/"+conv.ID, nil)
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", "owner@test.local")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner delete status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// 3. Conversation is gone
	if _, err := store.GetConversation(context.Background(), conv.ID); err == nil {
		t.Fatal("expected conversation to be deleted")
	}
}

func TestHTTP_AdminDeleteConversation(t *testing.T) {
	store := setupTestStore(t)
	p := &Plugin{
		store: store,
		provider: &Provider{db: store.db, crypto: store.crypto},
		cfg: pluginConfig{
			Media: MediaConfig{StoragePath: t.TempDir(), MaxFileSizeMB: 10},
		},
		users: supportUserLookupStub{byEmail: map[string]*pluginapi.User{
			"client@test.local": {ID: "client-1", IsAdmin: false},
			"admin@test.local":  {ID: "admin-1", IsAdmin: true},
		}},
		authMiddleware: testProtectedMiddleware,
		hub:            newHub(nil),
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	conv, err := store.CreateConversation(context.Background(), "client@test.local", "Client Ticket")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Non-admin calls admin delete route -> 403 Forbidden
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/support/conversations/"+conv.ID, nil)
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", "client@test.local")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin delete status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	// 2. Admin deletes conversation -> 200 OK
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/admin/support/conversations/"+conv.ID, nil)
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", "admin@test.local")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin delete status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// 3. Conversation is gone
	if _, err := store.GetConversation(context.Background(), conv.ID); err == nil {
		t.Fatal("expected conversation to be deleted")
	}
}
