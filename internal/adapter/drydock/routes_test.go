package drydock

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/docker"
)

// shortSocketPath returns a temp socket path short enough for the unix
// socket path limit (104 bytes on darwin); t.TempDir() embeds the full
// test name and overflows it.
func shortSocketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "lk")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return filepath.Join(dir, "d.sock")
}

func TestHandleContainersUsesCachedInventory(t *testing.T) {
	t.Parallel()

	client, calls, shutdown := newRouteTestDockerClient(t)
	defer shutdown()

	a := NewAdapter(client, "test-agent", AgentInfo{})
	if _, err := a.containers.BuildInventory(context.Background()); err != nil {
		t.Fatalf("build inventory: %v", err)
	}

	baseListCalls := calls.listCalls.Load()
	baseInspectCalls := calls.inspectCalls.Load()
	if baseListCalls == 0 || baseInspectCalls == 0 {
		t.Fatalf("expected initial docker calls to prime cache, got list=%d inspect=%d", baseListCalls, baseInspectCalls)
	}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/containers", nil)
		rec := httptest.NewRecorder()

		a.handleContainers(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusOK)
		}

		var containers []drydockContainer
		if err := json.NewDecoder(rec.Body).Decode(&containers); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(containers) != 1 {
			t.Fatalf("unexpected containers length: got %d want %d", len(containers), 1)
		}
	}

	if got := calls.listCalls.Load(); got != baseListCalls {
		t.Fatalf("expected no additional list calls after cache warmup; got %d want %d", got, baseListCalls)
	}
	if got := calls.inspectCalls.Load(); got != baseInspectCalls {
		t.Fatalf("expected no additional inspect calls after cache warmup; got %d want %d", got, baseInspectCalls)
	}
}

func TestHandleContainerLogsRejectsInvalidTail(t *testing.T) {
	t.Parallel()

	client, calls, shutdown := newRouteTestDockerClient(t)
	defer shutdown()

	a := NewAdapter(client, "test-agent", AgentInfo{})

	tests := []struct {
		name string
		tail string
	}{
		{name: "non numeric", tail: "abc"},
		{name: "zero", tail: "0"},
		{name: "negative", tail: "-5"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := calls.logsCalls.Load()

			req := httptest.NewRequest(http.MethodGet, "/api/containers/container-1/logs?tail="+tt.tail, nil)
			req.SetPathValue("id", "container-1")
			rec := httptest.NewRecorder()

			a.handleContainerLogs(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusBadRequest)
			}
			if got := calls.logsCalls.Load(); got != before {
				t.Fatalf("expected docker logs endpoint not to be called; got %d want %d", got, before)
			}
		})
	}
}

func TestHandleContainerLogsAcceptsPositiveTail(t *testing.T) {
	t.Parallel()

	client, calls, shutdown := newRouteTestDockerClient(t)
	defer shutdown()

	a := NewAdapter(client, "test-agent", AgentInfo{})

	req := httptest.NewRequest(http.MethodGet, "/api/containers/container-1/logs?tail=5", nil)
	req.SetPathValue("id", "container-1")
	rec := httptest.NewRecorder()

	a.handleContainerLogs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusOK)
	}
	if calls.logsCalls.Load() != 1 {
		t.Fatalf("expected one docker logs call, got %d", calls.logsCalls.Load())
	}
	if rec.Body.String() != "log line\n" {
		t.Fatalf("unexpected logs body: got %q want %q", rec.Body.String(), "log line\n")
	}
}

// TestHandleContainerLogsTimestampsAcceptsNumericForm covers the
// "== \"1\" || == \"true\"" pair for the timestamps query parameter:
// "timestamps=1" satisfies only the first disjunct, so an accidental
// invert-to-&& would evaluate the whole expression false and drop the
// timestamps flag sent to the docker daemon.
func TestHandleContainerLogsTimestampsAcceptsNumericForm(t *testing.T) {
	t.Parallel()

	client, calls, shutdown := newRouteTestDockerClient(t)
	defer shutdown()

	a := NewAdapter(client, "test-agent", AgentInfo{})

	req := httptest.NewRequest(http.MethodGet, "/api/containers/container-1/logs?timestamps=1", nil)
	req.SetPathValue("id", "container-1")
	rec := httptest.NewRecorder()

	a.handleContainerLogs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusOK)
	}
	rawQuery, _ := calls.lastLogsRawQuery.Load().(string)
	if !strings.Contains(rawQuery, "timestamps=1") {
		t.Fatalf("expected daemon query to include timestamps=1 for ?timestamps=1, got %q", rawQuery)
	}
}

type routeLogFlushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

type routeFailingLogWriter struct {
	header   http.Header
	attempts int
	flushes  int
}

func (w *routeFailingLogWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *routeFailingLogWriter) Write([]byte) (int, error) {
	w.attempts++
	return 0, io.ErrClosedPipe
}

func (w *routeFailingLogWriter) WriteHeader(int) {}

func (w *routeFailingLogWriter) Flush() {
	w.flushes++
}

func (r *routeLogFlushRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}

func TestHandleContainerLogsDecodesRawAndMultiplexedStreams(t *testing.T) {
	t.Parallel()

	stdout := routeTestDockerLogFrame(1, []byte("stdout line\n"))
	stderr := routeTestDockerLogFrame(2, []byte("stderr line\n"))
	tests := []struct {
		name     string
		body     []byte
		want     string
		minFlush int
	}{
		{name: "short raw TTY", body: []byte("tty\n"), want: "tty\n", minFlush: 1},
		{name: "multiplexed stdout and stderr", body: append(stdout, stderr...), want: "stdout line\nstderr line\n", minFlush: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, calls, shutdown := newRouteTestDockerClient(t)
			defer shutdown()
			calls.setLogsResponse("container-1", tt.body)

			a := NewAdapter(client, "test-agent", AgentInfo{})
			req := httptest.NewRequest(http.MethodGet, "/api/containers/container-1/logs?follow=true", nil)
			req.SetPathValue("id", "container-1")
			rec := &routeLogFlushRecorder{ResponseRecorder: httptest.NewRecorder()}

			a.handleContainerLogs(rec, req)

			if got := rec.Body.String(); got != tt.want {
				t.Fatalf("logs body = %q, want %q", got, tt.want)
			}
			if rec.flushes < tt.minFlush {
				t.Fatalf("flushes = %d, want at least %d", rec.flushes, tt.minFlush)
			}
		})
	}
}

func TestHandleContainerLogsStopsAfterResponseWriteFailure(t *testing.T) {
	t.Parallel()

	client, calls, shutdown := newRouteTestDockerClient(t)
	defer shutdown()
	calls.setLogsResponse("container-1", append(
		routeTestDockerLogFrame(1, []byte("first\n")),
		routeTestDockerLogFrame(2, []byte("second\n"))...,
	))

	a := NewAdapter(client, "test-agent", AgentInfo{})
	req := httptest.NewRequest(http.MethodGet, "/api/containers/container-1/logs?follow=true", nil)
	req.SetPathValue("id", "container-1")
	w := &routeFailingLogWriter{}

	a.handleContainerLogs(w, req)

	if w.attempts != 1 {
		t.Fatalf("response write attempts = %d, want decoder to stop after first failure", w.attempts)
	}
	if w.flushes != 0 {
		t.Fatalf("flushes = %d, want 0 after failed write", w.flushes)
	}
}

func TestHandleWatcherGetReturnsKnownWatcher(t *testing.T) {
	t.Parallel()

	client, _, shutdown := newRouteTestDockerClient(t)
	defer shutdown()

	a := NewAdapter(client, "test-agent", AgentInfo{})

	req := httptest.NewRequest(http.MethodGet, "/api/watchers/docker/docker", nil)
	req.SetPathValue("type", "docker")
	req.SetPathValue("name", "docker")
	rec := httptest.NewRecorder()

	a.handleWatcherGet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("unexpected content-type: got %q", ct)
	}

	var desc map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&desc); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if desc["type"] != "docker" {
		t.Fatalf("unexpected type: got %v want docker", desc["type"])
	}
	if desc["name"] != "docker" {
		t.Fatalf("unexpected name: got %v want docker", desc["name"])
	}
}

func TestHandleWatcherGetReturns404ForUnknown(t *testing.T) {
	t.Parallel()

	client, _, shutdown := newRouteTestDockerClient(t)
	defer shutdown()

	a := NewAdapter(client, "test-agent", AgentInfo{})

	req := httptest.NewRequest(http.MethodGet, "/api/watchers/unknown/missing", nil)
	req.SetPathValue("type", "unknown")
	req.SetPathValue("name", "missing")
	rec := httptest.NewRecorder()

	a.handleWatcherGet(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

// TestHandleWatcherGetReturns404ForPartialMatch covers the
// "EqualFold(Type) && EqualFold(Name)" match: type matches but name does
// not, so an accidental invert-to-|| would treat this as a match on type
// alone and return the docker watcher instead of 404.
func TestHandleWatcherGetReturns404ForPartialMatch(t *testing.T) {
	t.Parallel()

	client, _, shutdown := newRouteTestDockerClient(t)
	defer shutdown()

	a := NewAdapter(client, "test-agent", AgentInfo{})

	req := httptest.NewRequest(http.MethodGet, "/api/watchers/docker/not-docker", nil)
	req.SetPathValue("type", "docker")
	req.SetPathValue("name", "not-docker")
	rec := httptest.NewRecorder()

	a.handleWatcherGet(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleLogEntriesReturnsEmptyArray(t *testing.T) {
	t.Parallel()

	client, _, shutdown := newRouteTestDockerClient(t)
	defer shutdown()

	a := NewAdapter(client, "test-agent", AgentInfo{})

	req := httptest.NewRequest(http.MethodGet, "/api/log/entries", nil)
	rec := httptest.NewRecorder()

	a.handleLogEntries(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("unexpected content-type: got %q", ct)
	}

	var entries []any
	if err := json.NewDecoder(rec.Body).Decode(&entries); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty array, got %d entries", len(entries))
	}
}

type routeTestDockerCalls struct {
	listCalls        atomic.Int64
	inspectCalls     atomic.Int64
	logsCalls        atomic.Int64
	lastLogsRawQuery atomic.Value // string
	logResponses     sync.Map     // map[string]routeTestLogResponse
}

type routeTestLogResponse struct {
	body           []byte
	blockAfterBody bool
}

func (c *routeTestDockerCalls) setLogsResponse(containerID string, body []byte) {
	c.storeLogsResponse(containerID, routeTestLogResponse{
		body: append([]byte(nil), body...),
	})
}

func (c *routeTestDockerCalls) setBlockingLogsResponse(containerID string, body []byte) {
	c.storeLogsResponse(containerID, routeTestLogResponse{
		body:           append([]byte(nil), body...),
		blockAfterBody: true,
	})
}

func (c *routeTestDockerCalls) storeLogsResponse(containerID string, response routeTestLogResponse) {
	c.logResponses.Store(containerID, response)
}

func (c *routeTestDockerCalls) logsResponse(containerID string) routeTestLogResponse {
	if response, ok := c.logResponses.Load(containerID); ok {
		return response.(routeTestLogResponse)
	}
	return routeTestLogResponse{
		body: routeTestDockerLogFrame(1, []byte("log line\n")),
	}
}

func routeTestDockerLogFrame(streamType byte, payload []byte) []byte {
	frame := routeTestDockerLogHeader(streamType, uint32(len(payload)))
	frame = append(frame, payload...)
	return frame
}

func routeTestDockerLogHeader(streamType byte, size uint32) []byte {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:8], size)
	return header
}

func newRouteTestDockerClient(t *testing.T) (*docker.Client, *routeTestDockerCalls, func()) {
	t.Helper()

	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}

	calls := &routeTestDockerCalls{}

	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.VersionResponse{
			Version:    "26.0.0",
			APIVersion: "1.44",
		})
	})

	mux.HandleFunc("/v1.44/containers/json", func(w http.ResponseWriter, r *http.Request) {
		calls.listCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]docker.ContainerJSON{
			{
				ID:      "container-1",
				Image:   "nginx:latest",
				ImageID: "sha256:test",
				Labels: map[string]string{
					LabelDisplayName: "container-1",
				},
			},
		})
	})

	mux.HandleFunc("/v1.44/containers/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			calls.inspectCalls.Add(1)

			id := strings.TrimPrefix(r.URL.Path, "/v1.44/containers/")
			id = strings.TrimSuffix(id, "/json")
			if id == "" {
				http.NotFound(w, r)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(docker.ContainerInspect{
				ID:      id,
				Name:    "/" + id,
				Created: "2026-01-01T00:00:00Z",
				State: docker.ContainerState{
					Status:    "running",
					Running:   true,
					StartedAt: "2026-01-01T00:00:00Z",
				},
				Config: docker.ContainerConfig{
					Image: "nginx:latest",
					Labels: map[string]string{
						LabelDisplayName: "container-1",
					},
				},
				NetworkSettings: &docker.NetworkSettings{
					Networks: map[string]docker.NetworkEndpoint{},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			calls.logsCalls.Add(1)
			calls.lastLogsRawQuery.Store(r.URL.RawQuery)
			w.Header().Set("Content-Type", "application/octet-stream")

			id := strings.TrimPrefix(r.URL.Path, "/v1.44/containers/")
			id = strings.TrimSuffix(id, "/logs")
			response := calls.logsResponse(id)

			_, _ = w.Write(response.body)
			if response.blockAfterBody {
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				<-r.Context().Done()
			}
		default:
			http.NotFound(w, r)
		}
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
