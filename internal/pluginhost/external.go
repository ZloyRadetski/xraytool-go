package pluginhost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"

	"xraytool/internal/pluginapi"
	"xraytool/pluginrpc"
)

const (
	externalPluginStartTimeout = 10 * time.Second
	externalPluginRPCTimeout   = 3 * time.Second
	externalCallbackBodyLimit  = 1 << 20 // 1 MiB; callbacks should be compact.
)

// preflightPlugin is implemented by a plugin whose metadata requires a
// connection before Host can build the dependency graph.
type preflightPlugin interface {
	Preflight(context.Context) error
	AbortPreflight(context.Context)
}

// restartablePlugin is intentionally narrower than pluginapi.Plugin. Builtin
// plugins do not gain accidental process-restart semantics.
type restartablePlugin interface {
	Restart(context.Context) error
	IsExternal() bool
}

type externalLogProvider interface {
	Logs(maxLines int) []string
}

// externalPlugin adapts a go-plugin/gRPC subprocess to pluginapi.Plugin.
//
// It is deliberately not a transparent proxy for arbitrary Go interfaces.
// Only extension points with an explicit structured adapter below are exposed
// across the process boundary. All other publications/requires fail during
// Init with a specific configuration error.
type externalPlugin struct {
	name  string
	entry PluginEntry
	log   *slog.Logger

	opMu sync.Mutex // serialises connect/init/restart/stop transitions
	mu   sync.RWMutex

	client *plugin.Client
	remote *pluginrpc.Client
	meta   pluginapi.Metadata
	caps   map[string]any

	prepared    bool
	initialised bool
	stopping    bool
	restarting  bool
	restarts    int
	runCtx      context.Context

	config   pluginapi.RawConfig
	handlers map[string]pluginrpc.ServiceHandler
	logs     *externalLogSink
	banState *externalBanState

	// Restart-spawned Start calls are not owned by Host.starts, so Stop waits
	// for them itself before the Host tears down its registry.
	restartedStarts sync.WaitGroup
}

func newExternalPlugin(name string, entry PluginEntry, log *slog.Logger) *externalPlugin {
	if log == nil {
		log = slog.Default()
	}
	return &externalPlugin{
		name:     name,
		entry:    entry,
		log:      log.With("plugin", name, "source", "external"),
		logs:     newExternalLogSink(name, entry.LogPath),
		banState: newExternalBanState(),
	}
}

func (p *externalPlugin) IsExternal() bool { return true }

// Logs returns the most recent stderr lines emitted by the external process.
// A non-positive maxLines returns the complete bounded tail.
func (p *externalPlugin) Logs(maxLines int) []string { return p.logs.lines(maxLines) }

// Preflight launches the subprocess, verifies the go-plugin handshake and
// reads immutable metadata before Host.Graph runs.
func (p *externalPlugin) Preflight(ctx context.Context) error {
	if ctx == nil {
		return errors.New("external plugin preflight context must not be nil")
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()

	p.mu.RLock()
	ready := p.prepared && p.remote != nil && p.client != nil && !p.client.Exited()
	p.mu.RUnlock()
	if ready {
		return nil
	}
	return p.connectLocked(ctx)
}

func (p *externalPlugin) connectLocked(ctx context.Context) error {
	if strings.TrimSpace(p.entry.Exec) == "" {
		return fmt.Errorf("external plugin %q requires a non-empty exec", p.name)
	}
	if p.entry.RestartPolicy.MaxRestarts < 0 {
		return fmt.Errorf("external plugin %q has negative restart_policy.max_restarts", p.name)
	}
	if p.entry.RestartPolicy.Backoff < 0 {
		return fmt.Errorf("external plugin %q has negative restart_policy.backoff", p.name)
	}

	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig: pluginrpc.Handshake,
		Plugins: plugin.PluginSet{
			pluginrpc.PluginName: pluginrpc.ClientPlugin(),
		},
		Cmd:              exec.Command(p.entry.Exec, p.entry.Args...),
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		AutoMTLS:         true,
		StartTimeout:     externalPluginStartTimeout,
		Stderr:           p.logs,
		SyncStdout:       p.logs,
		SyncStderr:       p.logs,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "xraytool-external-plugin",
			Output: io.Discard,
			Level:  hclog.Off,
		}),
	})

	protocolClient, err := client.Client()
	if err != nil {
		client.Kill()
		return fmt.Errorf("start external plugin %q: %w", p.name, err)
	}
	raw, err := protocolClient.Dispense(pluginrpc.PluginName)
	if err != nil {
		client.Kill()
		return fmt.Errorf("dispense external plugin %q: %w", p.name, err)
	}
	remote, ok := raw.(*pluginrpc.Client)
	if !ok || remote == nil {
		client.Kill()
		return fmt.Errorf("external plugin %q returned unsupported go-plugin client %T", p.name, raw)
	}

	rpcCtx, cancel := externalRPCCtx(ctx)
	metadata, err := remote.Describe(rpcCtx)
	cancel()
	if err != nil {
		client.Kill()
		return fmt.Errorf("describe external plugin %q: %w", p.name, err)
	}
	meta, err := convertExternalMetadata(metadata)
	if err != nil {
		client.Kill()
		return fmt.Errorf("external plugin %q returned invalid metadata: %w", p.name, err)
	}
	if meta.Name != p.name {
		client.Kill()
		return fmt.Errorf("external plugin configured as %q returned Metadata.Name=%q", p.name, meta.Name)
	}

	p.mu.Lock()
	oldClient := p.client
	p.client = client
	p.remote = remote
	p.meta = meta
	p.caps = cloneExternalCapabilities(metadata.Capabilities)
	p.prepared = true
	p.stopping = false
	p.mu.Unlock()
	if oldClient != nil && oldClient != client {
		go oldClient.Kill()
	}
	p.log.Info("[pluginhost] external plugin handshake completed", "version", meta.Version, "api_version", meta.APIVersion)
	return nil
}

// AbortPreflight tears down a process started solely to inspect metadata after
// Host.Load fails before reaching normal Shutdown.
func (p *externalPlugin) AbortPreflight(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	_ = p.stop(ctx, true)
}

func convertExternalMetadata(metadata pluginrpc.Metadata) (pluginapi.Metadata, error) {
	if strings.TrimSpace(metadata.Name) == "" {
		return pluginapi.Metadata{}, errors.New("name is empty")
	}
	if strings.TrimSpace(metadata.Kind) == "" {
		return pluginapi.Metadata{}, errors.New("kind is empty")
	}
	if strings.TrimSpace(metadata.Version) == "" {
		return pluginapi.Metadata{}, errors.New("version is empty")
	}
	if strings.TrimSpace(metadata.APIVersion) == "" {
		return pluginapi.Metadata{}, errors.New("api_version is empty")
	}
	result := pluginapi.Metadata{
		Name:        metadata.Name,
		Kind:        metadata.Kind,
		Version:     metadata.Version,
		APIVersion:  metadata.APIVersion,
		Description: metadata.Description,
		Mandatory:   metadata.Mandatory,
		Publishes:   make([]pluginapi.ServiceRef, len(metadata.Publishes)),
		Requires:    make([]pluginapi.ServiceRef, len(metadata.Requires)),
	}
	for i, ref := range metadata.Publishes {
		result.Publishes[i] = pluginapi.ServiceRef{Name: ref.Name, Optional: ref.Optional}
	}
	for i, ref := range metadata.Requires {
		result.Requires[i] = pluginapi.ServiceRef{Name: ref.Name, Optional: ref.Optional}
	}
	return result, nil
}

func (p *externalPlugin) Metadata() pluginapi.Metadata {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cloneMetadata(p.meta)
}

func (p *externalPlugin) Init(ctx context.Context, cfg pluginapi.RawConfig, resolver pluginapi.ServiceResolver) error {
	if ctx == nil {
		return errors.New("external plugin Init context must not be nil")
	}
	if resolver == nil {
		return errors.New("external plugin Init resolver must not be nil")
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()

	p.mu.RLock()
	remote := p.remote
	meta := cloneMetadata(p.meta)
	p.mu.RUnlock()
	if remote == nil {
		return fmt.Errorf("external plugin %q was not preflighted", p.name)
	}
	p.mu.RLock()
	capabilities := cloneExternalCapabilities(p.caps)
	p.mu.RUnlock()
	if err := validateExternalPublications(p.name, meta, capabilities); err != nil {
		return err
	}
	handlers, err := p.serializableRequirements(meta, resolver)
	if err != nil {
		return err
	}
	proxyID := uint32(0)
	if len(handlers) > 0 {
		proxyID, err = remote.OpenServiceProxy(handlers)
		if err != nil {
			return fmt.Errorf("external plugin %q create ServiceProxy: %w", p.name, err)
		}
	}
	required := make([]string, 0, len(handlers))
	for name := range handlers {
		required = append(required, name)
	}
	sort.Strings(required)

	rpcCtx, cancel := externalRPCCtx(ctx)
	err = remote.Init(rpcCtx, cloneRawConfig(cfg), proxyID, required)
	cancel()
	if err != nil {
		return fmt.Errorf("external plugin %q Init RPC: %w", p.name, err)
	}

	p.mu.Lock()
	p.initialised = true
	p.config = cloneRawConfig(cfg)
	p.handlers = handlers
	p.mu.Unlock()
	return nil
}

func validateExternalPublications(pluginName string, metadata pluginapi.Metadata, capabilities map[string]any) error {
	for _, publication := range metadata.Publishes {
		if isExternalPaymentService(publication.Name) {
			if strings.TrimSpace(capabilityString(capabilities, "method_id")) == "" {
				return fmt.Errorf("external plugin %q publishes payment_provider but Metadata.Capabilities.method_id is empty", pluginName)
			}
			continue
		}
		switch publication.Name {
		case "pricing_engine", "notification_provider", "event_sink", externalAntifraudService:
			// These adapters have documented structured request/response shapes.
		default:
			return fmt.Errorf(
				"external plugin %q cannot publish service %q: arbitrary Go services are not RPC-compatible; "+
					"use a documented external adapter or add a dedicated ServiceHandler contract",
				pluginName, publication.Name,
			)
		}
	}
	return nil
}

func (p *externalPlugin) serializableRequirements(metadata pluginapi.Metadata, resolver pluginapi.ServiceResolver) (map[string]pluginrpc.ServiceHandler, error) {
	handlers := make(map[string]pluginrpc.ServiceHandler)
	for _, requirement := range metadata.Requires {
		service, err := resolver.Resolve(requirement.Name)
		if err != nil {
			if requirement.Optional {
				continue
			}
			return nil, fmt.Errorf("external plugin %q resolve required service %q: %w", p.name, requirement.Name, err)
		}
		handler, ok := service.(pluginrpc.ServiceHandler)
		if !ok {
			return nil, fmt.Errorf(
				"external plugin %q requires service %q, but %T is not serializable over the external plugin RPC transport; "+
					"adapt it with pluginrpc.ServiceHandler",
				p.name, requirement.Name, service,
			)
		}
		handlers[requirement.Name] = handler
	}
	// An external antifraud provider receives a brokered, host-owned update
	// sink automatically.  It is intentionally not expressed as Metadata.Requires:
	// the sink is a kernel primitive, not a plugin-published dependency, and
	// adding it to the public dependency graph would allow unrelated plugins to
	// claim access to the ban cache.  The handler only accepts structured
	// push_ban_update / push_unban messages and never exposes cache reads.
	if publishesExternalAntifraud(metadata) {
		handlers[externalBanUpdateSinkService] = pluginrpc.ServiceHandlerFunc(p.handleExternalBanUpdate)
	}
	return handlers, nil
}

// PublishedServices returns only services whose wire adapters were validated
// during Init. It is deliberately a fixed allow-list, not a reflection bridge.
func (p *externalPlugin) PublishedServices() map[string]any {
	p.mu.RLock()
	metadata := cloneMetadata(p.meta)
	initialised := p.initialised
	p.mu.RUnlock()
	if !initialised {
		return nil
	}
	services := make(map[string]any, len(metadata.Publishes))
	for _, publication := range metadata.Publishes {
		if isExternalPaymentService(publication.Name) {
			services[publication.Name] = pluginapi.PaymentProvider(p)
			continue
		}
		switch publication.Name {
		case "pricing_engine":
			services[publication.Name] = pluginapi.PricingEngine(p)
		case "notification_provider":
			services[publication.Name] = pluginapi.NotificationProvider(p)
		case "event_sink":
			services[publication.Name] = pluginapi.EventSink(p)
		case externalAntifraudService:
			services[publication.Name] = pluginapi.AntifraudProvider(p)
		}
	}
	return services
}

func isExternalPaymentService(name string) bool {
	return name == "payment_provider" || strings.HasPrefix(name, "payment_provider.")
}

// publishes reports whether this external process declared a particular
// registry publication. externalPlugin implements several adapter interfaces
// structurally, so Host accessors must use this metadata guard instead of a
// bare Go type assertion (otherwise, for example, an antifraud process would
// accidentally be dispatched as an EventSink).
func (p *externalPlugin) publishes(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, publication := range p.meta.Publishes {
		if publication.Name == name {
			return true
		}
	}
	return false
}

func (p *externalPlugin) publishesPaymentProvider() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, publication := range p.meta.Publishes {
		if isExternalPaymentService(publication.Name) {
			return true
		}
	}
	return false
}

// Start blocks in the plugin's Start RPC until the host run context is
// cancelled or the child exits. A crash schedules a policy-limited restart.
func (p *externalPlugin) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("external plugin Start context must not be nil")
	}
	p.mu.Lock()
	p.runCtx = ctx
	remote := p.remote
	initialised := p.initialised
	stopping := p.stopping
	p.mu.Unlock()
	if !initialised || remote == nil {
		return fmt.Errorf("external plugin %q Start called before Init", p.name)
	}
	if stopping {
		return context.Canceled
	}
	err := remote.Start(ctx)
	if err != nil && ctx.Err() == nil && p.shouldRestartAfterStart(remote) {
		p.log.Warn("[pluginhost] external plugin start stream ended; scheduling restart", "error", err)
		go p.restartAfterFailure(ctx)
	}
	return err
}

func (p *externalPlugin) restartAfterFailure(runCtx context.Context) {
	if runCtx == nil || runCtx.Err() != nil {
		return
	}
	if err := p.Restart(context.Background()); err != nil {
		p.log.Error("[pluginhost] external plugin restart failed", "error", err)
	}
}

// Stop asks the child to stop gracefully, then forcefully kills the subprocess
// if it is still alive. It is safe to call repeatedly.
func (p *externalPlugin) Stop(ctx context.Context) error { return p.stop(ctx, false) }

func (p *externalPlugin) stop(ctx context.Context, abort bool) error {
	if ctx == nil {
		return errors.New("external plugin Stop context must not be nil")
	}
	p.opMu.Lock()
	p.mu.Lock()
	p.stopping = true
	remote := p.remote
	client := p.client
	p.mu.Unlock()

	var errs []error
	if remote != nil {
		rpcCtx, cancel := externalRPCCtx(ctx)
		if err := remote.Stop(rpcCtx); err != nil && !isExpectedExternalStop(err, ctx) {
			errs = append(errs, fmt.Errorf("external plugin %q Stop RPC: %w", p.name, err))
		}
		cancel()
	}
	if client != nil {
		if err := killExternalClient(ctx, client); err != nil {
			errs = append(errs, fmt.Errorf("external plugin %q terminate process: %w", p.name, err))
		}
	}
	p.mu.Lock()
	if p.client == client {
		p.client = nil
		p.remote = nil
		p.prepared = false
	}
	if abort {
		p.initialised = false
		p.config = nil
		p.handlers = nil
	}
	p.mu.Unlock()
	p.opMu.Unlock()

	if err := waitGroupWithContext(ctx, &p.restartedStarts); err != nil {
		errs = append(errs, fmt.Errorf("external plugin %q wait for restarted Start: %w", p.name, err))
	}
	return errors.Join(errs...)
}

func isExpectedExternalStop(err error, ctx context.Context) bool {
	if err == nil {
		return true
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return strings.Contains(err.Error(), "external plugin process exited")
}

func killExternalClient(ctx context.Context, client *plugin.Client) error {
	done := make(chan struct{})
	go func() {
		client.Kill()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitGroupWithContext(ctx context.Context, group *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *externalPlugin) Health(ctx context.Context) error {
	if ctx == nil {
		return errors.New("external plugin Health context must not be nil")
	}
	p.mu.RLock()
	remote := p.remote
	client := p.client
	p.mu.RUnlock()
	if remote == nil || client == nil {
		return fmt.Errorf("external plugin %q is not connected", p.name)
	}
	if client.Exited() {
		return fmt.Errorf("external plugin %q process exited", p.name)
	}
	rpcCtx, cancel := externalRPCCtx(ctx)
	err := remote.Health(rpcCtx)
	cancel()
	return err
}

// Restart restarts an unhealthy external plugin according to its configured
// policy. A successful restart preserves the last validated config and only
// re-exposes explicit ServiceHandler dependencies.
func (p *externalPlugin) Restart(ctx context.Context) error {
	if ctx == nil {
		return errors.New("external plugin Restart context must not be nil")
	}
	p.opMu.Lock()
	
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		p.opMu.Unlock()
		return fmt.Errorf("external plugin %q is stopping and cannot be restarted", p.name)
	}
	if p.restarting {
		p.mu.Unlock()
		p.opMu.Unlock()
		return fmt.Errorf("external plugin %q restart is already in progress", p.name)
	}
	if p.entry.RestartPolicy.MaxRestarts == 0 {
		p.mu.Unlock()
		p.opMu.Unlock()
		return fmt.Errorf("external plugin %q restart is disabled by restart_policy.max_restarts=0", p.name)
	}
	if p.restarts >= p.entry.RestartPolicy.MaxRestarts {
		limit := p.entry.RestartPolicy.MaxRestarts
		p.mu.Unlock()
		p.opMu.Unlock()
		return fmt.Errorf("external plugin %q restart limit exhausted (%d)", p.name, limit)
	}
	p.restarting = true
	p.restarts++
	attempt := p.restarts
	backoff := p.entry.RestartPolicy.Backoff
	client := p.client
	remote := p.remote
	config := cloneRawConfig(p.config)
	handlers := cloneServiceHandlers(p.handlers)
	runCtx := p.runCtx
	initialised := p.initialised
	// Clear the old client before its Start RPC is interrupted. Otherwise its
	// completion can mistake an intentional restart for a crash and schedule a
	// second, competing restart.
	p.client = nil
	p.remote = nil
	p.prepared = false
	p.mu.Unlock()
	
	// Release the outer opMu during the backoff sleep so that Shutdown (which
	// takes opMu to Stop the plugin) doesn't deadlock.
	p.opMu.Unlock()

	defer func() {
		p.mu.Lock()
		p.restarting = false
		p.mu.Unlock()
	}()

	if backoff > 0 {
		timer := time.NewTimer(backoff)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return fmt.Errorf("external plugin %q restart backoff: %w", p.name, ctx.Err())
		}
	}
	
	// Re-acquire opMu before continuing the restart process.
	p.opMu.Lock()
	defer p.opMu.Unlock()

	p.log.Warn("[pluginhost] restarting external plugin", "attempt", attempt, "max_restarts", p.entry.RestartPolicy.MaxRestarts)
	if err := stopExternalConnection(ctx, p.name, remote, client); err != nil {
		p.log.Warn("[pluginhost] external plugin stop during restart failed", "error", err)
	}

	if err := p.connectLocked(ctx); err != nil {
		return err
	}
	if initialised {
		if err := p.initAfterRestartLocked(ctx, config, handlers); err != nil {
			return err
		}
	}
	if runCtx != nil && runCtx.Err() == nil && initialised {
		p.mu.RLock()
		restartedRemote := p.remote
		p.mu.RUnlock()
		p.restartedStarts.Add(1)
		go func(remote *pluginrpc.Client) {
			defer p.restartedStarts.Done()
			if err := p.startRestarted(runCtx, remote); err != nil && runCtx.Err() == nil && p.shouldRestartAfterStart(remote) {
				p.log.Warn("[pluginhost] restarted external plugin Start returned", "error", err)
				go p.restartAfterFailure(runCtx)
			}
		}(restartedRemote)
	}
	return nil
}

func (p *externalPlugin) initAfterRestartLocked(ctx context.Context, config pluginapi.RawConfig, handlers map[string]pluginrpc.ServiceHandler) error {
	p.mu.RLock()
	remote := p.remote
	p.mu.RUnlock()
	if remote == nil {
		return fmt.Errorf("external plugin %q reconnect did not yield RPC client", p.name)
	}
	proxyID := uint32(0)
	var err error
	if len(handlers) > 0 {
		proxyID, err = remote.OpenServiceProxy(handlers)
		if err != nil {
			return fmt.Errorf("external plugin %q recreate ServiceProxy: %w", p.name, err)
		}
	}
	required := make([]string, 0, len(handlers))
	for name := range handlers {
		required = append(required, name)
	}
	sort.Strings(required)
	rpcCtx, cancel := externalRPCCtx(ctx)
	err = remote.Init(rpcCtx, config, proxyID, required)
	cancel()
	if err != nil {
		return fmt.Errorf("external plugin %q reinitialise: %w", p.name, err)
	}
	p.mu.Lock()
	p.initialised = true
	p.config = cloneRawConfig(config)
	p.handlers = handlers
	p.mu.Unlock()
	return nil
}

func (p *externalPlugin) startRestarted(ctx context.Context, remote *pluginrpc.Client) error {
	if remote == nil {
		return fmt.Errorf("external plugin %q restarted Start has no RPC client", p.name)
	}
	return remote.Start(ctx)
}

func stopExternalConnection(ctx context.Context, name string, remote *pluginrpc.Client, client *plugin.Client) error {
	var errs []error
	if remote != nil {
		rpcCtx, cancel := externalRPCCtx(ctx)
		if err := remote.Stop(rpcCtx); err != nil && !isExpectedExternalStop(err, ctx) {
			errs = append(errs, fmt.Errorf("external plugin %q Stop RPC: %w", name, err))
		}
		cancel()
	}
	if client != nil {
		if err := killExternalClient(ctx, client); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *externalPlugin) isStopping() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stopping
}

// shouldRestartAfterStart avoids a second restart when an intentional Restart
// or Stop terminates an older Start RPC. The remote pointer changes before the
// new process is started, so an old Start completion is recognisably stale.
func (p *externalPlugin) shouldRestartAfterStart(remote *pluginrpc.Client) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return !p.stopping && !p.restarting && p.remote == remote
}

func cloneServiceHandlers(source map[string]pluginrpc.ServiceHandler) map[string]pluginrpc.ServiceHandler {
	if source == nil {
		return nil
	}
	result := make(map[string]pluginrpc.ServiceHandler, len(source))
	for name, handler := range source {
		result[name] = handler
	}
	return result
}

func externalRPCCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, externalPluginRPCTimeout)
}

// Call exposes the documented structured RPC escape hatch to code that has an
// explicit external contract. It is not used to transport arbitrary local
// services.
func (p *externalPlugin) Call(ctx context.Context, req pluginrpc.CallRequest) (pluginrpc.CallResponse, error) {
	if ctx == nil {
		return pluginrpc.CallResponse{}, errors.New("external plugin Call context must not be nil")
	}
	p.mu.RLock()
	remote := p.remote
	p.mu.RUnlock()
	if remote == nil {
		return pluginrpc.CallResponse{}, fmt.Errorf("external plugin %q is not connected", p.name)
	}
	rpcCtx, cancel := externalRPCCtx(ctx)
	response, err := remote.Call(rpcCtx, req)
	cancel()
	return response, err
}

// CalculatePrice is the explicit external adapter for the pricing_engine
// service. Its payload shape is the JSON form of pluginapi.PricingRequest and
// its result shape is pluginapi.PricingResult.
func (p *externalPlugin) CalculatePrice(ctx context.Context, request pluginapi.PricingRequest) (pluginapi.PricingResult, error) {
	response, err := p.Call(ctx, pluginrpc.CallRequest{
		Service: "pricing_engine", Method: "calculate_price", Payload: pricingRequestPayload(request),
	})
	if err != nil {
		return pluginapi.PricingResult{}, err
	}
	result, err := decodePricingResult(response.Payload)
	if err != nil {
		return pluginapi.PricingResult{}, fmt.Errorf("external pricing_engine response: %w", err)
	}
	return result, nil
}

// Channels returns the notification channels advertised in Metadata.Capabilities.
// It cannot return an error by the pluginapi contract, so malformed metadata
// simply results in no matching channel.
func (p *externalPlugin) Channels() []string {
	p.mu.RLock()
	capabilities := cloneExternalCapabilities(p.caps)
	p.mu.RUnlock()
	return capabilityStrings(capabilities, "channels")
}

func (p *externalPlugin) Send(ctx context.Context, notification pluginapi.Notification) error {
	_, err := p.Call(ctx, pluginrpc.CallRequest{
		Service: "notification_provider", Method: "send", Payload: map[string]any{
			"channel": notification.Channel,
			"to":      notification.To,
			"kind":    notification.Kind,
			"payload": notification.Payload,
		},
	})
	return err
}

func (p *externalPlugin) Handle(ctx context.Context, event pluginapi.Event) error {
	_, err := p.Call(ctx, pluginrpc.CallRequest{
		Service: "event_sink", Method: "handle", Payload: map[string]any{
			"type":        event.Type,
			"occurred_at": event.OccurredAt,
			"data":        event.Data,
			"user_meta":   event.UserMeta,
		},
	})
	return err
}

// MethodID returns the explicitly advertised payment provider identifier. It
// is read from transport metadata rather than inferred from the plugin name.
func (p *externalPlugin) MethodID() string {
	p.mu.RLock()
	capabilities := cloneExternalCapabilities(p.caps)
	p.mu.RUnlock()
	return capabilityString(capabilities, "method_id")
}

func (p *externalPlugin) CreateIntent(ctx context.Context, request pluginapi.PaymentIntentRequest) (*pluginapi.PaymentIntentResult, error) {
	response, err := p.Call(ctx, pluginrpc.CallRequest{
		Service: "payment_provider", Method: "create_intent", Payload: map[string]any{
			"user_id":      request.UserID,
			"amount":       request.Amount,
			"currency":     request.Currency,
			"description":  request.Description,
			"external_ref": request.ExternalRef,
			"custom_data":  request.CustomData,
		},
	})
	if err != nil {
		return nil, err
	}
	result, err := decodePaymentIntentResult(response.Payload)
	if err != nil {
		return nil, fmt.Errorf("external payment_provider create_intent response: %w", err)
	}
	return result, nil
}

// VerifyCallback forwards a bounded, replayable representation of the inbound
// HTTP callback. The original request body is restored before the call returns
// so middleware and diagnostics retain normal Go request semantics.
func (p *externalPlugin) VerifyCallback(ctx context.Context, request *http.Request) (*pluginapi.PaymentCallbackResult, error) {
	payload, err := serializePaymentCallback(request)
	if err != nil {
		return nil, err
	}
	response, err := p.Call(ctx, pluginrpc.CallRequest{
		Service: "payment_provider", Method: "verify_callback", Payload: payload,
	})
	if err != nil {
		return nil, err
	}
	result, err := decodePaymentCallbackResult(response.Payload)
	if err != nil {
		return nil, fmt.Errorf("external payment_provider verify_callback response: %w", err)
	}
	return result, nil
}

func (p *externalPlugin) Refund(ctx context.Context, externalID string, amount int) error {
	_, err := p.Call(ctx, pluginrpc.CallRequest{
		Service: "payment_provider", Method: "refund", Payload: map[string]any{
			"external_id": externalID,
			"amount":      amount,
		},
	})
	return err
}

func serializePaymentCallback(request *http.Request) (map[string]any, error) {
	if request == nil {
		return nil, errors.New("external payment_provider VerifyCallback received nil HTTP request")
	}
	var body []byte
	if request.Body != nil {
		limited := io.LimitReader(request.Body, externalCallbackBodyLimit+1)
		var err error
		body, err = io.ReadAll(limited)
		if err != nil {
			return nil, fmt.Errorf("read payment callback body: %w", err)
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		if len(body) > externalCallbackBodyLimit {
			return nil, fmt.Errorf("payment callback body exceeds external transport limit of %d bytes", externalCallbackBodyLimit)
		}
	}
	path, rawQuery := "", ""
	if request.URL != nil {
		path = request.URL.Path
		rawQuery = request.URL.RawQuery
	}
	headers := make(map[string][]string, len(request.Header))
	for name, values := range request.Header {
		headers[name] = append([]string(nil), values...)
	}
	return map[string]any{
		"method":      request.Method,
		"path":        path,
		"raw_query":   rawQuery,
		"host":        request.Host,
		"remote_addr": request.RemoteAddr,
		"headers":     headers,
		"body_base64": base64.StdEncoding.EncodeToString(body),
	}, nil
}

func capabilityStrings(capabilities map[string]any, key string) []string {
	value, ok := capabilities[key]
	if !ok {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		// Values produced by a Go implementation may retain []string until they
		// cross the wire; accept that form as well.
		if strings, ok := value.([]string); ok {
			return append([]string(nil), strings...)
		}
		return nil
	}
	channels := make([]string, 0, len(items))
	for _, item := range items {
		channel, ok := item.(string)
		if !ok || strings.TrimSpace(channel) == "" {
			continue
		}
		channels = append(channels, channel)
	}
	return channels
}

func capabilityString(capabilities map[string]any, key string) string {
	value, ok := capabilities[key]
	if !ok {
		return ""
	}
	stringValue, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(stringValue)
}

func cloneExternalCapabilities(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func pricingRequestPayload(request pluginapi.PricingRequest) map[string]any {
	return map[string]any{
		"user_id":              request.UserID,
		"plan_id":              request.PlanID,
		"promo_code":           request.PromoCode,
		"extra_devices":        request.ExtraDevices,
		"is_upgrade":           request.IsUpgrade,
		"current_plan_id":      request.CurrentPlanID,
		"amount":               request.Amount,
		"plan":                 pricingPlanPayload(request.Plan),
		"current_subscription": pricingSubscriptionPayload(request.CurrentSubscription),
		"max_devices":          request.MaxDevices,
		"platform":             request.Platform,
		"promo":                pricingPromoPayload(request.Promo),
		"now":                  request.Now,
	}
}

func pricingPlanPayload(plan *pluginapi.Plan) any {
	if plan == nil {
		return nil
	}
	return map[string]any{
		"id":                      plan.ID,
		"months":                  plan.Months,
		"base_price":              plan.BasePrice,
		"global_discount_percent": plan.GlobalDiscountPercent,
		"is_active":               plan.IsActive,
		"created_at":              plan.CreatedAt,
		"updated_at":              plan.UpdatedAt,
	}
}

func pricingSubscriptionPayload(subscription *pluginapi.Subscription) any {
	if subscription == nil {
		return nil
	}
	return map[string]any{
		"id":          subscription.ID,
		"user_id":     subscription.UserID,
		"email":       subscription.Email,
		"uuid":        subscription.UUID,
		"status":      subscription.Status,
		"max_devices": subscription.MaxDevices,
		"starts_at":   subscription.StartsAt,
		"ends_at":     subscription.EndsAt,
		"auto_renew":  subscription.AutoRenew,
		"metadata":    subscription.Metadata,
		"created_at":  subscription.CreatedAt,
		"updated_at":  subscription.UpdatedAt,
	}
}

func pricingPromoPayload(promo *pluginapi.PromoCode) any {
	if promo == nil {
		return nil
	}
	return map[string]any{
		"id":               promo.ID,
		"code":             promo.Code,
		"discount_percent": promo.DiscountPercent,
		"max_uses":         promo.MaxUses,
		"uses_count":       promo.UsesCount,
		"target_platform":  promo.TargetPlatform,
		"expires_at":       promo.ExpiresAt,
		"is_active":        promo.IsActive,
		"created_at":       promo.CreatedAt,
		"updated_at":       promo.UpdatedAt,
	}
}

func decodePricingResult(payload map[string]any) (pluginapi.PricingResult, error) {
	finalPrice, err := externalInt(payload, "final_price")
	if err != nil {
		return pluginapi.PricingResult{}, err
	}
	discount, err := externalInt(payload, "discount_percent")
	if err != nil {
		return pluginapi.PricingResult{}, err
	}
	result := pluginapi.PricingResult{
		FinalPrice:      finalPrice,
		DiscountPercent: discount,
		AppliedPromo:    externalString(payload, "applied_promo"),
		Description:     externalString(payload, "description"),
	}
	if value, ok := externalValue(payload, "applied_promo_id"); ok && value != nil {
		id, err := externalIntValue(value, "applied_promo_id")
		if err != nil {
			return pluginapi.PricingResult{}, err
		}
		id64 := int64(id)
		result.AppliedPromoID = &id64
	}
	return result, nil
}

func decodePaymentIntentResult(payload map[string]any) (*pluginapi.PaymentIntentResult, error) {
	result := &pluginapi.PaymentIntentResult{
		ExternalID: externalString(payload, "external_id"),
		PaymentURL: externalString(payload, "payment_url"),
	}
	if value, ok := externalValue(payload, "raw_response"); ok && value != nil {
		mapValue, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("raw_response must be an object, got %T", value)
		}
		result.RawResponse = mapValue
	}
	return result, nil
}

func decodePaymentCallbackResult(payload map[string]any) (*pluginapi.PaymentCallbackResult, error) {
	amount, err := externalInt(payload, "amount")
	if err != nil {
		return nil, err
	}
	result := &pluginapi.PaymentCallbackResult{
		ExternalID: externalString(payload, "external_id"),
		Status:     externalString(payload, "status"),
		Amount:     amount,
		Currency:   externalString(payload, "currency"),
	}
	if value, ok := externalValue(payload, "custom_data"); ok && value != nil {
		mapValue, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("custom_data must be an object, got %T", value)
		}
		result.CustomData = mapValue
	}
	return result, nil
}

func externalString(payload map[string]any, key string) string {
	value, ok := externalValue(payload, key)
	if !ok || value == nil {
		return ""
	}
	stringValue, _ := value.(string)
	return stringValue
}

func externalInt(payload map[string]any, key string) (int, error) {
	value, ok := externalValue(payload, key)
	if !ok || value == nil {
		return 0, nil
	}
	return externalIntValue(value, key)
}

func externalIntValue(value any, key string) (int, error) {
	switch value := value.(type) {
	case float64:
		if value != float64(int(value)) {
			return 0, fmt.Errorf("%s must be an integer, got %v", key, value)
		}
		return int(value), nil
	case int:
		return value, nil
	case int64:
		return int(value), nil
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer: %w", key, err)
		}
		return int(parsed), nil
	default:
		return 0, fmt.Errorf("%s must be an integer, got %T", key, value)
	}
}

// externalValue accepts snake_case and Go-style/camel-case variants. The
// public protocol documents snake_case; accepting the latter keeps adapters
// friendly to early Go SDK adopters that used encoding/json defaults.
func externalValue(payload map[string]any, key string) (any, bool) {
	wanted := normaliseExternalKey(key)
	for candidate, value := range payload {
		if normaliseExternalKey(candidate) == wanted {
			return value, true
		}
	}
	return nil, false
}

func normaliseExternalKey(key string) string {
	key = strings.ToLower(key)
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ReplaceAll(key, "-", "")
	return key
}

var (
	_ pluginapi.Plugin               = (*externalPlugin)(nil)
	_ pluginapi.ServiceProvider      = (*externalPlugin)(nil)
	_ pluginapi.PricingEngine        = (*externalPlugin)(nil)
	_ pluginapi.NotificationProvider = (*externalPlugin)(nil)
	_ pluginapi.EventSink            = (*externalPlugin)(nil)
	_ pluginapi.PaymentProvider      = (*externalPlugin)(nil)
	_ pluginapi.AntifraudProvider    = (*externalPlugin)(nil)
	_ preflightPlugin                = (*externalPlugin)(nil)
	_ restartablePlugin              = (*externalPlugin)(nil)
	_ externalLogProvider            = (*externalPlugin)(nil)
)
