package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"xraytool/internal/generate"
	"xraytool/internal/templates"
	"xraytool/internal/userdb"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"
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

			db := userdb.New(cfg.Paths.LimitedDB)

			// Interactive: show blocked users.
			if email == "" {
				all, err := db.All()
				if err != nil || len(all) == 0 {
					p.Error("no blocked users found")
				}
				fmt.Println("\033[0;36m--- Blocked Users ---\033[0m")
				for i, e := range all {
					fmt.Printf("\033[0;31m%d.\033[0m %s\n", i+1, e.Email)
				}
				fmt.Println()
				var choice string
				fmt.Print("Number to unblock: ")
				fmt.Scanln(&choice)
				var idx int
				if _, err := fmt.Sscanf(choice, "%d", &idx); err != nil || idx < 1 || idx > len(all) {
					p.Error("invalid selection")
				}
				email = all[idx-1].Email
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

			// Load from limited DB.
			dbEntry, _ := db.Get(email)

			if !isActive && dbEntry == nil {
				p.Errorf("user %q not found in active or blocked lists", email)
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

			if uuid == "" && dbEntry != nil {
				// Nothing in active config; try limited DB.
				if dbEntry.Limit != nil && limitPtr == nil {
					limitPtr = dbEntry.Limit
				}
				if subfile == "" {
					subfile = dbEntry.Subfile
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

			// Validate templates.
			if err := templates.Validate(cfg.Paths.TemplatesDir, xrayCfg); err != nil {
				p.Errorf("template validation: %v", err)
			}

			params := templates.ClientParams{
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
					xrayapi.New(cfg.Xray.APIAddr).RemoveUser(email, tags) //nolint:errcheck
				}
				xrayconfig.RemoveUserFromAllInbounds(xrayCfg, email) //nolint:errcheck
			}

			payload, err := templates.BuildForAllInbounds(cfg.Paths.TemplatesDir, xrayCfg, params)
			if err != nil {
				p.Errorf("building payload: %v", err)
			}
			if err := xrayconfig.AddUserToInbounds(xrayCfg, payload); err != nil {
				p.Errorf("adding user to config: %v", err)
			}

			if !legacy {
				if err := xrayapi.New(cfg.Xray.APIAddr).AddUser(payload, cfg.Paths.XrayConfig); err != nil {
					p.Errorf("xray API hot-add failed: %v\n\nUse --legacy to restart xray instead.", err)
				}
			}

			if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
				p.Errorf("writing xray config: %v", err)
			}

			// Remove from limited DB.
			db.Remove(email) //nolint:errcheck

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

	db := userdb.New(cfg.Paths.LimitedDB)

	xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
	if err != nil {
		return "", fmt.Errorf("reading xray config: %v", err)
	}

	isActive, _ := xrayconfig.UserExists(xrayCfg, email)
	dbEntry, _ := db.Get(email)

	if !isActive && dbEntry == nil {
		return "", fmt.Errorf("user %q not found in active or blocked lists", email)
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

	if uuid == "" && dbEntry != nil {
		if dbEntry.Limit != nil && limitPtr == nil {
			limitPtr = dbEntry.Limit
		}
		if subfile == "" {
			subfile = dbEntry.Subfile
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

	if err := templates.Validate(cfg.Paths.TemplatesDir, xrayCfg); err != nil {
		return "", fmt.Errorf("template validation: %v", err)
	}

	params := templates.ClientParams{
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
			xrayapi.New(cfg.Xray.APIAddr).RemoveUser(email, tags) //nolint:errcheck
		}
		xrayconfig.RemoveUserFromAllInbounds(xrayCfg, email) //nolint:errcheck
	}

	clientsPayload, err := templates.BuildForAllInbounds(cfg.Paths.TemplatesDir, xrayCfg, params)
	if err != nil {
		return "", fmt.Errorf("building payload: %v", err)
	}
	if err := xrayconfig.AddUserToInbounds(xrayCfg, clientsPayload); err != nil {
		return "", fmt.Errorf("adding user to config: %v", err)
	}

	if !legacy {
		if err := xrayapi.New(cfg.Xray.APIAddr).AddUser(clientsPayload, cfg.Paths.XrayConfig); err != nil {
			return "", fmt.Errorf("xray API hot-add failed: %v", err)
		}
	}

	if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
		return "", fmt.Errorf("writing xray config: %v", err)
	}

	db.Remove(email) //nolint:errcheck

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
