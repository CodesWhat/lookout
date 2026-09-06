package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortTempDir returns a short temp directory path suitable for unix sockets
// (macOS limits unix socket paths to 104 bytes).
func shortTempDir(t *testing.T) string {
	t.Helper()
	// Use os.MkdirTemp with /tmp as base to keep the path short.
	dir, err := os.MkdirTemp("", "lk")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startUnixHTTPServer creates a unix socket in a short temp dir and serves
// handler on it. Returns the socket path; the server is shut down via t.Cleanup.
func startUnixHTTPServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir := shortTempDir(t)
	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return path
}

// ---- NewClient via real unix socket ----

func TestNewClient_Success(t *testing.T) {
	t.Parallel()

	socketPath := startUnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"Version":"24.0.5","ApiVersion":"1.45"}`)
	}))

	c, err := NewClient(socketPath, 5)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.apiVersion != "v1.45" {
		t.Fatalf("apiVersion = %q, want %q", c.apiVersion, "v1.45")
	}
	if c.socketPath != socketPath {
		t.Fatalf("socketPath = %q, want %q", c.socketPath, socketPath)
	}
}

// TestNewClient_DialTimeoutMatchesRequestTimeoutSeconds verifies NewClient
// converts its requestTimeout (seconds) into dialTimeout via multiplication
// by time.Second, not some other operator. Dividing instead (as a mutant
// would) collapses a small integer number of seconds to zero nanoseconds.
func TestNewClient_DialTimeoutMatchesRequestTimeoutSeconds(t *testing.T) {
	t.Parallel()

	socketPath := startUnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"Version":"24.0.5","ApiVersion":"1.45"}`) //nolint:errcheck
	}))

	c, err := NewClient(socketPath, 7)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.dialTimeout != 7*time.Second {
		t.Fatalf("dialTimeout = %v, want %v", c.dialTimeout, 7*time.Second)
	}
}

// TestDefaultDialTimeoutEqualsThirtySeconds pins the fallback dial timeout's
// value so an arithmetic change to its definition (e.g. 30 / time.Second
// instead of 30 * time.Second) is caught even though the const declaration
// itself carries no runtime coverage.
func TestDefaultDialTimeoutEqualsThirtySeconds(t *testing.T) {
	t.Parallel()

	if defaultDialTimeout != 30*time.Second {
		t.Fatalf("defaultDialTimeout = %v, want %v", defaultDialTimeout, 30*time.Second)
	}
}

// TestMaxDockerErrorBodyBytesEquals4KiB pins the docker-error-body read cap,
// same reasoning as TestDefaultDialTimeoutEqualsThirtySeconds above.
func TestMaxDockerErrorBodyBytesEquals4KiB(t *testing.T) {
	t.Parallel()

	if maxDockerErrorBodyBytes != 4*1024 {
		t.Fatalf("maxDockerErrorBodyBytes = %d, want %d", maxDockerErrorBodyBytes, 4*1024)
	}
}

// TestNewClient_StreamTransportDialContext exercises the streamTransport.DialContext
// closure by issuing a streaming request via the real unix socket client.
func TestNewClient_StreamTransportDialContext(t *testing.T) {
	t.Parallel()

	socketPath := startUnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve both the version negotiation and a streaming events endpoint.
		if r.URL.Path == "/version" {
			fmt.Fprintf(w, `{"Version":"24.0.5","ApiVersion":"1.45"}`)
			return
		}
		// Events endpoint — just return 200 with empty body.
		w.WriteHeader(http.StatusOK)
	}))

	c, err := NewClient(socketPath, 5)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// DoStream uses streamClient which exercises the streamTransport.DialContext closure.
	resp, err := c.DoStream(t.Context(), http.MethodGet, "/events?type=container", nil)
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	resp.Body.Close()
}

func TestNewClient_NegotiationFallback(t *testing.T) {
	t.Parallel()

	// Serve bad JSON so negotiation fails → fallback to v1.44
	socketPath := startUnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "not-json")
	}))

	c, err := NewClient(socketPath, 5)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.apiVersion != "v1.44" {
		t.Fatalf("apiVersion = %q, want fallback %q", c.apiVersion, "v1.44")
	}
}

// ---- StartExec via real unix socket ----

func TestStartExec_DialFailure(t *testing.T) {
	t.Parallel()

	c := &Client{
		socketPath: "/nonexistent/path/docker.sock",
		apiVersion: "v1.44",
	}
	_, err := c.StartExec(t.Context(), "exec-123", false)
	if err == nil {
		t.Fatal("expected error dialing nonexistent socket, got nil")
	}
	if !strings.Contains(err.Error(), "dial docker socket") {
		t.Fatalf("error = %q, expected 'dial docker socket'", err.Error())
	}
}

func TestStartExec_ReadResponseError(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Drain the request
		buf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
		conn.Read(buf)                                               //nolint:errcheck
		// Send garbage (not valid HTTP)
		conn.Write([]byte("GARBAGE\r\n\r\n")) //nolint:errcheck
	}()

	c := &Client{socketPath: sockPath, apiVersion: "v1.44"}
	_, err = c.StartExec(t.Context(), "exec-123", false)
	wg.Wait()
	if err == nil {
		t.Fatal("expected error reading garbage response, got nil")
	}
}

func TestStartExec_Non101Response(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))       //nolint:errcheck
		conn.Read(buf)                                                     //nolint:errcheck
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")) //nolint:errcheck
	}()

	c := &Client{socketPath: sockPath, apiVersion: "v1.44"}
	_, err = c.StartExec(t.Context(), "exec-123", false)
	wg.Wait()
	if err == nil {
		t.Fatal("expected error for non-101 response, got nil")
	}
	if !strings.Contains(err.Error(), "101") {
		t.Fatalf("error = %q, expected to contain '101'", err.Error())
	}
}

func TestStartExec_Non101ResponseWithMessage(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	body := `{"message":"exec denied: no commands are allowlisted"}`
	resp := fmt.Sprintf("HTTP/1.1 403 Forbidden\r\nContent-Length: %d\r\n\r\n%s", len(body), body)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
		conn.Read(buf)                                               //nolint:errcheck
		conn.Write([]byte(resp))                                     //nolint:errcheck
	}()

	c := &Client{socketPath: sockPath, apiVersion: "v1.44"}
	_, err = c.StartExec(t.Context(), "exec-123", false)
	wg.Wait()
	if err == nil {
		t.Fatal("expected error for non-101 response, got nil")
	}
	if !strings.Contains(err.Error(), "101") {
		t.Fatalf("error = %q, expected to contain '101'", err.Error())
	}
	if !strings.Contains(err.Error(), "exec denied: no commands are allowlisted") {
		t.Fatalf("error = %q, expected to contain the docker error message", err.Error())
	}
}

func TestStartExec_Success_NoBuffered(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Read the raw HTTP request
		buf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
		conn.Read(buf)                                               //nolint:errcheck
		// Send 101 Switching Protocols with NO extra body bytes
		conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: tcp\r\nConnection: Upgrade\r\n\r\n")) //nolint:errcheck
		// Keep the connection open briefly
		time.Sleep(100 * time.Millisecond)
		conn.Close()
	}()

	c := &Client{socketPath: sockPath, apiVersion: "v1.44"}
	conn, err := c.StartExec(t.Context(), "exec-123", false)
	if err != nil {
		t.Fatalf("StartExec: %v", err)
	}
	conn.Close()
	wg.Wait()

	// Should be a plain net.Conn, not bufferedConn (no extra bytes after headers)
	if _, ok := conn.(*bufferedConn); ok {
		t.Fatal("expected plain net.Conn (no buffered bytes), got bufferedConn")
	}
}

func TestStartExec_Success_WithBuffered(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
		conn.Read(buf)                                               //nolint:errcheck
		// Send 101 + immediate extra bytes (simulates stream data arriving immediately after upgrade)
		conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: tcp\r\nConnection: Upgrade\r\n\r\nextra-stream-data")) //nolint:errcheck
		time.Sleep(100 * time.Millisecond)
		conn.Close()
	}()

	c := &Client{socketPath: sockPath, apiVersion: "v1.44"}
	conn, err := c.StartExec(t.Context(), "exec-123", true)
	if err != nil {
		t.Fatalf("StartExec: %v", err)
	}
	defer conn.Close()
	wg.Wait()

	// Should be a bufferedConn because extra bytes were buffered after headers
	if _, ok := conn.(*bufferedConn); !ok {
		t.Fatal("expected bufferedConn (extra buffered bytes), got plain net.Conn")
	}
}

// TestStartExec_WedgedDaemon reproduces a dockerd that accepts the exec-start
// connection but never writes a response (a wedged daemon or a deadlocked
// exec subsystem). Before the fix, StartExec dialed with plain net.Dial and
// blocked on http.ReadResponse with no deadline, so this would hang forever
// and leak the ExecSession registration. StartExec must now honor ctx's
// deadline and return promptly. The outer timeout is generous so a
// regression shows up as a failure, not a hang.
func TestStartExec_WedgedDaemon(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		// Drain whatever the client writes but never answer and never close:
		// the connection must stay open and silent until the client's own
		// deadline fires, or this test proves nothing. A single Read here
		// returns as soon as the request lands, and returning from this
		// goroutine closes the conn, handing the client a fast EOF that the
		// pre-fix code also survives. Loop until the client closes its side.
		buf := make([]byte, 4096)
		for {
			conn.SetReadDeadline(time.Now().Add(30 * time.Second)) //nolint:errcheck
			if _, err := conn.Read(buf); err != nil {
				return // the client gave up and closed its side
			}
		}
	}()

	c := &Client{socketPath: sockPath, apiVersion: "v1.44", dialTimeout: 200 * time.Millisecond}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := c.StartExec(ctx, "exec-123", false)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a wedged handshake, got nil")
		}
		// The error must be the deadline firing, not a fast EOF from a server
		// that hung up — an EOF here means the mock stopped wedging and the
		// test stopped testing the fix.
		var nerr net.Error
		if !errors.As(err, &nerr) || !nerr.Timeout() {
			t.Fatalf("expected a timeout from a wedged handshake, got %v", err)
		}
		if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
			t.Fatalf("StartExec returned in %v, before the 200ms deadline could have fired", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StartExec did not return within the outer timeout; handshake is not bounded")
	}

	<-accepted
	ln.Close()
	wg.Wait()
}

func TestStartExec_CancelInterruptsUpgrade(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	requestRead := make(chan struct{})
	peerClosed := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		accepted <- conn

		buf := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, readErr := conn.Read(buf); readErr != nil {
			return
		}
		close(requestRead)

		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = conn.Read(buf)
		close(peerClosed)
	}()

	c := &Client{socketPath: sockPath, apiVersion: "v1.44", dialTimeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, startErr := c.StartExec(ctx, "exec-canceled", false)
		result <- startErr
	}()

	var serverConn net.Conn
	select {
	case serverConn = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("Docker exec start connection was not accepted")
	}
	select {
	case <-requestRead:
	case <-time.After(2 * time.Second):
		_ = serverConn.Close()
		<-result
		<-serverDone
		t.Fatal("Docker exec start request was not received")
	}

	started := time.Now()
	cancel()
	select {
	case startErr := <-result:
		if !errors.Is(startErr, context.Canceled) {
			t.Fatalf("StartExec error = %v, want context canceled", startErr)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("StartExec took %v to return after cancellation", elapsed)
		}
	case <-time.After(time.Second):
		_ = serverConn.Close()
		startErr := <-result
		<-serverDone
		t.Fatalf("StartExec did not return promptly after cancellation; eventual error: %v", startErr)
	}

	select {
	case <-peerClosed:
	case <-time.After(time.Second):
		_ = serverConn.Close()
		<-serverDone
		t.Fatal("StartExec did not close the raw Unix socket after cancellation")
	}
	<-serverDone
}

// ---- closeConn ----

// errCloseConn is a net.Conn whose Close always returns an error, so we can
// exercise the slog.Debug branch in closeConn.
type errCloseConn struct {
	net.Conn
	closeErr error
}

func (e *errCloseConn) Close() error { return e.closeErr }

// TestCloseConn_Error verifies closeConn logs (via slog.Debug) when Close
// returns an error, and not just that it avoids panicking. Not parallel: it
// swaps the global slog default logger for the duration of the test: see the
// package note above TestEventStream_run_ErrorLogging-style tests for why
// that's safe here (serial tests fully run before any parallel test body
// starts executing).
func TestCloseConn_Error(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	// closeConn with a conn that errors on Close should log but not panic.
	c := &errCloseConn{Conn: nil, closeErr: fmt.Errorf("close failed")}
	closeConn(c, "test context") // must not panic

	if !strings.Contains(buf.String(), "closing docker connection") {
		t.Fatalf("expected a debug log for the failed Close, got: %s", buf.String())
	}
}

// TestCloseConn_NoError verifies closeConn does NOT log when Close succeeds,
// the mirror case of TestCloseConn_Error. Not parallel, same reasoning.
func TestCloseConn_NoError(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	// closeConn on a conn that closes cleanly should not log or panic.
	c1, c2 := net.Pipe()
	c2.Close()

	// closeConn calls c1.Close() which should succeed.
	closeConn(c1, "test context")

	if strings.Contains(buf.String(), "closing docker connection") {
		t.Fatalf("did not expect a debug log for a clean Close, got: %s", buf.String())
	}
}
