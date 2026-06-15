package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"strconv"
	"time"

	"xraytool/internal/database"
)

// handleAdminCreatePromoCode creates a new promo code.
func (r *Router) handleAdminCreatePromoCode(w http.ResponseWriter, req *http.Request) {
	var payload struct {
		Code            string `json:"code"`
		DiscountPercent int    `json:"discount_percent"`
		MaxUses         int    `json:"max_uses"`
		TargetPlatform  string `json:"target_platform"` // optional
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

	promo := database.PromoCode{
		Code:            payload.Code,
		DiscountPercent: payload.DiscountPercent,
		MaxUses:         payload.MaxUses,
		TargetPlatform:  payload.TargetPlatform,
		IsActive:        true,
	}

	db := database.DB()
	if err := db.Create(&promo).Error; err != nil {
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
	db := database.DB()
	var codes []database.PromoCode
	if err := db.Order("created_at desc").Find(&codes).Error; err != nil {
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

	db := database.DB()
	res := db.Delete(&database.PromoCode{}, id)
	if res.Error != nil {
		r.log.Error("Failed to delete promo code", "error", res.Error)
		http.Error(w, `{"error": "database error"}`, http.StatusInternalServerError)
		return
	}

	if res.RowsAffected == 0 {
		http.Error(w, `{"error": "promo code not found"}`, http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
