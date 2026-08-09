package support_chat

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupTestStore(t *testing.T) *Store {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("Failed to open db: %v", err)
	}

	if err := db.AutoMigrate(&Conversation{}, &Message{}, &Attachment{}); err != nil {
		t.Fatalf("Failed to migrate: %v", err)
	}

	masterKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	crypto, _ := NewCrypto(masterKey)

	return NewStore(db, crypto)
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
