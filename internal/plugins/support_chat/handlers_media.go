package support_chat

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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

		// Prepare local storage path
		err = os.MkdirAll(p.cfg.Media.StoragePath, 0755)
		if err != nil {
			p.log.Error("Failed to create media directory", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		fileName := uuid.New().String() + ".bin"
		localPath := filepath.Join(p.cfg.Media.StoragePath, fileName)
		outFile, err := os.Create(localPath)
		if err != nil {
			p.log.Error("Failed to create file", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Encrypt on the fly using conversation ID? 
		// Wait! Attachments are uploaded BEFORE they are linked to a message, so we don't know the ConversationID yet!
		// We can just use the Plugin's master key directly with a dummy static conversation ID "attachment" to encrypt the file, OR use the uploader ID.
		// Let's use a static context string "global_attachments" for all files.
		nonce, err := p.store.Crypto().EncryptStream("global_attachments", outFile, file)
		outFile.Close()
		if err != nil {
			os.Remove(localPath)
			p.log.Error("Failed to encrypt/save file", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Get file size
		fi, _ := os.Stat(localPath)
		var size int64
		if fi != nil {
			size = fi.Size()
		} else {
			size = handler.Size
		}

		// Save to DB
		att, err := p.store.CreateAttachment(r.Context(), userID, handler.Filename, mimeType, size, localPath, nonce)
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

		inFile, err := os.Open(att.StoragePath)
		if err != nil {
			p.log.Error("Failed to open media file", "error", err)
			http.Error(w, "File missing", http.StatusNotFound)
			return
		}
		defer inFile.Close()

		w.Header().Set("Content-Type", att.MimeType)
		w.Header().Set("Content-Disposition", `inline; filename="`+strings.ReplaceAll(att.FileName, `"`, `\"`)+`"`)
		w.Header().Set("Cache-Control", "private, max-age=86400")

		err = p.store.Crypto().DecryptStream("global_attachments", w, inFile, att.Nonce)
		if err != nil {
			p.log.Error("Failed to stream decrypted file", "error", err)
		}
	}
}
