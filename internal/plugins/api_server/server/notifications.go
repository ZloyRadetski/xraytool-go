package server

import (
	"context"
	"strings"

	"xraytool/internal/pluginapi"
)

// sendOTPNotification performs delivery outside the request path. The caller
// receives its HTTP response immediately, as it did with the legacy sender.
// Router.Shutdown waits for any delivery already started by a request.
func (r *Router) sendOTPNotification(channel, recipient, code string) bool {
	providers := r.notificationProvidersFor(channel)
	if len(providers) == 0 {
		return false
	}

	r.bgTasks.Add(1)
	go func() {
		defer r.bgTasks.Done()

		notification := pluginapi.Notification{
			Channel: channel,
			To:      recipient,
			Kind:    "otp_code",
			Payload: map[string]any{"code": code},
		}

		for _, provider := range providers {
			if err := provider.Send(context.Background(), notification); err != nil {
				r.log.Error("request_code: notification provider failed", "channel", channel, "provider", provider.Metadata().Name, "err", err)
			}
		}
	}()
	return true
}

func (r *Router) hasNotificationProvider(channel string) bool {
	return len(r.notificationProvidersFor(channel)) > 0
}

func (r *Router) notificationProvidersFor(channel string) []pluginapi.NotificationProvider {
	providers := make([]pluginapi.NotificationProvider, 0, len(r.notificationProviders))
	for _, provider := range r.notificationProviders {
		for _, supported := range provider.Channels() {
			if strings.EqualFold(strings.TrimSpace(supported), channel) {
				providers = append(providers, provider)
				break
			}
		}
	}
	return providers
}
