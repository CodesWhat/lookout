package server

import (
	"encoding/json"
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
func TestReadinessPingIsBoundedAndCollapsed(t *testing.T) {

	hold := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(hold) }) }
	t.Cleanup(release)

	client, pings, stop := newCountingPingDaemon(t, hold)
	defer stop()

	s := &Server{dockerClient: client, startTime: time.Now(), readiness: docker.HealthProbe{Timeout: 200 * time.Millisecond, TTL: 5 * time.Second}}

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
func TestReadinessPingResultIsCachedBriefly(t *testing.T) {

	client, pings, stop := newCountingPingDaemon(t, nil)
	defer stop()

	s := &Server{dockerClient: client, startTime: time.Now(), readiness: docker.HealthProbe{Timeout: 2 * time.Second, TTL: 60 * time.Millisecond}}
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

	time.Sleep(s.readiness.TTL + 40*time.Millisecond)
	if code := probe(); code != http.StatusOK {
		t.Fatalf("readiness request after the TTL = %d, want 200", code)
	}
	if got := pings.Load(); got != 2 {
		t.Fatalf("readiness after the TTL produced %d Docker pings in total, want 2", got)
	}
}
