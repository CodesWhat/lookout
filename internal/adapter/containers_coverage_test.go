package adapter

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/docker"
)

// newErrorDockerClient builds a stub docker client whose /containers/json
// always returns the given HTTP status code (and whose /containers/<id>/json
// optionally errors).
//
// listStatus controls the containers/json endpoint.
// inspectStatus, if non-zero, controls every containers/<id>/json endpoint.
// When inspectStatus is 0 the inspect endpoint falls through to the provided
// listed containers (returning a minimal valid inspect body).
func newErrorDockerClient(
	t *testing.T,
	listStatus int,
	inspectStatus int,
	listed []docker.ContainerJSON,
) (*docker.Client, func()) {
	t.Helper()

	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
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
		if listStatus != 0 {
			http.Error(w, "internal error", listStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listed)
	})

	mux.HandleFunc("/v1.44/containers/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/json") {
			http.NotFound(w, r)
			return
		}
		if inspectStatus != 0 {
			http.Error(w, "inspect error", inspectStatus)
			return
		}

		id := strings.TrimPrefix(r.URL.Path, "/v1.44/containers/")
		id = strings.TrimSuffix(id, "/json")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.ContainerInspect{
			ID:      id,
			Name:    "/" + id,
			Created: time.Now().UTC().Format(time.RFC3339),
			State: docker.ContainerState{
				Status:    "running",
				Running:   true,
				StartedAt: time.Now().UTC().Format(time.RFC3339),
			},
			Config: docker.ContainerConfig{
				Image:  "nginx:latest",
				Labels: map[string]string{"test.id": id},
			},
			NetworkSettings: &docker.NetworkSettings{
				Networks: map[string]docker.NetworkEndpoint{},
			},
		})
	})

	server := &http.Server{Handler: mux}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = server.Serve(listener)
	}()

	client, err := docker.NewClient(socketPath, 2)
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
		<-serverDone
	}

	return client, shutdown
}

// newSelectiveInspectFailureDockerClient builds a stub docker client whose
// /containers/json returns the given listed containers, and whose
// /containers/<id>/json returns 500 for failID but a minimal valid inspect
// body for every other ID. Used to distinguish "skip this one and keep
// processing the rest of the list" (continue) from "stop processing the list
// entirely" (break).
func newSelectiveInspectFailureDockerClient(
	t *testing.T,
	listed []docker.ContainerJSON,
	failID string,
) (*docker.Client, func()) {
	t.Helper()

	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
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
		_ = json.NewEncoder(w).Encode(listed)
	})

	mux.HandleFunc("/v1.44/containers/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/json") {
			http.NotFound(w, r)
			return
		}

		id := strings.TrimPrefix(r.URL.Path, "/v1.44/containers/")
		id = strings.TrimSuffix(id, "/json")

		if id == failID {
			http.Error(w, "inspect error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.ContainerInspect{
			ID:      id,
			Name:    "/" + id,
			Created: time.Now().UTC().Format(time.RFC3339),
			State: docker.ContainerState{
				Status:    "running",
				Running:   true,
				StartedAt: time.Now().UTC().Format(time.RFC3339),
			},
			Config: docker.ContainerConfig{
				Image:  "nginx:latest",
				Labels: map[string]string{"test.id": id},
			},
			NetworkSettings: &docker.NetworkSettings{
				Networks: map[string]docker.NetworkEndpoint{},
			},
		})
	})

	server := &http.Server{Handler: mux}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = server.Serve(listener)
	}()

	client, err := docker.NewClient(socketPath, 2)
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
		<-serverDone
	}

	return client, shutdown
}

// ---------------------------------------------------------------------------
// BuildInventory error paths
// ---------------------------------------------------------------------------

// TestBuildInventoryListError covers containers.go:53-55 — ListContainers
// returns an error, BuildInventory propagates it.
func TestBuildInventoryListError(t *testing.T) {
	t.Parallel()

	client, shutdown := newErrorDockerClient(t, http.StatusInternalServerError, 0, nil)
	defer shutdown()

	manager := NewContainerManager(client, "test-agent", nil)
	_, err := manager.BuildInventory(context.Background())
	if err == nil {
		t.Fatal("expected error from BuildInventory when list fails, got nil")
	}
	if !strings.Contains(err.Error(), "listing containers") {
		t.Fatalf("expected error to contain 'listing containers', got: %v", err)
	}
}

// TestBuildInventoryInspectError covers containers.go:62-64 — a container is
// listed but InspectContainer returns 500; the container must be skipped and
// the result should be empty (not an error).
func TestBuildInventoryInspectError(t *testing.T) {
	t.Parallel()

	listed := []docker.ContainerJSON{
		{
			ID:      "bad-container",
			Image:   "nginx:latest",
			ImageID: "sha256:abc",
			Labels:  map[string]string{},
		},
	}

	client, shutdown := newErrorDockerClient(t, 0, http.StatusInternalServerError, listed)
	defer shutdown()

	manager := NewContainerManager(client, "test-agent", nil)
	containers, err := manager.BuildInventory(context.Background())
	if err != nil {
		t.Fatalf("expected no error from BuildInventory when only inspect fails, got: %v", err)
	}
	if len(containers) != 0 {
		t.Fatalf("expected 0 containers (skipped on inspect error), got %d", len(containers))
	}
}

// TestBuildInventorySkipsFailedInspectAndContinuesToNextContainer covers
// containers.go:64 — the loop must skip a container whose inspect fails and
// continue to the next entry, not abort the whole list (continue vs break).
// A single-container list can't distinguish the two, so this uses two: the
// first fails inspect, the second must still make it into the result.
func TestBuildInventorySkipsFailedInspectAndContinuesToNextContainer(t *testing.T) {
	t.Parallel()

	listed := []docker.ContainerJSON{
		{ID: "bad-container", Image: "nginx:latest", ImageID: "sha256:bad", Labels: map[string]string{}},
		{ID: "good-container", Image: "redis:7", ImageID: "sha256:good", Labels: map[string]string{}},
	}

	client, shutdown := newSelectiveInspectFailureDockerClient(t, listed, "bad-container")
	defer shutdown()

	manager := NewContainerManager(client, "test-agent", nil)
	containers, err := manager.BuildInventory(context.Background())
	if err != nil {
		t.Fatalf("expected no error from BuildInventory when one inspect fails, got: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("expected 1 container after skipping the failed one, got %d", len(containers))
	}
	if containers[0].ID != "good-container" {
		t.Fatalf("expected surviving container to be good-container, got %q", containers[0].ID)
	}
}

// ---------------------------------------------------------------------------
// GetContainer not-found path
// ---------------------------------------------------------------------------

// TestGetContainerNotFound covers containers.go:96-98 — looking up an ID that
// is not in the map returns nil, false.
func TestGetContainerNotFound(t *testing.T) {
	t.Parallel()

	manager := NewContainerManager(nil, "test-agent", nil)
	c, ok := manager.GetContainer("nonexistent")
	if ok {
		t.Fatal("expected ok=false for unknown container ID")
	}
	if c != nil {
		t.Fatalf("expected nil container pointer, got %+v", c)
	}
}

// ---------------------------------------------------------------------------
// Refresh error paths
// ---------------------------------------------------------------------------

// TestRefreshListError covers containers.go:108-110 — ListContainers fails
// during Refresh.
func TestRefreshListError(t *testing.T) {
	t.Parallel()

	client, shutdown := newErrorDockerClient(t, http.StatusInternalServerError, 0, nil)
	defer shutdown()

	manager := NewContainerManager(client, "test-agent", nil)
	_, _, _, err := manager.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected error from Refresh when list fails, got nil")
	}
	if !strings.Contains(err.Error(), "listing containers") {
		t.Fatalf("expected error to contain 'listing containers', got: %v", err)
	}
}

// TestRefreshInspectError covers containers.go:123-125 — a container appears
// in the list but InspectContainer fails; it must be silently skipped.
func TestRefreshInspectError(t *testing.T) {
	t.Parallel()

	listed := []docker.ContainerJSON{
		{
			ID:      "bad-c",
			Image:   "nginx:latest",
			ImageID: "sha256:bad",
			Labels:  map[string]string{},
		},
	}

	client, shutdown := newErrorDockerClient(t, 0, http.StatusInternalServerError, listed)
	defer shutdown()

	manager := NewContainerManager(client, "test-agent", nil)
	added, updated, removed, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatalf("expected no error when only inspect fails during Refresh, got: %v", err)
	}
	if len(added) != 0 || len(updated) != 0 || len(removed) != 0 {
		t.Fatalf("expected empty diff when inspect fails, got added=%d updated=%d removed=%d",
			len(added), len(updated), len(removed))
	}
}

// TestRefreshSkipsFailedInspectAndContinuesToNextContainer covers
// containers.go:125 — the same continue-vs-break distinction as
// TestBuildInventorySkipsFailedInspectAndContinuesToNextContainer, but for
// the Refresh loop's cache-miss branch.
func TestRefreshSkipsFailedInspectAndContinuesToNextContainer(t *testing.T) {
	t.Parallel()

	listed := []docker.ContainerJSON{
		{ID: "bad-c", Image: "nginx:latest", ImageID: "sha256:bad", Labels: map[string]string{}},
		{ID: "good-c", Image: "redis:7", ImageID: "sha256:good", Labels: map[string]string{}},
	}

	client, shutdown := newSelectiveInspectFailureDockerClient(t, listed, "bad-c")
	defer shutdown()

	manager := NewContainerManager(client, "test-agent", nil)
	added, updated, removed, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatalf("expected no error when one inspect fails during Refresh, got: %v", err)
	}
	if len(updated) != 0 || len(removed) != 0 {
		t.Fatalf("expected updated=0 removed=0, got updated=%d removed=%d", len(updated), len(removed))
	}
	if len(added) != 1 {
		t.Fatalf("expected 1 added container after skipping the failed one, got %d", len(added))
	}
	if added[0].ID != "good-c" {
		t.Fatalf("expected surviving container to be good-c, got %q", added[0].ID)
	}
}

// TestRefreshStaleEviction covers containers.go:134-138 — after a container
// disappears from the list its inspect-cache entry is evicted.
func TestRefreshStaleEviction(t *testing.T) {
	t.Parallel()

	client, fixture, shutdown := newDynamicDockerClient(t)
	defer shutdown()

	// Start with c1 present.
	fixture.Set(buildInventorySnapshot([]fixtureContainer{
		{id: "c1", image: "nginx:1.0", state: "running"},
	}))

	manager := NewContainerManager(client, "test-agent", nil)
	if _, err := manager.BuildInventory(context.Background()); err != nil {
		t.Fatalf("build inventory: %v", err)
	}

	// First refresh: c1 is still present; cache entry is populated.
	if _, _, _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	manager.cacheMu.Lock()
	if _, found := manager.inspectCache["c1"]; !found {
		manager.cacheMu.Unlock()
		t.Fatal("expected c1 in inspect cache after first refresh")
	}
	manager.cacheMu.Unlock()

	// Second refresh: c1 disappears from the list — cache entry must be evicted.
	fixture.Set(buildInventorySnapshot([]fixtureContainer{}))

	_, _, removed, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	assertContainerIDs(t, removed, []string{"c1"})

	manager.cacheMu.Lock()
	if _, found := manager.inspectCache["c1"]; found {
		manager.cacheMu.Unlock()
		t.Fatal("expected c1 to be evicted from inspect cache after second refresh")
	}
	manager.cacheMu.Unlock()
}

// ---------------------------------------------------------------------------
// toContainer label fallback paths
// ---------------------------------------------------------------------------

// TestToContainerLabels covers containers.go:178-183 — the three label
// resolution branches: (1) inspect.Config.Labels non-nil (baseline), (2)
// inspect.Config.Labels nil → falls back to listEntry.Labels, (3) both nil →
// uses empty map.
func TestToContainerLabels(t *testing.T) {
	t.Parallel()

	manager := NewContainerManager(nil, "test-agent", nil)

	baseInspect := docker.ContainerInspect{
		ID:      "cid",
		Name:    "/test",
		Created: "2026-01-01T00:00:00Z",
		State: docker.ContainerState{
			Status:  "running",
			Running: true,
		},
		Config: docker.ContainerConfig{
			Image: "nginx:latest",
		},
	}

	tests := []struct {
		name           string
		inspectLabels  map[string]string
		listLabels     map[string]string
		wantLabelKey   string
		wantLabelValue string
		wantEmpty      bool
	}{
		{
			name:           "inspect labels used when present",
			inspectLabels:  map[string]string{"src": "inspect"},
			listLabels:     map[string]string{"src": "list"},
			wantLabelKey:   "src",
			wantLabelValue: "inspect",
		},
		{
			name:           "list labels used when inspect labels nil",
			inspectLabels:  nil,
			listLabels:     map[string]string{"src": "list"},
			wantLabelKey:   "src",
			wantLabelValue: "list",
		},
		{
			name:          "empty map when both nil",
			inspectLabels: nil,
			listLabels:    nil,
			wantEmpty:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inspect := baseInspect
			inspect.Config.Labels = tc.inspectLabels

			listEntry := &docker.ContainerJSON{
				ID:      "cid",
				Image:   "nginx:latest",
				ImageID: "sha256:abc",
				Labels:  tc.listLabels,
			}

			c := manager.toContainer(&inspect, listEntry)

			if tc.wantEmpty {
				if len(c.Labels) != 0 {
					t.Fatalf("expected empty labels map, got %v", c.Labels)
				}
				return
			}
			got, ok := c.Labels[tc.wantLabelKey]
			if !ok {
				t.Fatalf("expected label %q to be present", tc.wantLabelKey)
			}
			if got != tc.wantLabelValue {
				t.Fatalf("expected label %q=%q, got %q", tc.wantLabelKey, tc.wantLabelValue, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// BuildRuntimeDetails branch coverage
// ---------------------------------------------------------------------------

// TestBuildRuntimeDetailsOptionalFields covers containers.go:282-310 — the
// optional fields (Cmd, Health, NetworkSettings, Mounts) are each exercised.
func TestBuildRuntimeDetailsOptionalFields(t *testing.T) {
	t.Parallel()

	baseInspect := docker.ContainerInspect{
		ID:      "cid",
		Name:    "/test",
		Created: "2026-01-01T00:00:00Z",
		State: docker.ContainerState{
			Status:    "running",
			Running:   true,
			StartedAt: "2026-01-01T00:00:00Z",
		},
		Config: docker.ContainerConfig{
			Image: "nginx:latest",
		},
	}

	t.Run("cmd nil stays empty", func(t *testing.T) {
		t.Parallel()
		inspect := baseInspect
		inspect.Config.Cmd = nil
		d := BuildRuntimeDetails(&inspect)
		if d.Command != "" {
			t.Fatalf("expected empty Command when Cmd is nil, got %q", d.Command)
		}
	})

	t.Run("cmd set joins with space", func(t *testing.T) {
		t.Parallel()
		inspect := baseInspect
		inspect.Config.Cmd = []string{"nginx", "-g", "daemon off;"}
		d := BuildRuntimeDetails(&inspect)
		if d.Command != "nginx -g daemon off;" {
			t.Fatalf("expected joined command, got %q", d.Command)
		}
	})

	t.Run("health nil stays empty", func(t *testing.T) {
		t.Parallel()
		inspect := baseInspect
		inspect.State.Health = nil
		d := BuildRuntimeDetails(&inspect)
		if d.Health != "" {
			t.Fatalf("expected empty Health when Health state is nil, got %q", d.Health)
		}
	})

	t.Run("health set propagated", func(t *testing.T) {
		t.Parallel()
		inspect := baseInspect
		inspect.State.Health = &docker.HealthState{Status: "healthy"}
		d := BuildRuntimeDetails(&inspect)
		if d.Health != "healthy" {
			t.Fatalf("expected Health=healthy, got %q", d.Health)
		}
	})

	t.Run("network settings nil produces no network entries", func(t *testing.T) {
		t.Parallel()
		inspect := baseInspect
		inspect.NetworkSettings = nil
		d := BuildRuntimeDetails(&inspect)
		if len(d.Network) != 0 {
			t.Fatalf("expected no network entries, got %d", len(d.Network))
		}
	})

	t.Run("network settings with entries populated", func(t *testing.T) {
		t.Parallel()
		inspect := baseInspect
		inspect.NetworkSettings = &docker.NetworkSettings{
			Networks: map[string]docker.NetworkEndpoint{
				"bridge": {IPAddress: "172.17.0.2", Gateway: "172.17.0.1"},
			},
		}
		d := BuildRuntimeDetails(&inspect)
		if len(d.Network) != 1 {
			t.Fatalf("expected 1 network entry, got %d", len(d.Network))
		}
		if d.Network[0].Name != "bridge" {
			t.Fatalf("expected network name 'bridge', got %q", d.Network[0].Name)
		}
		if d.Network[0].IPAddress != "172.17.0.2" {
			t.Fatalf("expected IPAddress '172.17.0.2', got %q", d.Network[0].IPAddress)
		}
		if d.Network[0].Gateway != "172.17.0.1" {
			t.Fatalf("expected Gateway '172.17.0.1', got %q", d.Network[0].Gateway)
		}
	})

	t.Run("mounts empty produces no volumes", func(t *testing.T) {
		t.Parallel()
		inspect := baseInspect
		inspect.Mounts = nil
		d := BuildRuntimeDetails(&inspect)
		if len(d.Volumes) != 0 {
			t.Fatalf("expected no volumes, got %d", len(d.Volumes))
		}
	})

	t.Run("mounts populated", func(t *testing.T) {
		t.Parallel()
		inspect := baseInspect
		inspect.Mounts = []docker.MountPoint{
			{
				Type:        "bind",
				Source:      "/host/data",
				Destination: "/data",
				RW:          true,
			},
			{
				Type:        "volume",
				Source:      "myvolume",
				Destination: "/app",
				RW:          false,
			},
		}
		d := BuildRuntimeDetails(&inspect)
		if len(d.Volumes) != 2 {
			t.Fatalf("expected 2 volume entries, got %d", len(d.Volumes))
		}
		if d.Volumes[0].ReadOnly != false {
			t.Fatalf("expected first volume ReadOnly=false (RW=true), got %v", d.Volumes[0].ReadOnly)
		}
		if d.Volumes[1].ReadOnly != true {
			t.Fatalf("expected second volume ReadOnly=true (RW=false), got %v", d.Volumes[1].ReadOnly)
		}
		if d.Volumes[0].Source != "/host/data" {
			t.Fatalf("expected Source='/host/data', got %q", d.Volumes[0].Source)
		}
		if d.Volumes[1].Destination != "/app" {
			t.Fatalf("expected Destination='/app', got %q", d.Volumes[1].Destination)
		}
	})

	t.Run("env nil produces no entries", func(t *testing.T) {
		t.Parallel()
		inspect := baseInspect
		inspect.Config.Env = nil
		d := BuildRuntimeDetails(&inspect)
		if len(d.Env) != 0 {
			t.Fatalf("expected no env entries, got %d", len(d.Env))
		}
	})

	t.Run("env populated splits on first equals only", func(t *testing.T) {
		t.Parallel()
		inspect := baseInspect
		inspect.Config.Env = []string{"PATH=/usr/bin", "KEY=a=b", "EMPTY="}
		d := BuildRuntimeDetails(&inspect)
		if len(d.Env) != 3 {
			t.Fatalf("expected 3 env entries, got %d", len(d.Env))
		}
		if d.Env[0] != (EnvVar{Key: "PATH", Value: "/usr/bin"}) {
			t.Fatalf("expected PATH=/usr/bin, got %+v", d.Env[0])
		}
		if d.Env[1] != (EnvVar{Key: "KEY", Value: "a=b"}) {
			t.Fatalf("expected KEY=a=b (only first '=' splits), got %+v", d.Env[1])
		}
		if d.Env[2] != (EnvVar{Key: "EMPTY", Value: ""}) {
			t.Fatalf("expected EMPTY= to produce empty value, got %+v", d.Env[2])
		}
	})
}
