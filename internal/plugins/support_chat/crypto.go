package support_chat

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	attachmentMagic     = "SCF2"
	attachmentChunkSize = 64 * 1024
)

// Crypto provides the legacy and current encryption formats used by support
// chat. Legacy methods are retained only to migrate existing records.
type Crypto struct {
	masterKey []byte
}

// NewCrypto initializes crypto with a base64-encoded master key.
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

// deriveKey is the v1 derivation used by records written before encrypted
// metadata and authenticated attachment streams were introduced.
func (c *Crypto) deriveKey(conversationID string) ([]byte, error) {
	key := make([]byte, 32)
	reader := hkdf.New(sha256.New, c.masterKey, []byte(conversationID), []byte("support_chat_v1"))
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("derive legacy key: %w", err)
	}
	return key, nil
}

func (c *Crypto) deriveV2Key(purpose, recordID string) ([]byte, error) {
	key := make([]byte, 32)
	info := []byte("support_chat/v2/" + purpose)
	reader := hkdf.New(sha256.New, c.masterKey, []byte(recordID), info)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("derive v2 key: %w", err)
	}
	return key, nil
}

func fieldAAD(purpose, recordID string) []byte {
	return []byte("support_chat/v2|" + purpose + "|" + recordID)
}

// BlindIndex returns a non-reversible, keyed equality index. It is used only
// for lookups such as "all conversations of this user"; plaintext identifiers
// are never needed for those queries.
func (c *Crypto) BlindIndex(namespace, value string) string {
	mac := hmac.New(sha256.New, c.masterKey)
	_, _ = mac.Write([]byte("support_chat/blind-index/v1\x00" + namespace + "\x00" + value))
	return hex.EncodeToString(mac.Sum(nil))
}

// Encrypt and Decrypt are the v1 message format. They must not be used for
// new writes; MigrateLegacyData uses them to read existing records.
func (c *Crypto) Encrypt(conversationID string, plaintext string) (ciphertext, nonce []byte, err error) {
	key, err := c.deriveKey(conversationID)
	if err != nil {
		return nil, nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return aead.Seal(nil, nonce, []byte(plaintext), nil), nonce, nil
}

func (c *Crypto) Decrypt(conversationID string, ciphertext, nonce []byte) (string, error) {
	key, err := c.deriveKey(conversationID)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decryption failed: %w", err)
	}
	return string(plaintext), nil
}

// EncryptField encrypts a single database field and binds it to both its
// record identifier and purpose through AES-GCM additional authenticated data.
func (c *Crypto) EncryptField(purpose, recordID, plaintext string) (ciphertext, nonce []byte, err error) {
	key, err := c.deriveV2Key(purpose, recordID)
	if err != nil {
		return nil, nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return aead.Seal(nil, nonce, []byte(plaintext), fieldAAD(purpose, recordID)), nonce, nil
}

func (c *Crypto) DecryptField(purpose, recordID string, ciphertext, nonce []byte) (string, error) {
	key, err := c.deriveV2Key(purpose, recordID)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, fieldAAD(purpose, recordID))
	if err != nil {
		return "", fmt.Errorf("field decryption failed: %w", err)
	}
	return string(plaintext), nil
}

// EncryptAttachmentStream writes a chunked AES-GCM stream. Each attachment
// has a separate derived key and each chunk has authenticated sequencing data.
func (c *Crypto) EncryptAttachmentStream(attachmentID string, dst io.Writer, src io.Reader) error {
	key, err := c.deriveV2Key("attachment-content", attachmentID)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	baseNonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, baseNonce); err != nil {
		return err
	}
	if err := writeAll(dst, append([]byte(attachmentMagic), baseNonce...)); err != nil {
		return err
	}

	buf := make([]byte, attachmentChunkSize)
	var counter uint64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if err := writeAttachmentChunk(dst, aead, baseNonce, attachmentID, counter, buf[:n]); err != nil {
				return err
			}
			counter++
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	// An authenticated empty final frame makes truncation detectable.
	return writeAttachmentChunk(dst, aead, baseNonce, attachmentID, counter, nil)
}

func (c *Crypto) DecryptAttachmentStream(attachmentID string, dst io.Writer, src io.Reader) error {
	key, err := c.deriveV2Key("attachment-content", attachmentID)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	header := make([]byte, len(attachmentMagic)+aead.NonceSize())
	if _, err := io.ReadFull(src, header); err != nil {
		return fmt.Errorf("read attachment header: %w", err)
	}
	if !bytes.Equal(header[:len(attachmentMagic)], []byte(attachmentMagic)) {
		return errors.New("unsupported attachment encryption format")
	}
	baseNonce := header[len(attachmentMagic):]
	var counter uint64
	for {
		var lengthBytes [4]byte
		if _, err := io.ReadFull(src, lengthBytes[:]); err != nil {
			return fmt.Errorf("attachment stream ended before authenticated final frame: %w", err)
		}
		length := binary.BigEndian.Uint32(lengthBytes[:])
		if length < uint32(aead.Overhead()) || length > attachmentChunkSize+uint32(aead.Overhead()) {
			return errors.New("invalid attachment frame length")
		}
		sealed := make([]byte, length)
		if _, err := io.ReadFull(src, sealed); err != nil {
			return fmt.Errorf("read attachment frame: %w", err)
		}
		plaintext, err := aead.Open(nil, attachmentNonce(baseNonce, counter), sealed, attachmentAAD(attachmentID, counter))
		if err != nil {
			return fmt.Errorf("attachment authentication failed: %w", err)
		}
		counter++
		if len(plaintext) == 0 {
			var trailing [1]byte
			n, readErr := src.Read(trailing[:])
			if n != 0 || !errors.Is(readErr, io.EOF) {
				return errors.New("data found after attachment final frame")
			}
			return nil
		}
		if err := writeAll(dst, plaintext); err != nil {
			return err
		}
	}
}

func writeAttachmentChunk(dst io.Writer, aead cipher.AEAD, baseNonce []byte, attachmentID string, counter uint64, plaintext []byte) error {
	sealed := aead.Seal(nil, attachmentNonce(baseNonce, counter), plaintext, attachmentAAD(attachmentID, counter))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(sealed)))
	if err := writeAll(dst, length[:]); err != nil {
		return err
	}
	return writeAll(dst, sealed)
}

func attachmentNonce(base []byte, counter uint64) []byte {
	nonce := append([]byte(nil), base...)
	binary.BigEndian.PutUint64(nonce[len(nonce)-8:], counter)
	return nonce
}

func attachmentAAD(attachmentID string, counter uint64) []byte {
	var counterBytes [8]byte
	binary.BigEndian.PutUint64(counterBytes[:], counter)
	return append(append([]byte("support_chat/attachment/v2|"+attachmentID+"|"), counterBytes[:]...), 0)
}

func writeAll(dst io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := dst.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// EncryptStream and DecryptStream are legacy AES-CTR helpers. They remain for
// reading old attachments during migration. New files use the authenticated
// EncryptAttachmentStream format above.
func (c *Crypto) EncryptStream(conversationID string, dst io.Writer, src io.Reader) (nonce []byte, err error) {
	key, err := c.deriveKey(conversationID)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	nonce = make([]byte, block.BlockSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	stream := cipher.NewCTR(block, nonce)
	_, err = io.Copy(&cipher.StreamWriter{S: stream, W: dst}, src)
	return nonce, err
}

func (c *Crypto) DecryptStream(conversationID string, dst io.Writer, src io.Reader, nonce []byte) error {
	key, err := c.deriveKey(conversationID)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	if len(nonce) != block.BlockSize() {
		return errors.New("invalid legacy attachment nonce size")
	}
	stream := cipher.NewCTR(block, nonce)
	_, err = io.Copy(dst, &cipher.StreamReader{S: stream, R: src})
	return err
}
