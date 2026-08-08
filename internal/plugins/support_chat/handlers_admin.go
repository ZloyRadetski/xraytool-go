package support_chat

import (
	"encoding/json"
	"net/http"
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
