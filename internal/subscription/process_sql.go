package subscription

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"xraytool/internal/convert"
	"xraytool/internal/database"
	"xraytool/internal/events"
	"xraytool/internal/logger"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ProcessSQL is the next-generation subscription handler using the SQL database
// instead of the legacy devices_state.json and limited_users.db files.
func ProcessSQL(db *gorm.DB, cm *CacheManager, dispatcher *events.Dispatcher, req *Request) *Response {
	cfg := cm.cfg

	// 1. Resolve Client ID from request (xray_uuid)
	clientId, _ := ResolveClientID(req)
	if clientId == "" {
		return failResponse(404, "Invalid client id")
	}

	// 2. User-Agent Whitelisting
	uaWhitelist := cfg.Subscription.UserAgentWhitelist
	uaNoAccounting := cfg.Subscription.UserAgentNoChecks

	matchedAgent := ""
	uaLower := strings.ToLower(req.UserAgent)
	for _, agent := range uaWhitelist {
		if userAgentHasToken(uaLower, agent) {
			matchedAgent = agent
			break
		}
	}
	if matchedAgent == "" {
		return failResponse(403, "Unsupported client user-agent")
	}

	skipChecksAgent := ""
	for _, agent := range uaNoAccounting {
		if userAgentHasToken(uaLower, agent) {
			skipChecksAgent = agent
			break
		}
	}

	// 3. Load Subscription and User from Database
	var sub database.Subscription
	// Backward compatibility: match new ID, old XrayUUID, or old subfile in metadata
	var subs []database.Subscription
	var query *gorm.DB
	if db.Dialector.Name() == "postgres" {
		query = db.Where("id = ? OR xray_uuid = ? OR metadata::jsonb ->> 'subfile' = ? OR metadata::jsonb ->> 'subfile' = ?",
			clientId, clientId, clientId, clientId+".txt")
	} else {
		query = db.Where("id = ? OR xray_uuid = ? OR json_extract(metadata, '$.subfile') = ? OR json_extract(metadata, '$.subfile') = ?",
			clientId, clientId, clientId, clientId+".txt")
	}

	var user database.User
	if err := query.Limit(1).Find(&subs).Error; err != nil {
		return failResponse(500, "Database error")
	}

	cm.Refresh() // Ensure templates and Xray config are loaded
	xrayCfg := cm.GetRawConfig()
	if xrayCfg == nil {
		return failResponse(500, "xray config not loaded in cache")
	}

	var source string

	if len(subs) == 0 {
		source = "xray config"
		// Fallback: Check if user exists in xray_config.json directly (e.g. admin)
		var foundEmail string
		var foundUUID string
		inbounds, err := xrayCfg.GetInbounds()
		if err == nil {
			for _, ib := range inbounds {
				if ib.HasClientList() {
					clients, _ := ib.GetClients()
					for _, c := range clients {
						if c.GetString("id") == clientId || c.GetString("password") == clientId || c.GetString("subfile") == clientId || c.GetString("subfile") == clientId+".txt" {
							foundEmail = c.Email()
							foundUUID = c.GetString("id")
							break
						}
					}
				}
				if foundEmail != "" {
					break
				}
			}
		}

		if foundEmail != "" {
			// User exists only in config! Mock the sub and user objects
			sub = database.Subscription{
				ID:         clientId,
				XrayUUID:   foundUUID,
				Email:      foundEmail,
				Status:     "active",
				MaxDevices: 999, // admins have no device limit usually
			}
			// Skip device accounting and limits for admin fallback
			skipChecksAgent = "admin-fallback"
		} else {
			return failResponse(404, "User not found")
		}
	} else {
		source = "database"
		sub = subs[0]
		if err := db.Where("id = ?", sub.UserID).First(&user).Error; err != nil {
			return failResponse(404, "User not found")
		}
	}

	isBlockedUser := sub.Status == "expired" || sub.Status == "blocked" || sub.Status == "inactive"
	email := sub.Email
	uuid := sub.XrayUUID
	userPassSs := ""
	rawHy2Auth := ""

	// We still need to pull user's specific passwords from Xray config
	// because they are generated dynamically or stored only in Xray.
	if !isBlockedUser {
		inbounds, err := xrayCfg.GetInbounds()
		if err == nil {
			for _, ib := range inbounds {
				if ib.HasClientList() {
					clients, _ := ib.GetClients()
					for _, c := range clients {
						if c.Email() == email {
							if p := c.GetString("password"); p != "" {
								userPassSs = p
							}
							if a := c.GetString("auth"); a != "" {
								rawHy2Auth = a
							}
						}
					}
				}
			}
		}
	}

	expireVal := defaultExpireDate()
	if sub.EndsAt != nil {
		expireVal = sub.EndsAt.Format("02.01.2006")
	}

	deviceLimit := sub.MaxDevices
	if deviceLimit <= 0 && !isBlockedUser {
		deviceLimit = 3
	}

	// 4. Enforce Device Limits
	skipChecksReason := "none"
	if skipChecksAgent != "" {
		skipChecksReason = "ua:" + skipChecksAgent
	} else if isBlockedUser {
		skipChecksReason = "is-user-blocked"
	}
	skipChecks := skipChecksReason != "none"

	unsupportedClient := false
	deviceLimitReached := false
	hwid := ""

	if !skipChecks {
		hwid = normalizeHwid(pickRequestValue(req,
			[]string{"hwid", "HWID", "device_id", "deviceid", "deviceId", "deviceID", "udid", "UDID"},
			[]string{"HWID", "X-HWID", "X-Device-Id", "Device-Id", "DeviceId", "DeviceID", "X-DeviceId", "X-DeviceID", "X-Deviceid", "Deviceid", "X-UDID", "UDID"},
		))
		deviceModel := pickRequestValue(req, []string{"device_model", "model", "deviceModel"}, []string{"Device-Model", "X-Device-Model"})
		deviceOs := pickRequestValue(req, []string{"device_os", "os", "platform"}, []string{"Device-Os", "X-Device-Os", "X-Platform"})
		verOs := pickRequestValue(req, []string{"ver_os", "os_version", "osVersion"}, []string{"Ver-Os", "X-Os-Version", "Os-Version"})

		if hwid == "" {
			unsupportedClient = true
		}

		if !unsupportedClient && hwid != "" {
			// SQL-based Device tracking

			now := time.Now()

			err := db.Transaction(func(tx *gorm.DB) error {
				var device database.Device
				res := tx.Where("subscription_id = ? AND hw_id = ?", sub.ID, hwid).Limit(1).Find(&device)

				if res.Error != nil {
					return res.Error
				}

				if res.RowsAffected == 0 {
					// Check device limit before inserting
					// Lock the parent subscription row to prevent concurrent inserts from exceeding the limit
					var dummy database.Subscription
					if db.Dialector.Name() == "postgres" {
						tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", sub.ID).First(&dummy)
					} else if db.Dialector.Name() == "sqlite" {
						// Force a write lock in SQLite by touching the subscription record
						tx.Exec("UPDATE subscriptions SET updated_at = CURRENT_TIMESTAMP WHERE id = ?", sub.ID)
					}

					var currentCount int64
					tx.Model(&database.Device{}).Where("subscription_id = ?", sub.ID).Count(&currentCount)

					if currentCount >= int64(deviceLimit) {
						deviceLimitReached = true
						return nil // Don't error out, just don't insert
					}

					newDevice := database.Device{
						SubscriptionID: sub.ID,
						HWID:           hwid,
						DeviceModel:    deviceModel,
						DeviceOS:       deviceOs,
						VerOS:          verOs,
						UserAgent:      req.UserAgent,
						RequestCount:   1,
						FirstSeen:      now,
						LastSeen:       now,
					}
					return tx.Create(&newDevice).Error
				} else {
					return tx.Model(&device).Updates(map[string]interface{}{
						"last_seen":     now,
						"request_count": gorm.Expr("request_count + 1"),
						"device_model":  deviceModel,
						"device_os":     deviceOs,
						"ver_os":        verOs,
						"user_agent":    req.UserAgent,
					}).Error
				}
			})

			if err != nil {
				logger.Errorf("[ProcessSQL] SQL error in device check for subscription %s: %v", sub.ID, err)
				return failResponse(500, "database error")
			}

			if deviceLimitReached {
				eventData := map[string]interface{}{
					"email":        email,
					"client_id":    clientId,
					"subfile":      clientId + ".txt",
					"hwid":         hwid,
					"device_limit": deviceLimit,
					"device_model": deviceModel,
					"device_os":    deviceOs,
					"ver_os":       verOs,
					"user_agent":   req.UserAgent,
				}

				var userMetadata map[string]interface{}
				if user.Metadata != nil {
					userMetadata = user.Metadata
				}
				dispatcher.Dispatch("device.limit_reached", eventData, userMetadata)
			}
		}
	}

	// 5. Build Template Parameters
	pbk := derivePublicKey(firstRealityPrivateKey(xrayCfg))
	if pbk == "" {
		pbk = firstRealityPublicKey(xrayCfg)
	}
	sid := randomRealityShortID(xrayCfg)
	sni := firstRealitySNI(xrayCfg)
	serverIp := cfg.Server.IP

	ssServerPass := ssServerPassword(xrayCfg)
	mySsAuth := ""
	if ssServerPass != "" {
		if !isBlockedUser && userPassSs != "" && strings.ToLower(userPassSs) != "null" {
			mySsAuth = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("2022-blake3-aes-256-gcm:%s:%s", ssServerPass, userPassSs)))
		} else {
			mySsAuth = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("2022-blake3-aes-256-gcm:%s", ssServerPass)))
		}
	}

	hy2Pass := extractHy2Pass(rawHy2Auth)
	if hy2Pass == "" {
		hy2Pass = buildDeterministicHy2Pass(uuid, email)
	}
	hy2Auth := hy2Pass
	hysteria2Auth := hy2Pass
	hy2Obfs := getOrCreateHy2ObfsPassword(cfg.Paths.Hy2ConfigYAML, xrayCfg)

	// Fetch traffic download bytes
	downloadBytes := int64(0)
	downloadBytes = getDownloadBytes(cfg.Paths.StatsState, email)

	isVlessFormat := req.Query["format"] == "vless"

	// 6. Generate Response
	res := &Response{
		Headers: make(map[string]string),
	}

	res.Headers["X-SubPHP-Debug"] = "1"
	res.Headers["X-Checks-Bypass"] = skipChecksReason
	res.Headers["X-Is-User-Blocked"] = fmt.Sprintf("%t", isBlockedUser)
	res.Headers["X-Sub-Source"] = source

	var expireTs int64
	if sub.EndsAt != nil {
		expireTs = sub.EndsAt.Unix()
	} else {
		expireTs = parseDateToTimestamp(expireVal)
	}

	isBlockedOrExpired := isBlockedUser || expireTs <= time.Now().Unix()

	if deviceLimitReached || unsupportedClient || isBlockedOrExpired {
		res.Headers["Content-Disposition"] = `attachment; filename="configs.txt"`
		res.Headers["Profile-Title"] = "Torvalds VPN"
		res.Headers["Subscription-Userinfo"] = fmt.Sprintf("upload=0; download=%d; total=107374182400000; expire=%d", downloadBytes, expireTs)
		res.Headers["Profile-Update-Interval"] = "12"
		res.Headers["Profile-Type"] = "Sip002"
		res.Headers["Content-Type"] = "text/plain; charset=utf-8"
		res.Headers["Cache-Control"] = "no-store, no-cache, must-revalidate, max-age=0"
		res.Headers["Pragma"] = "no-cache"
		res.StatusCode = 200

		if deviceLimitReached {
			res.Headers["X-Reject-Reason"] = "device_limit_reached"
			res.Body = generateDummyVless(cm.cfg.Subscription.DummyConfigs.DeviceLimit)
		} else if unsupportedClient {
			res.Headers["X-Reject-Reason"] = "unsupported_client"
			res.Body = generateDummyVless(cm.cfg.Subscription.DummyConfigs.UnsupportedClient)
		} else { // isBlockedOrExpired
			res.Headers["X-Reject-Reason"] = "blocked_or_expired"
			res.Body = generateDummyVless(cm.cfg.Subscription.DummyConfigs.Expired)
		}
		return res
	}

	// Prepare VLESS output
	if isVlessFormat {
		res.Headers["Content-Disposition"] = `attachment; filename="configs.txt"`
		res.Headers["Profile-Title"] = "Torvalds VPN"
		res.Headers["Subscription-Userinfo"] = fmt.Sprintf("upload=0; download=%d; total=107374182400000; expire=%d", downloadBytes, expireTs)
		res.Headers["Profile-Update-Interval"] = "12"
		res.Headers["Profile-Type"] = "Sip002"
		res.Headers["Content-Type"] = "text/plain; charset=utf-8"
		res.Headers["Cache-Control"] = "no-store, no-cache, must-revalidate, max-age=0"
		res.Headers["Pragma"] = "no-cache"
	}

	// Default JSON output
	res.Headers["Content-Disposition"] = `attachment; filename="config.json"`
	res.Headers["Content-Type"] = "application/json; charset=utf-8"
	res.Headers["Cache-Control"] = "no-store, no-cache, must-revalidate, max-age=0"
	res.Headers["Pragma"] = "no-cache"

	// Parse subscription template (configs.txt) from cache
	subTmplData, routeGlobalData, routeRUData := cm.GetTemplates()
	if subTmplData == "" {
		return failResponse(500, "subscription template not found in cache")
	}
	subHeader, templates := parseSubscriptionTemplate(subTmplData)
	if len(templates) == 0 {
		return failResponse(500, "No templates found in subscription config")
	}

	canonicalSubLink := fmt.Sprintf("https://%s/client?id=%s", getRequestHost(req, cfg.Server.Domain), clientId)
	renderedHeader := generateHeader(email, canonicalSubLink, subHeader, fmt.Sprintf("%d", expireTs), fmt.Sprintf("%d", downloadBytes), isBlockedUser, deviceLimit)

	meta := parseHeaderMetadata(renderedHeader)
	for k, v := range meta.CustomHeaders {
		res.Headers[k] = v
	}

	res.Headers["Profile-Title"] = meta.Title
	if meta.UserInfo != "" {
		res.Headers["Subscription-Userinfo"] = meta.UserInfo
	}
	res.Headers["Profile-Update-Interval"] = meta.UpdateInterval
	if meta.WebUrl != "" {
		res.Headers["Profile-Web-Page-Url"] = meta.WebUrl
	}
	res.Headers["Profile-Type"] = "Sip002"

	// Render first template config
	jsonPayload := templates[0]
	replacer := strings.NewReplacer(
		"{HOST}", serverIp,
		"{PBK}", pbk,
		"{SID}", sid,
		"{SNI}", sni,
		"{UUID}", uuid,
		"{SS_AUTH}", mySsAuth,
		"{HY2_AUTH}", hy2Auth,
		"{HY2_OBFS}", hy2Obfs,
		"{HY2_OBFS_PASSWORD}", hy2Obfs,
		"{HYSTERIA2_AUTH}", hysteria2Auth,
		"{HYSTERIA2_OBFS}", hy2Obfs,
		"{EMAIL}", email,
		"{GLOBAL_ROUTING}", routeGlobalData,
		"{RU_ROUTING}", routeRUData,
	)
	jsonPayload = replacer.Replace(jsonPayload)

	// Validate JSON payload
	var temp interface{}
	if err := json.Unmarshal([]byte(jsonPayload), &temp); err != nil {
		logger.Errorf("[ProcessSQL] Invalid template config JSON for sub %s: %v", sub.ID, err)
		return failResponse(404, "Invalid template config JSON")
	}

	if isVlessFormat {
		shareLinks, err := convert.XrayJSONToShareText(jsonPayload)
		if err != nil {
			return failResponse(404, "VLESS subscription conversion failed: "+err.Error())
		}

		res.Headers["Content-Disposition"] = `attachment; filename="configs.txt"`
		res.Headers["Content-Type"] = "text/plain; charset=utf-8"
		res.Headers["Cache-Control"] = "no-store, no-cache, must-revalidate, max-age=0"
		res.Headers["Pragma"] = "no-cache"
		res.StatusCode = 200
		res.Body = shareLinks
		return res
	}

	res.StatusCode = 200
	res.Body = jsonPayload
	return res
}
