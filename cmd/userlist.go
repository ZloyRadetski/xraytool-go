package cmd

import (
	"fmt"
	"sort"

	"xraytool/internal/database"
	"xraytool/internal/user"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

func userListCmd(getUserSvc func() *user.Service) *cobra.Command {
	var batchMode bool

	cmd := &cobra.Command{
		Use:   "userlist",
		Short: "List active and blocked users",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil { return err }
			p := newPrinter(batchMode)

			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				return p.Errorf("reading xray config: %v", err)
			}

			users, err := xrayconfig.ListUsers(xrayCfg)
			if err != nil {
				return p.Errorf("listing users: %v", err)
			}

			// Sort by email.
			sort.Slice(users, func(i, j int) bool {
				return users[i].Email() < users[j].Email()
			})

			db := database.DB()
			type BlockedUser struct {
				Email   string
				Subfile string
				Limit   *float64
			}
			var limited []BlockedUser
			if db != nil {
				var subs []database.Subscription
				db.Where("status = ?", "blocked").Find(&subs)
				for _, sub := range subs {
					subfile := ""
					if sub.Metadata != nil {
						if sf, ok := sub.Metadata["subfile"].(string); ok {
							subfile = sf
						}
					}
					lv := float64(sub.MaxDevices)
					limited = append(limited, BlockedUser{Email: sub.Email, Subfile: subfile, Limit: &lv})
				}
			}
			if batchMode {
				// Machine-readable output.
				for _, u := range users {
					fmt.Printf("ACTIVE|%s|%s|%s\n",
						u.Email(),
						u.GetString("subfile"),
						u.GetString("expire"),
					)
				}
				for _, e := range limited {
					fmt.Printf("BLOCKED|%s|%s\n", e.Email, e.Subfile)
				}
				return nil
			}

			// Interactive output with colors.
			cyan := "\033[0;36m"
			yellow := "\033[1;33m"
			red := "\033[0;31m"
			nc := "\033[0m"

			fmt.Printf("\n%s=== Active (%d) ===%s\n", cyan, len(users), nc)
			for i, u := range users {
				expire := u.GetString("expire")
				if expire == "" {
					expire = "n/a"
				}
				sub := u.GetString("subfile")
				if sub == "" {
					sub = "?"
				}
				fmt.Printf("%s%d.%s %s [%s] | Expire: %s\n",
					yellow, i+1, nc,
					u.Email(), sub, expire,
				)
			}

			if len(limited) > 0 {
				fmt.Printf("\n%s=== Blocked (%d) ===%s\n", red, len(limited), nc)
				for i, e := range limited {
					limitStr := ""
					if e.Limit != nil {
						limitStr = fmt.Sprintf(" | Limit: %.0f", *e.Limit)
					}
					fmt.Printf("%s%d.%s %s [%s]%s\n",
						red, i+1, nc,
						e.Email, e.Subfile, limitStr,
					)
				}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&batchMode, "batch", false, "Machine-readable output")
	return cmd
}

func shareLinkCmd(getUserSvc func() *user.Service) *cobra.Command {
	var (
		email      string
		emailAlias string
	)

	cmd := &cobra.Command{
		Use:   "sharelink",
		Short: "Get the subscription link for a user",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil { return err }

			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			email, err := resolveEmail(email, emailAlias, true, "", p)
			if err != nil { return err }

			// Try active users first.
			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				return p.Errorf("reading xray config: %v", err)
			}

			client, _ := xrayconfig.FindUser(xrayCfg, email)
			if client != nil {
				sub := client.GetString("subfile")
				if sub != "" {
					svc := getUserSvc()
					link := svc.GenerateShareLink("", subfileID(sub))
					if isBatch {
						fmt.Printf("SUCCESS|LINK|%s\n", link)
					} else {
						p.OK("Link found:")
						fmt.Printf("\033[1m%s\033[0m\n", link)
					}
					return nil
				}
			}

			// Try SQL DB for legacy behavior fallback
			db := database.DB()
			if db != nil {
				var sub database.Subscription
				if err := db.Where("email = ?", email).First(&sub).Error; err == nil && sub.Metadata != nil {
					if sf, ok := sub.Metadata["subfile"].(string); ok && sf != "" {
						svc := getUserSvc()
						link := svc.GenerateShareLink("", subfileID(sf))
						status := sub.Status
						if isBatch {
							fmt.Printf("SUCCESS|LINK|%s|(%s)\n", link, status)
						} else {
							if status == "blocked" {
								p.Warn("User is blocked.")
							}
							fmt.Printf("Link: %s\n", link)
						}
						return nil
					}
				}
			}

			return p.Error("user not found")
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "User email/name")
	cmd.Flags().StringVar(&emailAlias, "name", "", "Alias for --email")
	return cmd
}
