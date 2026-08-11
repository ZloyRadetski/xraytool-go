package engine_xray

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

type templateEngineStub struct {
	domain.Engine
	snapshot []domain.VPNUserConfig
}

func (s *templateEngineStub) TemplateUserSnapshot(_ context.Context, _ []domain.VPNUserConfig) ([]domain.VPNUserConfig, error) {
	return append([]domain.VPNUserConfig(nil), s.snapshot...), nil
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

func TestPluginExposesUnderlyingTemplateUserSnapshotter(t *testing.T) {
	engine := &templateEngineStub{
		snapshot: []domain.VPNUserConfig{{Email: "ops@example.test", UUID: "static-id"}},
	}
	p := NewFromEngine(engine)

	require.True(t, p.SupportsTemplateUserSnapshot())
	snapshot, err := p.TemplateUserSnapshot(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, engine.snapshot, snapshot)
}
