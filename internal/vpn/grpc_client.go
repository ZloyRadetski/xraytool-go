// Package xrayapi provides a native gRPC client for the Xray API.
// This file implements direct gRPC communication with the Xray core process,
// replacing the os/exec-based approach in api.go with a persistent connection.
//
// Architecture notes:
//   - A single *grpc.ClientConn is shared and reused across all calls (Connection Pool).
//   - Reconnect logic handles TransientFailure and Shutdown states automatically.
//   - JSON-to-Protobuf conversion uses xray-core's own infra/conf package,
//     which is identical to the parsing logic the xray CLI uses internally.
//   - GetUsersStats is preferred over QueryStats because it returns strongly-typed
//     structs instead of strings that require manual parsing.
package vpn

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	loggerService "github.com/xtls/xray-core/app/log/command"
	handlerService "github.com/xtls/xray-core/app/proxyman/command"
	statsService "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"

	cserial "github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	xrayconf "github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy/shadowsocks"
	"github.com/xtls/xray-core/proxy/shadowsocks_2022"
	"github.com/xtls/xray-core/proxy/trojan"
	vlessin "github.com/xtls/xray-core/proxy/vless/inbound"
	vmessin "github.com/xtls/xray-core/proxy/vmess/inbound"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// defaultCallTimeout is applied to every individual gRPC call.
	// Intentionally short to avoid blocking the billing worker goroutine.
	defaultCallTimeout = 5 * time.Second

	// connectTimeout is used when the connection is being (re)established.
	connectTimeout = 3 * time.Second
)

// GRPCClient is a stateful client that holds a persistent gRPC connection to Xray.
// Use NewGRPCClient to create one. It is safe for concurrent use.
type GRPCClient struct {
	addr string
	log  *slog.Logger
	mu   sync.Mutex
}

// NewGRPCClient creates a new GRPCClient. The connection is lazy — it is only
// established on the first call. addr must be in "host:port" format (e.g. "127.0.0.1:10085").
// log is the logger to use; pass slog.Default() in cmd, or a test logger in tests.
func NewGRPCClient(addr string, log *slog.Logger) *GRPCClient {
	return &GRPCClient{
		addr: addr,
		log:  log.With("component", "xray-grpc"),
	}
}

var (
	globalConns    = make(map[string]*grpc.ClientConn)
	globalDialErr  = make(map[string]error)
	globalDialTime = make(map[string]time.Time)
	globalConnsMu  sync.Mutex
)

// ---------------------------------------------------------------------------
// Connection management
// ---------------------------------------------------------------------------

// dial returns the existing connection or establishes a new one.
// On each call the connection state is checked; broken connections are recreated.
// Xray gRPC does not use TLS by default — it binds to localhost only.
//
// Dry-run edge cases handled:
//   - Xray not running: dial blocks for connectTimeout, then returns error.
//   - Previous connection dropped: state is Shutdown/TransientFailure → recreate.
func (g *GRPCClient) dial() (*grpc.ClientConn, error) {
	globalConnsMu.Lock()
	defer globalConnsMu.Unlock()

	// Check if we recently failed to dial this address (within 10 seconds) to prevent dial storms
	if lastErr, ok := globalDialErr[g.addr]; ok {
		if time.Since(globalDialTime[g.addr]) < 10*time.Second {
			return nil, lastErr
		}
		// Cooldown expired, retry dialing
		delete(globalDialErr, g.addr)
		delete(globalDialTime, g.addr)
	}

	conn := globalConns[g.addr]
	if conn != nil {
		state := conn.GetState()
		if state != connectivity.Shutdown && state != connectivity.TransientFailure {
			return conn, nil
		}
		// Close stale connection before recreating.
		_ = conn.Close()
		delete(globalConns, g.addr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	//nolint:staticcheck // DialContext is the established pattern for lazy+block dial in grpc v1.
	newConn, err := grpc.DialContext(
		ctx,
		g.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		// Cache the dial failure with timestamp
		globalDialErr[g.addr] = err
		globalDialTime[g.addr] = time.Now()
		return nil, fmt.Errorf("xrayapi: dial %s: %w", g.addr, err)
	}

	g.log.Info("xrayapi: gRPC connection established", "addr", g.addr)
	globalConns[g.addr] = newConn
	return newConn, nil
}

// Close releases the underlying gRPC connection. Safe to call multiple times.
// Note: with global connection pooling, this is a no-op to allow connection reuse.
func (g *GRPCClient) Close() {
	// No-op. The connection is held globally and reused across clients.
}

// ---------------------------------------------------------------------------
// QueryStats — StatsService.GetUsersStats
// ---------------------------------------------------------------------------

// QueryStats fetches per-user traffic counters via the gRPC StatsService.
//
// We strictly use QueryStatsRequest instead of GetUsersStatsRequest because
// Xray-core's GetUsersStats has a known bug: it filters out users who are not
// currently online (missing from OnlineMap), which causes us to lose their traffic counters.
func (g *GRPCClient) QueryStats(ctx context.Context) ([]UserStat, error) {
	conn, err := g.dial()
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, defaultCallTimeout)
	defer cancel()

	client := statsService.NewStatsServiceClient(conn)
	resp, err := client.QueryStats(callCtx, &statsService.QueryStatsRequest{
		Pattern: "user>>>",
		Reset_:  false, // Billing worker resets counters via a dedicated call if needed.
	})
	if err != nil {
		return nil, fmt.Errorf("xrayapi: QueryStats: %w", err)
	}

	if resp == nil || len(resp.Stat) == 0 {
		return nil, nil
	}

	userMap := make(map[string]*UserStat)
	for _, stat := range resp.Stat {
		// Format: user>>>email>>>traffic>>>uplink
		parts := strings.Split(stat.Name, ">>>")
		if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" {
			continue
		}
		email := parts[1]
		if email == "" {
			continue
		}
		direction := parts[3]

		if _, ok := userMap[email]; !ok {
			userMap[email] = &UserStat{Email: email}
		}

		if direction == "uplink" {
			userMap[email].Up = stat.Value
		} else if direction == "downlink" {
			userMap[email].Down = stat.Value
		}
	}

	stats := make([]UserStat, 0, len(userMap))
	for _, u := range userMap {
		stats = append(stats, *u)
	}
	return stats, nil
}

// ---------------------------------------------------------------------------
// AddUser — HandlerService.AlterInbound (AddUserOperation)
// ---------------------------------------------------------------------------

// AddUser hot-adds a single user to one inbound via gRPC.
//
// Parameters:
//   - inboundTag: the Xray inbound tag (e.g. "vless-reality-443")
//   - inboundProtocol: the protocol name (e.g. "vless", "vmess", "trojan")
//   - clientJSON: raw JSON of the single client object as it appears in xray config
//     (e.g. {"id":"...","email":"...","flow":"xtls-rprx-vision"})
//
// JSON → Protobuf strategy:
//
//	We wrap clientJSON in a minimal settings object and feed it to the appropriate
//	infra/conf parser (the same one the xray CLI uses internally via inbound_user_add.go).
//	This avoids hand-crafting Protobuf structures for each protocol.
//
// Dry-run scenarios verified:
//   - Invalid UUID in clientJSON → parseUserForProtocol returns error before any gRPC call.
//   - Unsupported protocol → parseUserForProtocol returns descriptive error.
//   - gRPC call fails → error wrapped with tag/email for easy log correlation.
func (g *GRPCClient) AddUserSingle(ctx context.Context, inboundTag, inboundProtocol string, clientJSON []byte) error {
	user, err := parseUserForProtocol(inboundProtocol, clientJSON)
	if err != nil {
		return fmt.Errorf("xrayapi: AddUser: parse user (tag=%s proto=%s): %w", inboundTag, inboundProtocol, err)
	}

	conn, err := g.dial()
	if err != nil {
		return err
	}

	callCtx, cancel := context.WithTimeout(ctx, defaultCallTimeout)
	defer cancel()

	client := handlerService.NewHandlerServiceClient(conn)
	_, err = client.AlterInbound(callCtx, &handlerService.AlterInboundRequest{
		Tag: inboundTag,
		Operation: cserial.ToTypedMessage(
			&handlerService.AddUserOperation{User: user},
		),
	})
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			g.log.Info("xrayapi: user already in inbound, skipping", "tag", inboundTag, "email", user.Email)
			return nil
		}
		return fmt.Errorf("xrayapi: AddUser (tag=%s email=%s): %w", inboundTag, user.Email, err)
	}

	g.log.Info("xrayapi: user added via gRPC", "tag", inboundTag, "email", user.Email)
	return nil
}

// AddUser hot-adds users from the payload payload array.
// This matches the old api.Client.AddUser signature for seamless migration.
func (g *GRPCClient) AddUser(ctx context.Context, payload []TaggedClient, configPath string) error {
	// Read inbound metadata from the live config to resolve protocols.
	cfg, cfgErr := Read(configPath)
	if cfgErr != nil {
		g.log.Warn("failed to read config for AddUser protocols", "path", configPath, "err", cfgErr)
	}
	var inbounds []RawInbound
	if cfg != nil {
		inbounds, _ = cfg.GetInbounds()
	}
	ibByTag := make(map[string]string, len(inbounds))
	for _, ib := range inbounds {
		ibByTag[ib.Tag()] = ib.Protocol()
	}

	var errs []string
	var fallbackPayloads []TaggedClient

	for _, tc := range payload {
		proto := ibByTag[tc.Tag]
		if proto == "" {
			errs = append(errs, fmt.Sprintf("tag=%s: protocol not found in config", tc.Tag))
			continue
		}

		apiClient := tc.Client.ForXrayAPI()
		clientJSON, err := json.Marshal(apiClient)
		if err != nil {
			errs = append(errs, fmt.Sprintf("tag=%s: marshal err: %v", tc.Tag, err))
			continue
		}

		if err := g.AddUserSingle(ctx, tc.Tag, proto, clientJSON); err != nil {
			if strings.Contains(err.Error(), "unsupported protocol") {
				g.log.Info("xrayapi: queuing unsupported protocol for legacy hot-add", "tag", tc.Tag, "proto", proto)
				fallbackPayloads = append(fallbackPayloads, tc)
			} else {
				errs = append(errs, err.Error())
			}
		}
	}

	if len(fallbackPayloads) > 0 {
		if err := g.fallbackAddUser(ctx, fallbackPayloads, configPath); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("xrayapi: AddUser errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// fallbackAddUser uses the legacy os/exec approach for protocols that our protobuf parsers
// do not support (e.g. hysteria/hysteria2 on custom Xray builds).
func (g *GRPCClient) fallbackAddUser(ctx context.Context, payload []TaggedClient, configPath string) error {
	apiPld := buildAddPayload(payload, configPath)
	data, err := json.Marshal(apiPld)
	if err != nil {
		return fmt.Errorf("marshal fallback payload: %w", err)
	}

	f, err := os.CreateTemp("", "xray-adu-fallback-*.json")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(f.Name())

	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	f.Close()

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(callCtx, "xray", "api", "adu", "-s", g.addr, f.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		outStr := strings.TrimSpace(string(out))
		if strings.Contains(outStr, "already exists") {
			g.log.Info("xrayapi: user already exists in fallback hot-add, skipping")
			return nil
		}
		g.log.Warn("Fallback hot-add failed", "err", err, "out", outStr)
		return fmt.Errorf("fallback adu: %v (output: %s)", err, outStr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// RemoveUser — HandlerService.AlterInbound (RemoveUserOperation)
// ---------------------------------------------------------------------------

// RemoveUser hot-removes a user from every provided inbound tag via gRPC.
// All errors are collected; the operation continues even if one tag fails.
//
// Dry-run scenarios verified:
//   - Empty tags slice → returns nil immediately (no-op, not an error).
//   - Empty email → returns error before any network call.
//   - One tag fails, others succeed → partial error collected and returned.
//   - Goroutine leak check: cancel() is called in each loop iteration via defer.
func (g *GRPCClient) RemoveUser(ctx context.Context, email string, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return fmt.Errorf("xrayapi: RemoveUser: email must not be empty")
	}

	conn, err := g.dial()
	if err != nil {
		return err
	}

	client := handlerService.NewHandlerServiceClient(conn)

	var errs []string
	for _, tag := range tags {
		func() {
			callCtx, cancel := context.WithTimeout(ctx, defaultCallTimeout)
			defer cancel()

			_, callErr := client.AlterInbound(callCtx, &handlerService.AlterInboundRequest{
				Tag: tag,
				Operation: cserial.ToTypedMessage(
					&handlerService.RemoveUserOperation{Email: email},
				),
			})
			if callErr != nil {
				if strings.Contains(callErr.Error(), "not found") {
					g.log.Info("xrayapi: user already not in inbound, skipping", "tag", tag, "email", email)
				} else {
					errs = append(errs, fmt.Sprintf("tag=%s: %v", tag, callErr))
				}
			} else {
				g.log.Info("xrayapi: user removed via gRPC", "tag", tag, "email", email)
			}
		}()
	}

	if len(errs) > 0 {
		return fmt.Errorf("xrayapi: RemoveUser errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// JSON → Protobuf parser (mirrors xray-core's infra/conf logic)
// ---------------------------------------------------------------------------

// parseUserForProtocol converts a raw JSON client object into a *protocol.User
// Protobuf message suitable for gRPC AlterInbound calls.
//
// It wraps clientJSON in a minimal inbound settings object and feeds it to the
// appropriate xray-core infra/conf parser — the same path that inbound_user_add.go
// takes when processing adu JSON files. This means protocol-specific validation
// (UUID format, flow values, etc.) is performed by xray-core, not us.
//
// Supported protocols: vless, vmess, trojan, shadowsocks, shadowsocks-2022.
func parseUserForProtocol(proto string, clientJSON []byte) (*protocol.User, error) {
	switch strings.ToLower(proto) {
	case "vless", "xhttp", "splithttp":
		// VLESS inbound settings require "decryption":"none" — validated by Build().
		settingsJSON := fmt.Sprintf(`{"decryption":"none","clients":[%s]}`, string(clientJSON))
		rawSettings := json.RawMessage(settingsJSON)
		cfg := &xrayconf.VLessInboundConfig{}
		if err := json.Unmarshal(rawSettings, cfg); err != nil {
			return nil, fmt.Errorf("unmarshal vless settings: %w", err)
		}
		built, err := cfg.Build()
		if err != nil {
			return nil, fmt.Errorf("build vless config: %w", err)
		}
		return firstUserFrom(built)

	case "vmess":
		rawSettings := json.RawMessage(fmt.Sprintf(`{"clients":[%s]}`, string(clientJSON)))
		cfg := &xrayconf.VMessInboundConfig{}
		if err := json.Unmarshal(rawSettings, cfg); err != nil {
			return nil, fmt.Errorf("unmarshal vmess settings: %w", err)
		}
		built, err := cfg.Build()
		if err != nil {
			return nil, fmt.Errorf("build vmess config: %w", err)
		}
		return firstUserFrom(built)

	case "trojan":
		rawSettings := json.RawMessage(fmt.Sprintf(`{"clients":[%s]}`, string(clientJSON)))
		cfg := &xrayconf.TrojanServerConfig{}
		if err := json.Unmarshal(rawSettings, cfg); err != nil {
			return nil, fmt.Errorf("unmarshal trojan settings: %w", err)
		}
		built, err := cfg.Build()
		if err != nil {
			return nil, fmt.Errorf("build trojan config: %w", err)
		}
		return firstUserFrom(built)

	case "shadowsocks":
		rawSettings := json.RawMessage(fmt.Sprintf(`{"clients":[%s]}`, string(clientJSON)))
		cfg := &xrayconf.ShadowsocksServerConfig{}
		if err := json.Unmarshal(rawSettings, cfg); err != nil {
			return nil, fmt.Errorf("unmarshal shadowsocks settings: %w", err)
		}
		built, err := cfg.Build()
		if err != nil {
			return nil, fmt.Errorf("build shadowsocks config: %w", err)
		}
		return firstUserFrom(built)

	default:
		return nil, fmt.Errorf("unsupported protocol for gRPC AddUser: %q", proto)
	}
}

// firstUserFrom extracts the first *protocol.User from a built inbound proto.Message.
// It performs a type switch over the known inbound config types to retrieve the
// user slice, which is identical to the extractInboundUsers function in xray-core's
// inbound_user_add.go.
func firstUserFrom(built interface{}) (*protocol.User, error) {
	// Extract the user slice depending on the concrete inbound config type.
	var users []*protocol.User

	switch ty := built.(type) {
	case *vmessin.Config:
		users = ty.User
	case *vlessin.Config:
		users = ty.Users
	case *trojan.ServerConfig:
		users = ty.Users
	case *shadowsocks.ServerConfig:
		users = ty.Users
	case *shadowsocks_2022.MultiUserServerConfig:
		users = ty.Users
	default:
		// Fallback: try to extract from core.InboundHandlerConfig (e.g. if Build returned that).
		if ihc, ok := built.(*core.InboundHandlerConfig); ok {
			inst, err := ihc.ProxySettings.GetInstance()
			if err != nil {
				return nil, fmt.Errorf("get inbound instance: %w", err)
			}
			return firstUserFrom(inst)
		}
		return nil, fmt.Errorf("unsupported inbound config type: %T", built)
	}

	if len(users) == 0 {
		return nil, fmt.Errorf("no users found in parsed inbound config")
	}
	return users[0], nil
}

// ---------------------------------------------------------------------------
// RestartLogger — LoggerService.RestartLogger
// ---------------------------------------------------------------------------

// RebuildInbound hot-rebuilds a single inbound by removing it from the running
// Xray process and re-adding it with the provided inbound JSON.
// Used for protocols like hysteria2 that don't support per-user hot-add/hot-remove.
//
// The inboundJSON must be the raw JSON representation of the inbound as it
// appears in config.json (a single JSON object with "tag", "protocol",
// "settings", etc.).
func (g *GRPCClient) RebuildInbound(ctx context.Context, tag string, inboundJSON []byte) error {
	// Step 1: remove the inbound from the running process.
	rmCtx, rmCancel := context.WithTimeout(ctx, 10*time.Second)
	defer rmCancel()

	rmCmd := exec.CommandContext(rmCtx, "xray", "api", "rmi", "--server", g.addr, tag)
	rmOut, rmErr := rmCmd.CombinedOutput()
	if rmErr != nil {
		rmStr := strings.TrimSpace(string(rmOut))
		// "not found" or "not enough information for making a decision" (ErrNoClue) is acceptable — the inbound might already be absent.
		if !strings.Contains(rmStr, "not found") && !strings.Contains(rmStr, "not enough information for making a decision") {
			g.log.Warn("xrayapi: remove inbound failed", "tag", tag, "err", rmErr, "out", rmStr)
			return fmt.Errorf("rmi %s: %v (output: %s)", tag, rmErr, rmStr)
		}
		g.log.Info("xrayapi: inbound not found (already removed/not started yet), proceeding with add", "tag", tag)
	} else {
		g.log.Info("xrayapi: inbound removed", "tag", tag)
	}

	// Step 2: write the inbound JSON to a temp file.
	f, err := os.CreateTemp("", "xray-adi-inbound-*.json")
	if err != nil {
		return fmt.Errorf("creating temp file for adi: %w", err)
	}
	defer os.Remove(f.Name())

	wrappedJSON := fmt.Sprintf(`{"inbounds":[%s]}`, string(inboundJSON))
	if _, err := f.Write([]byte(wrappedJSON)); err != nil {
		f.Close()
		return fmt.Errorf("writing temp file for adi: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing temp file for adi: %w", err)
	}

	// Step 3: add the inbound back.
	adCtx, adCancel := context.WithTimeout(ctx, 10*time.Second)
	defer adCancel()

	adCmd := exec.CommandContext(adCtx, "xray", "api", "adi", "--server", g.addr, f.Name())
	adOut, adErr := adCmd.CombinedOutput()
	if adErr != nil {
		adStr := strings.TrimSpace(string(adOut))
		g.log.Error("xrayapi: add inbound failed", "tag", tag, "err", adErr, "out", adStr)
		return fmt.Errorf("adi %s: %v (output: %s)", tag, adErr, adStr)
	}

	g.log.Info("xrayapi: inbound rebuilt", "tag", tag)
	return nil
}

// ---------------------------------------------------------------------------
// RestartLogger — LoggerService.RestartLogger
// ---------------------------------------------------------------------------

// RestartLogger signals Xray core to close and reopen its log file handles.
// This is the safe, zero-downtime mechanism for log rotation:
//  1. The caller renames access.log → access.log.old  (Xray keeps writing to .old via the open fd)
//  2. RestartLogger is called  →  Xray closes the old fd and opens a fresh access.log
//  3. The caller drains access.log.old and removes it, freeing RAM (tmpfs)
//
// No user connections are interrupted; Xray core is NOT restarted.
//
// Dry-run scenarios verified:
//   - Xray not running: dial() returns error within connectTimeout.
//   - Call succeeds but Xray ignores it (noop): log file keeps growing — Rotator handles that gracefully.
func (g *GRPCClient) RestartLogger(ctx context.Context) error {
	conn, err := g.dial()
	if err != nil {
		return err
	}

	callCtx, cancel := context.WithTimeout(ctx, defaultCallTimeout)
	defer cancel()

	client := loggerService.NewLoggerServiceClient(conn)
	_, err = client.RestartLogger(callCtx, &loggerService.RestartLoggerRequest{})
	if err != nil {
		return fmt.Errorf("xrayapi: RestartLogger: %w", err)
	}

	g.log.Info("xrayapi: logger restarted via gRPC (log rotation)")
	return nil
}
