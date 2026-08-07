package pluginhost_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
)

type notificationStub struct {
	name     string
	channels []string
}

func (s *notificationStub) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{Name: s.name, Kind: "notification", APIVersion: "1"}
}
func (s *notificationStub) Init(context.Context, pluginapi.RawConfig, pluginapi.ServiceResolver) error {
	return nil
}
func (s *notificationStub) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (s *notificationStub) Stop(context.Context) error   { return nil }
func (s *notificationStub) Health(context.Context) error { return nil }
func (s *notificationStub) Channels() []string           { return s.channels }
func (s *notificationStub) Send(context.Context, pluginapi.Notification) error {
	return nil
}

var _ pluginapi.NotificationProvider = (*notificationStub)(nil)

func TestHostNotificationProviders_FiltersByChannel(t *testing.T) {
	core := makeCorePlugin()
	email := &notificationStub{name: "email", channels: []string{"email"}}
	telegram := &notificationStub{name: "telegram", channels: []string{"telegram", " EMAIL "}}

	host := pluginhost.New(pluginhost.PluginsConfig{
		"core":     {Enabled: true, Source: "builtin"},
		"email":    {Enabled: true, Source: "builtin"},
		"telegram": {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core":     func() pluginapi.Plugin { return core },
		"email":    func() pluginapi.Plugin { return email },
		"telegram": func() pluginapi.Plugin { return telegram },
	}, nil)

	require.NoError(t, host.Load(context.Background()))
	t.Cleanup(func() { _ = host.Shutdown(context.Background()) })

	providers := host.NotificationProviders("email")
	require.Len(t, providers, 2)
	require.Same(t, email, providers[0])
	require.Same(t, telegram, providers[1])
	require.Empty(t, host.NotificationProviders("sms"))
	require.Empty(t, host.NotificationProviders("  "))
}
