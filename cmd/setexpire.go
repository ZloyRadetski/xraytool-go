package cmd

import (
	"fmt"

	"xraytool/internal/xrayconfig"
	"xraytool/internal/user"

	"github.com/spf13/cobra"
)

func setExpireCmd(getUserSvc func() *user.Service) *cobra.Command {
	var (
		email      string
		emailAlias string
		expireVal  string
	)

	cmd := &cobra.Command{
		Use:   "setexpire",
		Short: "Update a user's expiry date",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil { return err }
			if email == "" {
				email = emailAlias
			}
			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			email, err := resolveEmail(email, emailAlias, true, "", p)
			if err != nil { return err }

			if expireVal == "" {
				fmt.Print("Enter new expiry date (DD-MM-YYYY) or offset (+30): ")
				fmt.Scanln(&expireVal)
			}
			if expireVal == "" {
				return p.Error("expire date is required")
			}

			req := user.SetExpireRequest{
				Email:  email,
				Expire: expireVal,
			}

			svc := getUserSvc()
			if err := svc.SetExpire(req); err != nil {
				return p.Errorf("error: %v", err)
			}

			if isBatch {
				fmt.Printf("SUCCESS|EXPIRE_SET|%s|%s\n", email, expireVal)
			} else {
				p.OK("User %s expire updated to %s", email, expireVal)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "User email/name")
	cmd.Flags().StringVar(&emailAlias, "name", "", "Alias for --email")
	cmd.Flags().StringVar(&expireVal, "expire", "", "New expiry date (DD-MM-YYYY)")
	return cmd
}

func updateLimitCmd(getUserSvc func() *user.Service) *cobra.Command {
	var (
		email      string
		emailAlias string
		limitStr   string
	)

	cmd := &cobra.Command{
		Use:     "setlimit",
		Aliases: []string{"updatelimit", "set-limit"},
		Short:   "Update a user's device connection limit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			requireRoot()
			if email == "" {
				email = emailAlias
			}
			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			if email == "" {
				xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
				if err != nil {
					p.Errorf("reading xray config: %v", err)
				}
				email = selectUserInteractive(xrayCfg, p)
			}
			if email == "" {
				p.Error("email is required")
			}
			if !validEmail(email) {
				p.Error("invalid characters in email (allowed: a-z A-Z 0-9 @ . _ -)")
			}

			limitPtr, err := limitPtrFromStr(limitStr)
			if err != nil || limitPtr == nil {
				return p.Errorf("invalid limit: %v", err)
			}

			req := user.UpdateLimitRequest{
				Email: email,
				Limit: limitPtr,
			}

			svc := getUserSvc()
			if err := svc.UpdateLimit(req); err != nil {
				return p.Errorf("error: %v", err)
			}

			if isBatch {
				fmt.Printf("SUCCESS|LIMIT_UPDATED|%s|%.0f\n", email, *limitPtr)
			} else {
				p.OK("User %s limit updated to %.0f", email, *limitPtr)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "User email/name")
	cmd.Flags().StringVar(&emailAlias, "name", "", "Alias for --email")
	cmd.Flags().StringVar(&limitStr, "limit", "", "New device limit (integer)")
	return cmd
}


