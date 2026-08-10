package support_chat

import (
	"bytes"
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

func TestCrypto_FieldEncryptionBindsRecordAndPurpose(t *testing.T) {
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	crypto, err := NewCrypto(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := crypto.EncryptField("conversation-subject", "conversation-1", "Sensitive subject")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("Sensitive subject")) {
		t.Fatal("field ciphertext contains plaintext")
	}
	if _, err := crypto.DecryptField("conversation-subject", "conversation-2", ciphertext, nonce); err == nil {
		t.Fatal("ciphertext must not decrypt under another record ID")
	}
	if _, err := crypto.DecryptField("attachment-file-name", "conversation-1", ciphertext, nonce); err == nil {
		t.Fatal("ciphertext must not decrypt under another purpose")
	}
	plaintext, err := crypto.DecryptField("conversation-subject", "conversation-1", ciphertext, nonce)
	if err != nil || plaintext != "Sensitive subject" {
		t.Fatalf("unexpected decrypted field: %q, %v", plaintext, err)
	}
}

func TestCrypto_AttachmentStreamIsAuthenticated(t *testing.T) {
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	crypto, err := NewCrypto(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := bytes.Repeat([]byte("attachment-data-"), 10_000)
	var encrypted bytes.Buffer
	if err := crypto.EncryptAttachmentStream("attachment-1", &encrypted, bytes.NewReader(plaintext)); err != nil {
		t.Fatal(err)
	}
	var decrypted bytes.Buffer
	if err := crypto.DecryptAttachmentStream("attachment-1", &decrypted, bytes.NewReader(encrypted.Bytes())); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted.Bytes(), plaintext) {
		t.Fatal("attachment stream round trip changed data")
	}

	tampered := append([]byte(nil), encrypted.Bytes()...)
	tampered[len(tampered)-1] ^= 0x01
	if err := crypto.DecryptAttachmentStream("attachment-1", &bytes.Buffer{}, bytes.NewReader(tampered)); err == nil {
		t.Fatal("tampered attachment must fail authentication")
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
