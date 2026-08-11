package cmd

import (
	"net"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"xraytool/internal/appconfig"
	"xraytool/internal/logger"
)

// startServerCmd is the stable public server command.  The former imperative
// composition root has been retired: both this command and the temporary v2
// spelling now enter the PluginHost-based kernel in server_kernel.go.
func startServerCmd(deps *AppDeps) *cobra.Command {
	cmd := startServerKernelCmd(deps)
	cmd.Use = "start-server"
	cmd.Short = "Start the xraytool REST API server"
	cmd.Long = "Start the xraytool REST API server using the Plugin Host architecture."

	// These switches were accepted by the imperative server command but never
	// changed its runtime behaviour. Keep them as hidden compatibility flags so
	// existing service units and automation keep parsing while configuration is
	// now owned by appconfig/plugins.
	var legacyAPIConfig string
	var legacyRunMigrations bool
	cmd.Flags().StringVar(&legacyAPIConfig, "api-config", "xray_api_config.json", "legacy compatibility flag")
	cmd.Flags().BoolVar(&legacyRunMigrations, "run-migrations", false, "legacy compatibility flag")
	_ = cmd.Flags().MarkHidden("api-config")
	_ = cmd.Flags().MarkHidden("run-migrations")
	return cmd
}

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
	logger.Warnf("[INTRUDER] reason=%s ip=%s method=%s path=%s", reason, getClientIP(r), r.Method, r.URL.Path)
}

//nolint:unused
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
