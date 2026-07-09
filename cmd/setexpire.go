package cmd

import (
	"context"
	"fmt"

	"xraytool/internal/user"

	"github.com/spf13/cobra"
)

func setExpireCmd(deps *AppDeps) *cobra.Command {
	var (
		email      string
		emailAlias string
		expireVal  string
	)

	cmd := &cobra.Command{
		Use:   "setexpire",
		Short: "Update a user's expiry date",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			if email == "" {
				email = emailAlias
			}
			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			email, err := resolveEmail(email, emailAlias, true, "", deps.Engine, p)
			if err != nil {
				return err
			}

			if expireVal == "" {
				fmt.Print("Enter new expiry date (DD-MM-YYYY) or offset (+30): ")
				fmt.Scanln(&expireVal) //nolint:errcheck
			}
			if expireVal == "" {
				return p.Error("expire date is required")
			}

			req := user.SetExpireRequest{
				Email:  email,
				Expire: expireVal,
			}

			svc := deps.UserSvc
			if err := svc.SetExpire(context.Background(), req); err != nil {
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

func updateLimitCmd(deps *AppDeps) *cobra.Command {
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
			requireRoot() //nolint:errcheck
			if email == "" {
				email = emailAlias
			}
			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			email, err := resolveEmail(email, emailAlias, true, "", deps.Engine, p)
			if err != nil {
				return err
			}

			limitPtr, err := limitPtrFromStr(limitStr)
			if err != nil || limitPtr == nil {
				return p.Errorf("invalid limit: %v", err)
			}

			req := user.UpdateLimitRequest{
				Email: email,
				Limit: limitPtr,
			}

			svc := deps.UserSvc
			if err := svc.UpdateLimit(context.Background(), req); err != nil {
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
