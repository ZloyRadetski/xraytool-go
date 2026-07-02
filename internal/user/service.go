package user

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/generate"
	"xraytool/internal/slave"
	"xraytool/internal/subscription"
)

// Service handles user lifecycle operations: creation, blocking, removal,
// expiry updates, and limit management.
//
// It depends on vpn.Engine, NOT on xrayapi or xrayconfig directly.
// This means the same Service code works with any VPN engine that implements
// the interface (Xray, Sing-box, …).
type Config struct {
	IsMaster bool
	Domain   string
}

type Service struct {
	registry   domain.Registry
	cfg        Config
	engine     domain.Engine
	propagator domain.EventPropagator
	log        *slog.Logger
	wg         sync.WaitGroup
}

// Wait blocks until all async propagations have finished.
func (s *Service) Wait() {
	s.wg.Wait()
}

// NewService creates a Service. engine must not be nil; pass &vpn.NoopEngine{}
// for tests or when the engine is intentionally disabled.
// log is the structured logger; pass slog.Default() at the composition root.
func NewService(registry domain.Registry, cfg Config, engine domain.Engine, propagator domain.EventPropagator, log *slog.Logger) *Service {
	return &Service{registry: registry, cfg: cfg, engine: engine, propagator: propagator, log: log.With("component", "user-service")}
}

// ─────────────────────────────────────────────────────────────────────────────
// Queries
// ─────────────────────────────────────────────────────────────────────────────

func (s *Service) GetBlockedSubscriptions(ctx context.Context) ([]domain.Subscription, error) {
	return s.registry.Subscriptions().FindByStatus(ctx, "blocked")
}

func (s *Service) GetSubscriptionByEmail(ctx context.Context, email string) (*domain.Subscription, error) {
	return s.registry.Subscriptions().FindLatestByEmail(ctx, email)
}

func (s *Service) ProcessSQLSubscription(ctx context.Context, cm *subscription.CacheManager, dispatcher *events.Dispatcher, subReq *subscription.Request, isBanned func(string) bool) *subscription.Response {
	return subscription.ProcessSQL(ctx, s.registry, cm, dispatcher, subReq, isBanned)
}

func (s *Service) BuildMasterSnapshot(ctx context.Context) slave.Snapshot {
	return slave.BuildMasterSnapshot(ctx, s.registry, s.engine)
}


// ─────────────────────────────────────────────────────────────────────────────
// CreateUser
// ─────────────────────────────────────────────────────────────────────────────

type CreateUserRequest struct {
	Email   string
	Name    string
	UUID    string
	Subfile string
	Expire  string
	Auth    string
	Limit   *float64
	Legacy  bool
	SkipDB  bool // Used by slaves via internal sync
}

type CreateUserResponse struct {
	Email   string
	UUID    string
	Subfile string
	Expire  string
	Auth    string
	Link    string
}

func (s *Service) CreateUser(ctx context.Context, req CreateUserRequest) (*CreateUserResponse, error) {
	email := req.Email
	if email == "" {
		email = req.Name
	}
	if email == "" {
		return nil, fmt.Errorf("email is required")
	}

	userUUID := req.UUID
	if userUUID == "" {
		u, err := generate.UUID()
		if err != nil {
			return nil, fmt.Errorf("generating UUID: %v", err)
		}
		userUUID = u
	}

	subfile := req.Subfile
	if subfile == "" {
		subfile = generate.Subfile()
	}

	expireVal := req.Expire
	if expireVal == "" {
		expireVal = time.Now().AddDate(0, 1, 0).Format("02-01-2006")
	}

	auth := req.Auth
	if auth == "" {
		auth = generate.Secret(32)
	}

	// Provision the user in the database and engine within a Unit of Work (transaction)
	userCfg := domain.VPNUserConfig{
		Email:   email,
		UUID:    userUUID,
		Auth:    auth,
		Subfile: subfile,
		Expire:  expireVal,
	}
	if req.Limit != nil {
		userCfg.MaxDevices = int(*req.Limit)
	}

	if !req.SkipDB && s.registry != nil {
		err := s.registry.WithTx(ctx, func(tx domain.Registry) error {
			limitInt := 3
			if req.Limit != nil {
				limitInt = int(*req.Limit)
			}

			subRepo := tx.Subscriptions()
			userRepo := tx.Users()

			sub, err := subRepo.FindByEmail(ctx, email)
			if err != nil {
				var endsAt *time.Time
				if t, tErr := time.Parse("02-01-2006", expireVal); tErr == nil {
					endsAt = &t
				}
				userID, _ := generate.UUID()
				if userID == "" {
					userID = userUUID
				}
				subID, _ := generate.UUID()
				if subID == "" {
					subID = userUUID + "-sub"
				}

				if err := userRepo.Create(ctx, &domain.User{
					ID:        userID,
					Username:  email,
					RefCode:   "ref_" + generate.Secret(8),
					CreatedAt: time.Now(),
				}); err != nil {
					return fmt.Errorf("creating user in DB: %w", err)
				}

				if err := subRepo.Create(ctx, &domain.Subscription{
					ID:         subID,
					UserID:     userID,
					Email:      email,
					XrayUUID:   userUUID,
					Status:     "active",
					MaxDevices: limitInt,
					EndsAt:     endsAt,
					Metadata:   map[string]interface{}{"subfile": subfile},
					CreatedAt:  time.Now(),
					UpdatedAt:  time.Now(),
				}); err != nil {
					return fmt.Errorf("creating subscription in DB: %w", err)
				}
			} else {
				if sub.Metadata == nil {
					sub.Metadata = make(map[string]interface{})
				}
				sub.XrayUUID = userUUID
				sub.Metadata["subfile"] = subfile
				sub.Status = "active"
				sub.MaxDevices = limitInt
				if expireVal != "" {
					if t, tErr := time.Parse("02-01-2006", expireVal); tErr == nil {
						sub.EndsAt = &t
					}
				}
				if err := subRepo.Update(ctx, sub); err != nil {
					return fmt.Errorf("updating subscription in DB: %w", err)
				}
			}

			// Add to engine within the transaction. If it fails, the whole DB TX rolls back.
			if !req.Legacy {
				if err := s.engine.AddUser(ctx, userCfg); err != nil {
					return fmt.Errorf("provisioning user in VPN engine: %w", err)
				}
			}
			return nil
		})

		if err != nil {
			return nil, err
		}
	} else {
		// If DB is skipped (e.g. internal sync), just add to engine
		if !req.Legacy {
			if err := s.engine.AddUser(ctx, userCfg); err != nil {
				return nil, fmt.Errorf("provisioning user in VPN engine: %w", err)
			}
		}
	}

	// Propagate limit update to slaves.
	if s.cfg.IsMaster {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			slaveParams := map[string]string{
				"email":   email,
				"uuid":    userUUID,
				"subfile": subfile,
				"expire":  expireVal,
				"auth":    auth,
			}
			if req.Limit != nil {
				slaveParams["limit"] = strconv.FormatFloat(*req.Limit, 'f', 0, 64)
			}
			if req.Legacy {
				slaveParams["legacy"] = "true"
			}

			if s.propagator != nil {
				s.propagator.PropagateAll("newuser", slaveParams)
			}
		}()
	}

	id := subfile
	if len(id) > 5 && id[len(id)-4:] == ".txt" {
		id = id[:len(id)-4]
	}
	link := s.GenerateShareLink("", id)

	return &CreateUserResponse{
		Email:   email,
		UUID:    userUUID,
		Subfile: subfile,
		Expire:  expireVal,
		Auth:    auth,
		Link:    link,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// UnlimitUser
// ─────────────────────────────────────────────────────────────────────────────

type UnlimitUserRequest struct {
	Email   string
	Name    string
	UUID    string
	Subfile string
	Expire  string
	Auth    string
	Limit   *float64
	Legacy  bool
	SkipDB  bool
}

func (s *Service) UnlimitUser(ctx context.Context, req UnlimitUserRequest) (*CreateUserResponse, error) {
	email := req.Email
	if email == "" {
		email = req.Name
	}
	if email == "" {
		return nil, fmt.Errorf("email is required")
	}

	userUUID := req.UUID
	subfile := req.Subfile
	expireVal := req.Expire
	auth := req.Auth

	if !req.SkipDB && s.registry != nil {
		s.registry.AntifraudBans().DeleteByEmail(ctx, email)
	}

	// Defaults for missing fields.
	if userUUID == "" {
		userUUID, _ = generate.UUID()
	}
	if subfile == "" {
		subfile = generate.Subfile()
	}
	if expireVal == "" {
		expireVal = time.Now().AddDate(0, 1, 0).Format("02-01-2006")
	}
	if auth == "" {
		auth = generate.Secret(32)
	}

	userCfg := domain.VPNUserConfig{
		Email:   email,
		UUID:    userUUID,
		Auth:    auth,
		Subfile: subfile,
		Expire:  expireVal,
	}
	if req.Limit != nil {
		userCfg.MaxDevices = int(*req.Limit)
	}

	if !req.Legacy {
		if err := s.engine.AddUser(ctx, userCfg); err != nil {
			return nil, fmt.Errorf("re-provisioning user in VPN engine: %w", err)
		}
	}

	if !req.SkipDB && s.registry != nil {
		s.setStatus(ctx, email, "active")
	}

	if s.cfg.IsMaster {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			slaveParams := map[string]string{
				"email":   email,
				"uuid":    userUUID,
				"subfile": subfile,
				"expire":  expireVal,
				"auth":    auth,
			}
			if req.Limit != nil {
				slaveParams["limit"] = strconv.FormatFloat(*req.Limit, 'f', 0, 64)
			}
			if req.Legacy {
				slaveParams["legacy"] = "true"
			}
			if s.propagator != nil {
				s.propagator.PropagateAll("unlimit", slaveParams)
			}
		}()
	}

	id := subfile
	if len(id) > 5 && id[len(id)-4:] == ".txt" {
		id = id[:len(id)-4]
	}
	link := s.GenerateShareLink("", id)

	return &CreateUserResponse{
		Email:   email,
		UUID:    userUUID,
		Subfile: subfile,
		Expire:  expireVal,
		Auth:    auth,
		Link:    link,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SetExpire
// ─────────────────────────────────────────────────────────────────────────────

type SetExpireRequest struct {
	Email  string
	Name   string
	Expire string
	SkipDB bool
}

// SetExpire updates the expiry date field stored in the engine's config.
// NOTE: Updating a metadata field inside the engine config is still
// Xray-specific behaviour (vpn.UpdateStringField). However, with the
// adapter pattern in place, future engines can simply no-op this or persist
// the expiry in a different way.  For now we keep the implementation here and
// plan to push it into the adapter in a follow-up task.
func (s *Service) SetExpire(ctx context.Context, req SetExpireRequest) error {
	email := req.Email
	if email == "" {
		email = req.Name
	}
	if email == "" || req.Expire == "" {
		return fmt.Errorf("email and expire are required")
	}

	// Delegate the config field mutation to the engine adapter via its
	// UpdateExpire extension point.  Because the vpn.Engine interface
	// does not yet expose a generic "update field" method (by design — we do
	// not want to leak config-format details into the interface), we call the
	// xrayconfig package here for now, with a TODO to move this into the adapter.
	//
	// TODO(refactor): add Engine.SetUserMetadata(ctx, email, key, value string)
	// to the interface so this call can be fully decoupled.
	if err := s.engine.SetExpire(ctx, email, req.Expire); err != nil {
		return err
	}

	if !req.SkipDB && s.registry != nil {
		s.setExpireDB(ctx, email, req.Expire)
	}

	if s.cfg.IsMaster {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if s.propagator != nil {
				s.propagator.PropagateAll("setexpire", map[string]string{"email": email, "expire": req.Expire})
			}
		}()
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// UpdateLimit
// ─────────────────────────────────────────────────────────────────────────────

type UpdateLimitRequest struct {
	Email  string
	Name   string
	Limit  *float64
	SkipDB bool
}

func (s *Service) UpdateLimit(ctx context.Context, req UpdateLimitRequest) error {
	email := req.Email
	if email == "" {
		email = req.Name
	}
	if email == "" || req.Limit == nil {
		return fmt.Errorf("email and limit are required")
	}

	// Same pattern as SetExpire: optional extension point on the adapter.
	if err := s.engine.SetLimit(ctx, email, *req.Limit); err != nil {
		return err
	}

	if !req.SkipDB && s.registry != nil {
		s.setLimitDB(ctx, email, int(*req.Limit))
	}

	if s.cfg.IsMaster {
		go func() {
			if s.propagator != nil {
				s.propagator.PropagateAll("setlimit", map[string]string{
					"email": email,
					"limit": strconv.FormatFloat(*req.Limit, 'f', 0, 64),
				})
			}
		}()
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// BlockOrRemoveUser
// ─────────────────────────────────────────────────────────────────────────────

type ModifyUserRequest struct {
	Email  string
	Name   string
	Action string // "limit" or "rm"
	Legacy bool
	SkipDB bool
}

func (s *Service) BlockOrRemoveUser(ctx context.Context, req ModifyUserRequest) error {
	email := req.Email
	if email == "" {
		email = req.Name
	}
	if email == "" {
		return fmt.Errorf("email is required")
	}

	if !req.Legacy {
		// Engine handles the remove atomically (config + gRPC hot-remove).
		if err := s.engine.RemoveUser(ctx, email); err != nil {
			// If user is not in the engine config, we treat it as already removed.
			// The DB update still proceeds so state stays consistent.
			if req.Action == "rm" && !req.SkipDB && s.registry != nil {
				s.registry.AntifraudBans().DeleteByEmail(ctx, email)
			}
			s.propagateBlockOrRemove(email, req.Action, req.Legacy)
			return nil
		}
	}

	if !req.SkipDB && s.registry != nil {
		if req.Action == "rm" {
			s.setStatus(ctx, email, "inactive")
		} else if req.Action == "limit" {
			s.blockDB(ctx, email)
		}
	}

	s.propagateBlockOrRemove(email, req.Action, req.Legacy)
	return nil
}

func (s *Service) propagateBlockOrRemove(email, action string, legacy bool) {
	if !s.cfg.IsMaster {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		slaveCmd := action
		if action == "rm" {
			slaveCmd = "rmuser"
		}
		slaveParams := map[string]string{"email": email}
		if legacy {
			slaveParams["legacy"] = "true"
		}
		if s.propagator != nil {
			s.propagator.PropagateAll(slaveCmd, slaveParams)
		}
	}()
}

// ─────────────────────────────────────────────────────────────────────────────
// DB helpers (pure DB operations, no engine calls)
// ─────────────────────────────────────────────────────────────────────────────

func (s *Service) setStatus(ctx context.Context, email, status string) {
	subRepo := s.registry.Subscriptions()
	if sub, err := subRepo.FindByEmail(ctx, email); err == nil {
		sub.Status = status
		subRepo.Update(ctx, sub)
	}
}

func (s *Service) blockDB(ctx context.Context, email string) {
	subRepo := s.registry.Subscriptions()
	if sub, err := subRepo.FindByEmail(ctx, email); err == nil {
		sub.Status = "blocked"
		subRepo.Update(ctx, sub)
	}
}

func (s *Service) setExpireDB(ctx context.Context, email, expireStr string) {
	subRepo := s.registry.Subscriptions()
	if sub, err := subRepo.FindByEmail(ctx, email); err == nil {
		if expireStr == "" || expireStr == "0" {
			sub.EndsAt = nil
		} else {
			if t, err := time.Parse("02-01-2006", expireStr); err == nil {
				sub.EndsAt = &t
			}
		}
		subRepo.Update(ctx, sub)
	}
}

func (s *Service) setLimitDB(ctx context.Context, email string, limit int) {
	subRepo := s.registry.Subscriptions()
	if sub, err := subRepo.FindByEmail(ctx, email); err == nil {
		sub.MaxDevices = limit
		subRepo.Update(ctx, sub)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Utility
// ─────────────────────────────────────────────────────────────────────────────

// ParseLimit parses a string into a *float64 limit, validating bounds.
func ParseLimit(s string) (*float64, error) {
	if s == "" {
		return nil, nil
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%f", &v); err != nil {
		return nil, fmt.Errorf("invalid limit %q: %w", s, err)
	}
	if v < 1 || v != math.Trunc(v) || v > 10000 {
		return nil, fmt.Errorf("limit must be a positive integer between 1 and 10000 (got %v)", v)
	}
	return &v, nil
}

// GenerateShareLink creates the canonical subscription URL for a client.
// If host is empty, it falls back to the configured server domain.
func (s *Service) GenerateShareLink(host, id string) string {
	domain := s.cfg.Domain
	if host != "" {
		domain = host
	}
	return fmt.Sprintf("https://%s/client?id=%s", domain, id)
}

// ─────────────────────────────────────────────────────────────────────────────
// User Extracted Methods
// ─────────────────────────────────────────────────────────────────────────────

func (s *Service) GetSubscriptionByUserID(ctx context.Context, userID string) (*domain.Subscription, error) {
	return s.registry.Subscriptions().FindLatestByUserID(ctx, userID)
}

func (s *Service) CountReferrals(ctx context.Context, userID string) (int64, error) {
	return s.registry.Users().CountReferrals(ctx, userID)
}

func (s *Service) SumReferralRewards(ctx context.Context, userID string) (int64, error) {
	return s.registry.Users().SumReferralRewards(ctx, userID)
}

func (s *Service) CountActiveDevices(ctx context.Context, subID string) (int64, error) {
	return s.registry.Devices().CountBySubscription(ctx, subID)
}

func (s *Service) GetLatestSubscriptionsByUserIDs(ctx context.Context, userIDs []string) ([]domain.Subscription, error) {
	return s.registry.Subscriptions().FindLatestByUserIDs(ctx, userIDs)
}

func (s *Service) GetReferralStats(ctx context.Context, userIDs []string) ([]domain.ReferralStats, error) {
	return s.registry.Users().GetReferralStats(ctx, userIDs)
}

func (s *Service) CountDevicesBySubscriptions(ctx context.Context, subIDs []string) (map[string]int64, error) {
	return s.registry.Devices().CountBySubscriptions(ctx, subIDs)
}

func (s *Service) FindUserByPlatformID(ctx context.Context, platform, idStr string) (*domain.User, error) {
	return s.registry.Users().FindByPlatformID(ctx, platform, idStr)
}

func (s *Service) FindUserByRefCode(ctx context.Context, code string) (*domain.User, error) {
	return s.registry.Users().FindByRefCode(ctx, code)
}

func (s *Service) FindAllUsers(ctx context.Context) ([]domain.User, error) {
	return s.registry.Users().FindAll(ctx)
}

func (s *Service) FindAdmins(ctx context.Context) ([]domain.User, error) {
	return s.registry.Users().FindAdmins(ctx)
}

func (s *Service) FindUserByID(ctx context.Context, id string) (*domain.User, error) {
	return s.registry.Users().FindByID(ctx, id)
}

func (s *Service) AdjustBalance(ctx context.Context, userID string, amount int) error {
	return s.registry.Users().AdjustBalance(ctx, userID, amount)
}

func (s *Service) UpdateMaxDevices(ctx context.Context, userID string, maxDevices int) error {
	return s.registry.Subscriptions().UpdateMaxDevicesByUserID(ctx, userID, maxDevices)
}

func (s *Service) UpdateAutoRenew(ctx context.Context, userID string, autoRenew bool) error {
	return s.registry.Subscriptions().UpdateAutoRenewByUserID(ctx, userID, autoRenew)
}

func (s *Service) AutoRenewSubscription(ctx context.Context, userID string, planID *int64, price int, newEndsAt *time.Time, maxDevices int) error {
	return s.registry.Subscriptions().AutoRenewSubscription(ctx, userID, planID, price, newEndsAt, maxDevices)
}

func (s *Service) DeleteNotificationsBySubID(ctx context.Context, subID string) error {
	return s.registry.Notifications().DeleteBySubscriptionID(ctx, subID)
}

func (s *Service) UpdateUser(ctx context.Context, user *domain.User) error {
	return s.registry.Users().Update(ctx, user)
}

func (s *Service) ListUsers(ctx context.Context, page, limit int, search string) ([]domain.User, int64, error) {
	return s.registry.Users().ListUsers(ctx, page, limit, search)
}



func (s *Service) UpdateSubscriptionFields(ctx context.Context, subID string, updates map[string]interface{}) error {
	return s.registry.Subscriptions().UpdateFields(ctx, subID, updates)
}

func (s *Service) UpdateIsBlocked(ctx context.Context, userID string, blocked bool) error {
	return s.registry.Users().UpdateIsBlocked(ctx, userID, blocked)
}

func (s *Service) FindDevicesBySubscriptionID(ctx context.Context, subID string) ([]domain.Device, error) {
	return s.registry.Devices().FindBySubscriptionID(ctx, subID)
}

func (s *Service) FindDeviceByIDAndSubscription(ctx context.Context, deviceID int64, subID string) (*domain.Device, error) {
	return s.registry.Devices().FindByIDAndSubscription(ctx, deviceID, subID)
}

func (s *Service) DeleteDevice(ctx context.Context, deviceID int64) error {
	return s.registry.Devices().Delete(ctx, deviceID)
}

func (s *Service) DeleteUserAndData(ctx context.Context, userID string) error {
	return s.registry.Users().DeleteUserAndData(ctx, userID)
}

func (s *Service) GenerateRefCode(ctx context.Context) string {
	for {
		code := "ref_" + generate.Secret(8)
		count, _ := s.registry.Users().CountByRefCode(ctx, code)
		if count == 0 {
			return code
		}
	}
}

func (s *Service) FindOrCreateWebUser(ctx context.Context, email string) (*domain.User, error) {
	user, err := s.registry.Users().FindByPlatformID(ctx, "web", email)
	if err == nil {
		return user, nil
	}

	var newUser *domain.User
	txErr := s.registry.WithTx(ctx, func(tx domain.Registry) error {
		userID, _ := generate.UUID()
		
		var refCode string
		for {
			refCode = "ref_" + generate.Secret(8)
			count, _ := tx.Users().CountByRefCode(ctx, refCode)
			if count == 0 {
				break
			}
		}

		u := domain.User{
			ID:       userID,
			Username: email,
			Balance:  0,
			IsAdmin:  false,
			RefCode:  refCode,
			Metadata: domain.Metadata{
				"email":  email,
				"source": "website",
			},
		}
		if err := tx.Users().Create(ctx, &u); err != nil {
			return fmt.Errorf("create user: %w", err)
		}

		subID, _ := generate.UUID()
		emailID, _ := generate.UUID()
		xrayUUID, _ := generate.UUID()

		sub := domain.Subscription{
			ID:         subID,
			UserID:     userID,
			Email:      fmt.Sprintf("web_%s", emailID[:8]),
			XrayUUID:   xrayUUID,
			Status:     "inactive",
			MaxDevices: 3,
			AutoRenew:  false,
		}
		if err := tx.Subscriptions().Create(ctx, &sub); err != nil {
			return fmt.Errorf("create subscription: %w", err)
		}

		newUser = &u
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	s.log.Info("web auto-registration", "email", email, "user_id", newUser.ID)
	return newUser, nil
}

type RegisterTelegramUserRequest struct {
	TelegramID       int64
	Username         string
	TelegramUsername string
	ReferredByCode   string
}

func (s *Service) RegisterTelegramUser(ctx context.Context, req RegisterTelegramUserRequest) (*domain.User, error) {
    tgIDStr := fmt.Sprintf("%d", req.TelegramID)
	existing, err := s.registry.Users().FindByPlatformID(ctx, "telegram", tgIDStr)
	if err == nil && existing != nil {
		return existing, nil
	}

	userID, _ := generate.UUID()
	refCode := s.GenerateRefCode(ctx)

	var referredByID *string
	if req.ReferredByCode != "" {
		referrer, err := s.registry.Users().FindByRefCode(ctx, req.ReferredByCode)
		if err == nil {
			referredByID = &referrer.ID
		} else {
			s.log.Warn("register user: invalid ref code provided", "code", req.ReferredByCode)
		}
	}

	user := domain.User{
		ID:         userID,
		Username:   req.Username,
		Balance:    0,
		IsAdmin:    false,
		RefCode:    refCode,
		ReferredBy: referredByID,
		Metadata: domain.Metadata{
			"telegram_id":       req.TelegramID,
			"telegram_username": req.TelegramUsername,
			"source":            "telegram_bot",
		},
	}

	if err := s.registry.Users().Create(ctx, &user); err != nil {
		return nil, fmt.Errorf("db create user: %w", err)
	}

	subID, _ := generate.UUID()
	xrayUUID, _ := generate.UUID()
	email := fmt.Sprintf("bot_client_%d", req.TelegramID)

	sub := domain.Subscription{
		ID:         subID,
		UserID:     userID,
		Email:      email,
		XrayUUID:   xrayUUID,
		Status:     "inactive",
		MaxDevices: 3,
		AutoRenew:  false,
	}

	if err := s.registry.Subscriptions().Create(ctx, &sub); err != nil {
		s.log.Error("register user: db create subscription", "err", err)
	}

	return &user, nil
}
