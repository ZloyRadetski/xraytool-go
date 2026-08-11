package server

import (
	"context"

	"xraytool/internal/pluginapi"
)

// sendOTPNotification performs delivery outside the request path. The caller
// receives its HTTP response immediately, as it did with the legacy sender.
// Router.Shutdown waits for any delivery already started by a request.
func (r *Router) sendOTPNotification(email, code string) {
	r.bgTasks.Add(1)
	go func() {
		defer r.bgTasks.Done()

		notification := pluginapi.Notification{
			Channel: "email",
			To:      email,
			Kind:    "otp_code",
			Payload: map[string]any{"code": code},
		}

		if len(r.notificationProviders) > 0 {
			for _, provider := range r.notificationProviders {
				if err := provider.Send(context.Background(), notification); err != nil {
					r.log.Error("request_code: notification provider failed", "email", email, "provider", provider.Metadata().Name, "err", err)
				}
			}
			return
		}

		// Fallback for development: log the code instead of sending an email.
		// Production delivery is exclusively a NotificationProvider plugin.
		r.log.Warn("request_code: no notification provider configured, logging code for debug", "email", email, "code", code)
	}()
}
