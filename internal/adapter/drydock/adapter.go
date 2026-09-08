package drydock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/codeswhat/portwing/internal/adapter"
	"github.com/codeswhat/portwing/internal/docker"
	applog "github.com/codeswhat/portwing/internal/log"
	"github.com/codeswhat/portwing/internal/protocol"
)

// Label constants for Drydock container configuration.
const (
	LabelWatch        = "dd.watch"
	LabelTagInclude   = "dd.tag.include"
	LabelTagExclude   = "dd.tag.exclude"
	LabelTagTransform = "dd.tag.transform"
	LabelDisplayName  = "dd.display.name"
	LabelDisplayIcon  = "dd.display.icon"
	LabelGroup        = "dd.group"
	LabelLinkTemplate = "dd.link.template"

	defaultMessageHandlerConcurrency = 32
	maxContainerLogStreams           = 128

	// maxContainerLogBytes caps a single dd:container_log_response payload.
	maxContainerLogBytes = 100 * 1024 * 1024 // 100 MiB
)

// followLogWindow and followLogGrace bound a dd:container_log_request with
// follow=true. The response is a single buffered string and cannot stream, so a
// follow request is served as a bounded live window: the daemon is asked to end
// the log stream at now+followLogWindow (a clean server-side EOF), and
// followLogGrace pads the handler's context deadline past that so the daemon's
// EOF wins in the normal case — the deadline only fires if an old daemon ignores
// `until`, preventing an indefinitely held handler semaphore slot. They are vars
// (not consts) only so tests can shrink the window; treat them as constants.
var (
	followLogWindow = 5 * time.Second
	followLogGrace  = 2 * time.Second
)

// Adapter is the Drydock adapter for Portwing. It provides container sync,
// component sync, watcher/trigger stubs, and SSE broadcasting.
type Adapter struct {
	containers   *adapter.ContainerManager
	sse          *SSEBroadcaster
	dockerClient *docker.Client

	// admit gates long-lived HTTP streams (SSE, follow-mode log tails)
	// against the server's shared stream concurrency limit. Set by
	// RegisterRoutes; the nil zero value is legal and always admits, which
	// keeps every test that builds an Adapter directly (without going
	// through RegisterRoutes) working unchanged.
	admit adapter.StreamAdmitter

	messageSem       chan struct{}
	semInit          sync.Once
	legacyLogSem     chan struct{}
	legacyLogSemInit sync.Once

	logStreamsMu sync.Mutex
	logStreams   map[string]activeContainerLogStream
}

type activeContainerLogStream struct {
	containerID string
	cancel      context.CancelFunc
}

// NewAdapter creates a Drydock adapter. info carries the static agent
// runtime details reported to the controller in the dd:ack event.
func NewAdapter(dockerClient *docker.Client, agentName string, info AgentInfo) *Adapter {
	cm := adapter.NewContainerManager(dockerClient, agentName, ParseLabels)
	return &Adapter{
		containers:   cm,
		sse:          NewSSEBroadcaster(cm, protocol.AgentVersion, info),
		dockerClient: dockerClient,
		messageSem:   make(chan struct{}, defaultMessageHandlerConcurrency),
		legacyLogSem: make(chan struct{}, 1),
		logStreams:   make(map[string]activeContainerLogStream),
	}
}

func (a *Adapter) Name() string { return "drydock" }

func (a *Adapter) Capabilities() []string {
	return []string{
		"dd:container-sync",
		"dd:logs",
	}
}

func (a *Adapter) HelloExtension() *adapter.HelloExtension {
	return &adapter.HelloExtension{
		DrydockCompat: protocol.DrydockCompat,
		WatcherTypes:  []string{"docker"},
		TriggerTypes:  []string{},
	}
}

func (a *Adapter) PollInterval() int { return 0 }

// OnConnect sends the component contract before the initial inventory after
// an edge-mode connection is established. Controllers must know which fields
// they own before ingesting Portwing's raw container state.
func (a *Adapter) OnConnect(ctx context.Context, sender adapter.MessageSender) error {
	a.sendComponentSync(sender)

	containers, err := a.containers.BuildInventory(ctx)
	if err != nil {
		slog.Warn("initial container sync failed", "error", err)
	} else {
		a.sendContainerSync(sender, containers)
	}
	return nil
}

// RefreshContainers delegates to the container manager.
func (a *Adapter) RefreshContainers(ctx context.Context) (added, updated, removed []adapter.Container, err error) {
	return a.containers.Refresh(ctx)
}

// OnContainerRefresh sends container events over the edge WebSocket when
// sender is non-nil (edge mode) and always broadcasts over SSE (standard mode).
func (a *Adapter) OnContainerRefresh(ctx context.Context, sender adapter.MessageSender, added, updated, removed []adapter.Container) error {
	// Edge-mode events (sender may be nil in standard mode).
	if sender != nil {
		for _, c := range added {
			a.sendContainerEvent(sender, protocol.TypeDDContainerAdded, c)
		}
		for _, c := range updated {
			a.sendContainerEvent(sender, protocol.TypeDDContainerUpdated, c)
		}
		for _, c := range removed {
			a.sendTypedMessage(sender, protocol.TypeDDContainerRemoved, protocol.DDContainerRemovedMessage{
				ID:   c.ID,
				Name: c.Name,
			})
		}
	}

	// SSE broadcasts (standard mode).
	for _, c := range added {
		a.sse.BroadcastContainerAdded(c)
	}
	for _, c := range updated {
		a.sse.BroadcastContainerUpdated(c)
	}
	for _, c := range removed {
		a.sse.BroadcastContainerRemoved(c.ID, c.Name)
	}

	// Full authoritative snapshot every poll cycle. Drydock relies on this
	// to prune containers that disappeared without a removal event.
	a.sse.BroadcastWatcherSnapshot()
	return nil
}

// HandleMessage handles Drydock-specific WebSocket message types.
func (a *Adapter) HandleMessage(ctx context.Context, sender adapter.MessageSender, msgType string, data json.RawMessage) bool {
	switch msgType {
	case protocol.TypeDDWatchRequest:
		var msg protocol.DDWatchRequestMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			slog.Warn("invalid watch_request message", "error", err)
			return true
		}
		if ctx.Err() == nil {
			a.handleWatchRequest(sender, msg)
		}
		return true

	case protocol.TypeDDWatchContainerRequest:
		var msg protocol.DDWatchContainerRequestMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			slog.Warn("invalid watch_container_request message", "error", err)
			return true
		}
		if ctx.Err() == nil {
			a.handleWatchContainerRequest(sender, msg)
		}
		return true

	case protocol.TypeDDTriggerRequest:
		var msg protocol.DDTriggerRequestMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			slog.Warn("invalid trigger_request message", "error", err)
			return true
		}
		if ctx.Err() == nil {
			a.handleTriggerRequest(sender, msg)
		}
		return true

	case protocol.TypeDDContainerLogRequest:
		var msg protocol.DDContainerLogRequestMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			slog.Warn("invalid container_log_request message", "error", err)
			return true
		}
		if msg.Stream {
			a.startContainerLogStream(ctx, sender, msg)
			return true
		}
		if ctx.Err() != nil {
			return true
		}
		legacyLogSem := a.getLegacyLogSemaphore()
		select {
		case legacyLogSem <- struct{}{}:
		case <-ctx.Done():
			return true
		default:
			a.sendTypedMessage(sender, protocol.TypeDDContainerLogResponse, protocol.DDContainerLogResponseMessage{
				RequestID:   msg.RequestID,
				ContainerID: msg.ContainerID,
				Logs:        "error: another buffered log request is active",
			})
			return true
		}
		if !a.spawnMessageHandlerWithCancelCleanup(ctx, msgType, func() {
			defer func() { <-legacyLogSem }()
			a.handleContainerLogRequest(ctx, sender, msg)
		}, func() { <-legacyLogSem }) {
			<-legacyLogSem
			if ctx.Err() == nil {
				a.sendTypedMessage(sender, protocol.TypeDDContainerLogResponse, protocol.DDContainerLogResponseMessage{
					RequestID:   msg.RequestID,
					ContainerID: msg.ContainerID,
					Logs:        "error: agent busy: too many active requests",
				})
			}
		}
		return true

	case protocol.TypeDDContainerLogCancel:
		var msg protocol.DDContainerLogCancelMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			slog.Warn("invalid container_log_cancel message", "error", err)
			return true
		}
		a.cancelContainerLogStream(msg)
		return true

	case protocol.TypeDDContainerDeleteRequest:
		var msg protocol.DDContainerDeleteRequestMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			slog.Warn("invalid container_delete_request message", "error", err)
			return true
		}
		if !a.spawnMessageHandler(ctx, msgType, func() {
			a.handleContainerDeleteRequest(ctx, sender, msg)
		}) && ctx.Err() == nil {
			a.sendTypedMessage(sender, protocol.TypeDDContainerDeleteResponse, protocol.DDContainerDeleteResponseMessage{
				RequestID:   msg.RequestID,
				ContainerID: msg.ContainerID,
				Success:     false,
				Error:       "agent busy: too many active requests",
			})
		}
		return true
	}

	return false
}

// Containers returns the underlying container manager for use by routes.
func (a *Adapter) Containers() *adapter.ContainerManager { return a.containers }

// SSE returns the SSE broadcaster for use by routes.
func (a *Adapter) SSE() *SSEBroadcaster { return a.sse }

// DockerClient returns the docker client for use by routes.
func (a *Adapter) DockerClient() *docker.Client { return a.dockerClient }

// ParseLabels extracts Drydock-specific labels from a container's label map.
func ParseLabels(labels map[string]string) adapter.LabelResult {
	return adapter.LabelResult{
		DisplayName:   labels[LabelDisplayName],
		DisplayIcon:   labels[LabelDisplayIcon],
		IncludeTags:   labels[LabelTagInclude],
		ExcludeTags:   labels[LabelTagExclude],
		TransformTags: labels[LabelTagTransform],
		Watcher:       labels[LabelWatch],
	}
}

// GetWatcherComponents returns the registered watcher component descriptors.
func GetWatcherComponents() []protocol.ComponentDescriptor {
	return []protocol.ComponentDescriptor{
		{
			Type: "docker",
			Name: "docker",
			Configuration: map[string]any{
				"description":  "Watches Docker containers for updates via Docker Engine API",
				"capabilities": []string{"container-sync", "labels"},
				"transport":    "docker-api",
				"execution":    "controller",
				"events":       "portwing",
			},
		},
	}
}

// GetTriggerComponents returns the registered trigger component descriptors.
func GetTriggerComponents() []protocol.ComponentDescriptor {
	// No triggers in v1.0 - agent-side triggering deferred to v2.0
	return []protocol.ComponentDescriptor{}
}

// --- internal handlers ---

func (a *Adapter) handleWatchRequest(sender adapter.MessageSender, msg protocol.DDWatchRequestMessage) {
	a.sendTypedMessage(sender, protocol.TypeDDWatchResponse, protocol.DDWatchResponseMessage{
		WatcherType: msg.WatcherType,
		WatcherName: msg.WatcherName,
		Results:     []json.RawMessage{},
	})
}

func (a *Adapter) handleWatchContainerRequest(sender adapter.MessageSender, msg protocol.DDWatchContainerRequestMessage) {
	a.sendTypedMessage(sender, protocol.TypeDDWatchContainerResponse, protocol.DDWatchContainerResponseMessage{
		WatcherType: msg.WatcherType,
		WatcherName: msg.WatcherName,
		ContainerID: msg.ContainerID,
		Result:      nil,
	})
}

func (a *Adapter) handleTriggerRequest(sender adapter.MessageSender, msg protocol.DDTriggerRequestMessage) {
	a.sendTypedMessage(sender, protocol.TypeDDTriggerResponse, protocol.DDTriggerResponseMessage{
		TriggerType: msg.TriggerType,
		TriggerName: msg.TriggerName,
		Success:     false,
		Message:     "triggers not implemented in v1.0",
	})
}

func (a *Adapter) handleContainerLogRequest(ctx context.Context, sender adapter.MessageSender, msg protocol.DDContainerLogRequestMessage) {
	tail := ""
	if msg.Tail > 0 {
		tail = fmt.Sprintf("%d", msg.Tail)
	}

	until := msg.Until
	if msg.Follow {
		// A dd:container_log_response carries a single buffered Logs string, so
		// it cannot stream a live tail. Honor Follow as a bounded live window:
		// portwing owns the end time so the handler always returns promptly and
		// never holds a message-handler semaphore slot indefinitely. Override
		// `until` with now+followLogWindow as Unix seconds — the grammar the
		// Docker daemon parses for `until` (RFC3339 is rejected). A
		// caller-supplied `until` is intentionally ignored on a follow request
		// so the daemon's EOF and the context deadline below always agree;
		// continuous tailing belongs on the stream=true dd:container_log_chunk
		// path, not on this legacy response pair. The context deadline is a
		// backstop in case an old daemon ignores `until`.
		until = strconv.FormatInt(time.Now().Add(followLogWindow).Unix(), 10)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, followLogWindow+followLogGrace)
		defer cancel()
	}

	body, err := a.dockerClient.GetContainerLogs(ctx, msg.ContainerID, tail, msg.Since, until, msg.Follow, msg.Timestamps)
	if err != nil {
		slog.Warn("failed to get container logs", "container", applog.Sanitize(msg.ContainerID), "error", applog.Sanitize(err.Error()))
		a.sendTypedMessage(sender, protocol.TypeDDContainerLogResponse, protocol.DDContainerLogResponseMessage{
			RequestID:   msg.RequestID,
			ContainerID: msg.ContainerID,
			Logs:        fmt.Sprintf("error: %v", err),
		})
		return
	}
	defer body.Close()

	// Decode to plain text: strip Docker's 8-byte multiplex frame headers for a
	// non-TTY container, or pass a TTY container's raw stream through unchanged
	// (demuxing raw output would corrupt it). Matches what the HTTP /logs route
	// returns for the same container. A trailing read error (EOF, or the follow
	// window's deadline firing mid-read) still yields the bytes read so far, so
	// use them and keep the error at debug.
	data, err := docker.DecodeContainerLogs(io.LimitReader(body, maxContainerLogBytes))
	if err != nil {
		slog.Debug("decoding container logs", "container", msg.ContainerID, "error", err)
	}

	a.sendTypedMessage(sender, protocol.TypeDDContainerLogResponse, protocol.DDContainerLogResponseMessage{
		RequestID:   msg.RequestID,
		ContainerID: msg.ContainerID,
		Logs:        string(data),
	})
}

func (a *Adapter) startContainerLogStream(ctx context.Context, sender adapter.MessageSender, msg protocol.DDContainerLogRequestMessage) {
	if msg.RequestID == "" || msg.ContainerID == "" {
		a.sendContainerLogStreamError(sender, msg, "requestId and containerId are required")
		return
	}

	streamCtx, cancel := context.WithCancel(ctx)
	active := activeContainerLogStream{
		containerID: msg.ContainerID,
		cancel:      cancel,
	}

	a.logStreamsMu.Lock()
	if a.logStreams == nil {
		a.logStreams = make(map[string]activeContainerLogStream)
	}
	switch {
	case len(a.logStreams) >= maxContainerLogStreams:
		a.logStreamsMu.Unlock()
		cancel()
		a.sendContainerLogStreamError(sender, msg, "too many active container log streams")
		return
	case a.logStreams[msg.RequestID].cancel != nil:
		a.logStreamsMu.Unlock()
		cancel()
		a.sendContainerLogStreamError(sender, msg, "duplicate requestId")
		return
	default:
		a.logStreams[msg.RequestID] = active
		a.logStreamsMu.Unlock()
	}

	go a.runContainerLogStream(streamCtx, cancel, sender, msg)
}

func (a *Adapter) cancelContainerLogStream(msg protocol.DDContainerLogCancelMessage) {
	a.logStreamsMu.Lock()
	active, ok := a.logStreams[msg.RequestID]
	a.logStreamsMu.Unlock()
	if !ok || (msg.ContainerID != "" && active.containerID != msg.ContainerID) {
		return
	}
	active.cancel()
}

func (a *Adapter) runContainerLogStream(ctx context.Context, cancel context.CancelFunc, sender adapter.MessageSender, msg protocol.DDContainerLogRequestMessage) {
	defer func() {
		a.logStreamsMu.Lock()
		delete(a.logStreams, msg.RequestID)
		a.logStreamsMu.Unlock()
		// streamCtx is a context.WithCancel child of the connection-lifetime
		// pumpCtx: the child only unregisters itself from the parent's
		// cancelCtx.children map when its own CancelFunc runs (context
		// package internals), so a stream that ends on its own — EOF, a
		// daemon error, a failed send — must still call cancel here or its
		// child registration leaks on pumpCtx for the rest of the WebSocket
		// connection. Safe to call unconditionally: cancel is idempotent, so
		// this is a no-op on the already-canceled path.
		cancel()
	}()

	tail := ""
	if msg.Tail > 0 {
		tail = strconv.Itoa(msg.Tail)
	}
	body, err := a.dockerClient.GetContainerLogs(
		ctx,
		msg.ContainerID,
		tail,
		msg.Since,
		msg.Until,
		msg.Follow,
		msg.Timestamps,
	)
	if err != nil {
		if ctx.Err() != nil {
			a.sendContainerLogStreamEnd(sender, msg, "canceled")
			return
		}
		slog.Warn(
			"failed to open container log stream",
			"container", applog.Sanitize(msg.ContainerID),
			"error", applog.Sanitize(err.Error()),
		)
		a.sendContainerLogStreamError(sender, msg, err.Error())
		return
	}
	defer body.Close()

	if err := a.forwardContainerLogStream(ctx, sender, msg, body); err != nil {
		if ctx.Err() != nil {
			a.sendContainerLogStreamEnd(sender, msg, "canceled")
			return
		}
		a.sendContainerLogStreamError(sender, msg, err.Error())
		return
	}
	a.sendContainerLogStreamEnd(sender, msg, "eof")
}

// forwardContainerLogStream pumps a container log response into
// dd:container_log_chunk messages. Decoding is the shared
// docker.DecodeContainerLogStream, which is what the HTTP log routes and the
// MCP tool already use: it tells a multiplexed stream from a raw TTY one the
// same way, and — the reason this stopped being a local copy — it emits the
// bytes it did read from a truncated final frame before reporting the read
// error, where the copy here dropped them.
func (a *Adapter) forwardContainerLogStream(
	ctx context.Context,
	sender adapter.MessageSender,
	msg protocol.DDContainerLogRequestMessage,
	body io.Reader,
) error {
	err := docker.DecodeContainerLogStream(body, func(stream docker.ContainerLogStream, payload []byte) error {
		name := "stdout"
		if stream == docker.ContainerLogStderr {
			name = "stderr"
		}
		return a.sendContainerLogStreamChunk(ctx, sender, msg, name, payload)
	})
	if err != nil {
		return fmt.Errorf("forwarding container log stream: %w", err)
	}
	return nil
}

func (a *Adapter) sendContainerLogStreamChunk(
	ctx context.Context,
	sender adapter.MessageSender,
	msg protocol.DDContainerLogRequestMessage,
	stream string,
	logs []byte,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sender == nil {
		return errors.New("container log stream sender is nil")
	}
	if err := sender.SendTypedMessage(protocol.TypeDDContainerLogChunk, protocol.DDContainerLogChunkMessage{
		RequestID:   msg.RequestID,
		ContainerID: msg.ContainerID,
		Stream:      stream,
		Logs:        string(logs),
	}); err != nil {
		return fmt.Errorf("sending container log chunk: %w", err)
	}
	return nil
}

func (a *Adapter) sendContainerLogStreamEnd(sender adapter.MessageSender, msg protocol.DDContainerLogRequestMessage, reason string) {
	a.sendTypedMessage(sender, protocol.TypeDDContainerLogEnd, protocol.DDContainerLogEndMessage{
		RequestID:   msg.RequestID,
		ContainerID: msg.ContainerID,
		Reason:      reason,
	})
}

func (a *Adapter) sendContainerLogStreamError(sender adapter.MessageSender, msg protocol.DDContainerLogRequestMessage, streamError string) {
	a.sendTypedMessage(sender, protocol.TypeDDContainerLogError, protocol.DDContainerLogErrorMessage{
		RequestID:   msg.RequestID,
		ContainerID: msg.ContainerID,
		Error:       streamError,
	})
}

func (a *Adapter) handleContainerDeleteRequest(ctx context.Context, sender adapter.MessageSender, msg protocol.DDContainerDeleteRequestMessage) {
	err := a.dockerClient.RemoveContainer(ctx, msg.ContainerID, true)
	if err != nil {
		slog.Warn("failed to delete container", "container", applog.Sanitize(msg.ContainerID), "error", applog.Sanitize(err.Error()))
		a.sendTypedMessage(sender, protocol.TypeDDContainerDeleteResponse, protocol.DDContainerDeleteResponseMessage{
			RequestID:   msg.RequestID,
			ContainerID: msg.ContainerID,
			Success:     false,
			Error:       err.Error(),
		})
		return
	}

	a.sendTypedMessage(sender, protocol.TypeDDContainerDeleteResponse, protocol.DDContainerDeleteResponseMessage{
		RequestID:   msg.RequestID,
		ContainerID: msg.ContainerID,
		Success:     true,
	})
}

func (a *Adapter) sendContainerSync(sender adapter.MessageSender, containers []adapter.Container) {
	a.sendTypedMessage(sender, protocol.TypeDDContainerSync, struct {
		Containers []drydockContainer `json:"containers"`
	}{
		Containers: toDrydockContainers(containers),
	})
}

func (a *Adapter) sendComponentSync(sender adapter.MessageSender) {
	watchers := GetWatcherComponents()
	triggers := GetTriggerComponents()

	protoWatchers := make([]protocol.ComponentDescriptor, len(watchers))
	for i, w := range watchers {
		protoWatchers[i] = protocol.ComponentDescriptor{
			Type:          w.Type,
			Name:          w.Name,
			Configuration: w.Configuration,
		}
	}
	protoTriggers := make([]protocol.ComponentDescriptor, len(triggers))
	for i, t := range triggers {
		protoTriggers[i] = protocol.ComponentDescriptor{
			Type:          t.Type,
			Name:          t.Name,
			Configuration: t.Configuration,
		}
	}

	a.sendTypedMessage(sender, protocol.TypeDDComponentSync, protocol.DDComponentSyncMessage{
		Watchers: protoWatchers,
		Triggers: protoTriggers,
	})
}

func (a *Adapter) sendContainerEvent(sender adapter.MessageSender, msgType string, container adapter.Container) {
	data, err := json.Marshal(toDrydockContainer(container))
	if err != nil {
		slog.Warn("failed to marshal container event", "id", container.ID, "error", err)
		return
	}

	switch msgType {
	case protocol.TypeDDContainerAdded:
		a.sendTypedMessage(sender, msgType, protocol.DDContainerAddedMessage{
			Container: json.RawMessage(data),
		})
	case protocol.TypeDDContainerUpdated:
		a.sendTypedMessage(sender, msgType, protocol.DDContainerUpdatedMessage{
			Container: json.RawMessage(data),
		})
	}
}

func (a *Adapter) sendTypedMessage(sender adapter.MessageSender, msgType string, data any) {
	if sender == nil {
		slog.Warn("failed to send typed message: sender is nil", "type", msgType)
		return
	}

	if err := sender.SendTypedMessage(msgType, data); err != nil {
		slog.Warn("failed to send typed message", "type", msgType, "error", err)
	}
}

func (a *Adapter) spawnMessageHandler(ctx context.Context, msgType string, fn func()) bool {
	return a.spawnMessageHandlerWithCancelCleanup(ctx, msgType, fn, nil)
}

func (a *Adapter) spawnMessageHandlerWithCancelCleanup(ctx context.Context, msgType string, fn, cancelCleanup func()) bool {
	sem := a.getMessageSemaphore()

	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		slog.Debug("skipping message handler due to canceled context", "type", applog.Sanitize(msgType), "error", applog.Sanitize(ctx.Err().Error()))
		return false
	default:
		return false
	}

	go func() {
		defer func() { <-sem }()

		if ctx.Err() != nil {
			if cancelCleanup != nil {
				cancelCleanup()
			}
			return
		}

		fn()
	}()
	return true
}

func (a *Adapter) getMessageSemaphore() chan struct{} {
	a.semInit.Do(func() {
		if a.messageSem == nil {
			a.messageSem = make(chan struct{}, defaultMessageHandlerConcurrency)
		}
	})
	return a.messageSem
}

func (a *Adapter) getLegacyLogSemaphore() chan struct{} {
	a.legacyLogSemInit.Do(func() {
		if a.legacyLogSem == nil {
			a.legacyLogSem = make(chan struct{}, 1)
		}
	})
	return a.legacyLogSem
}
