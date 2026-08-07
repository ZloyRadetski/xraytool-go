// antifraud_stub is a runnable reference external anti-fraud plugin.
//
// It tracks distinct source IPs per address and, after three observations,
// pushes a one-hour ban into the host-owned local cache. It intentionally uses
// only the v1 structured adapter and contains no database or engine-specific
// code, so it is a useful starting point for a real independently deployed
// provider.
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	pluginrpc "github.com/ZloyRadetski/xraytool-go/plugins-external/sdk"
)

const (
	antifraudService = "antifraud_provider"
	banUpdateSink   = "ban_update_sink"
)

func main() {
	pluginrpc.Serve(&antifraudStub{
		ips:  make(map[string]map[string]struct{}),
		bans: make(map[string]time.Time),
	})
}

type antifraudStub struct {
	mu       sync.Mutex
	services pluginrpc.ServiceCaller
	ips      map[string]map[string]struct{}
	bans     map[string]time.Time
}

func (*antifraudStub) Describe(context.Context) (pluginrpc.Metadata, error) {
	return pluginrpc.Metadata{
		Name:        "antifraud_stub",
		Kind:        "antifraud",
		Version:     "0.1.0",
		APIVersion:  "1",
		Description: "reference external anti-fraud provider with push-only bans",
		Publishes:   []pluginrpc.ServiceRef{{Name: antifraudService}},
	}, nil
}

func (p *antifraudStub) Init(_ context.Context, request pluginrpc.InitRequest) error {
	// The host provides this ServiceProxy automatically to an external plugin
	// which publishes antifraud_provider. It is intentionally not in Requires:
	// the provider can push decisions but cannot remotely read the host cache.
	if request.Services == nil {
		return errorsNew("ban_update_sink ServiceProxy is unavailable")
	}
	p.mu.Lock()
	p.services = request.Services
	p.mu.Unlock()
	return nil
}

func (*antifraudStub) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (*antifraudStub) Stop(context.Context) error      { return nil }
func (*antifraudStub) Health(context.Context) error    { return nil }

func (p *antifraudStub) Call(ctx context.Context, request pluginrpc.CallRequest) (pluginrpc.CallResponse, error) {
	if request.Service != antifraudService {
		return pluginrpc.CallResponse{}, fmt.Errorf("unsupported service %q", request.Service)
	}
	switch request.Method {
	case "force_unban":
		email := stringField(request.Payload, "email")
		if email == "" {
			return pluginrpc.CallResponse{}, errorsNew("force_unban requires email")
		}
		p.mu.Lock()
		delete(p.bans, email)
		services := p.services
		p.mu.Unlock()
		return pluginrpc.CallResponse{}, pushUnban(ctx, services, email)
	case "ingest_events":
		return pluginrpc.CallResponse{}, p.ingest(ctx, request.Payload)
	case "snapshot":
		return p.snapshot(), nil
	default:
		return pluginrpc.CallResponse{}, fmt.Errorf("unsupported antifraud_provider method %q", request.Method)
	}
}

func (p *antifraudStub) ingest(ctx context.Context, payload map[string]any) error {
	events, ok := payload["events"].([]any)
	if !ok {
		return errorsNew("ingest_events requires events array")
	}
	toBan := make(map[string]time.Time)
	p.mu.Lock()
	for _, raw := range events {
		event, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		email, ip := stringField(event, "email"), stringField(event, "ip")
		if email == "" || ip == "" {
			continue
		}
		if p.ips[email] == nil {
			p.ips[email] = make(map[string]struct{})
		}
		p.ips[email][ip] = struct{}{}
		if len(p.ips[email]) >= 3 {
			until := time.Now().UTC().Add(time.Hour)
			p.bans[email] = until
			toBan[email] = until
		}
	}
	services := p.services
	p.mu.Unlock()
	for email, until := range toBan {
		if err := pushBan(ctx, services, email, until); err != nil {
			return err
		}
	}
	return nil
}

func (p *antifraudStub) snapshot() pluginrpc.CallResponse {
	p.mu.Lock()
	activeBans := len(p.bans)
	trackedAddresses := len(p.ips)
	p.mu.Unlock()
	return pluginrpc.CallResponse{Payload: map[string]any{
		"active_bans":       activeBans,
		"tracked_addresses": trackedAddresses,
	}}
}

func pushBan(ctx context.Context, services pluginrpc.ServiceCaller, email string, until time.Time) error {
	if services == nil {
		return errorsNew("ban_update_sink ServiceProxy is unavailable")
	}
	_, err := services.Call(ctx, pluginrpc.CallRequest{
		Service: banUpdateSink,
		Method:  "push_ban_update",
		Payload: map[string]any{
			"email":        email,
			"banned_until": until.Format(time.RFC3339Nano),
		},
	})
	return err
}

func pushUnban(ctx context.Context, services pluginrpc.ServiceCaller, email string) error {
	if services == nil {
		return errorsNew("ban_update_sink ServiceProxy is unavailable")
	}
	_, err := services.Call(ctx, pluginrpc.CallRequest{
		Service: banUpdateSink,
		Method:  "push_unban",
		Payload: map[string]any{"email": email},
	})
	return err
}

func stringField(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func errorsNew(message string) error { return fmt.Errorf("antifraud_stub: %s", message) }
