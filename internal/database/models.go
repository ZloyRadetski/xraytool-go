// Package database provides the data layer for xraytool: models, DB init, and migrations.
package database

import "time"

// Metadata is a flexible JSON field for platform-specific user/subscription data.
//
// For Telegram: {"telegram_id": 12345, "telegram_username": "radetski", "source": "telegram_bot"}
// For Web:      {"email": "user@example.com", "password_hash": "...", "source": "website"}
//
// GORM serializes this as JSONB in Postgres and TEXT in SQLite via the "serializer:json" tag.
type Metadata map[string]interface{}

// User represents an xraytool end-user, originating from any platform (Telegram, web, etc.).
type User struct {
	// ID is a UUID v4 string, set by the application before insert.
	ID string `gorm:"type:text;primaryKey"`
	// Username is the display name (Telegram username, web login, etc.).
	Username string `gorm:"type:text;not null"`
	// Balance is the internal credit balance in integer units (e.g. kopecks or days).
	Balance int `gorm:"default:0;not null"`
	// IsAdmin grants administrative privileges.
	IsAdmin bool `gorm:"default:false;not null"`
	// RefCode is the user's own referral code, used to invite others.
	RefCode string `gorm:"type:text;uniqueIndex"`
	// ReferredBy is the ID of the user who referred this user (nullable FK → User.ID).
	ReferredBy *string `gorm:"type:text;index"`
	// Metadata stores platform-specific data as a JSON object.
	Metadata  Metadata `gorm:"serializer:json"`
	// IsBlocked indicates if the user is globally banned.
	IsBlocked bool `gorm:"default:false;not null"`
	CreatedAt time.Time
}

// Subscription represents an active or historical VPN subscription tied to a User.
type Subscription struct {
	// ID is a UUID v4 string.
	ID string `gorm:"type:text;primaryKey"`
	// UserID is the owning user's UUID (FK → User.ID).
	UserID string `gorm:"type:text;not null;index"`
	// Email is the xray client email identifier, e.g. "bot_client_123456".
	// Must be unique across all subscriptions.
	Email string `gorm:"type:text;not null;uniqueIndex"`
	// XrayUUID is the xray client UUID used in the inbound config.
	XrayUUID string `gorm:"type:text;not null;uniqueIndex"`
	// Status is the subscription lifecycle state: "active", "inactive", "expired", etc.
	Status string `gorm:"type:text;not null;default:'inactive';index"`
	// MaxDevices is how many devices can simultaneously use this subscription.
	MaxDevices int `gorm:"default:3;not null"`
	// StartsAt and EndsAt define the validity window (nullable for indefinite).
	StartsAt *time.Time
	EndsAt   *time.Time `gorm:"index"`
	// AutoRenew indicates whether to automatically extend the subscription.
	AutoRenew bool `gorm:"default:false;not null"`
	// Metadata stores extra data (plan name, coupon, etc.).
	Metadata  Metadata `gorm:"serializer:json"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Device tracks individual client devices that have accessed a subscription.
type Device struct {
	// ID is an auto-increment integer primary key.
	ID int64 `gorm:"primaryKey;autoIncrement"`
	// SubscriptionID links this device to a Subscription.
	SubscriptionID string `gorm:"type:text;not null;index"`
	// HWID is a hardware fingerprint hash supplied by the client.
	HWID        string `gorm:"type:text;not null;index"`
	DeviceModel string `gorm:"type:text"`
	DeviceOS    string `gorm:"type:text"`
	// VerOS is the OS version string.
	VerOS     string `gorm:"type:text"`
	UserAgent string `gorm:"type:text"`
	// RequestCount is incremented each time this device fetches a subscription.
	RequestCount int `gorm:"default:1;not null"`
	FirstSeen    time.Time
	LastSeen     time.Time
}

// Payment records a financial transaction for a user.
type Payment struct {
	// ID is an auto-increment integer primary key.
	ID int64 `gorm:"primaryKey;autoIncrement"`
	// UserID is the payer's UUID (FK → User.ID).
	UserID string `gorm:"type:text;not null;index"`
	// Amount is the payment value in integer units (kopecks, etc.).
	Amount int `gorm:"not null"`
	// Status is the payment state: "pending_card", "completed", "canceled", etc.
	Status string `gorm:"type:text;not null"`
	// PaymentType describes what was purchased: "subscription", "device_slot", etc.
	PaymentType string `gorm:"type:text;not null"`
	// Method is the payment provider/method: "platega", "cash", "transfer", "sbp".
	Method string `gorm:"type:text"`
	// ExternalID is the provider's transaction reference (unique, nullable for manual payments).
	// Using *string (pointer) so that multiple manual payments without an ExternalID
	// can coexist — NULL values are never considered equal in a unique index.
	ExternalID *string `gorm:"type:text;uniqueIndex"`
	// CustomData stores provider-specific response fields.
	CustomData Metadata `gorm:"serializer:json"`
	// PlanID is the ID of the plan purchased (if any).
	PlanID *int64 `gorm:"index"`
	// PromoCodeID is the ID of the promo code used (if any).
	PromoCodeID *int64 `gorm:"index"`
	CreatedAt  time.Time
}

// Plan represents a subscription plan.
type Plan struct {
	ID                    int64     `gorm:"primaryKey;autoIncrement"`
	Months                int       `gorm:"uniqueIndex;not null"`
	BasePrice             int       `gorm:"not null"`
	GlobalDiscountPercent int       `gorm:"default:0;not null"`
	IsActive              bool      `gorm:"default:true;not null"`
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// PromoCode represents a discount code that can be used by users.
type PromoCode struct {
	ID              int64      `gorm:"primaryKey;autoIncrement"`
	Code            string     `gorm:"type:text;uniqueIndex;not null"`
	DiscountPercent int        `gorm:"not null"`
	MaxUses         int        `gorm:"default:0;not null"` // 0 = unlimited
	UsesCount       int        `gorm:"default:0;not null"`
	TargetPlatform  string     `gorm:"type:text;not null;default:'all'"` // 'all', 'bot', 'web'
	ExpiresAt       *time.Time `gorm:"index"`
	IsActive        bool       `gorm:"default:true;not null"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ReferralReward records a credit award granted to a referrer when a referred user pays.
type ReferralReward struct {
	// ID is an auto-increment integer primary key.
	ID int64 `gorm:"primaryKey;autoIncrement"`
	// ReferrerID is the UUID of the user who made the referral.
	ReferrerID string `gorm:"type:text;not null;index"`
	// ReferredID is the UUID of the user who was referred.
	ReferredID string `gorm:"type:text;not null;index"`
	// PaymentID is the triggering payment's ID.
	PaymentID int64 `gorm:"not null;uniqueIndex"`
	// Amount is the reward value credited to ReferrerID.
	Amount int `gorm:"not null"`
	// Date of actual issuance/award.
	CreatedAt time.Time
}

// SubscriptionNotification records that a specific webhook warning was sent for a subscription.
type SubscriptionNotification struct {
	SubscriptionID string `gorm:"primaryKey;type:text"`
	WarningLevel   string `gorm:"primaryKey;type:text"`
	SentAt         time.Time `gorm:"autoCreateTime"`
}

// AntifraudBan records a temporary soft-ban imposed by the anti-fraud system.
// The ban lives only in Xray memory — the user is removed from Xray runtime but
// NOT from xrayconfig.json on disk. When ExpiresAt passes, the Unban Cleaner
// verifies the subscription is still active in the DB before re-adding the user.
//
// Index on Email: fast lookup during subscription serving and syncstates.
// Index on ExpiresAt: efficient range query by the Unban Cleaner ticker.
type AntifraudBan struct {
	ID        int64     `gorm:"primaryKey;autoIncrement"`
	Email     string    `gorm:"type:text;not null;uniqueIndex"`
	BannedAt  time.Time `gorm:"not null"`
	ExpiresAt time.Time `gorm:"not null;index"`
	// Reason contains a human-readable description, e.g. "5 unique IPs in 3m (limit 3)".
	Reason string `gorm:"type:text"`
}
