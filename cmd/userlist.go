package cmd

import (
	"fmt"
	"sort"

	"context"

	"github.com/spf13/cobra"
)

func userListCmd(deps *AppDeps) *cobra.Command {
	var batchMode bool

	cmd := &cobra.Command{
		Use:   "userlist",
		Short: "List active and blocked users",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			p := newPrinter(batchMode)

			engine := deps.Engine
			users, err := engine.ListUsers(context.Background())
			if err != nil {
				return p.Errorf("listing users: %v", err)
			}

			// Sort by email.
			sort.Slice(users, func(i, j int) bool {
				return users[i].Email < users[j].Email
			})

			type BlockedUser struct {
				Email   string
				Subfile string
				Limit   *float64
			}
			var limited []BlockedUser
			if deps.UserSvc != nil {
				if subs, err := deps.UserSvc.GetBlockedSubscriptions(context.Background()); err == nil {
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
			}
			if batchMode {
				// Machine-readable output.
				for _, u := range users {
					fmt.Printf("ACTIVE|%s|%s|%s\n",
						u.Email,
						u.Subfile,
						u.Expire,
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
				expire := u.Expire
				if expire == "" {
					expire = "n/a"
				}
				sub := u.Subfile
				if sub == "" {
					sub = "?"
				}
				fmt.Printf("%s%d.%s %s [%s] | Expire: %s\n",
					yellow, i+1, nc,
					u.Email, sub, expire,
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

func shareLinkCmd(deps *AppDeps) *cobra.Command {
	var (
		email      string
		emailAlias string
	)

	cmd := &cobra.Command{
		Use:   "sharelink",
		Short: "Get the subscription link for a user",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}

			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			email, err := resolveEmail(email, emailAlias, true, "", deps.Engine, p)
			if err != nil {
				return err
			}

			// Try active users first.
			engine := deps.Engine
			users, _ := engine.ListUsers(context.Background())
			var targetSubfile string
			for _, u := range users {
				if u.Email == email {
					targetSubfile = u.Subfile
					break
				}
			}

			if targetSubfile != "" {
				svc := deps.UserSvc
				link := svc.GenerateShareLink("", subfileID(targetSubfile))
				if isBatch {
					fmt.Printf("SUCCESS|LINK|%s\n", link)
				} else {
					p.OK("Link found:")
					fmt.Printf("\033[1m%s\033[0m\n", link)
				}
				return nil
			}

			// Try SQL DB for legacy behavior fallback
			if deps.UserSvc != nil {
				if sub, err := deps.UserSvc.GetSubscriptionByEmail(context.Background(), email); err == nil && sub.Metadata != nil {
					if sf, ok := sub.Metadata["subfile"].(string); ok && sf != "" {
						svc := deps.UserSvc
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
