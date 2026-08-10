package domain

import (
	"strconv"
	"time"
)

// Metadata is a flexible JSON field for platform-specific user/subscription data.
type Metadata map[string]interface{}

// ExtractTelegramID extracts the numeric telegram_id from Metadata if present.
func ExtractTelegramID(metadata Metadata) int64 {
	if metadata == nil {
		return 0
	}
	switch v := metadata["telegram_id"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		parsed, _ := strconv.ParseInt(v, 10, 64)
		return parsed
	default:
		return 0
	}
}

// User represents an xraytool end-user, originating from any platform.
type User struct {
	ID         string
	Username   string
	Balance    int
	IsAdmin    bool
	RefCode    string
	ReferredBy *string
	Metadata   Metadata
	IsBlocked  bool
	CreatedAt  time.Time
}

// Subscription represents an active or historical VPN subscription tied to a User.
type Subscription struct {
	ID         string
	UserID     string
	Email      string
	UUID       string
	Status     string
	MaxDevices int
	StartsAt   *time.Time
	EndsAt     *time.Time
	AutoRenew  bool
	Metadata   Metadata
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Device tracks individual client devices that have accessed a subscription.
type Device struct {
	ID             int64
	SubscriptionID string
	HWID           string
	DeviceModel    string
	DeviceOS       string
	UserAgent      string
}

// Payment records a financial transaction for a user.
type Payment struct {
	ID          int64
	UserID      string
	Amount      int
	Status      string
	PaymentType string
	Method      string
	ExternalID  *string
	CustomData  Metadata
	PlanID      *int64
	PromoCodeID *int64
	CreatedAt   time.Time
}

// Plan represents a subscription plan.
type Plan struct {
	ID                    int64
	Months                int
	BasePrice             int
	GlobalDiscountPercent int
	// EngineIDs optionally pins subscriptions purchased under this plan to a
	// subset of enabled engines when engines.routing_mode is by-plan.
	EngineIDs []string
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PromoCode represents a discount code that can be used by users.
type PromoCode struct {
	ID              int64
	Code            string
	DiscountPercent int
	MaxUses         int
	UsesCount       int
	TargetPlatform  string
	ExpiresAt       *time.Time
	IsActive        bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ReferralReward records a credit award granted to a referrer when a referred user pays.
type ReferralReward struct {
	ID         int64
	ReferrerID string
	ReferredID string
	PaymentID  int64
	Amount     int
	CreatedAt  time.Time
}

// SubscriptionNotification records that a specific webhook warning was sent for a subscription.
type SubscriptionNotification struct {
	SubscriptionID string
	WarningLevel   string
	SentAt         time.Time
}

// AntifraudBan records a temporary soft-ban imposed by the anti-fraud system.
type AntifraudBan struct {
	ID        int64
	Email     string
	BannedAt  time.Time
	ExpiresAt time.Time
	Reason    string
}
