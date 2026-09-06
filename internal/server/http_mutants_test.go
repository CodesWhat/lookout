package server

// http_mutants_test.go adds tests that specifically target Gremlins mutants
// surviving in http.go: boundary/negation/arithmetic conditions that existing
// tests exercised but did not pin down at the exact boundary or on both sides
// of a negated comparison.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/adapter"
	"github.com/codeswhat/portwing/internal/audit"
)

// TestMaxHijackBodyBytesValue pins the exact computed value of
// maxHijackBodyBytes, killing the two ARITHMETIC_BASE mutants on its
// `10 * 1024 * 1024` literal (http.go:69:31, http.go:69:38). Either mutated
// multiplication (to division) collapses the constant to 0 or 10.
func TestMaxHijackBodyBytesValue(t *testing.T) {
	t.Parallel()

	const want = 10 * 1024 * 1024
	if maxHijackBodyBytes != want {
		t.Fatalf("maxHijackBodyBytes = %d, want %d", maxHijackBodyBytes, want)
	}
}

// TestNewServerHTTPTimeouts pins the exact ReadHeaderTimeout and IdleTimeout
// NewServer configures on the underlying http.Server, killing the
// ARITHMETIC_BASE mutants at http.go:295:25 (`10 * time.Second`) and
// http.go:296:26 (`120 * time.Second`).
func TestNewServerHTTPTimeouts(t *testing.T) {
	t.Parallel()

	client, stop := newStubDockerClient(t)
	defer stop()

	s, err := NewServer(minimalConfig(), client, &stubServerAdapter{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	if got, want := s.httpServer.ReadHeaderTimeout, 10*time.Second; got != want {
		t.Errorf("ReadHeaderTimeout = %v, want %v", got, want)
	}
	if got, want := s.httpServer.IdleTimeout, 120*time.Second; got != want {
		t.Errorf("IdleTimeout = %v, want %v", got, want)
	}
}

// TestNewServerTLSConfigGuard exercises both sides of the two
// CONDITIONALS_NEGATION mutants at http.go:300 (`cfg.TLSCert != "" &&
// cfg.TLSKey != ""`). Only when both fields are non-empty must TLSConfig be
// set; either field alone must leave it nil.
func TestNewServerTLSConfigGuard(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		cert, key      string
		wantTLSEnabled bool
	}{
		{"neither set", "", "", false},
		{"cert only", "cert.pem", "", false}, // kills mutant on cfg.TLSKey != ""
		{"key only", "", "key.pem", false},   // kills mutant on cfg.TLSCert != ""
		{"both set", "cert.pem", "key.pem", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, stop := newStubDockerClient(t)
			defer stop()

			cfg := minimalConfig()
			cfg.TLSCert = tc.cert
			cfg.TLSKey = tc.key

			s, err := NewServer(cfg, client, &stubServerAdapter{})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
			}()

			gotEnabled := s.httpServer.TLSConfig != nil
			if gotEnabled != tc.wantTLSEnabled {
				t.Fatalf("TLSConfig set = %v, want %v", gotEnabled, tc.wantTLSEnabled)
			}
		})
	}
}

// TestOversizedAttachBodyAtExactLimitIsNotRejected verifies a body of exactly
// maxHijackBodyBytes is NOT rejected as too large, killing the
// CONDITIONALS_BOUNDARY mutant at http.go:594:15 (`len(body) > maxHijackBodyBytes`
// -> `>=`). The request still fails past the size check (httptest.ResponseRecorder
// doesn't support hijacking), but it must fail with 500 "hijacking not
// supported", not 413.
func TestOversizedAttachBodyAtExactLimitIsNotRejected(t *testing.T) {
	t.Parallel()

	s := &Server{}
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1.44/containers/abc123/attach",
		io.LimitReader(zeroReader{}, maxHijackBodyBytes),
	)
	rec := httptest.NewRecorder()
	s.handleDockerHijack(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("body exactly at maxHijackBodyBytes was rejected as too large")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (hijacking not supported)", rec.Code, http.StatusInternalServerError)
	}
}

// TestIsASCIIDigitsBoundary exercises isASCIIDigits at the '0' and '9'
// boundaries and their immediate neighbors, killing the two
// CONDITIONALS_BOUNDARY mutants at http.go:759 (`value[i] < '0'` and
// `value[i] > '9'`).
func TestIsASCIIDigitsBoundary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", true}, // exact lower boundary
		{"9", true}, // exact upper boundary
		{"09", true},
		{"/", false}, // '/' == '0'-1, just below lower boundary
		{":", false}, // ':' == '9'+1, just above upper boundary
		{"123", true},
		{"12a", false},
	}

	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			if got := isASCIIDigits(tc.value); got != tc.want {
				t.Errorf("isASCIIDigits(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// flushTrackingResponseWriter records whether Flush was called, to observe
// streamResponse's internal `if n > 0` guard.
type flushTrackingResponseWriter struct {
	hdr     http.Header
	body    []byte
	flushed bool
}

func (w *flushTrackingResponseWriter) Header() http.Header { return w.hdr }
func (w *flushTrackingResponseWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	return len(p), nil
}
func (w *flushTrackingResponseWriter) WriteHeader(int) {}
func (w *flushTrackingResponseWriter) Flush()          { w.flushed = true }

// zeroByteThenEOFReader returns (0, io.EOF) on every Read, simulating a
// stream that ends without ever producing a byte.
type zeroByteThenEOFReader struct{}

func (zeroByteThenEOFReader) Read([]byte) (int, error) { return 0, io.EOF }

// TestStreamResponseSkipsWriteAndFlushOnZeroRead verifies that a zero-byte
// read does not trigger a Write or a Flush, killing the CONDITIONALS_BOUNDARY
// mutant at http.go:803:8 (`if n > 0` -> `n >= 0`).
func TestStreamResponseSkipsWriteAndFlushOnZeroRead(t *testing.T) {
	t.Parallel()

	w := &flushTrackingResponseWriter{hdr: make(http.Header)}
	s := &Server{}
	s.streamResponse(w, zeroByteThenEOFReader{})

	if w.flushed {
		t.Fatal("streamResponse flushed on a zero-byte read, want no flush")
	}
	if len(w.body) != 0 {
		t.Fatalf("streamResponse wrote %d bytes on a zero-byte read, want 0", len(w.body))
	}
}

// TestShutdownWithNilHupDoneDoesNotPanic verifies Shutdown is a no-op on the
// hupCh/hupDone SIGHUP-reload cleanup when neither field was populated (a
// Server built without NewServer's authorized_keys setup). This kills the
// CONDITIONALS_NEGATION mutant at http.go:898:16 (`s.hupDone != nil` ->
// `== nil`): the mutated code would enter `select { case <-s.hupDone: default:
// close(s.hupDone) }` with hupDone nil, and `close` of a nil channel panics.
func TestShutdownWithNilHupDoneDoesNotPanic(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	auditor, closeAudit, err := audit.New("", 0)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(closeAudit)

	httpSrv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}

	s := &Server{
		rateLimiter: rl,
		auditor:     auditor,
		httpServer:  httpSrv,
		// hupCh and hupDone are left at their zero value (nil), matching a
		// Server with no AuthorizedKeysFile configured.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown with nil hupCh/hupDone: %v", err)
	}
}

// tickErrorCountingAdapter succeeds on the initial RefreshContainers call
// (so pollContainers reaches its ticker loop) and errors on every ticker
// call, while counting OnContainerRefresh invocations.
type tickErrorCountingAdapter struct {
	stubServerAdapter
	initialDone        atomic.Bool
	tickCount          atomic.Int32
	ticked             chan struct{}
	tickedOnce         sync.Once
	onRefreshCallCount atomic.Int32
}

func (a *tickErrorCountingAdapter) RefreshContainers(_ context.Context) ([]adapter.Container, []adapter.Container, []adapter.Container, error) {
	if a.initialDone.CompareAndSwap(false, true) {
		return nil, nil, nil, nil // initial refresh succeeds
	}
	if a.tickCount.Add(1) >= 2 {
		a.tickedOnce.Do(func() { close(a.ticked) })
	}
	return nil, nil, nil, context.DeadlineExceeded // every ticker refresh fails
}

func (a *tickErrorCountingAdapter) OnContainerRefresh(_ context.Context, _ adapter.MessageSender, _, _, _ []adapter.Container) error {
	a.onRefreshCallCount.Add(1)
	return nil
}

// TestPollContainersSkipsNotifyAfterTickRefreshError verifies that a
// RefreshContainers error on the ticker path causes pollContainers to
// `continue` (skip OnContainerRefresh) rather than fall through to call it,
// killing the CONDITIONALS_NEGATION mutant at http.go:970:11 (`if err != nil`
// -> `== nil`). A mutated build would keep calling OnContainerRefresh with
// the zero-value (nil) added/updated/removed slices after every failed
// ticker refresh, in addition to the single legitimate call the successful
// initial refresh makes.
func TestPollContainersSkipsNotifyAfterTickRefreshError(t *testing.T) {
	t.Parallel()

	client, stop := newStubDockerClient(t)
	defer stop()

	auditor, closeAudit, err := audit.New("", 0)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	defer closeAudit()

	cfg := minimalConfig()
	cfg.DDPollInterval = 1 // 1-second ticker

	a := &tickErrorCountingAdapter{ticked: make(chan struct{})}
	s := &Server{
		dockerClient: client,
		adapter:      a,
		cfg:          cfg,
		rateLimiter:  NewRateLimiter(),
		auditor:      auditor,
	}
	defer s.rateLimiter.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.pollContainers(ctx)
	}()

	// Wait for two ticker refreshes rather than a wall-clock deadline: by the
	// time the second one starts, a mutated build has already completed one
	// full ticker iteration and called OnContainerRefresh.
	select {
	case <-a.ticked:
	case <-time.After(20 * time.Second):
		t.Fatal("ticker did not fire twice in time")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pollContainers did not exit in time")
	}

	// The single legitimate call comes from the successful initial refresh
	// (before the ticker loop starts); every ticker refresh failed, so no
	// further calls should follow.
	if got := a.onRefreshCallCount.Load(); got != 1 {
		t.Fatalf("OnContainerRefresh called %d times (want exactly 1, from the initial refresh) after every ticker RefreshContainers failed", got)
	}
}

// TestIsDockerResourceActionEmptyPathDoesNotPanic verifies an empty path is
// rejected without indexing into it, killing the INVERT_LOGICAL mutant at
// http.go:728:16 (the `path == "" || path[0] != '/'` `||` turned into `&&`).
// Under the mutant, `path == ""` alone no longer short-circuits the OR chain,
// so Go must evaluate `path[0] != '/'` too — indexing an empty string, which
// panics.
func TestIsDockerResourceActionEmptyPathDoesNotPanic(t *testing.T) {
	t.Parallel()

	if got := isExecStartPath(""); got {
		t.Fatalf("isExecStartPath(\"\") = %v, want false", got)
	}
}

// TestIsDockerResourceActionMissingLeadingSlashRejected verifies a path that
// fails the leading-slash check is rejected even when the trailing-slash
// check alone would pass, killing the INVERT_LOGICAL mutant at http.go:728:34
// (the second `||`, between `(path=="" || path[0]!='/')` and
// `strings.HasSuffix(path, "/")`, turned into `&&`). Real code: the OR chain
// short-circuits true on `path[0] != '/'` and returns false. The mutant's AND
// requires HasSuffix(path, "/") to also be true; since it is not, the mutant
// skips the early return and falls through to a resource/action match against
// path[1:], incorrectly returning true.
func TestIsDockerResourceActionMissingLeadingSlashRejected(t *testing.T) {
	t.Parallel()

	if got := isDockerResourceAction("Xexec/y/start", "exec", "start"); got {
		t.Fatalf("isDockerResourceAction(%q, ...) = %v, want false (no leading slash)", "Xexec/y/start", got)
	}
}

// TestListenAndServePlainHTTPWhenOnlyOneTLSFieldSet verifies ListenAndServe
// takes the plain-HTTP path (not the TLS path) when only one of TLSCert/
// TLSKey is set, killing the INVERT_LOGICAL mutant at http.go:880:25
// (`s.cfg.TLSCert != "" && s.cfg.TLSKey != ""` turned into `||`).
//
// A wall-clock "still running after N seconds" branch doesn't discriminate
// deterministically: it only fails the mutant when the timer wins the race
// against however long the cert-load error takes to surface, which is a
// scheduling bet, not a proof. This instead uses the Addr() seam ListenAndServe
// now exposes (set the instant net.Listen succeeds, before the TLS branch, so
// it doesn't itself discriminate between the two paths) to poll a real plain
// HTTP GET against the bound port until it succeeds or a deadline fails the
// test. The real, unmutated plain-HTTP path answers almost immediately. The
// mutant takes the TLS branch instead: ServeTLS fails to load the nonexistent
// "fake.crt" before it ever calls Accept, so the listener never answers and
// every GET attempt times out, deterministically failing the probe deadline.
func TestListenAndServePlainHTTPWhenOnlyOneTLSFieldSet(t *testing.T) {
	client, stop := newStubDockerClient(t)
	defer stop()

	// Let the OS assign a free port directly inside ListenAndServe instead of
	// discovering one by binding, closing, and hoping nothing else claims it
	// before the real server rebinds — that gap was a source of unmutated
	// failures under parallel test load.
	cfg := minimalConfig()
	cfg.Port = "0"
	cfg.TLSCert = "fake.crt"
	cfg.TLSKey = ""

	s, err := NewServer(cfg, client, &stubServerAdapter{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.ListenAndServe()
	}()

	// Poll for the bound address rather than assuming it is ready by some
	// fixed delay: net.Listen happens before the TLS/plain branch, so this
	// step succeeds identically on both the real and mutant paths and only
	// proves the listener came up.
	addrDeadline := time.Now().Add(2 * time.Second)
	var addr net.Addr
	for time.Now().Before(addrDeadline) {
		if addr = s.Addr(); addr != nil {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if addr == nil {
		select {
		case err := <-errCh:
			t.Fatalf("ListenAndServe never bound a listener and returned: %v", err)
		default:
			t.Fatal("ListenAndServe never bound a listener")
		}
	}

	// Now prove the plain-HTTP path is actually the one answering, not just
	// that something is listening: poll a plain GET against the bound
	// address until it succeeds within a deadline.
	probeDeadline := time.Now().Add(3 * time.Second)
	httpClient := &http.Client{Timeout: 300 * time.Millisecond}
	url := "http://" + addr.String() + "/health"
	var lastErr error
	gotOK := false
	for time.Now().Before(probeDeadline) {
		resp, getErr := httpClient.Get(url)
		if getErr != nil {
			lastErr = getErr
			time.Sleep(20 * time.Millisecond)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
			time.Sleep(20 * time.Millisecond)
			continue
		}
		gotOK = true
		break
	}
	if !gotOK {
		t.Fatalf("plain HTTP GET %s never succeeded (want the plain-HTTP path to be serving): %v", url, lastErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("ListenAndServe returned an unexpected error after Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return after Shutdown")
	}
}
