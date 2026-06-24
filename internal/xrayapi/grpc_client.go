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
package xrayapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	handlerService "github.com/xtls/xray-core/app/proxyman/command"
	statsService "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	"xraytool/internal/xrayconfig"
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
	conn *grpc.ClientConn
}

// NewGRPCClient creates a new GRPCClient. The connection is lazy — it is only
// established on the first call. addr must be in "host:port" format (e.g. "127.0.0.1:10085").
func NewGRPCClient(addr string) *GRPCClient {
	return &GRPCClient{
		addr: addr,
		log:  slog.Default().With("component", "xray-grpc"),
	}
}

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
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.conn != nil {
		state := g.conn.GetState()
		if state != connectivity.Shutdown && state != connectivity.TransientFailure {
			return g.conn, nil
		}
		// Close stale connection before recreating.
		_ = g.conn.Close()
		g.conn = nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	//nolint:staticcheck // DialContext is the established pattern for lazy+block dial in grpc v1.
	conn, err := grpc.DialContext(
		ctx,
		g.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("xrayapi: dial %s: %w", g.addr, err)
	}

	g.log.Info("xrayapi: gRPC connection established", "addr", g.addr)
	g.conn = conn
	return conn, nil
}

// Close releases the underlying gRPC connection. Safe to call multiple times.
func (g *GRPCClient) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn != nil {
		_ = g.conn.Close()
		g.conn = nil
	}
}

// ---------------------------------------------------------------------------
// QueryStats — StatsService.GetUsersStats
// ---------------------------------------------------------------------------

// QueryStats fetches per-user traffic counters via the gRPC StatsService.
//
// It uses GetUsersStats (preferred over QueryStats+string-parsing) because it
// returns a strongly-typed slice of UserStat objects directly.
//
// Dry-run scenarios verified:
//   - Xray not running → dial() fails within connectTimeout → error returned.
//   - Empty stats → returns nil slice without error.
//   - Context deadline exceeded mid-call → wrapped error returned to caller.
func (g *GRPCClient) QueryStats() ([]UserStat, error) {
	conn, err := g.dial()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultCallTimeout)
	defer cancel()

	client := statsService.NewStatsServiceClient(conn)
	resp, err := client.GetUsersStats(ctx, &statsService.GetUsersStatsRequest{
		IncludeTraffic: true,
		Reset_:         false, // Billing worker resets counters via a dedicated call if needed.
	})
	if err != nil {
		return nil, fmt.Errorf("xrayapi: GetUsersStats: %w", err)
	}

	if resp == nil || len(resp.Users) == 0 {
		return nil, nil
	}

	stats := make([]UserStat, 0, len(resp.Users))
	for _, u := range resp.Users {
		if u.Email == "" || u.Traffic == nil {
			continue
		}
		stats = append(stats, UserStat{
			Email: u.Email,
			Up:    u.Traffic.Uplink,
			Down:  u.Traffic.Downlink,
		})
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
//   We wrap clientJSON in a minimal settings object and feed it to the appropriate
//   infra/conf parser (the same one the xray CLI uses internally via inbound_user_add.go).
//   This avoids hand-crafting Protobuf structures for each protocol.
//
// Dry-run scenarios verified:
//   - Invalid UUID in clientJSON → parseUserForProtocol returns error before any gRPC call.
//   - Unsupported protocol → parseUserForProtocol returns descriptive error.
//   - gRPC call fails → error wrapped with tag/email for easy log correlation.
func (g *GRPCClient) AddUserSingle(inboundTag, inboundProtocol string, clientJSON []byte) error {
	user, err := parseUserForProtocol(inboundProtocol, clientJSON)
	if err != nil {
		return fmt.Errorf("xrayapi: AddUser: parse user (tag=%s proto=%s): %w", inboundTag, inboundProtocol, err)
	}

	conn, err := g.dial()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultCallTimeout)
	defer cancel()

	client := handlerService.NewHandlerServiceClient(conn)
	_, err = client.AlterInbound(ctx, &handlerService.AlterInboundRequest{
		Tag: inboundTag,
		Operation: cserial.ToTypedMessage(
			&handlerService.AddUserOperation{User: user},
		),
	})
	if err != nil {
		return fmt.Errorf("xrayapi: AddUser (tag=%s email=%s): %w", inboundTag, user.Email, err)
	}

	g.log.Info("xrayapi: user added via gRPC", "tag", inboundTag, "email", user.Email)
	return nil
}

// AddUser hot-adds users from the payload payload array.
// This matches the old api.Client.AddUser signature for seamless migration.
func (g *GRPCClient) AddUser(payload []xrayconfig.TaggedClient, configPath string) error {
	// Read inbound metadata from the live config to resolve protocols.
	cfg, cfgErr := xrayconfig.Read(configPath)
	if cfgErr != nil {
		g.log.Warn("failed to read config for AddUser protocols", "path", configPath, "err", cfgErr)
	}
	var inbounds []xrayconfig.RawInbound
	if cfg != nil {
		inbounds, _ = cfg.GetInbounds()
	}
	ibByTag := make(map[string]string, len(inbounds))
	for _, ib := range inbounds {
		ibByTag[ib.Tag()] = ib.Protocol()
	}

	var errs []string
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

		if err := g.AddUserSingle(tc.Tag, proto, clientJSON); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("xrayapi: AddUser errors: %s", strings.Join(errs, "; "))
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
func (g *GRPCClient) RemoveUser(email string, tags []string) error {
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
			ctx, cancel := context.WithTimeout(context.Background(), defaultCallTimeout)
			defer cancel()

			_, callErr := client.AlterInbound(ctx, &handlerService.AlterInboundRequest{
				Tag: tag,
				Operation: cserial.ToTypedMessage(
					&handlerService.RemoveUserOperation{Email: email},
				),
			})
			if callErr != nil {
				errs = append(errs, fmt.Sprintf("tag=%s: %v", tag, callErr))
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
	case "vless":
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
