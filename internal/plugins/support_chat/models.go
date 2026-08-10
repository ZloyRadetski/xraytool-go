package support_chat

import "time"

const currentEncryptionVersion uint16 = 2

// Conversation contains operational metadata plus encrypted user-facing data.
// The legacy columns stay mapped only to read and clear records created before
// encrypted metadata was introduced; new records write empty placeholders to
// them so existing NOT NULL database constraints remain compatible.
type Conversation struct {
	ID        string     `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID    string     `gorm:"-" json:"user_id"`
	Subject   string     `gorm:"-" json:"subject"`
	Status    string     `gorm:"index;type:varchar(20);not null;default:'open'" json:"status"`
	CreatedAt time.Time  `gorm:"autoCreateTime;not null" json:"created_at"`
	UpdatedAt time.Time  `gorm:"autoUpdateTime;not null" json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`

	UserIDCiphertext  []byte `gorm:"type:blob" json:"-"`
	UserIDNonce       []byte `gorm:"type:blob" json:"-"`
	UserIDHash        string `gorm:"index;type:varchar(64)" json:"-"`
	SubjectCiphertext []byte `gorm:"type:blob" json:"-"`
	SubjectNonce      []byte `gorm:"type:blob" json:"-"`
	EncryptionVersion uint16 `gorm:"not null;default:0" json:"-"`
	KeyVersion        uint16 `gorm:"not null;default:0" json:"-"`

	LegacyUserID  string `gorm:"column:user_id;type:varchar(36);not null;default:''" json:"-"`
	LegacySubject string `gorm:"column:subject;type:text" json:"-"`
}

// Message stores an encrypted message body. Sender role and delivery metadata
// remain queryable to preserve unread counters and chronological rendering.
type Message struct {
	ID                string       `gorm:"primaryKey;type:varchar(36)"`
	ConversationID    string       `gorm:"index;type:varchar(36);not null"`
	SenderRole        string       `gorm:"type:varchar(20);not null"`
	Ciphertext        []byte       `gorm:"type:blob;not null"`
	Nonce             []byte       `gorm:"type:blob;not null"`
	EncryptionVersion uint16       `gorm:"not null;default:0"`
	KeyVersion        uint16       `gorm:"not null;default:0"`
	ReadAt            *time.Time   `gorm:"index"`
	CreatedAt         time.Time    `gorm:"autoCreateTime;not null"`
	Attachments       []Attachment `gorm:"foreignKey:MessageID"`
}

// Attachment stores encrypted metadata and a reference to an authenticated
// encrypted file. File locations are derived from StorageKey rather than kept
// as plaintext paths.
type Attachment struct {
	ID         string    `gorm:"primaryKey;type:varchar(36)" json:"id"`
	MessageID  *string   `gorm:"index;type:varchar(36)" json:"message_id,omitempty"`
	CreatedAt  time.Time `gorm:"autoCreateTime;not null" json:"created_at"`
	Nonce      []byte    `gorm:"type:blob" json:"-"` // legacy AES-CTR attachment nonce only
	StorageKey string    `gorm:"index;type:varchar(36)" json:"-"`

	UploaderID string `gorm:"-" json:"-"`
	FileName   string `gorm:"-" json:"file_name"`
	MimeType   string `gorm:"-" json:"mime_type"`
	Size       int64  `gorm:"-" json:"size"`

	UploaderIDCiphertext []byte `gorm:"type:blob" json:"-"`
	UploaderIDNonce      []byte `gorm:"type:blob" json:"-"`
	UploaderIDHash       string `gorm:"index;type:varchar(64)" json:"-"`
	FileNameCiphertext   []byte `gorm:"type:blob" json:"-"`
	FileNameNonce        []byte `gorm:"type:blob" json:"-"`
	MimeTypeCiphertext   []byte `gorm:"type:blob" json:"-"`
	MimeTypeNonce        []byte `gorm:"type:blob" json:"-"`
	SizeCiphertext       []byte `gorm:"type:blob" json:"-"`
	SizeNonce            []byte `gorm:"type:blob" json:"-"`
	EncryptionVersion    uint16 `gorm:"not null;default:0" json:"-"`
	KeyVersion           uint16 `gorm:"not null;default:0" json:"-"`

	LegacyUploaderID  string `gorm:"column:uploader_id;type:varchar(128);not null;default:''" json:"-"`
	LegacyFileName    string `gorm:"column:file_name;type:text;not null;default:''" json:"-"`
	LegacyMimeType    string `gorm:"column:mime_type;type:varchar(128);not null;default:''" json:"-"`
	LegacySize        int64  `gorm:"column:size;not null;default:0" json:"-"`
	LegacyFileHash    string `gorm:"column:file_hash;type:varchar(64)" json:"-"`
	LegacyStoragePath string `gorm:"column:storage_path;type:text;not null;default:''" json:"-"`
}

// AttachmentBlob owns one encrypted file. Its digest is a keyed HMAC scoped to
// the uploader, so the database cannot correlate equal files across users.
// Several Attachment records may reference the same blob.
type AttachmentBlob struct {
	StorageKey     string    `gorm:"primaryKey;type:varchar(36)"`
	UploaderIDHash string    `gorm:"uniqueIndex:idx_attachment_blob_digest;type:varchar(64)"`
	ContentDigest  string    `gorm:"uniqueIndex:idx_attachment_blob_digest;type:varchar(64)"`
	CreatedAt      time.Time `gorm:"autoCreateTime;not null"`
}
