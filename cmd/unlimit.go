package cmd

import (
	"fmt"
	"os"

	"xraytool/internal/generate"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

const minSubfileLen = 5 // Minimum valid subfile identifier length

func unlimitCmd() *cobra.Command {
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
		Short: "Unblock a previously blocked user",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			if email == "" {
				email = emailAlias
			}
			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			// Interactive: just prompt for email.
			if email == "" {
				fmt.Print("Enter name (email) to unblock: ")
				fmt.Scanln(&email)
			}
			if email == "" {
				p.Error("email is required")
			}
			if !validEmail(email) {
				p.Error("invalid characters in email (allowed: a-z A-Z 0-9 @ . _ -; cannot start with -)")
			}

			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				p.Errorf("reading xray config: %v", err)
			}

			// Check if already active.
			isActive, _ := xrayconfig.UserExists(xrayCfg, email)

			if !isActive {
				// The user is not in active xray config.
				// We can still try to recreate them if enough parameters are provided,
				// or if they exist in SQL DB we could pull from there, but for now we
				// just warn if they don't have enough args.
			}

			// --- Determine values to restore ---
			// Priority: forced flags > existing active > DB record > generate new.
			uuid := forcedUUID
			subfile := forcedSub
			expireVal := forcedExpire
			auth := forcedAuth
			limitPtr, err := limitPtrFromStr(limitStr)
			if err != nil {
				p.Errorf("%v", err)
			}

			if uuid == "" && isActive {
				// Re-use the current UUID from the active config.
				if c, _ := xrayconfig.FindUser(xrayCfg, email); c != nil {
					uuid = c.GetString("id")
					if subfile == "" {
						subfile = c.GetString("subfile")
					}
					if limitPtr == nil {
						if lv, ok := c.GetNumber("limit"); ok {
							limitPtr = &lv
						}
					}
					if auth == "" {
						auth = c.GetString("auth")
					}
				}
			}


			// Generate anything still missing.
			if uuid == "" {
				if uuid, err = generate.UUID(); err != nil {
					p.Errorf("generating UUID: %v", err)
				}
			}
			if subfile == "" || len(subfile) < minSubfileLen {
				subfile = generate.Subfile()
			}
			if expireVal == "" {
				expireVal = defaultExpireDate()
			}
			if auth == "" {
				auth = generate.Secret(32)
			}


			params := xrayconfig.ClientParams{
				Email:   email,
				UUID:    uuid,
				Auth:    auth,
				Subfile: subfile,
				Expire:  expireVal,
				Limit:   limitPtr,
			}

			// If user is already active, remove first (UUID might differ).
			if isActive {
				if !legacy {
					tags, _ := xrayconfig.InboundTagsForUser(xrayCfg, email)
					if err := xrayapi.NewGRPCClient(cfg.Xray.APIAddr).RemoveUser(email, tags); err != nil {
						fmt.Fprintf(os.Stderr, "[WARN] unlimit: hot-remove failed for %s: %v\n", email, err)
					}
				}
				if err := xrayconfig.RemoveUserFromAllInbounds(xrayCfg, email); err != nil {
					p.Errorf("removing old user config: %v", err)
				}
			}

			payload, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
			if err != nil {
				p.Errorf("building payload: %v", err)
			}
			if err := xrayconfig.AddUserToInbounds(xrayCfg, payload); err != nil {
				p.Errorf("adding user to config: %v", err)
			}

			if !legacy {
				apiClient := xrayapi.NewGRPCClient(cfg.Xray.APIAddr)
				if err := apiClient.AddUser(payload, cfg.Paths.XrayConfig); err != nil {
					p.Warnf("xray API hot-add failed: %v\n\nUse --legacy to restart xray instead.", err)
				}
			}

			if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
				p.Errorf("writing xray config: %v", err)
			}


			if legacy {
				systemctlRestart("xray")
			}

			sqlSetStatus(email, "active")

			// Propagate.
			if cfg.IsMaster() {
				sp := map[string]string{
					"email":   email,
					"uuid":    uuid,
					"subfile": subfile,
					"expire":  expireVal,
					"auth":    auth,
				}
				if limitPtr != nil {
					sp["limit"] = fmt.Sprintf("%.0f", *limitPtr)
				}
				if legacy {
					sp["legacy"] = "true"
				}
				propagate(cfg, "unlimit", sp, p)
			}

			if isBatch {
				fmt.Printf("SUCCESS|UNLIMITED|%s\n", email)
			} else {
				p.OK("User %s unblocked.", email)
			}
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

func ExecUnlimit(payload map[string]interface{}) (string, error) {
	email, _ := payload["email"].(string)
	if email == "" {
		if name, ok := payload["name"].(string); ok {
			email = name
		}
	}
	if email == "" {
		return "", fmt.Errorf("email is required")
	}
	if !validEmail(email) {
		return "", fmt.Errorf("invalid characters in email")
	}

	forcedUUID, _ := payload["uuid"].(string)
	forcedSub, _ := payload["subfile"].(string)
	forcedExpire, _ := payload["expire"].(string)
	forcedAuth, _ := payload["auth"].(string)

	var limitStr string
	if limitFloat, ok := payload["limit"].(float64); ok {
		limitStr = fmt.Sprintf("%.0f", limitFloat)
	} else if limitS, ok := payload["limit"].(string); ok {
		limitStr = limitS
	}

	legacy, _ := payload["legacy"].(bool)


	xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
	if err != nil {
		return "", fmt.Errorf("reading xray config: %v", err)
	}

	isActive, _ := xrayconfig.UserExists(xrayCfg, email)
	if !isActive {
		// Just proceed to re-add.
	}

	uuid := forcedUUID
	subfile := forcedSub
	expireVal := forcedExpire
	auth := forcedAuth
	limitPtr, err := limitPtrFromStr(limitStr)
	if err != nil {
		return "", err
	}

	if uuid == "" && isActive {
		if c, _ := xrayconfig.FindUser(xrayCfg, email); c != nil {
			uuid = c.GetString("id")
			if subfile == "" {
				subfile = c.GetString("subfile")
			}
			if limitPtr == nil {
				if lv, ok := c.GetNumber("limit"); ok {
					limitPtr = &lv
				}
			}
			if auth == "" {
				auth = c.GetString("auth")
			}
		}
	}


	if uuid == "" {
		if uuid, err = generate.UUID(); err != nil {
			return "", fmt.Errorf("generating UUID: %v", err)
		}
	}
	if subfile == "" || len(subfile) < minSubfileLen {
		subfile = generate.Subfile()
	}
	if expireVal == "" {
		expireVal = defaultExpireDate()
	}
	if auth == "" {
		auth = generate.Secret(32)
	}



	params := xrayconfig.ClientParams{
		Email:   email,
		UUID:    uuid,
		Auth:    auth,
		Subfile: subfile,
		Expire:  expireVal,
		Limit:   limitPtr,
	}

	if isActive {
		if !legacy {
			tags, _ := xrayconfig.InboundTagsForUser(xrayCfg, email)
			if err := xrayapi.NewGRPCClient(cfg.Xray.APIAddr).RemoveUser(email, tags); err != nil {
				// Non-fatal: log and continue. A failed hot-remove before hot-add
				// may cause a duplicate in xray, but config will be written correctly.
				fmt.Fprintf(os.Stderr, "[WARN] unlimit: hot-remove failed for %s: %v\n", email, err)
			}
		}
		if err := xrayconfig.RemoveUserFromAllInbounds(xrayCfg, email); err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] unlimit db repair: failed to remove %s from config: %v\n", email, err)
		}
	}

	clientsPayload, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
	if err != nil {
		return "", fmt.Errorf("building payload: %v", err)
	}
	if err := xrayconfig.AddUserToInbounds(xrayCfg, clientsPayload); err != nil {
		return "", fmt.Errorf("adding user to config: %v", err)
	}

	if !legacy {
		if err := xrayapi.NewGRPCClient(cfg.Xray.APIAddr).AddUser(clientsPayload, cfg.Paths.XrayConfig); err != nil {
			return "", fmt.Errorf("xray API hot-add failed: %v", err)
		}
	}

	if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
		return "", fmt.Errorf("writing xray config: %v", err)
	}


	if legacy {
		systemctlRestart("xray")
	}

	sqlSetStatus(email, "active")

	if cfg.IsMaster() {
		sp := map[string]string{
			"email":   email,
			"uuid":    uuid,
			"subfile": subfile,
			"expire":  expireVal,
			"auth":    auth,
		}
		if limitPtr != nil {
			sp["limit"] = fmt.Sprintf("%.0f", *limitPtr)
		}
		if legacy {
			sp["legacy"] = "true"
		}
		p := newPrinter(true)
		propagate(cfg, "unlimit", sp, p)
	}

	return fmt.Sprintf("SUCCESS|UNLIMITED|%s", email), nil
}
