package edge

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/protocol"
)

// ---------------------------------------------------------------------------
// bringUpExec — initial resize is gated by both Cols>0 and Rows>0
// (tunnel.go:191)
// ---------------------------------------------------------------------------

// TestBringUpExecInitialResizeGatedByColsAndRows proves the exact boundary of
// `msg.Cols > 0 && msg.Rows > 0`: a zero on either side must suppress the
// initial ResizeExec call, and positive values on both sides must trigger it.
// CONDITIONALS_BOUNDARY (`>` -> `>=`) on either operand would call ResizeExec
// with a zero dimension.
func TestBringUpExecInitialResizeGatedByColsAndRows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		cols, rows int
		wantResize bool
	}{
		{"cols zero", 0, 24, false},
		{"rows zero", 80, 0, false},
		{"both positive", 80, 24, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, ctrl := newTestClient(t)
			execConn := &fakeConn{blockRead: make(chan struct{})}
			fd := &fakeDocker{
				createExecID: "docker-" + tc.name,
				startConn:    execConn,
			}
			c.dockerClient = fd

			c.StartExec(context.Background(), protocol.ExecStartMessage{
				ExecID:      "rz-" + tc.name,
				ContainerID: "c1",
				Cmd:         []string{"sh"},
				Cols:        tc.cols,
				Rows:        tc.rows,
			})

			// exec_ready always arrives (the resize is best-effort and never
			// blocks bring-up); it's a reliable barrier proving bringUpExec has
			// already decided whether to call ResizeExec.
			expectType(t, ctrl, protocol.TypeExecReady)

			fd.mu.Lock()
			gotResize := len(fd.resizeCalls) > 0
			fd.mu.Unlock()
			if gotResize != tc.wantResize {
				t.Errorf("ResizeExec called = %v, want %v (cols=%d rows=%d)", gotResize, tc.wantResize, tc.cols, tc.rows)
			}

			close(execConn.blockRead)
			expectType(t, ctrl, protocol.TypeExecEnd)
		})
	}
}

// ---------------------------------------------------------------------------
// writeInput — fixed retry budget and backoff (tunnel.go:329, 333, 338)
// ---------------------------------------------------------------------------

// TestWriteInputRetriesExactlyTenTimesThenCloses proves writeInput makes
// exactly 10 write attempts, spaced by real 50ms waits, before giving up and
// closing the session. This pins CONDITIONALS_BOUNDARY (`attempt < 10` ->
// `<= 10`, which would allow an 11th attempt) and the retry-delay
// ARITHMETIC_BASE (`50 * time.Millisecond` -> `/`, which would make every
// retry near-instant instead of ~50ms apart) via the elapsed-time floor.
func TestWriteInputRetriesExactlyTenTimesThenCloses(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)
	conn := &gatedErrConn{gate: make(chan struct{})}
	close(conn.gate) // never block: every Write fails immediately
	s := newExecSession(c, "wi-retry-count", conn)

	// Safety net: if a mutant makes the retry loop not terminate on its own
	// (e.g. a decrementing attempt counter), force it closed so the test
	// fails on the assertions below instead of hanging the suite.
	safety := time.AfterFunc(5*time.Second, func() { s.Close() })
	defer safety.Stop()

	start := time.Now()
	s.writeInput([]byte("x"))
	elapsed := time.Since(start)

	if got := conn.attempts(); got != 10 {
		t.Errorf("write attempts = %d, want exactly 10", got)
	}
	// Timing-based: tunnel.go has no injectable sleep/after hook to count
	// calls/durations against instead, so this measures wall time. 9
	// inter-attempt waits of ~50ms real delay is ~450ms; a 400ms floor stays
	// well clear of scheduler jitter on the real path while still failing
	// hard against a near-instant `/` mutant on the retry delay.
	if elapsed < 400*time.Millisecond {
		t.Errorf("writeInput returned after %v, want at least ~400ms (9 x 50ms retry waits)", elapsed)
	}
	select {
	case <-s.done:
	default:
		t.Error("session was not closed after exhausting retries")
	}
}

// TestWriteInputRetryLogsOneBasedAttemptNumber covers the ARITHMETIC_BASE on
// the retry log's `attempt+1` (tunnel.go:333): the loop variable is
// zero-based, so the first logged attempt must read 1, not -1 or 0.
func TestWriteInputRetryLogsOneBasedAttemptNumber(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	c, _ := newTestClient(t)
	conn := &gatedErrConn{gate: make(chan struct{})}
	close(conn.gate)
	s := newExecSession(c, "wi-retry-log", conn)
	defer s.Close()

	logBuf := &bytes.Buffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(oldLogger)

	// Stop after the first attempt is logged instead of running all 10.
	go func() {
		waitFor(t, "first write attempt", func() bool { return conn.attempts() >= 1 })
		s.Close()
	}()
	s.writeInput([]byte("x"))

	// Check the FIRST logged attempt value, not whether "attempt=1" appears
	// anywhere: under scheduler starvation the background Close() above can
	// lag past more than one retry, and an attempt-1 mutant's third
	// iteration logs "attempt=1" too (attempt=2, attempt-1=1), which would
	// let a substring-anywhere check pass against the mutant.
	if got := firstLogAttempt(logBuf.String()); got != "1" {
		t.Errorf("first logged attempt = %q, want %q (1-based)", got, "1")
	}
}

// firstLogAttempt returns the value of the first "attempt=" field in the
// first log line that has one, or "" if none is found.
func firstLogAttempt(out string) string {
	for _, line := range strings.Split(out, "\n") {
		idx := strings.Index(line, "attempt=")
		if idx == -1 {
			continue
		}
		rest := line[idx+len("attempt="):]
		if sp := strings.IndexByte(rest, ' '); sp != -1 {
			rest = rest[:sp]
		}
		return rest
	}
	return ""
}

// ---------------------------------------------------------------------------
// doResize — fixed retry budget, backoff, and logging (tunnel.go:374, 378, 385)
// ---------------------------------------------------------------------------

// TestDoResizeRetriesExactlyTenTimesThenGivesUp is the doResize counterpart of
// TestWriteInputRetriesExactlyTenTimesThenCloses: it pins the exact 10-attempt
// budget (CONDITIONALS_BOUNDARY, INCREMENT_DECREMENT on the loop) and the
// ~50ms real retry spacing (ARITHMETIC_BASE on tunnel.go:385) via an
// elapsed-time floor. A context far longer than the expected ~450ms real
// runtime bounds the worst case (a decrementing attempt counter looping
// indefinitely) instead of hanging the suite.
func TestDoResizeRetriesExactlyTenTimesThenGivesUp(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)
	fd := &fakeDocker{resizeFailFirst: 99} // always fails
	c.dockerClient = fd
	s := &ExecSession{execID: "rz-count", client: c, dockerExecID: "docker-rz-count", done: make(chan struct{})}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	s.doResize(ctx, resizeOp{cols: 80, rows: 24})
	elapsed := time.Since(start)

	fd.mu.Lock()
	attempts := fd.resizeAttempts
	fd.mu.Unlock()
	if attempts != 10 {
		t.Errorf("ResizeExec attempts = %d, want exactly 10", attempts)
	}
	// Timing-based: see the matching comment in
	// TestWriteInputRetriesExactlyTenTimesThenCloses — no injectable clock,
	// so a 400ms floor stays close to the real ~450ms while still catching a
	// near-instant `/` mutant on the retry delay.
	if elapsed < 400*time.Millisecond {
		t.Errorf("doResize returned after %v, want at least ~400ms (9 x 50ms retry waits)", elapsed)
	}
}

// TestDoResizeRetryLogsOneBasedAttemptNumber covers the ARITHMETIC_BASE on
// doResize's retry log `attempt+1` (tunnel.go:378).
func TestDoResizeRetryLogsOneBasedAttemptNumber(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	c, _ := newTestClient(t)
	fd := &fakeDocker{resizeFailFirst: 1} // fails once, then succeeds
	c.dockerClient = fd
	s := &ExecSession{execID: "rz-log", client: c, dockerExecID: "docker-rz-log", done: make(chan struct{})}

	logBuf := &bytes.Buffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(oldLogger)

	s.doResize(context.Background(), resizeOp{cols: 1, rows: 1})

	if !strings.Contains(logBuf.String(), "attempt=1") {
		t.Errorf("retry log = %q, want it to contain attempt=1 (1-based)", logBuf.String())
	}
}

// ---------------------------------------------------------------------------
// recoverSession — panic is logged (tunnel.go:539)
// ---------------------------------------------------------------------------

// TestRecoverSessionLogsRecoveredPanic covers the CONDITIONALS_NEGATION on
// `r != nil` after recover(): recover() itself already stops the panic from
// propagating regardless of what's done with its result, so the log is the
// only observable difference between the real code and the mutant.
func TestRecoverSessionLogsRecoveredPanic(t *testing.T) {
	// Deliberately NOT t.Parallel(): captures the process-global slog default.

	logBuf := &bytes.Buffer{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverSession("test-goroutine", "panic-eid")
		panic("boom")
	}()
	<-done

	out := logBuf.String()
	if !strings.Contains(out, "recovered from panic") {
		t.Errorf("log = %q, want the panic-recovery message", out)
	}
	if !strings.Contains(out, "panic-eid") {
		t.Errorf("log = %q, want it to contain the execID", out)
	}
}

// ---------------------------------------------------------------------------
// enqueueInput — byte reservation rollback on a full inbox (tunnel.go:280,
// extra-mutator run)
// ---------------------------------------------------------------------------

// TestEnqueueInputRollsBackReservationOnQueueFull covers `s.queuedInputBytes
// -= len(data)` in the inputQueueFull branch. It fills a small inbox to
// capacity, then enqueues one more chunk that must be rejected because the
// channel, not the byte budget, is full.
//
// Correct code rolls the reservation back to exactly the accepted chunks'
// bytes. INVERT_ASSIGNMENTS (`-=` -> `+=`) would double-reserve the rejected
// chunk's bytes on top of that; REMOVE_SELF_ASSIGNMENTS (`-=` -> `=`) would
// discard the prior reservation and set it to just the rejected chunk's
// size. All three land on different values.
func TestEnqueueInputRollsBackReservationOnQueueFull(t *testing.T) {
	t.Parallel()

	s := &ExecSession{inbox: make(chan execItem, 2)}
	data := []byte("abcde") // 5 bytes

	for i := 0; i < cap(s.inbox); i++ {
		if got := s.enqueueInput(data); got != inputEnqueued {
			t.Fatalf("enqueueInput %d = %d, want inputEnqueued", i, got)
		}
	}

	if got := s.enqueueInput(data); got != inputQueueFull {
		t.Fatalf("enqueueInput on a full inbox = %d, want inputQueueFull", got)
	}

	s.mu.Lock()
	got := s.queuedInputBytes
	s.mu.Unlock()
	want := len(data) * cap(s.inbox)
	if got != want {
		t.Fatalf("queuedInputBytes = %d, want %d: the rejected enqueue's reservation must roll back exactly", got, want)
	}
}

// ---------------------------------------------------------------------------
// releaseInputBytes — subtracts across multiple calls (tunnel.go:290,
// extra-mutator run)
// ---------------------------------------------------------------------------

// TestReleaseInputBytesSubtractsAcrossMultipleCalls covers
// `s.queuedInputBytes -= n`. REMOVE_SELF_ASSIGNMENTS (`-=` -> `=`) would make
// each call overwrite the budget with just its own n instead of subtracting
// from the running total, so two releases in a row expose the bug that a
// single release cannot.
func TestReleaseInputBytesSubtractsAcrossMultipleCalls(t *testing.T) {
	t.Parallel()

	s := &ExecSession{queuedInputBytes: 30}
	s.releaseInputBytes(10)
	s.releaseInputBytes(7)

	s.mu.Lock()
	got := s.queuedInputBytes
	s.mu.Unlock()
	if got != 13 {
		t.Fatalf("queuedInputBytes = %d, want 13 after releasing 10 then 7 from 30", got)
	}
}
