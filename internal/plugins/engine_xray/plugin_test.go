package engine_xray

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
	
)

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
