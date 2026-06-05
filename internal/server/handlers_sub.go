package server

import (
	"net/http"
	"strings"

	"xraytool/internal/database"
	"xraytool/internal/subscription"
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

	// 3. Get remote IP
	remoteAddr := req.Header.Get("X-Real-IP")
	if remoteAddr == "" {
		remoteAddr = req.RemoteAddr
	}

	isBot := strings.Contains(strings.ToLower(req.UserAgent()), "megasupersecretua")
	if isBot {
		r.log.Debug("Incoming SQL subscription request", "ip", remoteAddr, "ua", req.UserAgent(), "path", req.URL.Path)
	} else {
		r.log.Info("Incoming SQL subscription request", "ip", remoteAddr, "ua", req.UserAgent(), "path", req.URL.Path)
	}

	// 4. Build subscription request payload
	subReq := &subscription.Request{
		RemoteAddr: remoteAddr,
		UserAgent:  req.UserAgent(),
		Query:      query,
		Headers:    headers,
	}

	// 5. Execute subscription process directly in memory using SQL Database
	subRes := subscription.ProcessSQL(database.DB(), r.cm, r.dispatcher, subReq)

	// 6. Send headers and write body
	for k, v := range subRes.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(subRes.StatusCode)
	if _, err := w.Write([]byte(subRes.Body)); err != nil {
		r.log.Error("Ошибка записи ответа SQL подписки", "err", err)
	}

	if isBot && subRes.StatusCode < 400 {
		r.log.Debug("Successfully served SQL subscription", "ip", remoteAddr, "status", subRes.StatusCode)
	} else if subRes.StatusCode >= 400 {
		r.log.Warn("Failed to serve SQL subscription", "ip", remoteAddr, "status", subRes.StatusCode)
	} else {
		r.log.Info("Successfully served SQL subscription", "ip", remoteAddr, "status", subRes.StatusCode)
	}
}
