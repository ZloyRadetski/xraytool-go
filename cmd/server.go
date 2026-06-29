package cmd

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xraytool/internal/database"
	"xraytool/internal/events"
	"xraytool/internal/logger"
	"xraytool/internal/server"
	"xraytool/internal/subscription"
	"xraytool/internal/worker"
	"xraytool/internal/xrayapi"
	"xraytool/internal/antifraud"

	"github.com/spf13/cobra"
)

type ApiConfig struct {
	APIKey      string   `json:"api_key"`
	AllowedDirs []string `json:"allowed_dirs"`
}

var apiConfig ApiConfig

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

func isPathAllowed(path string) bool {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		realPath, err = filepath.Abs(path)
		if err != nil {
			return false
		}
	}

	for _, dir := range apiConfig.AllowedDirs {
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
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			executablePath, err := os.Executable()
			if err != nil {
				fmt.Printf("Fatal error: %v\n", err)
				osExit(1)
				return
			}
			baseDir := filepath.Dir(executablePath)

			if configPath == "" {
				configPath = "xray_api_config.json"
			}

			if configPath == "xray_api_config.json" {
				configPath = filepath.Join(baseDir, "xray_api_config.json")
				if _, err := os.Stat(configPath); os.IsNotExist(err) {
					configPath = "/etc/xraytool/xray_api_config.json"
					if _, err := os.Stat(configPath); os.IsNotExist(err) {
						configPath = "xray_api_config.json"
					}
				}
			}

			confFile, err := os.ReadFile(configPath)
			if err != nil {
				fmt.Printf("Ошибка: Не удалось найти конфиг %s. Работа сервера невозможна.\n", configPath)
				osExit(1)
				return
			}

			if err := json.Unmarshal(confFile, &apiConfig); err != nil {
				fmt.Printf("Ошибка: Битый формат конфига %s: %v\n", configPath, err)
				osExit(1)
				return
			}

			if apiConfig.APIKey == "" {
				fmt.Printf("FATAL: api_key не может быть пустым в xray_api_config.json\n")
				osExit(1)
				return
			}

			if !cmd.Flags().Changed("port") && cfg != nil {
				port = cfg.Ports.APIServer
			}

			// ── Step 5.1: Initialize database (non-fatal) ─────────────────────────
			dbReady := false
			if cfg != nil {
				dbErr := database.Init(database.Config{
					Driver:      cfg.Database.Driver,
					DSN:         cfg.Database.DSN,
					SQLitePath:  cfg.Database.SQLitePath,
					AutoMigrate: runMigrations,
				})
				if dbErr != nil {
					// Non-fatal: server continues without DB (legacy routes still work).
					// Operator must fix DB config before new API endpoints become available.
					logger.Errorf("[!] Database init failed: %v", dbErr)
				} else {
					dbReady = true
					if runMigrations {
						if err := database.AutoMigrateAll(); err != nil {
							logger.Errorf("[!] Database auto-migrate failed: %v", err)
						} else {
							logger.Infof("[DB] Database auto-migration completed successfully")
						}
					}
					logger.Infof("[DB] Database initialized successfully (driver: %s)", cfg.Database.Driver)
				}
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

			subHandler := func(w http.ResponseWriter, r *http.Request) {
				// 1. Collect request headers
				headers := make(map[string]string)
				for k, v := range r.Header {
					if len(v) > 0 {
						headers[k] = v[0]
					}
				}
				headers["x-request-path"] = r.URL.Path

				// 2. Parse query parameters into map[string]string
				query := make(map[string]string)
				for k, v := range r.URL.Query() {
					if len(v) > 0 {
						query[k] = v[0]
					}
				}

				// 3. Get remote IP
				remoteAddr := getClientIP(r)
				isBot := strings.Contains(strings.ToLower(r.UserAgent()), "megasupersecretua")
				if isBot {
					logger.Debugf("Incoming subscription request from IP: %s, User-Agent: %s, Path: %s", remoteAddr, r.UserAgent(), r.URL.Path)
				} else {
					logger.Infof("Incoming subscription request from IP: %s, User-Agent: %s, Path: %s", remoteAddr, r.UserAgent(), r.URL.Path)
				}

				// 4. Build subscription request payload
				subReq := &subscription.Request{
					RemoteAddr: remoteAddr,
					UserAgent:  r.UserAgent(),
					Query:      query,
					Headers:    headers,
				}

				// 5. Execute subscription process directly in memory (No exec.Command)
				subRes := subscription.ProcessSQL(r.Context(), database.DB(), cacheManager, dispatcher, subReq, nil)

				subID := subRes.SubID
				if subID == "" {
					_, subID = subscription.ResolveClientID(subReq)
					if subID == "" {
						subID = "unknown"
					}
				}

				// 6. Send headers and write body
				for k, v := range subRes.Headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(subRes.StatusCode)
				if _, err := w.Write([]byte(subRes.Body)); err != nil {
					logger.Errorf("Ошибка записи ответа подписки: %v", err)
				}
				if isBot && subRes.StatusCode < 400 {
					logger.Debugf("Successfully served subscription to %s, sub_id: %s, status: %d", remoteAddr, subID, subRes.StatusCode)
				} else if subRes.StatusCode >= 400 {
					reason := subRes.ErrorReason
					if reason == "" {
						reason = "unknown reason"
					}
					logger.Warnf("Failed to serve subscription to %s, sub_id: %s, status: %d, reason: %s", remoteAddr, subID, subRes.StatusCode, reason)
				} else {
					logger.Infof("Successfully served subscription to %s, sub_id: %s, status: %d", remoteAddr, subID, subRes.StatusCode)
				}
			}

			mux.HandleFunc("/client", subHandler)
			mux.HandleFunc("/api/v1/sub", subHandler)

			mux.HandleFunc("/api/rest/update-links", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					logIntruder(r, "Wrong HTTP Method on update-links")
					http.NotFound(w, r)
					return
				}

				reqKey := r.Header.Get("X-API-Key")
				if subtle.ConstantTimeCompare([]byte(reqKey), []byte(apiConfig.APIKey)) != 1 {
					logIntruder(r, "Invalid or Missing API Key on update-links")
					http.NotFound(w, r)
					return
				}

				if err := r.ParseMultipartForm(10 << 20); err != nil {
					logIntruder(r, fmt.Sprintf("Form Parse Error: %v", err))
					http.NotFound(w, r)
					return
				}

				destPath := r.FormValue("path")
				if destPath == "" {
					logIntruder(r, "Missing 'path' field in update-links")
					http.NotFound(w, r)
					return
				}

				if !isPathAllowed(destPath) {
					logIntruder(r, fmt.Sprintf("Path Not Allowed: %s", destPath))
					http.NotFound(w, r)
					return
				}

				file, _, err := r.FormFile("file")
				if err != nil {
					logIntruder(r, "Missing 'file' field in multipart form")
					http.NotFound(w, r)
					return
				}
				defer file.Close()

				if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
					logger.Errorf(" [X] Ошибка создания директорий: %v", err)
					http.Error(w, `{"error": "disk error"}`, http.StatusInternalServerError)
					return
				}

				dst, err := os.Create(destPath)
				if err != nil {
					logger.Errorf(" [X] Ошибка записи файла: %v", err)
					http.Error(w, `{"error": "disk error"}`, http.StatusInternalServerError)
					return
				}
				defer dst.Close()

				const maxUploadBytes = 10 * 1024 * 1024 // 10 MB
				if _, err := io.Copy(dst, io.LimitReader(file, maxUploadBytes)); err != nil {
					logger.Errorf(" [X] Ошибка записи тела файла: %v", err)
					http.Error(w, `{"error": "write error"}`, http.StatusInternalServerError)
					return
				}

				go func() {
					if cfg != nil {
						dispatcher := events.NewDispatcher(cfg)
						dispatcher.Dispatch("file.uploaded", map[string]interface{}{
							"path": destPath,
						}, nil)
					}
				}()

				logger.Infof(" [V] Файл %s успешно загружен от %s (вебхук отправлен)", destPath, getClientIP(r))

				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"status": "success", "message": "file saved & webhook dispatched"}`))
			})

			mux.HandleFunc("/api/rest/upload", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					logIntruder(r, "Wrong HTTP Method on Upload")
					http.NotFound(w, r)
					return
				}

				reqKey := r.Header.Get("X-API-Key")
				if subtle.ConstantTimeCompare([]byte(reqKey), []byte(apiConfig.APIKey)) != 1 {
					logIntruder(r, "Invalid or Missing API Key on Upload")
					http.NotFound(w, r)
					return
				}

				if err := r.ParseMultipartForm(10 << 20); err != nil {
					logIntruder(r, fmt.Sprintf("Form Parse Error: %v", err))
					http.NotFound(w, r)
					return
				}

				destPath := r.FormValue("path")
				if destPath == "" {
					logIntruder(r, "Missing 'path' field in upload")
					http.NotFound(w, r)
					return
				}

				if !isPathAllowed(destPath) {
					logIntruder(r, fmt.Sprintf("Path Not Allowed: %s", destPath))
					http.NotFound(w, r)
					return
				}

				logger.Infof("Incoming upload request from %s for path %s", getClientIP(r), destPath)

				file, _, err := r.FormFile("file")
				if err != nil {
					logIntruder(r, "Missing 'file' field in multipart form")
					http.NotFound(w, r)
					return
				}
				defer file.Close()

				if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
					logger.Errorf(" [X] Ошибка создания директорий: %v", err)
					http.Error(w, `{"error": "disk error"}`, http.StatusInternalServerError)
					return
				}

				dst, err := os.Create(destPath)
				if err != nil {
					logger.Errorf(" [X] Ошибка записи файла: %v", err)
					http.Error(w, `{"error": "disk error"}`, http.StatusInternalServerError)
					return
				}
				defer dst.Close()

				const maxUploadBytes2 = 10 * 1024 * 1024 // 10 MB
				if _, err := io.Copy(dst, io.LimitReader(file, maxUploadBytes2)); err != nil {
					logger.Errorf(" [X] Ошибка записи тела файла: %v", err)
					http.Error(w, `{"error": "write error"}`, http.StatusInternalServerError)
					return
				}
				logger.Infof(" [V] Файл %s успешно загружен от %s", destPath, getClientIP(r))

				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"status": "success", "message": "file saved"}`))
			})

			mux.HandleFunc("/api/rest/download", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					logIntruder(r, "Wrong HTTP Method on Download")
					http.NotFound(w, r)
					return
				}

				reqKey := r.Header.Get("X-API-Key")
				if subtle.ConstantTimeCompare([]byte(reqKey), []byte(apiConfig.APIKey)) != 1 {
					logIntruder(r, "Invalid or Missing API Key on Download")
					http.NotFound(w, r)
					return
				}

				srcPath := r.URL.Query().Get("path")
				if srcPath == "" {
					logIntruder(r, "Missing 'path' parameter in download")
					http.NotFound(w, r)
					return
				}

				if !isPathAllowed(srcPath) {
					logIntruder(r, fmt.Sprintf("Path Not Allowed: %s", srcPath))
					http.NotFound(w, r)
					return
				}

				logger.Infof("Incoming download request from %s for path %s", getClientIP(r), srcPath)

				info, err := os.Stat(srcPath)
				if os.IsNotExist(err) || info.IsDir() {
					logIntruder(r, fmt.Sprintf("File Not Found: %s", srcPath))
					http.NotFound(w, r)
					return
				}

				logger.Infof(" [V] Отправка файла %s для %s", srcPath, getClientIP(r))
				http.ServeFile(w, r, srcPath)
			})

			// ── Step 5.2: Mount new REST API router (when DB is ready) ────────────
			// The new router handles /api/v1/users/*, /api/v1/payments/*, /api/v1/admin/* and /api/v2/sub-test.
			// Existing /client and /api/v1/sub are already registered above and take
			// priority due to Go 1.22+ most-specific-match routing — no conflict.
			if dbReady {
				apiRouter := server.New(cfg, apiConfig.APIKey, cacheManager)

				// ── Anti-Fraud Module ──────────────────────────────────────────────
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

				mux.Handle("/api/", apiRouter)
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
				logger.Warnf("[API] REST API v1 handlers NOT mounted (database unavailable)")
			}

			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				logIntruder(r, "Hit catch-all undefined route")
				http.NotFound(w, r)
			})

			logger.Infof(" Server:  127.0.0.1:%d", port)
			logger.Infof(" Script:  %s", xraytoolPath)
			logger.Infof(" Allowed: %s", strings.Join(apiConfig.AllowedDirs, ", "))

			logger.Infof("API server listening on 127.0.0.1:%d", port)
			
			srv := &http.Server{
				Addr:         fmt.Sprintf("127.0.0.1:%d", port),
				Handler:      mux,
				ReadTimeout:  10 * time.Second,
				WriteTimeout: 20 * time.Second,
				IdleTimeout:  120 * time.Second,
			}
			
			if err := srv.ListenAndServe(); err != nil {
				logger.Errorf("API Server failed: %v", err)
				log.Fatal(err)
			}
		},
	}

	cmd.Flags().IntVar(&port, "port", 8080, "Port to listen on")
	cmd.Flags().StringVar(&configPath, "api-config", "xray_api_config.json", "path to API config json")
	cmd.Flags().BoolVar(&runMigrations, "run-migrations", false, "Run database AutoMigrate on startup")
	return cmd
}
