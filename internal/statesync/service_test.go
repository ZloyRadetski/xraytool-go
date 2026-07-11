package statesync_test

import (
	"context"
	"testing"
	"time"

	"xraytool/internal/domain"
	"xraytool/internal/mocks"
	"xraytool/internal/statesync"
	"xraytool/internal/vpn"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockSubscriptionRepository is a simple helper mock for SubscriptionRepository
type MockSubscriptionRepository struct {
	mock.Mock
	domain.SubscriptionRepository
}

func (m *MockSubscriptionRepository) Create(ctx context.Context, sub *domain.Subscription) error {
	return m.Called(ctx, sub).Error(0)
}
func (m *MockSubscriptionRepository) Update(ctx context.Context, sub *domain.Subscription) error {
	return m.Called(ctx, sub).Error(0)
}
func (m *MockSubscriptionRepository) FindByID(ctx context.Context, id string) (*domain.Subscription, error) {
	ret := m.Called(ctx, id)
	return ret.Get(0).(*domain.Subscription), ret.Error(1)
}
func (m *MockSubscriptionRepository) FindByEmail(ctx context.Context, email string) (*domain.Subscription, error) {
	ret := m.Called(ctx, email)
	return ret.Get(0).(*domain.Subscription), ret.Error(1)
}
func (m *MockSubscriptionRepository) FindByStatus(ctx context.Context, status string) ([]domain.Subscription, error) {
	ret := m.Called(ctx, status)
	return ret.Get(0).([]domain.Subscription), ret.Error(1)
}
func (m *MockSubscriptionRepository) FindLatestByEmail(ctx context.Context, email string) (*domain.Subscription, error) {
	ret := m.Called(ctx, email)
	return ret.Get(0).(*domain.Subscription), ret.Error(1)
}
func (m *MockSubscriptionRepository) FindLatestByUserID(ctx context.Context, userID string) (*domain.Subscription, error) {
	ret := m.Called(ctx, userID)
	return ret.Get(0).(*domain.Subscription), ret.Error(1)
}
func (m *MockSubscriptionRepository) FindLatestByUserIDs(ctx context.Context, userIDs []string) ([]domain.Subscription, error) {
	ret := m.Called(ctx, userIDs)
	return ret.Get(0).([]domain.Subscription), ret.Error(1)
}
func (m *MockSubscriptionRepository) FindAll(ctx context.Context) ([]domain.Subscription, error) {
	ret := m.Called(ctx)
	if ret.Get(0) == nil {
		return nil, ret.Error(1)
	}
	return ret.Get(0).([]domain.Subscription), ret.Error(1)
}
func (m *MockSubscriptionRepository) UpdateMaxDevicesByUserID(ctx context.Context, userID string, maxDevices int) error {
	return m.Called(ctx, userID, maxDevices).Error(0)
}
func (m *MockSubscriptionRepository) UpdateAutoRenewByUserID(ctx context.Context, userID string, autoRenew bool) error {
	return m.Called(ctx, userID, autoRenew).Error(0)
}
func (m *MockSubscriptionRepository) AutoRenewSubscription(ctx context.Context, userID string, planID *int64, price int, newEndsAt *time.Time, maxDevices int) error {
	return m.Called(ctx, userID, planID, price, newEndsAt, maxDevices).Error(0)
}
func (m *MockSubscriptionRepository) UpdateFields(ctx context.Context, subID string, updates map[string]interface{}) error {
	return m.Called(ctx, subID, updates).Error(0)
}
func (m *MockSubscriptionRepository) GetMasterSnapshot(ctx context.Context) ([]domain.Subscription, error) {
	ret := m.Called(ctx)
	return ret.Get(0).([]domain.Subscription), ret.Error(1)
}

type MockUserRepository struct {
	mock.Mock
	domain.UserRepository
}

func (m *MockUserRepository) FindAll(ctx context.Context) ([]domain.User, error) {
	ret := m.Called(ctx)
	if ret.Get(0) == nil {
		return nil, ret.Error(1)
	}
	return ret.Get(0).([]domain.User), ret.Error(1)
}

type MockAntifraudBanRepository struct {
	mock.Mock
	domain.AntifraudBanRepository
}

func (m *MockAntifraudBanRepository) FindActive(ctx context.Context) ([]domain.AntifraudBan, error) {
	ret := m.Called(ctx)
	if ret.Get(0) == nil {
		return nil, ret.Error(1)
	}
	return ret.Get(0).([]domain.AntifraudBan), ret.Error(1)
}

func TestService_SelfHealMasterUUIDs(t *testing.T) {
	ctx := context.Background()

	t.Run("No subscriptions or users", func(t *testing.T) {
		regMock := mocks.NewRegistry(t)
		subRepoMock := new(MockSubscriptionRepository)
		userRepoMock := new(MockUserRepository)
		banRepoMock := new(MockAntifraudBanRepository)
		engineMock := mocks.NewEngine(t)

		regMock.On("Subscriptions").Return(subRepoMock)
		regMock.On("Users").Return(userRepoMock)
		regMock.On("AntifraudBans").Return(banRepoMock)

		subRepoMock.On("FindAll", ctx).Return([]domain.Subscription{}, nil)
		userRepoMock.On("FindAll", ctx).Return([]domain.User{}, nil)
		banRepoMock.On("FindActive", ctx).Return([]domain.AntifraudBan{}, nil)

		engineMock.On("SyncUsers", ctx, []domain.VPNUserConfig{}, true).Return(&domain.EngineSyncResult{Added: 0, Removed: 0}, nil)

		svc := statesync.NewService(regMock, engineMock, nil, nil)
		changed, err := svc.SelfHealMasterUUIDs(ctx)

		assert.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("No change needed", func(t *testing.T) {
		regMock := mocks.NewRegistry(t)
		subRepoMock := new(MockSubscriptionRepository)
		userRepoMock := new(MockUserRepository)
		banRepoMock := new(MockAntifraudBanRepository)
		engineMock := mocks.NewEngine(t)

		endsAt := time.Date(2026, 7, 7, 22, 10, 15, 0, time.UTC)
		subs := []domain.Subscription{
			{
				Email:      "user1@example.com",
				XrayUUID:   "uuid1",
				Status:     "active",
				MaxDevices: 3,
				EndsAt:     &endsAt,
				Metadata:   domain.Metadata{"subfile": "subfile1", "auth": "auth1"},
			},
		}

		expectedUser := vpn.SubscriptionToVPNUserConfig(subs[0])

		regMock.On("Subscriptions").Return(subRepoMock)
		regMock.On("Users").Return(userRepoMock)
		regMock.On("AntifraudBans").Return(banRepoMock)

		subRepoMock.On("FindAll", ctx).Return(subs, nil)
		userRepoMock.On("FindAll", ctx).Return([]domain.User{}, nil)
		banRepoMock.On("FindActive", ctx).Return([]domain.AntifraudBan{}, nil)

		engineMock.On("SyncUsers", ctx, []domain.VPNUserConfig{expectedUser}, true).Return(&domain.EngineSyncResult{Added: 0, Removed: 0}, nil)

		svc := statesync.NewService(regMock, engineMock, nil, nil)
		changed, err := svc.SelfHealMasterUUIDs(ctx)

		assert.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("Heal mismatches", func(t *testing.T) {
		regMock := mocks.NewRegistry(t)
		subRepoMock := new(MockSubscriptionRepository)
		userRepoMock := new(MockUserRepository)
		banRepoMock := new(MockAntifraudBanRepository)
		engineMock := mocks.NewEngine(t)

		endsAt := time.Date(2026, 7, 7, 22, 10, 15, 0, time.UTC)
		subs := []domain.Subscription{
			{
				Email:      "user1@example.com",
				XrayUUID:   "uuid-new",
				Status:     "active",
				MaxDevices: 5,
				EndsAt:     &endsAt,
				Metadata:   domain.Metadata{"subfile": "subfile-new", "auth": "auth-new"},
			},
		}

		expectedUser := vpn.SubscriptionToVPNUserConfig(subs[0])

		regMock.On("Subscriptions").Return(subRepoMock)
		regMock.On("Users").Return(userRepoMock)
		regMock.On("AntifraudBans").Return(banRepoMock)

		subRepoMock.On("FindAll", ctx).Return(subs, nil)
		userRepoMock.On("FindAll", ctx).Return([]domain.User{}, nil)
		banRepoMock.On("FindActive", ctx).Return([]domain.AntifraudBan{}, nil)

		engineMock.On("SyncUsers", ctx, []domain.VPNUserConfig{expectedUser}, true).Return(&domain.EngineSyncResult{Added: 1, Removed: 1}, nil)

		svc := statesync.NewService(regMock, engineMock, nil, nil)
		changed, err := svc.SelfHealMasterUUIDs(ctx)

		assert.NoError(t, err)
		assert.True(t, changed)
	})

	t.Run("Add missing active user", func(t *testing.T) {
		regMock := mocks.NewRegistry(t)
		subRepoMock := new(MockSubscriptionRepository)
		userRepoMock := new(MockUserRepository)
		banRepoMock := new(MockAntifraudBanRepository)
		engineMock := mocks.NewEngine(t)

		endsAt := time.Date(2026, 7, 7, 22, 10, 15, 0, time.UTC)
		subs := []domain.Subscription{
			{
				Email:      "user1@example.com",
				XrayUUID:   "uuid-new",
				Status:     "active",
				MaxDevices: 5,
				EndsAt:     &endsAt,
				Metadata:   domain.Metadata{"subfile": "subfile-new", "auth": "auth-new"},
			},
		}

		expectedUser := vpn.SubscriptionToVPNUserConfig(subs[0])

		regMock.On("Subscriptions").Return(subRepoMock)
		regMock.On("Users").Return(userRepoMock)
		regMock.On("AntifraudBans").Return(banRepoMock)

		subRepoMock.On("FindAll", ctx).Return(subs, nil)
		userRepoMock.On("FindAll", ctx).Return([]domain.User{}, nil)
		banRepoMock.On("FindActive", ctx).Return([]domain.AntifraudBan{}, nil)

		engineMock.On("SyncUsers", ctx, []domain.VPNUserConfig{expectedUser}, true).Return(&domain.EngineSyncResult{Added: 1, Removed: 0}, nil)

		svc := statesync.NewService(regMock, engineMock, nil, nil)
		changed, err := svc.SelfHealMasterUUIDs(ctx)

		assert.NoError(t, err)
		assert.True(t, changed)
	})

	t.Run("Remove inactive subscription", func(t *testing.T) {
		regMock := mocks.NewRegistry(t)
		subRepoMock := new(MockSubscriptionRepository)
		userRepoMock := new(MockUserRepository)
		banRepoMock := new(MockAntifraudBanRepository)
		engineMock := mocks.NewEngine(t)

		subs := []domain.Subscription{
			{
				Email:    "user1@example.com",
				XrayUUID: "uuid1",
				Status:   "expired",
			},
		}

		regMock.On("Subscriptions").Return(subRepoMock)
		regMock.On("Users").Return(userRepoMock)
		regMock.On("AntifraudBans").Return(banRepoMock)

		subRepoMock.On("FindAll", ctx).Return(subs, nil)
		userRepoMock.On("FindAll", ctx).Return([]domain.User{}, nil)
		banRepoMock.On("FindActive", ctx).Return([]domain.AntifraudBan{}, nil)

		engineMock.On("SyncUsers", ctx, []domain.VPNUserConfig{}, true).Return(&domain.EngineSyncResult{Added: 0, Removed: 1}, nil)

		svc := statesync.NewService(regMock, engineMock, nil, nil)
		changed, err := svc.SelfHealMasterUUIDs(ctx)

		assert.NoError(t, err)
		assert.True(t, changed)
	})

	t.Run("Remove admin-blocked user", func(t *testing.T) {
		regMock := mocks.NewRegistry(t)
		subRepoMock := new(MockSubscriptionRepository)
		userRepoMock := new(MockUserRepository)
		banRepoMock := new(MockAntifraudBanRepository)
		engineMock := mocks.NewEngine(t)

		subs := []domain.Subscription{
			{
				Email:    "user1@example.com",
				UserID:   "user-id-1",
				XrayUUID: "uuid1",
				Status:   "active",
			},
		}

		dbUsers := []domain.User{
			{
				ID:        "user-id-1",
				IsBlocked: true,
			},
		}

		regMock.On("Subscriptions").Return(subRepoMock)
		regMock.On("Users").Return(userRepoMock)
		regMock.On("AntifraudBans").Return(banRepoMock)

		subRepoMock.On("FindAll", ctx).Return(subs, nil)
		userRepoMock.On("FindAll", ctx).Return(dbUsers, nil)
		banRepoMock.On("FindActive", ctx).Return([]domain.AntifraudBan{}, nil)

		engineMock.On("SyncUsers", ctx, []domain.VPNUserConfig{}, true).Return(&domain.EngineSyncResult{Added: 0, Removed: 1}, nil)

		svc := statesync.NewService(regMock, engineMock, nil, nil)
		changed, err := svc.SelfHealMasterUUIDs(ctx)

		assert.NoError(t, err)
		assert.True(t, changed)
	})
}
