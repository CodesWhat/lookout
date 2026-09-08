package adapter

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/docker"
)

type logDaemonCalls struct {
	count    atomic.Int64
	rawQuery atomic.Value
}

func (c *logDaemonCalls) query() string {
	q, _ := c.rawQuery.Load().(string)
	return q
}

// newLogDaemonClient builds a docker client pointed at a fake daemon whose
// container-logs endpoint answers with status and body. Any other path 404s.
func newLogDaemonClient(t *testing.T, status int, body []byte) (*docker.Client, *logDaemonCalls, func()) {
	t.Helper()

	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}

	calls := &logDaemonCalls{}

	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(docker.VersionResponse{Version: "26.0.0", APIVersion: "1.44"})
	})
	mux.HandleFunc("/v1.44/containers/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/logs") {
			http.NotFound(w, r)
			return
		}
		calls.count.Add(1)
		calls.rawQuery.Store(r.URL.RawQuery)
		if status != http.StatusOK {
			http.Error(w, "daemon says no", status)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
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

	return client, calls, shutdown
}

func dockerLogFrame(streamType byte, payload []byte) []byte {
	frame := make([]byte, 8, 8+len(payload))
	frame[0] = streamType
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	return append(frame, payload...)
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *flushRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}

type failingLogWriter struct {
	header   http.Header
	attempts int
	flushes  int
}

func (w *failingLogWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingLogWriter) Write([]byte) (int, error) {
	w.attempts++
	return 0, io.ErrClosedPipe
}

func (w *failingLogWriter) WriteHeader(int) {}

func (w *failingLogWriter) Flush() { w.flushes++ }

func logRequest(target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetPathValue("id", "container-1")
	return req
}

func TestServeContainerLogsRejectsInvalidTail(t *testing.T) {
	t.Parallel()

	for _, tail := range []string{"abc", "0", "-5", "1.5"} {
		t.Run(tail, func(t *testing.T) {
			t.Parallel()

			client, calls, shutdown := newLogDaemonClient(t, http.StatusOK, nil)
			defer shutdown()

			rec := httptest.NewRecorder()
			ServeContainerLogs(rec, logRequest("/logs?tail="+tail), ContainerLogOptions{Client: client})

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if got := calls.count.Load(); got != 0 {
				t.Fatalf("daemon calls = %d, want 0 for a rejected tail", got)
			}
		})
	}
}

func TestServeContainerLogsDecodesAndFlushesPayloads(t *testing.T) {
	t.Parallel()

	body := append(dockerLogFrame(1, []byte("out\n")), dockerLogFrame(2, []byte("err\n"))...)
	client, calls, shutdown := newLogDaemonClient(t, http.StatusOK, body)
	defer shutdown()

	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	ServeContainerLogs(rec, logRequest("/logs?tail=5"), ContainerLogOptions{Client: client})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "out\nerr\n" {
		t.Fatalf("body = %q, want %q", got, "out\nerr\n")
	}
	if rec.flushes < 2 {
		t.Fatalf("flushes = %d, want one per payload", rec.flushes)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("content-type = %q", got)
	}
	if got := rec.Header().Get("Transfer-Encoding"); got != "" {
		t.Fatalf("transfer-encoding = %q, want unset for a non-follow read", got)
	}
	if got := calls.query(); !strings.Contains(got, "tail=5") {
		t.Fatalf("daemon query = %q, want tail=5", got)
	}
	if strings.Contains(calls.query(), "timestamps") {
		t.Fatalf("daemon query = %q, want no timestamps when the option is off", calls.query())
	}
}

func TestServeContainerLogsPassesTimestampsSinceAndUntil(t *testing.T) {
	t.Parallel()

	client, calls, shutdown := newLogDaemonClient(t, http.StatusOK, []byte("raw\n"))
	defer shutdown()

	rec := httptest.NewRecorder()
	ServeContainerLogs(
		rec,
		logRequest("/logs?since=2026-01-01T00:00:00Z&until=2026-01-02T00:00:00Z"),
		ContainerLogOptions{Client: client, Timestamps: true},
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	for _, want := range []string{"timestamps=1", "since=2026-01-01", "until=2026-01-02"} {
		if !strings.Contains(calls.query(), want) {
			t.Fatalf("daemon query = %q, want it to contain %q", calls.query(), want)
		}
	}
}

// TestServeContainerLogsGatesFollowOnTheStreamLimit pins the admission half of
// the shared handler: a follow request that can't get a slot is refused with
// the same body the Docker-proxy stream limit uses and never reaches the
// daemon, and an admitted one releases its slot when the handler returns.
func TestServeContainerLogsGatesFollowOnTheStreamLimit(t *testing.T) {
	t.Parallel()

	t.Run("rejected", func(t *testing.T) {
		t.Parallel()

		client, calls, shutdown := newLogDaemonClient(t, http.StatusOK, nil)
		defer shutdown()

		rec := httptest.NewRecorder()
		ServeContainerLogs(rec, logRequest("/logs?follow=1"), ContainerLogOptions{
			Client: client,
			Admit:  func() (func(), bool) { return nil, false },
		})

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
		if got := rec.Body.String(); got != StreamLimitRejectionMessage+"\n" {
			t.Fatalf("body = %q, want %q", got, StreamLimitRejectionMessage+"\n")
		}
		if got := calls.count.Load(); got != 0 {
			t.Fatalf("daemon calls = %d, want 0 for a rejected follow", got)
		}
	})

	t.Run("admitted", func(t *testing.T) {
		t.Parallel()

		client, _, shutdown := newLogDaemonClient(t, http.StatusOK, []byte("tty\n"))
		defer shutdown()

		var released atomic.Int64
		rec := httptest.NewRecorder()
		ServeContainerLogs(rec, logRequest("/logs?follow=true"), ContainerLogOptions{
			Client: client,
			Admit:  func() (func(), bool) { return func() { released.Add(1) }, true },
		})

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Body.String(); got != "tty\n" {
			t.Fatalf("body = %q, want %q", got, "tty\n")
		}
		if got := rec.Header().Get("Transfer-Encoding"); got != "chunked" {
			t.Fatalf("transfer-encoding = %q, want chunked", got)
		}
		if got := released.Load(); got != 1 {
			t.Fatalf("slot releases = %d, want 1", got)
		}
	})
}

func TestServeContainerLogsMapsDaemonErrors(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ daemon, want int }{
		{daemon: http.StatusNotFound, want: http.StatusNotFound},
		{daemon: http.StatusConflict, want: http.StatusConflict},
		{daemon: http.StatusInternalServerError, want: http.StatusInternalServerError},
	} {
		t.Run(http.StatusText(tt.daemon), func(t *testing.T) {
			t.Parallel()

			client, _, shutdown := newLogDaemonClient(t, tt.daemon, nil)
			defer shutdown()

			rec := httptest.NewRecorder()
			ServeContainerLogs(rec, logRequest("/logs"), ContainerLogOptions{Client: client})

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if !strings.Contains(rec.Body.String(), "getting logs") {
				t.Fatalf("body = %q, want it to name the failed operation", rec.Body.String())
			}
		})
	}
}

func TestServeContainerLogsStopsAfterWriteFailure(t *testing.T) {
	t.Parallel()

	body := append(dockerLogFrame(1, []byte("first\n")), dockerLogFrame(2, []byte("second\n"))...)
	client, _, shutdown := newLogDaemonClient(t, http.StatusOK, body)
	defer shutdown()

	w := &failingLogWriter{}
	ServeContainerLogs(w, logRequest("/logs?follow=1"), ContainerLogOptions{Client: client})

	if w.attempts != 1 {
		t.Fatalf("write attempts = %d, want the decoder to stop after the first failure", w.attempts)
	}
	if w.flushes != 0 {
		t.Fatalf("flushes = %d, want 0 after a failed write", w.flushes)
	}
}
