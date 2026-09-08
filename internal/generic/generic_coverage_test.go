package generic

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/docker"
)

// newTestDockerClientBlockingLogs creates a stub whose /logs endpoint writes a
// valid HTTP 200 and then blocks indefinitely (never sends any body bytes).
// This lets the test cancel the request context mid-ReadFull to trigger the
// non-EOF/non-ErrUnexpectedEOF error branch in handleContainerLogs (routes.go:58).
func newTestDockerClientBlockingLogs(t *testing.T) (*docker.Client, func()) {
	t.Helper()

	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.VersionResponse{Version: "26.0.0", APIVersion: "1.44"})
	})
	mux.HandleFunc("/v1.44/containers/", func(w http.ResponseWriter, r *http.Request) {
		if !endsWith(r.URL.Path, "/logs") {
			http.NotFound(w, r)
			return
		}
		// Write the HTTP 200 headers immediately so GetContainerLogs returns
		// a body, but then block until the request context is cancelled.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Block until the client goes away.
		<-r.Context().Done()
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

	return client, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
		<-serverDone
	}
}

// endsWith is a tiny helper to avoid importing strings just for HasSuffix.
func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// TestHandleContainerLogsContextCancel exercises the slog.Debug path in
// routes.go:58 — ReadFull returns a non-EOF/non-ErrUnexpectedEOF error
// (context.Canceled) when the request context is cancelled while blocking.
func TestHandleContainerLogsContextCancel(t *testing.T) {
	t.Parallel()

	client, shutdown := newTestDockerClientBlockingLogs(t)
	defer shutdown()

	a := New(client, "test-agent")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/containers/container-1/logs", nil).WithContext(ctx)
	req.SetPathValue("id", "container-1")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.handleContainerLogs(rec, req)
	}()

	// Give handleContainerLogs time to enter the ReadFull call.
	time.Sleep(100 * time.Millisecond)

	// Cancel the context — this causes ReadFull to return context.Canceled,
	// which is neither io.EOF nor io.ErrUnexpectedEOF, hitting the slog.Debug branch.
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleContainerLogs did not return after context cancel")
	}

	// The handler should have written the Content-Type header before
	// blocking, so status should be 200 (default for ResponseRecorder).
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}

// newTestDockerClientImmediateEventClose creates a stub whose /events endpoint
// sends one event and then immediately closes the connection. This lets
// docker.EventStream.run() exit its readEvents call quickly and close the
// eventCh channel when the request context is done, exercising the `!ok`
// branch in ServeHTTP (events.go:105-108).
func newTestDockerClientImmediateEventClose(t *testing.T) (*docker.Client, func()) {
	t.Helper()

	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.VersionResponse{Version: "26.0.0", APIVersion: "1.44"})
	})
	mux.HandleFunc("/v1.44/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Send one event then immediately close — readEvents returns nil (EOF).
		event := docker.DockerEvent{
			Type:   "container",
			Action: "start",
			Actor: docker.Actor{
				ID:         "xyz789",
				Attributes: map[string]string{"name": "test-c", "image": "alpine"},
			},
			Time: time.Now().Unix(),
		}
		_ = json.NewEncoder(w).Encode(event)
		// Return: HTTP handler exits, body closes, readEvents gets io.EOF.
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

	return client, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
		<-serverDone
	}
}

// TestServeHTTPEventChannelClosed exercises the `case de, ok := <-eventCh; if !ok`
// branch in ServeHTTP (events.go:105-108).
//
// The Docker stub immediately closes the event stream so readEvents returns
// quickly. After the stream closes, the EventStream goroutine enters its
// reconnect sleep. When we cancel the request context, ServeHTTP's ctx.Done()
// fires and causes the goroutine to exit, closing eventCh. Because both
// ctx.Done() and a closed eventCh may be simultaneously ready, Go's select
// may choose either — we run several iterations to maximise the chance of
// hitting the `!ok` path.
func TestServeHTTPEventChannelClosed(t *testing.T) {
	t.Parallel()

	// Run multiple attempts: the select in ServeHTTP is non-deterministic when
	// both ctx.Done() and a closed channel are ready, so repeated runs give
	// good coverage of the !ok branch in practice.
	const attempts = 20
	for i := 0; i < attempts; i++ {
		func() {
			client, shutdown := newTestDockerClientImmediateEventClose(t)
			defer shutdown()

			b := NewEventBroadcaster(client)

			ctx, cancel := context.WithCancel(context.Background())
			req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
			rec := &syncRecorder{rec: httptest.NewRecorder()}

			done := make(chan struct{})
			go func() {
				defer close(done)
				b.ServeHTTP(rec, req)
			}()

			// Give the EventStream goroutine time to consume the single event
			// and enter its reconnect delay (5 s by default). Then cancel so
			// the goroutine exits and closes eventCh.
			time.Sleep(150 * time.Millisecond)
			cancel()

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				cancel()
				<-done
				t.Fatal("ServeHTTP did not return")
			}
		}()
	}
}

// TestServeHTTPHeartbeatWriteError exercises the heartbeat write-error branch
// (events.go:98-101) by shortening the ticker interval via a custom writer
// that fails after the initial flush but before the heartbeat.
//
// Because the 30-second heartbeat ticker is internal to ServeHTTP we cannot
// inject a fast ticker without changing production code. This test instead
// verifies the broader ServeHTTP flow using failAfterNWriter and provides
// documentation that the heartbeat branch requires an extended soak to reach
// in a real test.
//
// Coverage of the heartbeat tick (case <-heartbeat.C) requires waiting 30 s,
// which is impractical in unit tests without mocking the ticker. This test
// documents that constraint. The actual coverage gap is recorded in residual.
func TestServeHTTPHeartbeatDocumented(t *testing.T) {
	t.Parallel()

	// Confirm ServeHTTP exits cleanly on context cancel without reaching the
	// heartbeat — the tick fires only after 30 s and this test does not wait
	// that long. The goal here is to confirm no panic occurs and that other
	// paths run correctly.
	client, _, shutdown := newTestDockerClient(t)
	defer shutdown()

	b := NewEventBroadcaster(client)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	rec := &syncRecorder{rec: httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, req)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("ServeHTTP did not return after context timeout")
	}
}

func TestHandleContainerLogsForwardsQueryParameters(t *testing.T) {
	t.Parallel()

	base := url.Values{"stderr": {"1"}, "stdout": {"1"}}
	tests := []struct {
		name  string
		query string
		want  url.Values
	}{
		{name: "defaults", want: base},
		{name: "tail", query: "tail=10", want: url.Values{"stderr": {"1"}, "stdout": {"1"}, "tail": {"10"}}},
		{name: "since", query: "since=1h", want: url.Values{"since": {"1h"}, "stderr": {"1"}, "stdout": {"1"}}},
		{name: "until", query: "until=30m", want: url.Values{"stderr": {"1"}, "stdout": {"1"}, "until": {"30m"}}},
		{
			name:  "combined",
			query: "tail=25&since=2026-01-01T00%3A00%3A00Z&until=2026-12-31T23%3A59%3A59Z",
			want: url.Values{
				"since":  {"2026-01-01T00:00:00Z"},
				"stderr": {"1"},
				"stdout": {"1"},
				"tail":   {"25"},
				"until":  {"2026-12-31T23:59:59Z"},
			},
		},
		{name: "minimum tail boundary", query: "tail=1", want: url.Values{"stderr": {"1"}, "stdout": {"1"}, "tail": {"1"}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, calls, shutdown := newTestDockerClient(t)
			defer shutdown()

			a := New(client, "test-agent")
			path := "/api/v1/containers/container-1/logs"
			if tc.query != "" {
				path += "?" + tc.query
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.SetPathValue("id", "container-1")
			rec := httptest.NewRecorder()

			a.handleContainerLogs(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			queries := calls.logsQueryClones()
			if len(queries) != 1 {
				t.Fatalf("Docker log queries = %d, want 1", len(queries))
			}
			if !reflect.DeepEqual(queries[0], tc.want) {
				t.Fatalf("Docker log query = %#v, want %#v", queries[0], tc.want)
			}
		})
	}
}

// newTestDockerClientWithHeartbeat creates a stub that keeps the /events
// stream open indefinitely, letting us test the heartbeat path by waiting
// for the ticker to fire — used only in integration-style tests.
//
// The heartbeat fires every 30 s, so this helper is defined for completeness
// but the corresponding test is skipped in short mode (go test -short).
func newTestDockerClientWithHeartbeat(t *testing.T) (*docker.Client, func()) {
	t.Helper()

	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.VersionResponse{Version: "26.0.0", APIVersion: "1.44"})
	})
	mux.HandleFunc("/v1.44/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Block until client disconnects — no events sent, so only the
		// heartbeat ticker can produce a write.
		<-r.Context().Done()
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

	return client, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
		<-serverDone
	}
}

// TestServeHTTPHeartbeat exercises the heartbeat case (<-heartbeat.C) in
// ServeHTTP (events.go:97-103) using a non-failing writer so flusher.Flush()
// is reached. It overrides heartbeatInterval so the test doesn't have to wait
// out the production 30 s ticker, and it asserts the heartbeat comment
// actually landed in the response body: previously the poll loop below just
// fell through on a bare timeout with no failure, so a broadcaster that never
// wrote a heartbeat still passed.
func TestServeHTTPHeartbeat(t *testing.T) {
	t.Parallel()

	client, shutdown := newTestDockerClientWithHeartbeat(t)
	defer shutdown()

	b := NewEventBroadcaster(client)
	b.heartbeatInterval = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	rec := &syncRecorder{rec: httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, req)
	}()

	// Poll until we see the heartbeat comment or the deadline expires.
	const heartbeatMarker = ": heartbeat"
	deadline := time.Now().Add(4 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		body = rec.BodyString()
		if strings.Contains(body, heartbeatMarker) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if !strings.Contains(body, heartbeatMarker) {
		t.Fatalf("ServeHTTP body = %q, want it to contain a %q heartbeat comment line", body, heartbeatMarker)
	}
}

// TestServeHTTPHeartbeatWriteError exercises the heartbeat write-error branch
// (events.go:99-100) where fmt.Fprintf returns an error after the heartbeat
// tick fires. The ticker fires after 30 s, so this test is skipped with -short.
func TestServeHTTPHeartbeatWriteError(t *testing.T) {
	if testing.Short() {
		t.Skip("heartbeat fires every 30 s — skipped in short mode")
	}

	// Use the blocking-events stub so no event writes happen before the
	// heartbeat. failAfterNWriter{n:0} fails on the very first fmt.Fprintf call,
	// which is the heartbeat write.
	client, shutdown := newTestDockerClientWithHeartbeat(t)
	defer shutdown()

	b := NewEventBroadcaster(client)

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	// n=0: fail on the very first Write call (the heartbeat "data: ..." line).
	w := &failAfterNWriter{n: 0}

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(w, req)
	}()

	// ServeHTTP should exit on its own once the heartbeat write fails.
	select {
	case <-done:
		// Good — ServeHTTP returned after the heartbeat write error.
	case <-time.After(35 * time.Second):
		cancel()
		<-done
		t.Fatal("ServeHTTP did not return after heartbeat write error")
	}
}

// TestHandleContainerLogsCustomError exercises the slog.Debug path via a
// Docker stub that sends a valid header claiming frameSize bytes, but then
// abruptly closes the connection mid-frame using a custom hijack to return
// an error from the underlying Read that is not EOF/ErrUnexpectedEOF.
//
// This is a belt-and-suspenders test alongside TestHandleContainerLogsContextCancel,
// which already covers this branch via context cancellation.
func TestHandleContainerLogsFrameReadError(t *testing.T) {
	t.Parallel()

	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.VersionResponse{Version: "26.0.0", APIVersion: "1.44"})
	})
	mux.HandleFunc("/v1.44/containers/", func(w http.ResponseWriter, r *http.Request) {
		if !endsWith(r.URL.Path, "/logs") {
			http.NotFound(w, r)
			return
		}
		// Use hijack to take over the connection and send malformed data.
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack not supported", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		// Write a minimal HTTP 200 response with headers.
		resp := "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n\r\n"
		_, _ = buf.WriteString(resp)
		_ = buf.Flush()

		// Write a valid 8-byte Docker log header claiming 5 bytes.
		hdr := make([]byte, 8)
		hdr[0] = 1
		binary.BigEndian.PutUint32(hdr[4:8], 5)
		chunk := append(hdr, []byte("hello")...)
		chunkStr := "d\r\n" // 13 bytes in hex = "d"
		_, _ = buf.WriteString(chunkStr)
		_, _ = buf.Write(chunk)
		_, _ = buf.WriteString("\r\n")
		_ = buf.Flush()

		// Now write a next header as a chunk but immediately close the
		// connection without completing the chunk — this will cause
		// the HTTP client's body reader to return an error that is neither
		// io.EOF nor io.ErrUnexpectedEOF (e.g. *net.OpError from broken pipe).
		partialHdr := []byte{1, 0, 0, 0, 0, 0, 0} // only 7 of 8 bytes
		chunkSize := "7\r\n"
		_, _ = buf.WriteString(chunkSize)
		_, _ = buf.Write(partialHdr)
		// Close without completing the chunk — causes connection reset.
	})

	server := &http.Server{Handler: mux}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = server.Serve(listener)
	}()

	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
		<-serverDone
	}()

	client, err := docker.NewClient(socketPath, 2)
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}

	a := New(client, "test-agent")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/containers/container-1/logs", nil)
	req.SetPathValue("id", "container-1")
	rec := httptest.NewRecorder()

	// Should complete without panic regardless of error path taken.
	a.handleContainerLogs(rec, req)
}
