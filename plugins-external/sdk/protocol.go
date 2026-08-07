// Package pluginrpc is the public Go SDK for xraytool external plugins.
//
// It is intentionally a standalone module: plugin binaries import this
// package instead of importing the xraytool application module or any of its
// internal packages. Its handshake and gRPC service are wire-compatible with
// xraytool/pluginrpc in the host process.
package pluginrpc

import (
	"context"
	"fmt"
)

const (
	// ProtocolVersion is the go-plugin transport protocol version. It changes
	// only when the handshake or gRPC service shape changes.
	ProtocolVersion = 1

	// PluginName is the single go-plugin dispense key exposed by an external
	// xraytool plugin process.
	PluginName = "xraytool"
)

// ServiceRef declares an external plugin dependency or publication. It
// mirrors the serializable portion of the host's service reference.
type ServiceRef struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional,omitempty"`
}

// Metadata is returned by an external plugin before it is initialised.
// Capabilities must contain JSON-compatible values.
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

// CallRequest invokes an explicitly versioned operation. Service, Method and
// Payload are structured rather than arbitrary Go interfaces, so plugins in
// separate processes and languages can use the same transport.
type CallRequest struct {
	Service string         `json:"service"`
	Method  string         `json:"method"`
	Payload map[string]any `json:"payload,omitempty"`
}

// CallResponse is the structured response to CallRequest.
type CallResponse struct {
	Payload map[string]any `json:"payload,omitempty"`
}

// ServiceCaller is supplied to a plugin during Init when it declares
// serializable required services. It can call only the services explicitly
// declared in Metadata.Requires and exposed by the host.
type ServiceCaller interface {
	Call(context.Context, CallRequest) (CallResponse, error)
}

// InitRequest is passed to Implementation.Init. Services is nil when the
// plugin has no serializable required services.
type InitRequest struct {
	Config   map[string]any
	Services ServiceCaller
}

// Implementation is implemented by an external plugin.
//
// Start must block until ctx is cancelled and then return promptly. Stop is
// invoked separately to allow graceful cleanup before the host exits the
// subprocess.
type Implementation interface {
	Describe(context.Context) (Metadata, error)
	Init(context.Context, InitRequest) error
	Start(context.Context) error
	Stop(context.Context) error
	Health(context.Context) error
	Call(context.Context, CallRequest) (CallResponse, error)
}

// ValidateCallRequest rejects ambiguous transport calls before they reach a
// plugin implementation or a host service proxy.
func ValidateCallRequest(req CallRequest) error {
	if req.Service == "" {
		return fmt.Errorf("pluginrpc: call service must not be empty")
	}
	if req.Method == "" {
		return fmt.Errorf("pluginrpc: call method must not be empty")
	}
	return nil
}
