package drydock

import (
	"fmt"
	"runtime"

	"github.com/codeswhat/portwing/internal/adapter"
)

type drydockContainer struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	DisplayIcon string `json:"displayIcon,omitempty"`
	Status      string `json:"status"`
	Health      string `json:"health,omitempty"`
	Watcher     string `json:"watcher"`
	Agent       string `json:"agent,omitempty"`

	Image  drydockContainerImage    `json:"image"`
	Result *adapter.ContainerResult `json:"result,omitempty"`
	Error  *drydockContainerError   `json:"error,omitempty"`

	UpdateAvailable bool                       `json:"updateAvailable"`
	UpdateKind      drydockContainerUpdateKind `json:"updateKind"`

	IncludeTags   string `json:"includeTags,omitempty"`
	ExcludeTags   string `json:"excludeTags,omitempty"`
	TransformTags string `json:"transformTags,omitempty"`

	Labels  map[string]string      `json:"labels,omitempty"`
	Details *drydockRuntimeDetails `json:"details,omitempty"`
}

type drydockContainerImage struct {
	ID       string                   `json:"id"`
	Registry drydockContainerRegistry `json:"registry"`
	Name     string                   `json:"name"`
	Tag      drydockContainerTag      `json:"tag"`
	Digest   drydockContainerDigest   `json:"digest"`

	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Created      string `json:"created,omitempty"`
}

type drydockContainerRegistry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type drydockContainerTag struct {
	Value  string `json:"value"`
	Semver bool   `json:"semver"`
}

type drydockContainerDigest struct {
	Watch bool   `json:"watch"`
	Value string `json:"value,omitempty"`
	Repo  string `json:"repo,omitempty"`
}

type drydockContainerUpdateKind struct {
	Kind string `json:"kind"`
}

type drydockContainerError struct {
	Message string `json:"message"`
}

type drydockRuntimeDetails struct {
	Ports     []string         `json:"ports"`
	Volumes   []string         `json:"volumes"`
	Env       []adapter.EnvVar `json:"env"`
	StartedAt string           `json:"startedAt,omitempty"`
}

func toDrydockContainers(containers []adapter.Container) []drydockContainer {
	wireContainers := make([]drydockContainer, len(containers))
	for index := range containers {
		wireContainers[index] = toDrydockContainer(containers[index])
	}
	return wireContainers
}

func toDrydockContainer(container adapter.Container) drydockContainer {
	architecture := container.Image.Architecture
	if architecture == "" {
		architecture = runtime.GOARCH
	}
	osName := container.Image.OS
	if osName == "" {
		osName = runtime.GOOS
	}
	registryURL := container.Image.Registry
	if registryURL == "" {
		registryURL = "docker.io"
	}

	wireContainer := drydockContainer{
		ID:          container.ID,
		Name:        container.Name,
		DisplayName: container.DisplayName,
		DisplayIcon: container.DisplayIcon,
		Status:      container.Status,
		Watcher:     container.Watcher,
		Agent:       container.Agent,
		Image: drydockContainerImage{
			ID: container.Image.ID,
			Registry: drydockContainerRegistry{
				Name: "unknown",
				URL:  registryURL,
			},
			Name: container.Image.Name,
			Tag: drydockContainerTag{
				Value:  container.Image.Tag,
				Semver: false,
			},
			Digest: drydockContainerDigest{
				Watch: false,
				Value: container.Image.Digest,
			},
			Architecture: architecture,
			OS:           osName,
			Created:      container.Image.Created,
		},
		Result:          container.Result,
		UpdateAvailable: container.UpdateAvailable,
		UpdateKind:      drydockContainerUpdateKind{Kind: "unknown"},
		IncludeTags:     container.IncludeTags,
		ExcludeTags:     container.ExcludeTags,
		TransformTags:   container.TransformTags,
		Labels:          container.Labels,
	}
	if container.Error != nil {
		wireContainer.Error = &drydockContainerError{Message: container.Error.Message}
	}
	if container.Details != nil {
		wireContainer.Details = toDrydockRuntimeDetails(container.Details)
		switch container.Details.Health {
		case "starting", "healthy", "unhealthy":
			wireContainer.Health = container.Details.Health
		}
	}
	return wireContainer
}

func toDrydockRuntimeDetails(details *adapter.RuntimeDetails) *drydockRuntimeDetails {
	wireDetails := &drydockRuntimeDetails{
		Ports:     make([]string, 0, len(details.Ports)),
		Volumes:   make([]string, 0, len(details.Volumes)),
		Env:       details.Env,
		StartedAt: details.Started,
	}
	if wireDetails.Env == nil {
		wireDetails.Env = []adapter.EnvVar{}
	}
	for _, port := range details.Ports {
		containerPort := fmt.Sprintf("%d/%s", port.Container, port.Protocol)
		if port.Host == 0 {
			wireDetails.Ports = append(wireDetails.Ports, containerPort)
			continue
		}
		host := fmt.Sprintf("%d", port.Host)
		if port.IP != "" {
			host = port.IP + ":" + host
		}
		wireDetails.Ports = append(wireDetails.Ports, host+"->"+containerPort)
	}
	for _, volume := range details.Volumes {
		binding := volume.Source + ":" + volume.Destination
		if volume.ReadOnly {
			binding += ":ro"
		}
		wireDetails.Volumes = append(wireDetails.Volumes, binding)
	}
	return wireDetails
}
