package cmd

import (
	"fmt"
	"sort"

	"xraytool/internal/userdb"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

func userListCmd() *cobra.Command {
	var batchMode bool

	cmd := &cobra.Command{
		Use:   "userlist",
		Short: "List active and blocked users",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()
			p := newPrinter(batchMode)

			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				p.Errorf("reading xray config: %v", err)
			}

			users, err := xrayconfig.ListUsers(xrayCfg)
			if err != nil {
				p.Errorf("listing users: %v", err)
			}

			// Sort by email.
			sort.Slice(users, func(i, j int) bool {
				return users[i].Email() < users[j].Email()
			})

			db := userdb.New(cfg.Paths.LimitedDB)
			limited, _ := db.All()

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
				return
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
		},
	}

	cmd.Flags().BoolVar(&batchMode, "batch", false, "Machine-readable output")
	return cmd
}

func shareLinkCmd() *cobra.Command {
	var (
		email      string
		emailAlias string
	)

	cmd := &cobra.Command{
		Use:   "sharelink",
		Short: "Get the subscription link for a user",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			if email == "" {
				email = emailAlias
			}
			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			if email != "" && !validEmail(email) {
				p.Error("invalid characters in email (allowed: a-z A-Z 0-9 @ . _ -; cannot start with -)")
			}

			// Interactive: pick user.
			if email == "" {
				xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
				if err != nil {
					p.Errorf("reading xray config: %v", err)
				}
				email = selectUserInteractive(xrayCfg, p)
			}

			// Try active users first.
			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				p.Errorf("reading xray config: %v", err)
			}

			client, _ := xrayconfig.FindUser(xrayCfg, email)
			if client != nil {
				sub := client.GetString("subfile")
				if sub != "" {
					link := fmt.Sprintf("https://%s/client?id=%s", cfg.Server.Domain, subfileID(sub))
					if isBatch {
						fmt.Printf("SUCCESS|LINK|%s\n", link)
					} else {
						p.OK("Link found:")
						fmt.Printf("\033[1m%s\033[0m\n", link)
					}
					return
				}
			}

			// Try limited DB.
			db := userdb.New(cfg.Paths.LimitedDB)
			entry, _ := db.Get(email)
			if entry != nil {
				link := fmt.Sprintf("https://%s/client?id=%s", cfg.Server.Domain, subfileID(entry.Subfile))
				if isBatch {
					fmt.Printf("SUCCESS|LINK|%s|(LIMITED)\n", link)
				} else {
					p.Warn("User is blocked.")
					fmt.Printf("Link: %s\n", link)
				}
				return
			}

			p.Error("user not found")
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "User email/name")
	cmd.Flags().StringVar(&emailAlias, "name", "", "Alias for --email")
	return cmd
}
