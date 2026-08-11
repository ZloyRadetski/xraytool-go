package server

import (
	json "github.com/goccy/go-json"
	"net/http"
)

type PlanResponse struct {
	ID                    int64 `json:"id"`
	Months                int   `json:"months"`
	BasePrice             int   `json:"base_price"`
	GlobalDiscountPercent int   `json:"discount_percent"`
	FinalPrice            int   `json:"final_price"`
}

func (r *Router) handleGetPlans(w http.ResponseWriter, req *http.Request) {
	plans, err := r.registry.Plans().FindActive(req.Context())
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
