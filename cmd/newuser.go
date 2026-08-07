package cmd

import (
	"context"
	"fmt"

	"xraytool/internal/plugins/core/user"

	"github.com/spf13/cobra"
)

func newUserCmd(deps *AppDeps) *cobra.Command {
	var (
		email        string
		emailAlias   string
		forcedUUID   string
		forcedSub    string
		forcedExpire string
		forcedAuth   string
		limitStr     string
		legacy       bool
	)

	cmd := &cobra.Command{
		Use:   "newuser",
		Short: "Create a new user",
		RunE: func(cmd *cobra.Command, _ []string) error {
			requireRoot() //nolint:errcheck

			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			email, err := resolveEmail(email, emailAlias, false, "Enter name (email): ", deps.Engine, p)
			if err != nil {
				return err
			}

			limitPtr, err := limitPtrFromStr(limitStr)
			if err != nil {
				return p.Errorf("%v", err)
			}

			req := user.CreateUserRequest{
				Email:   email,
				UUID:    forcedUUID,
				Subfile: forcedSub,
				Expire:  forcedExpire,
				Auth:    forcedAuth,
				Limit:   limitPtr,
				Legacy:  legacy,
			}

			svc := deps.UserSvc
			resp, err := svc.CreateUser(context.Background(), req)
			if err != nil {
				return p.Errorf("error creating user: %v", err)
			}
			p.OK("User created successfully!")
			p.Info("UUID: %s", resp.UUID)
			p.Info("Subfile: %s", resp.Subfile)

			// Service propagates internally via API if it's master.
			// No need to manually propagate here.

			if isBatch {
				fmt.Printf("SUCCESS|CREATED|%s\n", resp.Link)
			} else {
				p.OK("User %s created.", resp.Email)
				fmt.Printf("Link: \033[1m%s\033[0m\n", resp.Link)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "User email/name identifier")
	cmd.Flags().StringVar(&emailAlias, "name", "", "Alias for --email")
	cmd.Flags().StringVar(&forcedUUID, "uuid", "", "Force a specific UUID (used by slave sync)")
	cmd.Flags().StringVar(&forcedSub, "subfile", "", "Force a specific subscription filename")
	cmd.Flags().StringVar(&forcedExpire, "expire", "", "Expiry date in DD-MM-YYYY format (default: +30 days)")
	cmd.Flags().StringVar(&forcedAuth, "auth", "", "Force auth/password value")
	cmd.Flags().StringVar(&limitStr, "limit", "", "Device connection limit (integer)")
	cmd.Flags().BoolVar(&legacy, "legacy", false, "Use legacy mode: edit config + restart xray (no hot-add)")
	// Hide internal flags used by slave sync.
	cmd.Flags().MarkHidden("uuid")    //nolint:errcheck
	cmd.Flags().MarkHidden("subfile") //nolint:errcheck
	cmd.Flags().MarkHidden("auth")    //nolint:errcheck

	return cmd
}
