package billing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"

	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
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

	pricingMu sync.RWMutex
	pricing   pluginapi.PricingEngine
}

func NewService(registry domain.Registry, dispatcher *events.Dispatcher, log *slog.Logger) *Service {
	return NewServiceWithPricing(registry, dispatcher, log, nil)
}

// NewServiceWithPricing constructs a payment service with an optional pricing
// engine. Passing nil preserves the legacy in-process calculation and keeps
// existing callers fully backwards compatible while the plugin host is rolled
// out.
func NewServiceWithPricing(
	registry domain.Registry,
	dispatcher *events.Dispatcher,
	log *slog.Logger,
	pricing pluginapi.PricingEngine,
) *Service {
	return &Service{registry: registry, dispatcher: dispatcher, log: log, pricing: pricing}
}

// SetPricingEngine installs or replaces the pricing engine before the service
// begins serving requests. It is safe for the kernel to call during post-load
// wiring; reads are synchronised for callers that also expose a live reload.
func (s *Service) SetPricingEngine(pricing pluginapi.PricingEngine) {
	s.pricingMu.Lock()
	s.pricing = pricing
	s.pricingMu.Unlock()
}

func (s *Service) pricingEngine() pluginapi.PricingEngine {
	s.pricingMu.RLock()
	pricing := s.pricing
	s.pricingMu.RUnlock()
	return pricing
}

type CreatePaymentRequest struct {
	TelegramID  int64
	Email       string
	Amount      int
	PaymentType string
	Method      string
	ExternalID  string
	PlanID      *int64
	MaxDevices  int
	PromoCode   string
	Platform    string
}

// ProcessExternalPaymentStatus maps an external status and updates the payment, dispatching events if needed.
// Uses a transaction with pessimistic row locking to prevent duplicate subscription extension
// when two concurrent webhooks arrive for the same payment.
func (s *Service) ProcessExternalPaymentStatus(ctx context.Context, extID, status string) error {
	mappedStatus := status
	if status == "success" || status == "SUCCESS" || status == "CONFIRMED" || status == "COMPLETED" {
		mappedStatus = "completed"
	}

	var dispatchCompleted bool
	var paymentToSend *domain.Payment

	// Use WithTx to get a transactional registry; the payment lookup + update
	// happen atomically inside the same transaction to avoid TOCTOU.
	err := s.registry.WithTx(ctx, func(tx domain.Registry) error {
		payment, err := tx.Payments().FindByExternalID(ctx, extID)
		if err != nil {
			return err // not found or error
		}

		// Skip if already in the target status or already completed.
		if payment.Status == mappedStatus || payment.Status == "completed" {
			return nil
		}

		updated, resErr := tx.Payments().UpdateStatusIfNotCompleted(ctx, payment.ID, mappedStatus)
		if resErr != nil {
			return resErr
		}
		if !updated {
			return nil // concurrent update already processed it
		}

		s.log.Info("auto-updated payment status", "payment_id", payment.ID, "status", mappedStatus)

		if mappedStatus == "completed" {
			if payment.PlanID != nil {
				s.extendSubscriptionForPayment(ctx, tx, payment)
			} else if payment.Method != "balance" {
				if err := tx.Users().AdjustBalance(ctx, payment.UserID, payment.Amount); err != nil {
					s.log.Error("failed to credit user balance after payment completed", "payment_id", payment.ID, "err", err)
					return err
				}
			}
			dispatchCompleted = true
			paymentToSend = payment
		}
		return nil
	})

	if err != nil {
		return err
	}

	if dispatchCompleted && paymentToSend != nil {
		s.dispatcher.Dispatch("payment.completed", map[string]interface{}{
			"payment_id":   paymentToSend.ID,
			"amount":       paymentToSend.Amount,
			"payment_type": paymentToSend.PaymentType,
			"method":       paymentToSend.Method,
			"user_id":      paymentToSend.UserID,
		}, nil)
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

func (s *Service) CreatePayment(ctx context.Context, req CreatePaymentRequest) (*domain.Payment, error) {
	if req.TelegramID == 0 && req.Email == "" {
		return nil, fmt.Errorf("telegram_id or email is required")
	}
	if req.PlanID == nil && req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive if plan_id is missing")
	}
	if req.PaymentType == "" {
		return nil, fmt.Errorf("payment_type is required")
	}

	var user *domain.User
	var err error
	if req.TelegramID != 0 {
		tgIDStr := strconv.FormatInt(req.TelegramID, 10)
		user, err = s.registry.Users().FindByPlatformID(ctx, "telegram", tgIDStr)
	} else {
		user, err = s.registry.Users().FindByPlatformID(ctx, "web", req.Email)
	}
	if err != nil {
		return nil, ErrUserNotFound
	}

	var externalIDPtr *string
	if req.ExternalID != "" {
		externalIDPtr = &req.ExternalID
	}

	finalAmount, promoCodeID, err := s.calculatePaymentPrice(ctx, user.ID, req)
	if err != nil {
		return nil, err
	}

	customData := domain.Metadata{
		"telegram_id": req.TelegramID,
	}
	if req.Platform != "" {
		customData["platform"] = req.Platform
	}
	if req.MaxDevices > 0 {
		customData["max_devices"] = req.MaxDevices
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
		CustomData:  customData,
	}

	txErr := s.registry.WithTx(ctx, func(tx domain.Registry) error {
		if promoCodeID != nil {
			// Re-fetch and re-validate promo code inside the transaction to avoid TOCTOU.
			promo, err := tx.Promos().FindByID(ctx, *promoCodeID)
			if err != nil {
				return fmt.Errorf("promo not found: %w", err)
			}
			if !promo.IsActive {
				return fmt.Errorf("promo inactive")
			}
			if promo.ExpiresAt != nil && !time.Now().Before(*promo.ExpiresAt) {
				return fmt.Errorf("promo expired")
			}

			if promo.MaxUses > 0 {
				ok, err := tx.Promos().IncrementUses(ctx, *promoCodeID, int(promo.MaxUses))
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("promo limit")
				}
			} else {
				tx.Promos().IncrementUses(ctx, *promoCodeID, 0) //nolint:errcheck
			}

			count, err := tx.Payments().CountByUserAndPromo(ctx, user.ID, *promoCodeID)
			if err != nil {
				return err
			}
			if count > 0 {
				return fmt.Errorf("promo used by user")
			}
		}
		return tx.Payments().Create(ctx, &payment)
	})

	if txErr != nil {
		errMsg := txErr.Error()
		if errMsg == "promo limit" {
			return nil, ErrPromoLimitReached
		}
		if errMsg == "promo used by user" {
			return nil, ErrPromoAlreadyUsed
		}
		if strings.Contains(errMsg, "promo inactive") || strings.Contains(errMsg, "promo expired") || strings.Contains(errMsg, "promo not found") {
			return nil, fmt.Errorf("promo code is no longer valid")
		}
		return nil, fmt.Errorf("failed to create payment: %w", txErr)
	}

	return &payment, nil
}

// calculatePaymentPrice chooses the installed PricingEngine when one is
// available. The nil-engine branch intentionally retains the former
// calculation verbatim so legacy construction through NewService keeps its
// exact behaviour during the migration.
func (s *Service) calculatePaymentPrice(
	ctx context.Context,
	userID string,
	req CreatePaymentRequest,
) (int, *int64, error) {
	if pricing := s.pricingEngine(); pricing != nil {
		return s.calculatePriceWithEngine(ctx, pricing, userID, req)
	}
	return s.calculatePriceLegacy(ctx, userID, req)
}

func (s *Service) calculatePriceWithEngine(
	ctx context.Context,
	pricing pluginapi.PricingEngine,
	userID string,
	req CreatePaymentRequest,
) (int, *int64, error) {
	var plan *domain.Plan
	if req.PlanID != nil {
		var err error
		plan, err = s.registry.Plans().FindByID(ctx, fmt.Sprintf("%d", *req.PlanID))
		if err != nil {
			return 0, nil, ErrInvalidPlanID
		}
	}

	var promo *domain.PromoCode
	if req.PromoCode != "" {
		code := strings.ToUpper(strings.TrimSpace(req.PromoCode))
		// A missing promo was historically ignored at this point. Its validity
		// is checked again inside the payment transaction if it gets selected.
		promo, _ = s.registry.Promos().FindByCode(ctx, code)
	}

	var currentSub *domain.Subscription
	if plan != nil {
		// As before, no subscription simply means there is no upgrade charge.
		currentSub, _ = s.registry.Subscriptions().FindLatestByUserID(ctx, userID)
	}

	now := time.Now()
	pricingReq := pluginapi.PricingRequest{
		UserID:              userID,
		PromoCode:           req.PromoCode,
		ExtraDevices:        max(req.MaxDevices-3, 0),
		Amount:              req.Amount,
		Plan:                pricingPlanFromDomain(plan),
		CurrentSubscription: pricingSubscriptionFromDomain(currentSub),
		MaxDevices:          req.MaxDevices,
		Platform:            req.Platform,
		Promo:               pricingPromoFromDomain(promo),
		Now:                 now,
	}
	if req.PlanID != nil {
		pricingReq.PlanID = *req.PlanID
	}
	if currentSub != nil && currentSub.EndsAt != nil && currentSub.EndsAt.After(now) {
		pricingReq.IsUpgrade = req.MaxDevices > currentSub.MaxDevices
	}

	result, err := pricing.CalculatePrice(ctx, pricingReq)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to calculate payment price: %w", err)
	}
	return result.FinalPrice, result.AppliedPromoID, nil
}

// calculatePriceLegacy is the pre-plugin implementation kept as a fallback
// for callers of NewService. Do not change its decision rules independently of
// internal/plugins/pricing_default: regression tests protect their parity.
func (s *Service) calculatePriceLegacy(
	ctx context.Context,
	userID string,
	req CreatePaymentRequest,
) (int, *int64, error) {
	finalAmount := req.Amount
	var promoCodeID *int64

	if req.PlanID != nil {
		plan, err := s.registry.Plans().FindByID(ctx, fmt.Sprintf("%d", *req.PlanID))
		if err != nil {
			return 0, nil, ErrInvalidPlanID
		}

		var extraDevicesCost int
		if req.MaxDevices > 3 {
			extraDevicesCost = (req.MaxDevices - 3) * 40 * plan.Months
		}

		sub, subErr := s.registry.Subscriptions().FindLatestByUserID(ctx, userID)
		if subErr == nil && sub != nil && sub.EndsAt != nil && sub.EndsAt.After(time.Now()) && req.MaxDevices > sub.MaxDevices {
			remainingDuration := sub.EndsAt.Sub(time.Now())
			remainingDays := float64(remainingDuration.Hours() / 24.0)
			if remainingDays > 7 {
				upgradeMonths := int(math.Ceil(remainingDays / 30.0))
				currentDevices := sub.MaxDevices
				if currentDevices < 3 {
					currentDevices = 3
				}
				newExtraDevices := req.MaxDevices - currentDevices
				if newExtraDevices > 0 {
					extraDevicesCost += newExtraDevices * 40 * upgradeMonths
				}
			}
		}

		baseAmount := plan.BasePrice + extraDevicesCost

		globalPrice := baseAmount
		if plan.GlobalDiscountPercent > 0 {
			globalPrice = baseAmount - (baseAmount * plan.GlobalDiscountPercent / 100)
		}

		promoPrice := baseAmount
		if req.PromoCode != "" {
			code := strings.ToUpper(strings.TrimSpace(req.PromoCode))
			promo, err := s.registry.Promos().FindByCode(ctx, code)
			if err == nil {
				if promo.IsActive && (promo.ExpiresAt == nil || time.Now().Before(*promo.ExpiresAt)) {
					platform := strings.ToLower(strings.TrimSpace(req.Platform))
					if promo.TargetPlatform == "all" || promo.TargetPlatform == platform {
						promoPrice = baseAmount - (baseAmount * promo.DiscountPercent / 100)
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
		promo, err := s.registry.Promos().FindByCode(ctx, code)
		if err == nil {
			if promo.IsActive && (promo.ExpiresAt == nil || time.Now().Before(*promo.ExpiresAt)) {
				platform := strings.ToLower(strings.TrimSpace(req.Platform))
				if promo.TargetPlatform == "all" || promo.TargetPlatform == platform {
					promoCodeID = &promo.ID
				}
			}
		}
	}

	return finalAmount, promoCodeID, nil
}

func pricingPlanFromDomain(plan *domain.Plan) *pluginapi.Plan {
	if plan == nil {
		return nil
	}
	return &pluginapi.Plan{
		ID:                    plan.ID,
		Months:                plan.Months,
		BasePrice:             plan.BasePrice,
		GlobalDiscountPercent: plan.GlobalDiscountPercent,
		IsActive:              plan.IsActive,
		CreatedAt:             plan.CreatedAt,
		UpdatedAt:             plan.UpdatedAt,
	}
}

func pricingPromoFromDomain(promo *domain.PromoCode) *pluginapi.PromoCode {
	if promo == nil {
		return nil
	}
	return &pluginapi.PromoCode{
		ID:              promo.ID,
		Code:            promo.Code,
		DiscountPercent: promo.DiscountPercent,
		MaxUses:         promo.MaxUses,
		UsesCount:       promo.UsesCount,
		TargetPlatform:  promo.TargetPlatform,
		ExpiresAt:       promo.ExpiresAt,
		IsActive:        promo.IsActive,
		CreatedAt:       promo.CreatedAt,
		UpdatedAt:       promo.UpdatedAt,
	}
}

func pricingSubscriptionFromDomain(sub *domain.Subscription) *pluginapi.Subscription {
	if sub == nil {
		return nil
	}
	metadata := make(map[string]any, len(sub.Metadata))
	for key, value := range sub.Metadata {
		metadata[key] = value
	}
	return &pluginapi.Subscription{
		ID:         sub.ID,
		UserID:     sub.UserID,
		Email:      sub.Email,
		UUID:       sub.UUID,
		Status:     sub.Status,
		MaxDevices: sub.MaxDevices,
		StartsAt:   sub.StartsAt,
		EndsAt:     sub.EndsAt,
		AutoRenew:  sub.AutoRenew,
		Metadata:   metadata,
		CreatedAt:  sub.CreatedAt,
		UpdatedAt:  sub.UpdatedAt,
	}
}

// UpdateExternalID updates the external reference ID for a payment.
func (s *Service) UpdateExternalID(ctx context.Context, paymentID int64, extID string) error {
	return s.registry.WithTx(ctx, func(tx domain.Registry) error {
		p, err := tx.Payments().FindByID(ctx, fmt.Sprintf("%d", paymentID))
		if err != nil {
			return err
		}
		p.ExternalID = &extID
		return tx.Payments().Update(ctx, p)
	})
}

// FindPaymentByID returns a single payment by ID.
func (s *Service) FindPaymentByID(ctx context.Context, idStr string) (*domain.Payment, error) {
	return s.registry.Payments().FindByID(ctx, idStr)
}

// FindPaymentByExternalID returns a single payment by its external ID.
func (s *Service) FindPaymentByExternalID(ctx context.Context, extID string) (*domain.Payment, error) {
	return s.registry.Payments().FindByExternalID(ctx, extID)
}


// UpdatePaymentStatus updates the status of a payment.
func (s *Service) UpdatePaymentStatus(ctx context.Context, paymentID int64, status string, expectedStatuses []string) (bool, error) {
	var updated bool
	var dispatchCompleted bool
	var paymentToSend *domain.Payment

	err := s.registry.WithTx(ctx, func(tx domain.Registry) error {
		var err error
		updated, err = tx.Payments().UpdateStatus(ctx, paymentID, status, expectedStatuses)
		if err != nil {
			return err
		}
		if !updated {
			return nil
		}

		if status == "completed" {
			payment, err := tx.Payments().FindByID(ctx, fmt.Sprintf("%d", paymentID))
			if err == nil {
				if payment.PlanID != nil {
					s.extendSubscriptionForPayment(ctx, tx, payment)
				} else if payment.Method != "balance" {
					if err := tx.Users().AdjustBalance(ctx, payment.UserID, payment.Amount); err != nil {
						s.log.Error("failed to credit user balance after payment status update", "payment_id", payment.ID, "err", err)
						return err
					}
				}

				dispatchCompleted = true
				paymentToSend = payment
			}
		}
		return nil
	})

	if err != nil {
		return false, err
	}
	if !updated {
		return false, nil
	}

	if dispatchCompleted && paymentToSend != nil {
		s.dispatcher.Dispatch("payment.completed", map[string]interface{}{
			"payment_id":   paymentToSend.ID,
			"amount":       paymentToSend.Amount,
			"payment_type": paymentToSend.PaymentType,
			"method":       paymentToSend.Method,
			"user_id":      paymentToSend.UserID,
		}, nil)
	}
	return true, nil
}

func (s *Service) extendSubscriptionForPayment(ctx context.Context, registry domain.Registry, payment *domain.Payment) {
	if payment.PlanID == nil {
		return
	}
	maxDevices := 3
	if currentSub, subErr := registry.Subscriptions().FindLatestByUserID(ctx, payment.UserID); subErr == nil && currentSub != nil {
		maxDevices = currentSub.MaxDevices
	}

	if payment.CustomData != nil {
		if md, ok := payment.CustomData["max_devices"]; ok {
			if floatVal, ok := md.(float64); ok {
				maxDevices = int(floatVal)
			} else if intVal, ok := md.(int); ok {
				maxDevices = intVal
			}
		}
	}

	if maxDevices < 3 {
		maxDevices = 3
	}

	if err := registry.Subscriptions().AutoRenewSubscription(ctx, payment.UserID, payment.PlanID, 0, nil, maxDevices); err != nil {
		s.log.Error("failed to extend subscription after payment completed", "payment_id", payment.ID, "err", err)
	}
}

