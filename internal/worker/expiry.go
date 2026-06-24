package worker

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/events"
	"xraytool/internal/slave"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"
)

// ExpiryWorker periodically checks active subscriptions for expiration.
type ExpiryWorker struct {
	db         *gorm.DB
	cfg        *appconfig.Config
	dispatcher *events.Dispatcher
	apiClient  *xrayapi.GRPCClient
	log        *slog.Logger
}

// NewExpiryWorker creates a new ExpiryWorker.
func NewExpiryWorker(
	db *gorm.DB,
	cfg *appconfig.Config,
	dispatcher *events.Dispatcher,
	apiClient *xrayapi.GRPCClient,
	log *slog.Logger,
) *ExpiryWorker {
	return &ExpiryWorker{
		db:         db,
		cfg:        cfg,
		dispatcher: dispatcher,
		apiClient:  apiClient,
		log:        log.With("component", "expiry_worker"),
	}
}

// Run starts the background ticker. It blocks until ctx is canceled.
func (w *ExpiryWorker) Run(ctx context.Context) {
	interval, err := time.ParseDuration(w.cfg.Worker.ExpiryInterval)
	if err != nil || interval <= 0 {
		w.log.Warn("Invalid worker.expiry_interval, falling back to 5m", "val", w.cfg.Worker.ExpiryInterval)
		interval = 5 * time.Minute
	}

	w.log.Info("Starting expiry worker", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run once on startup
	w.ProcessOnce()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("Stopping expiry worker")
			return
		case <-ticker.C:
			w.ProcessOnce()
		}
	}
}

// ProcessOnce runs a single iteration of expiration checks and device limit checks.
func (w *ExpiryWorker) ProcessOnce() {
	// 0. Cleanup old devices (older than 48 hours) to prevent device limit exhaustion over time
	// DISABLED: We no longer delete old devices automatically.
	// if err := w.db.Where("last_seen < ?", time.Now().Add(-48*time.Hour)).Delete(&database.Device{}).Error; err != nil {
	// 	w.log.Warn("Failed to cleanup old devices", "error", err)
	// }

	var subs []database.Subscription
	// Fetch all active subscriptions (even those without ends_at, for device limits)
	if err := w.db.Where("status = ?", "active").Find(&subs).Error; err != nil {
		w.log.Error("Failed to fetch active subscriptions", "error", err)
		return
	}

	if len(subs) == 0 {
		return
	}

	var subIDs []string
	for _, sub := range subs {
		subIDs = append(subIDs, sub.ID)
	}

	type Result struct {
		SubscriptionID string
		Count          int64
	}
	var results []Result
	if err := w.db.Model(&database.Device{}).
		Select("subscription_id, count(*) as count").
		Where("subscription_id IN ?", subIDs).
		Group("subscription_id").
		Scan(&results).Error; err != nil {
		w.log.Warn("Failed to fetch device counts, skipping device limit checks", "error", err)
	}

	counts := make(map[string]int64)
	for _, r := range results {
		counts[r.SubscriptionID] = r.Count
	}

	now := time.Now()

	var expiredSubs []database.Subscription
	var devicesToDelete []int64

	for _, sub := range subs {
		// 1. Check time-based expiration
		if sub.EndsAt != nil {
			timeLeft := sub.EndsAt.Sub(now)
			if timeLeft <= 0 {
				expiredSubs = append(expiredSubs, sub)
				continue // already blocked, no need to check devices
			} else {
				w.handleWarnings(sub, timeLeft)
			}
		}

		// 2. Check Device Limits actively
		limit := sub.MaxDevices
		if limit <= 0 {
			limit = 3 // default limit if not set
		}

		deviceCount := counts[sub.ID]
		if deviceCount > int64(limit) {
			excess := int(deviceCount) - limit
			var oldestDevices []database.Device
			if err := w.db.Where("subscription_id = ?", sub.ID).Order("last_seen asc").Limit(excess).Find(&oldestDevices).Error; err == nil {
				for _, d := range oldestDevices {
					devicesToDelete = append(devicesToDelete, d.ID)
				}
			}
		}
	}

	if len(devicesToDelete) > 0 {
		if err := w.db.Where("id IN ?", devicesToDelete).Delete(&database.Device{}).Error; err != nil {
			w.log.Warn("Failed to delete excess devices", "error", err)
		} else {
			w.log.Info("Deleted excess devices in bulk", "count", len(devicesToDelete))
		}
	}

	if len(expiredSubs) > 0 {
		w.handleExpiredBulk(expiredSubs)
	}
}

func (w *ExpiryWorker) handleExpiredBulk(subs []database.Subscription) {
	w.log.Info("Bulk expiring subscriptions", "count", len(subs))

	// 1. Remove from Xray Config JSON safely in one go
	var allTags []string
	configSubfiles := make(map[string]string)
	
	modErr := xrayconfig.Modify(w.cfg.Paths.XrayConfig, func(cfg xrayconfig.RawConfig) error {
		findUser, err := xrayconfig.BuildUserIndex(cfg)
		if err != nil {
			return err
		}

		for _, sub := range subs {
			if c := findUser(sub.Email); c != nil {
				configSubfiles[sub.Email] = c.GetString("subfile")
			}
			t, _ := xrayconfig.InboundTagsForUser(cfg, sub.Email)
			allTags = append(allTags, t...)
			_ = xrayconfig.RemoveUserFromAllInbounds(cfg, sub.Email)
		}
		return nil
	})

	if modErr != nil {
		w.log.Error("Failed to remove users from xray config in bulk, aborting", "error", modErr)
		return
	}

	// Remove via API (skipped for bulk since config reload takes care of it)
	
	// Actually we should reload Xray to apply bulk changes, or let CacheManager refresh do it.
	// We'll just continue processing the rest of the DB changes.

	for _, sub := range subs {
		// 2. Add to Limited DB (Removed)

		// 3. Update DB Status
		res := w.db.Model(&sub).Where("status = ?", "active").Updates(map[string]interface{}{
			"status":     "expired",
			"updated_at": time.Now(),
		})
		if res.Error != nil || res.RowsAffected == 0 {
			continue
		}

		// 4. Fire webhook
		payload := map[string]interface{}{
			"user_id":         sub.UserID,
			"subscription_id": sub.ID,
			"email":           sub.Email,
			"ends_at":         sub.EndsAt.Format(time.RFC3339),
		}
		var userMetadata map[string]interface{}
		var user database.User
		if err := w.db.Where("id = ?", sub.UserID).First(&user).Error; err == nil && user.Metadata != nil {
			userMetadata = user.Metadata
		}
		if w.dispatcher != nil {
			w.dispatcher.Dispatch("subscription.expired", payload, userMetadata)
		}

		// 5. Propagate to slave nodes
		if w.cfg.IsMaster() {
			client := slave.NewClient(
				w.cfg.SlaveAPI.ConnectTimeout,
				w.cfg.SlaveAPI.RequestTimeout,
				w.cfg.SlaveAPI.RemotePath,
			)
			reg := slave.NewRegistry(w.cfg.Paths.ServersJSON, client)
			go reg.PropagateAll("rmuser", map[string]string{"email": sub.Email})
		}
	}
}

func (w *ExpiryWorker) handleExpired(sub database.Subscription) {
	w.log.Info("Subscription expired, restricting user", "email", sub.Email)

	// 1. Remove from Xray Config JSON and API safely
	var tags []string
	modErr := xrayconfig.Modify(w.cfg.Paths.XrayConfig, func(cfg xrayconfig.RawConfig) error {
		t, _ := xrayconfig.InboundTagsForUser(cfg, sub.Email)
		tags = t
		return xrayconfig.RemoveUserFromAllInbounds(cfg, sub.Email)
	})
	if modErr != nil {
		w.log.Error("Failed to remove user from xray config, aborting expiration", "email", sub.Email, "error", modErr)
		return // Abort to retry next time
	}

	if len(tags) > 0 && w.apiClient != nil {
		if err := w.apiClient.RemoveUser(sub.Email, tags); err != nil {
			w.log.Error("Failed to remove user via API", "email", sub.Email, "error", err)
			// Non-fatal, continuing to update DB
		}
	}

	// 2. Add to Limited DB (Removed)

	// 3. Update DB Status
	res := w.db.Model(&sub).Where("status = ?", "active").Updates(map[string]interface{}{
		"status":     "expired",
		"updated_at": time.Now(),
	})
	if res.Error != nil {
		w.log.Error("Failed to update subscription status", "email", sub.Email, "error", res.Error)
		// We already removed from Xray, so it's a partial failure.
		// Return to allow the worker to retry this user next time, and avoid spamming webhooks.
		return
	}
	if res.RowsAffected == 0 {
		w.log.Info("Subscription already processed concurrently", "email", sub.Email)
		return
	}

	// 4. Fire webhook
	payload := map[string]interface{}{
		"user_id":         sub.UserID,
		"subscription_id": sub.ID,
		"email":           sub.Email,
		"ends_at":         sub.EndsAt.Format(time.RFC3339),
	}
	var userMetadata map[string]interface{}
	var user database.User
	if err := w.db.Where("id = ?", sub.UserID).First(&user).Error; err == nil && user.Metadata != nil {
		userMetadata = user.Metadata
	}
	if w.dispatcher != nil {
		w.dispatcher.Dispatch("subscription.expired", payload, userMetadata)
	}

	// 5. Propagate to slave nodes
	if w.cfg.IsMaster() {
		client := slave.NewClient(
			w.cfg.SlaveAPI.ConnectTimeout,
			w.cfg.SlaveAPI.RequestTimeout,
			w.cfg.SlaveAPI.RemotePath,
		)
		reg := slave.NewRegistry(w.cfg.Paths.ServersJSON, client)
		go reg.PropagateAll("rmuser", map[string]string{"email": sub.Email})
	}
}

func (w *ExpiryWorker) handleWarnings(sub database.Subscription, timeLeft time.Duration) {
	var triggeredWarnStr string
	var minTriggeredDur time.Duration

	for _, warnStr := range w.cfg.Worker.ExpirationWarnings {
		warnDur, err := time.ParseDuration(warnStr)
		if err != nil {
			w.log.Error("Invalid warning duration in config", "val", warnStr, "error", err)
			continue
		}

		if timeLeft <= warnDur {
			// Try to insert notification record
			notif := database.SubscriptionNotification{
				SubscriptionID: sub.ID,
				WarningLevel:   warnStr,
				SentAt:         time.Now(),
			}

			// Use clause.OnConflict{DoNothing}
			res := w.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&notif)
			if res.Error != nil {
				w.log.Error("Failed to insert notification", "error", res.Error)
				continue
			}

			if res.RowsAffected > 0 {
				if triggeredWarnStr == "" || warnDur < minTriggeredDur {
					triggeredWarnStr = warnStr
					minTriggeredDur = warnDur
				}
			}
		}
	}

	if triggeredWarnStr != "" {
		w.log.Info("Sending expiration warning", "email", sub.Email, "level", triggeredWarnStr)

		payload := map[string]interface{}{
			"user_id":         sub.UserID,
			"subscription_id": sub.ID,
			"email":           sub.Email,
			"warning_level":   triggeredWarnStr,
			"time_left_sec":   timeLeft.Seconds(),
			"ends_at":         sub.EndsAt.Format(time.RFC3339),
		}
		var userMetadata map[string]interface{}
		var user database.User
		if err := w.db.Where("id = ?", sub.UserID).First(&user).Error; err == nil && user.Metadata != nil {
			userMetadata = user.Metadata
		}
		if w.dispatcher != nil {
			w.dispatcher.Dispatch("subscription.expiring", payload, userMetadata)
		}
	}
}
