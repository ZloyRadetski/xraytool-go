package payment

import (
	"fmt"
	"strings"
	"time"
	"strconv"

	"gorm.io/gorm"
	"xraytool/internal/database"
)

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

type CreatePaymentRequest struct {
	TelegramID  int64
	Amount      int
	PaymentType string
	Method      string
	ExternalID  string
	PlanID      *int64
	PromoCode   string
	Platform    string
}

func (s *Service) CreatePayment(req CreatePaymentRequest) (*database.Payment, error) {
	if req.TelegramID == 0 {
		return nil, fmt.Errorf("telegram_id is required")
	}
	if req.PlanID == nil && req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive if plan_id is missing")
	}
	if req.PaymentType == "" {
		return nil, fmt.Errorf("payment_type is required")
	}

	tgIDStr := strconv.FormatInt(req.TelegramID, 10)
	user, err := database.FindUserByPlatformID(s.db, "telegram", tgIDStr)
	if err != nil {
		return nil, fmt.Errorf("user not found")
	}

	var externalIDPtr *string
	if req.ExternalID != "" {
		externalIDPtr = &req.ExternalID
	}

	finalAmount := req.Amount
	var promoCodeID *int64

	if req.PlanID != nil {
		var plan database.Plan
		if err := s.db.First(&plan, *req.PlanID).Error; err != nil {
			return nil, fmt.Errorf("invalid plan_id")
		}

		globalPrice := plan.BasePrice
		if plan.GlobalDiscountPercent > 0 {
			globalPrice = plan.BasePrice - (plan.BasePrice * plan.GlobalDiscountPercent / 100)
		}

		promoPrice := plan.BasePrice
		if req.PromoCode != "" {
			var promo database.PromoCode
			code := strings.ToUpper(strings.TrimSpace(req.PromoCode))
			if err := s.db.Where("code = ?", code).First(&promo).Error; err == nil {
				if promo.IsActive && (promo.ExpiresAt == nil || time.Now().Before(*promo.ExpiresAt)) {
					platform := strings.ToLower(strings.TrimSpace(req.Platform))
					if promo.TargetPlatform == "all" || promo.TargetPlatform == platform {

						promoPrice = plan.BasePrice - (plan.BasePrice * promo.DiscountPercent / 100)
						promoCodeID = &promo.ID
					}
				}
			}
		}

		if globalPrice < promoPrice {
			finalAmount = globalPrice
			promoCodeID = nil
		} else {
			finalAmount = promoPrice
		}
	} else if req.PromoCode != "" {
		var promo database.PromoCode
		code := strings.ToUpper(strings.TrimSpace(req.PromoCode))
		if err := s.db.Where("code = ?", code).First(&promo).Error; err == nil {
			if promo.IsActive && (promo.ExpiresAt == nil || time.Now().Before(*promo.ExpiresAt)) {
				platform := strings.ToLower(strings.TrimSpace(req.Platform))
				if promo.TargetPlatform == "all" || promo.TargetPlatform == platform {
					promoCodeID = &promo.ID
				}
			}
		}
	}

	payment := database.Payment{
		UserID:      user.ID,
		Amount:      finalAmount,
		Status:      "pending_card",
		PaymentType: req.PaymentType,
		Method:      req.Method,
		ExternalID:  externalIDPtr,
		PlanID:      req.PlanID,
		PromoCodeID: promoCodeID,
		CustomData: database.Metadata{
			"telegram_id": req.TelegramID,
		},
	}

	txErr := s.db.Transaction(func(tx *gorm.DB) error {
		if promoCodeID != nil {
			var promo database.PromoCode
			if err := tx.First(&promo, *promoCodeID).Error; err != nil {
				return err
			}
			if promo.MaxUses > 0 {
				res := tx.Exec("UPDATE promo_codes SET uses_count = uses_count + 1 WHERE id = ? AND uses_count < max_uses", *promoCodeID)
				if res.RowsAffected == 0 {
					return fmt.Errorf("promo limit")
				}
			} else {
				tx.Exec("UPDATE promo_codes SET uses_count = uses_count + 1 WHERE id = ?", *promoCodeID)
			}

			var count int64
			tx.Model(&database.Payment{}).Where("user_id = ? AND promo_code_id = ? AND status IN (?, ?)", user.ID, *promoCodeID, "completed", "pending_card").Count(&count)
			if count > 0 {
				return fmt.Errorf("promo used by user")
			}
		}
		return tx.Create(&payment).Error
	})

	if txErr != nil {
		if txErr.Error() == "promo limit" {
			return nil, fmt.Errorf("promo code usage limit reached")
		}
		if txErr.Error() == "promo used by user" {
			return nil, fmt.Errorf("promo code already used or pending for this user")
		}
		return nil, fmt.Errorf("failed to create payment: %w", txErr)
	}

	return &payment, nil
}
