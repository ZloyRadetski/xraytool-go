package support_chat

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"xraytool/internal/pluginapi"
)

// getUserID accepts identity only from the authenticated reverse-proxy request.
// Query parameters are deliberately excluded: allowing user_id in a URL turns
// identity into attacker-controlled input that is commonly logged and shared.
func getUserID(r *http.Request) string {
	if uid, ok := r.Context().Value("user_id").(string); ok && uid != "" {
		return strings.TrimSpace(uid)
	}
	if uid := strings.TrimSpace(r.Header.Get("X-User-ID")); uid != "" {
		return uid
	}
	if uid := strings.TrimSpace(r.Header.Get("X-Telegram-ID")); uid != "" {
		return uid
	}
	return ""
}

// requireAdmin determines the role on the server, not from a query parameter
// or a header sent by a browser. The outer API-key middleware ensures headers
// are accepted only from a trusted service such as the web application.
func (p *Plugin) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := p.lookupRequestUser(r.Context(), getUserID(r))
		if err != nil || user == nil || !user.IsAdmin {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (p *Plugin) requestIsAdmin(ctx context.Context, userID string) bool {
	user, err := p.lookupRequestUser(ctx, userID)
	return err == nil && user != nil && user.IsAdmin
}

func (p *Plugin) lookupRequestUser(ctx context.Context, userID string) (*pluginapi.User, error) {
	if p.users == nil {
		return nil, http.ErrNotSupported
	}

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, http.ErrNoCookie
	}

	telegramID := strings.TrimPrefix(userID, "telegram:")
	if id, err := strconv.ParseInt(telegramID, 10, 64); err == nil && id > 0 {
		user, lookupErr := p.users.FindByTelegramID(ctx, id)
		if lookupErr == nil || user != nil {
			return user, lookupErr
		}
		// Older deployments stored Telegram clients under platform=telegram,
		// while newer integrations use platform=bot. Support both during the
		// transition without trusting a role supplied by the caller.
		return p.users.FindByPlatformID(ctx, "bot", telegramID)
	}

	return p.users.FindByEmailOrUsername(ctx, strings.ToLower(userID))
}

// To access URL params from http.ServeMux in go 1.22+ we can use r.PathValue("id")

type createConversationReq struct {
	Subject     string   `json:"subject"`
	Message     string   `json:"message"`
	Attachments []string `json:"attachments"`
}

func (p *Plugin) handleClientCreateConversation() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := getUserID(r)
		if userID == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		var req createConversationReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if (req.Subject == "" || req.Message == "") && len(req.Attachments) == 0 {
			http.Error(w, "Subject and message or attachments are required", http.StatusBadRequest)
			return
		}
		if req.Subject == "" {
			req.Subject = "Прикрепленный файл" // Default subject
		}

		// Enforce MaxOpenPerUser limit
		if p.cfg.MaxOpenPerUser > 0 {
			openStatus := "open"
			openConvs, err := p.store.ListConversations(r.Context(), ConversationFilter{
				UserID: &userID,
				Status: &openStatus,
			})
			if err != nil {
				p.log.Error("Failed to check open conversations", "error", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			if len(openConvs) >= p.cfg.MaxOpenPerUser {
				http.Error(w, "Maximum number of open conversations reached", http.StatusForbidden)
				return
			}
		}

		conv, err := p.store.CreateConversation(r.Context(), userID, req.Subject)
		if err != nil {
			p.log.Error("Failed to create conversation", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		msg, err := p.store.CreateMessage(r.Context(), conv.ID, "client", req.Message, req.Attachments)
		if err != nil {
			p.log.Error("Failed to create initial message", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Notify admins via websocket (phase 6)
		if bMsg, err := json.Marshal(map[string]any{
			"type": "new_conversation",
			"payload": map[string]any{
				"conversation_id": conv.ID,
				"user_id":         userID,
				"subject":         req.Subject,
				"created_at":      conv.CreatedAt,
			},
		}); err == nil {
			p.hub.BroadcastToAdmins(bMsg)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"conversation_id": conv.ID,
			"created_at":      conv.CreatedAt,
			"message":         msg,
		})
	}
}

func (p *Plugin) handleClientListConversations() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := getUserID(r)
		if userID == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		convs, err := p.store.ListConversations(r.Context(), ConversationFilter{UserID: &userID})
		if err != nil {
			p.log.Error("Failed to list conversations", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"conversations": convs})
	}
}

func (p *Plugin) handleClientGetConversation() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := getUserID(r)
		if userID == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		convID := r.PathValue("id")
		if convID == "" {
			http.Error(w, "Missing conversation ID", http.StatusBadRequest)
			return
		}

		conv, err := p.store.GetConversation(r.Context(), convID)
		if err != nil {
			http.Error(w, "Conversation not found", http.StatusNotFound)
			return
		}
		if conv.UserID != userID {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(conv)
	}
}

func (p *Plugin) handleClientListMessages() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := getUserID(r)
		if userID == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		convID := r.PathValue("id")
		if convID == "" {
			http.Error(w, "Missing conversation ID", http.StatusBadRequest)
			return
		}

		// Verify ownership
		conv, err := p.store.GetConversation(r.Context(), convID)
		if err != nil {
			http.Error(w, "Conversation not found", http.StatusNotFound)
			return
		}
		if conv.UserID != userID {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		// Mark as read
		_ = p.store.MarkMessagesRead(r.Context(), convID, "client")

		msgs, err := p.store.ListMessages(r.Context(), convID)
		if err != nil {
			p.log.Error("Failed to list messages", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"messages": msgs})
	}
}

type createMessageReq struct {
	Text        string   `json:"text"`
	Attachments []string `json:"attachments"`
}

func (p *Plugin) handleClientCreateMessage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := getUserID(r)
		if userID == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		convID := r.PathValue("id")
		if convID == "" {
			http.Error(w, "Missing conversation ID", http.StatusBadRequest)
			return
		}

		// Verify ownership and status
		conv, err := p.store.GetConversation(r.Context(), convID)
		if err != nil {
			http.Error(w, "Conversation not found", http.StatusNotFound)
			return
		}
		if conv.UserID != userID {
			http.Error(w, "Forbidden", http.StatusForbidden)
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

		msg, err := p.store.CreateMessage(r.Context(), convID, "client", req.Text, req.Attachments)
		if err != nil {
			p.log.Error("Failed to create message", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Notify via websocket (phase 6)
		if bMsg, err := json.Marshal(map[string]any{
			"type":    "new_message",
			"payload": msg,
		}); err == nil {
			p.hub.BroadcastToAdmins(bMsg)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(msg)
	}
}
