package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	json "github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"xraytool/internal/domain"
)

type staticClientEngineStub struct {
	domain.Engine
	applied []domain.StaticInboundClients
}

func (s *staticClientEngineStub) StaticClientSnapshot(context.Context, []domain.VPNUserConfig) ([]domain.StaticInboundClients, error) {
	return nil, nil
}

func (s *staticClientEngineStub) ApplyStaticClientSnapshot(_ context.Context, inbounds []domain.StaticInboundClients) error {
	s.applied = append([]domain.StaticInboundClients(nil), inbounds...)
	return nil
}

var _ domain.StaticClientSynchronizer = (*staticClientEngineStub)(nil)

func TestHandleInternalXraySyncAppliesStaticClients(t *testing.T) {
	engine := &staticClientEngineStub{}
	router := &Router{engine: engine, log: slog.Default()}

	snapshot := []domain.StaticInboundClients{{
		InboundTag: "vless-main",
		Protocol:   "vless",
		Clients:    json.RawMessage(`[{"email":"ops","id":"static-id","custom":"kept"}]`),
	}}
	payload, err := json.Marshal(snapshot)
	require.NoError(t, err)

	body, err := json.Marshal(internalSyncRequest{Action: "sync-static-clients", Payload: string(payload)})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/xray/sync", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	router.handleInternalXraySync(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, snapshot, engine.applied)
}
