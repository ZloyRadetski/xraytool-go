package pluginhost

import (
	"context"
	"time"
)

const defaultHealthCheckTimeout = 5 * time.Second

// StartHealthMonitor periodically invokes HealthCheck until ctx is cancelled
// or Host.Shutdown begins. The host owns the derived context so a monitor
// cannot outlive the plugin lifecycle when its caller supplied a long-lived
// application context.
// HealthCheck contains the restart policy for external subprocesses, so this
// is the production lifecycle hook that turns a failed liveness probe into a
// bounded recovery attempt rather than a passive diagnostic only.
//
// Calling it more than once is safe; each monitor is stopped by Shutdown.
func (h *Host) StartHealthMonitor(ctx context.Context, interval time.Duration) {
	if h == nil || ctx == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	monitorCtx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	if h.state != hostRunning {
		h.mu.Unlock()
		cancel()
		return
	}
	h.healthCancels = append(h.healthCancels, cancel)
	h.healthMonitors.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.healthMonitors.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-monitorCtx.Done():
				return
			case <-ticker.C:
				timeout := defaultHealthCheckTimeout
				if interval < timeout {
					timeout = interval
				}
				checkCtx, checkCancel := context.WithTimeout(monitorCtx, timeout)
				results := h.HealthCheck(checkCtx)
				checkCancel()
				for name, err := range results {
					if err != nil {
						h.log.Warn("[pluginhost] plugin health check failed", "plugin", name, "error", err)
					}
				}
			}
		}
	}()
}
