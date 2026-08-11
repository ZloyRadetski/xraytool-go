package engine_xray

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

type staticEngineStub struct {
	domain.Engine
	snapshot []domain.StaticInboundClients
	applied  []domain.StaticInboundClients
}

func (s *staticEngineStub) StaticClientSnapshot(_ context.Context, _ []domain.VPNUserConfig) ([]domain.StaticInboundClients, error) {
	return append([]domain.StaticInboundClients(nil), s.snapshot...), nil
}

func (s *staticEngineStub) ApplyStaticClientSnapshot(_ context.Context, clients []domain.StaticInboundClients) error {
	s.applied = append([]domain.StaticInboundClients(nil), clients...)
	return nil
}

func TestNewFromEngine_ReusesKernelEngine(t *testing.T) {
	engine := &NoopEngine{}
	p := NewFromEngine(engine)

	require.NoError(t, p.Init(context.Background(), nil, nil))
	require.NoError(t, p.Health(context.Background()))
	require.Same(t, engine, p.DomainEngine())
	require.NoError(t, p.AddUser(context.Background(), pluginapi.VPNUserConfig{
		Email: "user@example.test",
		UUID:  "uuid",
	}))
	require.NotNil(t, p.PublishedServices()["engine.softban"])
}

func TestPluginExposesUnderlyingStaticClientSynchronizer(t *testing.T) {
	engine := &staticEngineStub{
		snapshot: []domain.StaticInboundClients{{InboundTag: "vless-main", Protocol: "vless", Clients: []byte("[]")}},
	}
	p := NewFromEngine(engine)

	require.True(t, p.SupportsStaticClientSync())
	snapshot, err := p.StaticClientSnapshot(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, engine.snapshot, snapshot)

	desired := []domain.StaticInboundClients{{InboundTag: "vless-main", Protocol: "vless", Clients: []byte(`[{"email":"manual"}]`)}}
	require.NoError(t, p.ApplyStaticClientSnapshot(context.Background(), desired))
	require.Equal(t, desired, engine.applied)
}
