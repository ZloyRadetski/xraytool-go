package worker

import (
	"context"
	"testing"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/domain"
)

func TestExpiryWorker_Run(t *testing.T) {
	db := setupTestDB(t)
	cfg := &appconfig.Config{
		Worker: appconfig.WorkerConf{
			ExpiryInterval: "100ms",
		},
	}
	worker := setupTestWorker(t, db, cfg)

	ctx, cancel := context.WithCancel(context.Background())

	// Start the worker in a goroutine
	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()

	// Let it run for a bit to hit the ticker
	time.Sleep(250 * time.Millisecond)

	// Cancel the context to stop the worker
	cancel()

	// Wait for worker to exit
	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("worker.Run did not return after context cancellation")
	}
}

func TestExpiryWorker_Run_InvalidInterval(t *testing.T) {
	db := setupTestDB(t)
	cfg := &appconfig.Config{
		Worker: appconfig.WorkerConf{
			ExpiryInterval: "invalid", // Should fallback to 5m
		},
	}
	worker := setupTestWorker(t, db, cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()

	// Let it run once on startup
	time.Sleep(100 * time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker.Run did not return after context cancellation")
	}
}
func TestExpiryWorker_HandleExpired(t *testing.T) {
	db := setupTestDB(t)

	endsAt := time.Now().Add(-1 * time.Hour)
	sub := database.Subscription{
		ID:       "sub-handle",
		UserID:   "user-handle",
		XrayUUID: "uuid-sub-1",
		Email:    "test@example.com",
		Status:   "active",
		EndsAt:   &endsAt,
	}
	db.Create(&sub)

	cfg := &appconfig.Config{}
	worker := setupTestWorker(t, db, cfg)

	worker.handleExpired(context.Background(), domain.Subscription{
		ID:       sub.ID,
		UserID:   sub.UserID,
		XrayUUID: sub.XrayUUID,
		Email:    sub.Email,
		Status:   sub.Status,
		EndsAt:   sub.EndsAt,
	})

	var updatedSub database.Subscription
	db.First(&updatedSub, "id = ?", "sub-handle")

	if updatedSub.Status != "expired" {
		t.Errorf("Expected status to be 'expired', got '%s'", updatedSub.Status)
	}
}
