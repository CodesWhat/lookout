package adapter

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/codeswhat/portwing/internal/docker"
)

// StreamLimitRejectionMessage is the body returned when a long-lived adapter
// stream is rejected for want of a free concurrency slot. Matches the
// Docker-proxy stream rejection in internal/server/http.go so a client (or a
// test) can't tell the two rejection paths apart.
const StreamLimitRejectionMessage = "agent busy: too many concurrent streams"

// ContainerLogOptions carries the parts of a container-log request that differ
// between adapters. Everything else about the lifecycle is identical, so it
// lives in ServeContainerLogs.
type ContainerLogOptions struct {
	// Client reads the logs from the daemon.
	Client *docker.Client

	// Admit gates follow-mode streams against the server's shared stream
	// concurrency limit (SPEC 7.3). The nil zero value always admits.
	Admit StreamAdmitter

	// Timestamps asks the daemon to prefix each line with its timestamp.
	// This is the only behavioural difference between the two adapters: the
	// Drydock routes derive it from the request's `timestamps` query
	// parameter, and the generic REST surface has no such parameter, so it
	// always serves lines as the daemon wrote them.
	Timestamps bool
}

// ServeContainerLogs serves an adapter's GET .../containers/{id}/logs. It
// validates `tail`, gates follow-mode requests on the shared stream limit,
// then streams the daemon's response through docker.DecodeContainerLogStream,
// flushing after every payload so a follower sees lines as they arrive.
//
// The container ID comes from the request's "id" path value, so the route
// pattern must name it.
func ServeContainerLogs(w http.ResponseWriter, r *http.Request, opts ContainerLogOptions) {
	containerID := r.PathValue("id")
	tail := r.URL.Query().Get("tail")
	since := r.URL.Query().Get("since")
	until := r.URL.Query().Get("until")
	follow := r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true"

	if tail != "" {
		n, err := strconv.Atoi(tail)
		if err != nil || n <= 0 {
			http.Error(w, "invalid tail: must be a positive integer", http.StatusBadRequest)
			return
		}
		tail = strconv.Itoa(n)
	}

	// Bound concurrent follow-mode log streams against the shared stream
	// limit (SPEC 7.3), before the daemon call so a rejected follow request
	// costs nothing. Non-follow requests are a single bounded read and are
	// never gated.
	if follow {
		release, ok := opts.Admit.Admit()
		if !ok {
			slog.Warn("concurrent stream limit reached, rejecting log follow", "containerId", containerID)
			http.Error(w, StreamLimitRejectionMessage, http.StatusServiceUnavailable)
			return
		}
		defer release()
	}

	body, err := opts.Client.GetContainerLogs(r.Context(), containerID, tail, since, until, follow, opts.Timestamps)
	if err != nil {
		http.Error(w, fmt.Sprintf("getting logs: %v", err), docker.StatusCodeForError(err))
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if follow {
		w.Header().Set("Transfer-Encoding", "chunked")
	}

	flusher, canFlush := w.(http.Flusher)
	err = docker.DecodeContainerLogStream(body, func(_ docker.ContainerLogStream, payload []byte) error {
		if _, writeErr := w.Write(payload); writeErr != nil {
			return writeErr
		}
		if canFlush {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		slog.Debug("log stream ended", "error", err)
	}
}
