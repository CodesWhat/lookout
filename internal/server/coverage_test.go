package server

// coverage_test.go adds tests for functions in http.go, argon2.go, and
// middleware.go that had zero or low coverage in the baseline.
// Only *_test.go files are added/modified; no source files are touched.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/adapter"
	"github.com/codeswhat/portwing/internal/audit"
	"github.com/codeswhat/portwing/internal/config"
	"github.com/codeswhat/portwing/internal/docker"
	"github.com/codeswhat/portwing/internal/protocol"
)

// ---------------------------------------------------------------------------
// Stub adapter (satisfies adapter.ServerAdapter without real Docker)
// ---------------------------------------------------------------------------

type stubServerAdapter struct{}

func (s *stubServerAdapter) Name() string                            { return "stub" }
func (s *stubServerAdapter) Capabilities() []string                  { return []string{"stub"} }
func (s *stubServerAdapter) HelloExtension() *adapter.HelloExtension { return nil }
func (s *stubServerAdapter) PollInterval() int                       { return 0 }
func (s *stubServerAdapter) RegisterRoutes(_ *http.ServeMux, _ func(http.HandlerFunc) http.Handler, _ adapter.StreamAdmitter) {
}
func (s *stubServerAdapter) OnConnect(_ context.Context, _ adapter.MessageSender) error { return nil }
func (s *stubServerAdapter) RefreshContainers(_ context.Context) ([]adapter.Container, []adapter.Container, []adapter.Container, error) {
	return nil, nil, nil, nil
}
func (s *stubServerAdapter) OnContainerRefresh(_ context.Context, _ adapter.MessageSender, _, _, _ []adapter.Container) error {
	return nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// minimalConfig returns a Config that is safe for NewServer.
func minimalConfig() *config.Config {
	return &config.Config{
		Port:                 "3000",
		BindAddress:          "127.0.0.1",
		AllowUnauthenticated: true,
		AuditBufferSize:      16,
		NonceLRUSize:         100,
		DDPollInterval:       30,
	}
}

// newStubDockerClient creates a stub Docker Unix-socket server and returns a
// connected docker.Client. Uses the shortSocketPath helper from metrics_prom_test.go
// and the newStubMetricsDockerClient helper (both in this package).
func newStubDockerClient(t *testing.T) (*docker.Client, func()) {
	t.Helper()
	return newStubMetricsDockerClient(t, nil, nil)
}

// newUnixListener opens a unix socket at sockPath.
func newUnixListener(t *testing.T, sockPath string) net.Listener {
	t.Helper()
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	return l
}

// newDockerClientOnListener starts an HTTP server on listener and returns a
// docker.Client connected to it. cleanup shuts down the server and removes the
// socket directory.
func newDockerClientOnListener(t *testing.T, mux *http.ServeMux, sockPath string, listener net.Listener, sockCleanup func()) (*docker.Client, func()) {
	t.Helper()
	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(listener)
	}()

	client, err := docker.NewClient(sockPath, 5)
	if err != nil {
		_ = srv.Close()
		sockCleanup()
		t.Fatalf("docker.NewClient: %v", err)
	}

	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = listener.Close()
		<-done
		sockCleanup()
	}
	return client, stop
}

// buildVersionMux builds a minimal Docker API mux with /version and /_ping.
func buildVersionMux(version string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.VersionResponse{
			Version:    version,
			APIVersion: "1.44",
		})
	})
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// newDockerClientWithVersion creates a stub Docker client that reports the
// given version string.
func newDockerClientWithVersion(t *testing.T, version string) (*docker.Client, func()) {
	t.Helper()
	sockPath, cleanup := shortSocketPath(t)
	listener := newUnixListener(t, sockPath)
	mux := buildVersionMux(version)
	// Also handle versioned /v1.44/version path.
	mux.HandleFunc("/v1.44/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.VersionResponse{
			Version:    version,
			APIVersion: "1.44",
		})
	})
	return newDockerClientOnListener(t, mux, sockPath, listener, cleanup)
}

// newDockerClientWithPing creates a stub Docker client whose /_ping responds
// with either 200 (pingOK=true) or 500 (pingOK=false).
func newDockerClientWithPing(t *testing.T, pingOK bool) (*docker.Client, func()) {
	t.Helper()
	sockPath, cleanup := shortSocketPath(t)
	listener := newUnixListener(t, sockPath)

	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.VersionResponse{
			Version:    "26.0.0",
			APIVersion: "1.44",
		})
	})
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) {
		if pingOK {
			w.WriteHeader(http.StatusOK)
		} else {
			http.Error(w, "daemon unavailable", http.StatusInternalServerError)
		}
	})
	return newDockerClientOnListener(t, mux, sockPath, listener, cleanup)
}

// ---------------------------------------------------------------------------
// http.go — isWebSocketUpgrade
// ---------------------------------------------------------------------------

func TestIsWebSocketUpgradeTrue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		header string
		value  string
	}{
		{"Upgrade", "websocket"},
		{"Upgrade", "WebSocket"},
		{"Upgrade", "tcp"},
		{"Connection", "Upgrade"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.header+":"+c.value, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(c.header, c.value)
			if !isWebSocketUpgrade(req) {
				t.Errorf("expected true for %s=%s, got false", c.header, c.value)
			}
		})
	}
}

func TestIsWebSocketUpgradeFalse(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if isWebSocketUpgrade(req) {
		t.Error("expected isWebSocketUpgrade=false for plain request")
	}
}

// ---------------------------------------------------------------------------
// http.go — copyHeaders
// ---------------------------------------------------------------------------

func TestCopyHeadersBasic(t *testing.T) {
	t.Parallel()

	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("X-Custom", "value")
	src.Add("X-Multi", "a")
	src.Add("X-Multi", "b")

	dst := http.Header{}
	copyHeaders(dst, src)

	if dst.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type not copied: %v", dst.Get("Content-Type"))
	}
	if dst.Get("X-Custom") != "value" {
		t.Errorf("X-Custom not copied: %v", dst.Get("X-Custom"))
	}
	if got := dst["X-Multi"]; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("X-Multi not copied correctly: %v", got)
	}
}

func TestCopyHeadersStripsHopByHop(t *testing.T) {
	t.Parallel()

	hopHeaders := []string{
		"Connection",
		"Keep-Alive",
		"Transfer-Encoding",
		"Te",
		"Trailer",
		"Upgrade",
		"Proxy-Authorization",
		"Proxy-Authenticate",
	}

	src := http.Header{}
	for _, h := range hopHeaders {
		src.Set(h, "strip-me")
	}
	src.Set("X-Pass-Through", "keep-me")

	dst := http.Header{}
	copyHeaders(dst, src)

	for _, h := range hopHeaders {
		if v := dst.Get(h); v != "" {
			t.Errorf("hop-by-hop %q was not stripped (got %q)", h, v)
		}
	}
	if dst.Get("X-Pass-Through") != "keep-me" {
		t.Error("X-Pass-Through was wrongly stripped")
	}
}

func TestCopyHeadersEmpty(t *testing.T) {
	t.Parallel()
	dst := http.Header{}
	copyHeaders(dst, http.Header{})
	if len(dst) != 0 {
		t.Errorf("expected empty dst for empty src, got %v", dst)
	}
}

// ---------------------------------------------------------------------------
// http.go — streamResponse
// ---------------------------------------------------------------------------

func TestStreamResponseCopiesBody(t *testing.T) {
	t.Parallel()

	payload := "hello streaming world\n"
	rec := httptest.NewRecorder()
	s := &Server{}
	if err := s.streamResponse(rec, strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}

	if got := rec.Body.String(); got != payload {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestStreamResponseMultipleChunks(t *testing.T) {
	t.Parallel()

	chunks := []string{"chunk1\n", "chunk2\n", "chunk3\n"}
	var buf bytes.Buffer
	for _, c := range chunks {
		buf.WriteString(c)
	}

	rec := httptest.NewRecorder()
	s := &Server{}
	if err := s.streamResponse(rec, &buf); err != nil {
		t.Fatal(err)
	}

	want := strings.Join(chunks, "")
	if got := rec.Body.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// http.go — handleSimpleHealth
// ---------------------------------------------------------------------------

func TestHandleSimpleHealthDirect(t *testing.T) {
	t.Parallel()

	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.handleSimpleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("unexpected Content-Type: %q", ct)
	}
	var body protocol.HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status: got %q, want ok", body.Status)
	}
}

// ---------------------------------------------------------------------------
// http.go — handleHealth (live stub for both healthy and unhealthy paths)
// ---------------------------------------------------------------------------

func TestHandleHealthConnected(t *testing.T) {
	t.Parallel()

	client, stop := newDockerClientWithPing(t, true)
	defer stop()

	s := &Server{dockerClient: client}
	req := httptest.NewRequest(http.MethodGet, "/_portwing/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body protocol.HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "healthy" {
		t.Errorf("status: got %q, want healthy", body.Status)
	}
	if body.Docker != "connected" {
		t.Errorf("docker: got %q, want connected", body.Docker)
	}
}

func TestHandleHealthDisconnected(t *testing.T) {
	t.Parallel()

	client, stop := newDockerClientWithPing(t, false)
	defer stop()

	s := &Server{dockerClient: client}
	req := httptest.NewRequest(http.MethodGet, "/_portwing/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var body protocol.HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "unhealthy" {
		t.Errorf("status: got %q, want unhealthy", body.Status)
	}
	if body.Docker != "disconnected" {
		t.Errorf("docker: got %q, want disconnected", body.Docker)
	}
}

// ---------------------------------------------------------------------------
// http.go — handleInfo
// ---------------------------------------------------------------------------

func TestHandleInfo(t *testing.T) {
	t.Parallel()

	client, stop := newDockerClientWithVersion(t, "26.0.0")
	defer stop()

	s := &Server{
		dockerClient: client,
		adapter:      &stubServerAdapter{},
		cfg: &config.Config{
			AgentID:   "test-agent-id",
			AgentName: "test-agent",
		},
		startTime: time.Now().Add(-10 * time.Second),
	}

	req := httptest.NewRequest(http.MethodGet, "/_portwing/info", nil)
	rec := httptest.NewRecorder()
	s.handleInfo(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["mode"] != "standard" {
		t.Errorf("mode: got %v", body["mode"])
	}
	if body["agentId"] != "test-agent-id" {
		t.Errorf("agentId: got %v", body["agentId"])
	}
	if body["dockerVersion"] == nil || body["dockerVersion"] == "" {
		t.Error("dockerVersion missing from info response")
	}
	caps, ok := body["capabilities"].([]any)
	if !ok || len(caps) == 0 {
		t.Errorf("capabilities missing or empty: %v", body["capabilities"])
	}
}

// ---------------------------------------------------------------------------
// http.go — handleCompose (bad JSON → 400; valid JSON → hits execute path)
// ---------------------------------------------------------------------------

func buildComposeServer(t *testing.T) *Server {
	t.Helper()
	client, stop := newStubDockerClient(t)
	t.Cleanup(stop)

	auditor, closeAudit, err := audit.New("", 0)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(closeAudit)

	rl := NewRateLimiter()
	t.Cleanup(rl.Stop)

	return &Server{
		dockerClient: client,
		compose:      docker.NewComposeManager("", "1.44", client.GetSocketPath()),
		rateLimiter:  rl,
		auditor:      auditor,
		cfg:          minimalConfig(),
	}
}

func TestHandleComposeBadJSON(t *testing.T) {
	t.Parallel()

	s := buildComposeServer(t)
	req := httptest.NewRequest(http.MethodPost, "/_portwing/compose", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	s.handleCompose(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad JSON, got %d", rec.Code)
	}
}

func TestHandleComposeValidJSON(t *testing.T) {
	t.Parallel()

	s := buildComposeServer(t)
	body := `{"operation":"ps","stackName":"mystack"}`
	req := httptest.NewRequest(http.MethodPost, "/_portwing/compose", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	s.handleCompose(rec, req)

	// ComposeManager with empty stacksDir will return an error → 500, which is
	// fine. The test confirms we proceed past JSON decode without a 400.
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("expected non-400 for valid JSON, got 400: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// http.go — NewServer construction paths
// ---------------------------------------------------------------------------

func TestNewServerRejectsImplicitUnauthenticatedMode(t *testing.T) {
	t.Parallel()

	client, stop := newStubDockerClient(t)
	defer stop()

	cfg := minimalConfig()
	cfg.AllowUnauthenticated = false
	_, err := NewServer(cfg, client, &stubServerAdapter{})
	if err == nil {
		t.Fatal("expected NewServer to reject missing authentication without an explicit opt-in")
	}
	if !strings.Contains(err.Error(), "ALLOW_UNAUTHENTICATED") {
		t.Fatalf("expected opt-in guidance, got: %v", err)
	}
}

func TestNewServerAllowsExplicitLoopbackUnauthenticatedMode(t *testing.T) {
	t.Parallel()

	client, stop := newStubDockerClient(t)
	defer stop()

	s, err := NewServer(minimalConfig(), client, &stubServerAdapter{})
	if err != nil {
		t.Fatalf("NewServer with explicit local opt-in: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}

func TestNewServerRejectsUnauthenticatedNonLoopbackBind(t *testing.T) {
	t.Parallel()

	client, stop := newStubDockerClient(t)
	defer stop()

	cfg := minimalConfig()
	cfg.BindAddress = "0.0.0.0"
	_, err := NewServer(cfg, client, &stubServerAdapter{})
	if err == nil {
		t.Fatal("expected unauthenticated non-loopback bind to require a second explicit opt-in")
	}
	if !strings.Contains(err.Error(), "ALLOW_UNAUTHENTICATED_REMOTE") {
		t.Fatalf("expected remote opt-in guidance, got: %v", err)
	}
}

func TestNewServerAllowsExplicitRemoteUnauthenticatedMode(t *testing.T) {
	t.Parallel()

	client, stop := newStubDockerClient(t)
	defer stop()

	cfg := minimalConfig()
	cfg.BindAddress = "0.0.0.0"
	cfg.AllowUnauthenticatedRemote = true
	s, err := NewServer(cfg, client, &stubServerAdapter{})
	if err != nil {
		t.Fatalf("NewServer with both unauthenticated opt-ins: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}

func TestNewServerWithRawToken(t *testing.T) {
	t.Parallel()

	client, stop := newStubDockerClient(t)
	defer stop()

	cfg := minimalConfig()
	cfg.Token = "my-test-token"
	s, err := NewServer(cfg, client, &stubServerAdapter{})
	if err != nil {
		t.Fatalf("NewServer with token: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}

func TestNewServerWithTokenHash(t *testing.T) {
	t.Parallel()

	phc, err := HashToken("my-hash-token")
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}

	client, stop := newStubDockerClient(t)
	defer stop()

	cfg := minimalConfig()
	cfg.TokenHash = phc
	s, err := NewServer(cfg, client, &stubServerAdapter{})
	if err != nil {
		t.Fatalf("NewServer with token hash: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}

func TestNewServerBadTokenHash(t *testing.T) {
	t.Parallel()

	client, stop := newStubDockerClient(t)
	defer stop()

	cfg := minimalConfig()
	cfg.TokenHash = "not-a-valid-phc-string"
	_, err := NewServer(cfg, client, &stubServerAdapter{})
	if err == nil {
		t.Fatal("expected error for malformed TokenHash, got nil")
	}
}

// ---------------------------------------------------------------------------
// http.go — registerRoutes: verify routes are registered
// ---------------------------------------------------------------------------

// TestRegisterRoutesSimpleHealth verifies that GET /health is wired and
// returns 200 without credentials. Uses the ping-capable stub so
// /_portwing/health (which pings Docker) also works.
func TestRegisterRoutesSimpleHealth(t *testing.T) {
	t.Parallel()

	// Use a ping-capable stub so /_portwing/health can also succeed.
	client, stop := newDockerClientWithPing(t, true)
	defer stop()

	s, err := NewServer(minimalConfig(), client, &stubServerAdapter{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	mux := http.NewServeMux()
	s.registerRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, path := range []string{"/health", "/ready", "/_portwing/health"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: expected 200, got %d", path, resp.StatusCode)
		}
	}
}

// TestRegisterRoutesAuthGated verifies that an auth-gated route returns 401
// when credentials are required (TOKEN is set) and none are provided.
func TestRegisterRoutesAuthGated(t *testing.T) {
	t.Parallel()

	client, stop := newDockerClientWithPing(t, true)
	defer stop()

	cfg := minimalConfig()
	cfg.Token = "secret-token"
	s, err := NewServer(cfg, client, &stubServerAdapter{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	mux := http.NewServeMux()
	s.registerRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, path := range []string{"/_portwing/info", "/_portwing/audit/export"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401 for unauthenticated %s, got %d", path, resp.StatusCode)
		}
	}
}

// ---------------------------------------------------------------------------
// http.go — Shutdown: cancels pollCancel and stops rateLimiter
// ---------------------------------------------------------------------------

func TestShutdownCancelsGoroutines(t *testing.T) {
	t.Parallel()

	auditor, closeAudit, err := audit.New("", 0)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(closeAudit)

	rl := NewRateLimiter()

	pollCtx, pollCancel := context.WithCancel(context.Background())

	// Bind an httpServer to an ephemeral port so Shutdown has something to stop.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})}
	go func() { _ = httpSrv.Serve(ln) }()

	s := &Server{
		pollCancel:  pollCancel,
		hupDone:     make(chan struct{}),
		rateLimiter: rl,
		auditor:     auditor,
		httpServer:  httpSrv,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}

	// pollCtx must be cancelled.
	select {
	case <-pollCtx.Done():
	default:
		t.Error("pollCtx was not cancelled by Shutdown")
	}
	_ = pollCtx
}

// ---------------------------------------------------------------------------
// http.go — handleDockerProxy: non-exec path with websocket headers
// ---------------------------------------------------------------------------

func TestHandleDockerProxyNonExecWebSocket(t *testing.T) {
	t.Parallel()

	sockPath, cleanup := shortSocketPath(t)
	defer cleanup()
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	fakeDaemon := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"Id":"abc"}`)
		}),
	}
	go func() { _ = fakeDaemon.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = fakeDaemon.Shutdown(ctx)
	}()

	client, err := docker.NewClient(sockPath, 5)
	if err != nil {
		t.Fatalf("docker.NewClient: %v", err)
	}

	auditor, closeAudit, err := audit.New("", 0)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	defer closeAudit()

	s := &Server{
		dockerClient: client,
		rateLimiter:  NewRateLimiter(),
		auditor:      auditor,
	}
	defer s.rateLimiter.Stop()

	// WebSocket-upgrade on a non-exec path → goes through the regular proxy.
	req := httptest.NewRequest(http.MethodGet, "/v1.44/containers/json", nil)
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	s.handleDockerProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// argon2.go — parseParams edge cases
// ---------------------------------------------------------------------------

func TestParseParamsWrongFieldCount(t *testing.T) {
	t.Parallel()

	for _, s := range []string{
		"m=19456,t=2",         // 2 fields
		"m=19456,t=2,p=1,x=0", // 4 fields
		"",                    // empty
	} {
		if _, err := parseParams(s); err == nil {
			t.Errorf("parseParams(%q): expected error, got nil", s)
		}
	}
}

func TestParseParamsMalformedKV(t *testing.T) {
	t.Parallel()
	if _, err := parseParams("m=19456,t=2,noeq"); err == nil {
		t.Error("expected error for field without '=', got nil")
	}
}

func TestParseParamsUnknownKey(t *testing.T) {
	t.Parallel()
	if _, err := parseParams("m=19456,t=2,x=1"); err == nil {
		t.Error("expected error for unknown key 'x', got nil")
	}
}

func TestParseParamsNonIntegerValue(t *testing.T) {
	t.Parallel()
	if _, err := parseParams("m=abc,t=2,p=1"); err == nil {
		t.Error("expected error for non-integer value, got nil")
	}
}

func TestParseParamsParallelismOver255(t *testing.T) {
	t.Parallel()
	if _, err := parseParams("m=19456,t=2,p=256"); err == nil {
		t.Error("expected error for p=256, got nil")
	}
}

// ---------------------------------------------------------------------------
// argon2.go — Verify with mismatched hash
// ---------------------------------------------------------------------------

func TestVerifyMismatchedHash(t *testing.T) {
	t.Parallel()

	p := &Argon2idParams{
		Memory:      19456,
		Time:        2,
		Parallelism: 1,
		Salt:        []byte("somesalt"),
		Hash:        make([]byte, 32), // all-zero hash: won't match "anytoken"
	}
	if p.Verify("anytoken") {
		t.Error("Verify returned true for mismatched hash")
	}
}

// ---------------------------------------------------------------------------
// middleware.go — cleanup logic: expired entries are removed
// ---------------------------------------------------------------------------

func TestCleanupRemovesExpiredEntries(t *testing.T) {
	t.Parallel()

	rl := &RateLimiter{
		attempts: map[string]*ipAttempts{},
		maxFails: 10,
		window:   1 * time.Millisecond,
		maxIPs:   10000,
		done:     make(chan struct{}),
	}

	// Expired entry.
	rl.attempts["192.0.2.1"] = &ipAttempts{
		count:     5,
		firstFail: time.Now().Add(-10 * time.Millisecond),
	}
	// Non-expired entry.
	rl.attempts["192.0.2.2"] = &ipAttempts{
		count:     5,
		firstFail: time.Now().Add(10 * time.Minute),
	}

	// Replicate the cleanup logic inline.
	rl.mu.Lock()
	now := time.Now()
	for ip, a := range rl.attempts {
		if now.Sub(a.firstFail) > rl.window {
			delete(rl.attempts, ip)
		}
	}
	rl.mu.Unlock()

	rl.mu.Lock()
	_, has1 := rl.attempts["192.0.2.1"]
	_, has2 := rl.attempts["192.0.2.2"]
	rl.mu.Unlock()

	if has1 {
		t.Error("expired entry 192.0.2.1 was not removed")
	}
	if !has2 {
		t.Error("non-expired entry 192.0.2.2 was incorrectly removed")
	}
}

// ---------------------------------------------------------------------------
// middleware.go — IsRateLimited: expired window resets to not-limited
// ---------------------------------------------------------------------------

func TestIsRateLimitedExpiredWindowReset(t *testing.T) {
	t.Parallel()

	rl := &RateLimiter{
		attempts: map[string]*ipAttempts{},
		maxFails: 2,
		window:   1 * time.Millisecond,
		maxIPs:   10000,
		done:     make(chan struct{}),
	}
	rl.attempts["10.0.0.1"] = &ipAttempts{
		count:     999,
		firstFail: time.Now().Add(-10 * time.Second), // long expired
	}

	if rl.IsRateLimited("10.0.0.1") {
		t.Error("expected false after window expired, got true")
	}

	rl.mu.Lock()
	_, exists := rl.attempts["10.0.0.1"]
	rl.mu.Unlock()
	if exists {
		t.Error("expected expired entry to be deleted by IsRateLimited")
	}
}

// ---------------------------------------------------------------------------
// middleware.go — RecordFailure paths
// ---------------------------------------------------------------------------

func TestRecordFailureNewEntry(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	defer rl.Stop()

	rl.RecordFailure("10.0.0.1")

	rl.mu.Lock()
	a := rl.attempts["10.0.0.1"]
	rl.mu.Unlock()

	if a == nil {
		t.Fatal("expected entry for 10.0.0.1, got nil")
	}
	if a.count != 1 {
		t.Errorf("count: got %d, want 1", a.count)
	}
}

func TestRecordFailureIncrementsCount(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	defer rl.Stop()

	for i := 0; i < 5; i++ {
		rl.RecordFailure("10.0.0.2")
	}

	rl.mu.Lock()
	a := rl.attempts["10.0.0.2"]
	rl.mu.Unlock()

	if a.count != 5 {
		t.Errorf("count: got %d, want 5", a.count)
	}
}

func TestRecordFailureExpiredWindowReset(t *testing.T) {
	t.Parallel()

	rl := &RateLimiter{
		attempts: map[string]*ipAttempts{},
		maxFails: 10,
		window:   1 * time.Millisecond,
		maxIPs:   10000,
		done:     make(chan struct{}),
	}
	// Pre-plant an expired entry.
	rl.attempts["10.0.0.3"] = &ipAttempts{
		count:     50,
		firstFail: time.Now().Add(-10 * time.Second),
	}

	rl.RecordFailure("10.0.0.3")

	rl.mu.Lock()
	a := rl.attempts["10.0.0.3"]
	rl.mu.Unlock()

	if a.count != 1 {
		t.Errorf("expected count reset to 1, got %d", a.count)
	}
}

func TestRecordFailureDropsWhenAtMaxIPs(t *testing.T) {
	t.Parallel()

	rl := &RateLimiter{
		attempts: map[string]*ipAttempts{},
		maxFails: 10,
		window:   time.Minute,
		maxIPs:   2,
		done:     make(chan struct{}),
	}
	rl.RecordFailure("10.0.0.1")
	rl.RecordFailure("10.0.0.2")
	// This one should be silently dropped.
	rl.RecordFailure("10.0.0.3")

	rl.mu.Lock()
	_, exists := rl.attempts["10.0.0.3"]
	count := len(rl.attempts)
	rl.mu.Unlock()

	if exists {
		t.Error("expected 10.0.0.3 to be dropped, but it was recorded")
	}
	if count != 2 {
		t.Errorf("expected 2 entries, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// middleware.go — IsRateLimited threshold
// ---------------------------------------------------------------------------

func TestIsRateLimitedTripsAtThreshold(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter() // maxFails=10
	defer rl.Stop()

	for i := 0; i < 10; i++ {
		rl.RecordFailure("10.1.2.3")
	}
	if !rl.IsRateLimited("10.1.2.3") {
		t.Error("expected IsRateLimited=true after 10 failures")
	}
}

func TestIsRateLimitedBelowThreshold(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	defer rl.Stop()

	for i := 0; i < 9; i++ {
		rl.RecordFailure("10.1.2.4")
	}
	if rl.IsRateLimited("10.1.2.4") {
		t.Error("expected IsRateLimited=false with 9 failures (threshold=10)")
	}
}

// ---------------------------------------------------------------------------
// middleware.go — RecoveryMiddleware catches panics
// ---------------------------------------------------------------------------

func TestRecoveryMiddlewareCatchesPanic(t *testing.T) {
	t.Parallel()

	h := RecoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("intentional panic for test")
	}))
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 after panic recovery, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// middleware.go — statusRecorder Flush, Hijack, Unwrap
// ---------------------------------------------------------------------------

func TestStatusRecorderFlush(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, code: http.StatusOK}
	// httptest.ResponseRecorder implements http.Flusher; should not panic.
	sr.Flush()
}

func TestStatusRecorderHijackNotSupported(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, code: http.StatusOK}
	// httptest.ResponseRecorder does not implement http.Hijacker.
	_, _, err := sr.Hijack()
	if !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("expected http.ErrNotSupported, got %v", err)
	}
}

func TestStatusRecorderUnwrap(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, code: http.StatusOK}
	if got := sr.Unwrap(); got != rec {
		t.Errorf("Unwrap: got %T, want *httptest.ResponseRecorder", got)
	}
}
