package events

// Event represents a structured webhook event.
// Currently used as a reference struct for JSON, though the actual delivery
// has moved to eventsink_webhook.
type Event struct {
	EventID      string                 `json:"event_id"`
	EventType    string                 `json:"event_type"`
	Timestamp    string                 `json:"timestamp"`
	Data         map[string]interface{} `json:"data"`
	UserMetadata map[string]interface{} `json:"user_metadata,omitempty"`
}

// Dispatcher acts as an in-memory event bus connecting the core domain to the Plugin Host.
type Dispatcher struct {
	onDispatch func(eventType string, data map[string]interface{}, userMetadata map[string]interface{})
}

// Config holds settings for the event dispatcher.
type Config struct {
	// OnDispatch observes every event before the built-in HTTP-webhook
	// delivery. It lets the plugin-host composition root forward legacy event
	// producers to EventSink plugins without making this package depend on the
	// plugin API.
	OnDispatch func(eventType string, data map[string]interface{}, userMetadata map[string]interface{})
}

// NewDispatcher creates a new event dispatcher with the given config.
func NewDispatcher(cfg *Config) *Dispatcher {
	if cfg == nil {
		return &Dispatcher{}
	}
	return &Dispatcher{
		onDispatch: cfg.OnDispatch,
	}
}

// Dispatch sends an event with the given type, data, and metadata.
func (d *Dispatcher) Dispatch(eventType string, data map[string]interface{}, userMetadata map[string]interface{}) {
	if d.onDispatch != nil {
		d.onDispatch(eventType, data, userMetadata)
	}
}

// DispatchSync sends an event synchronously.
func (d *Dispatcher) DispatchSync(eventType string, data map[string]interface{}, userMetadata map[string]interface{}) {
	if d.onDispatch != nil {
		d.onDispatch(eventType, data, userMetadata)
	}
}

// Shutdown waits for all in-flight background webhook deliveries to complete.
func (d *Dispatcher) Shutdown() {
	// No-op in Phase 1.1: background delivery has moved to EventSink plugins.
}
