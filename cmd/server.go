package cmd

import (
	"context"
	"fmt"
	"log/slog"
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
	"xraytool/internal/database"
	"xraytool/internal/events"
	"xraytool/internal/logger"
	"xraytool/internal/server"
	"xraytool/internal/subscription"
	"xraytool/internal/worker"
	"xraytool/internal/xrayapi"

	"github.com/spf13/cobra"
)



func getClientIP(r *http.Request) string {
	ip := r.Header.Get("X-Real-IP")
	if ip == "" {
		ip = r.RemoteAddr
	}

	port := r.Header.Get("X-Real-Port")
	if port != "" && !strings.Contains(ip, ":") {
		ip = ip + ":" + port
	}

	return ip
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

func startServerCmd() *cobra.Command {
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
			if cfg.Server.APIKey == "" || cfg.Server.APIKey == "CHANGE_ME_IN_CONFIG" {
				return fmt.Errorf("FATAL: server.api_key не может быть пустым или дефолтным в xraytool.yml")
			}

			if !cmd.Flags().Changed("port") && cfg != nil {
				port = cfg.Ports.APIServer
			}

			// ── Step 5.1: Check Database ─────────────────────────
			dbReady := database.DB() != nil
			if dbReady {
				logger.Infof("[DB] Database ready (driver: %s)", cfg.Database.Driver)
			} else {
				logger.Errorf("[!] Database not ready")
			}

			// Graceful shutdown: close DB connection when server exits.
			defer func() {
				if err := database.Close(); err != nil {
					logger.Errorf("[DB] Failed to close database: %v", err)
				}
			}()

			mux := http.NewServeMux()
			xraytoolPath := executablePath

			// Initialize CacheManager globally for the server
			cacheManager := subscription.NewCacheManager(cfg)
			cacheManager.Refresh()

			// Initialize Dispatcher globally for the server to reuse http.Client
			dispatcher := events.NewDispatcher(cfg)

			// ── Step 5.2: Mount REST API router (when DB is ready) ────────────
			var apiRouter *server.Router
			if dbReady {
				apiRouter = server.New(cfg, cfg.Server.APIKey, cacheManager, database.DB())

				// 🟢 Anti-Fraud Module 🟢──────────────────────────────────────────────
				if cfg.AntiFraud.Enabled {
					afModule := antifraud.New(cfg, database.DB(), slog.Default())
					// On master: expose IngestEvents so slaves can forward IP events here.
					// On slave: IngestEvents is unused (nil hook is safe — router guards it).
					var ingestFn func(string, []antifraud.SlaveIPEvent)
					if cfg.IsMaster() {
						ingestFn = afModule.IngestEvents
					}
					apiRouter.WithAntiFraud(afModule.IsBanned, afModule.ForceUnban, afModule.GetSnapshot, ingestFn)
					go afModule.Run(context.Background())
					logger.Infof("[ANTIFRAUD] Anti-Fraud module started (log_path=%s, max_ips=%d)",
						cfg.AntiFraud.LogPath, cfg.AntiFraud.MaxIPs)
				} else {
					logger.Infof("[ANTIFRAUD] Anti-Fraud module DISABLED in config")
				}

				mux.Handle("/", apiRouter)
				logger.Infof("[API] REST API v1 handlers mounted (users, payments, admin)")

				if cfg.Worker.Enabled {
					apiClient := xrayapi.NewGRPCClient(cfg.Xray.APIAddr)
					wkr := worker.NewExpiryWorker(database.DB(), cfg, events.NewDispatcher(cfg), apiClient, slog.Default())
					go wkr.Run(context.Background())
					logger.Infof("[WORKER] Background Expiry Worker started with interval %s", cfg.Worker.ExpiryInterval)
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
			logger.Infof(" Allowed: %s", strings.Join(cfg.Server.AllowedDirs, ", "))

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
					dispatcher.Shutdown()
				}
				logger.Infof("[SHUTDOWN] All background tasks complete. Bye.")
			}()

			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Errorf("API Server failed: %v", err)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&port, "port", 8080, "Port to listen on")
	cmd.Flags().StringVar(&configPath, "api-config", "xray_api_config.json", "path to API config json")
	cmd.Flags().BoolVar(&runMigrations, "run-migrations", false, "Run database AutoMigrate on startup")
	return cmd
}
