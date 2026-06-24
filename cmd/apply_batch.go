package cmd

import (
	"encoding/json"
	"fmt"
	"sync"

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
			if len(payload.Remove) > 0 {
				_ = xrayconfig.RemoveUsersFromAllInbounds(xrayCfg, payload.Remove)
			}

			// Apply Adds
			var addEmails []string
			for _, u := range payload.Add {
				addEmails = append(addEmails, u.Email)
			}
			if len(addEmails) > 0 {
				_ = xrayconfig.RemoveUsersFromAllInbounds(xrayCfg, addEmails)
			}

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
			apiClient := xrayapi.NewGRPCClient(cfg.Xray.APIAddr)
			
			var wg sync.WaitGroup

			// 1. Hot-Remove
			tagsMap, _ := xrayconfig.InboundTagsForUsers(originalCfg, payload.Remove)
			for _, email := range payload.Remove {
				tags := tagsMap[email]
				wg.Add(1)
				go func(e string, t []string) {
					defer wg.Done()
					_ = apiClient.RemoveUser(e, t)
				}(email, tags)
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
					wg.Add(1)
					go func(tg []xrayconfig.TaggedClient) {
						defer wg.Done()
						_ = apiClient.AddUser(tg, cfg.Paths.XrayConfig)
					}(tagged)
				}
			}

			wg.Wait()

			// (systemctl restart fallback removed to ensure pure hot-reload without dropping connections)

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
