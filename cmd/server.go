package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"xraytool/internal/antifraud"
	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/logger"
	"xraytool/internal/server"
	"xraytool/internal/slave"
	"xraytool/internal/subscription"
	"xraytool/internal/vpn"
	"xraytool/internal/worker"

	"github.com/spf13/cobra"
)

func getClientIP(r *http.Request) string {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}

	if remoteIP == "127.0.0.1" || remoteIP == "::1" || remoteIP == "localhost" {
		ip := r.Header.Get("X-Real-IP")
		if ip != "" && net.ParseIP(ip) != nil {
			return ip
		}
	}
	return remoteIP
}

func logIntruder(r *http.Request, reason string) {
	ip := getClientIP(r)
	dump, err := httputil.DumpRequest(r, false)
	dumpStr := "Не удалось сделать дамп"
	if err == nil {
		dumpStr = string(dump)
	}
	logger.Warnf("\n[!!!] INTRUDER ALERT | %s\nIP: %s\n--- Request Dump ---\n%s\n--------------------\n", reason, ip, dumpStr)
}

func isPathAllowed(cfg *appconfig.Config, path string) bool {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		realPath, err = filepath.Abs(path)
		if err != nil {
			return false
		}
	}

	for _, dir := range cfg.Server.AllowedDirs {
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			realDir, err = filepath.Abs(dir)
			if err != nil {
				continue
			}
		}

		rel, err := filepath.Rel(realDir, realPath)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}

func startServerCmd(deps *AppDeps) *cobra.Command {
	var port int
	var runMigrations bool
	var configPath string

	cmd := &cobra.Command{
		Use:   "start-server",
		Short: "Start the xraytool REST API server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}

			executablePath, err := os.Executable()
			if err != nil {
				return fmt.Errorf("fatal error: %v", err)
			}
			if deps.Cfg.Server.APIKey == "" || deps.Cfg.Server.APIKey == "CHANGE_ME_IN_CONFIG" {
				return fmt.Errorf("FATAL: server.api_key не может быть пустым или дефолтным в xraytool.yml")
			}

			if !cmd.Flags().Changed("port") && deps.Cfg != nil {
				port = deps.Cfg.Ports.APIServer
			}

			// ── Step 5.1: Check Database ─────────────────────────
			dbReady := deps.Registry != nil
			if dbReady {
				logger.Infof("[DB] Database ready (driver: %s)", deps.Cfg.Database.Driver)
			} else {
				logger.Errorf("[!] Database not ready")
			}

			// Graceful shutdown: close DB connection when server exits.
			defer func() {
				for _, cleanup := range deps.Cleanup {
					cleanup()
				}
			}()

			// ── Step 5.1.5: Build the VPN Engine (Composition Root) ───────────────
			// This is the ONLY place in the entire codebase that decides which
			// concrete engine adapter to use. Everything downstream receives
			// a vpn.Engine interface and is fully engine-agnostic.
			var vpnEngine domain.Engine
			switch deps.Cfg.Engine.Type {
			// Future engines go here:
			// case "singbox":
			//     vpnEngine = singbox.New(deps.Cfg.Singbox.APIAddr, deps.Cfg.Paths.SingboxConfig, slog.Default())
			default:
				// Default: Xray-core adapter
				vpnEngine = vpn.NewAdapter(deps.Cfg.Xray.APIAddr, deps.Cfg.Paths.XrayConfig, slog.Default())
				logger.Infof("[ENGINE] Using Xray-core adapter (grpc_addr=%s)", deps.Cfg.Xray.APIAddr)
			}

			mux := http.NewServeMux()
			xraytoolPath := executablePath

			// Initialize CacheManager globally for the server
			cacheManager := subscription.NewCacheManager(deps.Cfg, vpnEngine)
			cacheManager.Refresh()

			// ── Step 5.2: Mount REST API router (when DB is ready) ────────────
			var apiRouter *server.Router
			if dbReady {
				var reporter domain.FraudEventReporter

				slaveClient := slave.NewClient(deps.Cfg.SlaveAPI.ConnectTimeout, deps.Cfg.SlaveAPI.RequestTimeout, deps.Cfg.SlaveAPI.RemotePath)

				if !deps.Cfg.IsMaster() && deps.Cfg.MasterAPI.URL != "" && deps.Cfg.AntiFraud.ReportToMaster {
					entry := slave.Entry{
						URL:      deps.Cfg.MasterAPI.URL,
						APIKey:   deps.Cfg.MasterAPI.APIKey,
						Insecure: deps.Cfg.MasterAPI.Insecure,
					}
					repAdp := slave.NewFraudReporterAdapter(slaveClient, entry, slog.Default())
					go repAdp.Run(context.Background())
					reporter = repAdp
				}

				apiRouter = server.New(deps.Cfg, deps.Cfg.Server.APIKey, cacheManager, vpnEngine, deps.UserSvc, deps.PaymentSvc, deps.Dispatcher, slog.Default())

				// 🟢 Anti-Fraud Module 🟢──────────────────────────────────────────────
				if deps.Cfg.AntiFraud.Enabled {
					saltAPIKey := deps.Cfg.Server.APIKey
					if !deps.Cfg.IsMaster() && deps.Cfg.MasterAPI.APIKey != "" {
						saltAPIKey = deps.Cfg.MasterAPI.APIKey
					}
					if deps.Cfg.AntiFraud.SaltSecret != "" {
						saltAPIKey = deps.Cfg.AntiFraud.SaltSecret
					}

					afConfig := &antifraud.Config{
						Enabled:               deps.Cfg.AntiFraud.Enabled,
						DryRun:                deps.Cfg.AntiFraud.DryRun,
						LogPath:               deps.Cfg.AntiFraud.LogPath,
						LogRotationSizeMB:     deps.Cfg.AntiFraud.LogRotationSizeMB,
						LogRotationMaxAge:     deps.Cfg.AntiFraud.LogRotationMaxAge,
						IPLimitTTL:            deps.Cfg.AntiFraud.IPLimitTTL,
						BanDuration:           deps.Cfg.AntiFraud.BanDuration,
						SuspiciousIPThreshold: deps.Cfg.AntiFraud.MaxIPs,
						ReportToMaster:        deps.Cfg.AntiFraud.ReportToMaster,
						IsMaster:              deps.Cfg.IsMaster(),
						APIKey:                saltAPIKey,
					}
					afModule := antifraud.New(afConfig, deps.Registry, vpnEngine, vpnEngine, deps.Propagator, reporter, slog.Default())
					var ingestFn func(string, []domain.FraudEvent)
					if deps.Cfg.IsMaster() {
						ingestFn = afModule.IngestEvents
					}
					apiRouter.WithAntiFraud(afModule.IsBanned, afModule.ForceUnban, afModule.GetSnapshot, ingestFn)
					go afModule.Run(context.Background())
					logger.Infof("[ANTIFRAUD] Anti-Fraud module started (log_path=%s, max_ips=%d)",
						deps.Cfg.AntiFraud.LogPath, deps.Cfg.AntiFraud.MaxIPs)
				} else {
					logger.Infof("[ANTIFRAUD] Anti-Fraud module DISABLED in config")
				}

				mux.Handle("/", apiRouter)
				logger.Infof("[API] REST API v1 handlers mounted (users, payments, admin)")

				if deps.Cfg.Worker.Enabled {
					wkr := worker.NewExpiryWorker(deps.Registry, deps.Cfg, deps.Dispatcher, vpnEngine, slog.Default())
					go wkr.Run(context.Background())
					logger.Infof("[WORKER] Background Expiry Worker started with interval %s", deps.Cfg.Worker.ExpiryInterval)

					// Start the Data Scrubber for Privacy
					scrubber := worker.NewScrubberWorker(deps.PaymentSvc, slog.Default())
					go scrubber.Run(context.Background())
					logger.Infof("[WORKER] Background Privacy Scrubber started (24-hour payment footprint retention)")
				} else {
					logger.Infof("[WORKER] Background Expiry Worker is DISABLED in config")
				}
			} else {
				logger.Warnf("DB not ready, skipping API initialization. Only static endpoints would be available if they existed.")
			}

			mux.HandleFunc("/undefined", func(w http.ResponseWriter, r *http.Request) {
				logIntruder(r, "Hit catch-all undefined route")
				http.NotFound(w, r)
			})

			logger.Infof(" Server:  127.0.0.1:%d", port)
			logger.Infof(" Script:  %s", xraytoolPath)
			logger.Infof(" Allowed: %s", strings.Join(deps.Cfg.Server.AllowedDirs, ", "))

			logger.Infof("API server listening on 127.0.0.1:%d", port)

			srv := &http.Server{
				Addr:         fmt.Sprintf("127.0.0.1:%d", port),
				Handler:      mux,
				ReadTimeout:  10 * time.Second,
				WriteTimeout: 20 * time.Second,
				IdleTimeout:  120 * time.Second,
			}

			// ── Graceful shutdown ────────────────────────────────────────────────
			// Listen for OS termination signals so we can drain in-flight requests
			// and webhook deliveries before the process exits.
			quit := make(chan os.Signal, 1)
			signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
			done := make(chan struct{})
			go func() {
				<-quit
				logger.Infof("[SHUTDOWN] Received termination signal — shutting down gracefully")

				// 1. Stop accepting new HTTP requests (30-second grace period).
				shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := srv.Shutdown(shutCtx); err != nil {
					logger.Errorf("[SHUTDOWN] HTTP server forced to close: %v", err)
				}

				// 2. Wait for all in-flight webhook deliveries and background tasks.
				if apiRouter != nil {
					logger.Infof("[SHUTDOWN] Waiting for background tasks and webhook deliveries...")
					apiRouter.Shutdown()
				} else {
					logger.Infof("[SHUTDOWN] Waiting for in-flight webhook deliveries...")
					deps.Dispatcher.Shutdown()
				}
				logger.Infof("[SHUTDOWN] All background tasks complete. Bye.")
				close(done)
			}()

			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Errorf("API Server failed: %v", err)
				return err
			}
			<-done
			return nil
		},
	}

	cmd.Flags().IntVar(&port, "port", 8080, "Port to listen on")
	cmd.Flags().StringVar(&configPath, "api-config", "xray_api_config.json", "path to API config json")
	cmd.Flags().BoolVar(&runMigrations, "run-migrations", false, "Run database AutoMigrate on startup")
	return cmd
}
