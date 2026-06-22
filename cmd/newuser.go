package cmd

import (
	"fmt"

	"xraytool/internal/generate"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

func newUserCmd() *cobra.Command {
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
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			// Reconcile --name / --email aliases.
			if email == "" {
				email = emailAlias
			}
			isBatch := cmd.Flags().Changed("email") || cmd.Flags().Changed("name")
			p := newPrinter(isBatch)

			// Interactive: prompt for email.
			if email == "" {
				fmt.Print("Enter name (email): ")
				fmt.Scanln(&email)
			}
			if email == "" {
				p.Error("email is required")
			}
			if !validEmail(email) {
				p.Error("invalid characters in email (allowed: a-z A-Z 0-9 @ . _ -)")
			}



			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				p.Errorf("reading xray config: %v", err)
			}
			exists, existsErr := xrayconfig.UserExists(xrayCfg, email)
			if existsErr != nil {
				p.Errorf("checking user existence: %v", existsErr)
			}
			if exists {
				p.Error("user already exists")
			}


			// --- Generate or use forced values ---
			uuid := forcedUUID
			if uuid == "" {
				if uuid, err = generate.UUID(); err != nil {
					p.Errorf("generating UUID: %v", err)
				}
			}
			subfile := forcedSub
			if subfile == "" {
				subfile = generate.Subfile()
			}
			expireVal := forcedExpire
			if expireVal == "" {
				expireVal = defaultExpireDate()
			}
			auth := forcedAuth
			if auth == "" {
				auth = generate.Secret(32)
			}
			limitPtr, err := limitPtrFromStr(limitStr)
			if err != nil {
				p.Errorf("%v", err)
			}

			params := xrayconfig.ClientParams{
				Email:   email,
				UUID:    uuid,
				Auth:    auth,
				Subfile: subfile,
				Expire:  expireVal,
				Limit:   limitPtr,
			}

			payload, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
			if err != nil {
				p.Errorf("building client payload: %v", err)
			}
			if len(payload) == 0 {
				p.Error("no client inbounds found in xray config — check that inbounds have settings.clients or settings.users arrays")
			}

			p.Info("Adding %s (expire: %s, limit: %s)…", email, expireVal, func() string {
				if limitPtr != nil {
					return fmt.Sprintf("%.0f", *limitPtr)
				}
				return "unset"
			}())

			// Apply to in-memory config.
			if err := xrayconfig.AddUserToInbounds(xrayCfg, payload); err != nil {
				p.Errorf("updating xray config: %v", err)
			}

			// Write config atomically FIRST — so disk state is consistent even
			// if the subsequent hot-add fails (xray will pick it up on restart).
			if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
				p.Errorf("writing xray config: %v", err)
			}

			// Hot-add via xray API.
			if !legacy {
				apiClient := xrayapi.New(cfg.Xray.APIAddr)
				if err := apiClient.AddUser(payload, cfg.Paths.XrayConfig); err != nil {
					p.Errorf("xray API hot-add failed: %v\n\nUse --legacy flag to restart xray instead.", err)
				}
			}

			if legacy {
				systemctlRestart("xray")
			}

			limitInt := 3
			if limitPtr != nil {
				limitInt = int(*limitPtr)
			}
			sqlCreateUserIfNotExist(email, uuid, expireVal, limitInt, subfile)

			// Propagate to slaves (master only).
			if cfg.IsMaster() {
				slaveParams := map[string]string{
					"email":   email,
					"uuid":    uuid,
					"subfile": subfile,
					"expire":  expireVal,
					"auth":    auth,
				}
				if limitPtr != nil {
					slaveParams["limit"] = fmt.Sprintf("%.0f", *limitPtr)
				}
				if legacy {
					slaveParams["legacy"] = "true"
				}
				if !isBatch {
					p.Info("Propagating to slave servers…")
				}
				propagate(cfg, "newuser", slaveParams, p)
			}

			id := subfileID(subfile)
			link := fmt.Sprintf("https://%s/client?id=%s", cfg.Server.Domain, id)

			if isBatch {
				fmt.Printf("SUCCESS|CREATED|%s\n", link)
			} else {
				p.OK("User %s created.", email)
				fmt.Printf("Link: \033[1m%s\033[0m\n", link)
			}
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

func ExecNewUser(payload map[string]interface{}) (string, error) {
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
		return "", fmt.Errorf("invalid characters in email (allowed: a-z A-Z 0-9 @ . _ -)")
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
	exists, existsErr := xrayconfig.UserExists(xrayCfg, email)
	if existsErr != nil {
		return "", fmt.Errorf("checking user existence: %v", existsErr)
	}
	if exists {
		return "", fmt.Errorf("user already exists")
	}



	uuid := forcedUUID
	if uuid == "" {
		if uuid, err = generate.UUID(); err != nil {
			return "", fmt.Errorf("generating UUID: %v", err)
		}
	}
	subfile := forcedSub
	if subfile == "" {
		subfile = generate.Subfile()
	}
	expireVal := forcedExpire
	if expireVal == "" {
		expireVal = defaultExpireDate()
	}
	auth := forcedAuth
	if auth == "" {
		auth = generate.Secret(32)
	}
	limitPtr, err := limitPtrFromStr(limitStr)
	if err != nil {
		return "", err
	}

	params := xrayconfig.ClientParams{
		Email:   email,
		UUID:    uuid,
		Auth:    auth,
		Subfile: subfile,
		Expire:  expireVal,
		Limit:   limitPtr,
	}

	clientsPayload, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
	if err != nil {
		return "", fmt.Errorf("building client payload: %v", err)
	}
	if len(clientsPayload) == 0 {
		return "", fmt.Errorf("no client inbounds found in xray config")
	}

	// Apply to in-memory config
	if err := xrayconfig.AddUserToInbounds(xrayCfg, clientsPayload); err != nil {
		return "", fmt.Errorf("updating xray config: %v", err)
	}

	// Write config atomically FIRST — disk state is consistent even if hot-add fails.
	if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
		return "", fmt.Errorf("writing xray config: %v", err)
	}

	// Hot-add via xray API
	if !legacy {
		apiClient := xrayapi.New(cfg.Xray.APIAddr)
		if err := apiClient.AddUser(clientsPayload, cfg.Paths.XrayConfig); err != nil {
			return "", fmt.Errorf("xray API hot-add failed: %v", err)
		}
	}
	if legacy {
		systemctlRestart("xray")
	}

	limitInt := 3
	if limitPtr != nil {
		limitInt = int(*limitPtr)
	}
	sqlCreateUserIfNotExist(email, uuid, expireVal, limitInt, subfile)

	// Propagate to slaves
	if cfg.IsMaster() {
		slaveParams := map[string]string{
			"email":   email,
			"uuid":    uuid,
			"subfile": subfile,
			"expire":  expireVal,
			"auth":    auth,
		}
		if limitPtr != nil {
			slaveParams["limit"] = fmt.Sprintf("%.0f", *limitPtr)
		}
		if legacy {
			slaveParams["legacy"] = "true"
		}

		p := newPrinter(true)
		propagate(cfg, "newuser", slaveParams, p)
	}

	id := subfileID(subfile)
	link := fmt.Sprintf("https://%s/client?id=%s", cfg.Server.Domain, id)

	return fmt.Sprintf("SUCCESS|CREATED|%s\nLink: %s", link, link), nil
}
