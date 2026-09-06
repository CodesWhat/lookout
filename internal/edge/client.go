package edge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/codeswhat/portwing/internal/adapter"
	"github.com/codeswhat/portwing/internal/audit"
	"github.com/codeswhat/portwing/internal/auth"
	"github.com/codeswhat/portwing/internal/config"
	"github.com/codeswhat/portwing/internal/docker"
	applog "github.com/codeswhat/portwing/internal/log"
	"github.com/codeswhat/portwing/internal/metrics"
	"github.com/codeswhat/portwing/internal/pool"
	"github.com/codeswhat/portwing/internal/protocol"
)

// errFatal is returned by connect when the connection fails in a way that
// cannot be recovered by retrying (e.g. a 404 on the WebSocket upgrade, or an
// Ed25519 hello-signing failure). Run propagates it as a fatal error instead of
// entering the reconnect loop.
var errFatal = errors.New("fatal connection error")

const (
	maxReadSize            = 16 * 1024 * 1024  // 16 MB — WebSocket read limit
	maxResponseBody        = 100 * 1024 * 1024 // 100 MB — proxied response body cap
	maxExecSessions        = 100               // concurrent exec sessions
	maxStreams             = 100               // concurrent in-flight tunneled requests
	maxOutboundQueuedBytes = 128 << 20         // aggregate queued and in-flight WebSocket bytes

	// sendQueueSize bounds outbound frames buffered for the sendPump. A full
	// queue means the controller can't keep up, so the connection is evicted
	// (slow-consumer backpressure) rather than letting the backlog grow.
	sendQueueSize = 256
	// writeWait bounds a single WebSocket write. A controller that can't accept
	// a frame within this window is treated as wedged and the connection is
	// evicted, instead of blocking the writer forever.
	writeWait = 10 * time.Second
)

// maxRequestBodyStream caps the in-memory reassembly buffer for a
// request.bodyStream=true request (CapRequestBodyStream). This is a simple
// bounded-buffer design, not a true streaming pipe into dockerd: the whole
// point of the capability is to get past a real build context's size and
// non-JSON bytes, and it can only raise the ceiling, not remove it, without
// also risking a stalled dockerd write blocking the single shared readPump
// goroutine (and therefore every other multiplexed message type, exec I/O,
// pings, other requests) on this connection. Larger than maxResponseBody
// because request bodies in this path are dominated by build contexts. A
// var, not a const, so tests can shrink it instead of sending 512 MB of
// filler to exercise the overflow path.
var maxRequestBodyStream int64 = 512 * 1024 * 1024 // 512 MB

// maxPendingRequestBodies caps how many request.bodyStream=true requests may
// be reassembling at once. maxRequestBodyStream bounds one buffer; without
// this the number of buffers is bounded only by how many RequestIDs the
// controller opens, because registration deliberately defers the streamSem
// slot until stream_end. Pinned to maxStreams: every pending body is a
// request that will claim one of those slots the moment its stream_end
// lands, so the two stages stay symmetric. A var, not a const, so tests can
// shrink it.
var maxPendingRequestBodies = maxStreams

// maxStreamedRequestBodyBytes caps the sum of every streamed request body
// the agent is holding in memory, which is what actually bounds agent memory
// on this path — a count cap alone would still permit maxStreams *
// maxRequestBodyStream. It spans both stages: the reassembly buffers still
// filling, and the reassembled bodies already handed to handleRequestTo,
// which keep their bytes for the whole Docker round trip. Charging only the
// first stage would let a controller finish every pending body to zero the
// count and immediately refill it, so the effective ceiling would be
// maxStreams * maxRequestBodyStream again. Twice maxRequestBodyStream, so a
// single largest-allowed body is still reachable and a second one can
// overlap it. A var, not a const, so tests can shrink it.
var maxStreamedRequestBodyBytes int64 = 1024 * 1024 * 1024 // 1 GB

// requestBodyStreamIdleTimeout bounds how long a pending BodyStream
// reassembly waits for the next stream frame (or stream_end) before it is
// abandoned. Reset on every chunk received, so it only fires against a
// controller that stalls mid-upload, not a slow-but-steady one. A var, not a
// const, so tests can shrink it instead of running the idle-timeout path at
// the real 30s.
var requestBodyStreamIdleTimeout = 30 * time.Second

// reconnectWait is the timer the reconnect-retry loop waits on between
// attempts. A var, not a direct time.After call, so tests can observe the
// exact backoff delay sequence deterministically instead of inferring it
// from how many reconnects fit in a wall-clock window.
var reconnectWait = time.After

type outboundQueueState struct {
	mu     sync.Mutex
	bytes  int64
	closed bool
}

type outboundTarget struct {
	conn  *websocket.Conn
	ch    chan protocol.Envelope
	state *outboundQueueState
}

type outboundEnqueueResult uint8

const (
	outboundEnqueued outboundEnqueueResult = iota
	outboundQueueClosed
	outboundByteLimitExceeded
	outboundFrameLimitExceeded
)

// dockerAPI is the subset of *docker.Client the edge Client depends on. It is
// defined on the consumer side so the tunnel's exec sessions and the request
// fan-out can be exercised against a fake Docker daemon without a live socket.
// *docker.Client satisfies it structurally.
type dockerAPI interface {
	GetVersion(ctx context.Context) (string, error)
	Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error)
	DoWithHeaders(ctx context.Context, method, path string, headers http.Header, body io.Reader) (*http.Response, error)
	DoStream(ctx context.Context, method, path string, body io.Reader) (*http.Response, error)
	DoStreamWithHeaders(ctx context.Context, method, path string, headers http.Header, body io.Reader) (*http.Response, error)
	CreateExec(ctx context.Context, containerID string, cmd []string, user string, tty bool) (string, error)
	StartExec(ctx context.Context, execID string, tty bool) (net.Conn, error)
	ResizeExec(ctx context.Context, execID string, cols, rows int) error
}

// hostCollector is the *metrics.Collector subset sendMetrics depends on,
// defined on the consumer side like dockerAPI above. *metrics.Collector
// satisfies it structurally.
//
// The seam is what makes the failed-collection branch testable at all:
// Collect's only error is a missing /proc, which cannot be arranged on the
// Linux hosts that run CI, and the field's concrete type meant the branch had
// no unit coverage anywhere.
type hostCollector interface {
	Collect() (*metrics.HostMetrics, error)
}

// edgeMessageSender wraps the edge Client to implement adapter.MessageSender.
type edgeMessageSender struct {
	client *Client
	target *outboundTarget
}

func (s *edgeMessageSender) SendTypedMessage(msgType string, data any) error {
	if s.target != nil {
		return s.client.sendTypedMessageTo(*s.target, msgType, data)
	}
	return s.client.sendTypedMessage(msgType, data)
}

func (c *Client) currentMessageSender() *edgeMessageSender {
	target := c.currentOutboundTarget()
	return &edgeMessageSender{client: c, target: &target}
}

// Client is the edge-mode WebSocket client that connects to a controller
// and tunnels Docker API requests over the WebSocket.
type Client struct {
	cfg          *config.Config
	dockerClient dockerAPI
	adapter      adapter.EdgeAdapter
	compose      *docker.ComposeManager
	collector    hostCollector
	metrics      *metrics.Registry
	auditor      *audit.Logger
	startTime    time.Time

	conn   *websocket.Conn
	connMu sync.Mutex

	// sendCh fronts all post-handshake writes with a single sendPump goroutine,
	// so a slow controller backs up here instead of head-of-line-blocking every
	// sender or stalling the read pump. It is nil outside an active connection;
	// the hello/welcome handshake writes the conn directly (no concurrent
	// writer exists yet). Guarded by connMu alongside conn.
	sendCh    chan protocol.Envelope
	sendState *outboundQueueState

	execSessions    sync.Map
	execAdmissionMu sync.Mutex

	// streamSem bounds concurrent in-flight request handlers (maxStreams).
	streamSem chan struct{}

	// welcomePollInterval is the poll interval (seconds) received from the
	// controller's welcome frame. Zero means the controller did not supply one,
	// so writePump falls back to DDPollInterval from config. Set once per
	// connection before writePump starts; writePump reads it without a lock.
	welcomePollInterval int

	// controllerCaps holds the capability tokens the controller advertised in
	// its welcome frame (nil/empty for a controller that predates
	// capabilities, or one with none to advertise). Set once per connection
	// in connect() before the read pump starts dispatching handleRequest
	// goroutines, and read-only for the rest of that connection's lifetime —
	// same single-writer-before-readers pattern as welcomePollInterval above,
	// so handleRequest's concurrent goroutines read it without a lock.
	controllerCaps []string

	// pendingBodies tracks in-flight request.bodyStream=true requests
	// (CapRequestBodyStream) awaiting reassembly, keyed by RequestID. An
	// entry is created when such a request arrives in readPump and removed
	// exactly once, by whichever of stream_end, the maxRequestBodyStream
	// or maxStreamedRequestBodyBytes caps, or requestBodyStreamIdleTimeout
	// fires first, so a given RequestID is dispatched or errored exactly
	// once. Entry count is capped by maxPendingRequestBodies. Guarded by
	// pendingBodiesMu; nil until the first BodyStream request arrives.
	pendingBodiesMu sync.Mutex
	pendingBodies   map[string]*pendingRequestBody

	// dispatchingBodies is the second half of the maxStreamedRequestBodyBytes
	// budget: the reassembled size of every streamed body that has left
	// pendingBodies for handleRequestTo but is still held in memory as
	// req.Body until the Docker round trip finishes. Keyed by a monotonic
	// sequence from nextDispatchSeq rather than by RequestID, so a controller
	// that reuses a RequestID while the first dispatch is still running
	// cannot overwrite a live reservation. Guarded by pendingBodiesMu, the
	// same lock as pendingBodies, so finishPendingBody's handoff between the
	// two is atomic and the sum never dips between stages. Nil until the
	// first streamed body is dispatched.
	dispatchingBodies map[uint64]int64
	nextDispatchSeq   uint64

	// metricsCollectFailing records whether the last host-metrics collection
	// failed, so sendMetrics can log the transitions instead of repeating one
	// warning every heartbeat forever on a host that can never report them.
	// It tracks the collection, not the connection, so a reconnect does not
	// re-announce a condition that never changed. Atomic because Swap is what
	// makes the transition log fire exactly once, which holds even if a caller
	// is ever added off the write pump.
	metricsCollectFailing atomic.Bool

	// Health server for Docker HEALTHCHECK.
	healthServer *http.Server
	// healthServerDone is closed by the ListenAndServe goroutine started in
	// startHealthServer once it has returned and performed its post-Shutdown
	// error check (and logged, if warranted). It gives callers — chiefly
	// tests asserting on that log — a happens-before point instead of
	// racing the goroutine with a poll loop. Each startHealthServer call
	// creates its own channel and hands it to its own goroutine, so a
	// second start cannot double-close or leave an earlier channel
	// unclosed.
	healthServerDone chan struct{}
	// healthListenAddr holds the net.Addr the health server's BaseContext
	// hook captured once its listener bound. It lets callers — chiefly
	// tests using an OS-assigned port ("0") — discover the real bound
	// address instead of racing a separate allocate/close/rebind, the same
	// pattern internal/server.Server.Addr uses for the main HTTP server.
	//
	// A pointer, not an atomic.Value, because startHealthServer has to be
	// able to reset it: BaseContext only runs on a successful listen, so a
	// restart that fails to bind would otherwise leave the previous server's
	// address readable and break HealthAddr's nil-until-bound contract.
	// atomic.Value cannot store a nil interface, which is what that reset
	// needs to write.
	healthListenAddr atomic.Pointer[net.Addr]
}

// pendingRequestBody accumulates the stream/stream_end frames that follow a
// request.bodyStream=true request until it can be dispatched to
// handleRequestTo with a fully reassembled Body.
type pendingRequestBody struct {
	req    protocol.RequestMessage
	target outboundTarget
	buf    bytes.Buffer
	timer  *time.Timer
	// gen counts how many times the idle timer has been armed for this
	// entry. time.Timer.Reset does not cancel an AfterFunc whose deadline
	// already elapsed, so a timeout callback can still be in flight when a
	// chunk re-arms the timer; each callback carries the gen it was armed
	// with and no-ops when it no longer matches. Guarded by pendingBodiesMu.
	//
	// It separates armings of this entry and nothing more. Every entry
	// starts at 0, so it cannot separate this entry from a later one
	// registered under the same RequestID; that is the entry-pointer half of
	// failPendingBody's match.
	gen uint64
}

// NewClient creates a new edge-mode Client.
func NewClient(cfg *config.Config, dockerClient *docker.Client, a adapter.EdgeAdapter, auditor *audit.Logger) *Client {
	if cfg.TLSSkipVerify {
		slog.Warn("TLS certificate verification disabled (TLS_SKIP_VERIFY=true): the outbound controller connection is vulnerable to man-in-the-middle interception; use only for testing")
	}
	registry := metrics.NewRegistry()
	registry.SetEdgeMode(true)
	return &Client{
		cfg:          cfg,
		dockerClient: dockerClient,
		adapter:      a,
		compose:      docker.NewComposeManager(cfg.StacksDir, dockerClient.GetAPIVersion(), cfg.DockerSocket),
		collector:    metrics.NewDaemonCollector(dockerClient, cfg.SkipDFCollection),
		metrics:      registry,
		auditor:      auditor,
		startTime:    time.Now(),
		streamSem:    make(chan struct{}, maxStreams),
	}
}

// Run is the main loop. It starts a minimal health server and then enters a
// connect-retry loop with exponential backoff and jitter.
func (c *Client) Run(ctx context.Context) error {
	c.ensureOperationalState()
	// Start minimal health HTTP server for Docker HEALTHCHECK.
	c.startHealthServer()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.healthServer.Shutdown(shutdownCtx); err != nil {
			slog.Warn("health server shutdown error", "error", err)
		}
	}()

	delay := time.Duration(c.cfg.ReconnectDelay) * time.Second
	maxDelay := time.Duration(c.cfg.MaxReconnectDelay) * time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		established, err := c.connect(ctx)
		if ctx.Err() != nil {
			// Shutting down - send close frame if we still have a connection.
			c.connMu.Lock()
			if c.conn != nil {
				// Best-effort close frame on shutdown; ignore send errors.
				_ = c.conn.WriteMessage(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown"),
				)
				closeWebSocket(c.conn, "shutdown")
				c.conn = nil
			}
			c.connMu.Unlock()
			return ctx.Err()
		}

		if err != nil {
			if errors.Is(err, errFatal) {
				slog.Error("fatal connection error, not retrying", "error", applog.Sanitize(err.Error()))
				return err
			}
			slog.Warn("connection lost", "error", applog.Sanitize(err.Error()))
		}

		// Reset backoff after a connection that was actually established, so a
		// long-lived session that later drops reconnects from RECONNECT_DELAY
		// instead of inheriting stale backoff from earlier failures (SPEC §13.1).
		if established {
			delay = time.Duration(c.cfg.ReconnectDelay) * time.Second
		}

		waitDuration := jitteredDuration(delay)

		c.metrics.IncReconnect()
		slog.Info("reconnecting", "delay", waitDuration.Round(time.Millisecond))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-reconnectWait(waitDuration):
		}

		// Exponential backoff.
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// connect dials the WebSocket, performs the hello/welcome handshake, syncs
// state, and runs the read and write pumps.
func (c *Client) connect(ctx context.Context) (bool, error) {
	c.ensureOperationalState()
	// Build TLS config. Pin a TLS 1.2 floor to match the server's inbound
	// posture; the controller dial relies on Go defaults otherwise.
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// #nosec G402 -- TLS_SKIP_VERIFY is an explicit test-only escape hatch documented as unsafe.
		InsecureSkipVerify: c.cfg.TLSSkipVerify,
	}

	if c.cfg.CACert != "" {
		caCert, err := os.ReadFile(c.cfg.CACert)
		if err != nil {
			return false, fmt.Errorf("reading CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return false, fmt.Errorf("failed to parse CA cert")
		}
		tlsConfig.RootCAs = pool
	}

	// Build WebSocket URL.
	wsURL := c.cfg.DrydockURL + "/api/portwing/ws"
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)

	dialer := websocket.Dialer{
		TLSClientConfig:  tlsConfig,
		HandshakeTimeout: 10 * time.Second,
	}

	slog.Info("connecting to controller", "url", wsURL)

	conn, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		if errors.Is(err, websocket.ErrBadHandshake) && resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, fmt.Errorf("%w: controller returned 404 on WebSocket upgrade — the /api/portwing/ws route is absent or has been disabled", errFatal)
		}
		return false, fmt.Errorf("websocket dial: %w", err)
	}
	conn.SetReadLimit(maxReadSize)

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	// Cancelling ctx has to reach the socket, not only the loops that poll it.
	// readPump checks ctx once per iteration, at the top; once it is inside
	// conn.ReadMessage the cancel is invisible to it until the controller
	// closes the socket or the steady-state read deadline expires — and that
	// deadline floors at 60 seconds (readDeadline), which outlives both
	// Docker's and Kubernetes' default SIGKILL grace. A healthy controller
	// with nothing to say is exactly the case that hits it. Closing the conn
	// is what makes the blocked read fail, so SIGTERM unwinds in milliseconds
	// instead of up to a minute.
	//
	// The read error that close produces is not a reconnect signal: Run tests
	// ctx.Err() before it looks at connect's error, so it returns ctx.Err()
	// without reaching IncReconnect.
	//
	// The closure captures this conn, not c.conn, so even a callback that ran
	// late could only close the generation it was registered for and never a
	// replacement the reconnect loop has since installed. stopOnCancel is
	// still deferred, because an AfterFunc that is never stopped stays
	// registered on ctx for as long as the agent runs and the reconnect loop
	// would add one per attempt. Nothing is logged here: the close is expected
	// and gorilla's Close only forwards the net.Conn's error, which the
	// teardown path below reports already.
	stopOnCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopOnCancel()

	// Send hello.
	if err := c.sendHello(ctx); err != nil {
		closeWebSocket(conn, "send hello failure")
		return false, fmt.Errorf("sending hello: %w", err)
	}

	// Wait for welcome.
	welcomeTimeout := time.Duration(c.cfg.WelcomeTimeout) * time.Second
	if err := conn.SetReadDeadline(time.Now().Add(welcomeTimeout)); err != nil {
		closeWebSocket(conn, "set welcome deadline failure")
		return false, fmt.Errorf("setting welcome deadline: %w", err)
	}

	_, msg, err := conn.ReadMessage()
	if err != nil {
		closeWebSocket(conn, "read welcome failure")
		return false, fmt.Errorf("reading welcome: %w", err)
	}

	var env protocol.Envelope
	if err := json.Unmarshal(msg, &env); err != nil {
		closeWebSocket(conn, "parse welcome failure")
		return false, fmt.Errorf("parsing welcome envelope: %w", err)
	}
	if env.Type == protocol.TypeError {
		var errMsg protocol.ErrorMessage
		parsed := json.Unmarshal(env.Data, &errMsg) == nil
		closeWebSocket(conn, "controller rejected hello")
		rejErr := fmt.Errorf("controller rejected hello: %s (%s)", errMsg.Message, errMsg.Code)

		switch {
		case !parsed:
			// No reliable code to classify on — treat as retryable.
			slog.Warn("controller rejected hello with an unparseable error payload", "raw", string(env.Data))
		case isTerminalHelloRejection(errMsg.Code):
			// Retrying the same configuration cannot change the outcome, so fail
			// fast (wrap errFatal) instead of reconnecting forever.
			slog.Error("controller rejected hello with a terminal code, not retrying",
				"code", errMsg.Code, "message", errMsg.Message)
			return false, fmt.Errorf("%w: %w", errFatal, rejErr)
		case !isKnownHelloRejection(errMsg.Code):
			// Unrecognized code: default to retry (no regression vs. before this
			// classifier), but log distinctly so a permanent-but-unknown code
			// doesn't loop silently.
			slog.Warn("controller rejected hello with an unrecognized code, retrying",
				"code", errMsg.Code, "message", errMsg.Message)
		default:
			slog.Warn("controller rejected hello, retrying", "code", errMsg.Code, "message", errMsg.Message)
		}
		return false, rejErr
	}
	if env.Type != protocol.TypeWelcome {
		closeWebSocket(conn, "unexpected welcome type")
		return false, fmt.Errorf("expected welcome, got %q", env.Type)
	}

	// Reset per-connection negotiated state so a reconnect to an older
	// controller (or one with a parse failure below) doesn't inherit
	// capabilities advertised by a previous connection.
	c.controllerCaps = nil

	var welcome protocol.WelcomeMessage
	if err := json.Unmarshal(env.Data, &welcome); err != nil {
		slog.Warn("could not parse welcome payload", "error", err)
	} else {
		c.controllerCaps = welcome.Capabilities
		if welcome.PollInterval > 0 {
			c.welcomePollInterval = welcome.PollInterval
		}
		if compat, ok := welcome.Config["serverCompatLevel"]; ok {
			// Compare major version only so patch-level bumps on either side
			// do not trigger spurious warnings. This matches the drydock
			// controller's comparison semantics (major-version-only check).
			// Any major-version mismatch (either direction) is diagnostic-only —
			// the wire connection is still accepted; this warns operators to
			// check the compat matrix.
			serverMajor := strings.SplitN(compat, ".", 2)[0]
			agentMajor := strings.SplitN(protocol.DrydockCompat, ".", 2)[0]
			if serverMajor != agentMajor {
				slog.Warn("controller compat level mismatch",
					"serverCompatLevel", applog.Sanitize(compat),
					"agentExpects", protocol.DrydockCompat,
				)
			}
		}
	}

	// Switch from the one-shot welcome deadline to the steady-state read
	// deadline (SPEC §13.2). readPump re-arms it after every message; if the
	// controller stops answering pings the read times out and we reconnect.
	if err := conn.SetReadDeadline(time.Now().Add(readDeadline(c.cfg.HeartbeatInterval))); err != nil {
		closeWebSocket(conn, "set read deadline failure")
		return false, fmt.Errorf("setting read deadline: %w", err)
	}

	slog.Info("connected to controller")
	c.metrics.SetControllerConnected(true)
	defer c.metrics.SetControllerConnected(false)

	pumpCtx, pumpCancel := context.WithCancel(ctx)
	defer pumpCancel()

	var wg sync.WaitGroup

	// Bring the outbound send path up before any post-handshake send, so the
	// adapter sync, metrics, and every pump funnel through the single sendPump
	// (the only writer) instead of writing the conn concurrently.
	sendCh := make(chan protocol.Envelope, sendQueueSize)
	sendState := &outboundQueueState{}
	c.connMu.Lock()
	c.sendCh = sendCh
	c.sendState = sendState
	c.connMu.Unlock()

	wg.Add(1)
	go func() {
		defer wg.Done()
		c.sendPump(pumpCtx, ctx, conn, sendCh)
	}()

	// Let adapter handle initial sync (container sync, component sync, etc.).
	sender := c.currentMessageSender()
	if err := c.adapter.OnConnect(ctx, sender); err != nil {
		slog.Warn("adapter OnConnect failed", "error", err)
	}

	// Send initial metrics.
	c.sendMetrics()

	// Run pumps.
	wg.Add(2)

	var readErr error
	go func() {
		defer wg.Done()
		readErr = c.readPump(pumpCtx)
		pumpCancel()
	}()

	go func() {
		defer wg.Done()
		c.writePump(pumpCtx)
	}()

	wg.Wait()

	// Tear down any exec sessions that outlived this connection so they (and
	// their Docker exec conns) don't leak across reconnects; the next
	// connection starts with a clean exec table.
	c.closeAllExecSessions()
	// Same for the streamed request bodies still reassembling: their
	// stream_end died with the connection, so the next one starts with a
	// clean pending table instead of rejecting the controller's retries.
	c.failAllPendingBodies()

	// Close connection.
	c.connMu.Lock()
	if c.conn != nil {
		closeWebSocket(c.conn, "connection loop end")
		c.conn = nil
	}
	c.sendCh = nil
	c.sendState = nil
	c.connMu.Unlock()

	// Reaching here means the welcome handshake succeeded, so the connection
	// counts as established even if the read pump later returned an error.
	return true, readErr
}

// sendHello sends the hello handshake message. When PRIVATE_KEY_FILE is
// configured, the hello is signed with Ed25519 and the signature fields are
// populated (tokenHash is left empty). Otherwise, tokenHash is set as before.
func (c *Client) sendHello(ctx context.Context) error {
	dockerVersion, err := c.dockerClient.GetVersion(ctx)
	if err != nil {
		dockerVersion = "unknown"
	}

	hostname, _ := os.Hostname()

	capabilities := []string{
		"compose",
		"exec",
		"metrics",
		"events",
		// Symmetric advertisement only — the controller does not gate on
		// anything in hello.capabilities today. The load-bearing negotiation
		// direction is welcome.capabilities -> controllerCaps, checked in
		// handleRequest via hasControllerCap.
		protocol.CapResponseBodyBase64,
		// Load-bearing here, unlike CapResponseBodyBase64 above: this tells
		// the controller the agent can reassemble a request.bodyStream=true
		// request (see readPump's TypeStream/TypeStreamEnd cases and
		// pendingRequestBody), so it is safe to send one. A controller that
		// doesn't recognize this token simply never sends bodyStream=true
		// and nothing changes for it.
		protocol.CapRequestBodyStream,
	}
	capabilities = append(capabilities, c.adapter.Capabilities()...)

	hello := protocol.HelloMessage{
		Version:       protocol.AgentVersion,
		Protocol:      protocol.ProtocolString,
		AgentID:       c.cfg.AgentID,
		AgentName:     c.cfg.AgentName,
		DockerVersion: dockerVersion,
		Hostname:      hostname,
		Capabilities:  capabilities,
	}

	// Sign the hello with Ed25519. If signing fails, this is fatal — drydock
	// rejects token-only agents, so falling back to a token hash (or retrying a
	// key the agent can't read/parse) would just loop forever against a
	// controller that always responds with ed25519-required. errFatal stops Run
	// rather than entering the reconnect loop.
	if c.cfg.PrivateKeyFile != "" {
		if err := c.signHello(ctx, &hello); err != nil {
			return fmt.Errorf("%w: ed25519 hello signing failed (check PRIVATE_KEY_FILE path, permissions, and format): %w", errFatal, err)
		}
	} else {
		c.setTokenHash(&hello)
	}

	// Merge adapter-specific hello extension fields.
	if ext := c.adapter.HelloExtension(); ext != nil {
		hello.DrydockCompat = ext.DrydockCompat
		hello.WatcherTypes = ext.WatcherTypes
		hello.TriggerTypes = ext.TriggerTypes
	}

	return c.sendTypedMessage(protocol.TypeHello, hello)
}

// setTokenHash sets the TokenHash field from cfg.Token.
// hasControllerCap reports whether the controller advertised the given
// capability token in its welcome frame for the current connection. An older
// controller's welcome omits the capabilities field entirely, which parses
// as a nil slice, so this reports false for it — the correct legacy-path
// answer — rather than erroring.
func (c *Client) hasControllerCap(token string) bool {
	for _, got := range c.controllerCaps {
		if got == token {
			return true
		}
	}
	return false
}

func (c *Client) setTokenHash(hello *protocol.HelloMessage) {
	if c.cfg.Token != "" {
		hash := sha256.Sum256([]byte(c.cfg.Token))
		hello.TokenHash = fmt.Sprintf("%x", hash)
	}
}

// signHello signs the hello message with the configured Ed25519 private key.
// The WebSocket upgrade path is used as the "path" in the canonical string,
// with the empty-body hash.
func (c *Client) signHello(_ context.Context, hello *protocol.HelloMessage) error {
	priv, err := auth.LoadPrivateKey(c.cfg.PrivateKeyFile)
	if err != nil {
		return fmt.Errorf("loading private key: %w", err)
	}

	pub := priv.Public().(ed25519.PublicKey)
	keyID := auth.KeyIDForPublicKey(pub)

	nonce, err := auth.NewNonce()
	if err != nil {
		return fmt.Errorf("generating nonce: %w", err)
	}

	tsUnix := time.Now().Unix()

	// The canonical path is the WebSocket upgrade URL path.
	wsPath := "/api/portwing/ws"
	msg := auth.CanonicalMessage("GET", wsPath, auth.BodyHashHex(nil), tsUnix, nonce)
	sig := ed25519.Sign(priv, msg)

	hello.PubKeyID = keyID
	hello.Timestamp = tsUnix
	hello.Nonce = nonce
	hello.Signature = base64.RawURLEncoding.EncodeToString(sig)
	// Do not set TokenHash when using Ed25519 auth.
	hello.TokenHash = ""

	return nil
}

// readPump reads messages from the WebSocket and dispatches them.
func (c *Client) readPump(ctx context.Context) error {
	sender := c.currentMessageSender()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read message: %w", err)
		}

		// Re-arm the read deadline on every received message, including pings
		// (SPEC §13.2). A read that blocks past the deadline means the
		// controller has gone silent and the connection is dead.
		if err := c.conn.SetReadDeadline(time.Now().Add(readDeadline(c.cfg.HeartbeatInterval))); err != nil {
			return fmt.Errorf("resetting read deadline: %w", err)
		}

		var env protocol.Envelope
		if err := json.Unmarshal(msg, &env); err != nil {
			slog.Warn("invalid message envelope", "error", err)
			continue
		}

		switch env.Type {
		case protocol.TypeRequest:
			var req protocol.RequestMessage
			if err := json.Unmarshal(env.Data, &req); err != nil {
				slog.Warn("invalid request message", "error", err)
				continue
			}
			if req.BodyStream {
				// The body isn't inline: it arrives as follow-up
				// TypeStream/TypeStreamEnd frames below, keyed by
				// RequestID. Defer dispatch (and the streamSem slot it
				// would consume) until reassembly completes.
				c.registerPendingBody(req, c.currentOutboundTarget())
				continue
			}
			// Bound concurrent request handlers (maxStreams). Reject rather than
			// block the read loop, which must keep servicing pings and exec I/O.
			select {
			case c.streamSem <- struct{}{}:
				target := c.currentOutboundTarget()
				go func() {
					defer func() { <-c.streamSem }()
					c.handleRequestTo(ctx, req, target)
				}()
			default:
				slog.Warn("concurrent request limit reached, rejecting", "max", maxStreams, "request_id", applog.Sanitize(req.RequestID))
				_ = c.sendTypedMessage(protocol.TypeError, protocol.ErrorMessage{
					Message:   "agent busy: too many concurrent requests",
					RequestID: req.RequestID,
				})
			}

		case protocol.TypeExecStart:
			var msg protocol.ExecStartMessage
			if err := json.Unmarshal(env.Data, &msg); err != nil {
				slog.Warn("invalid exec_start message", "error", err)
				continue
			}
			c.auditor.ExecStart(c.cfg.DrydockURL, msg.ContainerID, msg.ExecID)
			// Synchronous: StartExec only registers the session and spawns the
			// Docker bring-up, so it returns immediately. Registering before the
			// next message is dispatched is what keeps a following exec_input
			// from racing the bring-up and being dropped (ordered exec I/O).
			c.StartExec(ctx, msg)

		case protocol.TypeExecInput:
			var msg protocol.ExecInputMessage
			if err := json.Unmarshal(env.Data, &msg); err != nil {
				slog.Warn("invalid exec_input message", "error", err)
				continue
			}
			c.HandleInput(msg)

		case protocol.TypeExecResize:
			var msg protocol.ExecResizeMessage
			if err := json.Unmarshal(env.Data, &msg); err != nil {
				slog.Warn("invalid exec_resize message", "error", err)
				continue
			}
			c.HandleResize(ctx, msg)

		case protocol.TypeExecEnd:
			var msg protocol.ExecEndMessage
			if err := json.Unmarshal(env.Data, &msg); err != nil {
				slog.Warn("invalid exec_end message", "error", err)
				continue
			}
			c.EndExec(msg)

		case protocol.TypeStream:
			var sm protocol.StreamMessage
			if err := json.Unmarshal(env.Data, &sm); err != nil {
				slog.Warn("invalid stream message", "error", err)
				continue
			}
			// A chunk for a registered BodyStream request is consumed here;
			// anything else (an adapter-owned stream, or a chunk with no
			// matching pending body) falls through unchanged, preserving
			// existing behavior for any future adapter use of TypeStream.
			if c.appendPendingBody(sm.RequestID, sm.Data) {
				continue
			}
			if !c.adapter.HandleMessage(ctx, sender, env.Type, env.Data) {
				slog.Debug("unhandled message type", "type", applog.Sanitize(env.Type))
			}

		case protocol.TypeStreamEnd:
			var se protocol.StreamEndMessage
			if err := json.Unmarshal(env.Data, &se); err != nil {
				slog.Warn("invalid stream_end message", "error", err)
				continue
			}
			if req, target, reservation, ok := c.finishPendingBody(se.RequestID); ok {
				// Everything past this point, including the streamSem
				// admission check an inline-body request does inline above,
				// belongs to dispatchStreamedBody, which owns the byte
				// reservation finishPendingBody just made and releases it on
				// every exit. Doing the admission check here instead would
				// need a second release site on the rejection branch.
				go c.dispatchStreamedBody(ctx, req, target, reservation)
				continue
			}
			if !c.adapter.HandleMessage(ctx, sender, env.Type, env.Data) {
				slog.Debug("unhandled message type", "type", applog.Sanitize(env.Type))
			}

		case protocol.TypePing:
			var ping protocol.PingMessage
			if err := json.Unmarshal(env.Data, &ping); err != nil {
				slog.Debug("invalid ping message", "error", err)
				continue
			}
			// Best-effort pong reply; connection loss will surface on next read.
			_ = c.sendTypedMessage(protocol.TypePong, protocol.PongMessage(ping))

		case protocol.TypeError:
			var errMsg protocol.ErrorMessage
			if err := json.Unmarshal(env.Data, &errMsg); err != nil {
				slog.Warn("invalid error message", "error", err)
				continue
			}
			slog.Warn("received error from controller",
				"code", applog.Sanitize(errMsg.Code),
				"message", applog.Sanitize(errMsg.Message),
				"requestId", applog.Sanitize(errMsg.RequestID),
			)

		default:
			// Delegate to adapter for unrecognized message types.
			if !c.adapter.HandleMessage(ctx, sender, env.Type, env.Data) {
				slog.Debug("unhandled message type", "type", applog.Sanitize(env.Type))
			}
		}
	}
}

// registerPendingBody records a request.bodyStream=true request and starts
// its idle timeout, deferring dispatch until finishPendingBody sees the
// matching stream_end. A duplicate RequestID (a second bodyStream=true
// request arriving before the first finished), or a request that would push
// the agent past maxPendingRequestBodies concurrent reassemblies, is
// rejected with a TypeError instead of silently clobbering the in-flight
// reassembly or growing the map without bound.
func (c *Client) registerPendingBody(req protocol.RequestMessage, target outboundTarget) {
	if req.RequestID == "" {
		slog.Warn("streamed request body missing requestId, rejecting")
		_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
			Message: "streamed request body requires requestId",
		})
		return
	}

	c.pendingBodiesMu.Lock()
	if c.pendingBodies == nil {
		c.pendingBodies = make(map[string]*pendingRequestBody)
	}
	if _, exists := c.pendingBodies[req.RequestID]; exists {
		c.pendingBodiesMu.Unlock()
		slog.Warn("duplicate requestId for streamed request body, rejecting", "request_id", applog.Sanitize(req.RequestID))
		_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
			Message:   "duplicate requestId for streamed request body",
			RequestID: req.RequestID,
		})
		return
	}
	if len(c.pendingBodies) >= maxPendingRequestBodies {
		c.pendingBodiesMu.Unlock()
		slog.Warn("concurrent streamed request body limit reached, rejecting", "max", maxPendingRequestBodies, "request_id", applog.Sanitize(req.RequestID))
		_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
			Message:   "agent busy: too many concurrent streamed request bodies",
			RequestID: req.RequestID,
		})
		return
	}
	pb := &pendingRequestBody{req: req, target: target}
	// The callback names the entry it was armed for, not just the RequestID
	// it is filed under: the ID is freed and re-registered while a callback
	// for it may already be running (the teardown drain then the controller's
	// retry, or stream_end then a second request under the same ID), and the
	// successor starts at gen 0 exactly like this one. gen 0 is this first
	// timer's arming generation; appendPendingBody bumps it on every re-arm
	// so a callback that already fired can tell it lost the race and must not
	// fail a live upload.
	pb.timer = time.AfterFunc(requestBodyStreamIdleTimeout, func() {
		c.failPendingBody(req.RequestID, pb, 0)
	})
	c.pendingBodies[req.RequestID] = pb
	c.pendingBodiesMu.Unlock()
}

// appendPendingBody decodes and appends one stream chunk to the pending body
// registered under requestID. It reports false when requestID has no
// registered pending body (an adapter-owned stream, or a chunk that lost its
// race with an already-finished/failed/timed-out one), leaving env.Data
// untouched so the caller can fall through to adapter.HandleMessage. It
// reports true for everything else, including a decode failure or a
// per-request/aggregate size overflow — all of those fail the request
// (TypeError) and clean up, but the chunk was still consumed as belonging to
// this path.
func (c *Client) appendPendingBody(requestID, encodedChunk string) bool {
	c.pendingBodiesMu.Lock()
	pb, ok := c.pendingBodies[requestID]
	if !ok {
		c.pendingBodiesMu.Unlock()
		return false
	}

	decoded, err := base64.StdEncoding.DecodeString(encodedChunk)
	if err != nil {
		delete(c.pendingBodies, requestID)
		timer, target := pb.timer, pb.target
		c.pendingBodiesMu.Unlock()
		timer.Stop()
		_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
			Message:   "invalid base64 in streamed request body chunk",
			RequestID: requestID,
		})
		return true
	}

	// Two ceilings: this body on its own, and every streamed body the agent
	// is holding added together. The aggregate one is what bounds agent
	// memory, since the per-request cap multiplies by however many requests
	// are open, and it counts dispatched bodies too — those bytes are still
	// resident for the whole Docker round trip.
	var overflow string
	switch {
	case int64(pb.buf.Len()+len(decoded)) > maxRequestBodyStream:
		overflow = fmt.Sprintf("streamed request body exceeds %d byte limit", maxRequestBodyStream)
	case c.streamedBodyBytesLocked()+int64(len(decoded)) > maxStreamedRequestBodyBytes:
		overflow = fmt.Sprintf("streamed request bodies exceed the %d byte aggregate in-memory limit", maxStreamedRequestBodyBytes)
	}
	if overflow != "" {
		delete(c.pendingBodies, requestID)
		timer, target := pb.timer, pb.target
		c.pendingBodiesMu.Unlock()
		timer.Stop()
		_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
			Message:   overflow,
			RequestID: requestID,
		})
		return true
	}

	pb.buf.Write(decoded)
	// Re-arm on forward progress, so a slow-but-steady multi-chunk upload
	// isn't held to the same deadline as a stalled one. Reset alone is not
	// enough: it cannot recall an AfterFunc whose deadline already elapsed,
	// so a callback queued while this chunk held the lock would still find
	// the entry and fail an upload that was making progress. Bump gen and
	// arm a fresh timer, which leaves any in-flight callback stale.
	pb.gen++
	gen := pb.gen
	pb.timer.Stop()
	pb.timer = time.AfterFunc(requestBodyStreamIdleTimeout, func() {
		c.failPendingBody(requestID, pb, gen)
	})
	c.pendingBodiesMu.Unlock()
	return true
}

// streamedBodyBytesLocked sums every streamed request body the agent is
// currently holding: the reassembly buffers still filling in pendingBodies,
// plus the reassembled bodies dispatchingBodies is holding open across their
// Docker round trip. The caller must hold pendingBodiesMu, which is also what
// makes the sum safe to read: every removal path deletes its entry before
// releasing the lock, so an entry visible here is one nobody else is writing
// to.
//
// Summing two maps instead of maintaining one int64 counter is deliberate,
// and the reason survived adding the dispatch stage. pendingBodies has four
// removal sites (stream_end, a base64 decode failure, either size cap, and
// the idle timeout) and every one of them already deletes its entry, so no
// bookkeeping can drift there however many more get added. Only the dispatch
// stage needs an explicit reservation, and it has exactly one insert
// (finishPendingBody, under this lock) and one release (the sole defer in
// dispatchStreamedBody). Don't "simplify" this back into a counter: that puts
// a decrement on all five paths, and delete is idempotent where a subtraction
// is not, so a double release here is a no-op instead of silent negative
// drift. O(len(pendingBodies) + len(dispatchingBodies)); the first is capped
// by maxPendingRequestBodies and the second by the streamed requests in
// flight to Docker (streamSem) plus the short-lived rejections readPump has
// just spawned.
func (c *Client) streamedBodyBytesLocked() int64 {
	var total int64
	for _, pb := range c.pendingBodies {
		total += int64(pb.buf.Len())
	}
	for _, n := range c.dispatchingBodies {
		total += n
	}
	return total
}

// finishPendingBody removes and returns the reassembled request for
// requestID, ready for handleRequestTo, when stream_end names a registered
// pending body. It reports false for an unregistered requestID, so the
// caller can fall through to adapter.HandleMessage unchanged.
//
// The returned reservation is the dispatchingBodies key holding this body's
// bytes against maxStreamedRequestBodyBytes for as long as the caller keeps
// req.Body alive. Only dispatchStreamedBody may release it; a caller that
// takes a reservation and drops it on the floor leaks that many bytes of
// budget for the life of the process.
func (c *Client) finishPendingBody(requestID string) (protocol.RequestMessage, outboundTarget, uint64, bool) {
	c.pendingBodiesMu.Lock()
	pb, ok := c.pendingBodies[requestID]
	if !ok {
		c.pendingBodiesMu.Unlock()
		return protocol.RequestMessage{}, outboundTarget{}, 0, false
	}
	delete(c.pendingBodies, requestID)
	// Move the reservation from the reassembly stage to the dispatch stage
	// under the same lock as the delete above, so the aggregate sum never
	// dips between the two and no concurrent chunk can be admitted into a
	// gap. buf.Len() is the size the copy below will be, so the charge is
	// taken without holding this lock across a copy of up to
	// maxRequestBodyStream bytes. (pb.buf and its copy are both resident for
	// the length of that copy and the budget charges one, exactly as it did
	// before the dispatch stage was covered at all.)
	if c.dispatchingBodies == nil {
		c.dispatchingBodies = make(map[uint64]int64)
	}
	c.nextDispatchSeq++
	reservation := c.nextDispatchSeq
	c.dispatchingBodies[reservation] = int64(pb.buf.Len())
	timer := pb.timer
	c.pendingBodiesMu.Unlock()
	timer.Stop()

	req := pb.req
	// pb.buf.Bytes() aliases the buffer's internal array; copy it so the
	// request body survives independently of pb, which is discarded here.
	req.Body = append(json.RawMessage(nil), pb.buf.Bytes()...)
	req.BodyStream = false
	return req, pb.target, reservation, true
}

// releaseDispatchingBody drops the reservation finishPendingBody made for one
// dispatched body, once the bytes are no longer held. Deleting a map key is
// idempotent, so a double release is a no-op rather than the negative drift a
// counter decrement would leave behind.
func (c *Client) releaseDispatchingBody(reservation uint64) {
	c.pendingBodiesMu.Lock()
	delete(c.dispatchingBodies, reservation)
	c.pendingBodiesMu.Unlock()
}

// dispatchStreamedBody runs the whole post-reassembly path for one streamed
// request body and owns the maxStreamedRequestBodyBytes reservation
// finishPendingBody handed over. The release is a single defer on this
// function, which is what makes it unleakable: a streamSem rejection, a panic
// inside handleRequestTo, and a normal return all run it exactly once. That
// is why the streamSem admission check lives here rather than beside the
// inline-body one in readPump — there, the rejection branch would need its
// own release call, which is exactly the drift this shape avoids.
//
// Run it in its own goroutine: it must not run on the read pump, which has to
// keep servicing pings and exec I/O while a body is in Docker. One goroutine
// per stream_end is cheap even on the rejection branch, because the
// production send path enqueues without blocking.
func (c *Client) dispatchStreamedBody(ctx context.Context, req protocol.RequestMessage, target outboundTarget, reservation uint64) {
	defer c.releaseDispatchingBody(reservation)

	// Bound concurrent request handlers (maxStreams), the same admission
	// check an inline-body request gets in readPump. Reject rather than wait:
	// the reassembled body is already in memory, so queueing here would hold
	// it against the aggregate budget for as long as the agent stayed busy.
	select {
	case c.streamSem <- struct{}{}:
		defer func() { <-c.streamSem }()
		c.handleRequestTo(ctx, req, target)
	default:
		slog.Warn("concurrent request limit reached, rejecting", "max", maxStreams, "request_id", applog.Sanitize(req.RequestID))
		_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
			Message:   "agent busy: too many concurrent requests",
			RequestID: req.RequestID,
		})
	}
}

// failPendingBody is the idle-timeout callback for a pending body: it
// removes the entry and reports the timeout to the controller as a
// TypeError, but only when the entry filed under requestID is still the one
// this callback was armed for (want), at the generation it was armed with
// (gen).
//
// Both halves of that match are needed, and they cover different races.
//
// The entry pointer is what stops a fired callback from killing a successor.
// time.Timer.Stop does not wait for a callback that has already started, so
// one can be parked on this mutex while its entry is removed and the same
// RequestID is registered again: failAllPendingBodies then the controller's
// retry on the new connection is the reachable case, and finishPendingBody or
// appendPendingBody's cleanup paths followed by a re-use of the ID is the
// same shape within one connection. The successor is a fresh entry at gen 0,
// so requestID and gen alone match it and this callback would delete a live
// upload and time it out on the wire. Comparing the entry instead cannot
// match a successor: a successor is always a different allocation, and the
// closure holds want alive, so its address is not recycled underneath the
// comparison.
//
// The generation covers what identity cannot: the same entry re-arming its
// timer on every chunk. Reset cannot recall an AfterFunc whose deadline
// already elapsed, so a callback queued before the chunk landed still points
// at the live entry and must drop its firing.
//
// (The size-cap and decode-failure paths clean up inline in
// appendPendingBody.)
func (c *Client) failPendingBody(requestID string, want *pendingRequestBody, gen uint64) {
	c.pendingBodiesMu.Lock()
	pb, ok := c.pendingBodies[requestID]
	if !ok || pb != want || pb.gen != gen {
		c.pendingBodiesMu.Unlock()
		return
	}
	delete(c.pendingBodies, requestID)
	timer, target := pb.timer, pb.target
	c.pendingBodiesMu.Unlock()
	timer.Stop()
	_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
		Message:   fmt.Sprintf("streamed request body timed out after %s waiting for stream_end", requestBodyStreamIdleTimeout),
		RequestID: requestID,
	})
}

// composeRequestPrefix is the path standard mode's handleCompose (see
// internal/server/http.go) serves on, and the one edge mode must detect and
// route to c.compose instead of forwarding to dockerd — dockerd has no such
// route and would otherwise 404 every compose deploy (see handleComposeRequest).
const composeRequestPrefix = "/_portwing/compose"

// handleRequest executes a Docker API request locally and sends the response
// back over the WebSocket.
func (c *Client) handleRequest(ctx context.Context, req protocol.RequestMessage) {
	c.handleRequestTo(ctx, req, c.currentOutboundTarget())
}

func (c *Client) handleRequestTo(ctx context.Context, req protocol.RequestMessage, target outboundTarget) {
	if strings.HasPrefix(req.Path, composeRequestPrefix) {
		c.handleComposeRequestTo(ctx, req, target)
		return
	}

	start := time.Now()
	isStream := docker.IsStreamingRequest(req.Method, req.Path)

	var bodyReader io.Reader
	if req.Body != nil {
		bodyReader = bytes.NewReader(req.Body)
	}

	var resp *http.Response
	var err error
	requestHeaders := allowedDockerRequestHeaders(req.Headers)

	if isStream {
		resp, err = c.dockerClient.DoStreamWithHeaders(ctx, req.Method, req.Path, requestHeaders, bodyReader)
	} else {
		resp, err = c.dockerClient.DoWithHeaders(ctx, req.Method, req.Path, requestHeaders, bodyReader)
	}

	if err != nil {
		c.auditor.APIRequest(c.cfg.DrydockURL, req.Method, req.Path, audit.OutcomeError, 0, msEdge(start))
		// Best-effort error reply; connection loss will surface on the read pump.
		_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
			Message:   err.Error(),
			RequestID: req.RequestID,
		})
		return
	}
	defer resp.Body.Close()
	c.auditor.APIRequest(c.cfg.DrydockURL, req.Method, req.Path, audit.OutcomeAllowed, resp.StatusCode, msEdge(start))

	// Build response headers.
	headers := make(map[string]string)
	for key := range resp.Header {
		headers[key] = resp.Header.Get(key)
	}

	if isStream {
		// Send initial response header; best-effort — connection loss surfaces on the read pump.
		_ = c.sendTypedMessageTo(target, protocol.TypeResponse, protocol.ResponseMessage{
			RequestID:   req.RequestID,
			StatusCode:  resp.StatusCode,
			Headers:     headers,
			IsStream:    true,
			ContentType: resp.Header.Get("Content-Type"),
		})

		// Stream body in chunks using a pooled 32 KiB buffer so the per-request
		// stream buffer is reused instead of freshly allocated each time.
		buf := pool.GetStreamBuffer()
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				encoded := base64.StdEncoding.EncodeToString(buf[:n])
				_ = c.sendTypedMessageTo(target, protocol.TypeStream, protocol.StreamMessage{
					RequestID: req.RequestID,
					Data:      encoded,
				})
			}
			if readErr != nil {
				break
			}
		}
		pool.PutStreamBuffer(buf)

		_ = c.sendTypedMessageTo(target, protocol.TypeStreamEnd, protocol.StreamEndMessage{
			RequestID: req.RequestID,
			Reason:    "complete",
		})
	} else {
		// Read body (capped).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))

		respMsg := protocol.ResponseMessage{
			RequestID:   req.RequestID,
			StatusCode:  resp.StatusCode,
			Headers:     headers,
			ContentType: resp.Header.Get("Content-Type"),
		}

		// Negotiated via welcome.capabilities (see connect/hasControllerCap),
		// not a ProtocolVersion bump: bumping the version is a terminal,
		// hard mismatch that permanently breaks any agent/controller pairing
		// that disagrees on it, whereas this capability token degrades
		// gracefully — an old controller's welcome simply lacks the token,
		// and we fall through to the legacy path unchanged.
		if c.hasControllerCap(protocol.CapResponseBodyBase64) {
			// The controller understands bodyBase64, so send the raw response
			// bytes as standard base64 regardless of whether they're valid
			// JSON. This is what lets a non-JSON body (e.g. the plain-text
			// "OK" from GET /_ping) cross the wire at all — leave legacy Body
			// nil so the drydock decoder's bodyBase64-first check picks this
			// branch instead of the legacy json.RawMessage one.
			respMsg.BodyBase64 = base64.StdEncoding.EncodeToString(body)
		} else {
			// Legacy path, unchanged: ResponseMessage.Body is a
			// json.RawMessage, so a body that isn't valid JSON (e.g. the
			// plain-text "OK" from GET /_ping) fails this marshal. Dropping
			// that error, as this used to, left the controller waiting
			// forever for a response envelope that would never arrive.
			// Surface the failure as an error envelope (PR #201) so the
			// controller gets a definitive (if unhelpful) response instead
			// of hanging, since an unnegotiated connection has no other way
			// to carry a non-JSON body.
			respMsg.Body = json.RawMessage(body)
		}

		if err := c.sendTypedMessageTo(target, protocol.TypeResponse, respMsg); err != nil {
			slog.Warn("failed to encode Docker response envelope", "requestId", applog.Sanitize(req.RequestID), "path", applog.Sanitize(req.Path), "error", err)
			// Best-effort error reply; connection loss will surface on the read pump.
			_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
				Message:   fmt.Sprintf("encoding response: %v", err),
				RequestID: req.RequestID,
			})
		}
	}
}

// handleComposeRequest decodes and executes a compose request against the
// ComposeManager the client already holds, mirroring standard mode's
// handleCompose (internal/server/http.go). Edge mode advertises the
// "compose" capability, so a request on composeRequestPrefix must reach the
// compose manager rather than fall through to the dockerd proxy above, which
// has no such route and would 404.
func (c *Client) handleComposeRequest(ctx context.Context, req protocol.RequestMessage) {
	c.handleComposeRequestTo(ctx, req, c.currentOutboundTarget())
}

func (c *Client) handleComposeRequestTo(ctx context.Context, req protocol.RequestMessage, target outboundTarget) {
	var composeReq docker.ComposeRequest
	if err := json.Unmarshal(req.Body, &composeReq); err != nil {
		_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
			Message:   fmt.Sprintf("invalid compose request: %v", err),
			RequestID: req.RequestID,
		})
		return
	}

	resp, err := c.compose.Execute(ctx, composeReq)
	if err != nil {
		c.auditor.ComposeOp(c.cfg.DrydockURL, composeReq.Operation, composeReq.StackName, audit.OutcomeError)
		_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
			Message:   err.Error(),
			RequestID: req.RequestID,
		})
		return
	}

	outcome := audit.OutcomeAllowed
	if !resp.Success {
		outcome = audit.OutcomeError
	}
	c.auditor.ComposeOp(c.cfg.DrydockURL, composeReq.Operation, composeReq.StackName, outcome)

	body, err := json.Marshal(resp)
	if err != nil {
		// ComposeResponse is a plain struct of strings/bools and always
		// marshals cleanly; this guards against a future field change
		// introducing something that doesn't, surfacing it as an error
		// envelope instead of silently dropping the response.
		_ = c.sendTypedMessageTo(target, protocol.TypeError, protocol.ErrorMessage{
			Message:   fmt.Sprintf("encoding compose response: %v", err),
			RequestID: req.RequestID,
		})
		return
	}

	_ = c.sendTypedMessageTo(target, protocol.TypeResponse, protocol.ResponseMessage{
		RequestID:   req.RequestID,
		StatusCode:  http.StatusOK,
		Body:        json.RawMessage(body),
		ContentType: "application/json",
	})
}

// allowedDockerRequestHeaders forwards only Docker API metadata needed by the
// controller-owned transport. Controller/session authentication and hop-by-hop
// headers must never cross the Portwing-to-dockerd trust boundary.
func allowedDockerRequestHeaders(headers map[string]string) http.Header {
	allowed := http.Header{}
	for name, value := range headers {
		if strings.ContainsAny(value, "\r\n") || len(value) > 64*1024 {
			continue
		}
		switch strings.ToLower(name) {
		case "accept":
			allowed.Set("Accept", value)
		case "content-type":
			allowed.Set("Content-Type", value)
		case "x-registry-auth":
			allowed.Set("X-Registry-Auth", value)
		case "x-registry-config":
			allowed.Set("X-Registry-Config", value)
		}
	}
	return allowed
}

// writePump handles periodic outgoing messages: metrics, container refreshes,
// and keepalive pings.
func (c *Client) writePump(ctx context.Context) {
	heartbeat := time.Duration(c.cfg.HeartbeatInterval) * time.Second

	pollInterval := c.adapter.PollInterval()
	if pollInterval <= 0 {
		pollInterval = c.cfg.DDPollInterval
	}
	// Override with the poll interval from the welcome frame when the controller
	// supplies one; it takes precedence over both the adapter and config defaults.
	if c.welcomePollInterval > 0 {
		pollInterval = c.welcomePollInterval
	}
	pollDuration := time.Duration(pollInterval) * time.Second

	heartbeatTicker := time.NewTicker(heartbeat)
	defer heartbeatTicker.Stop()

	pollTicker := time.NewTicker(pollDuration)
	defer pollTicker.Stop()

	sender := c.currentMessageSender()

	for {
		select {
		case <-ctx.Done():
			return

		case <-heartbeatTicker.C:
			// Send metrics.
			c.sendMetrics()

			// Send keepalive ping; best-effort — connection loss surfaces on the read pump.
			_ = c.sendTypedMessage(protocol.TypePing, protocol.PingMessage{
				Timestamp: time.Now().UnixMilli(),
			})

		case <-pollTicker.C:
			// Refresh container inventory via adapter.
			added, updated, removed, err := c.adapter.RefreshContainers(ctx)
			if err != nil {
				slog.Warn("container refresh failed", "error", err)
				continue
			}
			if err := c.adapter.OnContainerRefresh(ctx, sender, added, updated, removed); err != nil {
				slog.Warn("container refresh notify failed", "error", err)
			}
		}
	}
}

// metricsUnavailableCode is the error frame's `code` when host metrics could
// not be collected — the wire's marker for "this agent is alive and cannot
// report these numbers", as distinct from an agent that has gone quiet.
const metricsUnavailableCode = "host-metrics-unavailable"

// sendMetrics collects and sends host metrics, and reports a failed collection
// instead of swallowing it.
//
// Collect hands back a partially populated snapshot alongside its error, and
// forwarding that is exactly what ErrHostMetricsUnsupported exists to prevent:
// a zero-filled metrics frame is indistinguishable from a real reading of a
// completely idle host. The other two surfaces both answer a failed collection
// with an explicit marker rather than zeros — Prometheus omits the host series
// behind `portwing_host_metrics_supported 0`, and the MCP host_metrics tool
// returns an error naming the missing procfs — so this one answers with the
// wire's own error frame (SPEC 3.3, 8), carrying metricsUnavailableCode.
// Staying silent, which is what it used to do, left the controller unable to
// tell an unsupported host from a dead agent.
//
// The frame keeps the metrics cadence: one per heartbeat, same as the metrics
// frame it stands in for, so the signal is periodic rather than one-shot. The
// log does not: the level marks the transition, because the failure is
// permanent on a host with no procfs and warning every 30 seconds forever
// would bury the tick that actually changed something. First failure (at
// startup, or the first after a recovery) warns, repeats while failed drop to
// debug, and a recovery says so once at info. Swap reports the previous state,
// so exactly one call logs each transition.
func (c *Client) sendMetrics() {
	m, err := c.collector.Collect()
	if err != nil {
		if c.metricsCollectFailing.Swap(true) {
			slog.Debug("metrics collection failed", "error", err)
		} else {
			slog.Warn("metrics collection failed", "error", err)
		}
		// Best-effort, like the metrics send below. Sent on every failed tick
		// whatever the log level, so the controller's signal stays periodic.
		_ = c.sendTypedMessage(protocol.TypeError, protocol.ErrorMessage{
			Message: err.Error(),
			Code:    metricsUnavailableCode,
		})
		return
	}
	if c.metricsCollectFailing.Swap(false) {
		slog.Info("metrics collection recovered")
	}
	// Best-effort metrics send; connection loss surfaces on the read pump.
	_ = c.sendTypedMessage(protocol.TypeMetrics, m)
}

// sendTypedMessage wraps data in an Envelope and sends it over the WebSocket.
func (c *Client) sendTypedMessage(msgType string, data any) error {
	return c.sendTypedMessageTo(c.currentOutboundTarget(), msgType, data)
}

func (c *Client) sendTypedMessageTo(target outboundTarget, msgType string, data any) error {
	rawData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", msgType, err)
	}

	env := protocol.Envelope{
		Type: msgType,
		Data: json.RawMessage(rawData),
	}

	c.sendMessageTo(target, env)
	return nil
}

// sendMessage hands an envelope to the sendPump. The enqueue is non-blocking:
// sendMessage runs on the read-pump goroutine for pongs and rejections, so it
// must never block. A full queue means the controller can't keep up — the
// connection is evicted (and Run reconnects) rather than dropping frames, which
// would hang a request or corrupt a stream.
//
// Before the send path is up (the hello/welcome handshake), sendCh is nil and
// the frame is written directly — no concurrent writer exists yet.
func (c *Client) sendMessage(env protocol.Envelope) {
	c.sendMessageTo(c.currentOutboundTarget(), env)
}

func (c *Client) currentOutboundTarget() outboundTarget {
	c.connMu.Lock()
	ch := c.sendCh
	conn := c.conn
	state := c.sendState
	if ch != nil && state == nil {
		// Tests and zero-value clients may install a queue directly. Production
		// connections publish the queue and its state together in connect.
		state = &outboundQueueState{}
		c.sendState = state
	}
	c.connMu.Unlock()
	return outboundTarget{conn: conn, ch: ch, state: state}
}

func (c *Client) sendMessageTo(target outboundTarget, env protocol.Envelope) {
	if target.ch == nil {
		// Handshake phase: synchronous direct write, provably single-writer.
		if target.conn == nil {
			return
		}
		_ = target.conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := target.conn.WriteJSON(env); err != nil {
			slog.Warn("websocket write failed", "type", env.Type, "error", err)
		}
		return
	}

	switch target.state.enqueue(target.ch, env) {
	case outboundEnqueued:
		return
	case outboundQueueClosed:
		c.failConn(target.conn, "send queue closed")
	case outboundByteLimitExceeded:
		if c.metrics != nil {
			c.metrics.IncBackpressure()
		}
		c.failConn(target.conn, "send queue byte limit exceeded")
	case outboundFrameLimitExceeded:
		if c.metrics != nil {
			c.metrics.IncBackpressure()
		}
		c.failConn(target.conn, "send queue full")
	}
}

func (s *outboundQueueState) enqueue(ch chan protocol.Envelope, env protocol.Envelope) outboundEnqueueResult {
	size := outboundEnvelopeBytes(env)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return outboundQueueClosed
	}
	if size > maxOutboundQueuedBytes-s.bytes {
		return outboundByteLimitExceeded
	}

	s.bytes += size
	select {
	case ch <- env:
		return outboundEnqueued
	default:
		s.bytes -= size
		return outboundFrameLimitExceeded
	}
}

func (s *outboundQueueState) release(env protocol.Envelope) {
	s.mu.Lock()
	s.bytes -= outboundEnvelopeBytes(env)
	if s.bytes < 0 {
		// Tests and the handshake may inject unreserved envelopes directly. The
		// production queue path always reserves through enqueue.
		s.bytes = 0
	}
	s.mu.Unlock()
}

func (s *outboundQueueState) closeAndDiscard(ch chan protocol.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for {
		select {
		case env := <-ch:
			s.bytes -= outboundEnvelopeBytes(env)
			if s.bytes < 0 {
				s.bytes = 0
			}
		default:
			return
		}
	}
}

func outboundEnvelopeBytes(env protocol.Envelope) int64 {
	return int64(len(env.Type) + len(env.Data))
}

// sendPump is the sole writer to the WebSocket once a connection is up.
// Fronting every send with one goroutine and a bounded queue is the tunnel's
// outbound backpressure: a slow controller backs up sendCh instead of
// head-of-line-blocking every sender or stalling the read pump, and a write
// that can't complete within writeWait evicts the connection rather than
// blocking forever.
//
// It takes two contexts because they answer different questions. ctx is the
// per-connection pump context, and cancelling it is how a dropped tunnel
// stops this pump. shutdownCtx is Run's own context, and its cancellation is
// the agent exiting. Only the outer one can classify a write failure, and it
// has to be the outer one: connect closes the conn from a context.AfterFunc
// on cancel, and that callback runs on its own goroutine, so a write can fail
// against the closed conn before the pump context has been cancelled. The
// outer context's error is already set before any child cancellation starts,
// which makes the check below race-free where a check on ctx would not be.
func (c *Client) sendPump(ctx, shutdownCtx context.Context, conn *websocket.Conn, sendCh chan protocol.Envelope) {
	c.connMu.Lock()
	state := c.sendState
	if c.sendCh != sendCh {
		state = &outboundQueueState{closed: true}
	} else if state == nil {
		// Tests may install sendCh directly instead of going through connect.
		state = &outboundQueueState{}
		c.sendState = state
	}
	c.connMu.Unlock()
	defer state.closeAndDiscard(sendCh)

	for {
		select {
		case <-ctx.Done():
			return
		case env := <-sendCh:
			if err := conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				state.release(env)
				c.failConn(conn, "set write deadline failed")
				return
			}
			if err := conn.WriteJSON(env); err != nil {
				state.release(env)
				if shutdownCtx.Err() != nil {
					// Shutdown closed the conn out from under this write.
					// Nothing is wrong and there is no generation left to
					// evict, so this neither warns nor calls failConn.
					slog.Debug("websocket write abandoned, shutting down", "type", env.Type, "error", err)
					return
				}
				slog.Warn("websocket write failed", "type", env.Type, "error", err)
				c.failConn(conn, "write failed")
				return
			}
			state.release(env)
		}
	}
}

// failConn evicts the WebSocket generation that encountered backpressure or a
// write failure. Closing that exact connection avoids a delayed sender from an
// old queue evicting a replacement connection after reconnect.
func (c *Client) failConn(conn *websocket.Conn, reason string) {
	if conn != nil {
		slog.Warn("evicting controller connection", "reason", reason)
		closeWebSocket(conn, reason)
	}
}

// closeAllExecSessions tears down every live exec session. Called when a
// controller connection ends so sessions don't leak across reconnects: each
// Close() also deregisters the session from execSessions, which is safe to do
// while ranging a sync.Map.
func (c *Client) closeAllExecSessions() {
	c.execSessions.Range(func(_, v any) bool {
		if s, ok := v.(*ExecSession); ok {
			s.Close()
		}
		return true
	})
}

// failAllPendingBodies drops every streamed request body still reassembling
// and releases its RequestID. It is closeAllExecSessions' sibling on the same
// teardown path: both own per-connection state that the next connection must
// not inherit.
//
// Without it a dropped tunnel leaves the entries registered with their idle
// timers running, so the controller's retry of the same RequestID after
// reconnect is answered with a duplicate-requestId rejection until
// requestBodyStreamIdleTimeout expires — up to 30 seconds of a request that
// can never succeed, on a connection that is otherwise healthy.
//
// Nothing goes on the wire. sendPump has already returned by the time teardown
// runs, so its queue reports closed and every send would take the eviction
// branch instead: a spurious "evicting controller connection" warning and a
// second Close on the conn that connect is about to close anyway, per pending
// body. closeAllExecSessions is silent for the same reason.
//
// Swapping the map out under the lock before touching any timer is what makes
// this safe against a timeout callback racing it: failPendingBody looks the
// entry up under the same lock, finds nothing, and returns without sending.
// Stopping an already-fired timer is a no-op, so the two cannot double-fail
// one request.
//
// The swap is not enough on its own once the controller reconnects, because
// by then the table is not empty: the retry has registered a fresh entry
// under the same RequestID, at the same gen 0 the drained one was armed at.
// What keeps a callback parked since the old connection from deleting it is
// that failPendingBody matches on the entry it was armed for, not on the
// RequestID.
func (c *Client) failAllPendingBodies() {
	c.pendingBodiesMu.Lock()
	dropped := c.pendingBodies
	c.pendingBodies = nil
	c.pendingBodiesMu.Unlock()

	if len(dropped) == 0 {
		return
	}
	for requestID, pb := range dropped {
		pb.timer.Stop()
		slog.Debug("dropping streamed request body: controller connection lost",
			"request_id", applog.Sanitize(requestID))
	}
	slog.Warn("controller connection lost with streamed request bodies in flight",
		"count", len(dropped))
}

// msEdge returns elapsed milliseconds since start as a float64.
func msEdge(start time.Time) float64 {
	return float64(time.Since(start).Nanoseconds()) / 1e6
}

// readDeadline returns the steady-state WebSocket read deadline:
// max(2 * HEARTBEAT_INTERVAL, 60s). Exceeding it means pings have gone
// unanswered, so the connection is treated as dead (SPEC §13.2).
func readDeadline(heartbeatSeconds int) time.Duration {
	d := 2 * time.Duration(heartbeatSeconds) * time.Second
	if d < 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

func jitteredDuration(delay time.Duration) time.Duration {
	const (
		minMillis = 750
		span      = 500
	)
	n, err := crand.Int(crand.Reader, big.NewInt(span+1))
	if err != nil {
		slog.Warn("generating reconnect jitter", "error", err)
		return delay
	}
	factorMillis := minMillis + n.Int64()
	return time.Duration((int64(delay) * factorMillis) / 1000)
}

func closeWebSocket(conn *websocket.Conn, context string) {
	if err := conn.Close(); err != nil {
		slog.Debug("closing websocket", "context", context, "error", err)
	}
}

// startHealthServer starts the local liveness, readiness, and operational
// metrics server used by Docker, Kubernetes, and Prometheus.
func (c *Client) startHealthServer() {
	c.ensureOperationalState()
	// Every call starts unbound. BaseContext below only fires once the
	// listener is up, so without this a restart that cannot bind (the port
	// taken in the gap, say) would leave the previous server's address in
	// place and HealthAddr would hand out an address nothing is serving.
	c.healthListenAddr.Store(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.HealthResponse{
			Status:        "ok",
			Live:          true,
			Ready:         false,
			Mode:          "edge",
			Version:       protocol.AgentVersion,
			UptimeSeconds: time.Since(c.startTime).Seconds(),
			Docker:        "unknown",
			Controller:    currentControllerState(c.metrics.ControllerConnected()),
		})
	})
	readiness := func(w http.ResponseWriter, r *http.Request) {
		dockerConnected := c.dockerReady(r.Context())
		controllerConnected := c.metrics.ControllerConnected()
		status := "healthy"
		httpStatus := http.StatusOK
		if !dockerConnected || !controllerConnected {
			status = "unhealthy"
			httpStatus = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		_ = json.NewEncoder(w).Encode(protocol.HealthResponse{
			Status:        status,
			Live:          true,
			Ready:         dockerConnected && controllerConnected,
			Mode:          "edge",
			Version:       protocol.AgentVersion,
			UptimeSeconds: time.Since(c.startTime).Seconds(),
			Docker:        currentDockerState(dockerConnected),
			Controller:    currentControllerState(controllerConnected),
		})
	}
	mux.HandleFunc("GET /ready", readiness)
	mux.HandleFunc("GET /_portwing/health", readiness)
	mux.HandleFunc("GET /_portwing/audit/export", func(w http.ResponseWriter, r *http.Request) {
		audit.ServeExportHTTP(w, r, c.auditor, c.metrics)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if c.auditor != nil {
			stats := c.auditor.Stats()
			c.metrics.SetAuditState(stats.Records, stats.Capacity, stats.SinkEnabled)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# HELP portwing_build_info Portwing agent build metadata.\n")
		fmt.Fprintf(&b, "# TYPE portwing_build_info gauge\n")
		fmt.Fprintf(&b, "portwing_build_info{version=\"%s\"} 1\n", metrics.EscapeLabelValue(protocol.AgentVersion))
		fmt.Fprintf(&b, "# HELP portwing_uptime_seconds Seconds since the agent started.\n")
		fmt.Fprintf(&b, "# TYPE portwing_uptime_seconds gauge\n")
		fmt.Fprintf(&b, "portwing_uptime_seconds %g\n", time.Since(c.startTime).Seconds())
		// The host series need the real collector's concrete type, which
		// production always holds. A test-injected fake leaves them out, the
		// same absence a nil collector produced before.
		if hostCol, ok := c.collector.(*metrics.Collector); ok {
			metrics.WriteHostPrometheus(&b, hostCol)
		}
		if dockerMetrics, ok := c.dockerClient.(metrics.DockerMetricsClient); ok {
			metrics.WriteContainerPrometheus(r.Context(), &b, dockerMetrics, metrics.EscapeLabelValue)
		}
		c.metrics.WritePrometheus(&b, metrics.EscapeLabelValue)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, b.String())
	})
	c.healthServer = &http.Server{
		Addr:              c.cfg.BindAddress + ":" + c.cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// BaseContext runs once, right after the listener binds, which is
		// the only hook stdlib gives us to learn the bound address without
		// owning the listener ourselves.
		BaseContext: func(ln net.Listener) context.Context {
			addr := ln.Addr()
			c.healthListenAddr.Store(&addr)
			return context.Background()
		},
	}

	done := make(chan struct{})
	c.healthServerDone = done
	go func() {
		if err := c.healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Warn("health server error", "error", err)
		}
		close(done)
	}()
}

// HealthAddr returns the address the health server bound, or nil if it
// hasn't bound one yet (or startHealthServer hasn't been called). It lets
// callers — chiefly tests using an OS-assigned port ("0") — discover the
// real bound address instead of racing a separate allocate/close/rebind.
func (c *Client) HealthAddr() net.Addr {
	if addr := c.healthListenAddr.Load(); addr != nil {
		return *addr
	}
	return nil
}

func (c *Client) ensureOperationalState() {
	if c.metrics == nil {
		c.metrics = metrics.NewRegistry()
		c.metrics.SetEdgeMode(true)
	}
	if c.startTime.IsZero() {
		c.startTime = time.Now()
	}
	if c.streamSem == nil {
		c.streamSem = make(chan struct{}, maxStreams)
	}
}

func (c *Client) dockerReady(ctx context.Context) bool {
	if c.dockerClient == nil {
		return false
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	response, err := c.dockerClient.Do(pingCtx, http.MethodGet, "/_ping", nil)
	if err != nil || response == nil {
		return false
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	return response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
}

func currentDockerState(connected bool) string {
	if connected {
		return "connected"
	}
	return "disconnected"
}

func currentControllerState(connected bool) string {
	if connected {
		return "connected"
	}
	return "disconnected"
}
