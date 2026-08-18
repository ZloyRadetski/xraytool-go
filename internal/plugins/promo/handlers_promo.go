package promo

import (
	"errors"
	json "github.com/goccy/go-json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xraytool/internal/domain"
)

// handleAdminCreatePromoCode creates a new promo code.
func (p *Plugin) handleAdminCreatePromoCode(w http.ResponseWriter, req *http.Request) {
	var payload struct {
		Code            string     `json:"code"`
		DiscountPercent int        `json:"discount_percent"`
		MaxUses         int        `json:"max_uses"`
		TargetPlatform  string     `json:"target_platform"` // optional
		ExpiresAt       *time.Time `json:"expires_at"`      // optional
	}

	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		http.Error(w, `{"error": "invalid payload"}`, http.StatusBadRequest)
		return
	}

	payload.Code = strings.ToUpper(strings.TrimSpace(payload.Code))
	if payload.Code == "" || payload.DiscountPercent <= 0 || payload.DiscountPercent > 100 {
		http.Error(w, `{"error": "invalid code or discount"}`, http.StatusBadRequest)
		return
	}

	if payload.TargetPlatform == "" {
		payload.TargetPlatform = "all"
	}

	promo := domain.PromoCode{
		Code:            payload.Code,
		DiscountPercent: payload.DiscountPercent,
		MaxUses:         payload.MaxUses,
		TargetPlatform:  payload.TargetPlatform,
		ExpiresAt:       payload.ExpiresAt,
		IsActive:        true,
	}

	if err := p.registry.Promos().Create(req.Context(), &promo); err != nil {
		if errors.Is(err, domain.ErrDuplicate) || strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "duplicate") {
			http.Error(w, `{"error": "promo code already exists"}`, http.StatusConflict)
			return
		}
		p.log.Error("Failed to create promo code", "error", err)
		http.Error(w, `{"error": "database error"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(promo) //nolint:errcheck
}

// handleAdminListPromoCodes returns all promo codes.
func (p *Plugin) handleAdminListPromoCodes(w http.ResponseWriter, req *http.Request) {
	codes, err := p.registry.Promos().FindAll(req.Context())
	if err != nil {
		p.log.Error("Failed to list promo codes", "error", err)
		http.Error(w, `{"error": "database error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(codes) //nolint:errcheck
}

// handleAdminDeletePromoCode hard-deletes or deactivates a promo code.
func (p *Plugin) handleAdminDeletePromoCode(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error": "invalid id"}`, http.StatusBadRequest)
		return
	}

	rowsAffected, err := p.registry.Promos().Delete(req.Context(), id)
	if err != nil {
		p.log.Error("Failed to delete promo code", "error", err)
		http.Error(w, `{"error": "database error"}`, http.StatusInternalServerError)
		return
	}

	if rowsAffected == 0 {
		http.Error(w, `{"error": "promo code not found"}`, http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleAdminEditPromoCode edits an existing promo code.
func (p *Plugin) handleAdminEditPromoCode(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, `{"error": "invalid promo code id"}`, http.StatusBadRequest)
		return
	}

	var payload struct {
		Code            string     `json:"code"`
		DiscountPercent int        `json:"discount_percent"`
		MaxUses         int        `json:"max_uses"`
		TargetPlatform  string     `json:"target_platform"` // optional
		ExpiresAt       *time.Time `json:"expires_at"`      // optional
		IsActive        *bool      `json:"is_active"`       // optional
	}

	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		http.Error(w, `{"error": "invalid payload"}`, http.StatusBadRequest)
		return
	}

	promo, err := p.registry.Promos().FindByID(req.Context(), id)
	if err != nil {
		http.Error(w, `{"error": "promo code not found"}`, http.StatusNotFound)
		return
	}

	if payload.Code != "" {
		promo.Code = strings.ToUpper(strings.TrimSpace(payload.Code))
	}
	if payload.DiscountPercent > 0 && payload.DiscountPercent <= 100 {
		promo.DiscountPercent = payload.DiscountPercent
	}
	if payload.MaxUses >= 0 {
		promo.MaxUses = payload.MaxUses
	}
	if payload.TargetPlatform != "" {
		promo.TargetPlatform = payload.TargetPlatform
	}
	if payload.IsActive != nil {
		promo.IsActive = *payload.IsActive
	}
	promo.ExpiresAt = payload.ExpiresAt

	if err := p.registry.Promos().Update(req.Context(), promo); err != nil {
		if errors.Is(err, domain.ErrDuplicate) || strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "duplicate") {
			http.Error(w, `{"error": "promo code name already exists"}`, http.StatusConflict)
			return
		}
		p.log.Error("Failed to update promo code", "error", err)
		http.Error(w, `{"error": "database error"}`, http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(promo) //nolint:errcheck
}

func (p *Plugin) handleValidatePromoCode(w http.ResponseWriter, req *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(req.URL.Query().Get("code")))
	platform := strings.ToLower(strings.TrimSpace(req.URL.Query().Get("platform")))

	if code == "" {
		http.Error(w, `{"error": "code is required"}`, http.StatusBadRequest)
		return
	}
	if platform == "" {
		http.Error(w, `{"error": "platform is required"}`, http.StatusBadRequest)
		return
	}

	promo, err := p.registry.Promos().FindByCode(req.Context(), code)
	if err != nil {
		http.Error(w, `{"error": "promo code not found"}`, http.StatusNotFound)
		return
	}

	if !promo.IsActive {
		http.Error(w, `{"error": "promo code is inactive"}`, http.StatusBadRequest)
		return
	}

	if promo.ExpiresAt != nil && time.Now().After(*promo.ExpiresAt) {
		http.Error(w, `{"error": "promo code has expired"}`, http.StatusBadRequest)
		return
	}

	if promo.TargetPlatform != "all" && promo.TargetPlatform != platform {
		http.Error(w, `{"error": "promo code is not valid for this platform"}`, http.StatusBadRequest)
		return
	}

	if promo.MaxUses > 0 && promo.UsesCount >= promo.MaxUses {
		http.Error(w, `{"error": "promo code usage limit reached"}`, http.StatusBadRequest)
		return
	}

	telegramIDStr := strings.TrimSpace(req.URL.Query().Get("telegram_id"))
	if telegramIDStr != "" {
		user, err := p.userSvc.FindUserByPlatformID(req.Context(), "telegram", telegramIDStr)
		if err == nil {
			count, _ := p.registry.Payments().CountByPromoAndUser(req.Context(), promo.ID, user.ID, "completed")
			if count > 0 {
				http.Error(w, `{"error": "promo code already used by this user"}`, http.StatusBadRequest)
				return
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
		"valid":            true,
		"discount_percent": promo.DiscountPercent,
		"id":               promo.ID,
	})
}
