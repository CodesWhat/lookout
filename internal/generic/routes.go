package generic

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/codeswhat/portwing/internal/adapter"
)

func (a *Adapter) handleContainers(w http.ResponseWriter, _ *http.Request) {
	containers := a.containers.GetContainers()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(containers); err != nil {
		slog.Error("failed to encode containers response", "error", err)
	}
}

// handleContainerLogs serves the generic container log route. The generic REST
// surface has no `timestamps` parameter, so lines are always served as the
// daemon wrote them; the rest of the lifecycle is
// adapter.ServeContainerLogs, shared with the Drydock adapter.
func (a *Adapter) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	adapter.ServeContainerLogs(w, r, adapter.ContainerLogOptions{
		Client: a.dockerClient,
		Admit:  a.admit,
	})
}
