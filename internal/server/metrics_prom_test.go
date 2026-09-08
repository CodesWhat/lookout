package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/docker"
	"github.com/codeswhat/portwing/internal/metrics"
	"github.com/codeswhat/portwing/internal/protocol"
)

// shortSocketPath returns a socket path short enough for darwin's 104-byte
// sun_path limit. os.MkdirTemp("", "lk") produces a much shorter path than
// t.TempDir(), which embeds the full test name.
func shortSocketPath(t *testing.T) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "lk")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	return filepath.Join(dir, "d.sock"), cleanup
}

// newStubMetricsDockerClient builds a stub Docker Unix socket server whose
// container list and per-container stats are configurable via the provided
// handlers. Pass a nil statsHandler to simulate a failing stats endpoint.
func newStubMetricsDockerClient(
	t *testing.T,
	containers []docker.ContainerJSON,
	statsHandler func(id string) (*docker.ContainerStatsResponse, bool),
) (*docker.Client, func()) {
	t.Helper()

	sockPath, cleanup := shortSocketPath(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		cleanup()
		t.Fatalf("listen on unix socket: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.VersionResponse{
			Version:    "26.0.0",
			APIVersion: "1.44",
		})
	})

	mux.HandleFunc("/v1.44/containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(containers)
	})

	// Handle /v1.44/containers/{id}/stats
	mux.HandleFunc("/v1.44/containers/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Extract id from /v1.44/containers/{id}/stats
		trimmed := strings.TrimPrefix(path, "/v1.44/containers/")
		parts := strings.SplitN(trimmed, "/", 2)
		if len(parts) != 2 || parts[1] != "stats" {
			http.NotFound(w, r)
			return
		}
		id := parts[0]
		if statsHandler == nil {
			http.Error(w, "stats unavailable", http.StatusInternalServerError)
			return
		}
		stats, ok := statsHandler(id)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats)
	})

	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(listener)
	}()

	client, err := docker.NewClient(sockPath, 5)
	if err != nil {
		_ = srv.Close()
		cleanup()
		t.Fatalf("new docker client: %v", err)
	}

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = listener.Close()
		<-done
		cleanup()
	}

	return client, shutdown
}

// makeTestServer builds a minimal Server sufficient for handleMetrics tests.
func makeTestServer(dockerClient *docker.Client) *Server {
	return &Server{
		dockerClient: dockerClient,
		collector:    metrics.NewCollector("/tmp", true), // skip disk
		metrics:      metrics.NewRegistry(),
		startTime:    time.Now().Add(-5 * time.Second),
	}
}

func TestHandleMetricsBuildInfo(t *testing.T) {
	t.Parallel()

	containers := []docker.ContainerJSON{}
	client, shutdown := newStubMetricsDockerClient(t, containers, nil)
	defer shutdown()

	s := makeTestServer(client)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	s.handleMetrics(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()

	if !strings.Contains(body, `portwing_build_info{version="`+protocol.AgentVersion+`"} 1`) {
		t.Errorf("missing build info line; got:\n%s", body)
	}
	if !strings.Contains(body, "portwing_uptime_seconds ") {
		t.Errorf("missing uptime line; got:\n%s", body)
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("unexpected Content-Type: %s", ct)
	}
}

func TestHandleMetricsContainerSeries(t *testing.T) {
	t.Parallel()

	containers := []docker.ContainerJSON{
		{
			ID:    "abc123def456",
			Names: []string{"/web"},
			Image: "nginx:latest",
		},
	}

	statsHandler := func(id string) (*docker.ContainerStatsResponse, bool) {
		if id != "abc123def456" {
			return nil, false
		}
		s := &docker.ContainerStatsResponse{}
		s.CPUStats.CPUUsage.TotalUsage = 5_000_000_000 // 5 seconds
		s.MemoryStats.Usage = 104857600                // 100 MiB
		s.MemoryStats.Limit = 536870912                // 512 MiB
		s.Networks = map[string]struct {
			RxBytes uint64 `json:"rx_bytes"`
			TxBytes uint64 `json:"tx_bytes"`
		}{
			"eth0": {RxBytes: 1024, TxBytes: 512},
			"eth1": {RxBytes: 256, TxBytes: 128},
		}
		return s, true
	}

	client, shutdown := newStubMetricsDockerClient(t, containers, statsHandler)
	defer shutdown()

	s := makeTestServer(client)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	s.handleMetrics(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()

	// CPU: 5e9 ns / 1e9 = 5 seconds
	if !strings.Contains(body, `container_cpu_usage_seconds_total{id="abc123def456",name="web",image="nginx:latest"} 5`) {
		t.Errorf("missing/wrong CPU series; got:\n%s", body)
	}

	if !strings.Contains(body, `container_memory_usage_bytes{id="abc123def456",name="web",image="nginx:latest"} 104857600`) {
		t.Errorf("missing/wrong memory usage series; got:\n%s", body)
	}

	if !strings.Contains(body, `container_spec_memory_limit_bytes{id="abc123def456",name="web",image="nginx:latest"} 536870912`) {
		t.Errorf("missing/wrong memory limit series; got:\n%s", body)
	}

	// rx: 1024+256=1280, tx: 512+128=640
	if !strings.Contains(body, `container_network_receive_bytes_total{id="abc123def456",name="web",image="nginx:latest"} 1280`) {
		t.Errorf("missing/wrong rx series; got:\n%s", body)
	}
	if !strings.Contains(body, `container_network_transmit_bytes_total{id="abc123def456",name="web",image="nginx:latest"} 640`) {
		t.Errorf("missing/wrong tx series; got:\n%s", body)
	}
}

func TestHandleMetricsMemoryLimitZeroOmitted(t *testing.T) {
	t.Parallel()

	containers := []docker.ContainerJSON{
		{ID: "nolimit1", Names: []string{"/nolimit"}, Image: "busybox:latest"},
	}

	statsHandler := func(id string) (*docker.ContainerStatsResponse, bool) {
		s := &docker.ContainerStatsResponse{}
		s.MemoryStats.Usage = 1024
		s.MemoryStats.Limit = 0 // no limit — must be omitted
		return s, true
	}

	client, shutdown := newStubMetricsDockerClient(t, containers, statsHandler)
	defer shutdown()

	s := makeTestServer(client)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	s.handleMetrics(rr, req)

	body := rr.Body.String()
	if strings.Contains(body, `container_spec_memory_limit_bytes{id="nolimit1"`) {
		t.Errorf("expected limit series to be omitted when limit=0; got:\n%s", body)
	}
}

func TestHandleMetricsFailingContainerSkipped(t *testing.T) {
	t.Parallel()

	containers := []docker.ContainerJSON{
		{ID: "good1", Names: []string{"/good"}, Image: "alpine:latest"},
		{ID: "bad1", Names: []string{"/bad"}, Image: "broken:latest"},
	}

	statsHandler := func(id string) (*docker.ContainerStatsResponse, bool) {
		if id == "bad1" {
			// Simulate failing stats endpoint for this container.
			return nil, false
		}
		s := &docker.ContainerStatsResponse{}
		s.CPUStats.CPUUsage.TotalUsage = 1_000_000_000
		s.MemoryStats.Usage = 4096
		return s, true
	}

	client, shutdown := newStubMetricsDockerClient(t, containers, statsHandler)
	defer shutdown()

	s := makeTestServer(client)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	s.handleMetrics(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()

	// good container must appear
	if !strings.Contains(body, `id="good1"`) {
		t.Errorf("good container missing from output; got:\n%s", body)
	}
	// bad container must not appear
	if strings.Contains(body, `id="bad1"`) {
		t.Errorf("bad container should be skipped but appeared in output; got:\n%s", body)
	}
}

func TestHandleMetricsLabelEscaping(t *testing.T) {
	t.Parallel()

	// Image name with backslash, double-quote, and newline characters.
	weirdImage := `a\b"c` + "\nd"
	containers := []docker.ContainerJSON{
		{ID: "esc1", Names: []string{`/normal`}, Image: weirdImage},
	}

	statsHandler := func(id string) (*docker.ContainerStatsResponse, bool) {
		s := &docker.ContainerStatsResponse{}
		s.MemoryStats.Usage = 512
		return s, true
	}

	client, shutdown := newStubMetricsDockerClient(t, containers, statsHandler)
	defer shutdown()

	s := makeTestServer(client)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	s.handleMetrics(rr, req)

	body := rr.Body.String()

	// The raw unescaped image must not appear in the output.
	if strings.Contains(body, weirdImage) {
		t.Errorf("unescaped label value found in output; got:\n%s", body)
	}
	// The escaped form must appear.
	escaped := `a\\b\"c\nd`
	if !strings.Contains(body, escaped) {
		t.Errorf("escaped label value %q not found in output; got:\n%s", escaped, body)
	}
}

func TestEscapeLabelValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{`hello`, `hello`},
		{`a\b`, `a\\b`},
		{`a"b`, `a\"b`},
		{"a\nb", `a\nb`},
		{`a\b"c` + "\nd", `a\\b\"c\nd`},
	}
	for _, c := range cases {
		got := metrics.EscapeLabelValue(c.in)
		if got != c.want {
			t.Errorf("EscapeLabelValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestHandleMetricsWithoutDockerClient covers the Server that has no Docker
// client: the scrape must serve the rest of the exposition without the
// container series. The nil client is dropped while it is still a typed
// pointer, because the collector's interface field would read it as non-nil
// and the collection runs on a goroutine of its own, where the resulting nil
// dereference would take the agent down instead of failing one scrape.
func TestHandleMetricsWithoutDockerClient(t *testing.T) {
	t.Parallel()

	s := makeTestServer(nil)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	s.handleMetrics(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `portwing_build_info{version="`+protocol.AgentVersion+`"} 1`) {
		t.Errorf("missing build info line; got:\n%s", body)
	}
	if strings.Contains(body, "container_") {
		t.Errorf("container series emitted without a Docker client; got:\n%s", body)
	}
}
