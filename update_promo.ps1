
$content = Get-Content "C:\Dev\SERVER\xraytool-go\internal\server\handlers_promo.go" -Raw
$content = $content -replace '"strconv"', "\"strconv\"`n`t`\"time\""
$content = $content -replace 'TargetPlatform  string `json:"target_platform"` // optional', "TargetPlatform  string ``json:`"target_platform`"`` // optional`n`t`tExpiresAt       *time.Time ``json:`"expires_at`"``      // optional"
$content = $content -replace 'TargetPlatform:  payload.TargetPlatform,', "TargetPlatform:  payload.TargetPlatform,`n`t`tExpiresAt:       payload.ExpiresAt,"
Set-Content "C:\Dev\SERVER\xraytool-go\internal\server\handlers_promo.go" $content

$edit_func = @"
`n// handleAdminEditPromoCode edits an existing promo code.
func (r *Router) handleAdminEditPromoCode(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, \`{"error": "invalid promo code id"}\`, http.StatusBadRequest)
		return
	}

	var payload struct {
		Code            string     \`json:"code"\`
		DiscountPercent int        \`json:"discount_percent"\`
		MaxUses         int        \`json:"max_uses"\`
		TargetPlatform  string     \`json:"target_platform"\` // optional
		ExpiresAt       *time.Time \`json:"expires_at"\`      // optional
		IsActive        *bool      \`json:"is_active"\`       // optional
	}

	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		http.Error(w, \`{"error": "invalid payload"}\`, http.StatusBadRequest)
		return
	}

	db := database.DB()
	var promo database.PromoCode
	if err := db.First(&promo, id).Error; err != nil {
		http.Error(w, \`{"error": "promo code not found"}\`, http.StatusNotFound)
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

	if err := db.Save(&promo).Error; err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			http.Error(w, \`{"error": "promo code name already exists"}\`, http.StatusConflict)
			return
		}
		r.log.Error("Failed to update promo code", "error", err)
		http.Error(w, \`{"error": "database error"}\`, http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(promo)
}
"@

Add-Content "C:\Dev\SERVER\xraytool-go\internal\server\handlers_promo.go" $edit_func

