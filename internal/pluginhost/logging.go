package pluginhost

// WithExternalLogsDisabled prevents the host from retaining or persisting
// stdout/stderr emitted by external plugins. The subprocesses still receive
// working writers, so disabling observability cannot make them fail to start.
func WithExternalLogsDisabled(disabled bool) HostOption {
	return func(h *Host) {
		h.externalLogsDisabled = disabled
	}
}
