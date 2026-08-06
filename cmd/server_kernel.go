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
	antifraudPlugin "xraytool/internal/plugins/antifraud"
	corePlugin "xraytool/internal/plugins/core"
	eventsinkPlugin "xraytool/internal/plugins/eventsink_webhook"
	"xraytool/internal/slave"
	"xraytool/internal/statesync"
	"xraytool/internal/vpn"
	"xraytool/internal/worker"
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

	// The antifraud module needs its optional slave reporter before Host.Load so
	// the plugin is fully initialised before Start is called.
	var fraudReporter domain.FraudEventReporter
	if cfg.AntiFraud.Enabled && !cfg.IsMaster() && cfg.MasterAPI.URL != "" && cfg.AntiFraud.ReportToMaster {
		client := slave.NewClient(cfg.SlaveAPI.ConnectTimeout, cfg.SlaveAPI.RequestTimeout, cfg.SlaveAPI.RemotePath)
		entry := slave.Entry{
			URL: cfg.MasterAPI.URL, APIKey: cfg.MasterAPI.APIKey, Insecure: cfg.MasterAPI.Insecure,
		}
		reporter := slave.NewFraudReporterAdapter(client, entry, slog.Default())
		go reporter.Run(ctx)
		fraudReporter = reporter
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
	factories["antifraud"] = func() pluginapi.Plugin {
		return antifraudPlugin.NewWithRuntime(antifraudPlugin.Runtime{
			Registry:   deps.Registry,
			Banner:     vpnEngine,
			LoggerCtl:  vpnEngine,
			Propagator: deps.Propagator,
			Reporter:   fraudReporter,
		})
	}

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

	// ── Step 5: Log eventsink_webhook status ─────────────────────────────────
	if evSink, ok := host.PluginByName("eventsink_webhook").(*eventsinkPlugin.Plugin); ok && evSink != nil {
		slog.Info("[KERNEL] eventsink_webhook plugin active — webhook events will be delivered via plugin")
	}

	// ── Step 6: Build HTTP Router ─────────────────────────────────────────────
	core.InitHTTPRouter(vpnEngine, cfg.Server.APIKey)
	apiRouter := core.HTTPRouter()

	// ── Step 7: Initial user sync (master only) ───────────────────────────────
	if cfg.IsMaster() && deps.Registry != nil && cfg.Paths.XrayTemplate != "" {
		if subs, err := deps.Registry.Subscriptions().FindAll(ctx); err != nil {
			slog.Warn("[KERNEL] Failed to load subscriptions for initial sync", "error", err)
		} else {
			dbUsers := make([]domain.VPNUserConfig, 0, len(subs))
			for _, sub := range subs {
				if sub.Status != "active" || sub.Email == "" || sub.XrayUUID == "" {
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
		// ── Antifraud plugin ─────────────────────────────────────────────────
		if afPlugin, ok := host.PluginByName("antifraud").(*antifraudPlugin.Plugin); ok && afPlugin != nil && afPlugin.Config().Enabled {
			mod := afPlugin.Module()
			if mod == nil {
				return fmt.Errorf("antifraud plugin loaded without an initialised module")
			}
			var ingestFn func(string, []domain.FraudEvent)
			if cfg.IsMaster() {
				ingestFn = mod.IngestEvents
			}
			apiRouter.WithAntiFraud(mod.IsBanned, mod.ForceUnban, mod.GetSnapshot, ingestFn)
			slog.Info("[KERNEL] Anti-Fraud plugin wired",
				"log_path", cfg.AntiFraud.LogPath, "max_ips", cfg.AntiFraud.MaxIPs)
		}

		// ── Sync service (slave mode) ─────────────────────────────────────────
		if !cfg.IsMaster() {
			apiRouter.WithSyncService(nil, deps.Registry)
		}

		// ── Workers ───────────────────────────────────────────────────────────
		if cfg.Worker.Enabled {
			wkr := worker.NewExpiryWorker(deps.Registry, cfg, core.Dispatcher(), vpnEngine, slog.Default())
			go wkr.Run(context.Background())
			slog.Info("[KERNEL] Expiry Worker started", "interval", cfg.Worker.ExpiryInterval)

			if cfg.IsMaster() && len(cfg.SlaveServers) > 0 {
				syncInterval, parseErr := time.ParseDuration(cfg.Worker.SyncStatesInterval)
				if parseErr != nil || syncInterval <= 0 {
					slog.Warn("[KERNEL] Invalid sync_states_interval, falling back to 3m")
					syncInterval = 3 * time.Minute
				}
				syncSvc := deps.SyncSvc
				if syncSvc == nil {
					syncSvc = statesync.NewService(deps.Registry, vpnEngine, deps.SlaveProvider, slog.Default())
				}
				apiRouter.WithSyncService(syncSvc, deps.Registry)
				syncWkr := worker.NewSyncStatesWorker(syncSvc, syncInterval, slog.Default())
				go syncWkr.Run(context.Background())
				slog.Info("[KERNEL] SyncStates Worker started", "interval", syncInterval)

				if adapter, ok := vpnEngine.(*vpn.Adapter); ok {
					adapter.OnConfigModified = func() {
						slog.Info("[KERNEL] Config modified, triggering slave sync")
						go func() {
							if _, err := syncSvc.SyncAllSlaves(context.Background(), false, false); err != nil {
								slog.Error("[KERNEL] Triggered sync failed", "error", err)
							}
						}()
					}
				}
			}

			scrubber := worker.NewScrubberWorker(core.PaymentSvc(), slog.Default())
			go scrubber.Run(context.Background())
			slog.Info("[KERNEL] Privacy Scrubber started")
		}
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
	webhooksIface := make([]interface{}, len(cfg.Webhooks))
	for i, w := range cfg.Webhooks {
		webhooksIface[i] = w
	}

	return pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "builtin"},
		"eventsink_webhook": {
			Enabled: len(cfg.Webhooks) > 0,
			Source:  "builtin",
			Config: pluginapi.RawConfig{
				"webhooks":       webhooksIface,
				"webhook_secret": cfg.WebhookSecret,
			},
		},
		"antifraud": {
			Enabled: cfg.AntiFraud.Enabled,
			Source:  "builtin",
			Config: pluginapi.RawConfig{
				"enabled":              cfg.AntiFraud.Enabled,
				"dry_run":              cfg.AntiFraud.DryRun,
				"log_path":             cfg.AntiFraud.LogPath,
				"max_ips":              cfg.AntiFraud.MaxIPs,
				"ip_limit_ttl":         cfg.AntiFraud.IPLimitTTL,
				"ban_duration":         cfg.AntiFraud.BanDuration,
				"log_rotation_size_mb": cfg.AntiFraud.LogRotationSizeMB,
				"log_rotation_max_age": cfg.AntiFraud.LogRotationMaxAge,
				"report_to_master":     cfg.AntiFraud.ReportToMaster,
				"salt_secret":          cfg.AntiFraud.SaltSecret,
				"is_master":            cfg.IsMaster(),
			},
		},
		"mailer_resend": {
			Enabled: cfg.Mailer.Enabled && cfg.Mailer.ResendAPIKey != "",
			Source:  "builtin",
			Config: pluginapi.RawConfig{
				"resend_api_key": cfg.Mailer.ResendAPIKey,
				"from_email":     cfg.Mailer.FromEmail,
			},
		},
	}
}
