package cmd

import (
	"context"
	"fmt"

	"xraytool/internal/user"

	"github.com/spf13/cobra"
)

//nolint:unused
const minSubfileLen = 5 // Minimum valid subfile identifier length

func unlimitCmd(deps *AppDeps) *cobra.Command {
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
		Use:   "unlimit",
		Short: "Unblock a user (removes from limited DB, adds back to xray)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}

			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			email, err := resolveEmail(email, emailAlias, false, "Enter name (email) to unblock: ", deps.Engine, p)
			if err != nil {
				return err
			}

			limitPtr, err := limitPtrFromStr(limitStr)
			if err != nil {
				return p.Errorf("%v", err)
			}

			req := user.UnlimitUserRequest{
				Email:   email,
				UUID:    forcedUUID,
				Subfile: forcedSub,
				Expire:  forcedExpire,
				Auth:    forcedAuth,
				Limit:   limitPtr,
				Legacy:  legacy,
			}

			svc := deps.UserSvc
			resp, err := svc.UnlimitUser(context.Background(), req)
			if err != nil {
				return p.Errorf("error unlimiting user: %v", err)
			}

			if isBatch {
				fmt.Printf("SUCCESS|UNLIMITED|%s\n", resp.Email)
			} else {
				p.OK("User %s unblocked.", resp.Email)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "User email/name")
	cmd.Flags().StringVar(&emailAlias, "name", "", "Alias for --email")
	cmd.Flags().StringVar(&forcedUUID, "uuid", "", "Force UUID (slave sync)")
	cmd.Flags().StringVar(&forcedSub, "subfile", "", "Force subfile (slave sync)")
	cmd.Flags().StringVar(&forcedExpire, "expire", "", "Expiry date DD-MM-YYYY")
	cmd.Flags().StringVar(&forcedAuth, "auth", "", "Force auth (slave sync)")
	cmd.Flags().StringVar(&limitStr, "limit", "", "Device limit")
	cmd.Flags().BoolVar(&legacy, "legacy", false, "Legacy mode: edit config + restart")
	cmd.Flags().MarkHidden("uuid")    //nolint:errcheck
	cmd.Flags().MarkHidden("subfile") //nolint:errcheck
	cmd.Flags().MarkHidden("auth")    //nolint:errcheck
	return cmd
}
