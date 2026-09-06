package drydock

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/codeswhat/portwing/internal/protocol"
)

type logStreamTestEvent struct {
	msgType string
	data    json.RawMessage
}

type logStreamTestSender struct {
	events chan logStreamTestEvent
}

func newLogStreamTestSender() *logStreamTestSender {
	return &logStreamTestSender{events: make(chan logStreamTestEvent, 16)}
}

func (s *logStreamTestSender) SendTypedMessage(msgType string, data any) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	s.events <- logStreamTestEvent{msgType: msgType, data: encoded}
	return nil
}

func waitForLogStreamEvent(t *testing.T, sender *logStreamTestSender) logStreamTestEvent {
	t.Helper()

	select {
	case event := <-sender.events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for log stream event")
		return logStreamTestEvent{}
	}
}

func waitForDockerLogCall(t *testing.T, calls *routeTestDockerCalls) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for calls.logsCalls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Docker log request")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForLogCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

type errObservedContext struct {
	context.Context
	once     sync.Once
	observed chan struct{}
	resume   chan struct{}
}

func (c *errObservedContext) Err() error {
	var first bool
	var firstErr error
	c.once.Do(func() {
		first = true
		firstErr = c.Context.Err()
		close(c.observed)
		<-c.resume
	})
	if first {
		return firstErr
	}
	return c.Context.Err()
}

type nthErrObservedContext struct {
	context.Context
	mu          sync.Mutex
	calls       int
	observeCall int
	observed    chan struct{}
	resume      chan struct{}
}

func (c *nthErrObservedContext) Err() error {
	c.mu.Lock()
	c.calls++
	observe := c.calls == c.observeCall
	c.mu.Unlock()

	if observe {
		close(c.observed)
		<-c.resume
	}
	return c.Context.Err()
}

func TestContainerLogStreamPreservesStdoutStderrAndEnds(t *testing.T) {
	t.Parallel()

	client, calls, shutdown := newRouteTestDockerClient(t)
	defer shutdown()
	calls.setLogsResponse("container-1", append(
		routeTestDockerLogFrame(1, []byte("out\n")),
		routeTestDockerLogFrame(2, []byte("err\n"))...,
	))

	a := NewAdapter(client, "test-agent", AgentInfo{})
	sender := newLogStreamTestSender()
	payload := json.RawMessage(`{
		"requestId":"stream-1",
		"containerId":"container-1",
		"stream":true,
		"follow":true,
		"tail":25,
		"timestamps":true
	}`)

	if !a.HandleMessage(context.Background(), sender, protocol.TypeDDContainerLogRequest, payload) {
		t.Fatal("expected streaming dd:container_log_request to be handled")
	}

	for _, want := range []struct {
		msgType string
		stream  string
		logs    string
	}{
		{msgType: "dd:container_log_chunk", stream: "stdout", logs: "out\n"},
		{msgType: "dd:container_log_chunk", stream: "stderr", logs: "err\n"},
	} {
		event := waitForLogStreamEvent(t, sender)
		if event.msgType != want.msgType {
			t.Fatalf("message type = %q, want %q", event.msgType, want.msgType)
		}

		var got struct {
			RequestID   string `json:"requestId"`
			ContainerID string `json:"containerId"`
			Stream      string `json:"stream"`
			Logs        string `json:"logs"`
		}
		if err := json.Unmarshal(event.data, &got); err != nil {
			t.Fatalf("decode chunk: %v", err)
		}
		if got.RequestID != "stream-1" || got.ContainerID != "container-1" ||
			got.Stream != want.stream || got.Logs != want.logs {
			t.Fatalf("chunk = %+v, want request/container %q/%q stream/logs %q/%q",
				got, "stream-1", "container-1", want.stream, want.logs)
		}
	}

	end := waitForLogStreamEvent(t, sender)
	if end.msgType != "dd:container_log_end" {
		t.Fatalf("message type = %q, want dd:container_log_end", end.msgType)
	}
	var endData struct {
		RequestID   string `json:"requestId"`
		ContainerID string `json:"containerId"`
	}
	if err := json.Unmarshal(end.data, &endData); err != nil {
		t.Fatalf("decode end: %v", err)
	}
	if endData.RequestID != "stream-1" || endData.ContainerID != "container-1" {
		t.Fatalf("end = %+v, want stream-1/container-1", endData)
	}

	query := lastLogsQuery(t, calls)
	if query.Get("follow") != "1" || query.Get("tail") != "25" || query.Get("timestamps") != "1" {
		t.Fatalf("Docker log query = %q, want follow=1 tail=25 timestamps=1", query.Encode())
	}
}

func TestContainerLogStreamCancelStopsActiveRequest(t *testing.T) {
	t.Parallel()

	client, calls, shutdown := newRouteTestDockerClient(t)
	defer shutdown()
	calls.setBlockingLogsResponse("container-1", routeTestDockerLogFrame(1, []byte("live\n")))

	a := NewAdapter(client, "test-agent", AgentInfo{})
	sender := newLogStreamTestSender()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	request := json.RawMessage(`{
		"requestId":"stream-cancel",
		"containerId":"container-1",
		"stream":true,
		"follow":true
	}`)
	if !a.HandleMessage(ctx, sender, protocol.TypeDDContainerLogRequest, request) {
		t.Fatal("expected streaming dd:container_log_request to be handled")
	}
	waitForDockerLogCall(t, calls)

	cancelMessage := json.RawMessage(`{"requestId":"stream-cancel","containerId":"container-1"}`)
	if !a.HandleMessage(ctx, sender, "dd:container_log_cancel", cancelMessage) {
		t.Fatal("expected dd:container_log_cancel to be handled")
	}

	for {
		event := waitForLogStreamEvent(t, sender)
		if event.msgType == "dd:container_log_end" {
			return
		}
	}
}

func TestContainerLogStreamOpenFailureUsesTerminalError(t *testing.T) {
	t.Parallel()

	client, _, shutdown := newRouteTestDockerClient(t)
	defer shutdown()

	a := NewAdapter(client, "test-agent", AgentInfo{})
	sender := newLogStreamTestSender()
	payload := json.RawMessage(`{
		"requestId":"stream-error",
		"containerId":"../invalid",
		"stream":true,
		"follow":true
	}`)

	if !a.HandleMessage(context.Background(), sender, protocol.TypeDDContainerLogRequest, payload) {
		t.Fatal("expected streaming dd:container_log_request to be handled")
	}

	event := waitForLogStreamEvent(t, sender)
	if event.msgType != "dd:container_log_error" {
		t.Fatalf("message type = %q, want dd:container_log_error", event.msgType)
	}
	var got struct {
		RequestID   string `json:"requestId"`
		ContainerID string `json:"containerId"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(event.data, &got); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if got.RequestID != "stream-error" || got.ContainerID != "../invalid" || got.Error == "" {
		t.Fatalf("error = %+v, want matching request/container and non-empty error", got)
	}
}

func TestContainerLogStreamRawTTYUsesStdoutChunk(t *testing.T) {
	t.Parallel()

	client, calls, shutdown := newRouteTestDockerClient(t)
	defer shutdown()
	calls.setLogsResponse("container-1", []byte("raw tty output\n"))

	a := NewAdapter(client, "test-agent", AgentInfo{})
	sender := newLogStreamTestSender()
	payload := json.RawMessage(`{
		"requestId":"stream-tty",
		"containerId":"container-1",
		"stream":true
	}`)
	if !a.HandleMessage(context.Background(), sender, protocol.TypeDDContainerLogRequest, payload) {
		t.Fatal("expected streaming dd:container_log_request to be handled")
	}

	event := waitForLogStreamEvent(t, sender)
	if event.msgType != protocol.TypeDDContainerLogChunk {
		t.Fatalf("message type = %q, want %q", event.msgType, protocol.TypeDDContainerLogChunk)
	}
	var got protocol.DDContainerLogChunkMessage
	if err := json.Unmarshal(event.data, &got); err != nil {
		t.Fatalf("decode chunk: %v", err)
	}
	if got.Stream != "stdout" || got.Logs != "raw tty output\n" {
		t.Fatalf("chunk = %+v, want raw output on stdout", got)
	}
}

func TestContainerLogStreamRejectsDuplicateAndCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fill func(*Adapter)
	}{
		{
			name: "duplicate request ID",
			fill: func(a *Adapter) {
				_, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				a.logStreams["stream-rejected"] = activeContainerLogStream{
					containerID: "container-1",
					cancel:      cancel,
				}
			},
		},
		{
			name: "stream capacity reached",
			fill: func(a *Adapter) {
				for i := 0; i < maxContainerLogStreams; i++ {
					_, cancel := context.WithCancel(context.Background())
					t.Cleanup(cancel)
					a.logStreams[time.Unix(int64(i), 0).String()] = activeContainerLogStream{
						containerID: "container-1",
						cancel:      cancel,
					}
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{logStreams: make(map[string]activeContainerLogStream)}
			tc.fill(a)
			sender := newLogStreamTestSender()
			payload := json.RawMessage(`{
				"requestId":"stream-rejected",
				"containerId":"container-1",
				"stream":true
			}`)

			if !a.HandleMessage(context.Background(), sender, protocol.TypeDDContainerLogRequest, payload) {
				t.Fatal("expected streaming dd:container_log_request to be handled")
			}
			event := waitForLogStreamEvent(t, sender)
			if event.msgType != protocol.TypeDDContainerLogError {
				t.Fatalf("message type = %q, want %q", event.msgType, protocol.TypeDDContainerLogError)
			}
		})
	}
}

// Legacy log requests buffer up to maxContainerLogBytes before sending one
// response. They need a dedicated single-request admission limit so the broad
// message-handler pool cannot retain many maximum-sized bodies at once.
func TestLegacyContainerLogRequestsLimitBufferedConcurrency(t *testing.T) {
	client, calls, shutdown := newRouteTestDockerClient(t)
	defer shutdown()
	calls.setBlockingLogsResponse("container-1", routeTestDockerLogFrame(1, []byte("live\n")))

	a := NewAdapter(client, "test-agent", AgentInfo{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender := newLogStreamTestSender()

	const attempts = 4
	ready := make(chan struct{}, attempts)
	start := make(chan struct{})
	returned := make(chan struct{}, attempts)
	for i := range attempts {
		requestID := fmt.Sprintf("legacy-buffered-%d", i)
		payload, err := json.Marshal(protocol.DDContainerLogRequestMessage{
			RequestID:   requestID,
			ContainerID: "container-1",
		})
		if err != nil {
			t.Fatalf("marshal legacy log request: %v", err)
		}
		go func() {
			ready <- struct{}{}
			<-start
			a.HandleMessage(ctx, sender, protocol.TypeDDContainerLogRequest, payload)
			returned <- struct{}{}
		}()
	}
	for range attempts {
		<-ready
	}
	close(start)
	waitForDockerLogCall(t, calls)

	deadline := time.Now().Add(300 * time.Millisecond)
	for calls.logsCalls.Load() == 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := calls.logsCalls.Load(); got != 1 {
		t.Fatalf("concurrent legacy buffered Docker log requests = %d, want 1", got)
	}

	for range attempts {
		select {
		case <-returned:
		case <-time.After(2 * time.Second):
			t.Fatal("legacy log HandleMessage did not return promptly")
		}
	}

	// Exactly three requests must have received an explicit overload response;
	// the admitted request is still blocked in Docker and cannot have replied.
	for i := 0; i < attempts-1; i++ {
		event := waitForLogStreamEvent(t, sender)
		var response protocol.DDContainerLogResponseMessage
		if err := json.Unmarshal(event.data, &response); err != nil {
			t.Fatalf("decode overload response: %v", err)
		}
		if event.msgType != protocol.TypeDDContainerLogResponse ||
			!strings.Contains(response.Logs, "another buffered log request is active") {
			t.Fatalf("overload response = %s %+v", event.msgType, response)
		}
	}

	// Cancel the admitted body, observe its partial response, and verify the
	// dedicated admission slot is released for a later request.
	cancel()
	admitted := waitForLogStreamEvent(t, sender)
	var admittedResponse protocol.DDContainerLogResponseMessage
	if err := json.Unmarshal(admitted.data, &admittedResponse); err != nil {
		t.Fatalf("decode admitted response: %v", err)
	}
	if strings.Contains(admittedResponse.Logs, "another buffered log request is active") {
		t.Fatalf("admitted request received overload response: %+v", admittedResponse)
	}

	waitForLogCondition(t, "legacy buffered log admission release", func() bool {
		return len(a.getLegacyLogSemaphore()) == 0
	})
	calls.setLogsResponse("container-1", routeTestDockerLogFrame(1, []byte("next\n")))
	nextPayload := json.RawMessage(`{"requestId":"legacy-next","containerId":"container-1"}`)
	if !a.HandleMessage(context.Background(), sender, protocol.TypeDDContainerLogRequest, nextPayload) {
		t.Fatal("later legacy log request was not handled")
	}
	waitForLogCondition(t, "later legacy Docker log request", func() bool {
		return calls.logsCalls.Load() == 2
	})
	next := waitForLogStreamEvent(t, sender)
	var nextResponse protocol.DDContainerLogResponseMessage
	if err := json.Unmarshal(next.data, &nextResponse); err != nil {
		t.Fatalf("decode later response: %v", err)
	}
	if nextResponse.RequestID != "legacy-next" || nextResponse.Logs != "next\n" {
		t.Fatalf("later response = %+v, want legacy-next / next newline", nextResponse)
	}
}

func TestLegacyContainerLogRequestStopsWhileWaitingForAdmission(t *testing.T) {
	t.Parallel()

	a := &Adapter{}
	legacyLogSem := a.getLegacyLogSemaphore()
	legacyLogSem <- struct{}{}
	t.Cleanup(func() { <-legacyLogSem })

	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := &errObservedContext{
		Context:  baseCtx,
		observed: make(chan struct{}),
		resume:   make(chan struct{}),
	}
	returned := make(chan bool, 1)
	go func() {
		returned <- a.HandleMessage(
			ctx,
			newLogStreamTestSender(),
			protocol.TypeDDContainerLogRequest,
			json.RawMessage(`{"requestId":"waiting","containerId":"container-1"}`),
		)
	}()

	select {
	case <-ctx.observed:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach the admission check")
	}
	cancel()
	close(ctx.resume)

	select {
	case handled := <-returned:
		if !handled {
			t.Fatal("canceled legacy log request was not recognized")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("legacy log request did not stop after cancellation")
	}
	if got := len(legacyLogSem); got != 1 {
		t.Fatalf("existing legacy admission reservation = %d, want 1", got)
	}
}

func TestLegacyContainerLogAdmissionReleasedWhenHandlerPoolCanceled(t *testing.T) {
	t.Parallel()

	a := NewAdapter(nil, "test-agent", AgentInfo{})
	for i := 0; i < cap(a.messageSem); i++ {
		a.messageSem <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan bool, 1)
	go func() {
		returned <- a.HandleMessage(
			ctx,
			newLogStreamTestSender(),
			protocol.TypeDDContainerLogRequest,
			json.RawMessage(`{"requestId":"handler-wait","containerId":"container-1"}`),
		)
	}()

	waitForLogCondition(t, "legacy log admission", func() bool {
		return len(a.getLegacyLogSemaphore()) == 1
	})
	cancel()

	select {
	case handled := <-returned:
		if !handled {
			t.Fatal("canceled legacy log request was not recognized")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("legacy log request did not stop after handler-pool cancellation")
	}
	if got := len(a.getLegacyLogSemaphore()); got != 0 {
		t.Fatalf("legacy admission reservations = %d after cancellation, want 0", got)
	}
}

func TestLegacyContainerLogAdmissionReleasedWhenSpawnedHandlerIsCanceled(t *testing.T) {
	t.Parallel()

	a := NewAdapter(nil, "test-agent", AgentInfo{})
	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := &nthErrObservedContext{
		Context:     baseCtx,
		observeCall: 2,
		observed:    make(chan struct{}),
		resume:      make(chan struct{}),
	}

	if !a.HandleMessage(
		ctx,
		newLogStreamTestSender(),
		protocol.TypeDDContainerLogRequest,
		json.RawMessage(`{"requestId":"spawned","containerId":"container-1"}`),
	) {
		t.Fatal("legacy log request was not recognized")
	}

	select {
	case <-ctx.observed:
	case <-time.After(2 * time.Second):
		t.Fatal("spawned handler did not reach its cancellation check")
	}
	cancel()
	close(ctx.resume)

	waitForLogCondition(t, "message handler admission release", func() bool {
		return len(a.getMessageSemaphore()) == 0
	})
	if got := len(a.getLegacyLogSemaphore()); got != 0 {
		t.Fatalf("legacy admission reservations = %d after spawned handler cancellation, want 0", got)
	}
}

func TestContainerLogStreamSenderFailureReleasesSlot(t *testing.T) {
	t.Parallel()

	client, _, shutdown := newRouteTestDockerClient(t)
	defer shutdown()

	a := NewAdapter(client, "test-agent", AgentInfo{})
	sender := &failingSender{err: errors.New("send failed")}
	payload := json.RawMessage(`{
		"requestId":"stream-send-error",
		"containerId":"container-1",
		"stream":true
	}`)
	if !a.HandleMessage(context.Background(), sender, protocol.TypeDDContainerLogRequest, payload) {
		t.Fatal("expected streaming dd:container_log_request to be handled")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		a.logStreamsMu.Lock()
		active := len(a.logStreams)
		a.logStreamsMu.Unlock()
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("active log streams = %d after sender failure, want 0", active)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestContainerLogRequestStreamFieldRoundTrips(t *testing.T) {
	t.Parallel()

	var request protocol.DDContainerLogRequestMessage
	if err := json.Unmarshal([]byte(`{"requestId":"stream-1","containerId":"container-1","stream":true}`), &request); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal encoded request: %v", err)
	}
	if got["stream"] != true {
		t.Fatalf("stream field = %v, want true; encoded request: %s", got["stream"], encoded)
	}
}

func TestContainerLogStreamRejectsMissingIdentityAndMismatchedCancel(t *testing.T) {
	t.Parallel()

	a := &Adapter{}
	sender := newLogStreamTestSender()
	a.startContainerLogStream(
		context.Background(),
		sender,
		protocol.DDContainerLogRequestMessage{RequestID: "missing-container"},
	)
	event := waitForLogStreamEvent(t, sender)
	if event.msgType != protocol.TypeDDContainerLogError {
		t.Fatalf("message type = %q, want %q", event.msgType, protocol.TypeDDContainerLogError)
	}

	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.logStreams = map[string]activeContainerLogStream{
		"stream-1": {
			containerID: "container-1",
			cancel:      cancel,
		},
	}
	a.cancelContainerLogStream(protocol.DDContainerLogCancelMessage{
		RequestID:   "stream-1",
		ContainerID: "different-container",
	})
	select {
	case <-streamCtx.Done():
		t.Fatal("mismatched container ID canceled the active stream")
	default:
	}
}

func TestForwardContainerLogStreamFrameFailures(t *testing.T) {
	t.Parallel()

	a := &Adapter{}
	msg := protocol.DDContainerLogRequestMessage{
		RequestID:   "stream-1",
		ContainerID: "container-1",
	}

	zeroLengthFrame := make([]byte, 8)
	zeroLengthFrame[0] = 1
	if err := a.forwardContainerLogStream(
		context.Background(),
		nil,
		msg,
		bytes.NewReader(zeroLengthFrame),
	); err != nil {
		t.Fatalf("zero-length frame: %v", err)
	}

	oversizedFrame := make([]byte, 8)
	oversizedFrame[0] = 1
	binary.BigEndian.PutUint32(oversizedFrame[4:8], maxContainerLogStreamFrameBytes+1)
	if err := a.forwardContainerLogStream(
		context.Background(),
		nil,
		msg,
		bytes.NewReader(oversizedFrame),
	); err == nil {
		t.Fatal("expected truncated oversized frame to fail")
	}

	truncatedFrame := make([]byte, 9)
	truncatedFrame[0] = 1
	binary.BigEndian.PutUint32(truncatedFrame[4:8], 4)
	if err := a.forwardContainerLogStream(
		context.Background(),
		nil,
		msg,
		bytes.NewReader(truncatedFrame),
	); err == nil {
		t.Fatal("expected truncated frame payload to fail")
	}
}

func TestForwardRawContainerLogStreamReaderAndSenderFailures(t *testing.T) {
	t.Parallel()

	a := &Adapter{}
	msg := protocol.DDContainerLogRequestMessage{
		RequestID:   "stream-1",
		ContainerID: "container-1",
	}
	readErr := errors.New("read failed")
	if err := a.forwardRawContainerLogStream(
		context.Background(),
		newLogStreamTestSender(),
		msg,
		iotest.ErrReader(readErr),
	); !errors.Is(err, readErr) {
		t.Fatalf("raw reader error = %v, want wrapped %v", err, readErr)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.sendContainerLogStreamChunk(
		canceledCtx,
		newLogStreamTestSender(),
		msg,
		"stdout",
		[]byte("ignored"),
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled send error = %v, want context canceled", err)
	}

	if err := a.sendContainerLogStreamChunk(
		context.Background(),
		nil,
		msg,
		"stdout",
		[]byte("ignored"),
	); err == nil {
		t.Fatal("nil sender should fail")
	}
}

// opaqueDoneContext is a context.Context whose Value never satisfies the
// context package's internal parentCancelCtx type assertion. A
// context.WithCancel child of a real *cancelCtx (like the edge read pump's
// connection-lifetime pumpCtx) registers itself in the parent's children
// map and unregisters silently when canceled, which makes a leaked
// registration invisible to a test. Deriving from opaqueDoneContext instead
// forces the context package onto its other propagation path: a persistent
// goroutine per child that blocks on "<-parent.Done()" until the child (or
// parent) is canceled. That turns the same underlying leak into a leaked
// goroutine this test can count.
type opaqueDoneContext struct {
	done chan struct{}
}

func (opaqueDoneContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (o opaqueDoneContext) Done() <-chan struct{}     { return o.done }
func (opaqueDoneContext) Err() error                  { return nil }
func (opaqueDoneContext) Value(any) any               { return nil }

func TestContainerLogStreamNormalCompletionCancelsStreamContext(t *testing.T) {
	client, calls, shutdown := newRouteTestDockerClient(t)
	defer shutdown()
	calls.setLogsResponse("container-1", routeTestDockerLogFrame(1, []byte("out\n")))

	a := NewAdapter(client, "test-agent", AgentInfo{})
	// Never canceled during the test: a leaked child's watcher goroutine
	// would otherwise exit anyway when this parent is canceled, hiding the
	// bug. A real pumpCtx stays live for the life of the WebSocket
	// connection, which is exactly what makes the leak this test targets
	// unbounded in production.
	parent := opaqueDoneContext{done: make(chan struct{})}

	baseline := runtime.NumGoroutine()

	const streams = 25
	for i := range streams {
		sender := newLogStreamTestSender()
		payload, err := json.Marshal(protocol.DDContainerLogRequestMessage{
			RequestID:   fmt.Sprintf("stream-normal-%d", i),
			ContainerID: "container-1",
			Stream:      true,
		})
		if err != nil {
			t.Fatalf("marshal request %d: %v", i, err)
		}
		if !a.HandleMessage(parent, sender, protocol.TypeDDContainerLogRequest, payload) {
			t.Fatalf("expected streaming request %d to be handled", i)
		}
		for {
			event := waitForLogStreamEvent(t, sender)
			if event.msgType == "dd:container_log_end" {
				break
			}
		}
	}

	// Each completed stream's deferred cleanup calls cancel() synchronously
	// before it returns, but the watcher goroutine it releases still needs
	// a scheduler tick to actually exit; poll briefly instead of asserting
	// on the very next NumGoroutine() call.
	var after int
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after-baseline < streams/2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if leaked := after - baseline; leaked >= streams/2 {
		t.Fatalf(
			"goroutine count grew by %d after %d normally-completed log streams (baseline=%d after=%d); "+
				"runContainerLogStream's deferred cleanup must cancel the stream's context so it unregisters "+
				"from its parent instead of leaking for the life of the connection",
			leaked, streams, baseline, after,
		)
	}
}

// ---- looksLikeDockerLogFrame ----

// TestLooksLikeDockerLogFrame exercises each of the four ANDed conditions in
// isolation: for every condition, the other three are held true so that
// only the guard under test can turn the overall result from true to
// false. This kills each individual invert-&&-to-|| mutation, since an
// inverted operator would let the surrounding true operands paper over the
// one false operand this case targets.
func TestLooksLikeDockerLogFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header []byte
		want   bool
	}{
		{name: "valid stdout frame header", header: []byte{1, 0, 0, 0, 0, 0, 0, 0}, want: true},
		{name: "valid stderr frame header", header: []byte{2, 0, 0, 0, 0, 0, 0, 0}, want: true},
		{name: "too short fails length check", header: []byte{1, 0, 0, 0, 0, 0, 0}, want: false},
		{name: "stream type out of range fails first byte check", header: []byte{3, 0, 0, 0, 0, 0, 0, 0}, want: false},
		{name: "byte 1 nonzero fails second byte check", header: []byte{1, 1, 0, 0, 0, 0, 0, 0}, want: false},
		{name: "byte 2 nonzero fails third byte check", header: []byte{1, 0, 1, 0, 0, 0, 0, 0}, want: false},
		{name: "byte 3 nonzero fails fourth byte check", header: []byte{1, 0, 0, 1, 0, 0, 0, 0}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := looksLikeDockerLogFrame(tt.header); got != tt.want {
				t.Fatalf("looksLikeDockerLogFrame(%v) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}
