package core

import (
	"context"
	"time"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

// The adapters in this file are the compatibility boundary between the legacy
// domain ports and the plugin API. They are intentionally mechanical: no
// business rule lives here, and domain objects are only translated at the
// plugin boundary.

type userRepository struct{ repo domain.UserRepository }

func (r userRepository) FindByID(ctx context.Context, id string) (*pluginapi.User, error) {
	u, err := r.repo.FindByID(ctx, id)
	return fromDomainUser(u), err
}

func (r userRepository) FindByEmailOrUsername(ctx context.Context, email string) (*pluginapi.User, error) {
	u, err := r.repo.FindByEmailOrUsername(ctx, email)
	return fromDomainUser(u), err
}

func (r userRepository) FindByTelegramID(ctx context.Context, telegramID int64) (*pluginapi.User, error) {
	u, err := r.repo.FindByTelegramID(ctx, telegramID)
	return fromDomainUser(u), err
}

func (r userRepository) FindByRefCode(ctx context.Context, code string) (*pluginapi.User, error) {
	u, err := r.repo.FindByRefCode(ctx, code)
	return fromDomainUser(u), err
}

func (r userRepository) Create(ctx context.Context, user *pluginapi.User) error {
	return r.repo.Create(ctx, toDomainUser(user))
}

func (r userRepository) Update(ctx context.Context, user *pluginapi.User) error {
	return r.repo.Update(ctx, toDomainUser(user))
}

func (r userRepository) Delete(ctx context.Context, id string) error {
	return r.repo.Delete(ctx, id)
}

func (r userRepository) FindAll(ctx context.Context) ([]pluginapi.User, error) {
	users, err := r.repo.FindAll(ctx)
	return fromDomainUsers(users), err
}

func (r userRepository) ListUsers(ctx context.Context, page, limit int, search string) ([]pluginapi.User, int64, error) {
	users, total, err := r.repo.ListUsers(ctx, page, limit, search)
	return fromDomainUsers(users), total, err
}

func (r userRepository) DeleteUserAndData(ctx context.Context, userID string) error {
	return r.repo.DeleteUserAndData(ctx, userID)
}

func (r userRepository) FindByPlatformID(ctx context.Context, platform, id string) (*pluginapi.User, error) {
	u, err := r.repo.FindByPlatformID(ctx, platform, id)
	return fromDomainUser(u), err
}

func (r userRepository) AddReferralReward(ctx context.Context, referrerID, referredID string, paymentID int64, reward int) error {
	return r.repo.AddReferralReward(ctx, referrerID, referredID, paymentID, reward)
}

func (r userRepository) CountReferrals(ctx context.Context, referrerID string) (int64, error) {
	return r.repo.CountReferrals(ctx, referrerID)
}

func (r userRepository) CountReferralRewards(ctx context.Context, referrerID string) (int64, error) {
	return r.repo.CountReferralRewards(ctx, referrerID)
}

func (r userRepository) SumReferralRewards(ctx context.Context, referrerID string) (int64, error) {
	return r.repo.SumReferralRewards(ctx, referrerID)
}

func (r userRepository) GetReferralStats(ctx context.Context, referrerIDs []string) ([]pluginapi.ReferralStats, error) {
	stats, err := r.repo.GetReferralStats(ctx, referrerIDs)
	if err != nil {
		return nil, err
	}
	result := make([]pluginapi.ReferralStats, len(stats))
	for i, stat := range stats {
		result[i] = pluginapi.ReferralStats{ReferrerID: stat.ReferrerID, Count: stat.Count, Total: stat.Total}
	}
	return result, nil
}

func (r userRepository) CountByRefCode(ctx context.Context, code string) (int64, error) {
	return r.repo.CountByRefCode(ctx, code)
}

func (r userRepository) FindAdmins(ctx context.Context) ([]pluginapi.User, error) {
	users, err := r.repo.FindAdmins(ctx)
	return fromDomainUsers(users), err
}

func (r userRepository) AdjustBalance(ctx context.Context, userID string, amount int) error {
	return r.repo.AdjustBalance(ctx, userID, amount)
}

func (r userRepository) UpdateIsBlocked(ctx context.Context, userID string, isBlocked bool) error {
	return r.repo.UpdateIsBlocked(ctx, userID, isBlocked)
}

type subscriptionRepository struct{ repo domain.SubscriptionRepository }

func (r subscriptionRepository) FindByID(ctx context.Context, id string) (*pluginapi.Subscription, error) {
	sub, err := r.repo.FindByID(ctx, id)
	return fromDomainSubscription(sub), err
}

func (r subscriptionRepository) FindByEmail(ctx context.Context, email string) (*pluginapi.Subscription, error) {
	sub, err := r.repo.FindByEmail(ctx, email)
	return fromDomainSubscription(sub), err
}

func (r subscriptionRepository) FindByUserID(ctx context.Context, userID string) ([]pluginapi.Subscription, error) {
	subs, err := r.repo.FindByUserID(ctx, userID)
	return fromDomainSubscriptions(subs), err
}

func (r subscriptionRepository) Create(ctx context.Context, sub *pluginapi.Subscription) error {
	return r.repo.Create(ctx, toDomainSubscription(sub))
}

func (r subscriptionRepository) Update(ctx context.Context, sub *pluginapi.Subscription) error {
	return r.repo.Update(ctx, toDomainSubscription(sub))
}

func (r subscriptionRepository) Delete(ctx context.Context, id string) error {
	return r.repo.Delete(ctx, id)
}

func (r subscriptionRepository) FindAll(ctx context.Context) ([]pluginapi.Subscription, error) {
	subs, err := r.repo.FindAll(ctx)
	return fromDomainSubscriptions(subs), err
}

func (r subscriptionRepository) FindByStatus(ctx context.Context, status string) ([]pluginapi.Subscription, error) {
	subs, err := r.repo.FindByStatus(ctx, status)
	return fromDomainSubscriptions(subs), err
}

func (r subscriptionRepository) GetMasterSnapshot(ctx context.Context) ([]pluginapi.Subscription, error) {
	subs, err := r.repo.GetMasterSnapshot(ctx)
	return fromDomainSubscriptions(subs), err
}

func (r subscriptionRepository) FindByClientIdentifier(ctx context.Context, clientID string) (*pluginapi.Subscription, error) {
	sub, err := r.repo.FindByClientIdentifier(ctx, clientID)
	return fromDomainSubscription(sub), err
}

func (r subscriptionRepository) FindLatestByEmail(ctx context.Context, email string) (*pluginapi.Subscription, error) {
	sub, err := r.repo.FindLatestByEmail(ctx, email)
	return fromDomainSubscription(sub), err
}

func (r subscriptionRepository) UpdateStatusIfActive(ctx context.Context, id, newStatus string) (bool, error) {
	return r.repo.UpdateStatusIfActive(ctx, id, newStatus)
}

func (r subscriptionRepository) UpdateFields(ctx context.Context, id string, updates map[string]interface{}) error {
	return r.repo.UpdateFields(ctx, id, updates)
}

func (r subscriptionRepository) FindLatestByUserID(ctx context.Context, userID string) (*pluginapi.Subscription, error) {
	sub, err := r.repo.FindLatestByUserID(ctx, userID)
	return fromDomainSubscription(sub), err
}

func (r subscriptionRepository) FindLatestByUserIDs(ctx context.Context, userIDs []string) ([]pluginapi.Subscription, error) {
	subs, err := r.repo.FindLatestByUserIDs(ctx, userIDs)
	return fromDomainSubscriptions(subs), err
}

func (r subscriptionRepository) UpdateMaxDevicesByUserID(ctx context.Context, userID string, maxDevices int) error {
	return r.repo.UpdateMaxDevicesByUserID(ctx, userID, maxDevices)
}

func (r subscriptionRepository) UpdateAutoRenewByUserID(ctx context.Context, userID string, autoRenew bool) error {
	return r.repo.UpdateAutoRenewByUserID(ctx, userID, autoRenew)
}

func (r subscriptionRepository) AutoRenewSubscription(ctx context.Context, userID string, planID *int64, totalPrice int, endsAt *time.Time, maxDevices int) error {
	return r.repo.AutoRenewSubscription(ctx, userID, planID, totalPrice, endsAt, maxDevices)
}

func (r subscriptionRepository) GetAllEmailsAndMaxDevices(ctx context.Context) ([]pluginapi.EmailAndMaxDevice, error) {
	items, err := r.repo.GetAllEmailsAndMaxDevices(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]pluginapi.EmailAndMaxDevice, len(items))
	for i, item := range items {
		result[i] = pluginapi.EmailAndMaxDevice{Email: item.Email, MaxDevices: item.MaxDevices}
	}
	return result, nil
}

type deviceRepository struct{ repo domain.DeviceRepository }

func (r deviceRepository) TrackDevice(ctx context.Context, subID, hwid, deviceModel, deviceOS, userAgent string, limit int) (bool, error) {
	return r.repo.TrackDevice(ctx, subID, hwid, deviceModel, deviceOS, userAgent, limit)
}

func (r deviceRepository) CountBySubscriptions(ctx context.Context, subIDs []string) (map[string]int64, error) {
	return r.repo.CountBySubscriptions(ctx, subIDs)
}

func (r deviceRepository) FindOldestBySubscription(ctx context.Context, subID string, limit int) ([]pluginapi.Device, error) {
	devices, err := r.repo.FindOldestBySubscription(ctx, subID, limit)
	return fromDomainDevices(devices), err
}

func (r deviceRepository) DeleteByIDs(ctx context.Context, ids []int64) error {
	return r.repo.DeleteByIDs(ctx, ids)
}

func (r deviceRepository) CountBySubscription(ctx context.Context, subID string) (int64, error) {
	return r.repo.CountBySubscription(ctx, subID)
}

func (r deviceRepository) FindBySubscriptionID(ctx context.Context, subID string) ([]pluginapi.Device, error) {
	devices, err := r.repo.FindBySubscriptionID(ctx, subID)
	return fromDomainDevices(devices), err
}

func (r deviceRepository) FindByIDAndSubscription(ctx context.Context, deviceID int64, subID string) (*pluginapi.Device, error) {
	device, err := r.repo.FindByIDAndSubscription(ctx, deviceID, subID)
	return fromDomainDevice(device), err
}

func (r deviceRepository) Delete(ctx context.Context, id int64) error {
	return r.repo.Delete(ctx, id)
}

type planRepository struct{ repo domain.PlanRepository }

func (r planRepository) FindByID(ctx context.Context, id string) (*pluginapi.Plan, error) {
	plan, err := r.repo.FindByID(ctx, id)
	return fromDomainPlan(plan), err
}

func (r planRepository) FindAll(ctx context.Context) ([]pluginapi.Plan, error) {
	plans, err := r.repo.FindAll(ctx)
	return fromDomainPlans(plans), err
}

func (r planRepository) FindActive(ctx context.Context) ([]pluginapi.Plan, error) {
	plans, err := r.repo.FindActive(ctx)
	return fromDomainPlans(plans), err
}

func (r planRepository) Create(ctx context.Context, plan *pluginapi.Plan) error {
	return r.repo.Create(ctx, toDomainPlan(plan))
}

func (r planRepository) Update(ctx context.Context, plan *pluginapi.Plan) error {
	return r.repo.Update(ctx, toDomainPlan(plan))
}

func (r planRepository) Delete(ctx context.Context, id string) error {
	return r.repo.Delete(ctx, id)
}

func fromDomainUser(user *domain.User) *pluginapi.User {
	if user == nil {
		return nil
	}
	return &pluginapi.User{
		ID: user.ID, Username: user.Username, Balance: user.Balance, IsAdmin: user.IsAdmin,
		RefCode: user.RefCode, ReferredBy: user.ReferredBy, Metadata: map[string]any(user.Metadata),
		IsBlocked: user.IsBlocked, CreatedAt: user.CreatedAt,
	}
}

func toDomainUser(user *pluginapi.User) *domain.User {
	if user == nil {
		return nil
	}
	return &domain.User{
		ID: user.ID, Username: user.Username, Balance: user.Balance, IsAdmin: user.IsAdmin,
		RefCode: user.RefCode, ReferredBy: user.ReferredBy, Metadata: domain.Metadata(user.Metadata),
		IsBlocked: user.IsBlocked, CreatedAt: user.CreatedAt,
	}
}

func fromDomainUsers(users []domain.User) []pluginapi.User {
	result := make([]pluginapi.User, len(users))
	for i := range users {
		result[i] = *fromDomainUser(&users[i])
	}
	return result
}

func fromDomainSubscription(sub *domain.Subscription) *pluginapi.Subscription {
	if sub == nil {
		return nil
	}
	return &pluginapi.Subscription{
		ID: sub.ID, UserID: sub.UserID, Email: sub.Email, UUID: sub.XrayUUID, Status: sub.Status,
		MaxDevices: sub.MaxDevices, StartsAt: sub.StartsAt, EndsAt: sub.EndsAt, AutoRenew: sub.AutoRenew,
		Metadata: map[string]any(sub.Metadata), CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt,
	}
}

func toDomainSubscription(sub *pluginapi.Subscription) *domain.Subscription {
	if sub == nil {
		return nil
	}
	return &domain.Subscription{
		ID: sub.ID, UserID: sub.UserID, Email: sub.Email, XrayUUID: sub.UUID, Status: sub.Status,
		MaxDevices: sub.MaxDevices, StartsAt: sub.StartsAt, EndsAt: sub.EndsAt, AutoRenew: sub.AutoRenew,
		Metadata: domain.Metadata(sub.Metadata), CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt,
	}
}

func fromDomainSubscriptions(subs []domain.Subscription) []pluginapi.Subscription {
	result := make([]pluginapi.Subscription, len(subs))
	for i := range subs {
		result[i] = *fromDomainSubscription(&subs[i])
	}
	return result
}

func fromDomainDevice(device *domain.Device) *pluginapi.Device {
	if device == nil {
		return nil
	}
	return &pluginapi.Device{
		ID: device.ID, SubscriptionID: device.SubscriptionID, HWID: device.HWID,
		DeviceModel: device.DeviceModel, DeviceOS: device.DeviceOS, UserAgent: device.UserAgent,
	}
}

func fromDomainDevices(devices []domain.Device) []pluginapi.Device {
	result := make([]pluginapi.Device, len(devices))
	for i := range devices {
		result[i] = *fromDomainDevice(&devices[i])
	}
	return result
}

func fromDomainPlan(plan *domain.Plan) *pluginapi.Plan {
	if plan == nil {
		return nil
	}
	return &pluginapi.Plan{
		ID: plan.ID, Months: plan.Months, BasePrice: plan.BasePrice,
		GlobalDiscountPercent: plan.GlobalDiscountPercent, IsActive: plan.IsActive,
		CreatedAt: plan.CreatedAt, UpdatedAt: plan.UpdatedAt,
	}
}

func toDomainPlan(plan *pluginapi.Plan) *domain.Plan {
	if plan == nil {
		return nil
	}
	return &domain.Plan{
		ID: plan.ID, Months: plan.Months, BasePrice: plan.BasePrice,
		GlobalDiscountPercent: plan.GlobalDiscountPercent, IsActive: plan.IsActive,
		CreatedAt: plan.CreatedAt, UpdatedAt: plan.UpdatedAt,
	}
}

func fromDomainPlans(plans []domain.Plan) []pluginapi.Plan {
	result := make([]pluginapi.Plan, len(plans))
	for i := range plans {
		result[i] = *fromDomainPlan(&plans[i])
	}
	return result
}

var _ pluginapi.UserRepository = userRepository{}
var _ pluginapi.SubscriptionRepository = subscriptionRepository{}
var _ pluginapi.DeviceRepository = deviceRepository{}
var _ pluginapi.PlanRepository = planRepository{}
