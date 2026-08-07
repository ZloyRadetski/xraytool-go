package cmd

// server_kernel.go — composition root на базе Plugin Host.
//
// Команда: xraytool start-server-v2  (Phase 1.1)
//
// Strangler fig: start-server (cmd/server.go) не тронут.
// После стабилизации в проде — переименовать, удалить server.go (Phase 7).

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
	corePlugin "xraytool/internal/plugins/core"
	vpn "xraytool/internal/plugins/engine_xray"
)

func startServerKernelCmd(deps *AppDeps) *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "start-server-v2",
		Short: "[Phase 1.1] Start the API server using the Plugin Host architecture",
		Long: `start-server-v2 starts the same xraytool REST API as start-server,
but uses the Plugin Host (internal/pluginhost) for dependency management.

Phase 1.1: antifraud, mailer_resend, eventsink_webhook are loaded as proper
optional plugins. Once stable in production this command replaces start-server.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			if deps.Cfg.Server.APIKey == "" || deps.Cfg.Server.APIKey == "CHANGE_ME_IN_CONFIG" {
				return fmt.Errorf("FATAL: server.api_key не может быть пустым или дефолтным в xraytool.yml")
			}
			if !cmd.Flags().Changed("port") && deps.Cfg != nil {
				port = deps.Cfg.Ports.APIServer
			}
			defer deps.RunCleanup()
			return runKernelServer(cmd.Context(), deps, port)
		},
	}
	cmd.Flags().IntVar(&port, "port", 8080, "Port to listen on")
	return cmd
}

func runKernelServer(ctx context.Context, deps *AppDeps, port int) error {
	cfg := deps.Cfg

	// ── Step 1: Reuse kernel-owned VPN engine ───────────────────────────────
	// loadDependencies owns the connection and engine lifecycle for this
	// transition command. Reusing it prevents a second adapter from diverging
	// from the state-sync service attached to the first one.
	vpnEngine := deps.Engine
	if vpnEngine == nil {
		vpnEngine = vpn.NewAdapter(
			cfg.Xray.APIAddr, cfg.Paths.XrayConfig, cfg.Paths.XrayTemplate,
			cfg.Reality.RotationEnabled, cfg.Reality.KeysFilepath,
			cfg.BlacklistedAdmins, slog.Default(),
		)
		slog.Info("[KERNEL] Using Xray-core adapter",
			"grpc_addr", cfg.Xray.APIAddr, "template", cfg.Paths.XrayTemplate)
	}


	// ── Step 2: Build PluginsConfig from appconfig ────────────────────────────
	pluginsCfg := buildPluginsConfig(cfg)

	// ── Step 3: Build Plugin Host ─────────────────────────────────────────────
	// emitFn is a forward declaration — filled after host is assigned.
	var host *pluginhost.Host
	emitFn := func(eventType string, data map[string]any, userMeta map[string]any) {
		if host == nil {
			return
		}
		ev := pluginapi.Event{
			Type:       eventType,
			OccurredAt: time.Now(),
			Data:       data,
			UserMeta:   userMeta,
		}
		for _, sink := range host.EventSinks() {
			go func(s pluginapi.EventSink, e pluginapi.Event) {
				_ = s.Handle(context.Background(), e)
			}(sink, ev)
		}
	}

	factories := pluginhost.BuiltinRegistry(cfg)
	factories["core"] = func() pluginapi.Plugin {
		return corePlugin.NewWithRuntime(cfg, corePlugin.Runtime{
			Registry:   deps.Registry,
			Engine:     vpnEngine,
			Propagator: deps.Propagator,
		})
	}
	// Populate optional built-in factories (antifraud, cluster_sync, …).
	// configureOptionalPluginFactories is defined in server_kernel_optional.go
	// (build tag !minimal) and builds the fraud reporter for slave nodes.
	configureOptionalPluginFactories(factories, deps, vpnEngine, nil)

	host = pluginhost.New(
		pluginsCfg,
		slog.Default(),
		factories,
		emitFn,
	)

	if err := host.Load(ctx); err != nil {
		return fmt.Errorf("plugin host Load() failed: %w", err)
	}

	// ── Step 4: Resolve the fully initialised core plugin ─────────────────────
	core, err := getPlugin[*corePlugin.Plugin](host, "core")
	if err != nil {
		return err
	}

	// ── Step 5: Log eventsink_webhook status (best-effort) ───────────────────
	slog.Info("[KERNEL] Plugin Host loaded", "event_sinks", len(host.EventSinks()), "payment_providers", len(host.PaymentProviders()))

	// ── Step 6: Build HTTP Router ─────────────────────────────────────────────
	core.InitHTTPRouter(vpnEngine, cfg.Server.APIKey)
	apiRouter := core.HTTPRouter()

	paymentProviders := host.PaymentProviders()
	if len(paymentProviders) > 0 {
		apiRouter = apiRouter.WithPaymentProviders(paymentProviders)
	}

	// ── Step 7: Initial user sync (master only) ───────────────────────────────
	if cfg.IsMaster() && deps.Registry != nil && cfg.Paths.XrayTemplate != "" {
		if subs, err := deps.Registry.Subscriptions().FindAll(ctx); err != nil {
			slog.Warn("[KERNEL] Failed to load subscriptions for initial sync", "error", err)
		} else {
			dbUsers := make([]domain.VPNUserConfig, 0, len(subs))
			for _, sub := range subs {
				if sub.Status != "active" || sub.Email == "" || sub.UUID == "" {
					continue
				}
				dbUsers = append(dbUsers, vpn.SubscriptionToVPNUserConfig(sub))
			}
			if result, err := vpnEngine.SyncUsers(ctx, dbUsers, false); err != nil {
				slog.Warn("[KERNEL] Initial user sync failed", "error", err)
			} else {
				slog.Info("[KERNEL] Initial sync complete", "added", result.Added)
			}
		}
	}

	// ── Step 8: Wire optional plugins into the router ─────────────────────────
	if deps.Registry != nil {
	// ── Antifraud plugin ───────────────────────────────────────────────────
		// Use the pluginapi interface to avoid importing the concrete plugin
		// package from the kernel. Any loaded antifraud plugin satisfying
		// pluginapi.AntifraudProvider is wired the same way.
		if afProvider, ok := host.PluginByName("antifraud").(pluginapi.AntifraudProvider); ok && afProvider != nil {
			apiRouter.WithAntifraudProvider(afProvider, host.BanCache().IsBanned, cfg.IsMaster())
			slog.Info("[KERNEL] Anti-Fraud plugin wired",
				"log_path", cfg.AntiFraud.LogPath, "max_ips", cfg.AntiFraud.MaxIPs)
		}

		// ── Sync service (slave mode) ─────────────────────────────────────────
		if !cfg.IsMaster() {
			apiRouter.WithSyncService(nil, deps.Registry)
		}

		// Workers are now started inside the core plugin's Start() method.
		// SyncStates worker is managed by the clustersync plugin's Start() method.
	}

	// ── Step 9: HTTP server ───────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.Handle("/", core.HTTPHandler())
	mux.HandleFunc("/undefined", func(w http.ResponseWriter, r *http.Request) {
		logIntruder(r, "Hit catch-all undefined route")
		http.NotFound(w, r)
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf("127.0.0.1:%d", port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 20 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	slog.Info("[KERNEL] API server (v2/plugin-host) listening", "addr", srv.Addr)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	done := make(chan struct{})

	go func() {
		<-quit
		slog.Info("[KERNEL] Received shutdown signal — stopping gracefully")
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			slog.Error("[KERNEL] HTTP server forced close", "error", err)
		}
		if err := host.Shutdown(shutCtx); err != nil {
			slog.Error("[KERNEL] Plugin host shutdown error", "error", err)
		}
		slog.Info("[KERNEL] Shutdown complete")
		close(done)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("API server failed: %w", err)
	}
	<-done
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// getPlugin retrieves a loaded plugin by name and type-asserts to T.
func getPlugin[T any](host *pluginhost.Host, name string) (T, error) {
	var zero T
	raw := host.PluginByName(name)
	if raw == nil {
		return zero, fmt.Errorf("plugin %q not loaded — check BuiltinRegistry and PluginsConfig", name)
	}
	typed, ok := raw.(T)
	if !ok {
		return zero, fmt.Errorf("plugin %q has unexpected type %T", name, raw)
	}
	return typed, nil
}

// buildPluginsConfig translates appconfig.Config to PluginsConfig.
// Phase 2: replaced by reading plugins.yaml directly.
func buildPluginsConfig(cfg *appconfig.Config) pluginhost.PluginsConfig {
	result := make(pluginhost.PluginsConfig)
	for name, conf := range cfg.Plugins {
		result[name] = pluginhost.PluginEntry{
			Enabled:       conf.Enabled,
			Source:        conf.Source,
			RestartPolicy: pluginhost.RestartPolicy(conf.RestartPolicy),
			Config:        copyConfig(conf.Config),
		}
	}
	// Convert engines: section entries to plugin entries with "engine_" prefix.
	// e.g. engines.xray → engine_xray plugin entry.
	for name, conf := range cfg.Engines.Entries {
		result["engine_"+name] = pluginhost.PluginEntry{
			Enabled:       conf.Enabled,
			Source:        conf.Source,
			RestartPolicy: pluginhost.RestartPolicy(conf.RestartPolicy),
			Config:        copyConfig(conf.Config),
		}
	}
	// Always ensure core is enabled
	if _, exists := result["core"]; !exists {
		result["core"] = pluginhost.PluginEntry{Enabled: true, Source: "builtin"}
	} else {
		coreConf := result["core"]
		coreConf.Enabled = true
		result["core"] = coreConf
	}
	return result
}

// copyConfig returns a deep copy of a config map to prevent the caller from
// accidentally mutating the appconfig entries through the PluginsConfig map.
func copyConfig(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// prepareMultiEngine builds a MultiEngine from the engine plugin factories that
// are enabled in cfg. It is the Phase 1.5 composition helper (plan §2.6.2).
//
// factories is mutated in-place: a factory that returns a non-EngineProvider is
// silently skipped so the caller does not need to filter the registry first.
//
// routingMode is variadic so call sites that rely on the broadcast default do
// not need to supply it; only the first value is used.
func prepareMultiEngine(
	cfg pluginhost.PluginsConfig,
	factories map[string]func() pluginapi.Plugin,
	log *slog.Logger,
	routingMode ...string,
) (*pluginhost.MultiEngine, error) {
	var engines []pluginapi.EngineProvider
	var providers []pluginapi.EngineProvider
	for name, entry := range cfg {
		if !entry.Enabled {
			continue
		}
		factory, ok := factories[name]
		if !ok {
			continue
		}
		plugin := factory()
		ep, ok := plugin.(pluginapi.EngineProvider)
		if !ok {
			continue
		}
		// Restore the factory so the Host can reuse the same instance.
		captured := ep
		factories[name] = func() pluginapi.Plugin { return captured }
		engines = append(engines, ep)
		providers = append(providers, ep)
	}

	multi := pluginhost.NewMultiEngine(engines, log)

	mode := ""
	if len(routingMode) > 0 {
		mode = routingMode[0]
	}
	if mode != "" && mode != string(pluginhost.RoutingModeBroadcast) {
		router, err := pluginhost.NewConfiguredEngineRouter(mode, providers)
		if err != nil {
			return nil, fmt.Errorf("prepareMultiEngine: %w", err)
		}
		multi = multi.WithRouter(router)
	}
	return multi, nil
}
