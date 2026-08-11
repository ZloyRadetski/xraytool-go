package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"xraytool/internal/appconfig"
	"xraytool/internal/pluginapi"
)

type notificationProviderStub struct {
	name string
	sent chan pluginapi.Notification
}

func (s *notificationProviderStub) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{Name: s.name, Kind: "notification", APIVersion: "1"}
}
func (s *notificationProviderStub) Init(context.Context, pluginapi.RawConfig, pluginapi.ServiceResolver) error {
	return nil
}
func (s *notificationProviderStub) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (s *notificationProviderStub) Stop(context.Context) error   { return nil }
func (s *notificationProviderStub) Health(context.Context) error { return nil }
func (s *notificationProviderStub) Channels() []string           { return []string{"email"} }
func (s *notificationProviderStub) Send(_ context.Context, n pluginapi.Notification) error {
	s.sent <- n
	return nil
}

var _ pluginapi.NotificationProvider = (*notificationProviderStub)(nil)

func TestSendOTPNotification_UsesInjectedProviders(t *testing.T) {
	provider := &notificationProviderStub{name: "email-test", sent: make(chan pluginapi.Notification, 1)}
	router := (&Router{log: slog.Default()}).WithNotificationProviders(provider)

	if !router.sendOTPNotification("email", "person@example.test", "123456") {
		t.Fatal("expected an injected provider to accept email OTP delivery")
	}

	select {
	case notification := <-provider.sent:
		require.Equal(t, "email", notification.Channel)
		require.Equal(t, "person@example.test", notification.To)
		require.Equal(t, "otp_code", notification.Kind)
		require.Equal(t, "123456", notification.Payload["code"])
	case <-time.After(time.Second):
		t.Fatal("notification provider was not called")
	}
	router.bgTasks.Wait()
}

func TestNewWithOptions_UsesNoLegacyMailerFallback(t *testing.T) {
	cfg := &appconfig.Config{Mailer: appconfig.MailerConf{
		Enabled:      true,
		ResendAPIKey: "test-key",
		FromEmail:    "noreply@example.test",
	}}
	router := NewWithOptions(cfg, "key", nil, nil, nil, nil, slog.Default(), nil, Options{DisableLegacyMailer: true})
	require.Empty(t, router.notificationProviders)
}

func TestSendOTPNotificationWithoutProviderDoesNotFallBackToLogs(t *testing.T) {
	router := &Router{log: slog.Default()}
	if router.sendOTPNotification("email", "person@example.test", "123456") {
		t.Fatal("OTP delivery without a provider must fail closed")
	}
}
