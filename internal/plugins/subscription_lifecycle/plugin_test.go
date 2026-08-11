package subscription_lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"xraytool/internal/database"
	"xraytool/internal/domain"
)

func TestExtendSubscriptionOwnsExpiryTransaction(t *testing.T) {
	db, err := database.NewConnection(database.Config{
		Driver:      "sqlite",
		SQLitePath:  "file:subscription-lifecycle-extension?mode=memory&cache=shared",
		AutoMigrate: true,
		Silent:      true,
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	defer sqlDB.Close()

	registry := database.NewRegistry(db)
	currentEnd := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, registry.Subscriptions().Create(context.Background(), &domain.Subscription{
		ID: "subscription-lifecycle-extension", UserID: "user-1",
		Email: "user@example.test", UUID: "uuid-1", Status: "expired", EndsAt: &currentEnd,
	}))

	p := &Plugin{registry: registry}
	require.NoError(t, p.ExtendSubscription(context.Background(), "subscription-lifecycle-extension", 1))

	updated, err := registry.Subscriptions().FindByID(context.Background(), "subscription-lifecycle-extension")
	require.NoError(t, err)
	require.Equal(t, "active", updated.Status)
	require.NotNil(t, updated.EndsAt)
	require.WithinDuration(t, currentEnd.AddDate(0, 1, 0), *updated.EndsAt, time.Second)

	err = p.ExtendSubscription(context.Background(), "subscription-lifecycle-extension", 0)
	require.ErrorContains(t, err, "months must be positive")
}
