package cmd

import (
	"fmt"

	"xraytool/internal/user"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

// rmUserCmd and limitCmd share the same core logic; the difference is whether
// the removed user is saved to limited_users.db.

func rmUserCmd(getUserSvc func() *user.Service) *cobra.Command {
	return rmOrLimitCmd("rm", getUserSvc)
}

func limitCmd(getUserSvc func() *user.Service) *cobra.Command {
	return rmOrLimitCmd("limit", getUserSvc)
}

// rmOrLimitCmd builds either the "rmuser" or "limit" cobra command.
func rmOrLimitCmd(action string, getUserSvc func() *user.Service) *cobra.Command {
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
		RunE: func(cmd *cobra.Command, _ []string) error {
			requireRoot()

			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			email, err := resolveEmail(email, emailAlias, true, "", p)
			if err != nil {
				return err
			}

			req := user.ModifyUserRequest{
				Email:  email,
				Action: action,
				Legacy: legacy,
			}

			svc := getUserSvc()
			if err := svc.BlockOrRemoveUser(req); err != nil {
				return p.Errorf("error: %v", err)
			}

			if isBatch {
				fmt.Printf("SUCCESS|%sED|%s\n", action, email)
			} else {
				p.OK("User %s: %s completed.", email, action)
			}
			return nil
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
