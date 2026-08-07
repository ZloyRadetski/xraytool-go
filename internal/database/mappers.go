package database

import "xraytool/internal/domain"

func (u User) ToDomain() domain.User {
	return domain.User{
		ID:         u.ID,
		Username:   u.Username,
		Balance:    u.Balance,
		IsAdmin:    u.IsAdmin,
		RefCode:    u.RefCode,
		ReferredBy: u.ReferredBy,
		Metadata:   domain.Metadata(u.Metadata),
		IsBlocked:  u.IsBlocked,
		CreatedAt:  u.CreatedAt,
	}
}

func FromDomainUser(u domain.User) User {
	return User{
		ID:         u.ID,
		Username:   u.Username,
		Balance:    u.Balance,
		IsAdmin:    u.IsAdmin,
		RefCode:    u.RefCode,
		ReferredBy: u.ReferredBy,
		Metadata:   Metadata(u.Metadata),
		IsBlocked:  u.IsBlocked,
		CreatedAt:  u.CreatedAt,
	}
}

func (s Subscription) ToDomain() domain.Subscription {
	return domain.Subscription{
		ID:         s.ID,
		UserID:     s.UserID,
		Email:      s.Email,
		UUID:       s.UUID,
		Status:     s.Status,
		MaxDevices: s.MaxDevices,
		StartsAt:   s.StartsAt,
		EndsAt:     s.EndsAt,
		AutoRenew:  s.AutoRenew,
		Metadata:   domain.Metadata(s.Metadata),
		CreatedAt:  s.CreatedAt,
		UpdatedAt:  s.UpdatedAt,
	}
}

func FromDomainSubscription(s domain.Subscription) Subscription {
	return Subscription{
		ID:         s.ID,
		UserID:     s.UserID,
		Email:      s.Email,
		UUID:       s.UUID,
		Status:     s.Status,
		MaxDevices: s.MaxDevices,
		StartsAt:   s.StartsAt,
		EndsAt:     s.EndsAt,
		AutoRenew:  s.AutoRenew,
		Metadata:   Metadata(s.Metadata),
		CreatedAt:  s.CreatedAt,
		UpdatedAt:  s.UpdatedAt,
	}
}

func (d Device) ToDomain() domain.Device {
	return domain.Device{
		ID:             d.ID,
		SubscriptionID: d.SubscriptionID,
		HWID:           d.HWID,
		DeviceModel:    d.DeviceModel,
		DeviceOS:       d.DeviceOS,
		UserAgent:      d.UserAgent,
	}
}

func FromDomainDevice(d domain.Device) Device {
	return Device{
		ID:             d.ID,
		SubscriptionID: d.SubscriptionID,
		HWID:           d.HWID,
		DeviceModel:    d.DeviceModel,
		DeviceOS:       d.DeviceOS,
		UserAgent:      d.UserAgent,
	}
}

func (p Payment) ToDomain() domain.Payment {
	return domain.Payment{
		ID:          p.ID,
		UserID:      p.UserID,
		Amount:      p.Amount,
		Status:      p.Status,
		PaymentType: p.PaymentType,
		Method:      p.Method,
		ExternalID:  p.ExternalID,
		CustomData:  domain.Metadata(p.CustomData),
		PlanID:      p.PlanID,
		PromoCodeID: p.PromoCodeID,
		CreatedAt:   p.CreatedAt,
	}
}

func FromDomainPayment(p domain.Payment) Payment {
	return Payment{
		ID:          p.ID,
		UserID:      p.UserID,
		Amount:      p.Amount,
		Status:      p.Status,
		PaymentType: p.PaymentType,
		Method:      p.Method,
		ExternalID:  p.ExternalID,
		CustomData:  Metadata(p.CustomData),
		PlanID:      p.PlanID,
		PromoCodeID: p.PromoCodeID,
		CreatedAt:   p.CreatedAt,
	}
}

func (p Plan) ToDomain() domain.Plan {
	return domain.Plan{
		ID:                    p.ID,
		Months:                p.Months,
		BasePrice:             p.BasePrice,
		GlobalDiscountPercent: p.GlobalDiscountPercent,
		EngineIDs:             append([]string(nil), p.EngineIDs...),
		IsActive:              p.IsActive,
		CreatedAt:             p.CreatedAt,
		UpdatedAt:             p.UpdatedAt,
	}
}

func FromDomainPlan(p domain.Plan) Plan {
	return Plan{
		ID:                    p.ID,
		Months:                p.Months,
		BasePrice:             p.BasePrice,
		GlobalDiscountPercent: p.GlobalDiscountPercent,
		EngineIDs:             append([]string(nil), p.EngineIDs...),
		IsActive:              p.IsActive,
		CreatedAt:             p.CreatedAt,
		UpdatedAt:             p.UpdatedAt,
	}
}

func (p PromoCode) ToDomain() domain.PromoCode {
	return domain.PromoCode{
		ID:              p.ID,
		Code:            p.Code,
		DiscountPercent: p.DiscountPercent,
		MaxUses:         p.MaxUses,
		UsesCount:       p.UsesCount,
		TargetPlatform:  p.TargetPlatform,
		ExpiresAt:       p.ExpiresAt,
		IsActive:        p.IsActive,
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
	}
}

func FromDomainPromoCode(p domain.PromoCode) PromoCode {
	return PromoCode{
		ID:              p.ID,
		Code:            p.Code,
		DiscountPercent: p.DiscountPercent,
		MaxUses:         p.MaxUses,
		UsesCount:       p.UsesCount,
		TargetPlatform:  p.TargetPlatform,
		ExpiresAt:       p.ExpiresAt,
		IsActive:        p.IsActive,
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
	}
}

func (r ReferralReward) ToDomain() domain.ReferralReward {
	return domain.ReferralReward{
		ID:         r.ID,
		ReferrerID: r.ReferrerID,
		ReferredID: r.ReferredID,
		PaymentID:  r.PaymentID,
		Amount:     r.Amount,
		CreatedAt:  r.CreatedAt,
	}
}

func FromDomainReferralReward(r domain.ReferralReward) ReferralReward {
	return ReferralReward{
		ID:         r.ID,
		ReferrerID: r.ReferrerID,
		ReferredID: r.ReferredID,
		PaymentID:  r.PaymentID,
		Amount:     r.Amount,
		CreatedAt:  r.CreatedAt,
	}
}

func (s SubscriptionNotification) ToDomain() domain.SubscriptionNotification {
	return domain.SubscriptionNotification{
		SubscriptionID: s.SubscriptionID,
		WarningLevel:   s.WarningLevel,
		SentAt:         s.SentAt,
	}
}

func FromDomainSubscriptionNotification(s domain.SubscriptionNotification) SubscriptionNotification {
	return SubscriptionNotification{
		SubscriptionID: s.SubscriptionID,
		WarningLevel:   s.WarningLevel,
		SentAt:         s.SentAt,
	}
}

func (a AntifraudBan) ToDomain() domain.AntifraudBan {
	return domain.AntifraudBan{
		ID:        a.ID,
		Email:     a.Email,
		BannedAt:  a.BannedAt,
		ExpiresAt: a.ExpiresAt,
		Reason:    a.Reason,
	}
}

func FromDomainAntifraudBan(a domain.AntifraudBan) AntifraudBan {
	return AntifraudBan{
		ID:        a.ID,
		Email:     a.Email,
		BannedAt:  a.BannedAt,
		ExpiresAt: a.ExpiresAt,
		Reason:    a.Reason,
	}
}
