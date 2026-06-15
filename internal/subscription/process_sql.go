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
	if err := db.Where("id = ? OR xray_uuid = ? OR json_extract(metadata, '$.subfile') = ? OR json_extract(metadata, '$.subfile') = ?", 
		clientId, clientId, clientId, clientId+".txt").First(&sub).Error; err != nil {
		return failResponse(404, "Subscription not found")
	}

	var user database.User
	if err := db.Where("id = ?", sub.UserID).First(&user).Error; err != nil {
		return failResponse(404, "User not found")
	}

	cm.Refresh() // Ensure templates and Xray config are loaded
	xrayCfg := cm.GetRawConfig()
	if xrayCfg == nil {
		return failResponse(500, "xray config not loaded in cache")
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
				err := tx.Where("subscription_id = ? AND hwid = ?", sub.ID, hwid).First(&device).Error

				if err == gorm.ErrRecordNotFound {
					// Check device limit before inserting
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
				} else if err == nil {
					return tx.Model(&device).Updates(map[string]interface{}{
						"last_seen":     now,
						"request_count": gorm.Expr("request_count + 1"),
						"device_model":  deviceModel,
						"device_os":     deviceOs,
						"ver_os":        verOs,
						"user_agent":    req.UserAgent,
					}).Error
				}
				return err
			})

			if err != nil {
				logger.Errorf("[ProcessSQL] SQL error in device check for subscription %s: %v", sub.ID, err)
				return failResponse(500, fmt.Sprintf("database error: %v", err))
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
				dispatcher.DispatchSync("device.limit_reached", eventData, userMetadata)
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
	if !isBlockedUser {
		downloadBytes = getDownloadBytes(cfg.Paths.InferredStats, email)
		if downloadBytes == 0 {
			downloadBytes = getDownloadBytes(cfg.Paths.StatsState, email)
		}
	}

	isVlessFormat := req.Query["format"] == "vless"

	// 6. Generate Response
	res := &Response{
		Headers: make(map[string]string),
	}

	res.Headers["X-SubPHP-Debug"] = "1"
	res.Headers["X-Checks-Bypass"] = skipChecksReason
	res.Headers["X-Is-User-Blocked"] = fmt.Sprintf("%t", isBlockedUser)

	expireTs := parseDateToTimestamp(expireVal)

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
			res.Body = generateDummyVless(cm.cfg.Subscription.DummyConfigs.DeviceLimit)
		} else if unsupportedClient {
			res.Body = generateDummyVless(cm.cfg.Subscription.DummyConfigs.UnsupportedClient)
		} else { // isBlockedOrExpired
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
	jsonPayload = strings.ReplaceAll(jsonPayload, "{HOST}", serverIp)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{PBK}", pbk)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{SID}", sid)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{SNI}", sni)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{UUID}", uuid)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{SS_AUTH}", mySsAuth)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{HY2_AUTH}", hy2Auth)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{HY2_OBFS}", hy2Obfs)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{HY2_OBFS_PASSWORD}", hy2Obfs)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{HYSTERIA2_AUTH}", hysteria2Auth)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{HYSTERIA2_OBFS}", hy2Obfs)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{EMAIL}", email)

	// Load Global and RU routing templates from Cache
	jsonPayload = strings.ReplaceAll(jsonPayload, "{GLOBAL_ROUTING}", routeGlobalData)
	jsonPayload = strings.ReplaceAll(jsonPayload, "{RU_ROUTING}", routeRUData)

	// Validate JSON payload
	var temp interface{}
	if err := json.Unmarshal([]byte(jsonPayload), &temp); err != nil {
		return failResponse(404, "Invalid template config JSON: "+err.Error())
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
