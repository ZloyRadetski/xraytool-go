package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	modernsqlite "github.com/glebarez/go-sqlite"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	libsqlite "modernc.org/sqlite/lib"

	"xraytool/internal/domain"
)

type gormRegistry struct {
	db *gorm.DB
}

func NewRegistry(db *gorm.DB) Registry {
	return &gormRegistry{db: db}
}

// GormDB returns the connection owned by a registry created by NewRegistry.
// It is intentionally a narrow composition-root escape hatch: application
// services continue to depend on domain.Registry, while the plugin host needs
// the pool solely to construct scoped PluginDBHandle instances.
func GormDB(registry domain.Registry) (*gorm.DB, bool) {
	gormRegistry, ok := registry.(*gormRegistry)
	if !ok || gormRegistry == nil || gormRegistry.db == nil {
		return nil, false
	}
	return gormRegistry.db, true
}

func wrapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.ErrNotFound
	}

	// SQLite (modernc.org/sqlite via glebarez)
	var modernErr *modernsqlite.Error
	if errors.As(err, &modernErr) {
		if modernErr.Code() == libsqlite.SQLITE_CONSTRAINT_UNIQUE || modernErr.Code() == libsqlite.SQLITE_CONSTRAINT_PRIMARYKEY || modernErr.Code() == libsqlite.SQLITE_CONSTRAINT {
			return domain.ErrDuplicate
		}
	}

	// PostgreSQL
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "23505" { // unique_violation
			return domain.ErrDuplicate
		}
	}

	return err
}

func (r *gormRegistry) Users() UserRepository {
	return &gormUserRepo{db: r.db}
}

func (r *gormRegistry) Subscriptions() SubscriptionRepository {
	return &gormSubscriptionRepo{db: r.db}
}

func (r *gormRegistry) Payments() PaymentRepository {
	return &gormPaymentRepo{db: r.db}
}

func (r *gormRegistry) Plans() PlanRepository {
	return &gormPlanRepo{db: r.db}
}

func (r *gormRegistry) Promos() PromoRepository {
	return &gormPromoRepo{db: r.db}
}

func (r *gormRegistry) AntifraudBans() AntifraudBanRepository {
	return &gormAntifraudBanRepo{db: r.db}
}

func (r *gormRegistry) Devices() DeviceRepository {
	return &gormDeviceRepo{db: r.db}
}

func (r *gormRegistry) Notifications() SubscriptionNotificationRepository {
	return &gormNotificationRepo{db: r.db}
}

func (r *gormRegistry) WithTx(ctx context.Context, fn func(tx Registry) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txRegistry := &gormRegistry{db: tx}
		return fn(txRegistry)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// User Repository
// ─────────────────────────────────────────────────────────────────────────────

type gormUserRepo struct {
	db *gorm.DB
}

func (r *gormUserRepo) FindByID(ctx context.Context, id string) (*domain.User, error) {
	var user User
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&user).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := user.ToDomain()
	return &d, nil
}

func (r *gormUserRepo) FindByEmailOrUsername(ctx context.Context, email string) (*domain.User, error) {
	var user User
	err := r.db.WithContext(ctx).Where("username = ?", email).First(&user).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := user.ToDomain()
	return &d, nil
}

func (r *gormUserRepo) FindByTelegramID(ctx context.Context, tgID int64) (*domain.User, error) {
	return FindUserByPlatformID(r.db.WithContext(ctx), "telegram", fmt.Sprintf("%d", tgID))
}

func (r *gormUserRepo) FindByRefCode(ctx context.Context, code string) (*domain.User, error) {
	var user User
	err := r.db.WithContext(ctx).Where("ref_code = ?", code).First(&user).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := user.ToDomain()
	return &d, nil
}

func (r *gormUserRepo) Create(ctx context.Context, user *domain.User) error {
	dbModel := FromDomainUser(*user)
	err := r.db.WithContext(ctx).Create(&dbModel).Error
	if err == nil {
		*user = dbModel.ToDomain()
	}
	return wrapError(err)
}

func (r *gormUserRepo) Update(ctx context.Context, user *domain.User) error {
	dbModel := FromDomainUser(*user)
	err := r.db.WithContext(ctx).Save(&dbModel).Error
	if err == nil {
		*user = dbModel.ToDomain()
	}
	return wrapError(err)
}

func (r *gormUserRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&User{}).Error
}

func (r *gormUserRepo) FindAll(ctx context.Context) ([]domain.User, error) {
	var users []User
	err := r.db.WithContext(ctx).Find(&users).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.User
	for _, u := range users {
		d = append(d, u.ToDomain())
	}
	return d, nil
}

func (r *gormUserRepo) FindByPlatformID(ctx context.Context, platform, id string) (*domain.User, error) {
	return FindUserByPlatformID(r.db.WithContext(ctx), platform, id)
}

func (r *gormUserRepo) AddReferralReward(ctx context.Context, referrerID string, referredID string, paymentID int64, reward int) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&ReferralReward{}).Where("payment_id = ?", paymentID).Count(&count).Error; err != nil {
			return wrapError(err)
		}
		if count > 0 {
			return nil
		}
		if result := tx.Model(&User{}).Where("id = ?", referrerID).Update("balance", gorm.Expr("balance + ?", reward)); result.Error != nil {
			return result.Error
		}
		rewardRow := ReferralReward{
			ReferrerID: referrerID,
			ReferredID: referredID,
			PaymentID:  paymentID,
			Amount:     reward,
		}
		return tx.Create(&rewardRow).Error
	})
}

func (r *gormUserRepo) CountReferrals(ctx context.Context, referrerID string) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&User{}).Where("referred_by = ?", referrerID).Count(&count).Error
	return count, wrapError(err)
}

func (r *gormUserRepo) CountReferralRewards(ctx context.Context, referrerID string) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&ReferralReward{}).Where("referrer_id = ?", referrerID).Count(&count).Error
	return count, wrapError(err)
}

func (r *gormUserRepo) SumReferralRewards(ctx context.Context, referrerID string) (int64, error) {
	var total int64
	err := r.db.WithContext(ctx).Model(&ReferralReward{}).Where("referrer_id = ?", referrerID).Select("COALESCE(SUM(amount),0)").Scan(&total).Error
	return total, err
}

func (r *gormUserRepo) GetReferralStats(ctx context.Context, referrerIDs []string) ([]ReferralStats, error) {
	if len(referrerIDs) == 0 {
		return nil, nil
	}
	type userCount struct {
		ReferrerID string `gorm:"column:referrer_id"`
		Count      int64  `gorm:"column:count"`
	}
	var uCounts []userCount
	if err := r.db.WithContext(ctx).Model(&User{}).
		Where("referred_by IN ?", referrerIDs).
		Select("referred_by as referrer_id, count(*) as count").
		Group("referred_by").
		Scan(&uCounts).Error; err != nil {
		return nil, wrapError(err)
	}

	type rewardSum struct {
		ReferrerID string `gorm:"column:referrer_id"`
		Total      int64  `gorm:"column:total"`
	}
	var rSums []rewardSum
	if err := r.db.WithContext(ctx).Model(&ReferralReward{}).
		Where("referrer_id IN ?", referrerIDs).
		Select("referrer_id, coalesce(sum(amount),0) as total").
		Group("referrer_id").
		Scan(&rSums).Error; err != nil {
		return nil, wrapError(err)
	}

	statsMap := make(map[string]*ReferralStats)
	for _, uc := range uCounts {
		statsMap[uc.ReferrerID] = &ReferralStats{
			ReferrerID: uc.ReferrerID,
			Count:      uc.Count,
			Total:      0,
		}
	}
	for _, rs := range rSums {
		s, ok := statsMap[rs.ReferrerID]
		if !ok {
			statsMap[rs.ReferrerID] = &ReferralStats{
				ReferrerID: rs.ReferrerID,
				Count:      0,
				Total:      rs.Total,
			}
		} else {
			s.Total = rs.Total
		}
	}

	res := make([]ReferralStats, 0, len(statsMap))
	for _, s := range statsMap {
		res = append(res, *s)
	}
	return res, nil
}

func (r *gormUserRepo) CountByRefCode(ctx context.Context, code string) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&User{}).Where("ref_code = ?", code).Count(&count).Error
	return count, wrapError(err)
}

func (r *gormUserRepo) FindAdmins(ctx context.Context) ([]domain.User, error) {
	var users []User
	err := r.db.WithContext(ctx).Where("is_admin = ?", true).Find(&users).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.User
	for _, u := range users {
		d = append(d, u.ToDomain())
	}
	return d, nil
}

func (r *gormUserRepo) AdjustBalance(ctx context.Context, userID string, amount int) error {
	query := "UPDATE users SET balance = CASE WHEN balance + ? < 0 THEN 0 ELSE balance + ? END WHERE id = ?"
	return wrapError(r.db.WithContext(ctx).Exec(query, amount, amount, userID).Error)
}

func (r *gormUserRepo) UpdateIsBlocked(ctx context.Context, userID string, isBlocked bool) error {
	return wrapError(r.db.WithContext(ctx).Model(&User{}).Where("id = ?", userID).Update("is_blocked", isBlocked).Error)
}

func (r *gormUserRepo) ListUsers(ctx context.Context, page, limit int, search string) ([]domain.User, int64, error) {
	if page < 1 {
		page = 1
	}
	if limit <= 0 {
		limit = 20
	}

	query := r.db.WithContext(ctx).Model(&User{})

	if search != "" {
		if r.db.Dialector.Name() == "postgres" {
			likeQ := "%" + search + "%"
			query = query.Where("username ILIKE ? OR metadata::text ILIKE ?", likeQ, likeQ)
		} else {
			searchLower := strings.ToLower(search)
			searchUpper := strings.ToUpper(search)
			query = query.Where("username LIKE ? OR username LIKE ? OR metadata LIKE ? OR metadata LIKE ?",
				"%"+searchLower+"%", "%"+searchUpper+"%", "%"+searchLower+"%", "%"+searchUpper+"%")
		}
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, wrapError(err)
	}

	var users []User
	offset := (page - 1) * limit
	if err := query.Order("created_at desc").Offset(offset).Limit(limit).Find(&users).Error; err != nil {
		return nil, 0, wrapError(err)
	}

	var d []domain.User
	for _, u := range users {
		d = append(d, u.ToDomain())
	}
	return d, total, nil
}

func (r *gormUserRepo) DeleteUserAndData(ctx context.Context, userID string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var subIDs []string
		if err := tx.Model(&Subscription{}).Where("user_id = ?", userID).Pluck("id", &subIDs).Error; err != nil {
			return wrapError(err)
		}

		if len(subIDs) > 0 {
			if err := tx.Where("subscription_id IN ?", subIDs).Delete(&Device{}).Error; err != nil {
				return wrapError(err)
			}
			if err := tx.Where("subscription_id IN ?", subIDs).Delete(&SubscriptionNotification{}).Error; err != nil {
				return wrapError(err)
			}
		}

		if err := tx.Where("user_id = ?", userID).Delete(&Subscription{}).Error; err != nil {
			return wrapError(err)
		}
		if err := tx.Where("user_id = ?", userID).Delete(&Payment{}).Error; err != nil {
			return wrapError(err)
		}
		if err := tx.Where("referrer_id = ? OR referred_id = ?", userID, userID).Delete(&ReferralReward{}).Error; err != nil {
			return wrapError(err)
		}
		if err := tx.Where("id = ?", userID).Delete(&User{}).Error; err != nil {
			return wrapError(err)
		}
		return nil
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Subscription Repository
// ─────────────────────────────────────────────────────────────────────────────

type gormSubscriptionRepo struct {
	db *gorm.DB
}

func (r *gormSubscriptionRepo) FindByID(ctx context.Context, id string) (*domain.Subscription, error) {
	var sub Subscription
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&sub).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := sub.ToDomain()
	return &d, nil
}

func (r *gormSubscriptionRepo) FindByEmail(ctx context.Context, email string) (*domain.Subscription, error) {
	var sub Subscription
	err := r.db.WithContext(ctx).Where("email = ?", email).Order("created_at desc").First(&sub).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := sub.ToDomain()
	return &d, nil
}

func (r *gormSubscriptionRepo) FindByUserID(ctx context.Context, userID string) ([]domain.Subscription, error) {
	var subs []Subscription
	err := r.db.WithContext(ctx).Where("user_id = ?", userID).Find(&subs).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Subscription
	for _, s := range subs {
		d = append(d, s.ToDomain())
	}
	return d, nil
}

func (r *gormSubscriptionRepo) Create(ctx context.Context, sub *domain.Subscription) error {
	dbModel := FromDomainSubscription(*sub)
	err := r.db.WithContext(ctx).Create(&dbModel).Error
	if err == nil {
		*sub = dbModel.ToDomain()
	}
	return wrapError(err)
}

func (r *gormSubscriptionRepo) Update(ctx context.Context, sub *domain.Subscription) error {
	dbModel := FromDomainSubscription(*sub)
	err := r.db.WithContext(ctx).Save(&dbModel).Error
	if err == nil {
		*sub = dbModel.ToDomain()
	}
	return wrapError(err)
}

func (r *gormSubscriptionRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&Subscription{}).Error
}

func (r *gormSubscriptionRepo) FindAll(ctx context.Context) ([]domain.Subscription, error) {
	var subs []Subscription
	err := r.db.WithContext(ctx).Find(&subs).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Subscription
	for _, s := range subs {
		d = append(d, s.ToDomain())
	}
	return d, nil
}

func (r *gormSubscriptionRepo) FindByStatus(ctx context.Context, status string) ([]domain.Subscription, error) {
	var subs []Subscription
	err := r.db.WithContext(ctx).Where("status = ?", status).Find(&subs).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Subscription
	for _, s := range subs {
		d = append(d, s.ToDomain())
	}
	return d, nil
}

func (r *gormSubscriptionRepo) GetMasterSnapshot(ctx context.Context) ([]domain.Subscription, error) {
	var subs []Subscription
	err := r.db.WithContext(ctx).Where("status = 'active'").Find(&subs).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Subscription
	for _, s := range subs {
		d = append(d, s.ToDomain())
	}
	return d, nil
}

func (r *gormSubscriptionRepo) FindByClientIdentifier(ctx context.Context, clientId string) (*domain.Subscription, error) {
	var subs []Subscription
	var query *gorm.DB
	if r.db.Dialector.Name() == "postgres" {
		query = r.db.WithContext(ctx).Where("id = ? OR uuid = ? OR metadata::jsonb ->> 'subfile' = ? OR metadata::jsonb ->> 'subfile' = ?",
			clientId, clientId, clientId, clientId+".txt")
	} else {
		query = r.db.WithContext(ctx).Where("id = ? OR uuid = ? OR json_extract(metadata, '$.subfile') = ? OR json_extract(metadata, '$.subfile') = ?",
			clientId, clientId, clientId, clientId+".txt")
	}
	if err := query.Limit(1).Find(&subs).Error; err != nil {
		return nil, wrapError(err)
	}
	if len(subs) == 0 {
		return nil, nil
	}
	d := subs[0].ToDomain()
	return &d, nil
}

func (r *gormSubscriptionRepo) FindLatestByEmail(ctx context.Context, email string) (*domain.Subscription, error) {
	var sub Subscription
	err := r.db.WithContext(ctx).Where("email = ?", email).Order("created_at desc").First(&sub).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := sub.ToDomain()
	return &d, nil
}

func (r *gormSubscriptionRepo) UpdateFields(ctx context.Context, id string, updates map[string]interface{}) error {
	return r.db.WithContext(ctx).Model(&Subscription{}).Where("id = ?", id).Updates(updates).Error
}

func (r *gormSubscriptionRepo) UpdateStatusIfActive(ctx context.Context, id string, newStatus string) (bool, error) {
	res := r.db.WithContext(ctx).Model(&Subscription{}).Where("id = ? AND status = 'active'", id).Updates(map[string]interface{}{
		"status":     newStatus,
		"updated_at": time.Now().UTC(),
	})
	return res.RowsAffected > 0, wrapError(res.Error)
}

func (r *gormSubscriptionRepo) FindLatestByUserID(ctx context.Context, userID string) (*domain.Subscription, error) {
	var sub Subscription
	err := r.db.WithContext(ctx).Where("user_id = ?", userID).Order("created_at desc").First(&sub).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := sub.ToDomain()
	return &d, nil
}

func (r *gormSubscriptionRepo) FindLatestByUserIDs(ctx context.Context, userIDs []string) ([]domain.Subscription, error) {
	if len(userIDs) == 0 {
		return []domain.Subscription{}, nil
	}

	var subs []Subscription
	// Fetch only the newest row for each user in the database. Loading every
	// historical subscription and de-duplicating it in Go makes this hot path
	// grow without bound as accounts are renewed.
	err := r.db.WithContext(ctx).Raw(`
		SELECT * FROM (
			SELECT subscriptions.*, ROW_NUMBER() OVER (
				PARTITION BY user_id ORDER BY created_at DESC, id DESC
			) AS row_number
			FROM subscriptions
			WHERE user_id IN ?
		) AS latest_subscriptions
		WHERE row_number = 1
		ORDER BY created_at DESC, id DESC
	`, userIDs).Scan(&subs).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Subscription
	for _, s := range subs {
		d = append(d, s.ToDomain())
	}
	return d, nil
}

func (r *gormSubscriptionRepo) UpdateMaxDevicesByUserID(ctx context.Context, userID string, maxDevices int) error {
	return r.db.WithContext(ctx).Model(&Subscription{}).Where("user_id = ?", userID).Update("max_devices", maxDevices).Error
}

func (r *gormSubscriptionRepo) UpdateAutoRenewByUserID(ctx context.Context, userID string, autoRenew bool) error {
	return r.db.WithContext(ctx).Model(&Subscription{}).Where("user_id = ?", userID).Update("auto_renew", autoRenew).Error
}

func (r *gormSubscriptionRepo) AutoRenewSubscription(ctx context.Context, userID string, planID *int64, planTotalPrice int, newEndsAtPtr *time.Time, maxDevices int) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var sub Subscription
		lockQuery := tx.Where("user_id = ?", userID).Order("created_at desc")
		if tx.Dialector.Name() == "postgres" {
			lockQuery = lockQuery.Set("gorm:query_option", "FOR UPDATE")
		}
		if err := lockQuery.First(&sub).Error; err != nil {
			return fmt.Errorf("subscription not found: %w", err)
		}

		var newEndsAt time.Time
		metadata := sub.Metadata
		if planID != nil {
			var plan Plan
			if err := tx.First(&plan, *planID).Error; err != nil {
				return fmt.Errorf("plan not found: %w", err)
			}
			now := time.Now().UTC()
			baseTime := now
			if sub.EndsAt != nil && sub.EndsAt.After(now) {
				baseTime = *sub.EndsAt
			}
			newEndsAt = baseTime.AddDate(0, plan.Months, 0)
			// Persist a routing snapshot with the subscription. Plan routing
			// then travels with state-sync events and does not require each
			// slave to resolve plan records independently.
			if metadata == nil {
				metadata = Metadata{}
			}
			if len(plan.EngineIDs) == 0 {
				delete(metadata, "plan_engine_ids")
			} else {
				metadata["plan_engine_ids"] = append([]string(nil), plan.EngineIDs...)
			}
		} else {
			if newEndsAtPtr == nil {
				return fmt.Errorf("newEndsAt is required if planID is nil")
			}
			newEndsAt = *newEndsAtPtr
		}

		if planTotalPrice > 0 {
			result := tx.Model(&User{}).
				Where("id = ? AND balance >= ?", userID, planTotalPrice).
				Update("balance", gorm.Expr("balance - ?", planTotalPrice))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return fmt.Errorf("insufficient balance")
			}
		}

		now := time.Now().UTC()
		updates := map[string]interface{}{
			"status":      "active",
			"ends_at":     newEndsAt,
			"max_devices": maxDevices,
			"updated_at":  now,
		}
		if err := tx.Model(&Subscription{}).
			Where("id = ?", sub.ID).
			Updates(updates).Error; err != nil {
			return err
		}
		if planID == nil {
			return nil
		}

		// GORM's JSON serializer is invoked for a model field, but not when a
		// raw map value is passed through Updates(map[string]any). Updating
		// metadata through a typed model therefore preserves both SQLite TEXT
		// and PostgreSQL JSONB behaviour instead of handing database/sql an
		// unsupported Go map.
		return tx.Model(&Subscription{}).
			Where("id = ?", sub.ID).
			Updates(&Subscription{Metadata: metadata}).Error
	})
}

func (r *gormSubscriptionRepo) GetAllEmailsAndMaxDevices(ctx context.Context) ([]EmailAndMaxDevice, error) {
	var rows []EmailAndMaxDevice
	err := r.db.WithContext(ctx).Model(&Subscription{}).Select("email, max_devices").Scan(&rows).Error
	return rows, err
}

// ─────────────────────────────────────────────────────────────────────────────
// Payment Repository
// ─────────────────────────────────────────────────────────────────────────────

type gormPaymentRepo struct {
	db *gorm.DB
}

func (r *gormPaymentRepo) FindByID(ctx context.Context, id string) (*domain.Payment, error) {
	var p Payment
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&p).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := p.ToDomain()
	return &d, nil
}

func (r *gormPaymentRepo) FindByUserID(ctx context.Context, userID string) ([]domain.Payment, error) {
	var payments []Payment
	err := r.db.WithContext(ctx).Where("user_id = ?", userID).Find(&payments).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Payment
	for _, p := range payments {
		d = append(d, p.ToDomain())
	}
	return d, nil
}

func (r *gormPaymentRepo) Create(ctx context.Context, p *domain.Payment) error {
	dbModel := FromDomainPayment(*p)
	err := r.db.WithContext(ctx).Create(&dbModel).Error
	if err == nil {
		*p = dbModel.ToDomain()
	}
	return wrapError(err)
}

func (r *gormPaymentRepo) Update(ctx context.Context, p *domain.Payment) error {
	dbModel := FromDomainPayment(*p)
	err := r.db.WithContext(ctx).Save(&dbModel).Error
	if err == nil {
		*p = dbModel.ToDomain()
	}
	return wrapError(err)
}

func (r *gormPaymentRepo) FindAll(ctx context.Context) ([]domain.Payment, error) {
	var payments []Payment
	err := r.db.WithContext(ctx).Find(&payments).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Payment
	for _, p := range payments {
		d = append(d, p.ToDomain())
	}
	return d, nil
}

func (r *gormPaymentRepo) FindPendingByProvider(ctx context.Context, provider string) ([]domain.Payment, error) {
	var payments []Payment
	err := r.db.WithContext(ctx).Where("status = 'pending' AND method = ?", provider).Find(&payments).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Payment
	for _, p := range payments {
		d = append(d, p.ToDomain())
	}
	return d, nil
}

func (r *gormPaymentRepo) FindPaymentsByFilters(ctx context.Context, status, method, paymentType, telegramIDStr string) ([]domain.Payment, error) {
	query := r.db.WithContext(ctx).Model(&Payment{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if method != "" {
		query = query.Where("method = ?", method)
	}
	if paymentType != "" {
		query = query.Where("payment_type = ?", paymentType)
	}
	if telegramIDStr != "" {
		user, err := FindUserByPlatformID(r.db.WithContext(ctx), "telegram", telegramIDStr)
		if err != nil {
			return []domain.Payment{}, nil
		}
		query = query.Where("user_id = ?", user.ID)
	}
	var payments []Payment
	err := query.Order("id DESC").Find(&payments).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Payment
	for _, p := range payments {
		d = append(d, p.ToDomain())
	}
	return d, nil
}

func (r *gormPaymentRepo) UpdateStatus(ctx context.Context, paymentID int64, newStatus string, expectedStatuses []string) (bool, error) {
	query := r.db.WithContext(ctx).Model(&Payment{}).Where("id = ?", paymentID)
	if len(expectedStatuses) > 0 {
		query = query.Where("status IN ?", expectedStatuses)
	} else if newStatus == "completed" {
		query = query.Where("status != 'completed'")
	}
	res := query.Update("status", newStatus)
	return res.RowsAffected > 0, wrapError(res.Error)
}

func (r *gormPaymentRepo) UpdateStatusIfNotCompleted(ctx context.Context, paymentID int64, newStatus string) (bool, error) {
	res := r.db.WithContext(ctx).Model(&Payment{}).Where("id = ? AND status != ?", paymentID, "completed").Update("status", newStatus)
	return res.RowsAffected > 0, wrapError(res.Error)
}

func (r *gormPaymentRepo) FindByExternalID(ctx context.Context, extID string) (*domain.Payment, error) {
	var p Payment
	err := r.db.WithContext(ctx).Where("external_id = ?", extID).First(&p).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := p.ToDomain()
	return &d, nil
}

func (r *gormPaymentRepo) CountByPromoAndUser(ctx context.Context, promoID int64, userID string, status string) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Payment{}).Where("user_id = ? AND promo_code_id = ? AND status = ?", userID, promoID, status).Count(&count).Error
	return count, wrapError(err)
}

func (r *gormPaymentRepo) CountByUserAndPromo(ctx context.Context, userID string, promoID int64) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Payment{}).
		Where("user_id = ? AND promo_code_id = ? AND status IN (?, ?)", userID, promoID, "completed", "pending_card").
		Count(&count).Error
	return count, wrapError(err)
}

func (r *gormPaymentRepo) ScrubOldExternalIDs(ctx context.Context, cutoff time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Model(&Payment{}).
		Where("status IN ('completed', 'failed', 'canceled') AND created_at < ? AND external_id IS NOT NULL", cutoff).
		Update("external_id", nil)
	return res.RowsAffected, wrapError(res.Error)
}

// ─────────────────────────────────────────────────────────────────────────────
// Plan Repository
// ─────────────────────────────────────────────────────────────────────────────

type gormPlanRepo struct {
	db *gorm.DB
}

func (r *gormPlanRepo) FindByID(ctx context.Context, id string) (*domain.Plan, error) {
	var plan Plan
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&plan).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := plan.ToDomain()
	return &d, nil
}

func (r *gormPlanRepo) FindAll(ctx context.Context) ([]domain.Plan, error) {
	var plans []Plan
	err := r.db.WithContext(ctx).Order("base_price asc").Find(&plans).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Plan
	for _, p := range plans {
		d = append(d, p.ToDomain())
	}
	return d, nil
}

func (r *gormPlanRepo) FindActive(ctx context.Context) ([]domain.Plan, error) {
	var plans []Plan
	err := r.db.WithContext(ctx).Where("is_active = ?", true).Order("months asc").Find(&plans).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Plan
	for _, p := range plans {
		d = append(d, p.ToDomain())
	}
	return d, nil
}

func (r *gormPlanRepo) Create(ctx context.Context, plan *domain.Plan) error {
	dbModel := FromDomainPlan(*plan)
	err := r.db.WithContext(ctx).Create(&dbModel).Error
	if err == nil {
		*plan = dbModel.ToDomain()
	}
	return wrapError(err)
}

func (r *gormPlanRepo) Update(ctx context.Context, plan *domain.Plan) error {
	dbModel := FromDomainPlan(*plan)
	err := r.db.WithContext(ctx).Save(&dbModel).Error
	if err == nil {
		*plan = dbModel.ToDomain()
	}
	return wrapError(err)
}

func (r *gormPlanRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&Plan{}).Error
}

// ─────────────────────────────────────────────────────────────────────────────
// PromoCode Repository
// ─────────────────────────────────────────────────────────────────────────────

type gormPromoRepo struct {
	db *gorm.DB
}

func (r *gormPromoRepo) FindByCode(ctx context.Context, code string) (*domain.PromoCode, error) {
	var promo PromoCode
	err := r.db.WithContext(ctx).Where("code = ?", code).First(&promo).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := promo.ToDomain()
	return &d, nil
}

func (r *gormPromoRepo) Create(ctx context.Context, promo *domain.PromoCode) error {
	dbModel := FromDomainPromoCode(*promo)
	err := r.db.WithContext(ctx).Create(&dbModel).Error
	if err == nil {
		*promo = dbModel.ToDomain()
	}
	return wrapError(err)
}

func (r *gormPromoRepo) Update(ctx context.Context, promo *domain.PromoCode) error {
	dbModel := FromDomainPromoCode(*promo)
	err := r.db.WithContext(ctx).Save(&dbModel).Error
	if err == nil {
		*promo = dbModel.ToDomain()
	}
	return wrapError(err)
}

func (r *gormPromoRepo) FindAll(ctx context.Context) ([]domain.PromoCode, error) {
	var codes []PromoCode
	err := r.db.WithContext(ctx).Order("created_at desc").Find(&codes).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.PromoCode
	for _, p := range codes {
		d = append(d, p.ToDomain())
	}
	return d, nil
}

func (r *gormPromoRepo) Delete(ctx context.Context, id int64) (int64, error) {
	res := r.db.WithContext(ctx).Delete(&PromoCode{}, id)
	return res.RowsAffected, wrapError(res.Error)
}

func (r *gormPromoRepo) FindByID(ctx context.Context, id int64) (*domain.PromoCode, error) {
	var p PromoCode
	err := r.db.WithContext(ctx).First(&p, id).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := p.ToDomain()
	return &d, nil
}

func (r *gormPromoRepo) IncrementUses(ctx context.Context, id int64, maxUses int) (bool, error) {
	var res *gorm.DB
	if maxUses > 0 {
		res = r.db.WithContext(ctx).Exec("UPDATE promo_codes SET uses_count = uses_count + 1 WHERE id = ? AND uses_count < ?", id, maxUses)
	} else {
		res = r.db.WithContext(ctx).Exec("UPDATE promo_codes SET uses_count = uses_count + 1 WHERE id = ?", id)
	}
	if res.Error != nil {
		return false, wrapError(res.Error)
	}
	if maxUses > 0 && res.RowsAffected == 0 {
		return false, nil // Limit reached
	}
	return true, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// AntifraudBan Repository
// ─────────────────────────────────────────────────────────────────────────────

type gormAntifraudBanRepo struct {
	db *gorm.DB
}

func (r *gormAntifraudBanRepo) FindByEmail(ctx context.Context, email string) (*domain.AntifraudBan, error) {
	var ban AntifraudBan
	err := r.db.WithContext(ctx).Where("email = ?", email).First(&ban).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := ban.ToDomain()
	return &d, nil
}

func (r *gormAntifraudBanRepo) Create(ctx context.Context, ban *domain.AntifraudBan) error {
	dbModel := FromDomainAntifraudBan(*ban)
	err := r.db.WithContext(ctx).Create(&dbModel).Error
	if err == nil {
		*ban = dbModel.ToDomain()
	}
	return wrapError(err)
}

func (r *gormAntifraudBanRepo) DeleteByEmail(ctx context.Context, email string) error {
	return r.db.WithContext(ctx).Where("email = ?", email).Delete(&AntifraudBan{}).Error
}

func (r *gormAntifraudBanRepo) FindAll(ctx context.Context) ([]domain.AntifraudBan, error) {
	var bans []AntifraudBan
	err := r.db.WithContext(ctx).Find(&bans).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.AntifraudBan
	for _, b := range bans {
		d = append(d, b.ToDomain())
	}
	return d, nil
}

func (r *gormAntifraudBanRepo) FindActive(ctx context.Context) ([]domain.AntifraudBan, error) {
	var bans []AntifraudBan
	err := r.db.WithContext(ctx).Where("expires_at > ?", time.Now().UTC()).Find(&bans).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.AntifraudBan
	for _, b := range bans {
		d = append(d, b.ToDomain())
	}
	return d, nil
}

func (r *gormAntifraudBanRepo) FindExpired(ctx context.Context) ([]domain.AntifraudBan, error) {
	var bans []AntifraudBan
	err := r.db.WithContext(ctx).Where("expires_at <= ?", time.Now().UTC()).Find(&bans).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.AntifraudBan
	for _, b := range bans {
		d = append(d, b.ToDomain())
	}
	return d, nil
}

func (r *gormAntifraudBanRepo) Upsert(ctx context.Context, ban *domain.AntifraudBan) error {
	dbModel := FromDomainAntifraudBan(*ban)
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "email"}},
		DoUpdates: clause.AssignmentColumns([]string{"banned_at", "expires_at", "reason"}),
	}).Create(&dbModel).Error
	if err != nil {
		return wrapError(err)
	}

	// On a conflict GORM does not reliably hydrate the existing primary key on
	// every supported dialect, so return the canonical row to the caller.
	var persisted AntifraudBan
	if err := r.db.WithContext(ctx).Where("email = ?", dbModel.Email).First(&persisted).Error; err != nil {
		return wrapError(err)
	}
	*ban = persisted.ToDomain()
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Device Repository
// ─────────────────────────────────────────────────────────────────────────────

type gormDeviceRepo struct {
	db *gorm.DB
}

func (r *gormDeviceRepo) TrackDevice(ctx context.Context, subID string, hwid, deviceModel, deviceOs, userAgent string, deviceLimit int) (bool, error) {
	deviceLimitReached := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var device Device
		res := tx.Where("subscription_id = ? AND hw_id = ?", subID, hwid).Limit(1).Find(&device)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			var dummy Subscription
			if r.db.Dialector.Name() == "postgres" {
				tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", subID).First(&dummy)
			} else if r.db.Dialector.Name() == "sqlite" {
				tx.Exec("UPDATE subscriptions SET updated_at = CURRENT_TIMESTAMP WHERE id = ?", subID)
			}
			var currentCount int64
			tx.Model(&Device{}).Where("subscription_id = ?", subID).Count(&currentCount)
			if currentCount >= int64(deviceLimit) {
				deviceLimitReached = true
				return nil
			}
			newDevice := Device{
				SubscriptionID: subID,
				HWID:           hwid,
				DeviceModel:    deviceModel,
				DeviceOS:       deviceOs,
				UserAgent:      userAgent,
			}
			return tx.Create(&newDevice).Error
		} else {
			return tx.Model(&device).Updates(map[string]interface{}{
				"device_model": deviceModel,
				"device_os":    deviceOs,
				"user_agent":   userAgent,
			}).Error
		}
	})
	return deviceLimitReached, wrapError(err)
}

func (r *gormDeviceRepo) CountBySubscriptions(ctx context.Context, subIDs []string) (map[string]int64, error) {
	type Result struct {
		SubscriptionID string
		Count          int64
	}
	var results []Result
	if err := r.db.WithContext(ctx).Model(&Device{}).
		Select("subscription_id, count(*) as count").
		Where("subscription_id IN ?", subIDs).
		Group("subscription_id").
		Scan(&results).Error; err != nil {
		return nil, wrapError(err)
	}
	counts := make(map[string]int64)
	for _, res := range results {
		counts[res.SubscriptionID] = res.Count
	}
	return counts, nil
}

func (r *gormDeviceRepo) FindOldestBySubscription(ctx context.Context, subID string, limit int) ([]domain.Device, error) {
	var devices []Device
	err := r.db.WithContext(ctx).Where("subscription_id = ?", subID).Order("id asc").Limit(limit).Find(&devices).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Device
	for _, dev := range devices {
		d = append(d, dev.ToDomain())
	}
	return d, nil
}

func (r *gormDeviceRepo) DeleteByIDs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Where("id IN ?", ids).Delete(&Device{}).Error
}

func (r *gormDeviceRepo) CountBySubscription(ctx context.Context, subID string) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Device{}).Where("subscription_id = ?", subID).Count(&count).Error
	return count, wrapError(err)
}

func (r *gormDeviceRepo) FindBySubscriptionID(ctx context.Context, subID string) ([]domain.Device, error) {
	var devices []Device
	err := r.db.WithContext(ctx).Where("subscription_id = ?", subID).Find(&devices).Error
	if err != nil {
		return nil, wrapError(err)
	}
	var d []domain.Device
	for _, dev := range devices {
		d = append(d, dev.ToDomain())
	}
	return d, nil
}

func (r *gormDeviceRepo) FindByIDAndSubscription(ctx context.Context, deviceID int64, subID string) (*domain.Device, error) {
	var device Device
	err := r.db.WithContext(ctx).Where("id = ? AND subscription_id = ?", deviceID, subID).First(&device).Error
	if err != nil {
		return nil, wrapError(err)
	}
	d := device.ToDomain()
	return &d, nil
}

func (r *gormDeviceRepo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&Device{}).Error
}

// ─────────────────────────────────────────────────────────────────────────────
// SubscriptionNotification Repository
// ─────────────────────────────────────────────────────────────────────────────

type gormNotificationRepo struct {
	db *gorm.DB
}

func (r *gormNotificationRepo) CreateIfNotExists(ctx context.Context, notif *domain.SubscriptionNotification) (bool, error) {
	dbModel := FromDomainSubscriptionNotification(*notif)
	res := r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&dbModel)
	if res.Error == nil {
		*notif = dbModel.ToDomain()
	}
	return res.RowsAffected > 0, wrapError(res.Error)
}

func (r *gormNotificationRepo) DeleteBySubscriptionID(ctx context.Context, subID string) error {
	return r.db.WithContext(ctx).Where("subscription_id = ?", subID).Delete(&SubscriptionNotification{}).Error
}
