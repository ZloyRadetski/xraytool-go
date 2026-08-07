package worker

import (
	"context"
	"log/slog"
	"time"

	"xraytool/internal/plugins/core/payment"
)

// ScrubberWorker periodically removes sensitive data (like old external payment IDs) to protect user anonymity.
type ScrubberWorker struct {
	paymentSvc *payment.Service
	log        *slog.Logger
	interval   time.Duration
	retention  time.Duration
}

// NewScrubberWorker creates a new ScrubberWorker.
// It will scrub data older than the retention duration, checking every interval.
func NewScrubberWorker(paymentSvc *payment.Service, log *slog.Logger) *ScrubberWorker {
	return &ScrubberWorker{
		paymentSvc: paymentSvc,
		log:        log.With("component", "scrubber_worker"),
		interval:   time.Hour,      // Check every hour
		retention:  24 * time.Hour, // Keep external_ids for 24 hours
	}
}

// Run starts the background ticker. It blocks until ctx is canceled.
func (w *ScrubberWorker) Run(ctx context.Context) {
	w.log.Info("Starting scrubber worker", "interval", w.interval, "retention", w.retention)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// Run once on startup
	w.scrub(ctx)

	for {
		select {
		case <-ctx.Done():
			w.log.Info("Stopping scrubber worker")
			return
		case <-ticker.C:
			w.scrub(ctx)
		}
	}
}

func (w *ScrubberWorker) scrub(ctx context.Context) {
	// Scrub old payment external IDs
	_, err := w.paymentSvc.ScrubOldPayments(ctx, w.retention)
	if err != nil {
		w.log.Error("Failed to scrub old payments", "err", err)
	}
}
