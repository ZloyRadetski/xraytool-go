package cmd

import (
	"fmt"

	"xraytool/internal/userdb"
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
			var limitPtr *float64

			if err != nil || client == nil {
				db := userdb.New(cfg.Paths.LimitedDB)
				entry, err2 := db.Get(email)
				if err2 != nil || entry == nil {
					p.Error("user not found")
				}
				if action == "rm" {
					if err := db.Remove(email); err != nil {
						p.Errorf("removing from limited db: %v", err)
					}
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
					p.Error("user is already limited")
				}
			}

			// Collect the user's subfile & limit before removing.
			subfile = client.GetString("subfile")
			if subfile == "" {
				subfile = "unknown.txt"
			}
			if lv, ok := client.GetNumber("limit"); ok {
				limitPtr = &lv
			}

			// Hot-remove via xray API.
			if !legacy {
				tags, _ := xrayconfig.InboundTagsForUser(xrayCfg, email)
				apiClient := xrayapi.New(cfg.Xray.APIAddr)
				if err := apiClient.RemoveUser(email, tags); err != nil {
					p.Errorf("xray API hot-remove failed: %v\n\nUse --legacy flag to restart xray instead.", err)
				}
			}

			// Remove from config.
			if err := xrayconfig.RemoveUserFromAllInbounds(xrayCfg, email); err != nil {
				p.Errorf("removing from xray config: %v", err)
			}
			if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
				p.Errorf("writing xray config: %v", err)
			}

			// Save to limited DB if this is a "limit" (block) action.
			if action == "limit" {
				db := userdb.New(cfg.Paths.LimitedDB)
				entry := userdb.Entry{
					Email:   email,
					Subfile: subfile,
					Limit:   limitPtr,
				}
				if err := db.Upsert(entry); err != nil {
					p.Errorf("saving to limited db: %v", err)
				}
			}

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
