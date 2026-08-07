package support_chat

import (
	"encoding/base64"
	"testing"
)

func TestCrypto(t *testing.T) {
	// 32-byte master key
	masterKey := "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0u1v=" // dummy b64 (length 44)
	crypto, err := NewCrypto(masterKey)
	if err != nil {
		t.Fatalf("Failed to init crypto: %v", err)
	}

	convID := "conv-123"
	plaintext := "Hello, this is a secret message!"

	ciphertext, nonce, err := crypto.Encrypt(convID, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if len(ciphertext) == 0 || len(nonce) == 0 {
		t.Fatal("Ciphertext or nonce is empty")
	}

	decrypted, err := crypto.Decrypt(convID, ciphertext, nonce)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if decrypted != plaintext {
		t.Errorf("Expected %q, got %q", plaintext, decrypted)
	}
}

func TestCrypto_WrongConversation(t *testing.T) {
	masterKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	crypto, _ := NewCrypto(masterKey)

	convID := "conv-123"
	plaintext := "Secret"

	ciphertext, nonce, _ := crypto.Encrypt(convID, plaintext)

	// Try to decrypt with wrong conversation ID
	_, err := crypto.Decrypt("conv-456", ciphertext, nonce)
	if err == nil {
		t.Fatal("Expected error when decrypting with wrong conversation ID, got nil")
	}
}
