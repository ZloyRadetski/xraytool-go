package payment

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"log/slog"

	"xraytool/internal/domain"
	"xraytool/internal/events"
)

var (
	ErrUserNotFound          = errors.New("user not found")
	ErrAlreadyPendingPayment = errors.New("user already has a pending payment")
	ErrPromoLimitReached     = errors.New("promo code usage limit reached")
	ErrPromoAlreadyUsed      = errors.New("promo code already used or pending for this user")
	ErrInvalidPlanID         = errors.New("invalid plan_id")
)

type Service struct {
	registry   domain.Registry
	dispatcher *events.Dispatcher
	log        *slog.Logger
}

func NewService(registry domain.Registry, dispatcher *events.Dispatcher, log *slog.Logger) *Service {
	return &Service{registry: registry, dispatcher: dispatcher, log: log}
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

// ProcessExternalPaymentStatus maps an external status and updates the payment, dispatching events if needed.
func (s *Service) ProcessExternalPaymentStatus(ctx context.Context, extID, status string) error {
	mappedStatus := status
	if status == "success" || status == "SUCCESS" || status == "CONFIRMED" || status == "COMPLETED" {
		mappedStatus = "completed"
	}

	payment, err := s.registry.Payments().FindByExternalID(ctx, extID)
	if err != nil {
		return err // not found or error
	}

	if payment.Status != mappedStatus && payment.Status != "completed" {
		updated, resErr := s.registry.Payments().UpdateStatusIfNotCompleted(ctx, payment.ID, mappedStatus)
		if resErr != nil {
			return resErr
		}
		if updated {
			s.log.Info("auto-updated payment status", "payment_id", payment.ID, "status", mappedStatus)

			if mappedStatus == "completed" {
				if err := s.applyReferralRewardForPayment(ctx, payment); err != nil {
					s.log.Error("failed to apply referral reward on platega callback", "err", err)
				}

				s.dispatcher.Dispatch("payment.completed", map[string]interface{}{
					"payment_id":   payment.ID,
					"amount":       payment.Amount,
					"payment_type": payment.PaymentType,
					"method":       payment.Method,
					"user_id":      payment.UserID,
				}, nil)
			}
		}
	}
	return nil
}

func (s *Service) FindAll(ctx context.Context) ([]domain.Payment, error) {
	return s.registry.Payments().FindAll(ctx)
}

// ScrubOldPayments deletes the external_id from payments older than a specific duration to protect user privacy.
func (s *Service) ScrubOldPayments(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	count, err := s.registry.Payments().ScrubOldExternalIDs(ctx, cutoff)
	if err == nil && count > 0 {
		s.log.Info("scrubbed old payment external IDs", "count", count, "cutoff", cutoff)
	}
	return count, err
}



// FindPaymentsByFilters is a specific method matching handlers.
func (s *Service) FindPaymentsByFilters(ctx context.Context, status, method, pt, tgIDStr string) ([]domain.Payment, error) {
	return s.registry.Payments().FindPaymentsByFilters(ctx, status, method, pt, tgIDStr)
}

func (s *Service) FindActivePlans(ctx context.Context) ([]domain.Plan, error) {
	return s.registry.Plans().FindActive(ctx)
}

func (s *Service) CreatePromoCode(ctx context.Context, promo *domain.PromoCode) error {
	return s.registry.Promos().Create(ctx, promo)
}

func (s *Service) FindAllPromoCodes(ctx context.Context) ([]domain.PromoCode, error) {
	return s.registry.Promos().FindAll(ctx)
}

func (s *Service) DeletePromoCode(ctx context.Context, id int64) (int64, error) {
	return s.registry.Promos().Delete(ctx, id)
}

func (s *Service) FindPromoCodeByID(ctx context.Context, id int64) (*domain.PromoCode, error) {
	return s.registry.Promos().FindByID(ctx, id)
}

func (s *Service) FindPromoCodeByCode(ctx context.Context, code string) (*domain.PromoCode, error) {
	return s.registry.Promos().FindByCode(ctx, code)
}

func (s *Service) UpdatePromoCode(ctx context.Context, promo *domain.PromoCode) error {
	return s.registry.Promos().Update(ctx, promo)
}

func (s *Service) CountPaymentsByPromoAndUser(ctx context.Context, promoID int64, userID string, status string) (int, error) {
	c, err := s.registry.Payments().CountByPromoAndUser(ctx, promoID, userID, status)
	return int(c), err
}

func (s *Service) CreatePayment(req CreatePaymentRequest) (*domain.Payment, error) {
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
	user, err := s.registry.Users().FindByPlatformID(context.Background(), "telegram", tgIDStr)
	if err != nil {
		return nil, ErrUserNotFound
	}

	var externalIDPtr *string
	if req.ExternalID != "" {
		externalIDPtr = &req.ExternalID
	}

	finalAmount := req.Amount
	var promoCodeID *int64

	if req.PlanID != nil {
		plan, err := s.registry.Plans().FindByID(context.Background(), fmt.Sprintf("%d", *req.PlanID))
		if err != nil {
			return nil, ErrInvalidPlanID
		}

		globalPrice := plan.BasePrice
		if plan.GlobalDiscountPercent > 0 {
			globalPrice = plan.BasePrice - (plan.BasePrice * plan.GlobalDiscountPercent / 100)
		}

		promoPrice := plan.BasePrice
		if req.PromoCode != "" {
			code := strings.ToUpper(strings.TrimSpace(req.PromoCode))
			promo, err := s.registry.Promos().FindByCode(context.Background(), code)
			if err == nil {
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
		code := strings.ToUpper(strings.TrimSpace(req.PromoCode))
		promo, err := s.registry.Promos().FindByCode(context.Background(), code)
		if err == nil {
			if promo.IsActive && (promo.ExpiresAt == nil || time.Now().Before(*promo.ExpiresAt)) {
				platform := strings.ToLower(strings.TrimSpace(req.Platform))
				if promo.TargetPlatform == "all" || promo.TargetPlatform == platform {
					promoCodeID = &promo.ID
				}
			}
		}
	}

	payment := domain.Payment{
		UserID:      user.ID,
		Amount:      finalAmount,
		Status:      "pending_card",
		PaymentType: req.PaymentType,
		Method:      req.Method,
		ExternalID:  externalIDPtr,
		PlanID:      req.PlanID,
		PromoCodeID: promoCodeID,
		CustomData: domain.Metadata{
			"telegram_id": req.TelegramID,
		},
	}

	txErr := s.registry.WithTx(context.Background(), func(tx domain.Registry) error {
		if promoCodeID != nil {
			promo, err := tx.Promos().FindByID(context.Background(), *promoCodeID)
			if err != nil {
				return err
			}
			if promo.MaxUses > 0 {
				ok, err := tx.Promos().IncrementUses(context.Background(), *promoCodeID, int(promo.MaxUses))
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("promo limit")
				}
			} else {
				tx.Promos().IncrementUses(context.Background(), *promoCodeID, 0)
			}

			count, err := tx.Payments().CountByUserAndPromo(context.Background(), user.ID, *promoCodeID)
			if err != nil {
				return err
			}
			if count > 0 {
				return fmt.Errorf("promo used by user")
			}
		}
		return tx.Payments().Create(context.Background(), &payment)
	})

	if txErr != nil {
		if txErr.Error() == "promo limit" {
			return nil, ErrPromoLimitReached
		}
		if txErr.Error() == "promo used by user" {
			return nil, ErrPromoAlreadyUsed
		}
		return nil, fmt.Errorf("failed to create payment: %w", txErr)
	}

	return &payment, nil
}

// FindPaymentByID returns a single payment by ID.
func (s *Service) FindPaymentByID(ctx context.Context, idStr string) (*domain.Payment, error) {
	return s.registry.Payments().FindByID(ctx, idStr)
}

// UpdatePaymentStatus updates the status of a payment.
func (s *Service) UpdatePaymentStatus(ctx context.Context, paymentID int64, status string, expectedStatuses []string) (bool, error) {
	updated, err := s.registry.Payments().UpdateStatus(ctx, paymentID, status, expectedStatuses)
	if err != nil {
		return false, err
	}
	if !updated {
		return false, nil
	}

	if status == "completed" {
		payment, err := s.registry.Payments().FindByID(ctx, fmt.Sprintf("%d", paymentID))
		if err == nil {
			if err := s.applyReferralRewardForPayment(ctx, payment); err != nil {
				s.log.Error("failed to apply referral reward", "err", err)
			}
			s.dispatcher.Dispatch("payment.completed", map[string]interface{}{
				"payment_id":   payment.ID,
				"amount":       payment.Amount,
				"payment_type": payment.PaymentType,
				"method":       payment.Method,
				"user_id":      payment.UserID,
			}, nil)
		}
	}
	return true, nil
}

// applyReferralRewardForPayment credits the referrer of the payer with 25% of
// the payment amount, and records a ReferralReward row. Returns nil if successful or no-op.
func (s *Service) applyReferralRewardForPayment(ctx context.Context, payment *domain.Payment) error {
	user, err := s.registry.Users().FindByID(ctx, payment.UserID)
	if err != nil {
		return nil // No user found, nothing to do
	}
	if user.ReferredBy == nil || *user.ReferredBy == "" {
		return nil
	}

	const referralPercent = 0.25
	reward := int(float64(payment.Amount) * referralPercent)
	if reward <= 0 {
		return nil
	}

	referrerID := *user.ReferredBy

	txErr := s.registry.Users().AddReferralReward(ctx, referrerID, user.ID, payment.ID, reward)

	if txErr != nil {
		s.log.Error("referral reward transaction failed", "err", txErr)
		return txErr
	}

	referrer, err := s.registry.Users().FindByID(ctx, referrerID)
	if err == nil {
		if tgIDRaw, ok := referrer.Metadata["telegram_id"]; ok {
			s.dispatcher.Dispatch("referral.reward", map[string]interface{}{
				"telegram_id":       tgIDRaw,
				"reward_amount":     reward,
				"referred_username": user.Username,
			}, nil)
		}
	}
	return nil
}
