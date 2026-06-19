package cmd

import (
	"encoding/json"
	"fmt"

	"xraytool/internal/userdb"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

// BatchPayload represents the requested operations for apply-batch
type BatchPayload struct {
	Add    []SnapshotUser    `json:"add"`
	Remove []string          `json:"remove"`
	Limit  []SnapshotLimited `json:"limit"`
}

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

			var payload BatchPayload
			if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
				printJSON(map[string]interface{}{"ok": false, "error": fmt.Sprintf("invalid json payload: %v", err)})
				return
			}

			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				printJSON(map[string]interface{}{"ok": false, "error": fmt.Sprintf("reading config: %v", err)})
				return
			}

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

			// Apply Limits (Database for slave node limits)
			db := userdb.New(cfg.Paths.LimitedDB)
			for _, l := range payload.Limit {
				if l.Limit != nil && *l.Limit == 0 {
					db.Remove(l.Email)
				} else {
					db.Upsert(userdb.Entry{
						Email:   l.Email,
						Subfile: l.Subfile,
						Limit:   l.Limit,
					})
				}
			}

			// Write config
			if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
				printJSON(map[string]interface{}{"ok": false, "error": fmt.Sprintf("writing config: %v", err)})
				return
			}

			// Restart Xray (one single restart for all operations)
			systemctlRestart("xray")

			printJSON(map[string]interface{}{
				"ok":      true,
				"status":  "success",
				"added":   len(payload.Add),
				"removed": len(payload.Remove),
				"limits":  len(payload.Limit),
			})
		},
	}
	cmd.Flags().StringVar(&payloadStr, "payload", "", "JSON payload containing batch operations")
	return cmd
}
