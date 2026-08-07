package support_chat

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Crypto provides per-conversation encryption using a master key.
type Crypto struct {
	masterKey []byte
}

// NewCrypto initializes the crypto service with a base64-encoded master key.
func NewCrypto(masterKeyB64 string) (*Crypto, error) {
	key, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode master key: %w", err)
	}
	if len(key) < 32 {
		return nil, errors.New("master key must be at least 32 bytes")
	}
	return &Crypto{masterKey: key}, nil
}

// deriveKey generates a 32-byte AES-256 key for a specific conversation using HKDF.
func (c *Crypto) deriveKey(conversationID string) ([]byte, error) {
	hash := sha256.New
	info := []byte("support_chat_v1")
	salt := []byte(conversationID) // Using conversationID as salt makes keys unique per chat

	hkdfReader := hkdf.New(hash, c.masterKey, salt, info)
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdfReader, key); err != nil {
		return nil, fmt.Errorf("hkdf derivation failed: %w", err)
	}
	return key, nil
}

// Encrypt encrypts a plaintext message for a given conversation.
// Returns the ciphertext and the nonce used.
func (c *Crypto) Encrypt(conversationID string, plaintext string) (ciphertext, nonce []byte, err error) {
	key, err := c.deriveKey(conversationID)
	if err != nil {
		return nil, nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}

	nonce = make([]byte, aesGCM.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext = aesGCM.Seal(nil, nonce, []byte(plaintext), nil)
	return ciphertext, nonce, nil
}

// Decrypt decrypts a ciphertext message using the conversation's derived key.
func (c *Crypto) Decrypt(conversationID string, ciphertext, nonce []byte) (string, error) {
	key, err := c.deriveKey(conversationID)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decryption failed (invalid key, tampered ciphertext, or wrong conversation): %w", err)
	}

	return string(plaintext), nil
}
