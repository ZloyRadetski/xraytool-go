// Package pluginrpc is the public transport contract for xraytool external
// plugins.
//
// It deliberately does not expose pluginapi's Go interfaces. Those interfaces
// contain in-process values such as *http.Request and repositories and cannot
// be safely or honestly transported across a process boundary. External
// plugins communicate through the versioned, structured Call API below.
//
// The package is usable by a separately built Go plugin. Non-Go plugins must
// implement the go-plugin handshake and the gRPC service described in
// proto/xraytool/plugin/v1/plugin.proto.
package pluginrpc

import (
	"context"
	"fmt"
)

const (
	// ProtocolVersion is the go-plugin transport protocol version. It changes
	// only when the go-plugin handshake or the gRPC service shape changes.
	ProtocolVersion = 1

	// PluginName is the single go-plugin dispense key exposed by an external
	// xraytool plugin process.
	PluginName = "xraytool"
)

// ServiceRef declares an external plugin dependency or publication. It mirrors
// the serializable fields of pluginapi.ServiceRef without importing an internal
// package, so this package can be used by an independently versioned SDK.
type ServiceRef struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional,omitempty"`
}

// Metadata is returned by an external plugin before it is initialised. Host
// validates it in exactly the same way as metadata from a builtin plugin.
//
// Capabilities contains transport-level, JSON-compatible values. Currently
// supported conventional keys are:
//   - "method_id" (string) for payment-like providers
//   - "channels" ([]string) for notification providers
//
// Capabilities are informational unless a documented adapter consumes them.
type Metadata struct {
	Name         string         `json:"name"`
	Kind         string         `json:"kind"`
	Version      string         `json:"version"`
	APIVersion   string         `json:"api_version"`
	Description  string         `json:"description,omitempty"`
	Mandatory    bool           `json:"mandatory,omitempty"`
	Publishes    []ServiceRef   `json:"publishes,omitempty"`
	Requires     []ServiceRef   `json:"requires,omitempty"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
}

// CallRequest invokes one explicitly versioned operation on an external
// plugin or a host service proxy. Payload must be JSON-compatible.
//
// Service and Method are part of the protocol, rather than method names being
// inferred from a Go interface. This makes unsupported services fail clearly
// instead of pretending arbitrary values are RPC-compatible.
type CallRequest struct {
	Service string         `json:"service"`
	Method  string         `json:"method"`
	Payload map[string]any `json:"payload,omitempty"`
}

// CallResponse is the structured response from a CallRequest.
type CallResponse struct {
	Payload map[string]any `json:"payload,omitempty"`
}

// ServiceCaller is supplied to an external plugin during Init. It can invoke
// only services declared in Metadata.Requires and only when the host has a
// concrete ServiceHandler for the service. It is not a generic object bridge.
type ServiceCaller interface {
	Call(ctx context.Context, req CallRequest) (CallResponse, error)
}

// ServiceHandler is implemented by a host-side service that intentionally
// supports the external structured transport. Repositories and arbitrary Go
// interfaces do not implement it by default.
//
// A host may adapt a narrow, audited subset of a local service with
// ServiceHandlerFunc. This is the only supported way to make a service
// available to an external plugin.
type ServiceHandler interface {
	Call(ctx context.Context, req CallRequest) (CallResponse, error)
}

// ServiceHandlerFunc adapts a function into ServiceHandler.
type ServiceHandlerFunc func(context.Context, CallRequest) (CallResponse, error)

func (f ServiceHandlerFunc) Call(ctx context.Context, req CallRequest) (CallResponse, error) {
	return f(ctx, req)
}

// InitRequest is passed to Implementation.Init. Services is nil when the
// plugin has no serializable required services.
type InitRequest struct {
	Config   map[string]any
	Services ServiceCaller
}

// Implementation is the SDK interface implemented by an external plugin.
//
// Start follows the same contract as pluginapi.Plugin.Start: it blocks until
// ctx is cancelled, then returns promptly. Stop is called independently for
// graceful cleanup before the host terminates the subprocess.
type Implementation interface {
	Describe(ctx context.Context) (Metadata, error)
	Init(ctx context.Context, req InitRequest) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Health(ctx context.Context) error
	Call(ctx context.Context, req CallRequest) (CallResponse, error)
}

// ValidateCallRequest rejects ambiguous transport calls before they reach a
// plugin implementation.
func ValidateCallRequest(req CallRequest) error {
	if req.Service == "" {
		return fmt.Errorf("pluginrpc: call service must not be empty")
	}
	if req.Method == "" {
		return fmt.Errorf("pluginrpc: call method must not be empty")
	}
	return nil
}
