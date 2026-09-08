package adapter

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/docker"
)

type fixtureContainer struct {
	id    string
	image string
	state string
}

type dockerInventorySnapshot struct {
	listed    []docker.ContainerJSON
	inspected map[string]docker.ContainerInspect
}

type dynamicDockerFixture struct {
	mu        sync.RWMutex
	inventory dockerInventorySnapshot
}

func (f *dynamicDockerFixture) Set(snapshot dockerInventorySnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inventory = snapshot
}

func (f *dynamicDockerFixture) Snapshot() dockerInventorySnapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()

	listed := make([]docker.ContainerJSON, len(f.inventory.listed))
	copy(listed, f.inventory.listed)

	inspected := make(map[string]docker.ContainerInspect, len(f.inventory.inspected))
	for id, inspect := range f.inventory.inspected {
		inspected[id] = inspect
	}

	return dockerInventorySnapshot{
		listed:    listed,
		inspected: inspected,
	}
}

func TestContainerManagerRefreshDiffsAddedUpdatedRemoved(t *testing.T) {
	t.Parallel()

	client, fixture, shutdown := newDynamicDockerClient(t)
	defer shutdown()

	fixture.Set(buildInventorySnapshot([]fixtureContainer{
		{id: "c1", image: "nginx:1.0", state: "running"},
		{id: "c2", image: "redis:7", state: "running"},
	}))

	manager := NewContainerManager(client, "test-agent", nil)
	if _, err := manager.BuildInventory(context.Background()); err != nil {
		t.Fatalf("build inventory: %v", err)
	}

	fixture.Set(buildInventorySnapshot([]fixtureContainer{
		{id: "c1", image: "nginx:1.0", state: "exited"},
		{id: "c3", image: "postgres:16", state: "running"},
	}))

	added, updated, removed, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	assertContainerIDs(t, added, []string{"c3"})
	assertContainerIDs(t, updated, []string{"c1"})
	assertContainerIDs(t, removed, []string{"c2"})

	if updated[0].Status != "stopped" {
		t.Fatalf("expected updated container status to be stopped, got %q", updated[0].Status)
	}

	current := manager.GetContainers()
	assertContainerIDs(t, current, []string{"c1", "c3"})

	c1, ok := manager.GetContainer("c1")
	if !ok {
		t.Fatalf("expected c1 in container map")
	}
	if c1.Status != "stopped" {
		t.Fatalf("expected c1 status to be stopped in current map, got %q", c1.Status)
	}
}

// containerSignalSnapshot builds a one-container inventory snapshot with the
// *listed* entry's State/Status/ImageID set explicitly — three of the four
// fields Refresh hashes into the inspect-cache signal (containerChangeSignal;
// the fourth is Names), distinct from the nested inspect State used by
// buildInventorySnapshot's fixtureContainer helper. Varying them across two
// fixture.Set calls is what lets a test force a signal change between two
// Refresh calls.
func containerSignalSnapshot(id, listState, listStatus, imageID, inspectStatus string, running bool) dockerInventorySnapshot {
	return dockerInventorySnapshot{
		listed: []docker.ContainerJSON{
			{
				ID:      id,
				Image:   "nginx:1.0",
				ImageID: imageID,
				State:   listState,
				Status:  listStatus,
				Labels:  map[string]string{"test.id": id},
			},
		},
		inspected: map[string]docker.ContainerInspect{
			id: {
				ID:      id,
				Name:    "/" + id,
				Created: "2026-01-01T00:00:00Z",
				State: docker.ContainerState{
					Status:    inspectStatus,
					Running:   running,
					StartedAt: "2026-01-01T00:00:00Z",
				},
				Config: docker.ContainerConfig{
					Image:  "nginx:1.0",
					Labels: map[string]string{"test.id": id},
				},
				NetworkSettings: &docker.NetworkSettings{
					Networks: map[string]docker.NetworkEndpoint{},
				},
			},
		},
	}
}

// TestRefreshBypassesStaleCacheWhenContainerSignalChanges covers the
// inspect-cache hit in Refresh — it is only used when the cached entry's
// signal (containerChangeSignal) still matches the freshly listed entry. A
// cache hit whose signal has drifted must fall through to a fresh
// InspectContainer call, not serve the stale cached container.
func TestRefreshBypassesStaleCacheWhenContainerSignalChanges(t *testing.T) {
	t.Parallel()

	client, fixture, shutdown := newDynamicDockerClient(t)
	defer shutdown()

	fixture.Set(containerSignalSnapshot("c1", "running", "Up 5 minutes", "sha256:v1", "running", true))

	manager := NewContainerManager(client, "test-agent", nil)
	if _, err := manager.BuildInventory(context.Background()); err != nil {
		t.Fatalf("build inventory: %v", err)
	}

	// First refresh populates the inspect cache with the "running" signal.
	if _, _, _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	c1, ok := manager.GetContainer("c1")
	if !ok || c1.Status != "running" {
		t.Fatalf("expected c1 running after first refresh, got %+v ok=%v", c1, ok)
	}

	// The listed entry's State/Status/ImageID change, which changes its
	// cache signal. A second refresh must detect the mismatch and
	// re-inspect rather than reuse the stale cached entry.
	fixture.Set(containerSignalSnapshot("c1", "exited", "Exited (0) 1 second ago", "sha256:v2", "exited", false))

	_, updated, _, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if len(updated) != 1 {
		t.Fatalf("expected c1 to be reported updated after its signal changed, got %d updated", len(updated))
	}
	if updated[0].Status != "stopped" {
		t.Fatalf("expected updated status to be stopped, got %q", updated[0].Status)
	}

	c1, ok = manager.GetContainer("c1")
	if !ok || c1.Status != "stopped" {
		t.Fatalf("expected c1 status stopped after signal change, got %+v ok=%v", c1, ok)
	}
}

// renamedContainerSnapshot builds a one-container inventory for a stopped
// container whose only difference across two calls is its name. State,
// Status and ImageID are held fixed, which is exactly what `docker rename`
// leaves alone on a stopped container.
func renamedContainerSnapshot(id, name string) dockerInventorySnapshot {
	return dockerInventorySnapshot{
		listed: []docker.ContainerJSON{
			{
				ID:      id,
				Names:   []string{"/" + name},
				Image:   "nginx:1.0",
				ImageID: "sha256:v1",
				State:   "exited",
				Status:  "Exited (0) 2 months ago",
				Labels:  map[string]string{"test.id": id},
			},
		},
		inspected: map[string]docker.ContainerInspect{
			id: {
				ID:      id,
				Name:    "/" + name,
				Created: "2026-01-01T00:00:00Z",
				State: docker.ContainerState{
					Status:     "exited",
					StartedAt:  "2026-01-01T00:00:00Z",
					FinishedAt: "2026-01-01T01:00:00Z",
				},
				Config: docker.ContainerConfig{
					Image:  "nginx:1.0",
					Labels: map[string]string{"test.id": id},
				},
				NetworkSettings: &docker.NetworkSettings{
					Networks: map[string]docker.NetworkEndpoint{},
				},
			},
		},
	}
}

// TestRefreshServesTheNewNameAfterARename pins the Names half of
// containerChangeSignal. `docker rename` on a stopped container touches
// nothing else the list endpoint reports: State and ImageID are unchanged and
// Status is a humanised age that can go a month without ticking over, so a
// signal built from those three alone stays a cache hit and the agent keeps
// serving the old name.
func TestRefreshServesTheNewNameAfterARename(t *testing.T) {
	t.Parallel()

	client, fixture, shutdown := newDynamicDockerClient(t)
	defer shutdown()

	fixture.Set(renamedContainerSnapshot("c1", "old-name"))

	manager := NewContainerManager(client, "test-agent", nil)
	if _, err := manager.BuildInventory(context.Background()); err != nil {
		t.Fatalf("build inventory: %v", err)
	}
	// The first refresh caches the inspect result under the old name.
	if _, _, _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	c1, ok := manager.GetContainer("c1")
	if !ok || c1.Name != "old-name" {
		t.Fatalf("c1 = %+v ok=%v, want name old-name before the rename", c1, ok)
	}

	before := renamedContainerSnapshot("c1", "old-name").listed[0]
	after := renamedContainerSnapshot("c1", "new-name").listed[0]
	if before.State != after.State || before.Status != after.Status || before.ImageID != after.ImageID {
		t.Fatal("fixture changed more than the name; the rename is no longer the only signal difference")
	}
	fixture.Set(renamedContainerSnapshot("c1", "new-name"))

	added, updated, removed, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if len(added) != 0 || len(removed) != 0 || len(updated) != 1 || updated[0].Name != "new-name" {
		t.Fatalf("rename diff: added=%v updated=%v removed=%v", added, updated, removed)
	}
	if _, updated, _, err := manager.Refresh(context.Background()); err != nil || len(updated) != 0 {
		t.Fatalf("unchanged refresh after rename: updated=%v err=%v", updated, err)
	}

	c1, ok = manager.GetContainer("c1")
	if !ok {
		t.Fatal("c1 missing from the inventory after the rename")
	}
	if c1.Name != "new-name" {
		t.Fatalf("name = %q, want new-name", c1.Name)
	}
	if c1.DisplayName != "new-name" {
		t.Fatalf("displayName = %q, want new-name", c1.DisplayName)
	}
}

func TestContainerChangeSignalIgnoresNameOrder(t *testing.T) {
	t.Parallel()
	entry := renamedContainerSnapshot("c1", "web").listed[0]
	entry.Names = []string{"/web", "/alias"}
	before := append([]string(nil), entry.Names...)
	want := containerChangeSignal(&entry)
	if !reflect.DeepEqual(entry.Names, before) {
		t.Fatal("signal mutated the caller's names")
	}
	entry.Names = []string{"/alias", "/web"}
	if got := containerChangeSignal(&entry); got != want {
		t.Fatalf("reordered names changed signal: %q != %q", got, want)
	}
}

func TestContainerManagerRefreshNoChanges(t *testing.T) {
	t.Parallel()

	client, fixture, shutdown := newDynamicDockerClient(t)
	defer shutdown()

	snapshot := buildInventorySnapshot([]fixtureContainer{
		{id: "c1", image: "nginx:1.0", state: "running"},
		{id: "c2", image: "redis:7", state: "exited"},
	})
	fixture.Set(snapshot)

	manager := NewContainerManager(client, "test-agent", nil)
	if _, err := manager.BuildInventory(context.Background()); err != nil {
		t.Fatalf("build inventory: %v", err)
	}

	fixture.Set(snapshot)

	added, updated, removed, err := manager.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if len(added) != 0 || len(updated) != 0 || len(removed) != 0 {
		t.Fatalf("expected empty diff, got added=%d updated=%d removed=%d", len(added), len(updated), len(removed))
	}
}

func TestContainerManagerRefreshDiffsHealthOnlyTransitions(t *testing.T) {
	t.Parallel()

	healthy := "healthy"
	tests := []struct {
		name       string
		before     *string
		after      *string
		wantHealth string
	}{
		{name: "health appears", after: &healthy, wantHealth: "healthy"},
		{name: "health disappears", before: &healthy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, fixture, shutdown := newDynamicDockerClient(t)
			defer shutdown()

			fixture.Set(inventorySnapshotWithHealth("c1", tt.before))
			manager := NewContainerManager(client, "test-agent", nil)
			if _, err := manager.BuildInventory(context.Background()); err != nil {
				t.Fatalf("build inventory: %v", err)
			}
			if tt.before == nil {
				manager.containersMu.Lock()
				container := manager.containers["c1"]
				container.Details = nil
				manager.containers["c1"] = container
				manager.containersMu.Unlock()
			}

			fixture.Set(inventorySnapshotWithHealth("c1", tt.after))
			added, updated, removed, err := manager.Refresh(context.Background())
			if err != nil {
				t.Fatalf("refresh: %v", err)
			}
			if len(added) != 0 || len(removed) != 0 {
				t.Fatalf("health-only transition changed membership: added=%d removed=%d", len(added), len(removed))
			}
			if len(updated) != 1 {
				t.Fatalf("health-only transition produced %d updated containers, want 1", len(updated))
			}
			if updated[0].Details == nil {
				t.Fatal("updated container details are nil")
			}
			if got := updated[0].Details.Health; got != tt.wantHealth {
				t.Fatalf("updated health = %q, want %q", got, tt.wantHealth)
			}
		})
	}
}

func inventorySnapshotWithHealth(id string, health *string) dockerInventorySnapshot {
	snapshot := buildInventorySnapshot([]fixtureContainer{{id: id, image: "nginx:1.0", state: "running"}})
	snapshot.listed[0].Status = "Up 5 minutes"
	inspect := snapshot.inspected[id]
	if health != nil {
		inspect.State.Health = &docker.HealthState{Status: *health}
		snapshot.listed[0].Status += " (" + *health + ")"
	}
	snapshot.inspected[id] = inspect
	return snapshot
}

func assertContainerIDs(t *testing.T, containers []Container, want []string) {
	t.Helper()

	got := make([]string, 0, len(containers))
	for _, c := range containers {
		got = append(got, c.ID)
	}

	sort.Strings(got)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)

	if !reflect.DeepEqual(got, wantSorted) {
		t.Fatalf("container IDs mismatch: got %v want %v", got, wantSorted)
	}
}

func newDynamicDockerClient(t *testing.T) (*docker.Client, *dynamicDockerFixture, func()) {
	t.Helper()

	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}

	fixture := &dynamicDockerFixture{
		inventory: dockerInventorySnapshot{
			listed:    []docker.ContainerJSON{},
			inspected: map[string]docker.ContainerInspect{},
		},
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
		snapshot := fixture.Snapshot()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot.listed)
	})

	mux.HandleFunc("/v1.44/containers/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/json") {
			http.NotFound(w, r)
			return
		}

		id := strings.TrimPrefix(r.URL.Path, "/v1.44/containers/")
		id = strings.TrimSuffix(id, "/json")

		snapshot := fixture.Snapshot()
		inspect, ok := snapshot.inspected[id]
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(inspect)
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

	return client, fixture, shutdown
}

func buildInventorySnapshot(containers []fixtureContainer) dockerInventorySnapshot {
	listed := make([]docker.ContainerJSON, 0, len(containers))
	inspected := make(map[string]docker.ContainerInspect, len(containers))

	for _, c := range containers {
		image := c.image
		if image == "" {
			image = "nginx:latest"
		}

		listed = append(listed, docker.ContainerJSON{
			ID:      c.id,
			Image:   image,
			ImageID: "sha256:" + c.id,
			Labels: map[string]string{
				"test.id": c.id,
			},
		})

		state := docker.ContainerState{
			Status:    c.state,
			StartedAt: "2026-01-01T00:00:00Z",
		}
		switch c.state {
		case "running":
			state.Running = true
		case "paused":
			state.Running = true
			state.Paused = true
		case "restarting":
			state.Running = true
			state.Restarting = true
		case "dead":
			state.Dead = true
		}

		inspected[c.id] = docker.ContainerInspect{
			ID:      c.id,
			Name:    "/" + c.id,
			Created: "2026-01-01T00:00:00Z",
			State:   state,
			Config: docker.ContainerConfig{
				Image: image,
				Labels: map[string]string{
					"test.id": c.id,
				},
			},
			NetworkSettings: &docker.NetworkSettings{
				Networks: map[string]docker.NetworkEndpoint{},
			},
		}
	}

	return dockerInventorySnapshot{
		listed:    listed,
		inspected: inspected,
	}
}

func TestContainerManagerRefreshSpecificRunningStates(t *testing.T) {
	t.Parallel()
	for _, specific := range []string{"paused", "restarting"} {
		t.Run(specific, func(t *testing.T) {
			t.Parallel()
			client, fixture, shutdown := newDynamicDockerClient(t)
			defer shutdown()
			setState := func(state string) {
				snapshot := buildInventorySnapshot([]fixtureContainer{{id: "c1", image: "nginx:latest", state: state}})
				snapshot.listed[0].State = state
				snapshot.listed[0].Status = state
				fixture.Set(snapshot)
			}
			setState("running")
			manager := NewContainerManager(client, "test-agent", nil)
			if _, _, _, err := manager.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, state := range []string{specific, "running"} {
				setState(state)
				added, updated, removed, err := manager.Refresh(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if len(added) != 0 || len(removed) != 0 {
					t.Fatalf("unexpected membership change: added=%v removed=%v", added, removed)
				}
				assertContainerIDs(t, updated, []string{"c1"})
				if got := updated[0].Status; got != state {
					t.Errorf("updated status = %q, want %q", got, state)
				}
				current, ok := manager.GetContainer("c1")
				if !ok || current.Status != state {
					t.Fatalf("current container = %+v, found = %v, want status %q", current, ok, state)
				}
			}
		})
	}
}
