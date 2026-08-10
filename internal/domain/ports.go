package domain

import (
	"context"
	"time"
)

// Registry is the driven port for the Unit of Work / Repository registry.
// Business logic depends only on this interface. Infrastructure adapters
// (e.g. database.gormRegistry) implement it.
type Registry interface {
	Users() UserRepository
	Subscriptions() SubscriptionRepository
	Payments() PaymentRepository
	Plans() PlanRepository
	Promos() PromoRepository
	AntifraudBans() AntifraudBanRepository
	Devices() DeviceRepository
	Notifications() SubscriptionNotificationRepository

	// WithTx runs the given function within a database transaction.
	WithTx(ctx context.Context, fn func(tx Registry) error) error
}

// UserRepository is the driven port for user persistence.
type UserRepository interface {
	FindByID(ctx context.Context, id string) (*User, error)
	FindByEmailOrUsername(ctx context.Context, email string) (*User, error)
	FindByTelegramID(ctx context.Context, tgID int64) (*User, error)
	FindByRefCode(ctx context.Context, code string) (*User, error)
	Create(ctx context.Context, user *User) error
	Update(ctx context.Context, user *User) error
	Delete(ctx context.Context, id string) error

	// Complex queries
	FindAll(ctx context.Context) ([]User, error)
	ListUsers(ctx context.Context, page, limit int, search string) ([]User, int64, error)
	DeleteUserAndData(ctx context.Context, userID string) error
	FindByPlatformID(ctx context.Context, platform, id string) (*User, error)
	AddReferralReward(ctx context.Context, referrerID string, referredID string, paymentID int64, reward int) error
	CountReferrals(ctx context.Context, referrerID string) (int64, error)
	CountReferralRewards(ctx context.Context, referrerID string) (int64, error)
	SumReferralRewards(ctx context.Context, referrerID string) (int64, error)
	GetReferralStats(ctx context.Context, referrerIDs []string) ([]ReferralStats, error)
	CountByRefCode(ctx context.Context, code string) (int64, error)
	FindAdmins(ctx context.Context) ([]User, error)
	AdjustBalance(ctx context.Context, userID string, amount int) error
	UpdateIsBlocked(ctx context.Context, userID string, isBlocked bool) error
}

// ReferralStats holds referral aggregation data for a single referrer.
type ReferralStats struct {
	ReferrerID string
	Count      int64
	Total      int64
}

// SubscriptionRepository is the driven port for subscription persistence.
type SubscriptionRepository interface {
	FindByID(ctx context.Context, id string) (*Subscription, error)
	FindByEmail(ctx context.Context, email string) (*Subscription, error)
	FindByUserID(ctx context.Context, userID string) ([]Subscription, error)
	Create(ctx context.Context, sub *Subscription) error
	Update(ctx context.Context, sub *Subscription) error
	Delete(ctx context.Context, id string) error

	// Complex queries
	FindAll(ctx context.Context) ([]Subscription, error)
	FindByStatus(ctx context.Context, status string) ([]Subscription, error)
	GetMasterSnapshot(ctx context.Context) ([]Subscription, error)
	FindByClientIdentifier(ctx context.Context, clientId string) (*Subscription, error)
	FindLatestByEmail(ctx context.Context, email string) (*Subscription, error)
	UpdateStatusIfActive(ctx context.Context, id string, newStatus string) (bool, error)
	UpdateFields(ctx context.Context, id string, updates map[string]interface{}) error
	FindLatestByUserID(ctx context.Context, userID string) (*Subscription, error)
	FindLatestByUserIDs(ctx context.Context, userIDs []string) ([]Subscription, error)
	UpdateMaxDevicesByUserID(ctx context.Context, userID string, maxDevices int) error
	UpdateAutoRenewByUserID(ctx context.Context, userID string, autoRenew bool) error
	AutoRenewSubscription(ctx context.Context, userID string, planID *int64, planTotalPrice int, newEndsAt *time.Time, maxDevices int) error
	GetAllEmailsAndMaxDevices(ctx context.Context) ([]EmailAndMaxDevice, error)
}

// EmailAndMaxDevice is a projection used by the antifraud device-limit cache.
type EmailAndMaxDevice struct {
	Email      string
	MaxDevices int
}

// PaymentRepository is the driven port for payment persistence.
type PaymentRepository interface {
	FindByID(ctx context.Context, id string) (*Payment, error)
	FindByUserID(ctx context.Context, userID string) ([]Payment, error)
	Create(ctx context.Context, p *Payment) error
	Update(ctx context.Context, p *Payment) error

	// Complex queries
	FindAll(ctx context.Context) ([]Payment, error)
	FindPendingByProvider(ctx context.Context, provider string) ([]Payment, error)
	FindPaymentsByFilters(ctx context.Context, status, method, paymentType, telegramIDStr string) ([]Payment, error)
	UpdateStatus(ctx context.Context, paymentID int64, newStatus string, expectedStatuses []string) (bool, error)
	UpdateStatusIfNotCompleted(ctx context.Context, paymentID int64, newStatus string) (bool, error)
	FindByExternalID(ctx context.Context, extID string) (*Payment, error)
	CountByPromoAndUser(ctx context.Context, promoID int64, userID string, status string) (int64, error)
	CountByUserAndPromo(ctx context.Context, userID string, promoID int64) (int64, error)
	ScrubOldExternalIDs(ctx context.Context, cutoff time.Time) (int64, error)
}

// PlanRepository is the driven port for plan persistence.
type PlanRepository interface {
	FindByID(ctx context.Context, id string) (*Plan, error)
	FindAll(ctx context.Context) ([]Plan, error)
	FindActive(ctx context.Context) ([]Plan, error)
	Create(ctx context.Context, plan *Plan) error
	Update(ctx context.Context, plan *Plan) error
	Delete(ctx context.Context, id string) error
}

// PromoRepository is the driven port for promo code persistence.
type PromoRepository interface {
	FindByCode(ctx context.Context, code string) (*PromoCode, error)
	Create(ctx context.Context, promo *PromoCode) error
	Update(ctx context.Context, promo *PromoCode) error
	IncrementUses(ctx context.Context, id int64, maxUses int) (bool, error)
	FindAll(ctx context.Context) ([]PromoCode, error)
	Delete(ctx context.Context, id int64) (int64, error)
	FindByID(ctx context.Context, id int64) (*PromoCode, error)
}

// DeviceRepository is the driven port for device persistence.
type DeviceRepository interface {
	TrackDevice(ctx context.Context, subID string, hwid, deviceModel, deviceOs, userAgent string, deviceLimit int) (deviceLimitReached bool, err error)
	CountBySubscriptions(ctx context.Context, subIDs []string) (map[string]int64, error)
	FindOldestBySubscription(ctx context.Context, subID string, limit int) ([]Device, error)
	DeleteByIDs(ctx context.Context, ids []int64) error
	CountBySubscription(ctx context.Context, subID string) (int64, error)
	FindBySubscriptionID(ctx context.Context, subID string) ([]Device, error)
	FindByIDAndSubscription(ctx context.Context, deviceID int64, subID string) (*Device, error)
	Delete(ctx context.Context, id int64) error
}

// AntifraudBanRepository is the driven port for antifraud ban persistence.
type AntifraudBanRepository interface {
	FindByEmail(ctx context.Context, email string) (*AntifraudBan, error)
	Create(ctx context.Context, ban *AntifraudBan) error
	DeleteByEmail(ctx context.Context, email string) error
	FindAll(ctx context.Context) ([]AntifraudBan, error)
	FindActive(ctx context.Context) ([]AntifraudBan, error)
	FindExpired(ctx context.Context) ([]AntifraudBan, error)
	Upsert(ctx context.Context, ban *AntifraudBan) error
}

// SubscriptionNotificationRepository is the driven port for notification persistence.
type SubscriptionNotificationRepository interface {
	CreateIfNotExists(ctx context.Context, notif *SubscriptionNotification) (bool, error)
	DeleteBySubscriptionID(ctx context.Context, subID string) error
}

// EventPropagator is the driven port for notifying external systems (e.g. slave nodes)
// about user lifecycle events (creation, update, removal).
type EventPropagator interface {
	PropagateAll(event string, payload map[string]string)
}

type FraudEvent struct {
	Email string
	IP    string
}

// FraudEventReporter is the driven port used by slave nodes to report
// suspicious IP events to the master node.
type FraudEventReporter interface {
	Report(events []FraudEvent) error
}

type SlaveUserTotal struct {
	Email string
	Slave int64
}

type SlaveReport struct {
	Enabled       bool
	TotalServers  int
	OKServers     int
	FailedServers int
}

// ClusterStatsProvider is the driven port for collecting stats from cluster nodes.
type ClusterStatsProvider interface {
	CollectSlaveTotals() ([]SlaveUserTotal, SlaveReport)
}
