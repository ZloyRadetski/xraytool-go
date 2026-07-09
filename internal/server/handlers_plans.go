package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type PlanResponse struct {
	ID                    int64 `json:"id"`
	Months                int   `json:"months"`
	BasePrice             int   `json:"base_price"`
	GlobalDiscountPercent int   `json:"discount_percent"`
	FinalPrice            int   `json:"final_price"`
}

func (r *Router) handleGetPlans(w http.ResponseWriter, req *http.Request) {
	plans, err := r.paymentSvc.FindActivePlans(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get plans")
		return
	}

	var resp []PlanResponse
	for _, p := range plans {
		finalPrice := p.BasePrice
		if p.GlobalDiscountPercent > 0 {
			finalPrice = p.BasePrice - (p.BasePrice * p.GlobalDiscountPercent / 100)
		}
		resp = append(resp, PlanResponse{
			ID:                    p.ID,
			Months:                p.Months,
			BasePrice:             p.BasePrice,
			GlobalDiscountPercent: p.GlobalDiscountPercent,
			FinalPrice:            finalPrice,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

func (r *Router) handleValidatePromoCode(w http.ResponseWriter, req *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(req.URL.Query().Get("code")))
	platform := strings.ToLower(strings.TrimSpace(req.URL.Query().Get("platform")))

	if code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}
	if platform == "" {
		writeError(w, http.StatusBadRequest, "platform is required")
		return
	}

	promo, err := r.paymentSvc.FindPromoCodeByCode(req.Context(), code)
	if err != nil {
		writeError(w, http.StatusNotFound, "promo code not found")
		return
	}

	if !promo.IsActive {
		writeError(w, http.StatusBadRequest, "promo code is inactive")
		return
	}

	if promo.ExpiresAt != nil && time.Now().After(*promo.ExpiresAt) {
		writeError(w, http.StatusBadRequest, "promo code has expired")
		return
	}

	if promo.TargetPlatform != "all" && promo.TargetPlatform != platform {
		writeError(w, http.StatusBadRequest, "promo code is not valid for this platform")
		return
	}

	if promo.MaxUses > 0 && promo.UsesCount >= promo.MaxUses {
		writeError(w, http.StatusBadRequest, "promo code usage limit reached")
		return
	}

	telegramIDStr := strings.TrimSpace(req.URL.Query().Get("telegram_id"))
	if telegramIDStr != "" {
		user, err := r.userSvc.FindUserByPlatformID(req.Context(), "telegram", telegramIDStr)
		if err == nil {
			count, _ := r.paymentSvc.CountPaymentsByPromoAndUser(req.Context(), promo.ID, user.ID, "completed")
			if count > 0 {
				writeError(w, http.StatusBadRequest, "promo code already used by this user")
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
