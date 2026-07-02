package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xraytool/internal/domain"
)

// handleAdminCreatePromoCode creates a new promo code.
func (r *Router) handleAdminCreatePromoCode(w http.ResponseWriter, req *http.Request) {
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

	if err := r.paymentSvc.CreatePromoCode(req.Context(), &promo); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			http.Error(w, `{"error": "promo code already exists"}`, http.StatusConflict)
			return
		}
		r.log.Error("Failed to create promo code", "error", err)
		http.Error(w, `{"error": "database error"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(promo)
}

// handleAdminListPromoCodes returns all promo codes.
func (r *Router) handleAdminListPromoCodes(w http.ResponseWriter, req *http.Request) {
	codes, err := r.paymentSvc.FindAllPromoCodes(req.Context())
	if err != nil {
		r.log.Error("Failed to list promo codes", "error", err)
		http.Error(w, `{"error": "database error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(codes)
}

// handleAdminDeletePromoCode hard-deletes or deactivates a promo code.
func (r *Router) handleAdminDeletePromoCode(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error": "invalid id"}`, http.StatusBadRequest)
		return
	}

	rowsAffected, err := r.paymentSvc.DeletePromoCode(req.Context(), id)
	if err != nil {
		r.log.Error("Failed to delete promo code", "error", err)
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
func (r *Router) handleAdminEditPromoCode(w http.ResponseWriter, req *http.Request) {
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

	promo, err := r.paymentSvc.FindPromoCodeByID(req.Context(), id)
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

	if err := r.paymentSvc.UpdatePromoCode(req.Context(), promo); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			http.Error(w, `{"error": "promo code name already exists"}`, http.StatusConflict)
			return
		}
		r.log.Error("Failed to update promo code", "error", err)
		http.Error(w, `{"error": "database error"}`, http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(promo)
}
