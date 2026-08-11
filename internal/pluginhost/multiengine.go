// Package pluginhost contains the host-side adapters between the kernel domain
// contracts and plugin extension points.
package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

// ErrNoEngineProviders means a MultiEngine was constructed without a usable
// engine plugin. It is intentionally returned instead of silently treating a
// mutating operation as successful.
var ErrNoEngineProviders = errors.New("pluginhost: no engine providers configured")

// EngineRouter decides which engines a VPN user should be registered on.
// The default implementation (broadcastRouter) sends every user to all engines.
//
// EngineRouter works with pluginapi types because it routes EngineProvider
// plugins. MultiEngine converts the domain user before calling it.
type EngineRouter interface {
	// EnginesFor returns the engines that should handle this user.
	EnginesFor(u pluginapi.VPNUserConfig) []pluginapi.EngineProvider
}

// broadcastRouter sends every user to all engines (plan §2.6.3, mode 1 — default).
type broadcastRouter struct {
	engines []pluginapi.EngineProvider
}

func (r *broadcastRouter) EnginesFor(_ pluginapi.VPNUserConfig) []pluginapi.EngineProvider {
	return append([]pluginapi.EngineProvider(nil), r.engines...)
}

// engineRef preserves the configured engine order and records the stable ID
// once. The ID is the router identity and makes aggregation deterministic even
// though individual engine calls are made concurrently.
type engineRef struct {
	provider pluginapi.EngineProvider
	id       string
}

// MultiEngine aggregates N EngineProvider plugins into one domain.Engine.
// Construct it through Host.MultiEngine once all engine plugins are loaded.
//
// The facade deliberately has domain types at its public mutation/read boundary
// and pluginapi types only at the plugin boundary. This keeps existing kernel
// consumers independent of the plugin transport contract.
type MultiEngine struct {
	engines []engineRef
	router  EngineRouter
	log     *slog.Logger
	initErr error
}

var _ domain.Engine = (*MultiEngine)(nil)
var _ pluginapi.ClientConfigContributor = (*MultiEngine)(nil)
var _ domain.StaticClientSynchronizer = (*MultiEngine)(nil)

// NewMultiEngine creates a MultiEngine with the broadcast router.
//
// It is safe to pass a nil logger: slog.Default is used. A configuration with
// no usable engines is represented by a valid object whose operations return
// ErrNoEngineProviders, which is safer than a silent no-op or a nil panic.
func NewMultiEngine(engines []pluginapi.EngineProvider, log *slog.Logger) *MultiEngine {
	if log == nil {
		log = slog.Default()
	}

	m := &MultiEngine{log: log}
	refs := make([]engineRef, 0, len(engines))
	seenIDs := make(map[string]struct{}, len(engines))
	var errs []error

	for i, engine := range engines {
		if isNilService(engine) {
			errs = append(errs, fmt.Errorf("pluginhost: engine provider at index %d is nil", i))
			continue
		}

		id := strings.TrimSpace(engine.ID())
		if id == "" {
			errs = append(errs, fmt.Errorf("pluginhost: engine provider at index %d has an empty ID", i))
			continue
		}
		if _, exists := seenIDs[id]; exists {
			errs = append(errs, fmt.Errorf("pluginhost: duplicate engine provider ID %q", id))
			continue
		}

		seenIDs[id] = struct{}{}
		refs = append(refs, engineRef{provider: engine, id: id})
	}

	m.engines = refs
	if len(refs) == 0 {
		errs = append(errs, ErrNoEngineProviders)
	}
	m.initErr = errors.Join(errs...)
	m.router = &broadcastRouter{engines: refsToProviders(refs)}
	return m
}

// WithRouter replaces the default broadcast router with a custom one. Passing
// nil restores the default, so callers cannot accidentally create a nil-router
// panic after construction.
func (m *MultiEngine) WithRouter(r EngineRouter) *MultiEngine {
	if m == nil {
		return nil
	}
	if r == nil {
		m.router = &broadcastRouter{engines: refsToProviders(m.engines)}
	} else {
		m.router = r
	}
	return m
}

// SupportsStaticClientSync reports whether exactly one configured engine owns
// hardcoded template clients. Static client artifacts have no engine ID in
// their wire format, so delegating to more than one engine would be ambiguous
// and unsafe. A single Xray engine remains supported alongside ordinary ones.
func (m *MultiEngine) SupportsStaticClientSync() bool {
	_, ok := m.staticClientSynchronizer()
	return ok
}

// StaticClientSnapshot delegates the optional static/template-client
// capability to its single supporting engine. This keeps cluster replication
// working when the kernel passes the MultiEngine facade instead of Xray.
func (m *MultiEngine) StaticClientSnapshot(ctx context.Context, users []domain.VPNUserConfig) ([]domain.StaticInboundClients, error) {
	synchronizer, ok := m.staticClientSynchronizer()
	if !ok {
		return nil, fmt.Errorf("pluginhost: static client synchronization requires exactly one capable engine")
	}
	return synchronizer.StaticClientSnapshot(ctx, users)
}

// ApplyStaticClientSnapshot delegates replicated hardcoded clients to the
// same engine. Dynamic database users remain routed by MultiEngine normally.
func (m *MultiEngine) ApplyStaticClientSnapshot(ctx context.Context, clients []domain.StaticInboundClients) error {
	synchronizer, ok := m.staticClientSynchronizer()
	if !ok {
		return fmt.Errorf("pluginhost: static client synchronization requires exactly one capable engine")
	}
	return synchronizer.ApplyStaticClientSnapshot(ctx, clients)
}

func (m *MultiEngine) staticClientSynchronizer() (domain.StaticClientSynchronizer, bool) {
	if m == nil {
		return nil, false
	}
	var selected domain.StaticClientSynchronizer
	for _, ref := range m.engines {
		if probe, ok := ref.provider.(interface{ SupportsStaticClientSync() bool }); ok && !probe.SupportsStaticClientSync() {
			continue
		}
		synchronizer, ok := ref.provider.(domain.StaticClientSynchronizer)
		if !ok {
			continue
		}
		if selected != nil {
			return nil, false
		}
		selected = synchronizer
	}
	return selected, selected != nil
}

func refsToProviders(refs []engineRef) []pluginapi.EngineProvider {
	providers := make([]pluginapi.EngineProvider, len(refs))
	for i, ref := range refs {
		providers[i] = ref.provider
	}
	return providers
}

func (m *MultiEngine) ready() error {
	if m == nil {
		return ErrNoEngineProviders
	}
	if m.initErr != nil {
		return m.initErr
	}
	if len(m.engines) == 0 {
		return ErrNoEngineProviders
	}
	return nil
}

// Validate reports whether the configured engine provider set is usable. It is
// primarily for composition roots that assemble a MultiEngine before Host.Load;
// normal domain operations perform the same check automatically.
func (m *MultiEngine) Validate() error {
	return m.ready()
}

func (m *MultiEngine) logger() *slog.Logger {
	if m != nil && m.log != nil {
		return m.log
	}
	return slog.Default()
}

// routedEngines obtains the router selection and canonicalises it to the
// configured engine order. A router is allowed to return an empty selection
// (for example, a plan that intentionally has no engine), but it may not select
// a provider that is not loaded in this MultiEngine.
func (m *MultiEngine) routedEngines(user pluginapi.VPNUserConfig) ([]engineRef, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}

	router := m.router
	if router == nil {
		return append([]engineRef(nil), m.engines...), nil
	}

	var selected []pluginapi.EngineProvider
	if checked, ok := router.(CheckedEngineRouter); ok {
		var err error
		selected, err = checked.EnginesForChecked(user)
		if err != nil {
			return nil, err
		}
	} else {
		selected = router.EnginesFor(user)
	}
	if len(selected) == 0 {
		return nil, nil
	}

	selectedIDs := make(map[string]struct{}, len(selected))
	knownIDs := make(map[string]struct{}, len(m.engines))
	for _, ref := range m.engines {
		knownIDs[ref.id] = struct{}{}
	}

	for i, engine := range selected {
		if isNilService(engine) {
			return nil, fmt.Errorf("pluginhost: engine router returned nil engine at index %d", i)
		}
		id := strings.TrimSpace(engine.ID())
		if id == "" {
			return nil, fmt.Errorf("pluginhost: engine router returned an engine with an empty ID at index %d", i)
		}
		if _, known := knownIDs[id]; !known {
			return nil, fmt.Errorf("pluginhost: engine router selected unloaded engine %q", id)
		}
		selectedIDs[id] = struct{}{}
	}

	targets := make([]engineRef, 0, len(selectedIDs))
	for _, ref := range m.engines {
		if _, selected := selectedIDs[ref.id]; selected {
			targets = append(targets, ref)
		}
	}
	return targets, nil
}

// fanOut calls fn on each target engine concurrently and collects errors in
// configured target order. Partial success is allowed: successful work is never
// rolled back because the individual engines are independent systems.
func (m *MultiEngine) fanOut(targets []engineRef, fn func(pluginapi.EngineProvider) error) error {
	if err := m.ready(); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("pluginhost: MultiEngine fan-out function must not be nil")
	}
	if len(targets) == 0 {
		return nil
	}
	if len(targets) == 1 {
		return fn(targets[0].provider)
	}

	type result struct {
		index int
		err   error
	}
	results := make([]error, len(targets))
	ch := make(chan result, len(targets))

	for i, target := range targets {
		go func(index int, provider pluginapi.EngineProvider) {
			ch <- result{index: index, err: fn(provider)}
		}(i, target.provider)
	}
	for range targets {
		result := <-ch
		results[result.index] = result.err
	}

	var errs []error
	for i, err := range results {
		if err == nil {
			continue
		}
		m.logger().Warn("[multiengine] engine operation failed (partial success)",
			"engine", targets[i].id, "error", err)
		errs = append(errs, fmt.Errorf("engine %q: %w", targets[i].id, err))
	}
	return errors.Join(errs...)
}

// fanOutAll is like fanOut but always targets all engines (used for removals,
// bans and other state that must not leave an orphan on an engine).
func (m *MultiEngine) fanOutAll(fn func(pluginapi.EngineProvider) error) error {
	if m == nil {
		return ErrNoEngineProviders
	}
	return m.fanOut(m.engines, fn)
}

// AddUser registers the user on all engines selected by the router.
func (m *MultiEngine) AddUser(ctx context.Context, user domain.VPNUserConfig) error {
	pluginUser := toPluginVPNUserConfig(user)
	targets, err := m.routedEngines(pluginUser)
	if err != nil {
		return err
	}
	return m.fanOut(targets, func(engine pluginapi.EngineProvider) error {
		return engine.AddUser(ctx, pluginUser)
	})
}

// AddUsersBulk registers multiple users in bulk on the appropriate engines.
// Users are grouped by router decision and each engine receives only its slice.
func (m *MultiEngine) AddUsersBulk(ctx context.Context, users []domain.VPNUserConfig) error {
	if err := m.ready(); err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
	}

	type engineBatch struct {
		ref   engineRef
		users []pluginapi.VPNUserConfig
	}
	batches := make([]engineBatch, len(m.engines))
	indexByID := make(map[string]int, len(m.engines))
	for i, ref := range m.engines {
		batches[i].ref = ref
		indexByID[ref.id] = i
	}

	for _, user := range users {
		pluginUser := toPluginVPNUserConfig(user)
		targets, err := m.routedEngines(pluginUser)
		if err != nil {
			return err
		}
		for _, target := range targets {
			batches[indexByID[target.id]].users = append(batches[indexByID[target.id]].users, pluginUser)
		}
	}

	type result struct {
		index int
		err   error
	}
	count := 0
	for _, batch := range batches {
		if len(batch.users) > 0 {
			count++
		}
	}
	if count == 0 {
		return nil
	}

	results := make([]error, len(batches))
	ch := make(chan result, count)
	for i, batch := range batches {
		if len(batch.users) == 0 {
			continue
		}
		go func(index int, entry engineBatch) {
			ch <- result{
				index: index,
				err:   entry.ref.provider.AddUsersBulk(ctx, entry.users),
			}
		}(i, batch)
	}
	for range count {
		result := <-ch
		results[result.index] = result.err
	}

	var errs []error
	for i, err := range results {
		if err == nil {
			continue
		}
		m.logger().Warn("[multiengine] AddUsersBulk partial failure",
			"engine", batches[i].ref.id, "error", err)
		errs = append(errs, fmt.Errorf("engine %q: %w", batches[i].ref.id, err))
	}
	return errors.Join(errs...)
}

// RemoveUser removes the user from all engines. We broadcast removals
// regardless of router mode to avoid orphaned users.
func (m *MultiEngine) RemoveUser(ctx context.Context, email string) error {
	return m.fanOutAll(func(engine pluginapi.EngineProvider) error {
		return engine.RemoveUser(ctx, email)
	})
}

// RemoveUsersBulk removes multiple users from all engines.
func (m *MultiEngine) RemoveUsersBulk(ctx context.Context, emails []string) error {
	return m.fanOutAll(func(engine pluginapi.EngineProvider) error {
		return engine.RemoveUsersBulk(ctx, emails)
	})
}

// SetExpire updates the expiry for a user on all engines.
func (m *MultiEngine) SetExpire(ctx context.Context, email string, expire string) error {
	return m.fanOutAll(func(engine pluginapi.EngineProvider) error {
		return engine.SetExpire(ctx, email, expire)
	})
}

// SetLimit updates the speed/traffic limit for a user on all engines.
func (m *MultiEngine) SetLimit(ctx context.Context, email string, limit float64) error {
	return m.fanOutAll(func(engine pluginapi.EngineProvider) error {
		return engine.SetLimit(ctx, email, limit)
	})
}

// RebuildInbound rebuilds a named inbound on all engines. An engine that does
// not know the tag should return nil: a protocol-specific inbound may not exist
// on every engine.
func (m *MultiEngine) RebuildInbound(ctx context.Context, tag string) error {
	return m.fanOutAll(func(engine pluginapi.EngineProvider) error {
		return engine.RebuildInbound(ctx, tag)
	})
}

// QueryStats collects traffic stats from all engines and merges them by email.
// Equal emails are summed across engines. Output is sorted by email so it does
// not vary with concurrent engine completion order.
func (m *MultiEngine) QueryStats(ctx context.Context) ([]domain.TrafficStat, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}

	type result struct {
		index int
		stats []pluginapi.TrafficStat
		err   error
	}
	ch := make(chan result, len(m.engines))
	for i, ref := range m.engines {
		go func(index int, provider pluginapi.EngineProvider) {
			stats, err := provider.QueryStats(ctx)
			ch <- result{index: index, stats: stats, err: err}
		}(i, ref.provider)
	}

	results := make([]result, len(m.engines))
	for range m.engines {
		result := <-ch
		results[result.index] = result
	}

	merged := make(map[string]domain.TrafficStat)
	var errs []error
	for i, result := range results {
		if result.err != nil {
			m.logger().Warn("[multiengine] QueryStats partial failure",
				"engine", m.engines[i].id, "error", result.err)
			errs = append(errs, fmt.Errorf("engine %q: %w", m.engines[i].id, result.err))
			continue
		}
		for _, stat := range result.stats {
			mergedStat := merged[stat.Email]
			mergedStat.Email = stat.Email
			mergedStat.Up += stat.Up
			mergedStat.Down += stat.Down
			merged[stat.Email] = mergedStat
		}
	}

	out := make([]domain.TrafficStat, 0, len(merged))
	for _, stat := range merged {
		out = append(out, stat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, errors.Join(errs...)
}

// BanUser bans the user on all engines, regardless of router mode.
func (m *MultiEngine) BanUser(ctx context.Context, email string) error {
	return m.fanOutAll(func(engine pluginapi.EngineProvider) error {
		return engine.BanUser(ctx, email)
	})
}

// UnbanUser unbans the user on all engines.
func (m *MultiEngine) UnbanUser(ctx context.Context, email string) error {
	return m.fanOutAll(func(engine pluginapi.EngineProvider) error {
		return engine.UnbanUser(ctx, email)
	})
}

// RestartLogger restarts the access log on all engines.
func (m *MultiEngine) RestartLogger(ctx context.Context) error {
	return m.fanOutAll(func(engine pluginapi.EngineProvider) error {
		return engine.RestartLogger(ctx)
	})
}

// ListUsers returns the union of users reported by all engines, deduplicated by
// email. If multiple engines report an email, the configured-first engine wins.
// The returned slice is sorted by email to keep state-sync behavior stable.
func (m *MultiEngine) ListUsers(ctx context.Context) ([]domain.VPNUserConfig, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}

	type result struct {
		index int
		users []pluginapi.VPNUserConfig
		err   error
	}
	ch := make(chan result, len(m.engines))
	for i, ref := range m.engines {
		go func(index int, provider pluginapi.EngineProvider) {
			users, err := provider.ListUsers(ctx)
			ch <- result{index: index, users: users, err: err}
		}(i, ref.provider)
	}

	results := make([]result, len(m.engines))
	for range m.engines {
		result := <-ch
		results[result.index] = result
	}

	seen := make(map[string]struct{})
	all := make([]domain.VPNUserConfig, 0)
	var errs []error
	for i, result := range results {
		if result.err != nil {
			m.logger().Warn("[multiengine] ListUsers partial failure",
				"engine", m.engines[i].id, "error", result.err)
			errs = append(errs, fmt.Errorf("engine %q: %w", m.engines[i].id, result.err))
			continue
		}
		for _, user := range result.users {
			if _, exists := seen[user.Email]; exists {
				continue
			}
			seen[user.Email] = struct{}{}
			all = append(all, toDomainVPNUserConfig(user))
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Email < all[j].Email })
	return all, errors.Join(errs...)
}

// SyncUsers regenerates configs and hot-adds/removes users on all engines.
// Its result is the sum of the per-engine changes.
func (m *MultiEngine) SyncUsers(
	ctx context.Context,
	dbUsers []domain.VPNUserConfig,
	removeOrphans bool,
) (*domain.EngineSyncResult, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}

	// Route the desired snapshot just like AddUsersBulk. Every provider is
	// still called (possibly with an empty slice) so removeOrphans=true also
	// cleans users that were moved away from that engine by a plan/override.
	batches := make([][]pluginapi.VPNUserConfig, len(m.engines))
	indexByID := make(map[string]int, len(m.engines))
	for index, ref := range m.engines {
		indexByID[ref.id] = index
	}
	for _, dbUser := range dbUsers {
		pluginUser := toPluginVPNUserConfig(dbUser)
		targets, err := m.routedEngines(pluginUser)
		if err != nil {
			return nil, err
		}
		for _, target := range targets {
			batches[indexByID[target.id]] = append(batches[indexByID[target.id]], pluginUser)
		}
	}

	type syncResult struct {
		index  int
		result *pluginapi.EngineSyncResult
		err    error
	}
	ch := make(chan syncResult, len(m.engines))
	for i, ref := range m.engines {
		users := batches[i]
		go func(index int, provider pluginapi.EngineProvider, users []pluginapi.VPNUserConfig) {
			// Engine providers are independent plugins. Give each one its own
			// slice so a provider that normalises its input cannot race with
			// another provider or mutate the next provider's view.
			usersForEngine := make([]pluginapi.VPNUserConfig, len(users))
			for j, u := range users {
				usersForEngine[j] = u
				if u.PlanEngineIDs != nil {
					usersForEngine[j].PlanEngineIDs = append([]string(nil), u.PlanEngineIDs...)
				}
				if u.SubscriptionEngineIDs != nil {
					usersForEngine[j].SubscriptionEngineIDs = append([]string(nil), u.SubscriptionEngineIDs...)
				}
			}
			engineResult, err := provider.SyncUsers(ctx, usersForEngine, removeOrphans)
			ch <- syncResult{index: index, result: engineResult, err: err}
		}(i, ref.provider, users)
	}

	results := make([]syncResult, len(m.engines))
	for range m.engines {
		result := <-ch
		results[result.index] = result
	}

	aggregate := &domain.EngineSyncResult{}
	var errs []error
	for i, result := range results {
		if result.err != nil {
			m.logger().Warn("[multiengine] SyncUsers partial failure",
				"engine", m.engines[i].id, "error", result.err)
			errs = append(errs, fmt.Errorf("engine %q: %w", m.engines[i].id, result.err))
			continue
		}
		if result.result != nil {
			aggregate.Added += result.result.Added
			aggregate.Removed += result.result.Removed
		}
	}
	return aggregate, errors.Join(errs...)
}

// BuildClientLinks collects share links from the engines selected by the
// router. Links are concatenated in configured engine order, independent of
// concurrent completion order.
func (m *MultiEngine) BuildClientLinks(
	ctx context.Context,
	user pluginapi.VPNUserConfig,
) ([]pluginapi.ClientLink, error) {
	targets, err := m.routedEngines(user)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}

	type result struct {
		index int
		links []pluginapi.ClientLink
		err   error
	}
	ch := make(chan result, len(targets))
	for i, target := range targets {
		go func(index int, provider pluginapi.EngineProvider) {
			links, err := provider.BuildClientLinks(ctx, user)
			ch <- result{index: index, links: links, err: err}
		}(i, target.provider)
	}

	results := make([]result, len(targets))
	for range targets {
		result := <-ch
		results[result.index] = result
	}

	var all []pluginapi.ClientLink
	var errs []error
	for i, result := range results {
		if result.err != nil {
			m.logger().Warn("[multiengine] BuildClientLinks partial failure",
				"engine", targets[i].id, "error", result.err)
			errs = append(errs, fmt.Errorf("engine %q: %w", targets[i].id, result.err))
			continue
		}
		all = append(all, result.links...)
	}
	return all, errors.Join(errs...)
}

func toPluginVPNUserConfig(user domain.VPNUserConfig) pluginapi.VPNUserConfig {
	return pluginapi.VPNUserConfig{
		Email:                 user.Email,
		UUID:                  user.UUID,
		Auth:                  user.Auth,
		Subfile:               user.Subfile,
		Expire:                user.Expire,
		MaxDevices:            user.MaxDevices,
		Flow:                  user.Flow,
		Cipher:                user.Cipher,
		PlanEngineIDs:         append([]string(nil), user.PlanEngineIDs...),
		SubscriptionEngineIDs: append([]string(nil), user.SubscriptionEngineIDs...),
	}
}

func toPluginVPNUserConfigs(users []domain.VPNUserConfig) []pluginapi.VPNUserConfig {
	out := make([]pluginapi.VPNUserConfig, len(users))
	for i, user := range users {
		out[i] = toPluginVPNUserConfig(user)
	}
	return out
}

func toDomainVPNUserConfig(user pluginapi.VPNUserConfig) domain.VPNUserConfig {
	return domain.VPNUserConfig{
		Email:                 user.Email,
		UUID:                  user.UUID,
		Auth:                  user.Auth,
		Subfile:               user.Subfile,
		Expire:                user.Expire,
		MaxDevices:            user.MaxDevices,
		Flow:                  user.Flow,
		Cipher:                user.Cipher,
		PlanEngineIDs:         append([]string(nil), user.PlanEngineIDs...),
		SubscriptionEngineIDs: append([]string(nil), user.SubscriptionEngineIDs...),
	}
}

// MultiEngine returns the aggregate facade for the loaded EngineProvider
// plugins. It returns nil when no provider is loaded; callers that construct a
// MultiEngine directly receive ErrNoEngineProviders from its operations instead.
func (h *Host) MultiEngine() *MultiEngine {
	engines := h.EngineProviders()
	if len(engines) == 0 {
		return nil
	}
	return NewMultiEngine(engines, h.log)
}
