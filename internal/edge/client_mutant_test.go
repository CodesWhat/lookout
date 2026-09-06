package edge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/codeswhat/portwing/internal/config"
	"github.com/codeswhat/portwing/internal/protocol"
)

// syncBuffer is a mutex-guarded bytes.Buffer, used as a slog sink in tests
// that swap in a capturing logger while a readPump/writePump goroutine is
// still running. A plain *bytes.Buffer is unsafe here: the pump goroutine's
// Write and the test goroutine's later String() race on the same memory with
// no happens-before edge between them (a websocket round trip isn't a Go
// memory-model synchronization point), which -race correctly flags even
// though the wire ordering happens to be deterministic.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------------------------------------------------------------------------
// Package-level constant definitions (client.go:45,46,58,72,95,103)
// ---------------------------------------------------------------------------

// TestPackageConstantValues pins the exact computed value of every size/
// duration constant derived from an arithmetic expression. Every use site in
// the package reads these through their symbolic names, so a mutation to the
// arithmetic in the definition (e.g. `*` -> `/`) changes the value everywhere
// consistently and is invisible to any test that only exercises behavior
// through the name. Hardcoding the expected literal here is the only way to
// pin the definition itself.
func TestPackageConstantValues(t *testing.T) {
	t.Parallel()

	if maxReadSize != 16*1024*1024 {
		t.Errorf("maxReadSize = %d, want %d", maxReadSize, 16*1024*1024)
	}
	if maxResponseBody != 100*1024*1024 {
		t.Errorf("maxResponseBody = %d, want %d", maxResponseBody, 100*1024*1024)
	}
	if writeWait != 10*time.Second {
		t.Errorf("writeWait = %v, want %v", writeWait, 10*time.Second)
	}
	if maxRequestBodyStream != 512*1024*1024 {
		t.Errorf("maxRequestBodyStream = %d, want %d", maxRequestBodyStream, 512*1024*1024)
	}
	if maxStreamedRequestBodyBytes != 1024*1024*1024 {
		t.Errorf("maxStreamedRequestBodyBytes = %d, want %d", maxStreamedRequestBodyBytes, 1024*1024*1024)
	}
	if requestBodyStreamIdleTimeout != 30*time.Second {
		t.Errorf("requestBodyStreamIdleTimeout = %v, want %v", requestBodyStreamIdleTimeout, 30*time.Second)
	}
}

// ---------------------------------------------------------------------------
// appendPendingBody — per-request and aggregate overflow boundaries
// (client.go:927, 929)
// ---------------------------------------------------------------------------

// TestAppendPendingBodyPerRequestCapBoundary proves the exact boundary of the
// per-request maxRequestBodyStream check: a chunk that lands exactly at the
// cap is accepted, and one byte over is rejected. This distinguishes `>` from
// `>=` (CONDITIONALS_BOUNDARY), `>` from `<=` (CONDITIONALS_NEGATION), and `+`
// from `-` in the byte-count arithmetic (ARITHMETIC_BASE): any of those
// mutations flips the accept/reject decision at this exact boundary.
func TestAppendPendingBodyPerRequestCapBoundary(t *testing.T) {
	// Not t.Parallel(): mutates the package-level maxRequestBodyStream var.
	orig := maxRequestBodyStream
	maxRequestBodyStream = 10
	t.Cleanup(func() { maxRequestBodyStream = orig })

	fa := &fakeAdapter{handleMsgResult: true}
	c, ctrl := newTestClient(t)
	c.adapter = fa
	//nolint:bodyclose // consumed and closed by handleRequestTo, the code under test.
	fd := &fakeDocker{doResp: mkResp(http.StatusCreated, "application/json", `{"ok":true}`)}
	c.dockerClient = fd

	runReadPump(t, c)

	// Exactly at the cap (10 bytes): must be accepted and dispatched.
	sendEnvelope(t, ctrl, protocol.TypeRequest, protocol.RequestMessage{
		RequestID:  "at-cap",
		Method:     http.MethodPost,
		Path:       "/containers/create",
		BodyStream: true,
	})
	sendEnvelope(t, ctrl, protocol.TypeStream, protocol.StreamMessage{
		RequestID: "at-cap",
		Data:      base64.StdEncoding.EncodeToString([]byte("0123456789")), // 10 bytes
	})
	sendEnvelope(t, ctrl, protocol.TypeStreamEnd, protocol.StreamEndMessage{RequestID: "at-cap", Reason: "complete"})

	var resp protocol.ResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &resp)
	if resp.RequestID != "at-cap" {
		t.Fatalf("RequestID = %q, want at-cap (the exact-cap body was rejected)", resp.RequestID)
	}

	// One byte over the cap: must be rejected with a TypeError.
	sendEnvelope(t, ctrl, protocol.TypeRequest, protocol.RequestMessage{
		RequestID:  "over-cap",
		Method:     http.MethodPost,
		Path:       "/containers/create",
		BodyStream: true,
	})
	sendEnvelope(t, ctrl, protocol.TypeStream, protocol.StreamMessage{
		RequestID: "over-cap",
		Data:      base64.StdEncoding.EncodeToString([]byte("01234567890")), // 11 bytes
	})

	var em protocol.ErrorMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeError), &em)
	if em.RequestID != "over-cap" {
		t.Fatalf("error RequestID = %q, want over-cap (the over-cap body was accepted)", em.RequestID)
	}
}

// TestAppendPendingBodyAggregateCapBoundary proves the exact boundary of the
// aggregate maxStreamedRequestBodyBytes check the same way as
// TestAppendPendingBodyPerRequestCapBoundary, for the second `case` in
// appendPendingBody's overflow switch.
func TestAppendPendingBodyAggregateCapBoundary(t *testing.T) {
	// Not t.Parallel(): mutates the package-level maxStreamedRequestBodyBytes
	// var. maxRequestBodyStream stays at its real 512 MB so the per-request
	// cap never fires first.
	orig := maxStreamedRequestBodyBytes
	maxStreamedRequestBodyBytes = 10
	t.Cleanup(func() { maxStreamedRequestBodyBytes = orig })

	fa := &fakeAdapter{handleMsgResult: true}
	c, ctrl := newTestClient(t)
	c.adapter = fa
	//nolint:bodyclose // consumed and closed by handleRequestTo, the code under test.
	fd := &fakeDocker{doResp: mkResp(http.StatusCreated, "application/json", `{"ok":true}`)}
	c.dockerClient = fd

	runReadPump(t, c)

	// Exactly at the aggregate cap: must be accepted.
	sendEnvelope(t, ctrl, protocol.TypeRequest, protocol.RequestMessage{
		RequestID:  "agg-at-cap",
		Method:     http.MethodPost,
		Path:       "/containers/create",
		BodyStream: true,
	})
	sendEnvelope(t, ctrl, protocol.TypeStream, protocol.StreamMessage{
		RequestID: "agg-at-cap",
		Data:      base64.StdEncoding.EncodeToString([]byte("0123456789")), // 10 bytes
	})
	sendEnvelope(t, ctrl, protocol.TypeStreamEnd, protocol.StreamEndMessage{RequestID: "agg-at-cap", Reason: "complete"})

	var resp protocol.ResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &resp)
	if resp.RequestID != "agg-at-cap" {
		t.Fatalf("RequestID = %q, want agg-at-cap (the exact-cap body was rejected)", resp.RequestID)
	}

	// One byte over: must be rejected.
	sendEnvelope(t, ctrl, protocol.TypeRequest, protocol.RequestMessage{
		RequestID:  "agg-over-cap",
		Method:     http.MethodPost,
		Path:       "/containers/create",
		BodyStream: true,
	})
	sendEnvelope(t, ctrl, protocol.TypeStream, protocol.StreamMessage{
		RequestID: "agg-over-cap",
		Data:      base64.StdEncoding.EncodeToString([]byte("01234567890")), // 11 bytes
	})

	var em protocol.ErrorMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeError), &em)
	if em.RequestID != "agg-over-cap" {
		t.Fatalf("error RequestID = %q, want agg-over-cap (the over-cap body was accepted)", em.RequestID)
	}
}

// ---------------------------------------------------------------------------
// finishPendingBody — reservation sequence (client.go:1023)
// ---------------------------------------------------------------------------

// TestFinishPendingBodyReservationIncrementsSequence proves nextDispatchSeq
// increments (not decrements) across successive calls. nextDispatchSeq is a
// uint64 starting at zero, so INCREMENT_DECREMENT (++  -> --) would underflow
// the first reservation to the maximum uint64 value instead of 1.
func TestFinishPendingBodyReservationIncrementsSequence(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)
	c.pendingBodies = map[string]*pendingRequestBody{
		"a": {req: protocol.RequestMessage{RequestID: "a"}, timer: time.NewTimer(time.Hour)},
		"b": {req: protocol.RequestMessage{RequestID: "b"}, timer: time.NewTimer(time.Hour)},
	}
	t.Cleanup(func() {
		c.pendingBodiesMu.Lock()
		for _, pb := range c.pendingBodies {
			pb.timer.Stop()
		}
		c.pendingBodiesMu.Unlock()
	})

	_, _, res1, ok := c.finishPendingBody("a")
	if !ok || res1 != 1 {
		t.Fatalf("first reservation = %d, ok=%v, want 1, true", res1, ok)
	}
	_, _, res2, ok := c.finishPendingBody("b")
	if !ok || res2 != 2 {
		t.Fatalf("second reservation = %d, ok=%v, want 2, true", res2, ok)
	}
}

// ---------------------------------------------------------------------------
// connect — welcome.PollInterval boundary at zero (client.go:458)
// ---------------------------------------------------------------------------

// TestConnectWelcomePollIntervalZeroDoesNotResetExisting covers the exact
// boundary of `welcome.PollInterval > 0`: with an existing non-zero
// c.welcomePollInterval and a welcome carrying PollInterval == 0, the field
// must be left untouched. CONDITIONALS_BOUNDARY (`>` -> `>=`) would make
// `0 >= 0` true and incorrectly reset it to 0.
func TestConnectWelcomePollIntervalZeroDoesNotResetExisting(t *testing.T) {
	t.Parallel()

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		readAndAckHello(t, ctrl)
		sendWelcomeMsg(t, ctrl, protocol.WelcomeMessage{PollInterval: 0})
		// Close immediately: the pumps finish as soon as the read errors,
		// instead of idling until a hardcoded server-side deadline.
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		DDPollInterval:    300,
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)
	c.welcomePollInterval = 77 // sentinel: must survive a PollInterval==0 welcome

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	established, _ := c.connect(ctx)
	if !established {
		t.Fatal("established = false, want true")
	}
	if c.welcomePollInterval != 77 {
		t.Errorf("welcomePollInterval = %d, want 77 (unchanged by a zero welcome value)", c.welcomePollInterval)
	}
}

// ---------------------------------------------------------------------------
// connect — controller compat mismatch is logged (client.go:470)
// ---------------------------------------------------------------------------

// TestConnectWelcomeCompatMismatchLogsWarning covers the CONDITIONALS_NEGATION
// on `serverMajor != agentMajor`: the mismatch branch's only observable effect
// is the warning log, so a mutation that inverts the comparison silently drops
// it (established stays true either way).
func TestConnectWelcomeCompatMismatchLogsWarning(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		readAndAckHello(t, ctrl)
		sendWelcomeMsg(t, ctrl, protocol.WelcomeMessage{
			Config: map[string]string{"serverCompatLevel": "99.0"},
		})
		// Close immediately: the pumps finish as soon as the read errors,
		// instead of idling until a hardcoded server-side deadline.
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		DDPollInterval:    300,
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	logBuf := &bytes.Buffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	established, _ := c.connect(ctx)
	if !established {
		t.Fatal("established = false, want true")
	}
	if !strings.Contains(logBuf.String(), "controller compat level mismatch") {
		t.Errorf("log = %q, want the compat mismatch warning", logBuf.String())
	}
}

// ---------------------------------------------------------------------------
// connect — adapter.OnConnect failure is logged (client.go:514)
// ---------------------------------------------------------------------------

// TestConnectAdapterOnConnectFailureLogsWarning covers the CONDITIONALS_
// NEGATION on OnConnect's error check the same way: established is true
// either way, so the log is the only observable signal.
func TestConnectAdapterOnConnectFailureLogsWarning(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		readAndAckHello(t, ctrl)
		sendWelcomeMsg(t, ctrl, protocol.WelcomeMessage{})
		// Close immediately: the pumps finish as soon as the read errors,
		// instead of idling until a hardcoded server-side deadline.
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		DDPollInterval:    300,
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)
	c.adapter = &fakeAdapter{onConnectErr: errors.New("sync failed"), caps: []string{}}

	logBuf := &bytes.Buffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	established, _ := c.connect(ctx)
	if !established {
		t.Fatal("established = false, want true even when OnConnect errors")
	}
	if !strings.Contains(logBuf.String(), "adapter OnConnect failed") {
		t.Errorf("log = %q, want the OnConnect failure warning", logBuf.String())
	}
}

// ---------------------------------------------------------------------------
// connect — c.conn cleared after the pumps finish (client.go:545)
// ---------------------------------------------------------------------------

// TestConnectClearsConnAfterPumpsFinish covers the CONDITIONALS_NEGATION on
// `c.conn != nil` in connect's post-pump cleanup: c.conn is always non-nil at
// that point in the real flow, so `!= nil` -> `== nil` always skips clearing
// it, leaving a stale reference to an already-torn-down connection.
func TestConnectClearsConnAfterPumpsFinish(t *testing.T) {
	t.Parallel()

	srv := newControllerServer(t, func(ctrl *websocket.Conn) {
		readAndAckHello(t, ctrl)
		sendWelcomeMsg(t, ctrl, protocol.WelcomeMessage{})
		// Close immediately so the read pump errors out and the pumps finish
		// quickly instead of idling for the full context timeout.
	})

	cfg := &config.Config{
		DrydockURL:        srv,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		DDPollInterval:    300,
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	established, _ := c.connect(ctx)
	if !established {
		t.Fatal("established = false, want true")
	}

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn != nil {
		t.Error("c.conn was not cleared after the pumps finished")
	}
}

// ---------------------------------------------------------------------------
// readPump — malformed exec_resize / exec_end / error frames are distinctly
// logged and skipped (client.go:760, 768, 822)
// ---------------------------------------------------------------------------

// TestReadPumpMalformedExecResizeLogsAndSkips covers the CONDITIONALS_
// NEGATION on the exec_resize unmarshal-error check: the malformed branch
// logs "invalid exec_resize message" and continues; the mutant would instead
// fall through silently (no such log) into HandleResize with a zero-value
// message.
func TestReadPumpMalformedExecResizeLogsAndSkips(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	c, ctrl := newTestClient(t)
	runReadPump(t, c)

	logBuf := &syncBuffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	badEnv := protocol.Envelope{Type: protocol.TypeExecResize, Data: json.RawMessage(`"notanobject"`)}
	if err := ctrl.WriteJSON(badEnv); err != nil {
		t.Fatalf("write bad exec_resize: %v", err)
	}

	sendEnvelope(t, ctrl, protocol.TypePing, protocol.PingMessage{Timestamp: 200})
	expectType(t, ctrl, protocol.TypePong)

	if !strings.Contains(logBuf.String(), "invalid exec_resize message") {
		t.Errorf("log = %q, want the invalid-exec_resize warning", logBuf.String())
	}
}

// TestReadPumpMalformedExecEndLogsAndSkips is the exec_end counterpart of
// TestReadPumpMalformedExecResizeLogsAndSkips, covering client.go:768.
func TestReadPumpMalformedExecEndLogsAndSkips(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	c, ctrl := newTestClient(t)
	runReadPump(t, c)

	logBuf := &syncBuffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	badEnv := protocol.Envelope{Type: protocol.TypeExecEnd, Data: json.RawMessage(`"notanobject"`)}
	if err := ctrl.WriteJSON(badEnv); err != nil {
		t.Fatalf("write bad exec_end: %v", err)
	}

	sendEnvelope(t, ctrl, protocol.TypePing, protocol.PingMessage{Timestamp: 201})
	expectType(t, ctrl, protocol.TypePong)

	if !strings.Contains(logBuf.String(), "invalid exec_end message") {
		t.Errorf("log = %q, want the invalid-exec_end warning", logBuf.String())
	}
}

// TestReadPumpMalformedErrorMessageLogsAndSkips covers the CONDITIONALS_
// NEGATION on the TypeError unmarshal-error check (client.go:822): a
// malformed error envelope must log "invalid error message" and continue,
// never falling through to log "received error from controller" with an
// empty (zero-value) payload.
func TestReadPumpMalformedErrorMessageLogsAndSkips(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	c, ctrl := newTestClient(t)
	runReadPump(t, c)

	logBuf := &syncBuffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	badEnv := protocol.Envelope{Type: protocol.TypeError, Data: json.RawMessage(`"notanobject"`)}
	if err := ctrl.WriteJSON(badEnv); err != nil {
		t.Fatalf("write bad error envelope: %v", err)
	}

	sendEnvelope(t, ctrl, protocol.TypePing, protocol.PingMessage{Timestamp: 202})
	expectType(t, ctrl, protocol.TypePong)

	out := logBuf.String()
	if !strings.Contains(out, "invalid error message") {
		t.Errorf("log = %q, want the invalid-error-message warning", out)
	}
	if strings.Contains(out, "received error from controller") {
		t.Errorf("log = %q, should not fall through to the parsed-error log", out)
	}
}

// ---------------------------------------------------------------------------
// handleRequestTo — streaming response loop terminates on the first error
// read, sending every chunk that preceded it (client.go:1181)
// ---------------------------------------------------------------------------

// multiChunkReader is an io.Reader that hands back its content across two
// non-empty reads (both returning a nil error) before a final read reports
// io.EOF, forcing the streaming copy loop in handleRequestTo through more
// than one no-error iteration. A single-chunk body can't distinguish
// `readErr != nil` from `readErr == nil`: with only one non-empty read, both
// break at the same point since nothing else is left to send. See
// TestHandleRequestStreamMultipleChunksSendsAllData.
type multiChunkReader struct {
	chunks [][]byte
	i      int
}

func (r *multiChunkReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, errReaderDone
	}
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}

var errReaderDone = errors.New("multiChunkReader: no more chunks")

func TestHandleRequestStreamMultipleChunksSendsAllData(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	body := &multiChunkReader{chunks: [][]byte{
		bytes.Repeat([]byte("a"), 100),
		bytes.Repeat([]byte("b"), 100),
	}}
	//nolint:bodyclose // consumed and closed by handleRequestTo, the code under test.
	fd := &fakeDocker{streamResp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       nopReadCloser{body},
	}}
	c.dockerClient = fd

	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "multi-chunk",
		Method:    "GET",
		Path:      "/containers/abc/logs",
	})

	var resp protocol.ResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &resp)
	if !resp.IsStream {
		t.Fatal("IsStream = false, want true")
	}

	var chunk1, chunk2 protocol.StreamMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeStream), &chunk1)
	decodeData(t, expectType(t, ctrl, protocol.TypeStream), &chunk2)

	got1, _ := base64.StdEncoding.DecodeString(chunk1.Data)
	got2, _ := base64.StdEncoding.DecodeString(chunk2.Data)
	if string(got1) != strings.Repeat("a", 100) {
		t.Errorf("first chunk = %q, want 100 a's", got1)
	}
	if string(got2) != strings.Repeat("b", 100) {
		t.Errorf("second chunk = %q, want 100 b's", got2)
	}

	var end protocol.StreamEndMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeStreamEnd), &end)
	if end.RequestID != "multi-chunk" || end.Reason != "complete" {
		t.Errorf("stream_end = %+v, want multi-chunk / complete", end)
	}
}

type nopReadCloser struct{ r *multiChunkReader }

func (n nopReadCloser) Read(p []byte) (int, error) { return n.r.Read(p) }
func (n nopReadCloser) Close() error               { return nil }

// ---------------------------------------------------------------------------
// allowedDockerRequestHeaders — value length boundary (client.go:1303)
// ---------------------------------------------------------------------------

// TestAllowedDockerRequestHeadersLengthBoundary proves the exact boundary of
// `len(value) > 64*1024`: a value of exactly 64KiB is forwarded, one byte
// longer is dropped.
func TestAllowedDockerRequestHeadersLengthBoundary(t *testing.T) {
	t.Parallel()

	atLimit := strings.Repeat("a", 64*1024)
	overLimit := atLimit + "a"

	got := allowedDockerRequestHeaders(map[string]string{"Accept": atLimit})
	if got.Get("Accept") != atLimit {
		t.Error("a 64KiB header value was dropped, want it forwarded")
	}

	got = allowedDockerRequestHeaders(map[string]string{"Accept": overLimit})
	if got.Get("Accept") != "" {
		t.Error("a 64KiB+1 header value was forwarded, want it dropped")
	}
}

// ---------------------------------------------------------------------------
// sendMessageTo — direct-write failure is logged (client.go:1437)
// ---------------------------------------------------------------------------

// TestSendMessageToHandshakePathLogsWriteFailure covers the CONDITIONALS_
// NEGATION on the handshake-path WriteJSON error check: the log is the only
// observable effect (both branches return immediately afterward).
func TestSendMessageToHandshakePathLogsWriteFailure(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	agent, ctrl := newWSPair(t)
	_ = ctrl.Close()
	_ = agent.Close()

	logBuf := &bytes.Buffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	c := &Client{}
	c.sendMessageTo(outboundTarget{conn: agent}, protocol.Envelope{Type: protocol.TypePing})

	if !strings.Contains(logBuf.String(), "websocket write failed") {
		t.Errorf("log = %q, want the write-failure warning", logBuf.String())
	}
}

// ---------------------------------------------------------------------------
// outboundQueueState.release — negative-byte clamp (client.go:1486)
// ---------------------------------------------------------------------------

// TestOutboundQueueStateReleaseClampsNegativeToZero proves release clamps
// s.bytes to 0 instead of letting it go negative. CONDITIONALS_NEGATION
// (`< 0` -> `>= 0`) would skip the clamp entirely here, since bytes is
// genuinely negative after the subtraction.
func TestOutboundQueueStateReleaseClampsNegativeToZero(t *testing.T) {
	t.Parallel()

	s := &outboundQueueState{bytes: 0}
	env := protocol.Envelope{Type: "x", Data: json.RawMessage(`"abcdefgh"`)}
	s.release(env) // bytes -= size(>0) with nothing reserved -> would go negative

	if s.bytes != 0 {
		t.Errorf("bytes = %d, want 0 (clamped)", s.bytes)
	}
}

// ---------------------------------------------------------------------------
// readDeadline — arithmetic (client.go:1587)
// ---------------------------------------------------------------------------

// TestReadDeadlineComputesDoubleHeartbeat pins the exact value of
// 2*heartbeatSeconds*time.Second for an input large enough that the 60s floor
// doesn't apply, so any single-operator arithmetic mutation changes the
// result away from the expected duration.
func TestReadDeadlineComputesDoubleHeartbeat(t *testing.T) {
	t.Parallel()

	if got := readDeadline(50); got != 100*time.Second {
		t.Errorf("readDeadline(50) = %v, want 100s", got)
	}
}

// ---------------------------------------------------------------------------
// msEdge — arithmetic (client.go:1580)
// ---------------------------------------------------------------------------

// TestMsEdgeComputesMilliseconds pins msEdge's division by 1e6 (nanoseconds
// to milliseconds): ARITHMETIC_BASE (`/` -> `*`) would produce a wildly
// different (huge) number for any non-trivial elapsed time.
func TestMsEdgeComputesMilliseconds(t *testing.T) {
	t.Parallel()

	start := time.Now().Add(-250 * time.Millisecond)
	got := msEdge(start)
	if got < 200 || got > 2000 {
		t.Errorf("msEdge(-250ms) = %v, want roughly 250 (in [200, 2000])", got)
	}
}

// ---------------------------------------------------------------------------
// closeWebSocket — close error is logged (client.go:1609)
// ---------------------------------------------------------------------------

// TestCloseWebSocketLogsErrorOnAlreadyClosed covers the CONDITIONALS_NEGATION
// on the Close() error check: closing an already-closed conn returns an
// error, whose only observable effect is the debug log.
func TestCloseWebSocketLogsErrorOnAlreadyClosed(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	agent, _ := newWSPair(t)
	_ = agent.Close()

	logBuf := &bytes.Buffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(oldLogger)

	closeWebSocket(agent, "double-close")

	if !strings.Contains(logBuf.String(), "closing websocket") {
		t.Errorf("log = %q, want the close-error debug message", logBuf.String())
	}
}

// ---------------------------------------------------------------------------
// ensureOperationalState — streamSem initialization (client.go:1700)
// ---------------------------------------------------------------------------

// TestEnsureOperationalStateInitializesStreamSem proves a zero-value Client
// gets a non-nil streamSem. CONDITIONALS_NEGATION (`== nil` -> `!= nil`) would
// skip creating it, since streamSem genuinely is nil going in.
func TestEnsureOperationalStateInitializesStreamSem(t *testing.T) {
	t.Parallel()

	c := &Client{}
	c.ensureOperationalState()

	if c.streamSem == nil {
		t.Fatal("streamSem is nil after ensureOperationalState")
	}
	if cap(c.streamSem) != maxStreams {
		t.Errorf("streamSem cap = %d, want %d", cap(c.streamSem), maxStreams)
	}
}

// ---------------------------------------------------------------------------
// dockerReady — nil response body (client.go:1715)
// ---------------------------------------------------------------------------

// TestDockerReadyNilBodyDoesNotPanic covers the CONDITIONALS_NEGATION on
// `response.Body != nil`: response.Body is a nil io.ReadCloser interface
// here, so the mutant (`== nil`) would call Close on a nil interface value
// and panic.
func TestDockerReadyNilBodyDoesNotPanic(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)
	fd := &fakeDocker{doResp: &http.Response{StatusCode: http.StatusOK, Body: nil}}
	c.dockerClient = fd

	if !c.dockerReady(context.Background()) {
		t.Error("dockerReady = false, want true for a 200 response with a nil body")
	}
}

// ---------------------------------------------------------------------------
// writePump — poll tick error paths are logged (client.go:1361, 1365)
// ---------------------------------------------------------------------------

// TestWritePumpPollRefreshErrorLogsAndSkipsNotify covers the CONDITIONALS_
// NEGATION on `err != nil` after RefreshContainers: the error branch logs
// "container refresh failed" and continues (skipping OnContainerRefresh); the
// mutant would silently fall through without that log.
func TestWritePumpPollRefreshErrorLogsAndSkipsNotify(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	c, _ := newTestClient(t)
	wc := newWireClient(t, &config.Config{SkipDFCollection: true})
	c.collector = wc.collector
	c.cfg.HeartbeatInterval = 999
	c.cfg.DDPollInterval = 999
	c.welcomePollInterval = 1 // fast poll tick
	c.adapter = &errRefreshAdapter{fakeAdapter: fakeAdapter{pollInterval: 999}}

	logBuf := &syncBuffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	ctx, cancel := context.WithCancel(context.Background())
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		c.writePump(ctx)
	}()
	t.Cleanup(cancel)

	// Poll for the observable side effect (the log line from the first poll
	// tick) instead of sleeping past the 1s tick with a fixed margin.
	waitFor(t, "container refresh failed log", func() bool {
		return strings.Contains(logBuf.String(), "container refresh failed")
	})
	cancel()
	<-pumpDone // writePump must have returned before logBuf is read below

	if !strings.Contains(logBuf.String(), "container refresh failed") {
		t.Errorf("log = %q, want the refresh-failed warning", logBuf.String())
	}
}

// TestWritePumpPollOnContainerRefreshErrorLogs covers the CONDITIONALS_
// NEGATION on `err != nil` after OnContainerRefresh (client.go:1365).
func TestWritePumpPollOnContainerRefreshErrorLogs(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	c, _ := newTestClient(t)
	wc := newWireClient(t, &config.Config{SkipDFCollection: true})
	c.collector = wc.collector
	c.cfg.HeartbeatInterval = 999
	c.cfg.DDPollInterval = 999
	c.welcomePollInterval = 1
	c.adapter = &errOnRefreshAdapter{fakeAdapter: fakeAdapter{pollInterval: 999}}

	logBuf := &syncBuffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	ctx, cancel := context.WithCancel(context.Background())
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		c.writePump(ctx)
	}()
	t.Cleanup(cancel)

	// Poll for the observable side effect (the log line from the first poll
	// tick) instead of sleeping past the 1s tick with a fixed margin.
	waitFor(t, "container refresh notify failed log", func() bool {
		return strings.Contains(logBuf.String(), "container refresh notify failed")
	})
	cancel()
	<-pumpDone // writePump must have returned before logBuf is read below

	if !strings.Contains(logBuf.String(), "container refresh notify failed") {
		t.Errorf("log = %q, want the notify-failed warning", logBuf.String())
	}
}

// ---------------------------------------------------------------------------
// Run — reconnect backoff actually grows (client.go:333, extra-mutator run)
// ---------------------------------------------------------------------------

// TestRunExponentialBackoffBoundsReconnectCount covers `delay *= 2` (client.go
// :333) by asserting the actual observed delay sequence doubles (within
// jitter) and then holds at the configured cap, instead of inferring growth
// from how many reconnects fit in a fixed wall-clock window.
//
// It overrides the package-level reconnectWait seam so the retry loop never
// really sleeps: each call records the delay it was asked to wait for and
// returns an already-fired channel. That makes the assertion exact and fast
// rather than a performance-dependent race against CI scheduling — under
// INVERT_ASSIGNMENTS (`*=` -> `/=`) the recorded delays shrink instead of
// growing, and under REMOVE_SELF_ASSIGNMENTS (`delay *= 2` -> `delay = 2`)
// they collapse to a 2ns constant after the first cycle; both fail the
// per-step bounds check deterministically.
func TestRunExponentialBackoffBoundsReconnectCount(t *testing.T) {
	// Deliberately NOT t.Parallel(): overrides the package-level reconnectWait var.

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	const wantSamples = 6

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var delays []time.Duration
	oldWait := reconnectWait
	reconnectWait = func(d time.Duration) <-chan time.Time {
		delays = append(delays, d)
		if len(delays) >= wantSamples {
			cancel()
		}
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	t.Cleanup(func() { reconnectWait = oldWait })

	addr := freeAddr(t)
	cfg := &config.Config{
		DrydockURL:        srv.URL,
		HeartbeatInterval: 30,
		WelcomeTimeout:    5,
		ReconnectDelay:    1, // 1s initial delay
		MaxReconnectDelay: 4, // low cap so the recorded sequence exercises it too
		DDPollInterval:    300,
		BindAddress:       "127.0.0.1",
		Port:              portFrom(addr),
		SkipDFCollection:  true,
	}
	c := newWireClient(t, cfg)

	_ = c.Run(ctx)

	if len(delays) < wantSamples {
		t.Fatalf("recorded %d reconnect delays, want at least %d", len(delays), wantSamples)
	}

	// jitteredDuration scales the nominal delay by [0.75, 1.25]x, so bound
	// each observed wait against the nominal doubling-then-capped sequence
	// with that same margin instead of asserting an exact value.
	nominal := time.Duration(cfg.ReconnectDelay) * time.Second
	maxDelay := time.Duration(cfg.MaxReconnectDelay) * time.Second
	for i, d := range delays[:wantSamples] {
		lo := time.Duration(float64(nominal) * 0.75)
		hi := time.Duration(float64(nominal) * 1.25)
		if d < lo || d > hi {
			t.Fatalf("delay[%d] = %v, want in [%v, %v] for nominal %v", i, d, lo, hi, nominal)
		}
		nominal *= 2
		if nominal > maxDelay {
			nominal = maxDelay
		}
	}
}

// ---------------------------------------------------------------------------
// outboundQueueState.enqueue — byte reservation rollback on frame-limit
// rejection (client.go:1478, extra-mutator run)
// ---------------------------------------------------------------------------

// TestOutboundQueueStateEnqueueRollsBackReservationOnFrameLimit covers
// `s.bytes -= size` in the outboundFrameLimitExceeded branch. It fills a
// small channel to capacity (two accepted enqueues), then enqueues one more
// envelope that must be rejected because the channel, not the byte budget,
// is full.
//
// Correct code rolls the reservation back to exactly the two accepted
// frames' bytes. INVERT_ASSIGNMENTS (`-=` -> `+=`) would double-reserve the
// rejected frame's bytes on top of that; REMOVE_SELF_ASSIGNMENTS (`-=` ->
// `=`) would throw away the prior reservation and set it to just the
// rejected frame's size. All three land on different values.
func TestOutboundQueueStateEnqueueRollsBackReservationOnFrameLimit(t *testing.T) {
	t.Parallel()

	ch := make(chan protocol.Envelope, 2)
	state := &outboundQueueState{}
	env := protocol.Envelope{Data: json.RawMessage(`"x"`)}
	size := outboundEnvelopeBytes(env)

	for i := 0; i < cap(ch); i++ {
		if got := state.enqueue(ch, env); got != outboundEnqueued {
			t.Fatalf("enqueue %d = %d, want outboundEnqueued", i, got)
		}
	}

	if got := state.enqueue(ch, env); got != outboundFrameLimitExceeded {
		t.Fatalf("enqueue on a full channel = %d, want outboundFrameLimitExceeded", got)
	}

	want := int64(cap(ch)) * size
	if state.bytes != want {
		t.Fatalf("state.bytes = %d, want %d: the rejected enqueue's reservation must roll back exactly", state.bytes, want)
	}
}

// ---------------------------------------------------------------------------
// streamedBodyBytesLocked — sums every entry, not just the last
// (client.go:986, extra-mutator run)
// ---------------------------------------------------------------------------

// TestStreamedBodyBytesLockedSumsAllEntries proves the pendingBodies loop
// (client.go:986, `total += int64(pb.buf.Len())`) accumulates across
// multiple entries. REMOVE_SELF_ASSIGNMENTS (`+=` -> `=`) would leave total
// holding only the last-iterated entry's length, undercounting whenever more
// than one streamed body is in flight.
func TestStreamedBodyBytesLockedSumsAllEntries(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)

	pbA := &pendingRequestBody{req: protocol.RequestMessage{RequestID: "a"}, timer: time.NewTimer(time.Hour)}
	pbA.buf.WriteString("12345") // 5 bytes
	pbB := &pendingRequestBody{req: protocol.RequestMessage{RequestID: "b"}, timer: time.NewTimer(time.Hour)}
	pbB.buf.WriteString("1234567") // 7 bytes
	t.Cleanup(func() {
		pbA.timer.Stop()
		pbB.timer.Stop()
	})

	c.pendingBodiesMu.Lock()
	c.pendingBodies = map[string]*pendingRequestBody{"a": pbA, "b": pbB}
	c.dispatchingBodies = map[uint64]int64{1: 3, 2: 11}
	total := c.streamedBodyBytesLocked()
	c.pendingBodiesMu.Unlock()

	const want = 5 + 7 + 3 + 11
	if total != want {
		t.Fatalf("streamedBodyBytesLocked() = %d, want %d (sum across both maps, not just the last entry of each)", total, want)
	}
}

// ---------------------------------------------------------------------------
// startHealthServer — readiness Ready field requires BOTH docker and
// controller (client.go:1646, extra-mutator run)
// ---------------------------------------------------------------------------

// TestHealthServerReadyFieldRequiresBothDockerAndController covers
// `Ready: dockerConnected && controllerConnected`. Existing tests only ever
// exercise the (false,false) and (true,true) points, where AND and OR agree;
// this pins a mixed point (docker up, controller down) where INVERT_LOGICAL
// (`&&` -> `||`) diverges from the correct AND.
func TestHealthServerReadyFieldRequiresBothDockerAndController(t *testing.T) {
	t.Parallel()

	c := &Client{
		cfg: &config.Config{
			BindAddress: "127.0.0.1",
			Port:        "0",
		},
	}
	//nolint:bodyclose // consumed and closed by dockerReady, the code under test.
	c.dockerClient = &fakeDocker{doResp: mkResp(http.StatusOK, "", "")}
	c.startHealthServer()
	t.Cleanup(func() {
		if c.healthServer != nil {
			_ = c.healthServer.Close()
		}
	})

	rec := httptest.NewRecorder()
	c.healthServer.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	var body protocol.HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	if body.Ready {
		t.Fatalf("Ready = true, want false: docker is connected but the controller link is not")
	}
	if body.Docker != "connected" || body.Controller != "disconnected" {
		t.Fatalf("readiness response = %+v, want docker connected / controller disconnected", body)
	}
}

// ---------------------------------------------------------------------------
// dockerReady — nil response with nil error (client.go:1712, extra-mutator
// run)
// ---------------------------------------------------------------------------

// TestDockerReadyNilResponseNilErrorIsNotReady covers `err != nil || response
// == nil`. A dockerClient that hands back (nil, nil) is the one case where
// INVERT_LOGICAL (`||` -> `&&`) diverges: the mutant no longer short-circuits
// on the nil response and falls through toward dereferencing it.
func TestDockerReadyNilResponseNilErrorIsNotReady(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)
	c.dockerClient = &fakeDocker{doResp: nil, doErr: nil}

	if got := c.dockerReady(context.Background()); got != false {
		t.Errorf("dockerReady() with nil response and nil error = %v, want false", got)
	}
}

// ---------------------------------------------------------------------------
// allowedDockerRequestHeaders — a rejected entry must not stop the loop
// (client.go:1304, extra-mutator run)
// ---------------------------------------------------------------------------

// TestAllowedDockerRequestHeadersSkipsOnlyTheBadEntry covers INVERT_LOOPCTRL
// (`continue` -> `break`) on the CRLF/oversize guard. Unlike the readPump
// switch-case continues (which are the last statement of an infinite `for`
// body and so are unaffected by this mutation), this continue sits at the
// top of a `for range` over a map with a case statement below it: a `break`
// here would abort the entire loop instead of skipping just the bad entry,
// dropping every header not yet visited.
//
// Go randomizes map iteration order, so a single run could get lucky (the
// bad entry lands last, after every good one was already set). Looping many
// times makes the bad entry land first often enough that a `break` is caught
// with overwhelming probability while a passing run under real code is
// guaranteed every time.
func TestAllowedDockerRequestHeadersSkipsOnlyTheBadEntry(t *testing.T) {
	t.Parallel()

	for i := 0; i < 200; i++ {
		headers := map[string]string{
			"Accept":            "application/json",
			"Content-Type":      "application/json",
			"X-Registry-Auth":   "creds",
			"X-Registry-Config": "config",
			"X-Bad-Header":      "value\r\ninjected",
		}
		got := allowedDockerRequestHeaders(headers)
		if got.Get("Accept") != "application/json" ||
			got.Get("Content-Type") != "application/json" ||
			got.Get("X-Registry-Auth") != "creds" ||
			got.Get("X-Registry-Config") != "config" {
			t.Fatalf("iteration %d: a good header was dropped because the bad one aborted the loop instead of being skipped: %+v", i, got)
		}
	}
}

// ---------------------------------------------------------------------------
// startHealthServer — a graceful shutdown must not log a health-server error
// (client.go:1686, extra-mutator run)
// ---------------------------------------------------------------------------

// TestHealthServerListenAndServeSwallowsGracefulShutdown covers `err != nil
// && err != http.ErrServerClosed`. ListenAndServe always returns a non-nil
// error, so INVERT_LOGICAL (`&&` -> `||`) makes the left operand alone
// satisfy the condition and logs a spurious "health server error" warning on
// every graceful shutdown, not just a real listen failure.
func TestHealthServerListenAndServeSwallowsGracefulShutdown(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	logBuf := &syncBuffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	addr := freeAddr(t)
	c := &Client{
		cfg: &config.Config{
			BindAddress: "127.0.0.1",
			Port:        portFrom(addr),
		},
	}
	c.startHealthServer()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.healthServer.Shutdown(shutdownCtx)
	})

	healthURL := "http://" + c.healthServer.Addr + "/health"
	waitFor(t, "health server ready", func() bool {
		//nolint:noctx,bodyclose
		resp, err := http.Get(healthURL) //nolint:gosec
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.healthServer.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}
	// Shutdown does not itself establish a happens-before relationship with
	// the err-check in the ListenAndServe goroutine, so join that goroutine
	// via healthServerDone (closed only after the check, and the log write
	// when warranted, complete) instead of polling the log on a timer — a
	// mutant that logs "health server error" could otherwise pass as clean
	// simply because the goroutine hadn't run by the deadline yet. The
	// timeout below fails the test rather than silently passing if the
	// goroutine never finishes.
	select {
	case <-c.healthServerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe goroutine did not finish its post-shutdown error check in time")
	}

	if strings.Contains(logBuf.String(), "health server error") {
		t.Errorf("log = %q, want no health-server-error warning on a graceful shutdown", logBuf.String())
	}
}
