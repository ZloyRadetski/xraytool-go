package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"xraytool/internal/pluginapi"
	"xraytool/pluginrpc"
)

// The antifraud wire adapter deliberately has a very small reverse surface.
// The external process may push decisions through its brokered ServiceProxy,
// but cannot ask the host whether an address is currently banned.  That keeps
// subscription checks entirely in-process even when the provider is remote.
const (
	externalAntifraudService     = "antifraud_provider"
	externalBanUpdateSinkService = "ban_update_sink"

	externalBanPushMethod  = "push_ban_update"
	externalBanUnbanMethod = "push_unban"
)

// externalBanState is the local read model owned by an external antifraud
// adapter.  It mirrors every accepted reverse update into the Host's
// LocalBanCache (once SetBanSink has been called), but retains its own map so
// IsBanned can never require a type assertion on the generic BanUpdateSink or
// make an RPC call.
type externalBanState struct {
	mu   sync.RWMutex
	bans map[string]time.Time
	sink pluginapi.BanUpdateSink
	now  func() time.Time
}

func newExternalBanState() *externalBanState {
	return &externalBanState{
		bans: make(map[string]time.Time),
		now:  time.Now,
	}
}

func (s *externalBanState) clock() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *externalBanState) setSink(sink pluginapi.BanUpdateSink) {
	if s == nil {
		return
	}
	now := s.clock()
	s.mu.Lock()
	s.sink = sink
	updates := make(map[string]time.Time, len(s.bans))
	for email, until := range s.bans {
		if !until.After(now) {
			delete(s.bans, email)
			continue
		}
		updates[email] = until
	}
	s.mu.Unlock()

	// Init is allowed to restore bans before Host has installed the sink.  Replay
	// the live snapshot now so Host.BanCache and the adapter converge without a
	// poll or a special startup RPC.
	if sink != nil {
		for email, until := range updates {
			sink.PushBanUpdate(email, until)
		}
	}
}

func (s *externalBanState) pushBan(email string, until time.Time) {
	if s == nil {
		return
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return
	}
	if !until.After(s.clock()) {
		s.pushUnban(email)
		return
	}
	s.mu.Lock()
	s.bans[email] = until
	sink := s.sink
	s.mu.Unlock()
	if sink != nil {
		sink.PushBanUpdate(email, until)
	}
}

func (s *externalBanState) pushUnban(email string) {
	if s == nil {
		return
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return
	}
	s.mu.Lock()
	delete(s.bans, email)
	sink := s.sink
	s.mu.Unlock()
	if sink != nil {
		sink.PushUnban(email)
	}
}

func (s *externalBanState) isBanned(email string) bool {
	if s == nil {
		return false
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return false
	}
	s.mu.RLock()
	until, ok := s.bans[email]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	if s.clock().Before(until) {
		return true
	}
	// Remove lazily, without racing a newer update.
	s.mu.Lock()
	if current, exists := s.bans[email]; exists && current.Equal(until) && !s.clock().Before(current) {
		delete(s.bans, email)
	}
	s.mu.Unlock()
	return false
}

func (p *externalPlugin) antifraudBanState() *externalBanState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.banState == nil {
		p.banState = newExternalBanState()
	}
	return p.banState
}

func publishesExternalAntifraud(metadata pluginapi.Metadata) bool {
	for _, publication := range metadata.Publishes {
		if publication.Name == externalAntifraudService {
			return true
		}
	}
	return false
}

// SetBanSink attaches the kernel cache. It never calls the external process:
// events flow from the provider to the host over the brokered sink, while hot
// reads flow only to the adapter's in-memory state.
func (p *externalPlugin) SetBanSink(sink pluginapi.BanUpdateSink) {
	p.antifraudBanState().setSink(sink)
}

// IsBanned is intentionally local and synchronous. Do not add a Call here:
// this method is used on the subscription hot path.
func (p *externalPlugin) IsBanned(email string) bool {
	return p.antifraudBanState().isBanned(email)
}

// ForceUnban invokes the provider's administrative operation and immediately
// removes the local decision after the provider accepts it. A provider may
// also send push_unban through the broker; the operation is idempotent.
func (p *externalPlugin) ForceUnban(ctx context.Context, email string) error {
	_, err := p.Call(ctx, pluginrpc.CallRequest{
		Service: externalAntifraudService,
		Method:  "force_unban",
		Payload: map[string]any{"email": strings.TrimSpace(email)},
	})
	if err != nil {
		return err
	}
	p.antifraudBanState().pushUnban(email)
	return nil
}

// Snapshot passes through the provider's diagnostic snapshot. It is not used
// for authorisation decisions; callers must use IsBanned / Host.BanCache.
func (p *externalPlugin) Snapshot(ctx context.Context) (map[string]any, error) {
	response, err := p.Call(ctx, pluginrpc.CallRequest{
		Service: externalAntifraudService,
		Method:  "snapshot",
	})
	if err != nil {
		return nil, err
	}
	if response.Payload == nil {
		return map[string]any{}, nil
	}
	result := make(map[string]any, len(response.Payload))
	for key, value := range response.Payload {
		result[key] = value
	}
	return result, nil
}

// IngestEvents forwards a batch received by the kernel to the provider using
// only serializable fields from pluginapi.FraudEvent.
func (p *externalPlugin) IngestEvents(ctx context.Context, sourceID string, events []pluginapi.FraudEvent) error {
	payloadEvents := make([]map[string]any, 0, len(events))
	for _, event := range events {
		payloadEvents = append(payloadEvents, map[string]any{
			"email": event.Email,
			"ip":    event.IP,
		})
	}
	_, err := p.Call(ctx, pluginrpc.CallRequest{
		Service: externalAntifraudService,
		Method:  "ingest_events",
		Payload: map[string]any{
			"source_id": sourceID,
			"events":    payloadEvents,
		},
	})
	return err
}

// handleExternalBanUpdate is exposed only through the service proxy that is
// created for an external antifraud provider. The payload format is documented
// in plugins-external/README.md. RFC3339/RFC3339Nano is canonical; Unix seconds
// are accepted for independently implemented clients using numeric envelopes.
func (p *externalPlugin) handleExternalBanUpdate(_ context.Context, request pluginrpc.CallRequest) (pluginrpc.CallResponse, error) {
	if request.Service != externalBanUpdateSinkService {
		return pluginrpc.CallResponse{}, fmt.Errorf("unexpected external antifraud reverse service %q", request.Service)
	}
	email := externalString(request.Payload, "email")
	switch request.Method {
	case externalBanPushMethod:
		until, err := externalBanUntil(request.Payload)
		if err != nil {
			return pluginrpc.CallResponse{}, err
		}
		if strings.TrimSpace(email) == "" {
			return pluginrpc.CallResponse{}, errorsNew("email must not be empty")
		}
		p.antifraudBanState().pushBan(email, until)
	case externalBanUnbanMethod:
		if strings.TrimSpace(email) == "" {
			return pluginrpc.CallResponse{}, errorsNew("email must not be empty")
		}
		p.antifraudBanState().pushUnban(email)
	default:
		return pluginrpc.CallResponse{}, fmt.Errorf("unsupported ban_update_sink method %q", request.Method)
	}
	return pluginrpc.CallResponse{Payload: map[string]any{}}, nil
}

// errorsNew avoids importing errors solely for two static adapter messages.
func errorsNew(message string) error {
	return fmt.Errorf("external antifraud ban_update_sink: %s", message)
}

func externalBanUntil(payload map[string]any) (time.Time, error) {
	value, ok := externalValue(payload, "banned_until")
	if !ok || value == nil {
		return time.Time{}, errorsNew("banned_until is required")
	}
	switch typed := value.(type) {
	case string:
		until, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(typed))
		if err != nil {
			return time.Time{}, fmt.Errorf("external antifraud ban_update_sink: parse banned_until: %w", err)
		}
		return until, nil
	case float64:
		if typed != float64(int64(typed)) {
			return time.Time{}, errorsNew("numeric banned_until must be Unix whole seconds")
		}
		return time.Unix(int64(typed), 0), nil
	case int:
		return time.Unix(int64(typed), 0), nil
	case int64:
		return time.Unix(typed, 0), nil
	case json.Number:
		seconds, err := typed.Int64()
		if err != nil {
			return time.Time{}, fmt.Errorf("external antifraud ban_update_sink: numeric banned_until: %w", err)
		}
		return time.Unix(seconds, 0), nil
	default:
		return time.Time{}, fmt.Errorf("external antifraud ban_update_sink: banned_until must be RFC3339 string or Unix seconds, got %T", value)
	}
}
