package vpn

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"testing"

	handlerService "github.com/xtls/xray-core/app/proxyman/command"
	statsService "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
)

// ---------------------------------------------------------------------------
// Minimal mock gRPC servers
// ---------------------------------------------------------------------------

// mockStatsServer implements StatsServiceServer with configurable responses.
type mockStatsServer struct {
	statsService.UnimplementedStatsServiceServer
	users []*statsService.UserStat
	err   error
}

func (m *mockStatsServer) GetUsersStats(_ context.Context, _ *statsService.GetUsersStatsRequest) (*statsService.GetUsersStatsResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &statsService.GetUsersStatsResponse{Users: m.users}, nil
}

func (m *mockStatsServer) QueryStats(_ context.Context, _ *statsService.QueryStatsRequest) (*statsService.QueryStatsResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	var stats []*statsService.Stat
	for _, u := range m.users {
		if u.Traffic != nil {
			stats = append(stats, &statsService.Stat{
				Name:  fmt.Sprintf("user>>>%s>>>traffic>>>uplink", u.Email),
				Value: u.Traffic.Uplink,
			})
			stats = append(stats, &statsService.Stat{
				Name:  fmt.Sprintf("user>>>%s>>>traffic>>>downlink", u.Email),
				Value: u.Traffic.Downlink,
			})
		}
	}
	return &statsService.QueryStatsResponse{Stat: stats}, nil
}

// mockHandlerServer implements HandlerServiceServer with configurable responses.
type mockHandlerServer struct {
	handlerService.UnimplementedHandlerServiceServer
	err error
	// last received request for assertion
	lastAlterReq *handlerService.AlterInboundRequest
}

func (m *mockHandlerServer) AlterInbound(_ context.Context, req *handlerService.AlterInboundRequest) (*handlerService.AlterInboundResponse, error) {
	m.lastAlterReq = req
	if m.err != nil {
		return nil, m.err
	}
	return &handlerService.AlterInboundResponse{}, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// startMockGRPCServer starts a local gRPC server on a random port and returns
// the address, the server instance, and a cleanup function.
// Both Stats and Handler services are registered on the same server.
func startMockGRPCServer(t *testing.T, statsSrv *mockStatsServer, handlerSrv *mockHandlerServer) (addr string, cleanup func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	if statsSrv != nil {
		statsService.RegisterStatsServiceServer(s, statsSrv)
	}
	if handlerSrv != nil {
		handlerService.RegisterHandlerServiceServer(s, handlerSrv)
	}
	go func() { _ = s.Serve(lis) }()
	return lis.Addr().String(), func() {
		s.Stop()
		_ = lis.Close()
	}
}

// newTestGRPCClient creates a GRPCClient connected to addr with a nop logger.
func newTestGRPCClient(addr string) *GRPCClient {
	return NewGRPCClient(addr, slog.Default())
}

// ---------------------------------------------------------------------------
// QueryStats tests
// ---------------------------------------------------------------------------

func TestGRPCClient_QueryStats_Success(t *testing.T) {
	statsSrv := &mockStatsServer{
		users: []*statsService.UserStat{
			{
				Email:   "alice@example.com",
				Traffic: &statsService.TrafficUserStat{Uplink: 100, Downlink: 200},
			},
			{
				Email:   "bob@example.com",
				Traffic: &statsService.TrafficUserStat{Uplink: 50, Downlink: 150},
			},
		},
	}
	addr, cleanup := startMockGRPCServer(t, statsSrv, nil)
	defer cleanup()

	c := newTestGRPCClient(addr)
	defer c.Close()

	stats, err := c.QueryStats(context.Background())
	if err != nil {
		t.Fatalf("QueryStats: unexpected error: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 stats, got %d", len(stats))
	}

	byEmail := make(map[string]UserStat)
	for _, s := range stats {
		byEmail[s.Email] = s
	}

	if s, ok := byEmail["alice@example.com"]; !ok || s.Up != 100 || s.Down != 200 {
		t.Errorf("alice stats wrong: %+v", byEmail["alice@example.com"])
	}
	if s, ok := byEmail["bob@example.com"]; !ok || s.Up != 50 || s.Down != 150 {
		t.Errorf("bob stats wrong: %+v", byEmail["bob@example.com"])
	}
}

func TestGRPCClient_QueryStats_Empty(t *testing.T) {
	statsSrv := &mockStatsServer{users: nil}
	addr, cleanup := startMockGRPCServer(t, statsSrv, nil)
	defer cleanup()

	c := newTestGRPCClient(addr)
	defer c.Close()

	stats, err := c.QueryStats(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected empty stats, got %d", len(stats))
	}
}

func TestGRPCClient_QueryStats_SkipsUsersWithoutTraffic(t *testing.T) {
	statsSrv := &mockStatsServer{
		users: []*statsService.UserStat{
			{Email: "valid@example.com", Traffic: &statsService.TrafficUserStat{Uplink: 1, Downlink: 2}},
			{Email: "notraffic@example.com", Traffic: nil},
			{Email: "", Traffic: &statsService.TrafficUserStat{Uplink: 999}},
		},
	}
	addr, cleanup := startMockGRPCServer(t, statsSrv, nil)
	defer cleanup()

	c := newTestGRPCClient(addr)
	defer c.Close()

	stats, err := c.QueryStats(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only "valid@example.com" should be returned; others are skipped.
	if len(stats) != 1 || stats[0].Email != "valid@example.com" {
		t.Errorf("expected only valid@example.com, got %+v", stats)
	}
}

func TestGRPCClient_QueryStats_Unreachable(t *testing.T) {
	// Port 1 is conventionally unused/refused.
	c := newTestGRPCClient("127.0.0.1:1")
	defer c.Close()

	_, err := c.QueryStats(context.Background())
	if err == nil {
		t.Fatal("expected error when server is unreachable, got nil")
	}
}

// ---------------------------------------------------------------------------
// RemoveUser tests
// ---------------------------------------------------------------------------

func TestGRPCClient_RemoveUser_Success(t *testing.T) {
	handlerSrv := &mockHandlerServer{}
	addr, cleanup := startMockGRPCServer(t, nil, handlerSrv)
	defer cleanup()

	c := newTestGRPCClient(addr)
	defer c.Close()

	err := c.RemoveUser(context.Background(), "test@example.com", []string{"vless-tag", "vmess-tag"})
	if err != nil {
		t.Fatalf("RemoveUser: unexpected error: %v", err)
	}
}

func TestGRPCClient_RemoveUser_EmptyTags(t *testing.T) {
	c := newTestGRPCClient("127.0.0.1:1") // won't actually connect
	if err := c.RemoveUser(context.Background(), "test@example.com", nil); err != nil {
		t.Errorf("expected nil for empty tags, got %v", err)
	}
}

func TestGRPCClient_RemoveUser_EmptyEmail(t *testing.T) {
	c := newTestGRPCClient("127.0.0.1:1") // won't actually connect
	err := c.RemoveUser(context.Background(), "", []string{"tag1"})
	if err == nil {
		t.Error("expected error for empty email, got nil")
	}
}

func TestGRPCClient_RemoveUser_ServerError(t *testing.T) {
	handlerSrv := &mockHandlerServer{err: fmt.Errorf("mock remove error")}
	addr, cleanup := startMockGRPCServer(t, nil, handlerSrv)
	defer cleanup()

	c := newTestGRPCClient(addr)
	defer c.Close()

	if err := c.RemoveUser(context.Background(), "test@example.com", []string{"tag1"}); err == nil {
		t.Fatal("expected error from server, got nil")
	}
}

// ---------------------------------------------------------------------------
// parseUserForProtocol tests (unit, no gRPC needed)
// ---------------------------------------------------------------------------

func TestParseUserForProtocol_VLESS(t *testing.T) {
	clientJSON := []byte(`{"id":"a3482e88-686a-4a58-8126-99c9df64b7bf","email":"vless@test.com","flow":"xtls-rprx-vision"}`)
	user, err := parseUserForProtocol("vless", clientJSON)
	if err != nil {
		t.Fatalf("parseUserForProtocol vless: %v", err)
	}
	if user.Email != "vless@test.com" {
		t.Errorf("expected email vless@test.com, got %s", user.Email)
	}
}

func TestParseUserForProtocol_VLESS_InvalidUUID(t *testing.T) {
	// Xray accepts strings 1-30 chars and hashes them to a UUID.
	// Empty string or >30 (but not 32-36) will fail UUID parsing.
	clientJSON := []byte(`{"id":"","email":"bad@test.com"}`)
	_, err := parseUserForProtocol("vless", clientJSON)
	if err == nil {
		t.Fatal("expected error for invalid UUID, got nil")
	}
}

func TestParseUserForProtocol_VMess(t *testing.T) {
	clientJSON := []byte(`{"id":"a3482e88-686a-4a58-8126-99c9df64b7bf","email":"vmess@test.com","alterId":0}`)
	user, err := parseUserForProtocol("vmess", clientJSON)
	if err != nil {
		t.Fatalf("parseUserForProtocol vmess: %v", err)
	}
	if user.Email != "vmess@test.com" {
		t.Errorf("expected email vmess@test.com, got %s", user.Email)
	}
}

func TestParseUserForProtocol_Unsupported(t *testing.T) {
	_, err := parseUserForProtocol("hysteria2", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for unsupported protocol, got nil")
	}
}

func TestParseUserForProtocol_EmptyJSON(t *testing.T) {
	_, err := parseUserForProtocol("vless", []byte(`{}`))
	// Empty JSON should fail because there's no valid UUID.
	if err == nil {
		t.Fatal("expected error for empty client JSON, got nil")
	}
}

func TestGRPCClient_AddUser_Success(t *testing.T) {
	handlerSrv := &mockHandlerServer{}
	addr, cleanup := startMockGRPCServer(t, nil, handlerSrv)
	defer cleanup()

	c := newTestGRPCClient(addr)
	defer c.Close()

	clientJSON := []byte(`{"id":"a3482e88-686a-4a58-8126-99c9df64b7bf","email":"add@test.com"}`)
	err := c.AddUserSingle(context.Background(), "inbound-vless", "vless", clientJSON)
	if err != nil {
		t.Fatalf("AddUser: unexpected error: %v", err)
	}

	if handlerSrv.lastAlterReq == nil || handlerSrv.lastAlterReq.Tag != "inbound-vless" {
		t.Errorf("AddUser: lastAlterReq tag mismatch or nil: %+v", handlerSrv.lastAlterReq)
	}
}

func TestGRPCClient_AddUser_InvalidProtocol(t *testing.T) {
	c := newTestGRPCClient("127.0.0.1:1")
	if err := c.AddUserSingle(context.Background(), "tag1", "invalid_proto", []byte(`{}`)); err == nil {
		t.Fatal("expected error for invalid protocol, got nil")
	}
}

func TestGRPCClient_AddUser_ServerError(t *testing.T) {
	handlerSrv := &mockHandlerServer{err: fmt.Errorf("mock add error")}
	addr, cleanup := startMockGRPCServer(t, nil, handlerSrv)
	defer cleanup()

	c := newTestGRPCClient(addr)
	defer c.Close()

	clientJSON := []byte(`{"id":"a3482e88-686a-4a58-8126-99c9df64b7bf","email":"add@test.com"}`)
	err := c.AddUserSingle(context.Background(), "inbound-vless", "vless", clientJSON)
	if err == nil {
		t.Fatal("expected error from server, got nil")
	}
}

func TestParseUserForProtocol_Trojan(t *testing.T) {
	clientJSON := []byte(`{"password":"test-password","email":"trojan@test.com"}`)
	user, err := parseUserForProtocol("trojan", clientJSON)
	if err != nil {
		t.Fatalf("parseUserForProtocol trojan: %v", err)
	}
	if user.Email != "trojan@test.com" {
		t.Errorf("expected email trojan@test.com, got %s", user.Email)
	}
}

func TestParseUserForProtocol_Shadowsocks(t *testing.T) {
	clientJSON := []byte(`{"method":"aes-128-gcm","password":"test-password","email":"ss@test.com"}`)
	user, err := parseUserForProtocol("shadowsocks", clientJSON)
	if err != nil {
		t.Fatalf("parseUserForProtocol shadowsocks: %v", err)
	}
	if user.Email != "ss@test.com" {
		t.Errorf("expected email ss@test.com, got %s", user.Email)
	}
}

func TestGRPCClient_ConnectionCaching(t *testing.T) {
	handlerSrv := &mockHandlerServer{}
	addr, cleanup := startMockGRPCServer(t, nil, handlerSrv)
	defer cleanup()

	c := newTestGRPCClient(addr)
	defer c.Close()

	globalConnsMu.Lock()
	conn0 := globalConns[addr]
	globalConnsMu.Unlock()
	if conn0 != nil {
		t.Fatal("expected global conn to be nil initially")
	}

	if err := c.RemoveUser(context.Background(), "test@example.com", []string{"tag1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	globalConnsMu.Lock()
	conn1 := globalConns[addr]
	globalConnsMu.Unlock()
	if conn1 == nil {
		t.Fatal("expected global conn to be initialized")
	}

	err := c.RemoveUser(context.Background(), "test@example.com", []string{"tag2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	globalConnsMu.Lock()
	conn2 := globalConns[addr]
	globalConnsMu.Unlock()
	if conn2 != conn1 {
		t.Fatal("expected global conn to be reused")
	}

	// Force close and delete to simulate broken connection
	globalConnsMu.Lock()
	_ = conn2.Close()
	delete(globalConns, addr)
	globalConnsMu.Unlock()

	err = c.RemoveUser(context.Background(), "test@example.com", []string{"tag3"})
	if err != nil {
		t.Fatalf("unexpected error after close/reconnect: %v", err)
	}

	globalConnsMu.Lock()
	conn3 := globalConns[addr]
	globalConnsMu.Unlock()
	if conn3 == nil || conn3 == conn1 {
		t.Fatal("expected new conn to be initialized after close")
	}
}
