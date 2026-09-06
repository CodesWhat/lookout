package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/codeswhat/portwing/internal/adapter"
	"github.com/codeswhat/portwing/internal/audit"
	"github.com/codeswhat/portwing/internal/auth"
	"github.com/codeswhat/portwing/internal/config"
	"github.com/codeswhat/portwing/internal/docker"
	"github.com/codeswhat/portwing/internal/mcp"
	"github.com/codeswhat/portwing/internal/metrics"
	"github.com/codeswhat/portwing/internal/protocol"
)

// hopByHopHeaders are headers that must not be forwarded by proxies.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Transfer-Encoding":   true,
	"Te":                  true,
	"Trailer":             true,
	"Upgrade":             true,
	"Proxy-Authorization": true,
	"Proxy-Authenticate":  true,
}

// portwingAuthHeaders authenticate the client to Portwing itself. They must never
// be forwarded to the Docker daemon (or the sockguard proxy sitting in front of
// it) — doing so leaks Portwing's own credentials downstream. http.Header.Del
// canonicalises the key, so current and legacy auth headers match correctly.
var portwingAuthHeaders = []string{
	"Authorization",
	"X-Portwing-Token",
	"X-Dd-Agent-Secret",
	auth.HeaderKeyID,
	auth.HeaderTimestamp,
	auth.HeaderNonce,
	auth.HeaderSignature,
	auth.HeaderSignatureVersion,
}

// stripPortwingAuthHeaders removes Portwing's own auth headers from a request
// bound for the Docker daemon.
func stripPortwingAuthHeaders(h http.Header) {
	for _, name := range portwingAuthHeaders {
		h.Del(name)
	}
}

// maxHijackBodyBytes caps the exec-start or attach request body read during a
// hijack so a hostile client can't force an unbounded in-memory read. Matches
// the "exec body (10 MB)" limit documented in SECURITY.md.
const maxHijackBodyBytes = 10 * 1024 * 1024 // 10 MB

// concurrencyLimiter admits a bounded number of concurrent sessions, mirroring
// edge mode's maxStreams/maxExecSessions semaphores (internal/edge/client.go).
//
// A nil limiter is unbounded. Servers assembled as struct literals rather than
// through NewServer never allocate one, so failing closed on nil would reject
// every stream those servers proxy.
type concurrencyLimiter struct {
	slots chan struct{}
}

// newConcurrencyLimiter returns nil for a non-positive limit: an operator whose
// controller legitimately runs more concurrent streams than the default needs a
// way to turn the bound off.
func newConcurrencyLimiter(limit int) *concurrencyLimiter {
	if limit <= 0 {
		return nil
	}
	return &concurrencyLimiter{slots: make(chan struct{}, limit)}
}

// acquire takes a slot without blocking and reports whether one was free. The
// caller rejects rather than queues, so a saturated agent answers immediately
// instead of holding the connection open.
func (l *concurrencyLimiter) acquire() bool {
	if l == nil {
		return true
	}
	select {
	case l.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns a slot taken by a successful acquire. Calls are paired with a
// defer at every acquire site.
func (l *concurrencyLimiter) release() {
	if l == nil {
		return
	}
	<-l.slots
}

func (l *concurrencyLimiter) limit() int {
	if l == nil {
		return 0
	}
	return cap(l.slots)
}

// Server is the standard-mode HTTP server that exposes Docker API proxy
// endpoints, adapter-specific routes, and health checks.
type Server struct {
	cfg          *config.Config
	dockerClient *docker.Client
	dockerDialer func(network, address string) (net.Conn, error)
	adapter      adapter.ServerAdapter
	compose      *docker.ComposeManager
	collector    *metrics.Collector
	metrics      *metrics.Registry
	rateLimiter  *RateLimiter
	verifier     tokenVerifier
	ed25519      Ed25519Config
	enroller     *auth.Enroller
	auditor      *audit.Logger
	httpServer   *http.Server
	startTime    time.Time

	// listenAddr holds the net.Addr ListenAndServe bound, set once the
	// listener is up and before Serve/ServeTLS starts blocking. It lets
	// callers — chiefly tests using an OS-assigned port ("0") — discover
	// the real bound address instead of guessing or racing a separate bind.
	listenAddr atomic.Value

	// streamSem bounds concurrent streaming proxy responses; execSem bounds
	// concurrent hijacked exec/attach sessions. Both are nil when unbounded.
	streamSem *concurrencyLimiter
	execSem   *concurrencyLimiter

	// pollCtx bounds the pollContainers goroutine started by ListenAndServe;
	// pollCancel stops it on Shutdown. Both are set at construction so a
	// Shutdown racing server startup never sees a half-written field.
	pollCtx    context.Context
	pollCancel context.CancelFunc
	// hupDone is closed by Shutdown to stop the SIGHUP goroutine.
	hupDone chan struct{}
	// hupCh is the signal channel registered for SIGHUP; kept so Shutdown
	// can call signal.Stop on it.
	hupCh chan os.Signal

	shutdownOnce   sync.Once
	auditCloseOnce sync.Once
	handlerWG      sync.WaitGroup
	handlerWait    sync.Once
	handlerDone    chan struct{}
	hijackMu       sync.Mutex
	hijackConns    map[net.Conn]struct{}
	shuttingDown   bool
}

// NewServer creates and configures a new standard-mode Server.
// It returns an error if the TokenHash is set but cannot be parsed; the PHC
// string is validated at startup so malformed configuration is caught early.
func NewServer(cfg *config.Config, dockerClient *docker.Client, a adapter.ServerAdapter) (*Server, error) {
	var verifier tokenVerifier
	switch {
	case cfg.Token != "":
		verifier = newRawTokenVerifier(cfg.Token)
	case cfg.TokenHash != "":
		params, err := ParsePHC(cfg.TokenHash)
		if err != nil {
			return nil, fmt.Errorf("parsing TOKEN_HASH: %w", err)
		}
		verifier = newArgon2Verifier(params)
	}
	// verifier == nil means no auth configured.

	auditor, _, err := audit.New(cfg.AuditLog, cfg.AuditBufferSize)
	if err != nil {
		return nil, fmt.Errorf("opening audit log: %w", err)
	}

	// Set up Ed25519 key registry if configured.
	var ed25519Cfg Ed25519Config
	if cfg.AuthorizedKeysFile != "" {
		reg := auth.NewKeyRegistry(cfg.AuthorizedKeysFile)
		if err := reg.Load(); err != nil {
			return nil, fmt.Errorf("loading authorized_keys: %w", err)
		}
		ed25519Cfg = Ed25519Config{
			Registry:       reg,
			Nonces:         auth.NewNonceLRU(cfg.NonceLRUSize, cfg.MaxClockSkewSeconds),
			MaxSkewSeconds: cfg.MaxClockSkewSeconds,
		}
	}

	// Missing authentication fails closed because the catch-all route proxies
	// the full Docker API. Local development requires an explicit opt-in.
	if verifier == nil && ed25519Cfg.Registry == nil {
		if !cfg.AllowUnauthenticated {
			return nil, fmt.Errorf("no authentication configured: set TOKEN, TOKEN_HASH, or AUTHORIZED_KEYS; for local development only, set ALLOW_UNAUTHENTICATED=true")
		}
		if !config.IsLoopbackBind(cfg.BindAddress) && !cfg.AllowUnauthenticatedRemote {
			return nil, fmt.Errorf("refusing unauthenticated non-loopback bind %q: set authentication, bind to loopback, or additionally set ALLOW_UNAUTHENTICATED_REMOTE=true", cfg.BindAddress)
		}
		slog.Warn("no authentication configured: all requests will be accepted without credentials — set TOKEN, TOKEN_HASH, or AUTHORIZED_KEYS")
	}

	// Set up enrollment handler if ENROLLMENT_TOKEN is configured.
	var enroller *auth.Enroller
	if cfg.EnrollmentToken != "" {
		if ed25519Cfg.Registry == nil {
			return nil, fmt.Errorf("ENROLLMENT_TOKEN requires AUTHORIZED_KEYS to be set")
		}
		enroller = auth.NewEnroller(cfg.EnrollmentToken, cfg.AuthorizedKeysFile, ed25519Cfg.Registry)
		enroller.OnResult = auditor.Enrollment
	}

	s := &Server{
		cfg:          cfg,
		dockerClient: dockerClient,
		adapter:      a,
		compose:      docker.NewComposeManager(cfg.StacksDir, dockerClient.GetAPIVersion(), cfg.DockerSocket),
		collector:    metrics.NewDaemonCollector(dockerClient, cfg.SkipDFCollection),
		metrics:      metrics.NewRegistry(),
		rateLimiter:  NewRateLimiter(),
		verifier:     verifier,
		ed25519:      ed25519Cfg,
		enroller:     enroller,
		auditor:      auditor,
		startTime:    time.Now(),
		hupDone:      make(chan struct{}),
		handlerDone:  make(chan struct{}),
		hijackConns:  make(map[net.Conn]struct{}),
		streamSem:    newConcurrencyLimiter(cfg.MaxStreamSessions),
		execSem:      newConcurrencyLimiter(cfg.MaxExecSessions),
	}
	s.pollCtx, s.pollCancel = context.WithCancel(context.Background())

	// Reload authorized_keys on SIGHUP so keys can be rotated or revoked
	// without a restart. The nonce LRU is preserved across reloads.
	if ed25519Cfg.Registry != nil {
		reg := ed25519Cfg.Registry
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		s.hupCh = hup
		go func() {
			for {
				select {
				case <-s.hupDone:
					return
				case _, ok := <-hup:
					if !ok {
						return
					}
					if err := reg.Load(); err != nil {
						slog.Error("SIGHUP: authorized_keys reload failed", "error", err)
						continue
					}
					slog.Info("SIGHUP: authorized_keys reloaded", "keys", reg.Len())
				}
			}
		}()
	}

	if len(cfg.TrustedProxies) > 0 {
		nets, err := ParseTrustedProxies(cfg.TrustedProxies)
		if err != nil {
			return nil, fmt.Errorf("parsing TRUSTED_PROXIES: %w", err)
		}
		s.rateLimiter.SetTrustedProxies(nets)
	}
	if s.enroller != nil {
		s.enroller.ActorResolver = s.rateLimiter.clientIP
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	handler := RecoveryMiddleware(http.Handler(mux))

	s.httpServer = &http.Server{
		Addr:    cfg.BindAddress + ":" + cfg.Port,
		Handler: s.trackActiveHandler(handler),
		// Bound the request-header read to mitigate slow-header (Slowloris)
		// attacks. ReadTimeout/WriteTimeout are deliberately left zero so the
		// streaming endpoints (logs, events, stats, exec) are not cut off;
		// ReadHeaderTimeout covers only the header phase, IdleTimeout reaps
		// idle keep-alive connections.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// BaseContext runs once, right after the listener binds, which is
		// the only hook stdlib gives us to learn the bound address without
		// owning the listener ourselves (see ListenAndServe below).
		BaseContext: func(ln net.Listener) context.Context {
			s.listenAddr.Store(ln.Addr())
			return context.Background()
		},
	}

	// Configure TLS if certs provided.
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		s.httpServer.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			CurvePreferences: []tls.CurveID{
				tls.X25519,
				tls.CurveP256,
			},
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			},
		}
	}

	return s, nil
}

// registerRoutes wires up all HTTP endpoints. Routes requiring authentication
// are wrapped with the auth middleware.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	// No auth required.
	mux.HandleFunc("GET /_portwing/health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleHealth)
	mux.HandleFunc("GET /health", s.handleSimpleHealth)

	// Enrollment endpoint: reachable WITHOUT auth (it IS the bootstrap), but
	// rate-limited. Registered only when ENROLLMENT_TOKEN is configured.
	//
	// Registered WITHOUT a method prefix so this exact-path pattern also
	// catches non-POST methods: the bare pattern outranks the "/" catch-all
	// in ServeMux precedence (exact path beats subtree wildcard), so a
	// wrong-method request lands here instead of falling through to the
	// Docker proxy. Enroller.ServeHTTP already replies 405 for non-POST.
	if s.enroller != nil {
		enrollHandler := s.rateLimiter.rateLimitOnly(s.enroller, s.metrics)
		mux.Handle("/api/portwing/enroll", enrollHandler)
	}

	// Auth required - wrap with audit-aware auth middleware (with Ed25519 support).
	authWrap := func(h http.HandlerFunc) http.Handler {
		return s.rateLimiter.AuthMiddlewareWithEd25519(s.verifier, s.ed25519, s.auditor, s.metrics, http.HandlerFunc(h))
	}

	mux.Handle("GET /_portwing/info", authWrap(s.handleInfo))
	mux.Handle("POST /_portwing/compose", authWrap(s.handleCompose))
	mux.Handle("GET /_portwing/metrics", authWrap(s.handleMetrics))
	mux.Handle("GET /metrics", authWrap(s.handleMetrics))
	mux.Handle("GET /_portwing/audit", authWrap(s.handleAudit))
	mux.Handle("GET /_portwing/audit/export", authWrap(s.handleAuditExport))
	mcpHandler := authWrap(func(w http.ResponseWriter, r *http.Request) {
		mcp.NewHandler(s.dockerClient, s.collector).ServeHTTP(w, r)
	})
	// Registered WITHOUT a method prefix (see the enroll route above for
	// why): mcp.Handler.ServeHTTP already dispatches POST to the JSON-RPC
	// handler and replies 405 for every other method, but that method
	// switch was dead code while only "POST /_portwing/mcp" was registered,
	// letting the "/" catch-all serve wrong-method requests instead.
	mux.Handle("/_portwing/mcp", mcpHandler)

	// Adapter-specific routes. Long-lived adapter streams (SSE, follow-mode
	// log tails) share s.streamSem with the Docker-proxy streaming path
	// (SPEC 7.3) rather than getting their own limiter: Go 1.22+ ServeMux
	// dispatches each request to exactly one handler, so an adapter route
	// never also reaches handleDockerProxy and a single request can never be
	// admitted twice.
	admitStream := adapter.StreamAdmitter(func() (func(), bool) {
		if !s.streamSem.acquire() {
			return nil, false
		}
		return s.streamSem.release, true
	})
	s.adapter.RegisterRoutes(mux, authWrap, admitStream)

	// Docker API proxy - catch-all (must be last).
	mux.Handle("/", authWrap(s.handleDockerProxy))
}

// handleHealth returns readiness including Docker connectivity. It is exposed
// at both the compatibility path /_portwing/health and the explicit /ready.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	err := s.dockerClient.Ping(ctx)

	status := "healthy"
	dockerStatus := "connected"
	httpStatus := http.StatusOK
	if err != nil {
		status = "unhealthy"
		dockerStatus = "disconnected"
		httpStatus = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(protocol.HealthResponse{
		Status:        status,
		Live:          true,
		Ready:         err == nil,
		Mode:          "standard",
		Version:       protocol.AgentVersion,
		UptimeSeconds: elapsedSeconds(s.startTime),
		Docker:        dockerStatus,
		Controller:    "not_applicable",
	})
}

// handleSimpleHealth reports process liveness without probing dependencies.
func (s *Server) handleSimpleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(protocol.HealthResponse{
		Status:        "ok",
		Live:          true,
		Ready:         false,
		Mode:          "standard",
		Version:       protocol.AgentVersion,
		UptimeSeconds: elapsedSeconds(s.startTime),
		Docker:        "unknown",
		Controller:    "not_applicable",
	})
}

func elapsedSeconds(start time.Time) float64 {
	if start.IsZero() {
		return 0
	}
	return time.Since(start).Seconds()
}

// handleInfo returns agent metadata.
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	dockerVersion, err := s.dockerClient.GetVersion(ctx)
	if err != nil {
		dockerVersion = "unknown"
	}

	hostname, _ := os.Hostname()

	capabilities := []string{
		"compose",
		"exec",
		"metrics",
		"events",
	}
	capabilities = append(capabilities, s.adapter.Capabilities()...)

	info := map[string]any{
		"version":       protocol.AgentVersion,
		"dockerVersion": dockerVersion,
		"mode":          "standard",
		"uptime":        time.Since(s.startTime).String(),
		"hostname":      hostname,
		"agentId":       s.cfg.AgentID,
		"agentName":     s.cfg.AgentName,
		"capabilities":  capabilities,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

// handleCompose dispatches Docker Compose operations.
func (s *Server) handleCompose(w http.ResponseWriter, r *http.Request) {
	actor := s.rateLimiter.clientIP(r)

	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	var req docker.ComposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	resp, err := s.compose.Execute(r.Context(), req)
	if err != nil {
		s.auditor.ComposeOp(actor, req.Operation, req.StackName, audit.OutcomeError)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	outcome := audit.OutcomeAllowed
	if !resp.Success {
		outcome = audit.OutcomeError
	}
	s.auditor.ComposeOp(actor, req.Operation, req.StackName, outcome)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleDockerProxy is the transparent Docker API proxy. It forwards requests
// to the local Docker daemon, handling both regular and streaming responses.
func (s *Server) handleDockerProxy(w http.ResponseWriter, r *http.Request) {
	// Determine if this is a streaming endpoint.
	isStream := docker.IsStreamingRequest(r.Method, r.URL.Path)

	// Docker exec and attach upgrade requests need a bidirectional raw
	// connection. The regular HTTP transport cannot proxy the upgraded stream.
	if isDockerHijackPath(r.URL.Path) && isWebSocketUpgrade(r) {
		s.handleDockerHijack(w, r)
		return
	}

	// Bound concurrent streams (SPEC 7.3). Checked after the hijack branch so
	// an upgraded exec takes an exec slot rather than one of each, and only for
	// streaming paths so a short proxied request is never turned away.
	if isStream {
		if !s.streamSem.acquire() {
			slog.Warn("concurrent stream limit reached, rejecting", "max", s.streamSem.limit())
			http.Error(w, "agent busy: too many concurrent streams", http.StatusServiceUnavailable)
			return
		}
		defer s.streamSem.release()
	}

	// Build Docker API request.
	dockerURL := fmt.Sprintf("http://localhost%s", r.URL.RequestURI())
	// #nosec G704 -- URL is fixed to localhost for the Docker socket proxy; RequestURI only selects the Docker API path/query.
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, dockerURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy headers (strip hop-by-hop), then strip Portwing's own auth headers
	// so they are never forwarded to the Docker socket.
	copyHeaders(proxyReq.Header, r.Header)
	stripPortwingAuthHeaders(proxyReq.Header)

	var resp *http.Response
	if isStream {
		resp, err = s.dockerClient.DoStreamRaw(proxyReq)
	} else {
		resp, err = s.dockerClient.DoRaw(proxyReq)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers.
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Stream or copy body.
	if isStream {
		s.streamResponse(w, resp.Body)
	} else {
		// io.Copy to a ResponseWriter: errors indicate a dropped client connection.
		_, _ = io.Copy(w, resp.Body)
	}
}

// handleExecHijack retains the exec-specific entry point used by focused tests.
// Actual routing for exec and attach upgrades goes through handleDockerHijack.
func (s *Server) handleExecHijack(w http.ResponseWriter, r *http.Request) {
	s.handleDockerHijack(w, r)
}

// handleDockerHijack proxies an upgraded Docker exec or attach request over a
// raw Unix socket connection. Bytes already buffered by net/http after the
// request are relayed through clientBuf once the upgrade succeeds.
func (s *Server) handleDockerHijack(w http.ResponseWriter, r *http.Request) {
	if isExecStartPath(r.URL.Path) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		execID := parts[len(parts)-2]
		s.auditor.ExecStart(s.rateLimiter.clientIP(r), r.URL.Path, execID)
	}

	// Bound concurrent exec/attach sessions (SPEC 7.3) before the body read and
	// the hijack, so a rejected session costs nothing and can still be answered
	// with a normal HTTP response.
	if !s.execSem.acquire() {
		slog.Warn("exec session limit reached, rejecting", "max", s.execSem.limit())
		http.Error(w, "agent busy: exec session limit reached", http.StatusServiceUnavailable)
		return
	}
	defer s.execSem.release()

	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, maxHijackBodyBytes+1))
		if err != nil {
			http.Error(w, fmt.Sprintf("reading upgrade request body: %v", err), http.StatusBadRequest)
			return
		}
	}
	if len(body) > maxHijackBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("hijack failed: %v", err), http.StatusInternalServerError)
		return
	}
	if !s.trackHijackedConnection(clientConn) {
		return
	}
	defer s.untrackHijackedConnection(clientConn)

	var dockerConn net.Conn
	var closeOnce sync.Once
	closeConnections := func() {
		closeOnce.Do(func() {
			_ = clientConn.Close()
			if dockerConn != nil {
				_ = dockerConn.Close()
			}
		})
	}
	defer closeConnections()

	// Connect to Docker daemon.
	dialer := s.dockerDialer
	if dialer == nil {
		dialer = net.Dial
	}
	dockerConn, err = dialer("unix", s.dockerClient.GetSocketPath())
	if err != nil {
		// Best-effort 502 write; client may have already gone.
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	if !s.trackHijackedConnection(dockerConn) {
		return
	}
	defer s.untrackHijackedConnection(dockerConn)

	// Rebuild the request so Portwing credentials and unrelated hop-by-hop
	// headers cannot reach Docker. Preserve the requested upgrade protocol and
	// end-to-end Docker headers, consume Expect after buffering the body, and
	// derive Content-Length from the bounded body read above.
	dockerURL := fmt.Sprintf("http://localhost%s", r.URL.RequestURI())
	// #nosec G704 -- URL is fixed to localhost for the Docker socket proxy; RequestURI only selects the Docker API path/query.
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, dockerURL, bytes.NewReader(body))
	if err != nil {
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	copyHeaders(proxyReq.Header, r.Header)
	stripPortwingAuthHeaders(proxyReq.Header)
	proxyReq.Header.Del("Content-Length")
	proxyReq.Header.Del("Expect")
	proxyReq.Header.Set("Connection", "Upgrade")
	upgrade := r.Header.Get("Upgrade")
	if upgrade == "" {
		upgrade = "tcp"
	}
	proxyReq.Header.Set("Upgrade", upgrade)

	if err := proxyReq.Write(dockerConn); err != nil {
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Read Docker response.
	dockerBuf := bufio.NewReader(dockerConn)
	resp, err := http.ReadResponse(dockerBuf, proxyReq)
	if err != nil {
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Forward the response status to the client.
	if err := resp.Write(clientConn); err != nil {
		return
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		return
	}

	// Bidirectional proxy; io.Copy errors just mean one side closed.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if _, err := io.Copy(dockerConn, clientBuf); err != nil {
			closeConnections()
			return
		}
		if closeWriter, ok := dockerConn.(interface{ CloseWrite() error }); ok {
			if err := closeWriter.CloseWrite(); err != nil {
				closeConnections()
			}
			return
		}
		closeConnections()
	}()

	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, dockerBuf)
		closeConnections()
	}()

	wg.Wait()
}

func isDockerHijackPath(path string) bool {
	return isExecStartPath(path) || isContainerAttachPath(path)
}

func isExecStartPath(path string) bool {
	return isDockerResourceAction(path, "exec", "start")
}

func isContainerAttachPath(path string) bool {
	return isDockerResourceAction(path, "containers", "attach")
}

func isDockerResourceAction(path, resource, action string) bool {
	if path == "" || path[0] != '/' || strings.HasSuffix(path, "/") {
		return false
	}
	parts := strings.Split(path[1:], "/")
	switch len(parts) {
	case 3:
	case 4:
		if !isDockerAPIVersion(parts[0]) {
			return false
		}
		parts = parts[1:]
	default:
		return false
	}
	return parts[0] == resource && parts[1] != "" && parts[2] == action
}

func isDockerAPIVersion(segment string) bool {
	version, ok := strings.CutPrefix(segment, "v")
	if !ok {
		return false
	}
	major, minor, ok := strings.Cut(version, ".")
	return ok && isASCIIDigits(major) && isASCIIDigits(minor)
}

func isASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// isWebSocketUpgrade checks if the request is a WebSocket upgrade request.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		strings.EqualFold(r.Header.Get("Upgrade"), "tcp") ||
		strings.EqualFold(r.Header.Get("Connection"), "Upgrade")
}

// copyHeaders copies headers from src to dst, stripping hop-by-hop headers.
func copyHeaders(dst, src http.Header) {
	connectionHeaders := make(map[string]bool)
	for _, value := range src.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			name = http.CanonicalHeaderKey(strings.TrimSpace(name))
			if name != "" {
				connectionHeaders[name] = true
			}
		}
	}
	for key, values := range src {
		canonicalKey := http.CanonicalHeaderKey(key)
		if hopByHopHeaders[canonicalKey] || connectionHeaders[canonicalKey] {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

// streamResponse copies from body to the ResponseWriter, flushing after each
// read for streaming endpoints.
func (s *Server) streamResponse(w http.ResponseWriter, body io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)

	for {
		n, err := body.Read(buf)
		if n > 0 {
			// Write to ResponseWriter: errors indicate a dropped client connection.
			_, _ = w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) trackActiveHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handlerWG.Add(1)
		defer s.handlerWG.Done()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) trackHijackedConnection(conn net.Conn) bool {
	s.hijackMu.Lock()
	defer s.hijackMu.Unlock()
	if s.shuttingDown {
		_ = conn.Close()
		return false
	}
	if s.hijackConns == nil {
		s.hijackConns = make(map[net.Conn]struct{})
	}
	s.hijackConns[conn] = struct{}{}
	return true
}

func (s *Server) untrackHijackedConnection(conn net.Conn) {
	s.hijackMu.Lock()
	delete(s.hijackConns, conn)
	s.hijackMu.Unlock()
}

func (s *Server) closeHijackedConnections() {
	s.hijackMu.Lock()
	s.shuttingDown = true
	connections := make([]net.Conn, 0, len(s.hijackConns))
	for conn := range s.hijackConns {
		connections = append(connections, conn)
	}
	s.hijackMu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (s *Server) waitForActiveHandlers(ctx context.Context) error {
	if s.handlerDone == nil {
		return nil
	}
	s.handlerWait.Do(func() {
		go func() {
			s.handlerWG.Wait()
			close(s.handlerDone)
		}()
	})
	select {
	case <-s.handlerDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ListenAndServe starts the HTTP server. It launches background container
// polling and uses TLS if certificates are configured.
func (s *Server) ListenAndServe() error {
	go s.pollContainers(s.pollCtx)

	if s.cfg.TLSCert != "" && s.cfg.TLSKey != "" {
		return s.httpServer.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
	}
	return s.httpServer.ListenAndServe()
}

// Addr returns the address ListenAndServe bound, or nil if it hasn't bound
// one yet. It exists so callers — tests, chiefly, using an OS-assigned port
// ("0") — can discover the real listening address instead of guessing it or
// racing a separate bind/close/rebind.
func (s *Server) Addr() net.Addr {
	addr, _ := s.listenAddr.Load().(net.Addr)
	return addr
}

// Shutdown gracefully shuts down the HTTP server and stops background goroutines.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		// Stop the pollContainers goroutine.
		if s.pollCancel != nil {
			s.pollCancel()
		}

		// Stop the SIGHUP reload goroutine.
		if s.hupCh != nil {
			signal.Stop(s.hupCh)
		}
		if s.hupDone != nil {
			select {
			case <-s.hupDone:
			default:
				close(s.hupDone)
			}
		}

		// Stop the rate limiter cleanup goroutine.
		s.rateLimiter.Stop()

		// Stop the nonce LRU cleanup goroutine.
		if s.ed25519.Nonces != nil {
			s.ed25519.Nonces.Close()
		}

		// net/http stops tracking a connection once a handler hijacks it, so
		// close Docker exec/attach relays explicitly before waiting for the
		// middleware to emit its final audit record.
		s.closeHijackedConnections()
	})

	// A context-bounded failure means active handlers may still emit their final
	// records. Keep the sink open so a later successful Shutdown can drain those
	// handlers and close it exactly once.
	err := s.httpServer.Shutdown(ctx)
	if err != nil {
		return err
	}
	if err := s.waitForActiveHandlers(ctx); err != nil {
		return err
	}
	s.auditCloseOnce.Do(s.auditor.Close)
	return nil
}

// pollContainers periodically refreshes the container inventory via the
// adapter and lets the adapter broadcast changes. It exits when ctx is cancelled.
func (s *Server) pollContainers(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	// Build the initial inventory and immediately publish its diff. Waiting for
	// the first ticker would leave already-connected standard-mode clients with
	// the empty startup snapshot for an entire poll interval.
	added, updated, removed, err := s.adapter.RefreshContainers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("initial container inventory failed", "error", err)
		}
	} else if ctx.Err() != nil {
		return
	} else if err := s.adapter.OnContainerRefresh(ctx, nil, added, updated, removed); err != nil {
		if ctx.Err() == nil {
			slog.Error("initial container refresh notify failed", "error", err)
		}
	}

	interval := s.adapter.PollInterval()
	if interval <= 0 {
		interval = s.cfg.DDPollInterval
	}

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			added, updated, removed, err := s.adapter.RefreshContainers(ctx)
			if err != nil {
				slog.Error("container refresh failed", "error", err)
				continue
			}
			// In standard mode, sender is nil — adapter handles SSE internally.
			if err := s.adapter.OnContainerRefresh(ctx, nil, added, updated, removed); err != nil {
				slog.Error("container refresh notify failed", "error", err)
			}
		}
	}
}
