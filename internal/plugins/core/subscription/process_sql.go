package subscription

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	json "github.com/goccy/go-json"
	"strings"
	"time"

	"log/slog"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/logger"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/core/convert"
)

// ProcessSQL is the next-generation subscription handler using the SQL database
// instead of the legacy devices_state.json and limited_users.db files.
//
// isBanned is a function provided by the antifraud module; pass nil to disable
// anti-fraud checks (useful for tests or when the module is disabled).
func ProcessSQL(ctx context.Context, reg domain.Registry, cm *CacheManager, dispatcher *events.Dispatcher, req *Request, isBanned func(email string) bool) *Response {
	cfg := cm.cfg

	// 1. Resolve Client ID from request (subscription UUID or legacy identifier).
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

	cm.Refresh() // Ensure templates and the engine snapshot are loaded.
	snapshot, hasSnapshot := cm.SubscriptionConfigSnapshot()
	if !hasSnapshot {
		return failResponse(500, "engine configuration not loaded in cache")
	}

	var source string
	var sub domain.Subscription
	var user domain.User

	if subPtr == nil {
		source = "engine config"
		// Check engine-projected active clients first. This is the normal
		// Plugin Host path and keeps native engine JSON out of subscription
		// delivery.
		var foundEmail string
		var foundUUID string
		var isBlacklistedAdmin bool

		if hasSnapshot {
			if client := findSubscriptionClient(snapshot.ActiveClients, clientId); client != nil {
				foundEmail = client.Email
				foundUUID = client.ID
			}
		}

		// If not found in active config, check the engine-projected template to
		// see if it belongs to a blacklisted admin.
		if foundEmail == "" && hasSnapshot {
			if client := findSubscriptionClient(snapshot.TemplateClients, clientId); client != nil {
				foundEmail = client.Email
				foundUUID = client.ID
				isBlacklistedAdmin = true
			}
		}
		if foundEmail != "" {
			// User exists only in config/template! Mock the sub and user objects
			sub = domain.Subscription{
				ID:         clientId,
				UUID:       foundUUID,
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
	uuid := sub.UUID
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

	// Pull engine-managed user secrets from the projected snapshot. The legacy
	// raw-config fallback remains only for the old non-plugin entry point.
	if !isBlockedUser {
		if hasSnapshot {
			for _, client := range snapshot.ActiveClients {
				if client.Email != email {
					continue
				}
				if client.Password != "" {
					userPassSs = client.Password
				}
				if client.Auth != "" {
					rawHy2Auth = client.Auth
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

	pbk = snapshot.RealityPublicKey
	sid = subscriptionShortID(snapshot.RealityShortIDs, sub.ID)
	sni := snapshot.RealityServerName
	if sni == "" {
		sni = "google.com"
	}
	serverIp := cfg.Server.IP

	ssServerPass := snapshot.ShadowSocksSecret
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
	hy2Obfs := snapshot.HysteriaObfsSecret

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
	trafficProvider, quotaProvider := cm.TrafficProviders()
	if trafficProvider != nil {
		usage, usageFound, usageErr := trafficProvider.Usage(ctx, email)
		if usageErr != nil {
			logger.Warnf("[ProcessSQL] traffic provider failed for %s: %v; using legacy state files", email, usageErr)
		} else if usageFound {
			uploadBytes = usage.UploadBytes
			downloadBytes = usage.DownloadBytes
		}
	}

	if quotaProvider != nil {
		decision, quotaErr := quotaProvider.CheckQuota(ctx, pluginapi.Subscription{
			ID: sub.ID, UserID: sub.UserID, Email: sub.Email, UUID: sub.UUID,
			Status: sub.Status, MaxDevices: sub.MaxDevices, StartsAt: sub.StartsAt,
			EndsAt: sub.EndsAt, AutoRenew: sub.AutoRenew, Metadata: sub.Metadata,
			CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt,
		}, pluginapi.TrafficUsage{UploadBytes: uploadBytes, DownloadBytes: downloadBytes})
		if quotaErr != nil {
			logger.Warnf("[ProcessSQL] traffic quota provider failed for %s: %v", email, quotaErr)
		} else if decision.Exceeded {
			reason := decision.Reason
			if reason == "" {
				reason = "traffic_quota_exceeded"
			}
			return &Response{
				StatusCode: 200,
				Headers: map[string]string{
					"Content-Type":        "text/plain; charset=utf-8",
					"Content-Disposition": `attachment; filename="configs.txt"`,
					"Profile-Title":       "Torvalds VPN",
					"X-Reject-Reason":     reason,
					"Cache-Control":       "no-store, no-cache, must-revalidate, max-age=0",
					"Pragma":              "no-cache",
				},
				Body: generateDummyVless(cm.cfg.Subscription.DummyConfigs.Expired),
			}
		}
	}

	requestedFormat := strings.ToLower(strings.TrimSpace(req.Query["format"]))
	isVlessFormat := requestedFormat == "vless"
	isClashFormat := requestedFormat == "clash"

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

	// Prepare plain-text (share links / clash yaml) output
	if isVlessFormat || isClashFormat {
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
	deliveryJSON := jsonPayload
	formatJSON := jsonPayload
	templateHandled := false
	if processor := cm.SubscriptionTemplateProcessor(); processor != nil {
		processed, processErr := processor.ProcessSubscriptionTemplate(ctx, jsonPayload)
		if processErr != nil {
			logger.Errorf("[ProcessSQL] Subscription template processor failed for sub %s: %v", sub.ID, processErr)
			return failResponse(500, "Subscription template processing failed: "+processErr.Error())
		}
		if processed.Handled {
			if strings.TrimSpace(processed.JSONConfig) == "" {
				return failResponse(500, "Subscription template processor returned empty JSON config")
			}
			deliveryJSON = processed.JSONConfig
			templateHandled = true
			if strings.TrimSpace(processed.ExportJSONConfig) != "" {
				formatJSON = processed.ExportJSONConfig
			}
		}
	}

	// Engines can supply native client links independently of any concrete
	// subscription format. A format provider receives these links below; its
	// absence intentionally falls back to the legacy Xray conversion package.
	var pluginLinks []pluginapi.ClientLink
	if (isVlessFormat || isClashFormat) && !templateHandled {
		links, available, contributorErr := cm.BuildClientLinks(ctx, pluginapi.VPNUserConfig{
			Email:      email,
			UUID:       uuid,
			Auth:       rawHy2Auth,
			Subfile:    clientId,
			Expire:     expireVal,
			MaxDevices: deviceLimit,
		})
		if contributorErr != nil {
			logger.Warnf("[ProcessSQL] engine client-link contributor failed for %s: %v; using legacy template", sub.ID, contributorErr)
		} else if available {
			pluginLinks = links
		}
	}

	if isVlessFormat || isClashFormat {
		if provider := cm.SubscriptionFormatProvider(); provider != nil {
			formatted, err := provider.RenderSubscription(ctx, pluginapi.SubscriptionFormatRequest{
				Format: requestedFormat, JSONConfig: formatJSON, Links: pluginLinks,
			})
			if err != nil {
				if isVlessFormat {
					return failResponse(404, "VLESS subscription conversion failed: "+err.Error())
				}
				return failResponse(404, "Clash subscription conversion failed: "+err.Error())
			}
			if formatted.Handled {
				res.Headers["Content-Disposition"] = formatted.ContentDisposition
				res.Headers["Content-Type"] = formatted.ContentType
				res.Headers["Cache-Control"] = "no-store, no-cache, must-revalidate, max-age=0"
				res.Headers["Pragma"] = "no-cache"
				res.StatusCode = 200
				res.Body = formatted.Body
				return res
			}
		}
	}

	// The compatibility fallback intentionally stays in core for callers that
	// construct CacheManager outside Plugin Host. Normal server startup wires
	// subscription_format_legacy above, so the conversion implementation is
	// replaceable without changing the subscription hot path.
	if isVlessFormat {
		shareLinks, err := convert.XrayJSONToShareText(formatJSON)
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

	if isClashFormat {
		clashYAML, err := convert.XrayJSONToClashYAML(formatJSON)
		if err != nil {
			return failResponse(404, "Clash subscription conversion failed: "+err.Error())
		}

		res.Headers["Content-Disposition"] = `attachment; filename="config.yaml"`
		res.Headers["Content-Type"] = "text/yaml; charset=utf-8"
		res.Headers["Cache-Control"] = "no-store, no-cache, must-revalidate, max-age=0"
		res.Headers["Pragma"] = "no-cache"
		res.StatusCode = 200
		res.Body = clashYAML
		return res
	}

	res.StatusCode = 200
	res.Body = deliveryJSON
	return res
}

func findSubscriptionClient(clients []pluginapi.SubscriptionClient, clientID string) *pluginapi.SubscriptionClient {
	for index := range clients {
		client := &clients[index]
		if client.ID == clientID || client.Password == clientID || client.Subfile == clientID || client.Subfile == clientID+".txt" {
			return client
		}
	}
	return nil
}

func subscriptionShortID(ids []string, subscriptionID string) string {
	if len(ids) == 0 {
		return ""
	}
	hash := sha256.Sum256([]byte(subscriptionID))
	return ids[binary.BigEndian.Uint64(hash[:8])%uint64(len(ids))]
}
