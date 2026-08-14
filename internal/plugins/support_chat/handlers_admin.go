package support_chat

import (
	"encoding/json"
	"net/http"
	"time"
)

func (p *Plugin) handleAdminListConversations() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Assume admin auth middleware is applied
		var filter ConversationFilter

		if uid := r.URL.Query().Get("user_id"); uid != "" {
			filter.UserID = &uid
		}
		if status := r.URL.Query().Get("status"); status != "" {
			filter.Status = &status
		}

		convs, err := p.store.ListConversations(r.Context(), filter)
		if err != nil {
			p.log.Error("Failed to list conversations for admin", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"conversations": convs})
	}
}

func (p *Plugin) handleAdminListMessages() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		convID := r.PathValue("id")
		if convID == "" {
			http.Error(w, "Missing conversation ID", http.StatusBadRequest)
			return
		}

		// Verify conversation exists
		if _, err := p.store.GetConversation(r.Context(), convID); err != nil {
			http.Error(w, "Conversation not found", http.StatusNotFound)
			return
		}

		// Mark as read
		_ = p.store.MarkMessagesRead(r.Context(), convID, "admin")

		msgs, err := p.store.ListMessages(r.Context(), convID)
		if err != nil {
			p.log.Error("Failed to list messages for admin", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"messages": msgs})
	}
}

func (p *Plugin) handleAdminCreateMessage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		convID := r.PathValue("id")
		if convID == "" {
			http.Error(w, "Missing conversation ID", http.StatusBadRequest)
			return
		}

		// Verify conversation
		conv, err := p.store.GetConversation(r.Context(), convID)
		if err != nil {
			http.Error(w, "Conversation not found", http.StatusNotFound)
			return
		}
		if conv.Status == "closed" {
			http.Error(w, "Conversation is closed", http.StatusForbidden)
			return
		}

		var req createMessageReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Text == "" && len(req.Attachments) == 0 {
			http.Error(w, "Text or attachments are required", http.StatusBadRequest)
			return
		}

		msg, err := p.store.CreateMessage(r.Context(), convID, "admin", req.Text, req.Attachments)
		if err != nil {
			p.log.Error("Failed to create message for admin", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Notify via websocket (phase 6)
		if bMsg, err := json.Marshal(map[string]any{
			"type": "new_message",
			"payload": msg,
		}); err == nil {
			p.hub.BroadcastToClient(conv.UserID, bMsg)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(msg)
	}
}

type patchStatusReq struct {
	Status string `json:"status"` // 'open', 'closed', 'resolved'
}

func (p *Plugin) handleAdminPatchStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		convID := r.PathValue("id")
		if convID == "" {
			http.Error(w, "Missing conversation ID", http.StatusBadRequest)
			return
		}

		var req patchStatusReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Status != "open" && req.Status != "closed" && req.Status != "resolved" {
			http.Error(w, "Invalid status", http.StatusBadRequest)
			return
		}

		if err := p.store.UpdateStatus(r.Context(), convID, req.Status); err != nil {
			p.log.Error("Failed to update status", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Notify via websocket (phase 6)
		if bMsg, err := json.Marshal(map[string]any{
			"type": "status_changed",
			"payload": map[string]any{
				"conversation_id": convID,
				"status":          req.Status,
			},
		}); err == nil {
			// In a real app we might want to get the user ID for this conv
			// to notify them. But broadcasting to all for now is okay, or we could fetch conv.
			if conv, err := p.store.GetConversation(r.Context(), convID); err == nil {
				p.hub.BroadcastToClient(conv.UserID, bMsg)
				p.hub.BroadcastToAdmins(bMsg)
			}
		}

		w.WriteHeader(http.StatusOK)
	}
}

func (p *Plugin) handleAdminDeleteConversation() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		convID := r.PathValue("id")
		if convID == "" {
			http.Error(w, "Missing conversation ID", http.StatusBadRequest)
			return
		}

		adminID := getUserID(r)

		conv, err := p.store.GetConversation(r.Context(), convID)
		if err != nil {
			http.Error(w, "Conversation not found", http.StatusNotFound)
			return
		}

		if err := p.store.DeleteConversation(r.Context(), convID, p.cfg.Media.StoragePath); err != nil {
			if p.log != nil {
				p.log.Error("Failed to delete conversation by admin", "error", err, "conv_id", convID)
			}
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if p.log != nil {
			p.log.Info("Support conversation deleted by admin", "conv_id", convID, "admin_id", adminID, "user_id", conv.UserID)
		}

		// Notify via websocket
		if p.hub != nil {
			if bMsg, err := json.Marshal(map[string]any{
				"type": "conversation_deleted",
				"payload": map[string]any{
					"conversation_id": convID,
					"deleted_by":      adminID,
				},
			}); err == nil {
				p.hub.BroadcastToClient(conv.UserID, bMsg)
				p.hub.BroadcastToAdmins(bMsg)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}
}

type createBanReq struct {
	UserID    string     `json:"user_id"`
	Reason    string     `json:"reason"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (p *Plugin) handleAdminCreateBan() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createBanReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.UserID == "" {
			http.Error(w, "Missing user_id", http.StatusBadRequest)
			return
		}

		adminID := getUserID(r)
		ban, err := p.store.BanUser(r.Context(), req.UserID, req.Reason, adminID, req.ExpiresAt)
		if err != nil {
			if p.log != nil {
				p.log.Error("Failed to ban user in support", "error", err, "user_id", req.UserID)
			}
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if p.log != nil {
			p.log.Info("Support ban created", "user_id", req.UserID, "admin_id", adminID, "expires_at", req.ExpiresAt)
		}

		// Terminate any active WebSocket session for the banned client
		if p.hub != nil {
			p.hub.DisconnectUser(req.UserID)

			// Notify admins via websocket
			if bMsg, err := json.Marshal(map[string]any{
				"type": "user_banned",
				"payload": map[string]any{
					"user_id":    req.UserID,
					"reason":     req.Reason,
					"expires_at": req.ExpiresAt,
					"banned_by":  adminID,
				},
			}); err == nil {
				p.hub.BroadcastToAdmins(bMsg)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(ban)
	}
}

func (p *Plugin) handleAdminDeleteBan() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		targetUserID := r.PathValue("user_id")
		if targetUserID == "" {
			http.Error(w, "Missing user_id", http.StatusBadRequest)
			return
		}

		adminID := getUserID(r)
		if err := p.store.UnbanUser(r.Context(), targetUserID); err != nil {
			if p.log != nil {
				p.log.Error("Failed to unban user in support", "error", err, "user_id", targetUserID)
			}
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if p.log != nil {
			p.log.Info("Support ban removed", "user_id", targetUserID, "admin_id", adminID)
		}

		// Notify admins via websocket
		if p.hub != nil {
			if bMsg, err := json.Marshal(map[string]any{
				"type": "user_unbanned",
				"payload": map[string]any{
					"user_id": targetUserID,
				},
			}); err == nil {
				p.hub.BroadcastToAdmins(bMsg)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}
}

func (p *Plugin) handleAdminListBans() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bans, err := p.store.ListBans(r.Context())
		if err != nil {
			if p.log != nil {
				p.log.Error("Failed to list support bans", "error", err)
			}
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"bans": bans})
	}
}

func (p *Plugin) handleAdminGetBan() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		targetUserID := r.PathValue("user_id")
		if targetUserID == "" {
			http.Error(w, "Missing user_id", http.StatusBadRequest)
			return
		}

		ban, err := p.store.GetBan(r.Context(), targetUserID)
		if err != nil {
			if p.log != nil {
				p.log.Error("Failed to get support ban", "error", err, "user_id", targetUserID)
			}
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if ban == nil {
			http.Error(w, "Ban not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ban)
	}
}


