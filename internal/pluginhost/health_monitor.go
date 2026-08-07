package pluginhost

import (
	"context"
	"time"
)

const defaultHealthCheckTimeout = 5 * time.Second

// StartHealthMonitor periodically invokes HealthCheck until ctx is cancelled.
// HealthCheck contains the restart policy for external subprocesses, so this
// is the production lifecycle hook that turns a failed liveness probe into a
// bounded recovery attempt rather than a passive diagnostic only.
//
// Calling it more than once is safe but intentionally creates independent
// monitors; composition roots should start exactly one monitor per Host.
func (h *Host) StartHealthMonitor(ctx context.Context, interval time.Duration) {
	if h == nil || ctx == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				timeout := defaultHealthCheckTimeout
				if interval < timeout {
					timeout = interval
				}
				checkCtx, cancel := context.WithTimeout(ctx, timeout)
				results := h.HealthCheck(checkCtx)
				cancel()
				for name, err := range results {
					if err != nil {
						h.log.Warn("[pluginhost] plugin health check failed", "plugin", name, "error", err)
					}
				}
			}
		}
	}()
}
