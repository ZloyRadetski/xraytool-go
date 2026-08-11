package worker

import (
	"context"
	"log/slog"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/events"
)

// ExpiryWorker periodically checks active subscriptions for expiration.
type ExpiryWorker struct {
	registry   domain.Registry
	cfg        *appconfig.Config
	dispatcher *events.Dispatcher
	engine     domain.Engine
	propagator domain.EventPropagator
	log        *slog.Logger
}

// NewExpiryWorker creates a new ExpiryWorker.
func NewExpiryWorker(
	registry domain.Registry,
	cfg *appconfig.Config,
	dispatcher *events.Dispatcher,
	engine domain.Engine,
	log *slog.Logger,
	propagators ...domain.EventPropagator,
) *ExpiryWorker {
	var propagator domain.EventPropagator
	if len(propagators) > 0 {
		propagator = propagators[0]
	}
	return &ExpiryWorker{
		registry:   registry,
		cfg:        cfg,
		dispatcher: dispatcher,
		engine:     engine,
		propagator: propagator,
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
	w.ProcessOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			w.log.Info("Stopping expiry worker")
			return
		case <-ticker.C:
			w.ProcessOnce(ctx)
		}
	}
}

// ProcessOnce runs a single iteration of expiration checks and device limit checks.
func (w *ExpiryWorker) ProcessOnce(ctx context.Context) {
	// 0. Cleanup old devices (older than 48 hours) to prevent device limit exhaustion over time
	// DISABLED: We no longer delete old devices automatically.
	// if err := w.db.Where("last_seen < ?", time.Now().Add(-48*time.Hour)).Delete(&database.Device{}).Error; err != nil {
	// 	w.log.Warn("Failed to cleanup old devices", "error", err)
	// }

	var subs []domain.Subscription
	// Fetch all active subscriptions (even those without ends_at, for device limits)
	subs, err := w.registry.Subscriptions().FindByStatus(ctx, "active")
	if err != nil {
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

	counts, err := w.registry.Devices().CountBySubscriptions(ctx, subIDs)
	if err != nil {
		w.log.Warn("Failed to fetch device counts, skipping device limit checks", "error", err)
	}

	now := time.Now()

	var expiredSubs []domain.Subscription
	var devicesToDelete []int64

	for _, sub := range subs {
		// 1. Check time-based expiration
		if sub.EndsAt != nil {
			timeLeft := sub.EndsAt.Sub(now)
			if timeLeft <= 0 {
				expiredSubs = append(expiredSubs, sub)
				continue // already blocked, no need to check devices
			} else {
				w.handleWarnings(ctx, sub, timeLeft)
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
			oldestDevices, err := w.registry.Devices().FindOldestBySubscription(ctx, sub.ID, excess)
			if err == nil {
				for _, d := range oldestDevices {
					devicesToDelete = append(devicesToDelete, d.ID)
				}
			}
		}
	}

	if len(devicesToDelete) > 0 {
		if err := w.registry.Devices().DeleteByIDs(ctx, devicesToDelete); err != nil {
			w.log.Warn("Failed to delete excess devices", "error", err)
		} else {
			w.log.Info("Deleted excess devices in bulk", "count", len(devicesToDelete))
		}
	}

	if len(expiredSubs) > 0 {
		w.handleExpiredBulk(ctx, expiredSubs)
	}
}

func (w *ExpiryWorker) handleExpiredBulk(ctx context.Context, subs []domain.Subscription) {
	w.log.Info("Bulk expiring subscriptions", "count", len(subs))

	// 1. Collect emails and remove from VPN engine in a single atomic bulk call.
	// The engine adapter handles config write + gRPC hot-remove internally.
	emails := make([]string, 0, len(subs))
	for _, sub := range subs {
		emails = append(emails, sub.Email)
	}

	if err := w.engine.RemoveUsersBulk(ctx, emails); err != nil {
		w.log.Error("Failed to bulk-remove users from VPN engine, aborting", "error", err)
		return
	}

	for _, sub := range subs {
		// 2. Update DB Status
		updated, err := w.registry.Subscriptions().UpdateStatusIfActive(ctx, sub.ID, "expired")
		if err != nil || !updated {
			continue
		}

		// 3. Fire webhook
		payload := map[string]interface{}{
			"user_id":         sub.UserID,
			"subscription_id": sub.ID,
			"email":           sub.Email,
			"ends_at":         sub.EndsAt.Format(time.RFC3339),
		}
		var userMetadata map[string]interface{}
		user, err := w.registry.Users().FindByID(ctx, sub.UserID)
		if err == nil && user.Metadata != nil {
			userMetadata = user.Metadata
		}
		if w.dispatcher != nil {
			w.dispatcher.Dispatch("subscription.expired", payload, userMetadata)
		}

		// 4. Propagate to slave nodes
		if w.cfg.IsMaster() && w.propagator != nil {
			go w.propagator.PropagateAll("rmuser", map[string]string{"email": sub.Email})
		}
	}
}

func (w *ExpiryWorker) handleExpired(ctx context.Context, sub domain.Subscription) {
	w.log.Info("Subscription expired, restricting user", "email", sub.Email)

	// 1. Remove user from VPN engine (config write + gRPC hot-remove in one call).
	// The engine adapter is idempotent: if the user is already absent it returns nil.
	if err := w.engine.RemoveUser(ctx, sub.Email); err != nil {
		w.log.Error("Failed to remove user from VPN engine, aborting expiration",
			"email", sub.Email, "error", err)
		return // Abort to retry next tick
	}

	// 2. Update DB Status
	updated, err := w.registry.Subscriptions().UpdateStatusIfActive(ctx, sub.ID, "expired")
	if err != nil {
		w.log.Error("Failed to update subscription status", "email", sub.Email, "error", err)
		// User already removed from engine — partial failure.
		// Return to allow the worker to retry this user next time.
		return
	}
	if !updated {
		w.log.Info("Subscription already processed concurrently", "email", sub.Email)
		return
	}

	// 3. Fire webhook
	payload := map[string]interface{}{
		"user_id":         sub.UserID,
		"subscription_id": sub.ID,
		"email":           sub.Email,
		"ends_at":         sub.EndsAt.Format(time.RFC3339),
	}
	var userMetadata map[string]interface{}
	user, err := w.registry.Users().FindByID(ctx, sub.UserID)
	if err == nil && user.Metadata != nil {
		userMetadata = user.Metadata
	}
	if w.dispatcher != nil {
		w.dispatcher.Dispatch("subscription.expired", payload, userMetadata)
	}

	// 4. Propagate to slave nodes
	if w.cfg.IsMaster() && w.propagator != nil {
		go w.propagator.PropagateAll("rmuser", map[string]string{"email": sub.Email})
	}
}

func (w *ExpiryWorker) handleWarnings(ctx context.Context, sub domain.Subscription, timeLeft time.Duration) {
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
			notif := domain.SubscriptionNotification{
				SubscriptionID: sub.ID,
				WarningLevel:   warnStr,
				SentAt:         time.Now(),
			}

			// Use clause.OnConflict{DoNothing} (abstracted)
			inserted, err := w.registry.Notifications().CreateIfNotExists(ctx, &notif)
			if err != nil {
				w.log.Error("Failed to insert notification", "error", err)
				continue
			}

			if inserted {
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
		user, err := w.registry.Users().FindByID(ctx, sub.UserID)
		if err == nil && user.Metadata != nil {
			userMetadata = user.Metadata
		}
		if w.dispatcher != nil {
			w.dispatcher.Dispatch("subscription.expiring", payload, userMetadata)
		}
	}
}
