// Package pluginhost — multiengine.go
//
// MultiEngine is a fan-out facade that aggregates N loaded EngineProvider plugins
// into a single domain.Engine-compatible interface. All existing consumers
// (user.Service, worker.ExpiryWorker, server.Router, antifraud.Module, statesync)
// receive a MultiEngine — they don't need to know whether one or several engines
// are active.
//
// Phase 0.5: the struct is defined and ready but it cannot be used yet because no
// real EngineProvider plugins exist. Phase 1.5 wires it into the Host.
//
// Fan-out semantics (plan §2.6.2):
//   - AddUser/RemoveUser: called on engines selected by the EngineRouter.
//   - QueryStats: results merged by email (traffic summed across engines).
//   - BanUser/UnbanUser: sent to all engines (broadcast, regardless of router mode).
//   - SyncUsers: called per-engine independently; results aggregated.
//   - Partial failure: logged and returned as a multi-error, but does NOT roll back
//     operations already completed on other engines (same approach as ExpiryWorker today).
package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"xraytool/internal/pluginapi"
)

// EngineRouter decides which engines a VPN user should be registered on.
// The default implementation (broadcastRouter) sends every user to all engines.
type EngineRouter interface {
	// EnginesFor returns the engines that should handle this user.
	EnginesFor(u pluginapi.VPNUserConfig) []pluginapi.EngineProvider
}

// broadcastRouter sends every user to all engines (plan §2.6.3, mode 1 — default).
type broadcastRouter struct {
	engines []pluginapi.EngineProvider
}

func (r *broadcastRouter) EnginesFor(_ pluginapi.VPNUserConfig) []pluginapi.EngineProvider {
	return r.engines
}

// MultiEngine aggregates N EngineProvider plugins into one facade.
// Construct via Host.MultiEngine() once all engine plugins are loaded.
type MultiEngine struct {
	engines []pluginapi.EngineProvider
	router  EngineRouter
	log     *slog.Logger
}

// NewMultiEngine creates a MultiEngine with the broadcast router.
// engines must not be empty — callers should verify that at least one engine
// plugin is loaded before calling this.
func NewMultiEngine(engines []pluginapi.EngineProvider, log *slog.Logger) *MultiEngine {
	return &MultiEngine{
		engines: engines,
		router:  &broadcastRouter{engines: engines},
		log:     log,
	}
}

// WithRouter replaces the default broadcast router with a custom one.
func (m *MultiEngine) WithRouter(r EngineRouter) *MultiEngine {
	m.router = r
	return m
}

// ── fan-out helpers ──────────────────────────────────────────────────────────

// fanOut calls fn on each target engine concurrently and collects errors.
// Partial success is allowed: if engine A succeeds and engine B fails, the
// successful operation on A is NOT rolled back. All errors are joined and returned.
func (m *MultiEngine) fanOut(targets []pluginapi.EngineProvider, fn func(pluginapi.EngineProvider) error) error {
	if len(targets) == 0 {
		return nil
	}
	if len(targets) == 1 {
		return fn(targets[0])
	}

	type result struct {
		engineID string
		err      error
	}
	ch := make(chan result, len(targets))

	for _, eng := range targets {
		go func(e pluginapi.EngineProvider) {
			ch <- result{engineID: e.ID(), err: fn(e)}
		}(eng)
	}

	var errs []error
	for range targets {
		r := <-ch
		if r.err != nil {
			m.log.Warn("[multiengine] engine operation failed (partial success)",
				"engine", r.engineID, "error", r.err)
			errs = append(errs, fmt.Errorf("engine %q: %w", r.engineID, r.err))
		}
	}
	return errors.Join(errs...)
}

// fanOutAll is like fanOut but always targets all engines (used for ban/unban).
func (m *MultiEngine) fanOutAll(fn func(pluginapi.EngineProvider) error) error {
	return m.fanOut(m.engines, fn)
}

// ── domain.Engine interface implementation ───────────────────────────────────

// AddUser registers the user on all engines selected by the router.
func (m *MultiEngine) AddUser(ctx context.Context, user pluginapi.VPNUserConfig) error {
	return m.fanOut(m.router.EnginesFor(user), func(e pluginapi.EngineProvider) error {
		return e.AddUser(ctx, user)
	})
}

// AddUsersBulk registers multiple users in bulk on appropriate engines.
// Users are grouped by router decision; each engine receives only its slice.
func (m *MultiEngine) AddUsersBulk(ctx context.Context, users []pluginapi.VPNUserConfig) error {
	// Build per-engine user lists according to the router.
	perEngine := make(map[string][]pluginapi.VPNUserConfig, len(m.engines))
	engineByID := make(map[string]pluginapi.EngineProvider, len(m.engines))
	for _, e := range m.engines {
		engineByID[e.ID()] = e
	}
	for _, u := range users {
		for _, e := range m.router.EnginesFor(u) {
			perEngine[e.ID()] = append(perEngine[e.ID()], u)
		}
	}

	var errs []error
	// Fan-out across engines concurrently.
	type result struct {
		id  string
		err error
	}
	ch := make(chan result, len(perEngine))
	for id, slice := range perEngine {
		go func(id string, slice []pluginapi.VPNUserConfig) {
			ch <- result{id: id, err: engineByID[id].AddUsersBulk(ctx, slice)}
		}(id, slice)
	}
	for range perEngine {
		r := <-ch
		if r.err != nil {
			m.log.Warn("[multiengine] AddUsersBulk partial failure", "engine", r.id, "error", r.err)
			errs = append(errs, fmt.Errorf("engine %q: %w", r.id, r.err))
		}
	}
	return errors.Join(errs...)
}

// RemoveUser removes the user from all engines.
// We broadcast removals to all engines regardless of router to avoid orphans.
func (m *MultiEngine) RemoveUser(ctx context.Context, email string) error {
	return m.fanOutAll(func(e pluginapi.EngineProvider) error {
		return e.RemoveUser(ctx, email)
	})
}

// RemoveUsersBulk removes multiple users from all engines.
func (m *MultiEngine) RemoveUsersBulk(ctx context.Context, emails []string) error {
	return m.fanOutAll(func(e pluginapi.EngineProvider) error {
		return e.RemoveUsersBulk(ctx, emails)
	})
}

// SetExpire updates the expiry for a user on all engines.
func (m *MultiEngine) SetExpire(ctx context.Context, email string, expire string) error {
	return m.fanOutAll(func(e pluginapi.EngineProvider) error {
		return e.SetExpire(ctx, email, expire)
	})
}

// SetLimit updates the speed/traffic limit for a user on all engines.
func (m *MultiEngine) SetLimit(ctx context.Context, email string, limit float64) error {
	return m.fanOutAll(func(e pluginapi.EngineProvider) error {
		return e.SetLimit(ctx, email, limit)
	})
}

// RebuildInbound rebuilds a named inbound on all engines. An engine that does
// not know the tag should return nil silently (it is not an error if a
// protocol-specific inbound doesn't exist on a different engine).
func (m *MultiEngine) RebuildInbound(ctx context.Context, tag string) error {
	return m.fanOutAll(func(e pluginapi.EngineProvider) error {
		return e.RebuildInbound(ctx, tag)
	})
}

// QueryStats collects traffic stats from all engines and merges them by email.
// If the same email appears in multiple engines (broadcast mode), upload and
// download are summed — this is the documented behaviour (plan §2.6.5).
func (m *MultiEngine) QueryStats(ctx context.Context) ([]pluginapi.TrafficStat, error) {
	type result struct {
		stats []pluginapi.TrafficStat
		err   error
	}
	ch := make(chan result, len(m.engines))
	for _, e := range m.engines {
		go func(e pluginapi.EngineProvider) {
			stats, err := e.QueryStats(ctx)
			ch <- result{stats, err}
		}(e)
	}

	merged := make(map[string]*pluginapi.TrafficStat)
	var errs []error
	for range m.engines {
		r := <-ch
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		for _, s := range r.stats {
			if existing, ok := merged[s.Email]; ok {
				existing.Up += s.Up
				existing.Down += s.Down
			} else {
				cp := s
				merged[s.Email] = &cp
			}
		}
	}

	out := make([]pluginapi.TrafficStat, 0, len(merged))
	for _, s := range merged {
		out = append(out, *s)
	}
	return out, errors.Join(errs...)
}

// BanUser bans the user on ALL engines (broadcast, regardless of router).
func (m *MultiEngine) BanUser(ctx context.Context, email string) error {
	return m.fanOutAll(func(e pluginapi.EngineProvider) error {
		return e.BanUser(ctx, email)
	})
}

// UnbanUser unbans the user on ALL engines.
func (m *MultiEngine) UnbanUser(ctx context.Context, email string) error {
	return m.fanOutAll(func(e pluginapi.EngineProvider) error {
		return e.UnbanUser(ctx, email)
	})
}

// RestartLogger restarts the access log on all engines.
func (m *MultiEngine) RestartLogger(ctx context.Context) error {
	return m.fanOutAll(func(e pluginapi.EngineProvider) error {
		return e.RestartLogger(ctx)
	})
}

// ListUsers returns the union of users reported by all engines, deduplicating by email.
// If an email appears on multiple engines (broadcast), the entry from the first engine
// that reports it is used (they should be identical for UUIDs etc).
func (m *MultiEngine) ListUsers(ctx context.Context) ([]pluginapi.VPNUserConfig, error) {
	type result struct {
		users []pluginapi.VPNUserConfig
		err   error
	}
	ch := make(chan result, len(m.engines))
	for _, e := range m.engines {
		go func(e pluginapi.EngineProvider) {
			users, err := e.ListUsers(ctx)
			ch <- result{users, err}
		}(e)
	}

	seen := make(map[string]struct{})
	var all []pluginapi.VPNUserConfig
	var errs []error
	for range m.engines {
		r := <-ch
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		for _, u := range r.users {
			if _, ok := seen[u.Email]; !ok {
				seen[u.Email] = struct{}{}
				all = append(all, u)
			}
		}
	}
	return all, errors.Join(errs...)
}

// SyncUsers regenerates configs and hot-adds/removes users on all engines.
// Returns the aggregated EngineSyncResult (Added/Removed summed across engines).
func (m *MultiEngine) SyncUsers(ctx context.Context, dbUsers []pluginapi.VPNUserConfig, removeOrphans bool) (*pluginapi.EngineSyncResult, error) {
	type result struct {
		r   *pluginapi.EngineSyncResult
		err error
		id  string
	}
	ch := make(chan result, len(m.engines))
	for _, e := range m.engines {
		go func(e pluginapi.EngineProvider) {
			r, err := e.SyncUsers(ctx, dbUsers, removeOrphans)
			ch <- result{r, err, e.ID()}
		}(e)
	}

	agg := &pluginapi.EngineSyncResult{}
	var errs []error
	for range m.engines {
		r := <-ch
		if r.err != nil {
			m.log.Warn("[multiengine] SyncUsers partial failure", "engine", r.id, "error", r.err)
			errs = append(errs, fmt.Errorf("engine %q: %w", r.id, r.err))
			continue
		}
		if r.r != nil {
			agg.Added += r.r.Added
			agg.Removed += r.r.Removed
		}
	}
	return agg, errors.Join(errs...)
}

// BuildClientLinks collects share links from all engines and returns the union.
// This is the implementation of ClientConfigContributor for the multi-engine case —
// subscription.go calls this instead of parsing vpn.RawConfig directly.
func (m *MultiEngine) BuildClientLinks(ctx context.Context, u pluginapi.VPNUserConfig) ([]pluginapi.ClientLink, error) {
	type linkResult struct {
		links []pluginapi.ClientLink
		err   error
	}
	targets := m.router.EnginesFor(u)
	ch := make(chan linkResult, len(targets))
	for _, e := range targets {
		go func(e pluginapi.EngineProvider) {
			links, err := e.BuildClientLinks(ctx, u)
			ch <- linkResult{links, err}
		}(e)
	}

	var all []pluginapi.ClientLink
	var errs []error
	for range targets {
		r := <-ch
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		all = append(all, r.links...)
	}
	return all, errors.Join(errs...)
}

// MultiEngine returns itself — convenience so Host.MultiEngine() can return a
// single object usable both as domain.Engine and as ClientConfigContributor.
func (h *Host) MultiEngine() *MultiEngine {
	engines := h.EngineProviders()
	if len(engines) == 0 {
		return nil
	}
	return NewMultiEngine(engines, h.log)
}
