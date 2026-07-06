package subscription

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"log/slog"
	"xraytool/internal/convert"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/logger"
	"xraytool/internal/vpn"
)

// ProcessSQL is the next-generation subscription handler using the SQL database
// instead of the legacy devices_state.json and limited_users.db files.
//
// isBanned is a function provided by the antifraud module; pass nil to disable
// anti-fraud checks (useful for tests or when the module is disabled).
func ProcessSQL(ctx context.Context, reg domain.Registry, cm *CacheManager, dispatcher *events.Dispatcher, req *Request, isBanned func(email string) bool) *Response {
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
	subPtr, err := reg.Subscriptions().FindByClientIdentifier(ctx, clientId)
	if err != nil {
		logger.Errorf("[ProcessSQL] DB error fetching subscription %s: %v", clientId, err)
		return failResponse(500, "Database error")
	}

	cm.Refresh() // Ensure templates and Xray config are loaded
	xrayCfg := cm.GetRawConfig()
	if xrayCfg == nil {
		return failResponse(500, "xray config not loaded in cache")
	}

	var source string
	var sub domain.Subscription
	var user domain.User

	if subPtr == nil {
		source = "xray config"
		// Fallback: Check if user exists in xray_config.json directly (e.g. admin)
		var foundEmail string
		var foundUUID string
		var isBlacklistedAdmin bool

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

		// If not found in active config, check template config to see if they are a blacklisted admin
		if foundEmail == "" && cm.cfg.Paths.XrayTemplate != "" {
			if tmplCfg, err := vpn.Read(cm.cfg.Paths.XrayTemplate); err == nil {
				if tmplInbounds, err := tmplCfg.GetInbounds(); err == nil {
					for _, ib := range tmplInbounds {
						if ib.HasClientList() {
							clients, _ := ib.GetClients()
							for _, c := range clients {
								if c.GetString("id") == clientId || c.GetString("password") == clientId || c.GetString("subfile") == clientId || c.GetString("subfile") == clientId+".txt" {
									foundEmail = c.Email()
									foundUUID = c.GetString("id")
									isBlacklistedAdmin = true
									break
								}
							}
						}
						if foundEmail != "" {
							break
						}
					}
				}
			}
		}

		if foundEmail != "" {
			// User exists only in config/template! Mock the sub and user objects
			sub = domain.Subscription{
				ID:         clientId,
				XrayUUID:   foundUUID,
				Email:      foundEmail,
				Status:     "active",
				MaxDevices: 999, // admins have no device limit usually
			}
			if isBlacklistedAdmin {
				sub.Status = "expired" // Forces dummy warning config output
			}
			// Skip device accounting and limits for admin fallback
			skipChecksAgent = "admin-fallback"
		} else {
			return failResponse(404, "User not found")
		}
	} else {
		source = "database"
		sub = *subPtr
		userPtr, err := reg.Users().FindByID(ctx, sub.UserID)
		if err != nil {
			logger.Errorf("[ProcessSQL] DB error fetching user for sub %s: %v", sub.ID, err)
			return failResponse(404, "User not found")
		}
		user = *userPtr
	}

	isBlockedUser := sub.Status == "expired" || sub.Status == "blocked" || sub.Status == "inactive"
	email := sub.Email
	uuid := sub.XrayUUID
	userPassSs := ""
	rawHy2Auth := ""

	// Anti-Fraud check: if the user is currently soft-banned, return ONLY dummy
	// warning profiles. Real outbounds are NOT generated, preventing any attempt
	// to connect and protecting server CPU from TLS handshake load.
	if isBanned != nil && isBanned(email) {
		res := &Response{
			StatusCode: 200,
			Headers: map[string]string{
				"Content-Type":        "text/plain; charset=utf-8",
				"Content-Disposition": `attachment; filename="configs.txt"`,
				"Profile-Title":       "Torvalds VPN",
				"X-Reject-Reason":     "antifraud_ban",
				"Cache-Control":       "no-store, no-cache, must-revalidate, max-age=0",
				"Pragma":              "no-cache",
			},
			Body: generateDummyVless(cm.cfg.Subscription.DummyConfigs.AntiFraud),
		}
		slog.Default().Info("antifraud: serving dummy subscription", "email", email)
		return res
	}

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

		if hwid == "" {
			unsupportedClient = true
		}

		if !unsupportedClient && hwid != "" {
			// SQL-based Device tracking
			deviceLimitReached, err = reg.Devices().TrackDevice(ctx, sub.ID, hwid, deviceModel, deviceOs, req.UserAgent, deviceLimit)

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
	var pbk string
	var sid string

	keys := cm.GetRealityKeys()
	if keys != nil {
		pbk = keys.PublicKey
		if len(keys.ShortIDs) > 0 {
			h := sha256.Sum256([]byte(sub.ID))
			val := binary.BigEndian.Uint64(h[:8])
			idx := val % uint64(len(keys.ShortIDs))
			sid = keys.ShortIDs[idx]
		}
	}

	if pbk == "" {
		pbk = derivePublicKey(firstRealityPrivateKey(xrayCfg))
		if pbk == "" {
			pbk = firstRealityPublicKey(xrayCfg)
		}
	}
	if sid == "" {
		sid = randomRealityShortID(xrayCfg)
	}
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
	if hy2Pass == "" || isUUID(hy2Pass) {
		hy2Pass = buildDeterministicHy2Pass(uuid, email)
	}
	hy2Auth := hy2Pass
	hysteria2Auth := hy2Pass
	hy2Obfs := getOrCreateHy2ObfsPassword(cfg.Paths.Hy2ConfigYAML, xrayCfg)

	// Fetch traffic bytes: try inferred (combined master+slave) stats first,
	// falling back to local master stats if the user isn't found there.
	var uploadBytes, downloadBytes int64
	var found bool
	if cfg.Paths.InferredStats != "" {
		uploadBytes, downloadBytes, found = getTrafficBytes(cfg.Paths.InferredStats, email)
	}
	if !found {
		uploadBytes, downloadBytes, _ = getTrafficBytes(cfg.Paths.StatsState, email)
	}

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
		res.Headers["Subscription-Userinfo"] = fmt.Sprintf("upload=%d; download=%d; total=107374182400000; expire=%d", uploadBytes, downloadBytes, expireTs)
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
		res.Headers["Subscription-Userinfo"] = fmt.Sprintf("upload=%d; download=%d; total=107374182400000; expire=%d", uploadBytes, downloadBytes, expireTs)
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

	domainStr := cfg.Server.Domain
	hostStr := getRequestHost(req, domainStr)
	if hostStr != "" {
		domainStr = hostStr
	}
	canonicalSubLink := fmt.Sprintf("https://%s/client?id=%s", domainStr, clientId)
	renderedHeader := generateHeader(email, canonicalSubLink, subHeader, fmt.Sprintf("%d", expireTs), fmt.Sprintf("%d", uploadBytes), fmt.Sprintf("%d", downloadBytes), isBlockedUser, deviceLimit)

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
		"{UP}", fmt.Sprintf("%d", uploadBytes),
		"{DOWN}", fmt.Sprintf("%d", downloadBytes),
	)
	jsonPayload = replacer.Replace(jsonPayload)

	// Validate JSON payload
	if !json.Valid([]byte(jsonPayload)) {
		logger.Errorf("[ProcessSQL] Invalid template config JSON for sub %s", sub.ID)
		return failResponse(500, "Invalid template config JSON")
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
