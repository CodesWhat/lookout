package drydock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/adapter"
	"github.com/codeswhat/portwing/internal/docker"
	"github.com/codeswhat/portwing/internal/protocol"
)

type captureSender struct {
	msgType string
	data    any
}

func (s *captureSender) SendTypedMessage(msgType string, data any) error {
	s.msgType = msgType
	s.data = data
	return nil
}

type failingSender struct {
	err error
}

func (s *failingSender) SendTypedMessage(string, any) error {
	return s.err
}

type blockingSender struct {
	started chan struct{}
	release <-chan struct{}
	calls   atomic.Int32
}

func (s *blockingSender) SendTypedMessage(string, any) error {
	s.calls.Add(1)
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release
	return nil
}

func TestSendContainerSync_DoesNotPreMarshalContainers(t *testing.T) {
	a := &Adapter{}
	sender := &captureSender{}
	containers := []adapter.Container{
		{
			ID:          "abc123",
			Name:        "web",
			DisplayName: "web",
			Status:      "running",
			Watcher:     "docker",
			Image: adapter.ContainerImage{
				ID:       "sha256:deadbeef",
				Registry: "docker.io",
				Name:     "nginx",
				Tag:      "latest",
			},
		},
	}

	a.sendContainerSync(sender, containers)

	if sender.msgType != protocol.TypeDDContainerSync {
		t.Fatalf("expected message type %q, got %q", protocol.TypeDDContainerSync, sender.msgType)
	}
	if sender.data == nil {
		t.Fatal("expected data to be sent")
	}
	if containsRawMessage(reflect.TypeOf(sender.data)) {
		t.Fatalf("expected sync payload without pre-marshaled json.RawMessage containers, got %T", sender.data)
	}

	payload, err := json.Marshal(sender.data)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	var got struct {
		Containers []drydockContainer `json:"containers"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	want := toDrydockContainers(containers)
	if !reflect.DeepEqual(want, got.Containers) {
		t.Fatalf("unexpected containers payload: got %#v want %#v", got.Containers, want)
	}
}

func containsRawMessage(t reflect.Type) bool {
	if t == nil {
		return false
	}

	rawMessageType := reflect.TypeOf(json.RawMessage{})
	if t == rawMessageType {
		return true
	}

	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return containsRawMessage(t.Elem())
	case reflect.Map:
		return containsRawMessage(t.Key()) || containsRawMessage(t.Elem())
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if containsRawMessage(t.Field(i).Type) {
				return true
			}
		}
	}

	return false
}

func TestSendTypedMessage_LogsSenderError(t *testing.T) {
	logBuf := &bytes.Buffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	a := &Adapter{}
	a.sendTypedMessage(&failingSender{err: errors.New("send failed")}, protocol.TypeDDContainerSync, map[string]string{"ok": "no"})

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "failed to send typed message") {
		t.Fatalf("expected send failure log, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, protocol.TypeDDContainerSync) {
		t.Fatalf("expected log to include message type %q, got: %s", protocol.TypeDDContainerSync, logOutput)
	}
}

func TestSendTypedMessage_LogsNilSender(t *testing.T) {
	logBuf := &bytes.Buffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	a := &Adapter{}
	a.sendTypedMessage(nil, protocol.TypeDDWatchResponse, struct{}{})

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "sender is nil") {
		t.Fatalf("expected nil-sender log, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, protocol.TypeDDWatchResponse) {
		t.Fatalf("expected log to include message type %q, got: %s", protocol.TypeDDWatchResponse, logOutput)
	}
}

func TestMessageHandlerRejectsWhenConcurrencyLimitReached(t *testing.T) {
	a := &Adapter{}
	release := make(chan struct{})
	sender := &blockingSender{started: make(chan struct{}, defaultMessageHandlerConcurrency), release: release}
	var workers sync.WaitGroup
	defer func() { close(release); workers.Wait() }()
	for i := 0; i < defaultMessageHandlerConcurrency; i++ {
		workers.Add(1)
		if !a.spawnMessageHandler(context.Background(), "test", func() {
			defer workers.Done()
			_ = sender.SendTypedMessage("test", nil)
		}) {
			workers.Done()
			t.Fatal("handler rejected below limit")
		}
	}
	for i := 0; i < defaultMessageHandlerConcurrency; i++ {
		select {
		case <-sender.started:
		case <-time.After(time.Second):
			t.Fatal("admitted handler did not start")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan bool, 1)
	go func() {
		result <- a.spawnMessageHandler(ctx, "overload", func() { t.Error("overloaded handler started") })
	}()
	select {
	case admitted := <-result:
		if admitted {
			t.Fatal("handler admitted beyond limit")
		}
	case <-time.After(250 * time.Millisecond):
		cancel()
		<-result
		t.Fatal("full handler pool blocked admission")
	}
	if got := len(a.getMessageSemaphore()); got != defaultMessageHandlerConcurrency {
		t.Fatalf("occupied slots=%d", got)
	}
}

func TestHandleMessage_RespectsContextCancellationWhileWaitingForSemaphore(t *testing.T) {
	a := &Adapter{
		messageSem: make(chan struct{}, 1),
	}
	a.messageSem <- struct{}{}
	defer func() { <-a.messageSem }()

	sender := &captureSender{}
	payload := json.RawMessage(`{"watcherType":"docker","watcherName":"main"}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resultCh := make(chan bool, 1)
	go func() {
		resultCh <- a.HandleMessage(ctx, sender, protocol.TypeDDWatchRequest, payload)
	}()

	select {
	case handled := <-resultCh:
		if !handled {
			t.Fatalf("expected message type to be recognized")
		}
	case <-time.After(time.Second):
		t.Fatalf("expected HandleMessage to return after context cancellation")
	}

	if sender.msgType != "" {
		t.Fatalf("expected no message to be sent when context is canceled, got %q", sender.msgType)
	}
}

// ---------------------------------------------------------------------------
// handleContainerDeleteRequest
// ---------------------------------------------------------------------------

// deleteTestDockerCalls records the DELETE requests observed by the fake
// docker daemon used in the container-delete tests below.
type deleteTestDockerCalls struct {
	count     atomic.Int32
	lastPath  atomic.Value // string
	lastQuery atomic.Value // string
}

// newDeleteTestDockerClient stands up a fake Docker daemon (over a Unix
// socket, mirroring newRouteTestDockerClient in routes_test.go) whose
// DELETE /containers/{id} response status is controlled by respondStatus.
func newDeleteTestDockerClient(t *testing.T, respondStatus int) (*docker.Client, *deleteTestDockerCalls, func()) {
	t.Helper()

	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}

	calls := &deleteTestDockerCalls{}

	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.VersionResponse{
			Version:    "26.0.0",
			APIVersion: "1.44",
		})
	})
	mux.HandleFunc("/v1.44/containers/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		calls.count.Add(1)
		calls.lastPath.Store(strings.TrimPrefix(r.URL.Path, "/v1.44/containers/"))
		calls.lastQuery.Store(r.URL.RawQuery)

		if respondStatus == http.StatusNoContent {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "container not found", respondStatus)
	})

	server := &http.Server{Handler: mux}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = server.Serve(listener)
	}()

	client, err := docker.NewClient(socketPath, 2)
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
		<-serverDone
	}

	return client, calls, shutdown
}

// TestHandleContainerDeleteRequest_Success verifies that a successful Docker
// removal produces a DDContainerDeleteResponseMessage with Success=true and
// no Error, and that RemoveContainer was called with the requested ID.
func TestHandleContainerDeleteRequest_Success(t *testing.T) {
	t.Parallel()

	client, calls, shutdown := newDeleteTestDockerClient(t, http.StatusNoContent)
	defer shutdown()

	a := &Adapter{
		dockerClient: client,
		messageSem:   make(chan struct{}, defaultMessageHandlerConcurrency),
	}
	sender := &captureSender{}

	msg := protocol.DDContainerDeleteRequestMessage{RequestID: "req-delete-success", ContainerID: "container-1"}
	a.handleContainerDeleteRequest(context.Background(), sender, msg)

	if calls.count.Load() != 1 {
		t.Fatalf("expected RemoveContainer to be called once, got %d", calls.count.Load())
	}
	if got := calls.lastPath.Load(); got != "container-1" {
		t.Fatalf("expected RemoveContainer called with id %q, got %q", "container-1", got)
	}

	if sender.msgType != protocol.TypeDDContainerDeleteResponse {
		t.Fatalf("expected message type %q, got %q", protocol.TypeDDContainerDeleteResponse, sender.msgType)
	}
	resp, ok := sender.data.(protocol.DDContainerDeleteResponseMessage)
	if !ok {
		t.Fatalf("expected response payload type protocol.DDContainerDeleteResponseMessage, got %T", sender.data)
	}
	if resp.ContainerID != "container-1" {
		t.Fatalf("expected ContainerID %q, got %q", "container-1", resp.ContainerID)
	}
	if resp.RequestID != "req-delete-success" {
		t.Fatalf("expected RequestID %q, got %q", "req-delete-success", resp.RequestID)
	}
	if !resp.Success {
		t.Fatalf("expected Success=true, got false (error=%q)", resp.Error)
	}
	if resp.Error != "" {
		t.Fatalf("expected empty Error on success, got %q", resp.Error)
	}
}

// TestHandleContainerDeleteRequest_Error verifies that a Docker removal
// failure produces a DDContainerDeleteResponseMessage with Success=false and
// a populated Error.
func TestHandleContainerDeleteRequest_Error(t *testing.T) {
	t.Parallel()

	client, calls, shutdown := newDeleteTestDockerClient(t, http.StatusNotFound)
	defer shutdown()

	a := &Adapter{
		dockerClient: client,
		messageSem:   make(chan struct{}, defaultMessageHandlerConcurrency),
	}
	sender := &captureSender{}

	msg := protocol.DDContainerDeleteRequestMessage{RequestID: "req-delete-error", ContainerID: "missing-container"}
	a.handleContainerDeleteRequest(context.Background(), sender, msg)

	if calls.count.Load() != 1 {
		t.Fatalf("expected RemoveContainer to be called once, got %d", calls.count.Load())
	}

	if sender.msgType != protocol.TypeDDContainerDeleteResponse {
		t.Fatalf("expected message type %q, got %q", protocol.TypeDDContainerDeleteResponse, sender.msgType)
	}
	resp, ok := sender.data.(protocol.DDContainerDeleteResponseMessage)
	if !ok {
		t.Fatalf("expected response payload type protocol.DDContainerDeleteResponseMessage, got %T", sender.data)
	}
	if resp.ContainerID != "missing-container" {
		t.Fatalf("expected ContainerID %q, got %q", "missing-container", resp.ContainerID)
	}
	if resp.RequestID != "req-delete-error" {
		t.Fatalf("expected RequestID %q, got %q", "req-delete-error", resp.RequestID)
	}
	if resp.Success {
		t.Fatal("expected Success=false on docker error")
	}
	if resp.Error == "" {
		t.Fatal("expected a populated Error on failure")
	}
}

func TestHandleContainerDeleteRequest_EmptyRequestID(t *testing.T) {
	t.Parallel()

	client, _, shutdown := newDeleteTestDockerClient(t, http.StatusNoContent)
	defer shutdown()

	a := &Adapter{
		dockerClient: client,
		messageSem:   make(chan struct{}, defaultMessageHandlerConcurrency),
	}
	sender := &captureSender{}

	msg := protocol.DDContainerDeleteRequestMessage{ContainerID: "container-1"}
	a.handleContainerDeleteRequest(context.Background(), sender, msg)

	if sender.msgType != protocol.TypeDDContainerDeleteResponse {
		t.Fatalf("expected message type %q, got %q", protocol.TypeDDContainerDeleteResponse, sender.msgType)
	}
	resp, ok := sender.data.(protocol.DDContainerDeleteResponseMessage)
	if !ok {
		t.Fatalf("expected response payload type protocol.DDContainerDeleteResponseMessage, got %T", sender.data)
	}
	if resp.RequestID != "" {
		t.Fatalf("expected empty RequestID, got %q", resp.RequestID)
	}
	if !resp.Success {
		t.Fatalf("expected Success=true, got false (error=%q)", resp.Error)
	}
}
