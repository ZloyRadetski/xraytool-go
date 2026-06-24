package cmd

import (
	"fmt"

	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

// rmUserCmd and limitCmd share the same core logic; the difference is whether
// the removed user is saved to limited_users.db.

func rmUserCmd() *cobra.Command {
	return rmOrLimitCmd("rm")
}

func limitCmd() *cobra.Command {
	return rmOrLimitCmd("limit")
}

// rmOrLimitCmd builds either the "rmuser" or "limit" cobra command.
func rmOrLimitCmd(action string) *cobra.Command {
	var (
		email      string
		emailAlias string
		legacy     bool
	)

	use, short := "rmuser", "Remove a user permanently"
	if action == "limit" {
		use, short = "limit", "Block a user (removes from xray, saves to limited DB)"
	}

	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			if email == "" {
				email = emailAlias
			}
			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			// Interactive: pick from list.
			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				p.Errorf("reading xray config: %v", err)
			}

			if email == "" {
				email = selectUserInteractive(xrayCfg, p)
			}
			if email == "" {
				p.Error("email is required")
			}
			if !validEmail(email) {
				p.Error("invalid characters in email (allowed: a-z A-Z 0-9 @ . _ -; cannot start with -)")
			}

			// Verify the user exists.
			client, err := xrayconfig.FindUser(xrayCfg, email)
			var subfile string

			if err != nil || client == nil {
				// No legacy DB check needed, just say user not found if not in xray config
				// UNLESS they are trying to remove a user that is only in SQL.
				// But limit.go operates on Xray config mostly.
				// Let's just proceed to update SQL status anyway.
				
				if action == "rm" {
					sqlSetStatus(email, "inactive")

					if cfg.IsMaster() {
						slaveParams := map[string]string{"email": email}
						if legacy {
							slaveParams["legacy"] = "true"
						}
						propagate(cfg, "rmuser", slaveParams, p)
					}

					if isBatch {
						fmt.Printf("SUCCESS|%sED|%s\n", action, email)
					} else {
						p.OK("User %s: %s completed.", email, action)
					}
					return
				} else {
					p.Error("user is already limited/blocked or not in xray config")
				}
			}

			// Collect the user's subfile & limit before removing.
			subfile = client.GetString("subfile")
			if subfile == "" {
				subfile = "unknown.txt"
			}

			// Hot-remove via xray API.
			if !legacy {
				tags, tagsErr := xrayconfig.InboundTagsForUser(xrayCfg, email)
				if tagsErr != nil {
					p.Errorf("getting inbound tags for %s: %v", email, tagsErr)
				}
				apiClient := xrayapi.NewGRPCClient(cfg.Xray.APIAddr)
				if err := apiClient.RemoveUser(email, tags); err != nil {
					p.Warn("xray API hot-remove failed: %v\n\nUse --legacy flag to restart xray instead.", err)
				}
			}

			// Remove from config then write atomically FIRST.
			if err := xrayconfig.RemoveUserFromAllInbounds(xrayCfg, email); err != nil {
				p.Errorf("removing from xray config: %v", err)
			}
			if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
				p.Errorf("writing xray config: %v", err)
			}

			// Save to limited DB if this is a "limit" (block) action.
			// (Removed legacy limited_users.db code. The SQL DB is updated below.)

			if legacy {
				systemctlRestart("xray")
			}

			if action == "limit" {
				sqlSetStatus(email, "blocked")
			} else {
				sqlSetStatus(email, "inactive") // or whatever makes sense for rmuser
			}

			// Propagate.
			if cfg.IsMaster() {
				slaveCmd := action // "limit" or "rmuser"
				if action == "rm" {
					slaveCmd = "rmuser"
				}
				slaveParams := map[string]string{"email": email}
				if legacy {
					slaveParams["legacy"] = "true"
				}
				propagate(cfg, slaveCmd, slaveParams, p)
			}

			if isBatch {
				fmt.Printf("SUCCESS|%sED|%s\n", action, email)
			} else {
				p.OK("User %s: %s completed.", email, action)
			}
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "User email/name")
	cmd.Flags().StringVar(&emailAlias, "name", "", "Alias for --email")
	cmd.Flags().BoolVar(&legacy, "legacy", false, "Legacy mode: edit config + restart xray")
	return cmd
}

// selectUserInteractive presents an interactive numbered list for the user to pick from.
func selectUserInteractive(xrayCfg xrayconfig.RawConfig, p *Printer) string {
	users, err := xrayconfig.ListUsers(xrayCfg)
	if err != nil || len(users) == 0 {
		p.Error("no users found in xray config")
	}

	fmt.Println("\033[0;36m--- Select user ---\033[0m")
	for i, u := range users {
		fmt.Printf("\033[1;33m%d.\033[0m %s\n", i+1, u.Email())
	}
	fmt.Println()

	var choice string
	fmt.Print("Number or email: ")
	fmt.Scanln(&choice)

	var idx int
	if _, err := fmt.Sscanf(choice, "%d", &idx); err == nil {
		if idx >= 1 && idx <= len(users) {
			return users[idx-1].Email()
		}
	}
	return choice // manual email entry
}
