package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/codeswhat/portwing/internal/docker"
)

// cachedContainer pairs a built Container with the signal that produced it.
type cachedContainer struct {
	container Container
	signal    string
}

// ContainerManager maintains an inventory of Docker containers and computes
// diffs between snapshots. A LabelParser is injected to allow adapter-specific
// label extraction.
type ContainerManager struct {
	dockerClient *docker.Client
	agentName    string
	labelParser  LabelParser
	containersMu sync.RWMutex
	containers   map[string]Container // keyed by container ID
	cacheMu      sync.Mutex
	inspectCache map[string]cachedContainer // keyed by container ID
}

// NewContainerManager creates a ContainerManager. If labelParser is nil, a
// default no-op parser is used.
func NewContainerManager(dockerClient *docker.Client, agentName string, labelParser LabelParser) *ContainerManager {
	if labelParser == nil {
		labelParser = func(labels map[string]string) LabelResult {
			return LabelResult{}
		}
	}
	return &ContainerManager{
		dockerClient: dockerClient,
		agentName:    agentName,
		labelParser:  labelParser,
		containers:   make(map[string]Container),
		inspectCache: make(map[string]cachedContainer),
	}
}

// BuildInventory lists all containers, inspects each one, builds Container
// structs, and stores them for later diffing.
func (m *ContainerManager) BuildInventory(ctx context.Context) ([]Container, error) {
	listed, err := m.dockerClient.ListContainers(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	containers := make([]Container, 0, len(listed))
	newMap := make(map[string]Container, len(listed))

	for _, entry := range listed {
		inspect, err := m.dockerClient.InspectContainer(ctx, entry.ID)
		if err != nil {
			slog.Warn("failed to inspect container", "id", entry.ID, "error", err)
			continue
		}

		c := m.toContainer(inspect, &entry)
		containers = append(containers, c)
		newMap[c.ID] = c
	}

	m.containersMu.Lock()
	m.containers = newMap
	m.containersMu.Unlock()
	return containers, nil
}

// GetContainers returns the current inventory as a slice.
func (m *ContainerManager) GetContainers() []Container {
	m.containersMu.RLock()
	defer m.containersMu.RUnlock()

	result := make([]Container, 0, len(m.containers))
	for _, c := range m.containers {
		result = append(result, c)
	}
	return result
}

// GetContainer returns a single container by ID.
func (m *ContainerManager) GetContainer(id string) (*Container, bool) {
	m.containersMu.RLock()
	defer m.containersMu.RUnlock()

	c, ok := m.containers[id]
	if !ok {
		return nil, false
	}
	return &c, true
}

// Refresh rebuilds the inventory and computes the diff against the previous
// snapshot. Returns added, updated, and removed containers.
func (m *ContainerManager) Refresh(ctx context.Context) (added, updated, removed []Container, err error) {
	oldMap := m.snapshotContainers()

	listed, err := m.dockerClient.ListContainers(ctx, true)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing containers: %w", err)
	}

	newMap := make(map[string]Container, len(listed))

	m.cacheMu.Lock()
	for _, entry := range listed {
		signal := containerChangeSignal(&entry)

		if cached, hit := m.inspectCache[entry.ID]; hit && cached.signal == signal {
			c := cached.container
			newMap[c.ID] = c
			continue
		}

		inspect, err := m.dockerClient.InspectContainer(ctx, entry.ID)
		if err != nil {
			// The daemon still listed this container, so a failed inspect
			// is a failure to re-read it, not evidence that it is gone.
			// Dropping it here would report it removed and pull it out of
			// the served inventory until some later poll happened to
			// succeed, so serve the last known build instead. The cache
			// entry is deliberately left holding its old signal: the next
			// poll then misses again and retries the inspect.
			last, known := m.lastKnownContainer(entry.ID, oldMap)
			if !known {
				slog.Warn("failed to inspect container during refresh", "id", entry.ID, "error", err)
				continue
			}
			slog.Warn(
				"failed to inspect container during refresh, keeping last known entry",
				"id", entry.ID,
				"error", err,
			)
			newMap[last.ID] = last
			continue
		}

		c := m.toContainer(inspect, &entry)
		m.inspectCache[entry.ID] = cachedContainer{container: c, signal: signal}
		newMap[c.ID] = c
	}

	// Evict stale cache entries.
	for id := range m.inspectCache {
		if _, ok := newMap[id]; !ok {
			delete(m.inspectCache, id)
		}
	}
	m.cacheMu.Unlock()

	for id, c := range newMap {
		if old, exists := oldMap[id]; exists {
			if old.Name != c.Name || old.Status != c.Status || containerHealth(old) != containerHealth(c) {
				updated = append(updated, c)
			}
		} else {
			added = append(added, c)
		}
	}

	for id, old := range oldMap {
		if _, exists := newMap[id]; !exists {
			removed = append(removed, old)
		}
	}

	m.containersMu.Lock()
	m.containers = newMap
	m.containersMu.Unlock()
	return added, updated, removed, nil
}

// containerChangeSignal fingerprints a listed container so Refresh can tell
// whether its cached inspect result is still current. It covers every mutable
// field the list endpoint reports that the built Container depends on.
//
// Names is in there because `docker rename` changes nothing else: State and
// ImageID are untouched and Status is a humanised age ("Exited (0) 2 months
// ago"), which for a stopped container can go a month without ticking over.
// A rename would otherwise keep serving the old name until something
// unrelated happened to invalidate the entry. Container names cannot contain
// "|" or ",", so joining them can't collide with a different name set.
func containerChangeSignal(entry *docker.ContainerJSON) string {
	names := slices.Clone(entry.Names)
	slices.Sort(names)
	return entry.State + "|" + entry.Status + "|" + entry.ImageID + "|" +
		strings.Join(names, ",")
}

// lastKnownContainer returns the most recent successful build of a container:
// its inspect-cache entry if one survives, otherwise its entry in the previous
// inventory snapshot. The cache is empty until the first Refresh populates it,
// so the snapshot covers containers that BuildInventory produced. Callers must
// hold cacheMu.
func (m *ContainerManager) lastKnownContainer(id string, previous map[string]Container) (Container, bool) {
	if cached, ok := m.inspectCache[id]; ok {
		return cached.container, true
	}
	c, ok := previous[id]
	return c, ok
}

func containerHealth(container Container) string {
	if container.Details == nil {
		return ""
	}
	return container.Details.Health
}

// snapshotContainers returns a copy of the current container map.
func (m *ContainerManager) snapshotContainers() map[string]Container {
	m.containersMu.RLock()
	defer m.containersMu.RUnlock()

	snapshot := make(map[string]Container, len(m.containers))
	for id, c := range m.containers {
		snapshot[id] = c
	}
	return snapshot
}

// toContainer converts Docker inspect and list results into a Container.
func (m *ContainerManager) toContainer(inspect *docker.ContainerInspect, listEntry *docker.ContainerJSON) Container {
	labels := inspect.Config.Labels
	if labels == nil {
		labels = listEntry.Labels
	}
	if labels == nil {
		labels = make(map[string]string)
	}

	lr := m.labelParser(labels)

	// Use the container name from inspect, stripping leading slash.
	name := strings.TrimPrefix(inspect.Name, "/")
	if lr.DisplayName == "" {
		lr.DisplayName = name
	}
	if lr.Watcher == "" {
		lr.Watcher = "docker"
	}

	registry, imageName, tag := ParseImageRef(inspect.Config.Image)

	c := Container{
		ID:          inspect.ID,
		Name:        name,
		DisplayName: lr.DisplayName,
		DisplayIcon: lr.DisplayIcon,
		Status:      ContainerStatus(&inspect.State),
		Watcher:     lr.Watcher,
		Agent:       m.agentName,

		Image: ContainerImage{
			ID:       listEntry.ImageID,
			Registry: registry,
			Name:     imageName,
			Tag:      tag,
			Created:  inspect.Created,
		},

		UpdateAvailable: false,
		UpdateKind:      UpdateKindUnknown,

		IncludeTags:   lr.IncludeTags,
		ExcludeTags:   lr.ExcludeTags,
		TransformTags: lr.TransformTags,

		Labels:  labels,
		Details: BuildRuntimeDetails(inspect),
	}

	return c
}

// ParseImageRef parses a Docker image reference into registry, name, and tag.
// Examples:
//
//	"registry.example.com/org/image:tag" -> ("registry.example.com", "org/image", "tag")
//	"nginx:latest"                       -> ("docker.io", "nginx", "latest")
//	"nginx"                              -> ("docker.io", "nginx", "latest")
//	"ghcr.io/owner/repo:v1.2"           -> ("ghcr.io", "owner/repo", "v1.2")
func ParseImageRef(imageRef string) (registry, name, tag string) {
	tag = "latest"

	// Split off tag or digest.
	ref := imageRef
	if idx := strings.LastIndex(ref, ":"); idx != -1 {
		// Make sure this colon is not part of a registry port by checking
		// if the part after the colon contains a slash (which would mean
		// it's a port:path, not name:tag).
		// Also skip empty candidates (e.g. input ":") to preserve the
		// default "latest" tag.
		candidate := ref[idx+1:]
		if candidate != "" && !strings.Contains(candidate, "/") {
			tag = candidate
			ref = ref[:idx]
		}
	}

	// Determine if the first component is a registry (contains a dot or
	// colon, or is "localhost").
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 1 {
		// Simple name like "nginx".
		return "docker.io", ref, tag
	}

	firstPart := parts[0]
	if strings.Contains(firstPart, ".") || strings.Contains(firstPart, ":") || firstPart == "localhost" {
		registry = firstPart
		name = parts[1]
	} else {
		registry = "docker.io"
		name = ref
	}

	return registry, name, tag
}

// BuildRuntimeDetails constructs RuntimeDetails from a Docker inspect response.
func BuildRuntimeDetails(inspect *docker.ContainerInspect) *RuntimeDetails {
	details := &RuntimeDetails{
		Platform: inspect.Platform,
		Created:  inspect.Created,
		Started:  inspect.State.StartedAt,
	}

	if inspect.Config.Cmd != nil {
		details.Command = strings.Join(inspect.Config.Cmd, " ")
	}

	// Environment variables. Each entry is "KEY=VALUE"; split on the first
	// "=" so values containing "=" are preserved intact.
	for _, entry := range inspect.Config.Env {
		key, value, _ := strings.Cut(entry, "=")
		details.Env = append(details.Env, EnvVar{Key: key, Value: value})
	}

	// Health status.
	if inspect.State.Health != nil {
		details.Health = inspect.State.Health.Status
	}

	// Networks.
	if inspect.NetworkSettings != nil {
		for netName, ep := range inspect.NetworkSettings.Networks {
			details.Network = append(details.Network, NetworkInfo{
				Name:      netName,
				IPAddress: ep.IPAddress,
				Gateway:   ep.Gateway,
			})
		}
	}

	// Volumes / mounts.
	for _, mount := range inspect.Mounts {
		details.Volumes = append(details.Volumes, VolumeInfo{
			Type:        mount.Type,
			Source:      mount.Source,
			Destination: mount.Destination,
			ReadOnly:    !mount.RW,
		})
	}

	return details
}

// ContainerStatus maps Docker container state to a simple status string.
func ContainerStatus(state *docker.ContainerState) string {
	switch {
	case state.Running:
		return "running"
	case state.Paused:
		return "paused"
	case state.Restarting:
		return "restarting"
	case state.Dead:
		return "dead"
	case state.Status == "created":
		return "created"
	default:
		return "stopped"
	}
}
