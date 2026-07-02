package slave

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"xraytool/internal/domain"
)

// EventPropagatorAdapter implements domain.EventPropagator by sending commands
// to all slave nodes asynchronously.
type EventPropagatorAdapter struct {
	reg *Registry
}

func NewEventPropagatorAdapter(reg *Registry) *EventPropagatorAdapter {
	return &EventPropagatorAdapter{reg: reg}
}

func (a *EventPropagatorAdapter) PropagateAll(event string, payload map[string]string) {
	if a.reg != nil {
		a.reg.PropagateAll(event, payload)
	}
}

// FraudReporterAdapter implements domain.FraudEventReporter by batching IP events
// and sending them to the master node via JSON over HTTP.
type FraudReporterAdapter struct {
	mu     sync.Mutex
	buf    []domain.FraudEvent
	client *Client
	entry  Entry
	log    *slog.Logger
}

func NewFraudReporterAdapter(client *Client, entry Entry, log *slog.Logger) *FraudReporterAdapter {
	return &FraudReporterAdapter{
		buf:    make([]domain.FraudEvent, 0, 64),
		client: client,
		entry:  entry,
		log:    log,
	}
}

func (r *FraudReporterAdapter) Report(events []domain.FraudEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Hard limit to prevent OOM
	if len(r.buf) >= 10000 {
		return nil
	}
	r.buf = append(r.buf, events...)
	return nil
}

// Run flushes the buffer periodically. It should be run in a goroutine.
func (r *FraudReporterAdapter) Run(ctx context.Context) {
	r.log.Info("antifraud slave reporter adapter: starting")
	defer r.log.Info("antifraud slave reporter adapter: stopped")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.flush()
			return
		case <-ticker.C:
			r.flush()
		}
	}
}

func (r *FraudReporterAdapter) flush() {
	r.mu.Lock()
	if len(r.buf) == 0 {
		r.mu.Unlock()
		return
	}
	batch := make([]domain.FraudEvent, len(r.buf))
	copy(batch, r.buf)
	r.mu.Unlock()

	type slaveIPEvent struct {
		Email string `json:"email"`
		IP    string `json:"ip"`
	}

	payloadEvents := make([]slaveIPEvent, len(batch))
	for i, e := range batch {
		payloadEvents[i] = slaveIPEvent{Email: e.Email, IP: e.IP}
	}

	payload, err := json.Marshal(struct {
		Events []slaveIPEvent `json:"events"`
	}{Events: payloadEvents})
	
	if err != nil {
		r.log.Error("antifraud slave reporter adapter: failed to marshal batch", slog.String("err", err.Error()))
		return
	}

	if r.entry.URL == "" {
		r.log.Warn("antifraud slave reporter adapter: master_api.url is not configured, cannot forward events")
		r.mu.Lock()
		r.buf = r.buf[:0]
		r.mu.Unlock()
		return
	}

	_, err = r.client.Call(r.entry, "antifraud-events", map[string]string{"payload": string(payload)})
	if err != nil {
		r.log.Warn("antifraud slave reporter adapter: failed to reach master", slog.String("err", err.Error()))
		return
	}

	r.mu.Lock()
	if len(r.buf) == len(batch) {
		r.buf = r.buf[:0]
	} else {
		n := copy(r.buf, r.buf[len(batch):])
		r.buf = r.buf[:n]
	}
	r.mu.Unlock()
}
