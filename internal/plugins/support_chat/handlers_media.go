package support_chat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
)

func (p *Plugin) handleUploadAttachment() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := getUserID(r)
		if userID == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		banned, _, err := p.store.IsUserBanned(r.Context(), userID)
		if err != nil {
			if p.log != nil {
				p.log.Error("Failed to check support ban", "error", err, "user_id", userID)
			}
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if banned {
			http.Error(w, "User is banned from support", http.StatusForbidden)
			return
		}

		// Enforce size limit
		maxBytes := int64(p.cfg.Media.MaxFileSizeMB) * 1024 * 1024
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

		if err := r.ParseMultipartForm(maxBytes); err != nil {
			http.Error(w, "File too large or invalid multipart form", http.StatusBadRequest)
			return
		}

		file, handler, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "Missing file", http.StatusBadRequest)
			return
		}
		defer file.Close()

		// Determine mime type
		mimeType := handler.Header.Get("Content-Type")
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}

		// Calculate a source hash first, then turn it into an HMAC scoped to the
		// uploader. This permits deduplication for one user without leaking that
		// another user uploaded identical content.
		hasher := sha256.New()
		if _, err := io.Copy(hasher, file); err != nil {
			http.Error(w, "Failed to read file", http.StatusInternalServerError)
			return
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "Failed to rewind file", http.StatusInternalServerError)
			return
		}
		contentDigest := p.store.AttachmentContentDigest(userID, hex.EncodeToString(hasher.Sum(nil)))
		existingBlob, err := p.store.FindAttachmentBlob(r.Context(), userID, contentDigest)
		if err != nil {
			p.log.Error("Failed to find attachment blob", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if err := os.MkdirAll(p.cfg.Media.StoragePath, 0700); err != nil {
			p.log.Error("Failed to create media directory", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if err := os.Chmod(p.cfg.Media.StoragePath, 0700); err != nil {
			p.log.Error("Failed to protect media directory", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		attachmentID := uuid.New().String()
		storageKey := ""
		localPath := ""
		if existingBlob != nil {
			storageKey = existingBlob.StorageKey
		} else {
			storageKey = uuid.New().String()
			localPath = attachmentStoragePath(p.cfg.Media.StoragePath, storageKey)
			outFile, err := os.OpenFile(localPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				p.log.Error("Failed to create file", "error", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			err = p.store.Crypto().EncryptAttachmentStream(storageKey, outFile, file)
			closeErr := outFile.Close()
			if err != nil || closeErr != nil {
				_ = os.Remove(localPath)
				if err == nil {
					err = closeErr
				}
				p.log.Error("Failed to encrypt/save file", "error", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}

			if _, err := p.store.CreateAttachmentBlob(r.Context(), storageKey, userID, contentDigest); err != nil {
				// A concurrent identical upload may have won the unique blob claim.
				blob, findErr := p.store.FindAttachmentBlob(r.Context(), userID, contentDigest)
				if findErr != nil || blob == nil {
					_ = os.Remove(localPath)
					p.log.Error("Failed to save attachment blob", "error", err)
					http.Error(w, "Internal server error", http.StatusInternalServerError)
					return
				}
				_ = os.Remove(localPath)
				storageKey = blob.StorageKey
				localPath = ""
			}
		}

		// Save to DB
		att, err := p.store.CreateAttachment(r.Context(), attachmentID, storageKey, userID, handler.Filename, mimeType, handler.Size)
		if err != nil {
			// Keep a newly claimed blob intact. A concurrent identical upload may
			// already be creating its attachment record against it; removing the
			// file here would break that otherwise valid upload. An unreferenced
			// blob is safe to reuse on the next identical upload.
			p.log.Error("Failed to save attachment to db", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":        att.ID,
			"file_name": att.FileName,
			"mime_type": att.MimeType,
		})
	}
}

func (p *Plugin) handleDownloadAttachment() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := getUserID(r)
		if userID == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		attID := r.PathValue("id")
		if attID == "" {
			http.Error(w, "Missing attachment ID", http.StatusBadRequest)
			return
		}

		att, err := p.store.GetAttachment(r.Context(), attID)
		if err != nil {
			http.Error(w, "Attachment not found", http.StatusNotFound)
			return
		}

		// Access control:
		// If linked to a message, allow either its owner or an administrator
		// resolved from the server-side user repository. Never treat a query
		// parameter as an authorization decision.
		if att.MessageID != nil && *att.MessageID != "" {
			if !p.requestIsAdmin(r.Context(), userID) && !p.store.UserOwnsAttachment(r.Context(), userID, att) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		} else {
			// Unlinked attachment: only uploader can view
			if att.UploaderID != userID {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}

		storageKey := attachmentStorageKey(att)
		storagePath := attachmentStoragePath(p.cfg.Media.StoragePath, storageKey)
		if att.EncryptionVersion < currentEncryptionVersion {
			storagePath = att.LegacyStoragePath
		}
		inFile, err := os.Open(storagePath)
		if err != nil {
			p.log.Error("Failed to open media file", "error", err)
			http.Error(w, "File missing", http.StatusNotFound)
			return
		}
		defer inFile.Close()

		w.Header().Set("Content-Type", att.MimeType)
		w.Header().Set("Content-Disposition", `inline; filename="`+strings.ReplaceAll(att.FileName, `"`, `\"`)+`"`)
		w.Header().Set("Cache-Control", "no-store")

		if att.EncryptionVersion >= currentEncryptionVersion {
			err = p.store.Crypto().DecryptAttachmentStream(storageKey, w, inFile)
		} else {
			err = p.store.legacyAttachmentCrypto().DecryptStream("global_attachments", w, inFile, att.Nonce)
		}
		if err != nil {
			p.log.Error("Failed to stream decrypted file", "error", err)
		}
	}
}
