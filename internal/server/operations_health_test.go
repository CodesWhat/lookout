package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/docker"
	"github.com/codeswhat/portwing/internal/protocol"
)

type operationalHealthResponse struct {
	Status        string  `json:"status"`
	Live          bool    `json:"live"`
	Ready         bool    `json:"ready"`
	Mode          string  `json:"mode"`
	Version       string  `json:"version"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
	Docker        string  `json:"docker"`
	Controller    string  `json:"controller"`
}

func TestLivenessResponseIsProcessOnly(t *testing.T) {
	t.Parallel()

	s := &Server{startTime: time.Now().Add(-3 * time.Second)}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	s.handleSimpleHealth(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got operationalHealthResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode liveness response: %v", err)
	}
	if got.Status != "ok" || !got.Live || got.Ready {
		t.Fatalf("liveness = %+v, want live process without readiness claim", got)
	}
	if got.Mode != "standard" || got.Version != protocol.AgentVersion {
		t.Fatalf("mode/version = %q/%q, want standard/%q", got.Mode, got.Version, protocol.AgentVersion)
	}
	if got.UptimeSeconds < 2.5 || got.Docker != "unknown" || got.Controller != "not_applicable" {
		t.Fatalf("operational fields = %+v", got)
	}
}

func TestReadinessResponseIncludesDockerState(t *testing.T) {
	t.Parallel()

	client, stop := newDockerClientWithPing(t, true)
	defer stop()

	s := &Server{dockerClient: client, startTime: time.Now().Add(-2 * time.Second)}
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rr := httptest.NewRecorder()
	s.handleHealth(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got operationalHealthResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode readiness response: %v", err)
	}
	if got.Status != "healthy" || !got.Live || !got.Ready {
		t.Fatalf("readiness = %+v, want healthy/live/ready", got)
	}
	if got.Mode != "standard" || got.Docker != "connected" || got.Controller != "not_applicable" {
		t.Fatalf("operational fields = %+v", got)
	}
}

// newCountingPingDaemon starts a fake Docker daemon on a Unix socket that
// counts /_ping requests and, while hold is open, blocks each one. It is the
// slow-daemon case the readiness bound exists for: the ping never answers, so
// an unbounded handler would sit on a Docker connection until the client gave
// up.
func newCountingPingDaemon(t *testing.T, hold <-chan struct{}) (*docker.Client, *atomic.Int64, func()) {
	t.Helper()

	var pings atomic.Int64
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
		pings.Add(1)
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	client, stop := newDockerClientOnListener(t, mux, sockPath, listener, cleanup)
	return client, &pings, stop
}

// TestReadinessPingIsBoundedAndCollapsed is a regression test for the
// unauthenticated readiness fan-out: /ready and /_portwing/health pinged
// Docker synchronously with the caller's own context and no cap, so a slow
// daemon let remote callers accumulate one handler and one Docker connection
// per request. Concurrent readiness requests must produce a single ping, and
// each must return within the per-ping timeout.
//
// Not t.Parallel(): it reassigns the package-level readinessPing* vars, which
// every readiness handler reads.
func TestReadinessPingIsBoundedAndCollapsed(t *testing.T) {
	origTimeout, origTTL := readinessPingTimeout, readinessPingTTL
	readinessPingTimeout = 200 * time.Millisecond
	readinessPingTTL = 50 * time.Millisecond
	t.Cleanup(func() {
		readinessPingTimeout, readinessPingTTL = origTimeout, origTTL
	})

	hold := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(hold) }) }
	t.Cleanup(release)

	client, pings, stop := newCountingPingDaemon(t, hold)
	defer stop()

	s := &Server{dockerClient: client, startTime: time.Now()}

	const callers = 8
	codes := make([]int, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	start := time.Now()
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			rr := httptest.NewRecorder()
			s.handleHealth(rr, httptest.NewRequest(http.MethodGet, "/ready", nil))
			codes[i] = rr.Code
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// The ping never answers, so every caller must be released by the
	// timeout rather than by the daemon.
	if elapsed > 5*time.Second {
		t.Fatalf("%d concurrent readiness requests took %s against a hung daemon", callers, elapsed)
	}
	for i, code := range codes {
		if code != http.StatusServiceUnavailable {
			t.Errorf("readiness caller %d = %d, want 503 while the daemon is hung", i, code)
		}
	}
	if got := pings.Load(); got != 1 {
		t.Fatalf("%d concurrent readiness requests produced %d Docker pings, want 1", callers, got)
	}
	release()
}

// TestReadinessPingResultIsCachedBriefly verifies the other half of the
// bound: back-to-back readiness requests reuse one ping result, and the cache
// expires so readiness cannot go stale indefinitely.
//
// Not t.Parallel(): see TestReadinessPingIsBoundedAndCollapsed.
func TestReadinessPingResultIsCachedBriefly(t *testing.T) {
	origTimeout, origTTL := readinessPingTimeout, readinessPingTTL
	readinessPingTimeout = 2 * time.Second
	readinessPingTTL = 60 * time.Millisecond
	t.Cleanup(func() {
		readinessPingTimeout, readinessPingTTL = origTimeout, origTTL
	})

	client, pings, stop := newCountingPingDaemon(t, nil)
	defer stop()

	s := &Server{dockerClient: client, startTime: time.Now()}
	probe := func() int {
		rr := httptest.NewRecorder()
		s.handleHealth(rr, httptest.NewRequest(http.MethodGet, "/ready", nil))
		return rr.Code
	}

	for i := 0; i < 3; i++ {
		if code := probe(); code != http.StatusOK {
			t.Fatalf("readiness request %d = %d, want 200", i, code)
		}
	}
	if got := pings.Load(); got != 1 {
		t.Fatalf("three readiness requests inside the TTL produced %d Docker pings, want 1", got)
	}

	time.Sleep(readinessPingTTL + 40*time.Millisecond)
	if code := probe(); code != http.StatusOK {
		t.Fatalf("readiness request after the TTL = %d, want 200", code)
	}
	if got := pings.Load(); got != 2 {
		t.Fatalf("readiness after the TTL produced %d Docker pings in total, want 2", got)
	}
}

// probeInflight reports whether the probe still holds an unfinished flight.
func probeInflight(p *healthProbe) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight != nil
}

// TestReadinessProbeReleasesFlightOnPanic is a regression test for a wedge:
// the probe published its result and closed the in-flight channel on the
// normal return path only, so a ping that panicked left inflight set forever
// and every later readiness request blocked on a channel nothing would close.
// One panicking request would have taken the readiness endpoint down for the
// life of the process.
//
// Not t.Parallel(): it reassigns the package-level readinessPing* vars.
func TestReadinessProbeReleasesFlightOnPanic(t *testing.T) {
	origTimeout, origTTL := readinessPingTimeout, readinessPingTTL
	readinessPingTimeout = 2 * time.Second
	readinessPingTTL = time.Millisecond
	t.Cleanup(func() {
		readinessPingTimeout, readinessPingTTL = origTimeout, origTTL
	})

	var probe healthProbe

	// A waiter that arrives while the panicking ping is running must be
	// released by it, not left blocked. Its own context is deliberately not
	// cancellable, so only the flight handoff can free it.
	entered := make(chan struct{})
	unblock := make(chan struct{})
	waiterDone := make(chan error, 1)

	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_ = probe.check(context.Background(), func(context.Context) error {
			close(entered)
			<-unblock
			panic("docker ping exploded")
		})
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the panicking ping never started")
	}
	go func() { waiterDone <- probe.check(context.Background(), func(context.Context) error { return nil }) }()
	// Give the waiter time to reach the in-flight wait. Too short only makes
	// this assertion weaker, never wrong.
	time.Sleep(50 * time.Millisecond)
	close(unblock)

	if got := <-panicked; got == nil {
		t.Fatal("the panic was swallowed; it must still reach RecoveryMiddleware")
	}
	select {
	case err := <-waiterDone:
		if err == nil {
			t.Fatal("waiter got a healthy result from a ping that panicked")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a caller waiting on the panicking ping was never released")
	}

	// The flight must be gone, or the next request wedges on the same channel.
	if probeInflight(&probe) {
		t.Fatal("the probe still holds an in-flight ping after the ping panicked")
	}

	// And behaviourally: once the cached result expires, a later request runs
	// its own ping and gets a real answer.
	time.Sleep(readinessPingTTL + 20*time.Millisecond)
	pinged := false
	if err := probe.check(context.Background(), func(context.Context) error {
		pinged = true
		return nil
	}); err != nil {
		t.Fatalf("readiness check after a panicking ping returned %v, want nil", err)
	}
	if !pinged {
		t.Fatal("the readiness check after the TTL did not run its own ping")
	}
}

// TestReadinessProbeWaiterHonoursCanceledContext covers the waiter's
// cancellation branch: a readiness client queued behind an in-flight ping that
// hangs up must be released by its own context rather than waiting out the
// ping it never asked for. Cancelling before the call makes the select
// deterministic — the leader's flight channel is still open, so only ctx.Done
// is ready.
func TestReadinessProbeWaiterHonoursCanceledContext(t *testing.T) {
	t.Parallel()

	var probe healthProbe

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLeader := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseLeader)

	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- probe.check(context.Background(), func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the leading ping never started")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := probe.check(ctx, func(context.Context) error {
		t.Error("a waiter must not start its own ping while one is in flight")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter with a canceled context returned %v, want context.Canceled", err)
	}

	releaseLeader()
	select {
	case err := <-leaderDone:
		if err != nil {
			t.Fatalf("the leading ping returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the leading ping never finished")
	}
}
