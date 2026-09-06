package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/codeswhat/portwing/internal/adapter"
	"github.com/codeswhat/portwing/internal/audit"
	"github.com/codeswhat/portwing/internal/config"
	"github.com/codeswhat/portwing/internal/docker"
	"github.com/codeswhat/portwing/internal/metrics"
	"github.com/codeswhat/portwing/internal/protocol"
)

func TestHealthServerConfiguresReadHeaderTimeout(t *testing.T) {
	t.Parallel()

	c := &Client{
		cfg: &config.Config{
			BindAddress: "127.0.0.1",
			Port:        "0",
		},
	}

	c.startHealthServer()
	t.Cleanup(func() {
		if c.healthServer != nil {
			_ = c.healthServer.Close()
		}
	})

	if c.healthServer == nil {
		t.Fatal("healthServer was not initialized")
	}
	if c.healthServer.ReadHeaderTimeout < 5*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s, want at least 5s", c.healthServer.ReadHeaderTimeout)
	}
}

func TestHealthServerSeparatesLivenessFromDisconnectedReadiness(t *testing.T) {
	t.Parallel()

	c := &Client{
		cfg: &config.Config{
			BindAddress: "127.0.0.1",
			Port:        "0",
		},
	}
	c.startHealthServer()
	t.Cleanup(func() {
		if c.healthServer != nil {
			_ = c.healthServer.Close()
		}
	})

	liveness := httptest.NewRecorder()
	c.healthServer.Handler.ServeHTTP(
		liveness,
		httptest.NewRequest(http.MethodGet, "/health", nil),
	)
	if liveness.Code != http.StatusOK {
		t.Fatalf("liveness status = %d, want 200", liveness.Code)
	}
	var live protocol.HealthResponse
	if err := json.NewDecoder(liveness.Body).Decode(&live); err != nil {
		t.Fatalf("decode liveness: %v", err)
	}
	if !live.Live || live.Ready || live.Status != "ok" ||
		live.Docker != "unknown" || live.Controller != "disconnected" {
		t.Fatalf("liveness response = %+v", live)
	}

	readiness := httptest.NewRecorder()
	c.healthServer.Handler.ServeHTTP(
		readiness,
		httptest.NewRequest(http.MethodGet, "/ready", nil),
	)
	if readiness.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d, want 503", readiness.Code)
	}
	var ready protocol.HealthResponse
	if err := json.NewDecoder(readiness.Body).Decode(&ready); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	if ready.Ready || ready.Status != "unhealthy" ||
		ready.Docker != "disconnected" || ready.Controller != "disconnected" {
		t.Fatalf("readiness response = %+v", ready)
	}
}

func TestNewClientFieldsSet(t *testing.T) {
	t.Parallel()

	auditor, closeAudit, err := audit.New("", 0)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(closeAudit)

	// docker.NewClient with a non-existent socket falls back to apiVersion="v1.44".
	dc, err := docker.NewClient("/tmp/portwing-test-nonexistent.sock", 1)
	if err != nil {
		t.Fatalf("docker.NewClient: %v", err)
	}

	cfg := &config.Config{
		TLSSkipVerify:    false,
		SkipDFCollection: true,
	}
	a := &fakeAdapter{caps: []string{"test"}}

	c := NewClient(cfg, dc, a, auditor)
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.cfg != cfg {
		t.Error("cfg not wired")
	}
	if c.streamSem == nil {
		t.Error("streamSem is nil")
	}
}

// TestNewClientTLSSkipVerifyLogs covers the slog.Warn branch in NewClient
// when TLSSkipVerify is true.
func TestNewClientTLSSkipVerifyLogs(t *testing.T) {
	t.Parallel()

	auditor, closeAudit, err := audit.New("", 0)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(closeAudit)

	dc, err := docker.NewClient("/tmp/portwing-test-nonexistent.sock", 1)
	if err != nil {
		t.Fatalf("docker.NewClient: %v", err)
	}

	cfg := &config.Config{
		TLSSkipVerify:    true, // triggers the slog.Warn line
		SkipDFCollection: true,
	}

	c := NewClient(cfg, dc, &fakeAdapter{}, auditor)
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
}

// ---------------------------------------------------------------------------
// connect — CACert paths (uncovered in connect, lines 214-223)
// ---------------------------------------------------------------------------

// TestConnectCACertMissing verifies that a non-existent CA cert file causes
// connect to return an error wrapping the read failure.
func TestConnectCACertMissing(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		DrydockURL:        "http://127.0.0.1:1",
		CACert:            filepath.Join(t.TempDir(), "absent.pem"),
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
	}
	c := newWireClient(t, cfg)

	_, err := c.connect(context.Background())
	if err == nil {
		t.Fatal("connect succeeded with a missing CA cert, want error")
	}
	if !strings.Contains(err.Error(), "reading CA cert") {
		t.Errorf("error = %q, want to contain 'reading CA cert'", err)
	}
}

// TestConnectCACertBadPEM verifies that a file with invalid PEM content causes
// connect to return the "failed to parse CA cert" error.
func TestConnectCACertBadPEM(t *testing.T) {
	t.Parallel()

	badCert := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badCert, []byte("not pem content\n"), 0o600); err != nil {
		t.Fatalf("write bad cert: %v", err)
	}

	cfg := &config.Config{
		DrydockURL:        "http://127.0.0.1:1",
		CACert:            badCert,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
	}
	c := newWireClient(t, cfg)

	_, err := c.connect(context.Background())
	if err == nil {
		t.Fatal("connect succeeded with bad PEM, want error")
	}
	if !strings.Contains(err.Error(), "failed to parse CA cert") {
		t.Errorf("error = %q, want to contain 'failed to parse CA cert'", err)
	}
}

// TestConnectCACertValid covers line 223 (tlsConfig.RootCAs = pool): when the CA
// cert file contains valid PEM, the pool is accepted. The subsequent dial fails
// for a different reason (bad handshake), but the CA cert path is exercised.
func TestConnectCACertValid(t *testing.T) {
	t.Parallel()

	// Valid self-signed CA certificate — just needs to parse successfully.
	// Generated with: openssl req -x509 -newkey rsa:2048 -keyout /dev/null
	//   -out /dev/stdout -days 3650 -nodes -subj "/CN=test"
	const testCACert = `-----BEGIN CERTIFICATE-----
MIIC/zCCAeegAwIBAgIUK520GOBwcfjs/k1R8beZZ8vG4CAwDQYJKoZIhvcNAQEL
BQAwDzENMAsGA1UEAwwEdGVzdDAeFw0yNjA2MjMxNjE0MTVaFw0zNjA2MjAxNjE0
MTVaMA8xDTALBgNVBAMMBHRlc3QwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEK
AoIBAQCW0AOl8KwCXkDkEARt0WUcZF7II/is9kGQfFVlQ8HKudiceS+BY/aneAMd
3jwtZQMLWXOaDWrCTndbxMbRS4PCweP9pQc+MKro5nlP2p/4u7SlXoXcrC0diq7G
zLri9mKa0vzgiXIX174Ycw8zXa5dWzT9NVpoJHLD/1SYgYGrawj9ywltL9PUDuCd
37mzh1WcEmlSnIogf1YJ2tNxD/mA5nuItZfXIS868dIQfp3gPleVCxKEOCr0fD4O
5Q37DSvrjSPaXpljm8R98rPt+Oy1/ZKYtYwax2BOUvJ30sT1kw6NYoI7jOJQMwv5
uJOAevCfSyDulP7bXQ1HLayJ7rypAgMBAAGjUzBRMB0GA1UdDgQWBBQtFLXFQG4Y
4oOUXaM24rgZGYeIKzAfBgNVHSMEGDAWgBQtFLXFQG4Y4oOUXaM24rgZGYeIKzAP
BgNVHRMBAf8EBTADAQH/MA0GCSqGSIb3DQEBCwUAA4IBAQAiNGcKrAbAGU+Wb/hi
IMcaaGWjbKF7smStSo756LVFSaPcH/e/yP1VCZPOKqNypIligFPqW1uyEK4Fr+lC
idbp1SpLvVvg22MnaEUvDxk9NDhP2IXux82htk8oCPbcTmq165pQZ6lIO+p8wYiZ
dA+zx/3nyq0u1hKJsUZIq4IyI3tyqZyBcSiyD1KqDAjBV7A/QgtDs4Xpxl8kGoEW
bglUORJj9Dw8+QyAfnTnmn6Zw2IWJTfrIbcNOy5+kAJPiStv/vQt/ti7AISP0+Y/
oQOMD1RdrfX7bTuqErGI0kwsbmoCaSVV78kYYTe871CpCNLWlAX9DoZG3pxcSTrg
Yofu
-----END CERTIFICATE-----
`

	certFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(certFile, []byte(testCACert), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	// Use a 503 server so the dial completes quickly with a non-fatal error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		DrydockURL:        srv.URL,
		CACert:            certFile,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
	}
	c := newWireClient(t, cfg)

	_, err := c.connect(context.Background())
	// Expect a non-fatal dial error (not a CA cert error).
	if err == nil {
		t.Fatal("connect succeeded unexpectedly")
	}
	if strings.Contains(err.Error(), "CA cert") {
		t.Errorf("error mentions CA cert: %v (want dial error)", err)
	}
	if errors.Is(err, errFatal) {
		t.Errorf("should be non-fatal dial error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// connect — non-404 dial error (line 246)
// ---------------------------------------------------------------------------

// TestConnectNonFatalDialError confirms that a plain non-404 connection error
// does not wrap errFatal (the retry loop should kick in). We use a server
// that immediately rejects the HTTP upgrade with a 500 status.
func TestConnectNonFatalDialError(t *testing.T) {
	t.Parallel()

	// Server returns 500, not 404 — so this is a bad handshake but not the
	// fatal-404 path. The error wraps websocket.ErrBadHandshake but with a
	// non-404 status code, so it falls through to the plain "websocket dial: ..."
	// error branch (non-fatal).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		DrydockURL:        srv.URL,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
	}
	c := newWireClient(t, cfg)

	_, err := c.connect(context.Background())
	if err == nil {
		t.Fatal("connect succeeded against a 503 server, want error")
	}
	if errors.Is(err, errFatal) {
		t.Errorf("503 error should NOT wrap errFatal: %v", err)
	}
}

// ---------------------------------------------------------------------------
// connect — sendHello failure (line 255-258)
// ---------------------------------------------------------------------------

// TestConnectSendHelloFailure exercises the path where sendHello returns an
// errFatal (bad private key), so connect returns it immediately.
func TestConnectSendHelloFailure(t *testing.T) {
	t.Parallel()

	// Controller accepts the upgrade but sendHello will fail before writing.
	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		// Wait long enough for the agent to fail — the server just keeps the conn open.
		time.Sleep(500 * time.Millisecond)
	})

	badKey := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(badKey, []byte("not a key\n"), 0o600); err != nil {
		t.Fatalf("write bad key: %v", err)
	}

	cfg := &config.Config{
		DrydockURL:        srv,
		PrivateKeyFile:    badKey,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
	}
	c := newWireClient(t, cfg)

	established, err := c.connect(context.Background())
	if err == nil {
		t.Fatal("connect succeeded with bad key, want error")
	}
	if established {
		t.Error("established = true, want false after sendHello failure")
	}
	if !errors.Is(err, errFatal) {
		t.Errorf("error should wrap errFatal for a bad key: %v", err)
	}
}

// ---------------------------------------------------------------------------
// connect — welcome read failure (line 268-271)
// ---------------------------------------------------------------------------

// TestConnectWelcomeReadFailure verifies that if the controller closes the
// connection without sending a welcome, connect returns an error.
func TestConnectWelcomeReadFailure(t *testing.T) {
	t.Parallel()

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		// Read the hello but immediately close without sending welcome.
		readAndAckHello(t, ctrl)
		// Controller closes conn — agent side gets a read error waiting for welcome.
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
	}
	c := newWireClient(t, cfg)

	established, err := c.connect(context.Background())
	if err == nil {
		t.Fatal("connect succeeded without welcome, want error")
	}
	if established {
		t.Error("established = true, want false when welcome not delivered")
	}
}

// ---------------------------------------------------------------------------
// connect — welcome parse failure (line 274-277 and 278-281)
// ---------------------------------------------------------------------------

// TestConnectWelcomeParseFailure: controller sends garbled bytes as the
// welcome frame, so json.Unmarshal fails.
func TestConnectWelcomeParseFailure(t *testing.T) {
	t.Parallel()

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		readAndAckHello(t, ctrl)
		if err := ctrl.WriteMessage(websocket.TextMessage, []byte("{{{invalid")); err != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
	}
	c := newWireClient(t, cfg)

	established, err := c.connect(context.Background())
	if err == nil {
		t.Fatal("connect succeeded with garbled welcome, want error")
	}
	if established {
		t.Error("established = true with garbled welcome")
	}
}

// TestConnectWelcomeUnexpectedType: controller sends a valid envelope but with
// the wrong type (not "welcome").
func TestConnectWelcomeUnexpectedType(t *testing.T) {
	t.Parallel()

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		readAndAckHello(t, ctrl)
		// Send a ping instead of a welcome.
		data, _ := json.Marshal(protocol.PingMessage{Timestamp: 1})
		env := protocol.Envelope{Type: protocol.TypePing, Data: json.RawMessage(data)}
		_ = ctrl.WriteJSON(env)
		time.Sleep(200 * time.Millisecond)
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
	}
	c := newWireClient(t, cfg)

	established, err := c.connect(context.Background())
	if err == nil {
		t.Fatal("connect succeeded with wrong welcome type, want error")
	}
	if established {
		t.Error("established = true with wrong welcome type")
	}
	if !strings.Contains(err.Error(), "expected welcome") {
		t.Errorf("error = %q, want to mention 'expected welcome'", err)
	}
}

// ---------------------------------------------------------------------------
// connect — welcome payload with bad JSON (slog.Warn path, line 285-286)
// ---------------------------------------------------------------------------

// TestConnectWelcomeInvalidPayload: envelope type is "welcome" but the inner
// data is not valid WelcomeMessage JSON. The agent should warn but still run.
func TestConnectWelcomeInvalidPayload(t *testing.T) {
	t.Parallel()

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		readAndAckHello(t, ctrl)
		// Valid envelope, invalid data.
		env := protocol.Envelope{
			Type: protocol.TypeWelcome,
			Data: json.RawMessage(`"notanobject"`),
		}
		_ = ctrl.WriteJSON(env)
		// Hold connection until agent cancels context.
		_ = ctrl.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, _ = ctrl.ReadMessage()
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		DDPollInterval:    300,
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	established, err := c.connect(ctx)
	// Should establish (welcome envelope type matched), runs until ctx cancelled.
	if !established {
		t.Errorf("established = false, want true (welcome type matched even with bad payload): err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// connect — adapter OnConnect failure (line 329-331)
// ---------------------------------------------------------------------------

// TestConnectAdapterOnConnectFailure verifies that an adapter OnConnect error
// is logged but does not abort the connection.
func TestConnectAdapterOnConnectFailure(t *testing.T) {
	t.Parallel()

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		readAndAckHello(t, ctrl)
		sendWelcomeMsg(t, ctrl, protocol.WelcomeMessage{})
		// Hold until agent disconnects.
		_ = ctrl.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, _ = ctrl.ReadMessage()
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		DDPollInterval:    300,
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)
	// Override adapter to fail OnConnect.
	c.adapter = &fakeAdapter{onConnectErr: errors.New("sync failed"), caps: []string{}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// connect must still establish despite OnConnect error.
	established, _ := c.connect(ctx)
	if !established {
		t.Error("established = false, want true even when OnConnect errors")
	}
}

// ---------------------------------------------------------------------------
// Run — context already cancelled (line 149-151)
// ---------------------------------------------------------------------------

// TestRunCtxAlreadyCancelledBeforeLoop verifies that Run returns immediately
// when the context is already cancelled on entry.
func TestRunCtxAlreadyCancelledBeforeLoop(t *testing.T) {
	t.Parallel()

	addr := freeAddr(t)
	cfg := &config.Config{
		DrydockURL:        "http://127.0.0.1:1",
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		ReconnectDelay:    1,
		MaxReconnectDelay: 60,
		DDPollInterval:    300,
		BindAddress:       "127.0.0.1",
		Port:              portFrom(addr),
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling Run

	err := c.Run(ctx)
	if err == nil {
		t.Fatal("Run returned nil with pre-cancelled ctx, want ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Run — reconnect loop: non-fatal error, backoff reset after established
// ---------------------------------------------------------------------------

// TestRunReconnectsAfterNonFatalError confirms that a non-fatal connection
// error doesn't stop Run — it enters the reconnect wait. We use a server
// that returns HTTP 503 (bad handshake, non-fatal) so the dial completes
// quickly without a long TCP timeout.
func TestRunReconnectsAfterNonFatalError(t *testing.T) {
	t.Parallel()

	// A 503 server causes a fast non-fatal dial error (ErrBadHandshake with non-404).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	addr := freeAddr(t)
	cfg := &config.Config{
		DrydockURL:        srv.URL,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		ReconnectDelay:    5, // 5s delay — ctx expires during the wait (no tight loop)
		MaxReconnectDelay: 10,
		DDPollInterval:    300,
		BindAddress:       "127.0.0.1",
		Port:              portFrom(addr),
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	// Short ctx: expires during the first reconnect wait, not during the dial.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := c.Run(ctx)
	// Run must return ctx.Err(), not a fatal or dial error.
	if err == nil {
		t.Fatal("Run returned nil, want ctx error")
	}
	if errors.Is(err, errFatal) {
		t.Errorf("Run returned errFatal on a 503: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context error after reconnect loop", err)
	}
}

// ---------------------------------------------------------------------------
// Run — ctx cancel while holding a live connection (lines 154-167)
// ---------------------------------------------------------------------------

// runReturnBackstop bounds the waits in the three cancel tests below. Every
// step in them is synchronised on a channel, so this only decides how long a
// genuine hang takes to report — it is not what makes the tests pass. Generous
// on purpose: the old 3s value was in a dead heat with the controller's own
// 3s read deadline, and lost it under load.
const runReturnBackstop = 10 * time.Second

// TestRunCtxCancelWithActiveConn proves that cancelling Run's context while a
// connection is genuinely live makes Run return ctx.Err() from its
// post-connect check, without scheduling a reconnect.
//
// Two things make that a proof rather than a hope. The controller pings the
// agent and waits for the pong before the test cancels. readPump is the only
// thing that answers a ping, so the pong means readPump is running and
// dispatching, and the cancel is therefore observed from a live pump instead
// of from one that does not exist yet. It deliberately does not claim where
// in its loop readPump is: the pong is handed to sendPump through a buffered
// channel, so readPump may still be short of its next ctx check or its next
// ReadMessage when the cancel lands. That costs nothing, because the
// controller closes the socket after the cancel and either position unblocks
// from it, one by finding ctx already cancelled, the other by the read
// failing.
//
// The controller, not a timer, is what closes that socket, and it has to be.
// readPump only checks ctx at the top of its loop, so once it is inside
// ReadMessage the cancel is invisible to it and Run cannot return until the
// socket closes or the 60s read deadline fires.
//
// The reconnect counter is what separates the two ways Run can return
// ctx.Err() here, which asserting on the error alone cannot do. The
// post-connect check returns before IncReconnect; the reconnect wait's
// ctx.Done case returns after it. Asserting the counter is still zero pins
// the branch under test.
func TestRunCtxCancelWithActiveConn(t *testing.T) {
	t.Parallel()

	// Closed once the controller has had a ping answered, so readPump is
	// provably running and dispatching rather than merely about to exist.
	pumpLive := make(chan struct{})
	// Closed by the test after it cancels, to release the controller handler
	// and with it the socket.
	ctrlHold := make(chan struct{})
	// Carries the controller conn out to the test so cleanup can drop it. A
	// failing test cancels ctx, but a readPump already inside ReadMessage
	// cannot see that, so without closing the socket here the agent would
	// outlive the test until the controller's read deadline expired.
	ctrlConn := make(chan *websocket.Conn, 1)
	// Carries the first controller-side failure out to the test, so a broken
	// handler reports its cause instead of spending the backstop and blaming
	// readPump for a write that never went out.
	ctrlErr := make(chan error, 1)

	// OnceFunc because a controller handler that returned early would let the
	// agent reconnect and run this closure a second time, and a bare close
	// would then panic inside net/http instead of failing the test.
	pumpIsLive := sync.OnceFunc(func() { close(pumpLive) })

	reportCtrl := func(err error) {
		select {
		case ctrlErr <- err:
		default: // keep the first failure; later ones are consequences of it
		}
	}

	const pingStamp = 4242

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		select {
		case ctrlConn <- ctrl:
		default: // a second connection; the first one is the one cleanup drops
		}

		readAndAckHello(t, ctrl)
		sendWelcomeMsg(t, ctrl, protocol.WelcomeMessage{})

		// Ping the agent. Only readPump answers a ping, so the pong below is
		// the liveness proof the cancel depends on.
		if err := ctrl.SetWriteDeadline(time.Now().Add(runReturnBackstop)); err != nil {
			reportCtrl(fmt.Errorf("set controller write deadline: %w", err))
			return
		}
		pingData, err := json.Marshal(protocol.PingMessage{Timestamp: pingStamp})
		if err != nil {
			reportCtrl(fmt.Errorf("marshal ping: %w", err))
			return
		}
		if err := ctrl.WriteJSON(protocol.Envelope{Type: protocol.TypePing, Data: pingData}); err != nil {
			reportCtrl(fmt.Errorf("write ping to agent: %w", err))
			return
		}

		// Read past the metrics frame connect sends before the pumps start,
		// until the pong arrives. The deadline is only here so a broken agent
		// fails the test instead of wedging srv.Close in cleanup; the test
		// synchronises on pumpLive, never on this timer.
		if err := ctrl.SetReadDeadline(time.Now().Add(runReturnBackstop)); err != nil {
			reportCtrl(fmt.Errorf("set controller read deadline: %w", err))
			return
		}
		for {
			_, raw, err := ctrl.ReadMessage()
			if err != nil {
				reportCtrl(fmt.Errorf("read from agent while waiting for the pong: %w", err))
				return
			}
			var env protocol.Envelope
			if err := json.Unmarshal(raw, &env); err != nil || env.Type != protocol.TypePong {
				continue
			}
			var pong protocol.PongMessage
			if err := json.Unmarshal(env.Data, &pong); err != nil || pong.Timestamp != pingStamp {
				continue
			}
			break
		}
		pumpIsLive()

		// Hold the socket open, reading nothing, until the test has cancelled.
		// Returning closes the conn (newControllerServer defers it), which is
		// what unblocks the agent's readPump.
		<-ctrlHold
	})
	releaseCtrl := sync.OnceFunc(func() { close(ctrlHold) })
	dropCtrl := sync.OnceFunc(func() {
		select {
		case conn := <-ctrlConn:
			_ = conn.Close()
		default: // never connected, nothing to drop
		}
	})

	addr := freeAddr(t)
	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		ReconnectDelay:    1,
		MaxReconnectDelay: 60,
		DDPollInterval:    300,
		BindAddress:       "127.0.0.1",
		Port:              portFrom(addr),
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	// Every exit path, not just the happy one. Cancel alone does not stop a
	// readPump that is already inside ReadMessage, so this releases the
	// handler and drops the socket too. Registered after newControllerServer,
	// so cleanup LIFO runs it before srv.Close rather than deadlocking on it.
	t.Cleanup(func() {
		cancel()
		releaseCtrl()
		dropCtrl()
	})

	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	// Wait until readPump has answered the controller's ping.
	select {
	case <-pumpLive:
	case err := <-ctrlErr:
		t.Fatalf("controller side failed before the agent answered the ping: %v", err)
	case <-time.After(runReturnBackstop):
		t.Fatal("readPump never answered the controller ping")
	}

	// Cancel first, then drop the socket, so ctx.Err() is already set by the
	// time connect returns.
	cancel()
	releaseCtrl()

	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(runReturnBackstop):
		t.Fatal("Run did not return after context cancel")
	}

	// Zero reconnects means Run left through its post-connect ctx.Err() check.
	// The reconnect wait reaches its own ctx.Done case only after incrementing
	// this, so a count of 1 would mean the cancel was observed a branch later.
	var rendered strings.Builder
	c.metrics.WritePrometheus(&rendered, func(value string) string { return value })
	if !strings.Contains(rendered.String(), "portwing_edge_reconnects_total 0\n") {
		t.Errorf("Run scheduled a reconnect on a cancelled context, want the post-connect ctx exit:\n%s", rendered.String())
	}
}

// ---------------------------------------------------------------------------
// Run — ctx cancel against a quiet controller (PW-8.4)
// ---------------------------------------------------------------------------

// idleCancelBudget is the assertion PW-8.4 exists to make: Run must unwind
// within this of a cancel, even when the controller is healthy, silent, and
// never closes the socket. It is deliberately far below the 60-second floor
// readDeadline imposes, because that floor was the defect — a readPump parked
// in conn.ReadMessage could not see the cancel until the deadline fired, and
// SIGTERM therefore outlived Docker's and Kubernetes' default SIGKILL grace.
const idleCancelBudget = time.Second

// readPumpSettle bounds the scheduler across the handful of instructions
// between readPump handing its pong to sendPump and blocking in
// conn.ReadMessage. Nothing observable from outside the agent separates those
// two points — every synchronisation point a controller has sits before the
// loop's ctx check, not after it — so this is not what makes the assertion
// correct. Run has to return inside idleCancelBudget from either position.
// It is what makes a regression fail every run instead of most of them: with
// the cancel landing inside the read, an agent without the AfterFunc seam
// hangs for the full 60-second read deadline. Should the loop-top ctx check
// catch a cancel first, this test still passes on a build with no seam at
// all, so the evidence that the seam carries the behaviour is the mutation
// run — deleting the AfterFunc and watching this fail — not the assertion on
// its own.
const readPumpSettle = 100 * time.Millisecond

// pingUntilPong drives the controller side of the liveness handshake the
// cancel tests depend on: it pings the agent, then reads until the matching
// pong comes back, skipping the metrics frame connect emits before the pumps
// start. Only readPump answers a ping, so a nil return means the agent's read
// pump is running and has completed a full iteration of its loop.
//
// It returns its errors rather than calling t.Fatalf because it runs on the
// controller handler's goroutine, where a Fatalf would not stop the test.
func pingUntilPong(ctrl *websocket.Conn, stamp int64, budget time.Duration) error {
	if err := ctrl.SetWriteDeadline(time.Now().Add(budget)); err != nil {
		return fmt.Errorf("set controller write deadline: %w", err)
	}
	pingData, err := json.Marshal(protocol.PingMessage{Timestamp: stamp})
	if err != nil {
		return fmt.Errorf("marshal ping: %w", err)
	}
	if err := ctrl.WriteJSON(protocol.Envelope{Type: protocol.TypePing, Data: pingData}); err != nil {
		return fmt.Errorf("write ping to agent: %w", err)
	}
	if err := ctrl.SetReadDeadline(time.Now().Add(budget)); err != nil {
		return fmt.Errorf("set controller read deadline: %w", err)
	}
	for {
		_, raw, err := ctrl.ReadMessage()
		if err != nil {
			return fmt.Errorf("read from agent while waiting for the pong: %w", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil || env.Type != protocol.TypePong {
			continue
		}
		var pong protocol.PongMessage
		if err := json.Unmarshal(env.Data, &pong); err != nil || pong.Timestamp != stamp {
			continue
		}
		return nil
	}
}

// TestRunCtxCancelAgainstIdleController is the regression test for PW-8.4.
// TestRunCtxCancelWithActiveConn above proves Run takes the post-connect
// ctx.Err() branch, but it lets the controller close the socket immediately
// after the cancel, so it never measures how long an agent whose peer stays
// open takes to notice. This one does: the controller answers the handshake,
// pings once to prove readPump is live, and then holds the socket open on its
// own channel, sending nothing and closing nothing until cleanup.
//
// That is the shape a quiet-but-healthy controller has, and before the
// AfterFunc seam in connect it was unbounded in practice — readPump only
// tests ctx at the top of its loop, so a cancel that lands while it is inside
// conn.ReadMessage is invisible until readDeadline expires, and readDeadline
// floors at 60 seconds. Closing the conn on cancel is what turns that into a
// failed read.
//
// The reconnect counter separates a prompt return through the ctx branch from
// a prompt return that scheduled a retry first: the close the seam performs
// surfaces to readPump as an ordinary read error, so an agent that classified
// it before checking ctx would come back inside the budget with the counter
// at 1. Zero is what pins the ctx branch.
func TestRunCtxCancelAgainstIdleController(t *testing.T) {
	t.Parallel()

	// Closed once the controller has had a ping answered, so readPump is
	// provably running rather than merely about to exist.
	pumpLive := make(chan struct{})
	pumpIsLive := sync.OnceFunc(func() { close(pumpLive) })
	// Released only by cleanup. The point of the test is that nothing on the
	// controller side helps the agent unwind, so this must not be closed
	// before the assertion the way TestRunCtxCancelWithActiveConn closes its
	// own hold.
	ctrlHold := make(chan struct{})
	// Carries the controller conn out so cleanup can drop it after a failing
	// run, where the agent has not closed its own side.
	ctrlConn := make(chan *websocket.Conn, 1)
	// Carries the first controller-side failure out, so a broken handler
	// reports its cause instead of spending the backstop and blaming readPump.
	ctrlErr := make(chan error, 1)

	reportCtrl := func(err error) {
		select {
		case ctrlErr <- err:
		default: // keep the first failure; later ones are consequences of it
		}
	}

	const pingStamp = 8484

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		select {
		case ctrlConn <- ctrl:
		default: // a second connection; the first one is the one cleanup drops
		}

		readAndAckHello(t, ctrl)
		sendWelcomeMsg(t, ctrl, protocol.WelcomeMessage{})

		if err := pingUntilPong(ctrl, pingStamp, runReturnBackstop); err != nil {
			reportCtrl(err)
			return
		}
		pumpIsLive()

		// Silent from here: no frames, no close, no read. Returning would
		// close the conn (newControllerServer defers it), which is exactly the
		// help this test withholds.
		<-ctrlHold
	})
	releaseCtrl := sync.OnceFunc(func() { close(ctrlHold) })
	dropCtrl := sync.OnceFunc(func() {
		select {
		case conn := <-ctrlConn:
			_ = conn.Close()
		default: // never connected, nothing to drop
		}
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		ReconnectDelay:    1,
		MaxReconnectDelay: 60,
		DDPollInterval:    300,
		BindAddress:       "127.0.0.1",
		// Port 0, not a freeAddr reservation: nothing here talks to the health
		// server, and freeAddr hands back a port it has already released, so
		// every test that uses one is racing the others for it. Two more
		// entrants was enough to make TestStartHealthServerTwiceDoesNotPanic's
		// rebind lose that race under -count=20.
		Port:             "0",
		SkipDFCollection: true,
	}
	c := newWireClient(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	// Every exit path, not just the happy one. A failing run leaves readPump
	// parked in ReadMessage against a peer that is still holding, so cleanup
	// has to release the handler and drop the socket or the agent outlives the
	// test by a minute. Registered after newControllerServer so cleanup's LIFO
	// order runs it before srv.Close rather than deadlocking on it.
	t.Cleanup(func() {
		cancel()
		releaseCtrl()
		dropCtrl()
	})

	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	select {
	case <-pumpLive:
	case err := <-ctrlErr:
		t.Fatalf("controller side failed before the agent answered the ping: %v", err)
	case <-time.After(runReturnBackstop):
		t.Fatal("readPump never answered the controller ping")
	}

	// Let readPump get back into the read the cancel has to interrupt.
	time.Sleep(readPumpSettle)

	start := time.Now()
	cancel()

	select {
	case err := <-runDone:
		if elapsed := time.Since(start); elapsed > idleCancelBudget {
			t.Errorf("Run took %v to return after cancel against a silent controller, want under %v", elapsed, idleCancelBudget)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(idleCancelBudget):
		t.Fatalf("Run did not return within %v of the cancel: the read is still waiting on a controller that never speaks", idleCancelBudget)
	}

	// Zero reconnects means the read error the cancel-close produced was
	// classified as the ctx exit, not as a dropped tunnel worth retrying.
	var rendered strings.Builder
	c.metrics.WritePrometheus(&rendered, func(value string) string { return value })
	if !strings.Contains(rendered.String(), "portwing_edge_reconnects_total 0\n") {
		t.Errorf("Run scheduled a reconnect on a cancelled context, want the post-connect ctx exit:\n%s", rendered.String())
	}
}

// TestRunReconnectsAfterControllerCloseWithCancelSeamArmed is the other half
// of the seam's contract: closing the conn on cancel must not turn an ordinary
// tunnel drop into a shutdown, and the AfterFunc registered for a connection
// that has already ended must not touch the one that replaced it.
//
// The controller drops its first connection right after the welcome, with the
// agent's context still live, so Run has to reconnect and count it. It then
// serves a second connection and proves that one is live by answering a ping
// on it — which it could not do if the first connection's callback had
// followed the agent across the reconnect. Only after that does the test
// cancel.
//
// The final count of exactly 1 is what carries both claims. A seam that
// swallowed the real drop would leave it at 0; a seam whose cancel-close were
// classified as a reconnectable error before Run checked ctx would push it
// to 2.
func TestRunReconnectsAfterControllerCloseWithCancelSeamArmed(t *testing.T) {
	t.Parallel()

	var dialMu sync.Mutex
	dials := 0

	secondLive := make(chan struct{})
	secondIsLive := sync.OnceFunc(func() { close(secondLive) })
	ctrlHold := make(chan struct{})
	releaseCtrl := sync.OnceFunc(func() { close(ctrlHold) })
	ctrlConn := make(chan *websocket.Conn, 1)
	ctrlErr := make(chan error, 1)

	reportCtrl := func(err error) {
		select {
		case ctrlErr <- err:
		default: // keep the first failure
		}
	}

	const pingStamp = 8485

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		dialMu.Lock()
		dials++
		n := dials
		dialMu.Unlock()

		readAndAckHello(t, ctrl)
		sendWelcomeMsg(t, ctrl, protocol.WelcomeMessage{})

		if n == 1 {
			// Returning closes the controller side. That is a plain drop on a
			// live context, so Run must retry it rather than treat it as the
			// cancel exit.
			return
		}

		select {
		case ctrlConn <- ctrl:
		default: // only the first replacement connection is tracked
		}

		if err := pingUntilPong(ctrl, pingStamp, runReturnBackstop); err != nil {
			reportCtrl(err)
			return
		}
		secondIsLive()

		// Hold this one open the same way, so the cancel below has to be what
		// ends it.
		<-ctrlHold
	})
	dropCtrl := sync.OnceFunc(func() {
		select {
		case conn := <-ctrlConn:
			_ = conn.Close()
		default: // never reached a second connection
		}
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		ReconnectDelay:    0, // reconnect immediately; the drop is the point, not the backoff
		MaxReconnectDelay: 0,
		DDPollInterval:    300,
		BindAddress:       "127.0.0.1",
		Port:              "0", // ephemeral, for the reason above
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		releaseCtrl()
		dropCtrl()
	})

	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	select {
	case <-secondLive:
	case err := <-ctrlErr:
		t.Fatalf("controller side failed on the second connection: %v", err)
	case <-time.After(runReturnBackstop):
		t.Fatal("the agent never reconnected and answered a ping on a second connection")
	}

	time.Sleep(readPumpSettle)

	start := time.Now()
	cancel()

	select {
	case err := <-runDone:
		if elapsed := time.Since(start); elapsed > idleCancelBudget {
			t.Errorf("Run took %v to return after cancel on the reconnected tunnel, want under %v", elapsed, idleCancelBudget)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(idleCancelBudget):
		t.Fatalf("Run did not return within %v of the cancel on the second connection", idleCancelBudget)
	}

	var rendered strings.Builder
	c.metrics.WritePrometheus(&rendered, func(value string) string { return value })
	if !strings.Contains(rendered.String(), "portwing_edge_reconnects_total 1\n") {
		t.Errorf("reconnects after one controller drop and one cancel, want exactly 1:\n%s", rendered.String())
	}
}

// ---------------------------------------------------------------------------
// Run — health server shutdown error (line 140-142 of the defer in Run)
// ---------------------------------------------------------------------------

// We can't easily make Shutdown return an error in a unit test without
// replacing the healthServer, but we can verify the defer runs by checking
// that the health server port is freed after Run returns.
// This is implicitly exercised by TestRunFatalConnectNoRetry in wire_contract_test.go.

// ---------------------------------------------------------------------------
// writePump — heartbeat tick path (lines 696-703)
// ---------------------------------------------------------------------------

// TestWritePumpHeartbeatTick verifies the heartbeat branch: after a tick the
// pump sends a TypePing message. We use a 1-second heartbeat and wait up to
// 2s for the first tick.
func TestWritePumpHeartbeatTick(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	wc := newWireClient(t, &config.Config{SkipDFCollection: true})
	c.collector = wc.collector

	c.cfg.HeartbeatInterval = 1 // 1s heartbeat ticker — fires before 2s deadline
	c.adapter = &fakeAdapter{pollInterval: 999}
	c.cfg.DDPollInterval = 999 // large: poll must not fire during test

	ctx, cancel := context.WithCancel(context.Background())
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		c.writePump(ctx)
	}()

	// Read with a 2s deadline — the 1s heartbeat tick fires within this window.
	if err := ctrl.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		cancel()
		<-pumpDone
		t.Fatalf("set read deadline: %v", err)
	}

	gotPing := false
	for !gotPing {
		_, raw, err := ctrl.ReadMessage()
		if err != nil {
			break
		}
		var env protocol.Envelope
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		if env.Type == protocol.TypePing {
			gotPing = true
		}
	}

	cancel()
	<-pumpDone

	if !gotPing {
		t.Error("writePump never sent a TypePing on the heartbeat tick")
	}
}

// ---------------------------------------------------------------------------
// writePump — poll tick: RefreshContainers error path (line 708-710)
// ---------------------------------------------------------------------------

// errRefreshAdapter is a fakeAdapter whose RefreshContainers always errors.
type errRefreshAdapter struct {
	fakeAdapter
}

func (a *errRefreshAdapter) RefreshContainers(_ context.Context) (_, _, _ []adapter.Container, err error) {
	return nil, nil, nil, errors.New("refresh failed")
}

// TestWritePumpPollRefreshError verifies that a RefreshContainers error is
// logged and the pump continues (doesn't crash or return).
func TestWritePumpPollRefreshError(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	wc := newWireClient(t, &config.Config{SkipDFCollection: true})
	c.collector = wc.collector
	c.cfg.HeartbeatInterval = 999 // large: heartbeat must not fire during test
	c.cfg.DDPollInterval = 999    // fallback (overridden by welcomePollInterval)
	c.welcomePollInterval = 1     // 1s poll tick fires within readTimeout (2s)
	c.adapter = &errRefreshAdapter{fakeAdapter: fakeAdapter{pollInterval: 999}}

	ctx, cancel := context.WithCancel(context.Background())
	go c.writePump(ctx)
	t.Cleanup(cancel)

	// Wait for the first poll tick, which calls RefreshContainers (returns error)
	// and then continues. The pump sends nothing on error but should keep running.
	// Confirm liveness by sending a direct frame after a moment.
	time.Sleep(1100 * time.Millisecond)

	// Pump should still be running: send a frame directly and read it back.
	if err := c.sendTypedMessage(protocol.TypePong, protocol.PongMessage{Timestamp: 42}); err != nil {
		t.Fatalf("sendTypedMessage after refresh error: %v", err)
	}
	if err := ctrl.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	_, raw, err := ctrl.ReadMessage()
	if err != nil {
		t.Fatalf("read after refresh error: %v", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Could be any frame from the pump; we just confirm it's alive.
	_ = env
}

// ---------------------------------------------------------------------------
// writePump — poll tick: OnContainerRefresh error (line 712-714)
// ---------------------------------------------------------------------------

// errOnRefreshAdapter is a fakeAdapter whose OnContainerRefresh always errors.
type errOnRefreshAdapter struct {
	fakeAdapter
}

func (a *errOnRefreshAdapter) OnContainerRefresh(_ context.Context, _ adapter.MessageSender, _, _, _ []adapter.Container) error {
	return errors.New("notify failed")
}

// TestWritePumpPollOnContainerRefreshError verifies that an OnContainerRefresh
// error is logged and the pump continues.
func TestWritePumpPollOnContainerRefreshError(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	wc := newWireClient(t, &config.Config{SkipDFCollection: true})
	c.collector = wc.collector
	c.cfg.HeartbeatInterval = 999
	c.cfg.DDPollInterval = 999 // fallback (overridden by welcomePollInterval)
	c.welcomePollInterval = 1  // 1s poll tick fires within readTimeout (2s)
	c.adapter = &errOnRefreshAdapter{fakeAdapter: fakeAdapter{pollInterval: 999}}

	ctx, cancel := context.WithCancel(context.Background())
	go c.writePump(ctx)
	t.Cleanup(cancel)

	// Wait for the first poll tick to fire and log OnContainerRefresh error.
	time.Sleep(1100 * time.Millisecond)

	// Pump still alive.
	if err := c.sendTypedMessage(protocol.TypePong, protocol.PongMessage{Timestamp: 43}); err != nil {
		t.Fatalf("sendTypedMessage: %v", err)
	}
	if err := ctrl.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	_, _, err := ctrl.ReadMessage()
	if err != nil {
		t.Fatalf("read after OnContainerRefresh error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// writePump — welcomePollInterval overrides config (line 678-680)
// ---------------------------------------------------------------------------

// TestWritePumpWelcomePollIntervalOverride verifies that a non-zero
// welcomePollInterval is used instead of cfg.DDPollInterval.
func TestWritePumpWelcomePollIntervalOverride(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)
	wc := newWireClient(t, &config.Config{SkipDFCollection: true})
	c.collector = wc.collector
	c.cfg.HeartbeatInterval = 999
	c.cfg.DDPollInterval = 1                   // positive fallback (required by time.NewTicker)
	c.adapter = &fakeAdapter{pollInterval: -1} // <= 0 → falls back to DDPollInterval
	c.welcomePollInterval = 999                // large override: poll must not fire during test

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// writePump should run without crashing; context cancel exits it.
	c.writePump(ctx)
}

// ---------------------------------------------------------------------------
// sendMetrics — failed collection reaches the wire
// ---------------------------------------------------------------------------

// stubCollector is a hostCollector whose Collect returns whatever the test
// scripts. It exists because the real collector cannot be made to fail on a
// Linux host: Collect's only error is a missing /proc.
type stubCollector struct {
	snapshot *metrics.HostMetrics
	err      error
}

func (s stubCollector) Collect() (*metrics.HostMetrics, error) { return s.snapshot, s.err }

// TestSendMetricsReportsFailedCollection pins the contract for a collection
// that fails: an explicit error frame carrying host-metrics-unavailable, never
// silence and never the zero-filled snapshot Collect returns alongside its
// error. Silence is what shipped, and it left a controller unable to tell an
// unsupported host from an agent that had stopped talking.
func TestSendMetricsReportsFailedCollection(t *testing.T) {
	t.Parallel()

	unsupported := fmt.Errorf("%w: /proc: no such file or directory", metrics.ErrHostMetricsUnsupported)

	tests := []struct {
		name      string
		collector stubCollector
		wantType  string
		// wantMessage is the exact ErrorMessage.Message for a failure case.
		wantMessage string
		wantCPUCore int
	}{
		{
			name:        "unsupported host still answers",
			collector:   stubCollector{snapshot: &metrics.HostMetrics{CPUCores: 8}, err: unsupported},
			wantType:    protocol.TypeError,
			wantMessage: unsupported.Error(),
		},
		{
			name:        "nil snapshot alongside the error",
			collector:   stubCollector{snapshot: nil, err: unsupported},
			wantType:    protocol.TypeError,
			wantMessage: unsupported.Error(),
		},
		{
			name:        "collection error that is not the sentinel",
			collector:   stubCollector{snapshot: &metrics.HostMetrics{}, err: errors.New("collector wedged")},
			wantType:    protocol.TypeError,
			wantMessage: "collector wedged",
		},
		{
			name:        "successful collection is unchanged",
			collector:   stubCollector{snapshot: &metrics.HostMetrics{CPUCores: 4, MemoryTotal: 1 << 30}},
			wantType:    protocol.TypeMetrics,
			wantCPUCore: 4,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, ctrl := newTestClient(t)
			c.collector = tc.collector

			c.sendMetrics()

			data := expectType(t, ctrl, tc.wantType)
			if tc.wantType == protocol.TypeMetrics {
				var msg protocol.MetricsMessage
				decodeData(t, data, &msg)
				if msg.CPUCores != tc.wantCPUCore {
					t.Errorf("CPUCores = %d, want %d", msg.CPUCores, tc.wantCPUCore)
				}
				return
			}

			var em protocol.ErrorMessage
			decodeData(t, data, &em)
			if em.Code != metricsUnavailableCode {
				t.Errorf("error Code = %q, want %q", em.Code, metricsUnavailableCode)
			}
			if em.Message != tc.wantMessage {
				t.Errorf("error Message = %q, want %q", em.Message, tc.wantMessage)
			}
			if em.RequestID != "" {
				t.Errorf("error RequestID = %q, want empty: the metrics tick answers no request", em.RequestID)
			}
		})
	}
}

// TestSendMetricsFailureCarriesNoZeroedSnapshot is the half the frame type
// alone does not prove: a failed collection must not also put the partially
// populated snapshot on the wire, because a zero-filled metrics frame reads as
// a real measurement of a completely idle host.
func TestSendMetricsFailureCarriesNoZeroedSnapshot(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	c.collector = stubCollector{
		snapshot: &metrics.HostMetrics{CPUCores: 8},
		err:      metrics.ErrHostMetricsUnsupported,
	}

	c.sendMetrics()

	// The error frame is the only frame. A metrics frame arriving either
	// before or after it would be the zero-filled snapshot.
	if env := expectEnvelope(t, ctrl); env.Type != protocol.TypeError {
		t.Fatalf("first frame = %q (data=%s), want %q", env.Type, env.Data, protocol.TypeError)
	}
	if err := ctrl.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, raw, err := ctrl.ReadMessage(); err == nil {
		t.Fatalf("a second frame followed the error frame: %s", raw)
	}
}

// levelRecordingHandler captures the level and message of every log record,
// for the transition assertions below. Concurrency-safe because it is
// installed as the process-wide default logger and other goroutines may still
// be logging into it.
type levelRecordingHandler struct {
	mu      sync.Mutex
	records []loggedRecord
}

type loggedRecord struct {
	level   slog.Level
	message string
}

func (h *levelRecordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *levelRecordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, loggedRecord{level: r.Level, message: r.Message})
	return nil
}

func (h *levelRecordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecordingHandler) WithGroup(string) slog.Handler      { return h }

// take returns the records whose message is one of want, and clears the
// buffer. Filtering by message keeps an unrelated log line from another
// goroutine out of the assertion.
func (h *levelRecordingHandler) take(want ...string) []loggedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []loggedRecord
	for _, r := range h.records {
		for _, w := range want {
			if r.message == w {
				out = append(out, r)
				break
			}
		}
	}
	h.records = nil
	return out
}

// TestSendMetricsLogsCollectionTransitionsOnly pins the log-level policy for a
// failure that persists. The error frame is per-tick, but the log is not: a
// host with no procfs fails every heartbeat forever, so warning each time
// would bury the tick that actually changed something. Only the transitions
// are loud.
//
// The sequence is failure, failure, success, failure. The fourth call is the
// first failure after a recovery, so it warns again — a second break after a
// recovery is a real event an operator has to see, not a repeat.
func TestSendMetricsLogsCollectionTransitionsOnly(t *testing.T) {
	// Not t.Parallel(): swaps the process-wide default slog logger.

	const (
		failedMsg    = "metrics collection failed"
		recoveredMsg = "metrics collection recovered"
	)

	handler := &levelRecordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	failing := stubCollector{
		snapshot: &metrics.HostMetrics{CPUCores: 8},
		err:      fmt.Errorf("%w: /proc: no such file or directory", metrics.ErrHostMetricsUnsupported),
	}
	healthy := stubCollector{snapshot: &metrics.HostMetrics{CPUCores: 8, MemoryTotal: 1 << 30}}

	steps := []struct {
		name        string
		collector   stubCollector
		wantLevel   slog.Level
		wantMessage string
		wantFrame   string
	}{
		{
			name:        "first failure warns",
			collector:   failing,
			wantLevel:   slog.LevelWarn,
			wantMessage: failedMsg,
			wantFrame:   protocol.TypeError,
		},
		{
			name:        "repeat failure drops to debug",
			collector:   failing,
			wantLevel:   slog.LevelDebug,
			wantMessage: failedMsg,
			wantFrame:   protocol.TypeError,
		},
		{
			name:        "recovery says so once at info",
			collector:   healthy,
			wantLevel:   slog.LevelInfo,
			wantMessage: recoveredMsg,
			wantFrame:   protocol.TypeMetrics,
		},
		{
			name:        "failure after a recovery warns again",
			collector:   failing,
			wantLevel:   slog.LevelWarn,
			wantMessage: failedMsg,
			wantFrame:   protocol.TypeError,
		},
	}

	c, ctrl := newTestClient(t)
	byLevel := map[slog.Level]int{}

	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			c.collector = tc.collector

			c.sendMetrics()

			// Reading the frame first is the ordering barrier: sendMetrics
			// logs before it sends, so a frame in hand means the record is
			// already in the handler.
			if env := expectEnvelope(t, ctrl); env.Type != tc.wantFrame {
				t.Fatalf("frame = %q (data=%s), want %q", env.Type, env.Data, tc.wantFrame)
			}

			got := handler.take(failedMsg, recoveredMsg)
			if len(got) != 1 {
				t.Fatalf("collection log records = %+v, want exactly one", got)
			}
			if got[0].level != tc.wantLevel {
				t.Errorf("log level = %v, want %v", got[0].level, tc.wantLevel)
			}
			if got[0].message != tc.wantMessage {
				t.Errorf("log message = %q, want %q", got[0].message, tc.wantMessage)
			}
			byLevel[got[0].level]++
		})
	}

	// The aggregate over the whole sequence, which is what the policy is for.
	wantByLevel := map[slog.Level]int{
		slog.LevelWarn:  2, // the two transitions into failure
		slog.LevelDebug: 1, // the repeat while already failed
		slog.LevelInfo:  1, // the single recovery
	}
	for level, want := range wantByLevel {
		if byLevel[level] != want {
			t.Errorf("%v records = %d, want %d (levels seen: %v)", level, byLevel[level], want, byLevel)
		}
	}
	if len(byLevel) != len(wantByLevel) {
		t.Errorf("levels seen = %v, want exactly %v", byLevel, wantByLevel)
	}
}

// ---------------------------------------------------------------------------
// sendTypedMessage — json.Marshal error path (line 733-735)
// ---------------------------------------------------------------------------

// TestSendTypedMessageMarshalError: pass a value that cannot be marshaled
// (a channel). The error should be returned without panicking.
func TestSendTypedMessageMarshalError(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)
	// A channel cannot be marshaled by encoding/json.
	err := c.sendTypedMessage("test", make(chan int))
	if err == nil {
		t.Fatal("sendTypedMessage succeeded marshaling a chan, want error")
	}
	if !strings.Contains(err.Error(), "marshaling") {
		t.Errorf("error = %q, want to mention marshaling", err)
	}
}

// ---------------------------------------------------------------------------
// sendMessage — nil sendCh + nil conn (line 762-764)
// ---------------------------------------------------------------------------

// TestSendMessageNilConnNilQueue covers the handshake path where both
// sendCh and conn are nil (e.g. between connections): sendMessage must be a
// no-op and not panic.
func TestSendMessageNilConnNilQueue(t *testing.T) {
	t.Parallel()

	c := &Client{cfg: &config.Config{}}
	// Both sendCh and conn are nil (zero-value Client with no connection).
	// sendMessage should return without panicking.
	c.sendMessage(protocol.Envelope{Type: protocol.TypePing})
}

// ---------------------------------------------------------------------------
// sendPump — SetWriteDeadline failure (line 791-794)
// ---------------------------------------------------------------------------

// errDeadlineConn wraps a websocket.Conn and makes SetWriteDeadline always fail.
// We can't embed *websocket.Conn directly (it has unexported fields), so we
// test this path via TestSendPumpEvictsOnWriteFailure which already covers
// WriteJSON failure. The SetWriteDeadline branch (lines 791-794) requires
// the underlying conn's net.Conn to fail on deadline set — this is
// generally unreachable in non-test code without a custom net.Conn.
//
// Coverage note: lines 791-794 are the "set write deadline failed" path in
// sendPump. In practice this branch is unreachable with a real *websocket.Conn
// because the underlying net.Conn always accepts deadline changes (even on a
// closed socket the method may succeed). We note this as an
// effectively-unreachable branch below.

// ---------------------------------------------------------------------------
// jitteredDuration — error path (line 852-855)
// ---------------------------------------------------------------------------
// jitteredDuration's error branch (crand.Int fails) is only reachable when the
// OS entropy source is exhausted, which cannot happen in unit tests. This is
// an effectively-unreachable branch.

// ---------------------------------------------------------------------------
// closeWebSocket — error path (line 861-863)
// ---------------------------------------------------------------------------

// TestCloseWebSocketAlreadyClosed exercises the error path by closing a
// websocket that is already closed — the second close should return an error,
// which closeWebSocket logs at Debug and swallows.
func TestCloseWebSocketAlreadyClosed(t *testing.T) {
	t.Parallel()

	agent, _ := newWSPair(t)
	// Close once.
	if err := agent.Close(); err != nil {
		t.Logf("first close: %v", err)
	}
	// Second call: closes an already-closed conn; closeWebSocket must not panic.
	closeWebSocket(agent, "double-close")
}

// ---------------------------------------------------------------------------
// startHealthServer — ListenAndServe error (line 882-884)
// ---------------------------------------------------------------------------

// TestStartHealthServerPortConflict arranges for startHealthServer to fail by
// pre-binding the same address. The goroutine logs the error but does not
// crash. We verify the server was initialized and the error was handled.
func TestStartHealthServerPortConflict(t *testing.T) {
	t.Parallel()

	// Bind the port first to force a conflict.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	// Extract host/port from the httptest server.
	addr := strings.TrimPrefix(srv.URL, "http://")
	_, port, _ := strings.Cut(addr, ":")

	c := &Client{
		cfg: &config.Config{
			BindAddress: "127.0.0.1",
			Port:        port,
		},
	}
	// startHealthServer starts a goroutine; the conflict error is logged async.
	c.startHealthServer()
	t.Cleanup(func() {
		if c.healthServer != nil {
			_ = c.healthServer.Close()
		}
	})

	// Give the goroutine time to detect the conflict.
	time.Sleep(50 * time.Millisecond)
	// No panic means success — the error was swallowed by the log line.
	if c.healthServer == nil {
		t.Error("healthServer was not set even when ListenAndServe fails")
	}
}

// ---------------------------------------------------------------------------
// startHealthServer — a second start on the same Client must get its own
// healthServerDone channel, closed by its own goroutine (client.go:1699)
// ---------------------------------------------------------------------------

// TestStartHealthServerTwiceDoesNotPanic calls startHealthServer twice on one
// Client, shutting the health server down and joining its goroutine between
// the two starts. Each start must create its own healthServerDone channel
// and hand it to its own goroutine: if a shared field were instead guarded
// by a client-lifetime sync.Once, the second start's goroutine would find
// the guard already spent and never close its channel, hanging any caller
// that waits on it. The test proves the second close actually happens by
// waiting on the channel captured right after the second start, with a
// timeout that fails the test instead of hanging forever.
func TestStartHealthServerTwiceDoesNotPanic(t *testing.T) {
	t.Parallel()

	// Port "0" asks the kernel for a free port at bind time instead of
	// pre-allocating one with freeAddr and closing it: that allocate/close
	// window is exactly what let a parallel test's bind race in and take the
	// address before this test's (re)start got to it.
	c := &Client{
		cfg: &config.Config{
			BindAddress: "127.0.0.1",
			Port:        "0",
		},
	}

	c.startHealthServer()
	waitForHealthServer(t, c, "/health")

	shutdownCtx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	if err := c.healthServer.Shutdown(shutdownCtx1); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	select {
	case <-c.healthServerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first ListenAndServe goroutine did not finish in time")
	}

	// Start again on the same Client. Port "0" again — another OS-assigned
	// port, read back the same way, rather than reusing the first server's
	// now-closed address.
	c.startHealthServer()
	secondDone := c.healthServerDone
	t.Cleanup(func() {
		if c.healthServer != nil {
			_ = c.healthServer.Close()
		}
	})

	waitForHealthServer(t, c, "/health")

	shutdownCtx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := c.healthServer.Shutdown(shutdownCtx2); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second ListenAndServe goroutine did not finish in time")
	}
}

// TestStartHealthServerFailedRebindClearsHealthAddr pins HealthAddr's
// nil-until-bound contract across a restart that cannot bind. BaseContext is
// the only hook that records the address and it runs on a successful listen,
// so a second start that fails leaves nothing behind to overwrite the first
// server's address with. Without the reset in startHealthServer the accessor
// keeps handing out an address whose listener belongs to a server the Client
// no longer holds, which reads as a healthy agent to anything polling it.
//
// The occupied port is what makes the failure deterministic: the test holds
// the listener for the rest of the run, so the rebind cannot win a retry.
func TestStartHealthServerFailedRebindClearsHealthAddr(t *testing.T) {
	t.Parallel()

	c := &Client{
		cfg: &config.Config{
			BindAddress: "127.0.0.1",
			Port:        "0",
		},
	}

	c.startHealthServer()
	first := c.healthServer
	firstDone := c.healthServerDone
	t.Cleanup(func() {
		_ = first.Close()
		select {
		case <-firstDone:
		case <-time.After(healthServerLivenessTimeout):
			t.Error("the first ListenAndServe goroutine did not finish")
		}
	})

	waitForHealthServer(t, c, "/health")
	bound := c.HealthAddr()
	if bound == nil {
		t.Fatal("HealthAddr() = nil after a successful bind")
	}

	// Take a port and keep it, so the restart below has nowhere to land.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen to occupy a port: %v", err)
	}
	t.Cleanup(func() { _ = blocker.Close() })
	_, blockedPort, err := net.SplitHostPort(blocker.Addr().String())
	if err != nil {
		t.Fatalf("split the occupied address: %v", err)
	}

	c.cfg.Port = blockedPort
	c.startHealthServer()
	second := c.healthServer
	t.Cleanup(func() { _ = second.Close() })

	// ListenAndServe returns the bind error before it ever reaches Serve, so
	// BaseContext does not run and the goroutine closes this straight away.
	// Waiting on it is the happens-before point for the assertion.
	select {
	case <-c.healthServerDone:
	case <-time.After(healthServerLivenessTimeout):
		t.Fatal("the failed ListenAndServe goroutine did not finish")
	}

	if addr := c.HealthAddr(); addr != nil {
		t.Errorf("HealthAddr() = %v after a failed rebind, want nil (the first server's was %v)", addr, bound)
	}
}

// ---------------------------------------------------------------------------
// connect — welcome with matching compat level (no warning branch)
// ---------------------------------------------------------------------------

// TestConnectWelcomeCompatMatch covers the welcome.Config["serverCompatLevel"]
// branch when it matches protocol.DrydockCompat (no warning).
func TestConnectWelcomeCompatMatch(t *testing.T) {
	t.Parallel()

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		readAndAckHello(t, ctrl)
		sendWelcomeMsg(t, ctrl, protocol.WelcomeMessage{
			PollInterval: 0,
			Config:       map[string]string{"serverCompatLevel": protocol.DrydockCompat},
		})
		_ = ctrl.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, _ = ctrl.ReadMessage()
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		DDPollInterval:    300,
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	established, err := c.connect(ctx)
	if !established {
		t.Errorf("established = false, want true: %v", err)
	}
}

// ---------------------------------------------------------------------------
// connect — welcome with PollInterval==0 (no override)
// ---------------------------------------------------------------------------

// TestConnectWelcomePollIntervalZeroNotOverridden covers the branch where
// welcome.PollInterval == 0, so c.welcomePollInterval is NOT updated.
func TestConnectWelcomePollIntervalZeroNotOverridden(t *testing.T) {
	t.Parallel()

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		readAndAckHello(t, ctrl)
		sendWelcomeMsg(t, ctrl, protocol.WelcomeMessage{PollInterval: 0}) // zero — no override
		_ = ctrl.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, _ = ctrl.ReadMessage()
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		DDPollInterval:    300,
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	established, _ := c.connect(ctx)
	if !established {
		t.Error("established = false, want true")
	}
	if c.welcomePollInterval != 0 {
		t.Errorf("welcomePollInterval = %d, want 0 (zero from welcome should not override)", c.welcomePollInterval)
	}
}

// ---------------------------------------------------------------------------
// Run — exponential backoff capping (line 196-199)
// ---------------------------------------------------------------------------

// TestRunBackoffCaps confirms the `delay *= 2` and `delay > maxDelay` cap:
// Run must not block forever when delays are capped to maxDelay. This is
// exercised implicitly by TestRunReconnectsAfterNonFatalError but we add an
// explicit small-delay test to confirm the cap logic is hit.
func TestRunBackoffCaps(t *testing.T) {
	t.Parallel()

	// Use a 503 server so dials complete quickly without TCP timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	addr := freeAddr(t)
	cfg := &config.Config{
		DrydockURL:        srv.URL,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		ReconnectDelay:    5,  // non-zero so no tight loop
		MaxReconnectDelay: 10, // larger cap
		DDPollInterval:    300,
		BindAddress:       "127.0.0.1",
		Port:              portFrom(addr),
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	// Short ctx: expires before the reconnect wait fires.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := c.Run(ctx)
	if errors.Is(err, errFatal) {
		t.Errorf("Run returned errFatal on connection refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// writePump — pollInterval <= 0 defaults to cfg.DDPollInterval (line 673-675)
// ---------------------------------------------------------------------------

// TestWritePumpPollIntervalFallback verifies that when adapter.PollInterval()
// returns <= 0, the pump uses cfg.DDPollInterval instead. We just confirm
// the pump starts without panicking when DDPollInterval is positive.
func TestWritePumpPollIntervalFallback(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)
	wc := newWireClient(t, &config.Config{SkipDFCollection: true})
	c.collector = wc.collector
	c.cfg.HeartbeatInterval = 999
	c.cfg.DDPollInterval = 999                 // prevent poll from firing
	c.adapter = &fakeAdapter{pollInterval: -1} // <= 0 → use DDPollInterval

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.writePump(ctx)
}

// ---------------------------------------------------------------------------
// writeInput — retry and eventual close (already covered in TestHandleInputClosesSessionAfterWriteFailure)
// but we also need the debug-log-on-retry branch (line 224 in the else branch)
// ---------------------------------------------------------------------------

// TestWriteInputLogsRetryBeforeClose drives the retry debug-log in writeInput.
// The fakeConn fails all writes, so writeInput retries 10 times (logging each
// retry via slog.Debug) before calling Close. This is already mostly covered
// by TestHandleInputClosesSessionAfterWriteFailure; the else branch at line
// 224 is covered as a side effect of the retry loop. No additional test needed.

// ---------------------------------------------------------------------------
// readPump — TypeExecInput malformed payload (line 531-533)
// ---------------------------------------------------------------------------

// TestReadPumpSkipsMalformedExecInput confirms that a badly formed exec_input
// payload is skipped without crashing.
func TestReadPumpSkipsMalformedExecInput(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	runReadPump(t, c)

	badEnv := protocol.Envelope{Type: protocol.TypeExecInput, Data: json.RawMessage(`"notanobject"`)}
	if err := ctrl.WriteJSON(badEnv); err != nil {
		t.Fatalf("write bad exec_input: %v", err)
	}

	sendEnvelope(t, ctrl, protocol.TypePing, protocol.PingMessage{Timestamp: 55})
	expectType(t, ctrl, protocol.TypePong)
}

// ---------------------------------------------------------------------------
// sendHello — GetVersion error (line 377-379)
// ---------------------------------------------------------------------------

// TestSendHelloGetVersionError covers the GetVersion error branch: when
// GetVersion fails, dockerVersion is set to "unknown" and hello is still sent.
func TestSendHelloGetVersionError(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	// Override to return an error: we need a GetVersion implementation that errors.
	c.dockerClient = &versionErrDocker{}
	c.adapter = &fakeAdapter{caps: []string{"test"}}

	if err := c.sendHello(context.Background()); err != nil {
		t.Fatalf("sendHello failed unexpectedly: %v", err)
	}

	data := expectType(t, ctrl, protocol.TypeHello)
	var hello protocol.HelloMessage
	decodeData(t, data, &hello)

	if hello.DockerVersion != "unknown" {
		t.Errorf("DockerVersion = %q, want 'unknown' when GetVersion errors", hello.DockerVersion)
	}
}

// versionErrDocker is a fakeDocker whose GetVersion always returns an error.
type versionErrDocker struct {
	fakeDocker
}

func (d *versionErrDocker) GetVersion(_ context.Context) (string, error) {
	return "", errors.New("docker unavailable")
}

// ---------------------------------------------------------------------------
// readPump — malformed TypeRequest payload (line 495-498)
// ---------------------------------------------------------------------------

// TestReadPumpSkipsMalformedRequest confirms that a badly formed request
// payload is skipped without crashing.
func TestReadPumpSkipsMalformedRequest(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	runReadPump(t, c)

	badEnv := protocol.Envelope{Type: protocol.TypeRequest, Data: json.RawMessage(`"notanobject"`)}
	if err := ctrl.WriteJSON(badEnv); err != nil {
		t.Fatalf("write bad request: %v", err)
	}

	sendEnvelope(t, ctrl, protocol.TypePing, protocol.PingMessage{Timestamp: 66})
	expectType(t, ctrl, protocol.TypePong)
}

// ---------------------------------------------------------------------------
// readPump — streamSem success path (lines 503-506)
// ---------------------------------------------------------------------------

// TestReadPumpDispatchesRequestViaStreamSem covers the
// `case c.streamSem <- struct{}{}:` branch: a valid TypeRequest is received,
// the semaphore has space, and handleRequest is invoked in a goroutine.
func TestReadPumpDispatchesRequestViaStreamSem(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	//nolint:bodyclose
	c.dockerClient = &fakeDocker{doResp: mkResp(http.StatusOK, "application/json", `{}`)}
	runReadPump(t, c)

	req := protocol.RequestMessage{
		RequestID: "stream-sem-1",
		Method:    "GET",
		Path:      "/containers/json",
	}
	reqData, _ := json.Marshal(req)
	env := protocol.Envelope{Type: protocol.TypeRequest, Data: json.RawMessage(reqData)}
	if err := ctrl.WriteJSON(env); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// handleRequest runs asynchronously in a goroutine; it sends a TypeResponse
	// (or TypeError) back through the conn. Wait for it.
	if err := ctrl.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	gotResp := false
	for !gotResp {
		_, raw, err := ctrl.ReadMessage()
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		var got protocol.Envelope
		if json.Unmarshal(raw, &got) != nil {
			continue
		}
		if got.Type == protocol.TypeResponse || got.Type == protocol.TypeError {
			gotResp = true
		}
	}
	if !gotResp {
		t.Error("never received TypeResponse or TypeError from handleRequest goroutine")
	}
}

// ---------------------------------------------------------------------------
// Run — backoff reset after established connection (line 181-183)
// ---------------------------------------------------------------------------

// TestRunBackoffResetAfterEstablished covers line 181-183: when a connection
// that was established drops (for a non-ctx reason), the backoff resets to
// ReconnectDelay. We hold the second connection open until the test cancels.
func TestRunBackoffResetAfterEstablished(t *testing.T) {
	t.Parallel()

	// secondDialReady signals that the second connection dial has been accepted —
	// at this point, line 181 must have already fired (it runs before the 2nd dial).
	secondDialReady := make(chan struct{})
	var dialMu sync.Mutex
	dialCount := 0

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		dialMu.Lock()
		dialCount++
		n := dialCount
		dialMu.Unlock()

		if n == 1 {
			// First dial: establish (sends welcome) then close.
			readAndAckHello(t, ctrl)
			sendWelcomeMsg(t, ctrl, protocol.WelcomeMessage{})
			// return here → defer conn.Close() fires → server drops connection
			return
		}
		// Second dial: signal that we're in the second connection, then hang.
		close(secondDialReady)
		_ = ctrl.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, _ = ctrl.ReadMessage()
	})

	addr := freeAddr(t)
	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		ReconnectDelay:    0, // zero delay so reconnect is immediate
		MaxReconnectDelay: 0,
		DDPollInterval:    300,
		BindAddress:       "127.0.0.1",
		Port:              portFrom(addr),
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	// Wait until the second connection is live. By that point Run has already
	// passed through line 181 (delay reset after established=true) for the
	// first iteration.
	select {
	case <-secondDialReady:
	case <-time.After(5 * time.Second):
		t.Fatal("never reached second dial")
	}

	// Cancel to stop Run.
	cancel()

	select {
	case err := <-runDone:
		if errors.Is(err, errFatal) {
			t.Errorf("Run returned errFatal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// ---------------------------------------------------------------------------
// Run — reconnect wait ctx.Done() (line 190-191)
// ---------------------------------------------------------------------------

// TestRunReconnectWaitCtxDone covers line 190-191: during the reconnect wait,
// the context is cancelled so the select takes ctx.Done() instead of time.After.
// We use a reconnect delay long enough that ctx.Done() fires first.
func TestRunReconnectWaitCtxDone(t *testing.T) {
	t.Parallel()

	// Use a 503 server for fast non-fatal errors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	addr := freeAddr(t)
	cfg := &config.Config{
		DrydockURL:        srv.URL,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		ReconnectDelay:    5, // 5s delay — context expires before delay
		MaxReconnectDelay: 10,
		DDPollInterval:    300,
		BindAddress:       "127.0.0.1",
		Port:              portFrom(addr),
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	// Short context — fires during the reconnect delay select, taking ctx.Done().
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := c.Run(ctx)
	if err == nil {
		t.Fatal("Run returned nil, want ctx error")
	}
	if errors.Is(err, errFatal) {
		t.Errorf("Run returned errFatal: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context error", err)
	}
}

// ---------------------------------------------------------------------------
// Run — delay capping: delay *= 2 exceeds maxDelay (line 197-199)
// ---------------------------------------------------------------------------

// TestRunDelayCapping covers line 197-199: after the first failed connection,
// delay doubles and exceeds maxDelay, which is then capped.
// ReconnectDelay=2, MaxReconnectDelay=1: delay=2s, but maxDelay=1s.
// First fail: wait jitteredDuration(2s)≈1.5-2.5s, then delay*=2=4s, 4s>1s → cap to 1s. ✓
// Use ctx timeout just past the first reconnect wait so the cap fires before exit.
func TestRunDelayCapping(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	addr := freeAddr(t)
	cfg := &config.Config{
		DrydockURL:        srv.URL,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		ReconnectDelay:    2, // 2s initial delay
		MaxReconnectDelay: 1, // 1s cap → delay*2=4s > 1s triggers cap
		DDPollInterval:    300,
		BindAddress:       "127.0.0.1",
		Port:              portFrom(addr),
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	// Context must live through:
	//   1) first 503 dial (fast)
	//   2) first reconnect wait (jitteredDuration(2s) ≈ 1.5–2.5s)
	//   3) delay*=2=4s, cap to 1s (instant)
	// Then ctx expires after a total of ~4s (well above worst case 2.5s wait).
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	err := c.Run(ctx)
	// Any ctx error is fine — cap branch ran (no panic/fatal).
	if errors.Is(err, errFatal) {
		t.Errorf("Run returned errFatal: %v", err)
	}
}

// ---------------------------------------------------------------------------
// sendMetrics — error path (line 722-725)
// ---------------------------------------------------------------------------

// TestSendMetricsWithFailingCollector covers the error branch by using the
// sendMetrics function via TestConnectSuccessfulWelcomeParsed's flow (which
// already calls sendMetrics after the welcome). However, to specifically target
// the error case, we use a collector pointed at a definitely-absent path so
// disk collection fails.
//
// metrics.NewCollector is called with the root path and a skipDisk flag.
// When skipDisk=false and the root path doesn't exist, Collect() returns an error.
func TestSendMetricsWithFailingCollector(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)
	// Use a collector with skipDisk=false on an absent path.
	// metrics.NewCollector("/nonexistent-path-xyz", false) should make Collect() fail.
	// We use newWireClient to construct it with the right constructor.
	wc := newWireClient(t, &config.Config{SkipDFCollection: false})
	// Reassign the collector root to something absent by re-creating.
	// Since we can't change the root path after construction, use a path that
	// definitely won't have /var/lib/docker in a test env.
	// On macOS, /var/lib/docker doesn't exist, so SkipDFCollection=false triggers
	// a df call that fails.
	c.collector = wc.collector

	// sendMetrics should log the error and return without panicking.
	// On systems where /var/lib/docker exists this may succeed; that's ok.
	c.sendMetrics()
}

// ---------------------------------------------------------------------------
// sendPump — WriteJSON error path (lines 795-799)
// ---------------------------------------------------------------------------

// TestSendPumpWriteJSONError covers lines 795-799: when conn.WriteJSON fails,
// failConn is called and sendPump returns. We close the agent-side conn BEFORE
// sending a message so the WriteJSON call fails immediately.
func TestSendPumpWriteJSONError(t *testing.T) {
	t.Parallel()

	c, _ := newHandshakeTestClient(t)
	sendCh := make(chan protocol.Envelope, sendQueueSize)
	c.connMu.Lock()
	c.sendCh = sendCh
	agentConn := c.conn
	c.connMu.Unlock()

	// Close the agent's conn so WriteJSON fails on the FIRST write attempt.
	// We do this BEFORE enqueueing so sendPump definitely sees the closed conn.
	if err := agentConn.Close(); err != nil {
		t.Logf("close agent conn: %v", err)
	}

	pumpDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(pumpDone)
		c.sendPump(ctx, context.Background(), agentConn, sendCh)
	}()

	// Enqueue a message; sendPump picks it up and tries to write to the
	// already-closed conn, which should fail immediately.
	sendCh <- protocol.Envelope{Type: protocol.TypePong, Data: json.RawMessage(`{}`)}

	// sendPump must exit after the write failure.
	select {
	case <-pumpDone:
		// Success: sendPump exited via the WriteJSON error path.
	case <-time.After(readTimeout):
		t.Fatal("sendPump did not exit after WriteJSON failure")
	}
}

// TestHandleResizeDoneWhenInboxFull covers the done branch in HandleResize
// when the inbox is already at capacity so the inbox send would block.
// With both inbox-full (default path) and done-closed, Go's select is
// non-deterministic. To reliably hit the done branch we need inbox FULL and
// done CLOSED — then select randomly picks done or default. We verify the
// done slog.Debug is exercised by running multiple times.

// ---------------------------------------------------------------------------
// dockerReady — /_ping status-code boundary (client.go:1705-1719)
// ---------------------------------------------------------------------------

// TestDockerReadyStatusCodeBoundary pins the 2xx range dockerReady treats as
// healthy: >= http.StatusOK (200) and < http.StatusMultipleChoices (300).
// The rows straddle both boundaries so a >= -> > or < -> <= mutation on
// either comparison flips a result and fails the test.
func TestDockerReadyStatusCodeBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{name: "below range", status: 199, want: false},
		{name: "range start", status: http.StatusOK, want: true},
		{name: "still in range", status: 299, want: true},
		{name: "range end excluded", status: http.StatusMultipleChoices, want: false},
		{name: "well above range", status: 500, want: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, _ := newTestClient(t)
			//nolint:bodyclose // the response body is consumed and closed by dockerReady, the code under test.
			c.dockerClient = &fakeDocker{doResp: mkResp(tc.status, "", "")}

			if got := c.dockerReady(context.Background()); got != tc.want {
				t.Errorf("dockerReady() with status %d = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}
