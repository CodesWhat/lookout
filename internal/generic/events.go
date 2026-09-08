package generic

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/codeswhat/portwing/internal/docker"
)

// genericEvent is the stable envelope written on the SSE stream.
type genericEvent struct {
	TS          string            `json:"ts"`
	Type        string            `json:"type"`
	Action      string            `json:"action"`
	ContainerID string            `json:"containerId"`
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type sseClient struct {
	id     string
	events chan []byte
}

// EventBroadcaster subscribes to Docker container events and fans them out
// to connected SSE clients. It keeps exactly one upstream docker.EventStream
// subscription alive regardless of how many clients are connected: the
// subscription starts lazily when the first client registers and stops when
// the last one leaves, so N dashboards watching the same agent cost the
// daemon one /events connection, not N.
type EventBroadcaster struct {
	dockerClient *docker.Client

	mu      sync.RWMutex
	clients map[string]*sseClient

	// upstreamCancel stops the current shared docker.EventStream
	// subscription. Non-nil only while at least one client is connected;
	// guarded by mu like the clients map, since both change together at
	// the first-join/last-leave edges.
	upstreamCancel context.CancelFunc

	// newEventStream constructs the shared upstream subscriber. Defaults to
	// a real *docker.EventStream; overridable per-instance in tests to reach
	// the defensive Subscribe-error branch in startUpstreamLocked that a
	// live *docker.EventStream never triggers today. Instance-scoped (not a
	// package var) so overriding it in one test can't race with another
	// test's broadcaster instance under -race.
	newEventStream func(client *docker.Client) eventSubscriber

	// marshalEvent serializes a genericEvent for the SSE wire. Defaults to
	// json.Marshal; overridable per-instance in tests to reach the marshal-
	// error branch in pumpUpstream, which genericEvent's plain string/map
	// fields can't trigger in practice.
	marshalEvent func(v any) ([]byte, error)

	// heartbeatInterval controls how often ServeHTTP writes the SSE
	// keepalive comment. Defaults to 30s; overridable per-instance in tests
	// so a heartbeat test can observe one firing without waiting out the
	// production interval.
	heartbeatInterval time.Duration
}

// NewEventBroadcaster creates an EventBroadcaster.
func NewEventBroadcaster(dockerClient *docker.Client) *EventBroadcaster {
	return &EventBroadcaster{
		dockerClient: dockerClient,
		clients:      make(map[string]*sseClient),
		newEventStream: func(client *docker.Client) eventSubscriber {
			return docker.NewEventStream(client)
		},
		marshalEvent:      json.Marshal,
		heartbeatInterval: 30 * time.Second,
	}
}

// ServeHTTP implements http.Handler for SSE connections.
func (b *EventBroadcaster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	client := &sseClient{
		id:     uuid.New().String(),
		events: make(chan []byte, 64),
	}

	b.registerClient(client)

	slog.Info("generic SSE client connected", "clientId", client.id)

	ctx := r.Context()
	heartbeat := time.NewTicker(b.heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("generic SSE client disconnected", "clientId", client.id)
			b.removeClient(client.id)
			return

		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ": heartbeat\n\n"); err != nil {
				b.removeClient(client.id)
				return
			}
			flusher.Flush()

		case data, ok := <-client.events:
			if !ok {
				return
			}

			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				b.removeClient(client.id)
				return
			}
			flusher.Flush()
		}
	}
}

// registerClient adds a client and, if it is the first one, starts the
// shared upstream Docker event subscription that will feed it.
func (b *EventBroadcaster) registerClient(client *sseClient) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[client.id] = client
	if len(b.clients) == 1 {
		b.startUpstreamLocked()
	}
}

// removeClient cleans up a disconnected client and, if it was the last one,
// stops the shared upstream subscription — nothing is left to feed.
func (b *EventBroadcaster) removeClient(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	client, ok := b.clients[id]
	if !ok {
		return
	}

	close(client.events)
	delete(b.clients, id)
	if len(b.clients) == 0 {
		b.stopUpstreamLocked()
	}
	slog.Info("generic SSE client removed", "clientId", id)
}

// eventSubscriber is the subset of *docker.EventStream that
// startUpstreamLocked needs. It exists so tests can substitute a stub whose
// Subscribe returns an error, exercising the defensive branch below that a
// live *docker.EventStream never triggers today.
type eventSubscriber interface {
	Subscribe(ctx context.Context) (<-chan docker.DockerEvent, error)
}

// startUpstreamLocked opens the shared docker.EventStream subscription and
// starts the goroutine that fans its events out to every registered client.
// Callers must hold mu.
func (b *EventBroadcaster) startUpstreamLocked() {
	ctx, cancel := context.WithCancel(context.Background())

	stream := b.newEventStream(b.dockerClient)
	eventCh, err := stream.Subscribe(ctx)
	if err != nil {
		// EventStream.Subscribe's current implementation never fails
		// synchronously — it reconnects with backoff internally and only
		// reports failures via log lines — but don't leave upstreamCancel
		// unset (and clients waiting on a subscription that never started)
		// if a future implementation does.
		cancel()
		slog.Error("failed to subscribe to docker events", "error", err)
		return
	}

	b.upstreamCancel = cancel
	go b.pumpUpstream(eventCh)
}

// stopUpstreamLocked cancels the shared upstream subscription, if any.
// Callers must hold mu.
func (b *EventBroadcaster) stopUpstreamLocked() {
	if b.upstreamCancel != nil {
		b.upstreamCancel()
		b.upstreamCancel = nil
	}
}

// pumpUpstream reads events off the shared subscription until it closes
// (which happens once stopUpstreamLocked cancels its context) and fans each
// one out to every connected client.
func (b *EventBroadcaster) pumpUpstream(eventCh <-chan docker.DockerEvent) {
	for de := range eventCh {
		ge := genericEvent{
			TS:          time.Unix(de.Time, 0).UTC().Format(time.RFC3339),
			Type:        de.Type,
			Action:      de.Action,
			ContainerID: de.Actor.ID,
			Name:        de.Actor.Attributes["name"],
			Image:       de.Actor.Attributes["image"],
			Labels:      filterLabels(de.Actor.Attributes),
		}

		data, err := b.marshalEvent(ge)
		if err != nil {
			slog.Error("failed to marshal generic event", "error", err)
			continue
		}

		b.broadcast(data)
	}
}

// broadcast sends raw event data to every connected SSE client. A client
// whose buffer is full is slow or stuck; drop the event for that client
// rather than block the shared upstream pump on it.
func (b *EventBroadcaster) broadcast(data []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, client := range b.clients {
		select {
		case client.events <- data:
		default:
			slog.Warn("generic SSE client buffer full, dropping event", "clientId", client.id)
		}
	}
}

// filterLabels strips the synthetic Docker attributes (name, image, etc.)
// that are not real container labels, leaving only the label key/value pairs.
var syntheticKeys = map[string]bool{
	"name":                              true,
	"image":                             true,
	"exitCode":                          true,
	"signal":                            true,
	"execID":                            true,
	"maintainer":                        true,
	"org.opencontainers.image.created":  true,
	"org.opencontainers.image.revision": true,
	"org.opencontainers.image.source":   true,
	"org.opencontainers.image.title":    true,
	"org.opencontainers.image.url":      true,
	"org.opencontainers.image.version":  true,
}

func filterLabels(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]string)
	for k, v := range attrs {
		if !syntheticKeys[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
