package server

import (
	"net/http"
	"strings"

	"xraytool/internal/plugins/core/subscription"
)

// handleSubscriptionV2 is the new endpoint that serves Xray configurations
// backed by the GORM database (Postgres/SQLite) instead of legacy files.
func (r *Router) handleSubscriptionV2(w http.ResponseWriter, req *http.Request) {
	// 1. Collect request headers
	headers := make(map[string]string)
	for k, v := range req.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	headers["x-request-path"] = req.URL.Path

	// 2. Parse query parameters into map[string]string
	query := make(map[string]string)
	for k, v := range req.URL.Query() {
		if len(v) > 0 {
			query[k] = v[0]
		}
	}

	// 3. Get remote IP securely
	remoteAddr := getClientIP(req)

	ua := strings.ToLower(req.UserAgent())
	isBot := false
	if r.cfg != nil {
		for _, w := range r.cfg.Subscription.UserAgentNoChecks {
			if strings.Contains(ua, strings.ToLower(w)) {
				isBot = true
				break
			}
		}
	}
	if isBot {
		r.log.Debug("Incoming SQL subscription request", "ip", remoteAddr, "ua", req.UserAgent(), "url", req.URL.String())
	} else {
		r.log.Info("Incoming SQL subscription request", "ip", remoteAddr, "ua", req.UserAgent(), "url", req.URL.String())
	}

	// 4. Build subscription request payload
	subReq := &subscription.Request{
		RemoteAddr: remoteAddr,
		UserAgent:  req.UserAgent(),
		Query:      query,
		Headers:    headers,
	}

	// 5. Execute subscription process directly in memory using SQL Database
	subRes := r.userSvc.ProcessSQLSubscription(req.Context(), r.cm, r.dispatcher, subReq, r.isBanned)

	// 6. Send headers and write body
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	for k, v := range subRes.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(subRes.StatusCode)
	if _, err := w.Write([]byte(subRes.Body)); err != nil {
		r.log.Error("Ошибка записи ответа SQL подписки", "err", err)
	}

	source := subRes.Headers["X-Sub-Source"]
	if source == "" {
		source = "unknown"
	}
	rejectReason := subRes.Headers["X-Reject-Reason"]
	bypass := subRes.Headers["X-Checks-Bypass"]

	if isBot && subRes.StatusCode < 400 {
		r.log.Debug("Successfully served SQL subscription", "ip", remoteAddr, "status", subRes.StatusCode, "source", source, "bypass", bypass, "reject_reason", rejectReason)
	} else if subRes.StatusCode >= 400 || rejectReason != "" {
		r.log.Warn("Failed to serve SQL subscription", "ip", remoteAddr, "status", subRes.StatusCode, "source", source, "bypass", bypass, "reject_reason", rejectReason)
	} else {
		r.log.Info("Successfully served SQL subscription", "ip", remoteAddr, "status", subRes.StatusCode, "source", source, "bypass", bypass, "reject_reason", rejectReason)
	}
}
