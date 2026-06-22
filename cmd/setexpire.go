package cmd

import (
	"fmt"

	"xraytool/internal/convert"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

func setExpireCmd() *cobra.Command {
	var (
		email      string
		emailAlias string
		expireVal  string
	)

	cmd := &cobra.Command{
		Use:   "setexpire",
		Short: "Update a user's expiry date",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			if email == "" {
				email = emailAlias
			}
			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			if email == "" || expireVal == "" {
				p.Error("--email and --expire are required")
			}
			if !validEmail(email) {
				p.Error("invalid characters in email (allowed: a-z A-Z 0-9 @ . _ -; cannot start with -)")
			}
			if _, err := convert.ParseExpiryDate(expireVal); err != nil {
				p.Errorf("invalid expire date format (supports DD.MM.YYYY, RFC3339, etc): %v", err)
			}

			updatedActive := false
			if err := xrayconfig.Modify(cfg.Paths.XrayConfig, func(c xrayconfig.RawConfig) error {
				exists, err := xrayconfig.UserExists(c, email)
				if err != nil {
					return err
				}
				if !exists {
					return nil
				}
				updatedActive = true
				return xrayconfig.UpdateStringField(c, email, "expire", expireVal)
			}); err != nil {
				p.Errorf("updating expire: %v", err)
			}

			if !updatedActive {
				p.Errorf("user %q not found in xray config", email)
			}

			sqlSetExpire(email, expireVal)

			if cfg.IsMaster() {
				propagate(cfg, "setexpire", map[string]string{
					"email": email, "expire": expireVal,
				}, p)
			}

			if isBatch {
				fmt.Printf("SUCCESS|EXPIRE_SET|%s|%s\n", email, expireVal)
			} else {
				p.OK("Expiry for %s set to %s.", email, expireVal)
			}
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "User email/name")
	cmd.Flags().StringVar(&emailAlias, "name", "", "Alias for --email")
	cmd.Flags().StringVar(&expireVal, "expire", "", "New expiry date (DD-MM-YYYY)")
	return cmd
}

func updateLimitCmd() *cobra.Command {
	var (
		email      string
		emailAlias string
		limitStr   string
	)

	cmd := &cobra.Command{
		Use:     "setlimit",
		Aliases: []string{"updatelimit", "set-limit"},
		Short:   "Update a user's device connection limit",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			if email == "" {
				email = emailAlias
			}
			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			if email == "" || limitStr == "" {
				p.Error("--email and --limit are required")
			}
			if !validEmail(email) {
				p.Error("invalid characters in email (allowed: a-z A-Z 0-9 @ . _ -; cannot start with -)")
			}

			limitPtr, err := limitPtrFromStr(limitStr)
			if err != nil || limitPtr == nil {
				p.Errorf("invalid limit: %q", limitStr)
			}

			updatedActive := false
			// Update in active config.
			if err := xrayconfig.Modify(cfg.Paths.XrayConfig, func(c xrayconfig.RawConfig) error {
				exists, _ := xrayconfig.UserExists(c, email)
				if !exists {
					return nil // no-op; will try limited DB
				}
				updatedActive = true
				return xrayconfig.UpdateNumberField(c, email, "limit", *limitPtr)
			}); err != nil {
				p.Errorf("updating active config: %v", err)
			}

			if !updatedActive {
				p.Errorf("user %q not found in xray config", email)
			}

			sqlSetLimit(email, int(*limitPtr))

			if cfg.IsMaster() {
				propagate(cfg, "setlimit", map[string]string{
					"email": email, "limit": limitStr,
				}, p)
			}

			if isBatch {
				fmt.Printf("SUCCESS|LIMIT_UPDATED|%s|%s\n", email, limitStr)
			} else {
				p.OK("Device limit for %s set to %s.", email, limitStr)
			}
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "User email/name")
	cmd.Flags().StringVar(&emailAlias, "name", "", "Alias for --email")
	cmd.Flags().StringVar(&limitStr, "limit", "", "New device limit (integer)")
	return cmd
}

func ExecSetExpire(payload map[string]interface{}) (string, error) {
	email, _ := payload["email"].(string)
	if email == "" {
		if name, ok := payload["name"].(string); ok {
			email = name
		}
	}
	expireVal, _ := payload["expire"].(string)

	if email == "" || expireVal == "" {
		return "", fmt.Errorf("email and expire are required")
	}
	if !validEmail(email) {
		return "", fmt.Errorf("invalid characters in email")
	}
	if _, err := convert.ParseExpiryDate(expireVal); err != nil {
		return "", fmt.Errorf("invalid expire date format: %v", err)
	}

	updatedActive := false
	if err := xrayconfig.Modify(cfg.Paths.XrayConfig, func(c xrayconfig.RawConfig) error {
		exists, err := xrayconfig.UserExists(c, email)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		updatedActive = true
		return xrayconfig.UpdateStringField(c, email, "expire", expireVal)
	}); err != nil {
		return "", fmt.Errorf("updating expire: %v", err)
	}

	if !updatedActive {
		return "", fmt.Errorf("user %q not found in xray config", email)
	}

	sqlSetExpire(email, expireVal)

	if cfg.IsMaster() {
		p := newPrinter(true)
		propagate(cfg, "setexpire", map[string]string{
			"email": email, "expire": expireVal,
		}, p)
	}

	return fmt.Sprintf("SUCCESS|EXPIRE_SET|%s|%s", email, expireVal), nil
}

func ExecUpdateLimit(payload map[string]interface{}) (string, error) {
	email, _ := payload["email"].(string)
	if email == "" {
		if name, ok := payload["name"].(string); ok {
			email = name
		}
	}

	var limitStr string
	if limitFloat, ok := payload["limit"].(float64); ok {
		limitStr = fmt.Sprintf("%.0f", limitFloat)
	} else if limitS, ok := payload["limit"].(string); ok {
		limitStr = limitS
	}

	if email == "" || limitStr == "" {
		return "", fmt.Errorf("email and limit are required")
	}
	if !validEmail(email) {
		return "", fmt.Errorf("invalid characters in email")
	}

	limitPtr, err := limitPtrFromStr(limitStr)
	if err != nil || limitPtr == nil {
		return "", fmt.Errorf("invalid limit: %q", limitStr)
	}

	updatedActive := false
	if err := xrayconfig.Modify(cfg.Paths.XrayConfig, func(c xrayconfig.RawConfig) error {
		exists, _ := xrayconfig.UserExists(c, email)
		if !exists {
			return nil
		}
		updatedActive = true
		return xrayconfig.UpdateNumberField(c, email, "limit", *limitPtr)
	}); err != nil {
		return "", fmt.Errorf("updating active config: %v", err)
	}

	if !updatedActive {
		return "", fmt.Errorf("user %q not found in xray config", email)
	}

	sqlSetLimit(email, int(*limitPtr))

	if cfg.IsMaster() {
		p := newPrinter(true)
		propagate(cfg, "setlimit", map[string]string{
			"email": email, "limit": limitStr,
		}, p)
	}

	return fmt.Sprintf("SUCCESS|LIMIT_UPDATED|%s|%s", email, limitStr), nil
}
