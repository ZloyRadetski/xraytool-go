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
	"xraytool/internal/userdb"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"
)

// ExpiryWorker periodically checks active subscriptions for expiration.
type ExpiryWorker struct {
	db         *gorm.DB
	cfg        *appconfig.Config
	dispatcher *events.Dispatcher
	apiClient  *xrayapi.Client
	log        *slog.Logger
}

// NewExpiryWorker creates a new ExpiryWorker.
func NewExpiryWorker(
	db *gorm.DB,
	cfg *appconfig.Config,
	dispatcher *events.Dispatcher,
	apiClient *xrayapi.Client,
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

// ProcessOnce runs a single iteration of expiration checks.
func (w *ExpiryWorker) ProcessOnce() {
	var subs []database.Subscription
	// Only fetch active subscriptions that have an EndsAt date
	if err := w.db.Where("status = ? AND ends_at IS NOT NULL", "active").Find(&subs).Error; err != nil {
		w.log.Error("Failed to fetch active subscriptions", "error", err)
		return
	}

	now := time.Now()

	for _, sub := range subs {
		timeLeft := sub.EndsAt.Sub(now)

		if timeLeft <= 0 {
			w.handleExpired(sub)
		} else {
			w.handleWarnings(sub, timeLeft)
		}
	}
}

func (w *ExpiryWorker) handleExpired(sub database.Subscription) {
	w.log.Info("Subscription expired, restricting user", "email", sub.Email)

	// 1. Update DB Status
	if err := w.db.Model(&sub).Updates(map[string]interface{}{
		"status":     "expired",
		"updated_at": time.Now(),
	}).Error; err != nil {
		w.log.Error("Failed to update subscription status", "email", sub.Email, "error", err)
		return
	}

	// 2. Remove from Xray Config JSON and API safely
	var tags []string
	modErr := xrayconfig.Modify(w.cfg.Paths.XrayConfig, func(cfg xrayconfig.RawConfig) error {
		t, _ := xrayconfig.InboundTagsForUser(cfg, sub.Email)
		tags = t
		return xrayconfig.RemoveUserFromAllInbounds(cfg, sub.Email)
	})
	if modErr != nil {
		w.log.Error("Failed to remove user from xray config", "email", sub.Email, "error", modErr)
	} else if len(tags) > 0 && w.apiClient != nil {
		if err := w.apiClient.RemoveUser(sub.Email, tags); err != nil {
			w.log.Error("Failed to remove user via API", "email", sub.Email, "error", err)
		}
	}

	// 4. Add to Limited DB
	limitDB := userdb.New(w.cfg.Paths.LimitedDB)
	subfile := ""
	if sub.Metadata != nil {
		if sf, ok := sub.Metadata["subfile"].(string); ok {
			subfile = sf
		}
	}
	if err := limitDB.Upsert(userdb.Entry{
		Email:   sub.Email,
		Subfile: subfile,
		Limit:   nil, // Expired, not just device limited
	}); err != nil {
		w.log.Error("Failed to update limited users DB", "email", sub.Email, "error", err)
	}

	// 5. Fire webhook
	payload := map[string]interface{}{
		"user_id":         sub.UserID,
		"subscription_id": sub.ID,
		"email":           sub.Email,
		"ends_at":         sub.EndsAt.Format(time.RFC3339),
	}
	if w.dispatcher != nil {
		w.dispatcher.Dispatch("subscription.expired", payload, nil)
	}
}

func (w *ExpiryWorker) handleWarnings(sub database.Subscription, timeLeft time.Duration) {
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
				// We actually inserted it, so trigger webhook
				w.log.Info("Sending expiration warning", "email", sub.Email, "level", warnStr)

				payload := map[string]interface{}{
					"user_id":         sub.UserID,
					"subscription_id": sub.ID,
					"email":           sub.Email,
					"warning_level":   warnStr,
					"time_left_sec":   timeLeft.Seconds(),
					"ends_at":         sub.EndsAt.Format(time.RFC3339),
				}
				if w.dispatcher != nil {
					w.dispatcher.Dispatch("subscription.expiring", payload, nil)
				}
			}
		}
	}
}
