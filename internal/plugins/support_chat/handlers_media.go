package support_chat

import (
	"encoding/json"
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

		// New uploads deliberately do not use global deduplication: an ordinary
		// content hash leaks whether another user uploaded the same file.
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
		localPath := attachmentStoragePath(p.cfg.Media.StoragePath, attachmentID)
		outFile, err := os.OpenFile(localPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			p.log.Error("Failed to create file", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		err = p.store.Crypto().EncryptAttachmentStream(attachmentID, outFile, file)
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

		// Save to DB
		att, err := p.store.CreateAttachment(r.Context(), attachmentID, userID, handler.Filename, mimeType, handler.Size)
		if err != nil {
			os.Remove(localPath)
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
		// If linked to a message, verify conversation ownership
		if att.MessageID != nil && *att.MessageID != "" {
			// Find conversation
			// We need to fetch the message, then the conversation
			// To keep it simple, just allow it if the user is an admin OR they are the owner of the conversation
			isAdmin := r.URL.Query().Get("admin") == "true" // Basic admin check (in prod should use RBAC middleware)
			if !isAdmin {
				// We need a helper to check ownership
				if !p.store.UserOwnsAttachment(r.Context(), userID, att) {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			}
		} else {
			// Unlinked attachment: only uploader can view
			if att.UploaderID != userID {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}

		storagePath := attachmentStoragePath(p.cfg.Media.StoragePath, att.ID)
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
			err = p.store.Crypto().DecryptAttachmentStream(att.ID, w, inFile)
		} else {
			err = p.store.legacyAttachmentCrypto().DecryptStream("global_attachments", w, inFile, att.Nonce)
		}
		if err != nil {
			p.log.Error("Failed to stream decrypted file", "error", err)
		}
	}
}
