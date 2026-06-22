package cmd

import (
	"encoding/json"
	"fmt"

	"xraytool/internal/slave"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)



func applyBatchCmd() *cobra.Command {
	var payloadStr string

	cmd := &cobra.Command{
		Use:   "apply-batch",
		Short: "Apply a batch of user operations at once",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			if payloadStr == "" {
				printJSON(map[string]interface{}{"ok": false, "error": "payload is required"})
				return
			}

			var payload slave.BatchPayload
			if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
				printJSON(map[string]interface{}{"ok": false, "error": fmt.Sprintf("invalid json payload: %v", err)})
				return
			}

			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				printJSON(map[string]interface{}{"ok": false, "error": fmt.Sprintf("reading config: %v", err)})
				return
			}
			
			// Keep a clean copy of original config for tag extraction during Hot-Remove
			originalCfg, _ := xrayconfig.Read(cfg.Paths.XrayConfig)

			// Apply Removes
			for _, email := range payload.Remove {
				_ = xrayconfig.RemoveUserFromAllInbounds(xrayCfg, email)
			}

			// Apply Adds
			for _, u := range payload.Add {
				// Remove first to ensure a clean replace if they already exist
				_ = xrayconfig.RemoveUserFromAllInbounds(xrayCfg, u.Email)

				params := xrayconfig.ClientParams{
					Email:   u.Email,
					UUID:    u.UUID,
					Auth:    u.Auth,
					Subfile: u.Subfile,
					Expire:  u.Expire,
					Limit:   u.Limit,
				}
				tagged, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
				if err == nil && len(tagged) > 0 {
					_ = xrayconfig.AddUserToInbounds(xrayCfg, tagged)
				}
			}

			// Removed apply Limits

			// Write config
			if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
				printJSON(map[string]interface{}{"ok": false, "error": fmt.Sprintf("writing config: %v", err)})
				return
			}

			// Apply Hot-Reload using Xray API
			apiClient := xrayapi.New(cfg.Xray.APIAddr)
			
			// 1. Hot-Remove
			for _, email := range payload.Remove {
				tags, _ := xrayconfig.InboundTagsForUser(originalCfg, email)
				_ = apiClient.RemoveUser(email, tags)
			}
			
			// 2. Hot-Add/Update
			for _, u := range payload.Add {
				params := xrayconfig.ClientParams{
					Email:   u.Email,
					UUID:    u.UUID,
					Auth:    u.Auth,
					Subfile: u.Subfile,
					Expire:  u.Expire,
					Limit:   u.Limit,
				}
				tagged, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
				if err == nil && len(tagged) > 0 {
					_ = apiClient.AddUser(tagged, cfg.Paths.XrayConfig)
				}
			}

			// Systemctl restart for fallback/legacy systems (no-op in Docker)
			systemctlRestart("xray")

			printJSON(map[string]interface{}{
				"ok":      true,
				"status":  "success",
				"added":   len(payload.Add),
				"removed": len(payload.Remove),
			})
		},
	}
	cmd.Flags().StringVar(&payloadStr, "payload", "", "JSON payload containing batch operations")
	return cmd
}
