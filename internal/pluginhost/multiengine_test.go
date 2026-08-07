package pluginhost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

// fakeEngine embeds the complete plugin contract and overrides only the engine
// methods exercised by these tests. The embedded interface is never invoked.
type fakeEngine struct {
	pluginapi.EngineProvider

	id    string
	delay time.Duration

	mu sync.Mutex

	addUserCalls  []pluginapi.VPNUserConfig
	addUserErr    error
	bulkUserCalls [][]pluginapi.VPNUserConfig
	bulkUserErr   error

	stats    []pluginapi.TrafficStat
	statsErr error

	users    []pluginapi.VPNUserConfig
	usersErr error

	syncCalls  [][]pluginapi.VPNUserConfig
	syncResult *pluginapi.EngineSyncResult
	syncErr    error

	links    []pluginapi.ClientLink
	linksErr error
}

var _ pluginapi.EngineProvider = (*fakeEngine)(nil)

func (e *fakeEngine) ID() string { return e.id }

func (e *fakeEngine) AddUser(_ context.Context, user pluginapi.VPNUserConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.addUserCalls = append(e.addUserCalls, user)
	return e.addUserErr
}

func (e *fakeEngine) AddUsersBulk(_ context.Context, users []pluginapi.VPNUserConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bulkUserCalls = append(e.bulkUserCalls, append([]pluginapi.VPNUserConfig(nil), users...))
	return e.bulkUserErr
}

func (e *fakeEngine) QueryStats(ctx context.Context) ([]pluginapi.TrafficStat, error) {
	if err := e.wait(ctx); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]pluginapi.TrafficStat(nil), e.stats...), e.statsErr
}

func (e *fakeEngine) ListUsers(ctx context.Context) ([]pluginapi.VPNUserConfig, error) {
	if err := e.wait(ctx); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]pluginapi.VPNUserConfig(nil), e.users...), e.usersErr
}

func (e *fakeEngine) SyncUsers(
	_ context.Context,
	users []pluginapi.VPNUserConfig,
	_ bool,
) (*pluginapi.EngineSyncResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.syncCalls = append(e.syncCalls, append([]pluginapi.VPNUserConfig(nil), users...))
	if e.syncResult == nil {
		return nil, e.syncErr
	}
	result := *e.syncResult
	return &result, e.syncErr
}

func (e *fakeEngine) BuildClientLinks(
	ctx context.Context,
	_ pluginapi.VPNUserConfig,
) ([]pluginapi.ClientLink, error) {
	if err := e.wait(ctx); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]pluginapi.ClientLink(nil), e.links...), e.linksErr
}

func (e *fakeEngine) wait(ctx context.Context) error {
	if e.delay == 0 {
		return nil
	}
	timer := time.NewTimer(e.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *fakeEngine) addCalls() []pluginapi.VPNUserConfig {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]pluginapi.VPNUserConfig(nil), e.addUserCalls...)
}

func (e *fakeEngine) bulkCalls() [][]pluginapi.VPNUserConfig {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]pluginapi.VPNUserConfig, len(e.bulkUserCalls))
	for i, call := range e.bulkUserCalls {
		out[i] = append([]pluginapi.VPNUserConfig(nil), call...)
	}
	return out
}

func (e *fakeEngine) syncUserCalls() [][]pluginapi.VPNUserConfig {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]pluginapi.VPNUserConfig, len(e.syncCalls))
	for i, call := range e.syncCalls {
		out[i] = append([]pluginapi.VPNUserConfig(nil), call...)
	}
	return out
}

type routerFunc func(pluginapi.VPNUserConfig) []pluginapi.EngineProvider

func (f routerFunc) EnginesFor(user pluginapi.VPNUserConfig) []pluginapi.EngineProvider {
	return f(user)
}

func TestMultiEngineImplementsDomainEngineAndConvertsValues(t *testing.T) {
	t.Parallel()

	first := &fakeEngine{
		id:    "xray",
		delay: 30 * time.Millisecond,
		stats: []pluginapi.TrafficStat{
			{Email: "bravo@example.com", Up: 2, Down: 5},
			{Email: "alpha@example.com", Up: 1, Down: 2},
		},
		users: []pluginapi.VPNUserConfig{
			{Email: "duplicate@example.com", UUID: "first"},
			{Email: "zulu@example.com", UUID: "zulu"},
		},
		syncResult: &pluginapi.EngineSyncResult{Added: 1, Removed: 2},
	}
	second := &fakeEngine{
		id: "singbox",
		stats: []pluginapi.TrafficStat{
			{Email: "alpha@example.com", Up: 3, Down: 4},
		},
		users: []pluginapi.VPNUserConfig{
			{Email: "alpha@example.com", UUID: "alpha"},
			{Email: "duplicate@example.com", UUID: "second"},
		},
		syncResult: &pluginapi.EngineSyncResult{Added: 3, Removed: 4},
	}

	multi := NewMultiEngine([]pluginapi.EngineProvider{first, second}, nil)
	var engine domain.Engine = multi

	user := domain.VPNUserConfig{
		Email:      "user@example.com",
		UUID:       "uuid",
		Auth:       "auth",
		Subfile:    "subfile",
		Expire:     "2027-01-01",
		MaxDevices: 3,
		Flow:       "xtls-rprx-vision",
		Cipher:     "aes-128-gcm",
	}
	wantPluginUser := pluginapi.VPNUserConfig{
		Email:      user.Email,
		UUID:       user.UUID,
		Auth:       user.Auth,
		Subfile:    user.Subfile,
		Expire:     user.Expire,
		MaxDevices: user.MaxDevices,
		Flow:       user.Flow,
		Cipher:     user.Cipher,
	}

	require.NoError(t, engine.AddUser(context.Background(), user))
	require.Equal(t, []pluginapi.VPNUserConfig{wantPluginUser}, first.addCalls())
	require.Equal(t, []pluginapi.VPNUserConfig{wantPluginUser}, second.addCalls())

	stats, err := engine.QueryStats(context.Background())
	require.NoError(t, err)
	require.Equal(t, []domain.TrafficStat{
		{Email: "alpha@example.com", Up: 4, Down: 6},
		{Email: "bravo@example.com", Up: 2, Down: 5},
	}, stats)

	users, err := engine.ListUsers(context.Background())
	require.NoError(t, err)
	require.Equal(t, []domain.VPNUserConfig{
		{Email: "alpha@example.com", UUID: "alpha"},
		{Email: "duplicate@example.com", UUID: "first"},
		{Email: "zulu@example.com", UUID: "zulu"},
	}, users)

	syncResult, err := engine.SyncUsers(context.Background(), []domain.VPNUserConfig{user}, true)
	require.NoError(t, err)
	require.Equal(t, &domain.EngineSyncResult{Added: 4, Removed: 6}, syncResult)
	require.Equal(t, [][]pluginapi.VPNUserConfig{{wantPluginUser}}, first.syncUserCalls())
	require.Equal(t, [][]pluginapi.VPNUserConfig{{wantPluginUser}}, second.syncUserCalls())
}

func TestMultiEngineRoutesBulkUsersAndKeepsLinkOrderDeterministic(t *testing.T) {
	t.Parallel()

	xray := &fakeEngine{
		id:    "xray",
		delay: 30 * time.Millisecond,
		links: []pluginapi.ClientLink{{Protocol: "vless", URI: "vless://xray"}},
	}
	singbox := &fakeEngine{
		id:    "singbox",
		links: []pluginapi.ClientLink{{Protocol: "hysteria2", URI: "hy2://singbox"}},
	}

	multi := NewMultiEngine([]pluginapi.EngineProvider{xray, singbox}, nil).WithRouter(
		routerFunc(func(user pluginapi.VPNUserConfig) []pluginapi.EngineProvider {
			switch user.Email {
			case "singbox@example.com":
				return []pluginapi.EngineProvider{singbox}
			case "links@example.com":
				// Deliberately return the reverse order. MultiEngine must
				// canonicalise it to configured order.
				return []pluginapi.EngineProvider{singbox, xray}
			default:
				return []pluginapi.EngineProvider{xray}
			}
		}),
	)

	require.NoError(t, multi.AddUsersBulk(context.Background(), []domain.VPNUserConfig{
		{Email: "xray@example.com", UUID: "x"},
		{Email: "singbox@example.com", UUID: "s"},
	}))
	require.Equal(t, [][]pluginapi.VPNUserConfig{{
		{Email: "xray@example.com", UUID: "x"},
	}}, xray.bulkCalls())
	require.Equal(t, [][]pluginapi.VPNUserConfig{{
		{Email: "singbox@example.com", UUID: "s"},
	}}, singbox.bulkCalls())

	links, err := multi.BuildClientLinks(context.Background(), pluginapi.VPNUserConfig{
		Email: "links@example.com",
	})
	require.NoError(t, err)
	require.Equal(t, []pluginapi.ClientLink{
		{Protocol: "vless", URI: "vless://xray"},
		{Protocol: "hysteria2", URI: "hy2://singbox"},
	}, links)
}

func TestMultiEngineEmptyAndNilLoggerAreSafe(t *testing.T) {
	t.Parallel()

	empty := NewMultiEngine(nil, nil)
	err := empty.AddUser(context.Background(), domain.VPNUserConfig{Email: "user@example.com"})
	require.ErrorIs(t, err, ErrNoEngineProviders)

	_, err = empty.QueryStats(context.Background())
	require.ErrorIs(t, err, ErrNoEngineProviders)

	only := &fakeEngine{id: "xray"}
	multi := NewMultiEngine([]pluginapi.EngineProvider{only}, nil)
	require.NotPanics(t, func() {
		multi.WithRouter(nil)
		require.NoError(t, multi.AddUser(context.Background(), domain.VPNUserConfig{
			Email: "user@example.com",
		}))
	})
	require.Len(t, only.addCalls(), 1)
}

func TestMultiEngineReportsPartialFailuresAfterOtherEnginesRun(t *testing.T) {
	t.Parallel()

	failure := errors.New("xray is unavailable")
	failing := &fakeEngine{id: "xray", addUserErr: failure}
	succeeding := &fakeEngine{id: "singbox"}
	multi := NewMultiEngine([]pluginapi.EngineProvider{failing, succeeding}, nil)

	err := multi.AddUser(context.Background(), domain.VPNUserConfig{Email: "user@example.com"})
	require.ErrorIs(t, err, failure)
	require.Len(t, failing.addCalls(), 1)
	require.Len(t, succeeding.addCalls(), 1)
}

func TestConfiguredEngineRouterModesAndOverridePrecedence(t *testing.T) {
	t.Parallel()

	xray := &fakeEngine{id: "xray"}
	singbox := &fakeEngine{id: "singbox"}
	providers := []pluginapi.EngineProvider{xray, singbox}

	tests := []struct {
		name string
		mode string
		user pluginapi.VPNUserConfig
		want []string
	}{
		{
			name: "broadcast selects all",
			mode: string(RoutingModeBroadcast),
			want: []string{"xray", "singbox"},
		},
		{
			name: "by plan selects plan engines",
			mode: string(RoutingModeByPlan),
			user: pluginapi.VPNUserConfig{PlanEngineIDs: []string{"singbox"}},
			want: []string{"singbox"},
		},
		{
			name: "by plan empty keeps compatibility broadcast",
			mode: string(RoutingModeByPlan),
			want: []string{"xray", "singbox"},
		},
		{
			name: "override beats broadcast and plan",
			mode: string(RoutingModeBroadcast),
			user: pluginapi.VPNUserConfig{
				PlanEngineIDs:         []string{"xray", "singbox"},
				SubscriptionEngineIDs: []string{"singbox"},
			},
			want: []string{"singbox"},
		},
		{
			name: "override mode falls back to plan",
			mode: string(RoutingModeBySubscriptionOverride),
			user: pluginapi.VPNUserConfig{PlanEngineIDs: []string{"xray"}},
			want: []string{"xray"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			router, err := NewConfiguredEngineRouter(tt.mode, providers)
			require.NoError(t, err)

			selected, err := router.EnginesForChecked(tt.user)
			require.NoError(t, err)
			got := make([]string, len(selected))
			for i, engine := range selected {
				got[i] = engine.ID()
			}
			require.Equal(t, tt.want, got)
		})
	}
}

func TestConfiguredEngineRouterRejectsUnknownSubscriptionEngine(t *testing.T) {
	t.Parallel()

	router, err := NewConfiguredEngineRouter("by-subscription-override", []pluginapi.EngineProvider{
		&fakeEngine{id: "xray"},
	})
	require.NoError(t, err)

	_, err = router.EnginesForChecked(pluginapi.VPNUserConfig{
		Email:                 "person@example.test",
		SubscriptionEngineIDs: []string{"not-loaded"},
	})
	require.ErrorContains(t, err, "not-loaded")
}

func TestMultiEngineSyncUsersHonoursConfiguredRouting(t *testing.T) {
	t.Parallel()

	xray := &fakeEngine{id: "xray", syncResult: &pluginapi.EngineSyncResult{Added: 1}}
	singbox := &fakeEngine{id: "singbox", syncResult: &pluginapi.EngineSyncResult{Added: 1}}
	router, err := NewConfiguredEngineRouter(string(RoutingModeByPlan), []pluginapi.EngineProvider{xray, singbox})
	require.NoError(t, err)

	multi := NewMultiEngine([]pluginapi.EngineProvider{xray, singbox}, nil).WithRouter(router)
	result, err := multi.SyncUsers(context.Background(), []domain.VPNUserConfig{
		{Email: "xray@example.test", PlanEngineIDs: []string{"xray"}},
		{Email: "singbox@example.test", PlanEngineIDs: []string{"singbox"}},
	}, true)
	require.NoError(t, err)
	require.Equal(t, &domain.EngineSyncResult{Added: 2}, result)
	require.Equal(t, [][]pluginapi.VPNUserConfig{{
		{Email: "xray@example.test", PlanEngineIDs: []string{"xray"}},
	}}, xray.syncUserCalls())
	require.Equal(t, [][]pluginapi.VPNUserConfig{{
		{Email: "singbox@example.test", PlanEngineIDs: []string{"singbox"}},
	}}, singbox.syncUserCalls())
}
