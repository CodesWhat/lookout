package server

// middleware_mutants_test.go adds tests that specifically target Gremlins
// mutants surviving in middleware.go: integer boundary conditions, an
// arithmetic constant, and negated error-logging checks that existing tests
// exercised on only one side.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFinishAuthDoesNotDecrementInFlightBelowZero verifies finishAuthLocked
// leaves inFlight at 0 (and still removes an empty tracking record) when
// there is no in-flight reservation to release. This kills the
// CONDITIONALS_BOUNDARY mutant at middleware.go:272:16 (`if a.inFlight > 0`
// -> `>= 0`), which would decrement inFlight to -1 and, as a result, no
// longer see the record as empty (count==0 && inFlight==0) and leave it in
// the table.
func TestFinishAuthDoesNotDecrementInFlightBelowZero(t *testing.T) {
	t.Parallel()

	rl := &RateLimiter{attempts: map[string]*ipAttempts{"client": {inFlight: 0}}}
	a := rl.attempts["client"]

	rl.finishAuth("client", true)

	if a.inFlight != 0 {
		t.Fatalf("inFlight = %d, want 0 (must not go negative)", a.inFlight)
	}
	if _, ok := rl.attempts["client"]; ok {
		t.Fatal("empty tracking record (count=0, inFlight=0) must be removed on success")
	}
}

// TestFinishEnrollmentDoesNotDecrementBelowZero verifies finishEnrollment
// leaves enrollmentInFlight at 0 when there is nothing in flight, killing the
// CONDITIONALS_BOUNDARY mutant at middleware.go:310:27
// (`if rl.enrollmentInFlight > 0` -> `>= 0`).
func TestFinishEnrollmentDoesNotDecrementBelowZero(t *testing.T) {
	t.Parallel()

	rl := &RateLimiter{attempts: map[string]*ipAttempts{}}

	rl.finishEnrollment("client", true)

	if rl.enrollmentInFlight != 0 {
		t.Fatalf("enrollmentInFlight = %d, want 0 (must not go negative)", rl.enrollmentInFlight)
	}
}

// TestMsComputesMillisecondsNotMicroseconds pins ms() to a millisecond-scale
// result, killing the ARITHMETIC_BASE mutant at middleware.go:635:50
// (`/ 1e6` -> `* 1e6`). The mutant would inflate ~50ms of real elapsed time
// (roughly 5e7 nanoseconds) to roughly 5e13, far outside any sane bound.
func TestMsComputesMillisecondsNotMicroseconds(t *testing.T) {
	t.Parallel()

	start := time.Now().Add(-50 * time.Millisecond)
	got := ms(start)

	if got < 0 {
		t.Fatalf("ms() = %v, want a non-negative elapsed duration", got)
	}
	if got > 100000 {
		t.Fatalf("ms() = %v, want a millisecond-scale value for ~50ms elapsed (got a value consistent with a `/` -> `*` mutant on the 1e6 divisor)", got)
	}
}

// recordingLogHandler is a minimal slog.Handler that captures every log
// message it receives, for tests that need to assert on log output rather
// than a return value or control-flow side effect.
type recordingLogHandler struct {
	mu       sync.Mutex
	messages []string
}

func (h *recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, r.Message)
	return nil
}

func (h *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingLogHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingLogHandler) has(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.messages {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

// installLogCapture swaps the default slog logger for one that records into
// the returned handler, restoring the previous default on cleanup. Callers
// must not run in parallel with other tests that depend on the default
// logger, since this mutates global state.
func installLogCapture(t *testing.T) *recordingLogHandler {
	t.Helper()
	h := &recordingLogHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// TestAuthMiddlewareReadDeadlineErrorsAreLogged exercises both sides of the
// CONDITIONALS_NEGATION mutants at middleware.go:470:77 and middleware.go:481:62
// (`err != nil` -> `== nil`, guarding the SetReadDeadline warnings around the
// Ed25519 auth-body read). httptest.ResponseRecorder does not implement the
// http.ResponseController deadline interfaces, so both SetReadDeadline calls
// fail and both warnings must be logged; a real, deadline-capable connection
// must log neither.
//
// Not t.Parallel(): it mutates the package-level slog default logger.
func TestAuthMiddlewareReadDeadlineErrorsAreLogged(t *testing.T) {
	ed, priv := setupEd25519(t)
	rl := NewRateLimiter()
	defer rl.Stop()
	h := rl.AuthMiddlewareWithEd25519(nil, ed, noAudit(t), nil, http.HandlerFunc(okHandler))

	logs := installLogCapture(t)

	req := httptest.NewRequest(http.MethodGet, "/_portwing/info", nil)
	signEd25519Request(t, req, nil, priv, time.Now().Unix(), freshNonce(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !logs.has("setting auth body read deadline") {
		t.Error(`expected "setting auth body read deadline" warning when the ResponseWriter does not support deadlines`)
	}
	if !logs.has("clearing auth body read deadline") {
		t.Error(`expected "clearing auth body read deadline" warning when the ResponseWriter does not support deadlines`)
	}
}

// TestAuthMiddlewareReadDeadlineSucceedsWithoutLoggingOnRealConn is the
// complementary case: a real network connection (via httptest.NewServer)
// supports SetReadDeadline, so neither warning should be logged.
//
// Not t.Parallel(): it mutates the package-level slog default logger.
func TestAuthMiddlewareReadDeadlineSucceedsWithoutLoggingOnRealConn(t *testing.T) {
	ed, priv := setupEd25519(t)
	rl := NewRateLimiter()
	defer rl.Stop()
	h := rl.AuthMiddlewareWithEd25519(nil, ed, noAudit(t), nil, http.HandlerFunc(okHandler))

	ts := httptest.NewServer(h)
	defer ts.Close()

	logs := installLogCapture(t)

	body := []byte(`{}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/_portwing/info", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	signEd25519Request(t, req, body, priv, time.Now().Unix(), freshNonce(t))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if logs.has("setting auth body read deadline") {
		t.Error(`unexpected "setting auth body read deadline" warning on a real, deadline-capable connection`)
	}
	if logs.has("clearing auth body read deadline") {
		t.Error(`unexpected "clearing auth body read deadline" warning on a real, deadline-capable connection`)
	}
}

// closeErrorReadCloser wraps a reader and returns an error from Close.
type closeErrorReadCloser struct {
	io.Reader
}

func (closeErrorReadCloser) Close() error { return errors.New("close failed") }

// TestAuthMiddlewareBodyCloseErrorIsLogged exercises both sides of the
// CONDITIONALS_NEGATION mutant at middleware.go:475:45 (`closeErr != nil` ->
// `== nil`): a request body whose Close returns an error must log a
// "closing request body" warning; one that closes cleanly must not.
//
// Not t.Parallel(): it mutates the package-level slog default logger.
func TestAuthMiddlewareBodyCloseErrorIsLogged(t *testing.T) {
	ed, priv := setupEd25519(t)
	rl := NewRateLimiter()
	defer rl.Stop()
	h := rl.AuthMiddlewareWithEd25519(nil, ed, noAudit(t), nil, http.HandlerFunc(okHandler))

	logs := installLogCapture(t)

	req := httptest.NewRequest(http.MethodGet, "/_portwing/info", nil)
	req.Body = closeErrorReadCloser{Reader: strings.NewReader("")}
	signEd25519Request(t, req, nil, priv, time.Now().Unix(), freshNonce(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !logs.has("closing request body") {
		t.Error(`expected "closing request body" warning when Body.Close() fails`)
	}
}

// TestAuthMiddlewareBodyCloseSuccessIsNotLogged is the complementary case: a
// body that closes without error must not log a "closing request body"
// warning.
//
// Not t.Parallel(): it mutates the package-level slog default logger.
func TestAuthMiddlewareBodyCloseSuccessIsNotLogged(t *testing.T) {
	ed, priv := setupEd25519(t)
	rl := NewRateLimiter()
	defer rl.Stop()
	h := rl.AuthMiddlewareWithEd25519(nil, ed, noAudit(t), nil, http.HandlerFunc(okHandler))

	logs := installLogCapture(t)

	req := httptest.NewRequest(http.MethodGet, "/_portwing/info", nil)
	req.Body = io.NopCloser(strings.NewReader(""))
	signEd25519Request(t, req, nil, priv, time.Now().Unix(), freshNonce(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if logs.has("closing request body") {
		t.Error(`unexpected "closing request body" warning when Body.Close() succeeds`)
	}
}

// TestFinishAuthLockedResetsStreakAfterWindowExpires verifies finishAuthLocked
// resets the failure streak (count=1, firstFail=now) rather than incrementing
// it when the tracked entry's window has already expired, killing the
// INVERT_LOGICAL mutant at middleware.go:283:26 (`a.firstFail.IsZero() ||
// now.Sub(a.firstFail) > rl.window` turned into `&&`). firstFail is non-zero
// here (already set), so the mutant's AND requires IsZero() to also be true;
// since it is not, the mutant falls through to `a.count++` instead of
// resetting.
func TestFinishAuthLockedResetsStreakAfterWindowExpires(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	defer rl.Stop()

	// Guard direct map access with rl.mu: NewRateLimiter starts a cleanup
	// goroutine that owns the same map, so an unsynchronized read/write here
	// races with it even though the window makes an actual overlap unlikely.
	rl.mu.Lock()
	rl.attempts["some-ip"] = &ipAttempts{count: 5, firstFail: time.Now().Add(-2 * rl.window)}
	rl.mu.Unlock()

	rl.finishAuth("some-ip", false)

	rl.mu.Lock()
	got := rl.attempts["some-ip"].count
	rl.mu.Unlock()
	if got != 1 {
		t.Fatalf("count = %d, want 1 (streak should have reset after the window expired)", got)
	}
}

// TestParseTrustedProxiesSkipsEmptyEntry verifies an empty entry is skipped,
// not treated as loop-ending, killing the INVERT_LOOPCTRL mutant at
// middleware.go:698:4 (`continue` -> `break`). Real code skips the empty
// entry via `continue` and goes on to parse the following CIDR. The mutant's
// `break` exits the loop entirely at the first (empty) entry, before ever
// reaching the CIDR.
func TestParseTrustedProxiesSkipsEmptyEntry(t *testing.T) {
	t.Parallel()

	nets, err := ParseTrustedProxies([]string{"", "10.0.0.0/8"})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	if len(nets) != 1 {
		t.Fatalf("len(nets) = %d, want 1 (empty entry skipped, CIDR parsed)", len(nets))
	}
}
