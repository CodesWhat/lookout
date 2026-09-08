package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/docker"
)

func newStreamRouteServer(t *testing.T, upstream http.HandlerFunc, timeout, streams int) (*Server, *httptest.Server) {
	t.Helper()
	path, cleanup := shortSocketPath(t)
	listener := newUnixListener(t, path)
	daemon := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			_, _ = io.WriteString(w, `{"Version":"26.0.0","ApiVersion":"1.44"}`)
			return
		}
		upstream(w, r)
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = daemon.Serve(listener) }()
	t.Cleanup(func() { _ = daemon.Close(); <-done; cleanup() })
	client, err := docker.NewClient(path, timeout)
	if err != nil {
		t.Fatal(err)
	}
	cfg := minimalConfig()
	cfg.MaxStreamSessions = streams
	cfg.Token = "proxy-secret"
	cfg.AllowUnauthenticated = false
	s, err := NewServer(cfg, client, &stubServerAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.httpServer.Handler)
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s, ts
}

func streamRouteRequest(t *testing.T, ctx context.Context, ts *httptest.Server, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(headerPortwingToken, "proxy-secret")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestProxyStreamTruncationAbortsResponse(t *testing.T) {
	s, ts := newStreamRouteServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n8\r\npart")
		_ = rw.Flush()
	}, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp := streamRouteRequest(t, ctx, ts, http.MethodGet, "/containers/example/export")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("truncated upstream completed normally: %d %q", resp.StatusCode, body)
	}
	if string(body) != "part" {
		t.Fatalf("unexpected payload %q", body)
	}
	if !s.streamSem.acquire() {
		t.Fatal("aborted stream retained admission slot")
	}
	s.streamSem.release()
}

func TestRecoveryPreservesHTTPAbort(t *testing.T) {
	var caught any
	func() {
		defer func() { caught = recover() }()
		h := RecoveryMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	if caught != http.ErrAbortHandler { //nolint:errorlint // The recovery boundary must preserve the exact net/http sentinel.
		t.Fatalf("caught %v, want http.ErrAbortHandler", caught)
	}
}

type streamFailureWriter struct{ http.ResponseWriter }

func (streamFailureWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type streamReadSequence struct {
	reads int
	err   error
}

func (r *streamReadSequence) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		return copy(p, "part"), r.err
	}
	return 0, io.EOF
}

func TestStreamStopsAfterWriteFailure(t *testing.T) {
	body := &streamReadSequence{}
	s := &Server{}
	if err := s.streamResponse(streamFailureWriter{httptest.NewRecorder()}, body); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write failure = %v", err)
	}
	if body.reads != 1 {
		t.Fatalf("read %d times after failing to deliver first bytes", body.reads)
	}
}

func TestStreamReadErrorTerminatesHTTPResponse(t *testing.T) {
	for _, readErr := range []error{io.EOF, io.ErrUnexpectedEOF} {
		t.Run(readErr.Error(), func(t *testing.T) {
			s := &Server{}
			ts := httptest.NewServer(RecoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := s.streamResponse(w, &streamReadSequence{err: readErr}); err != nil {
					panic(http.ErrAbortHandler)
				}
			})))
			defer ts.Close()
			ts.Client().Timeout = time.Second
			resp, err := ts.Client().Get(ts.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if string(body) != "part" || (errors.Is(readErr, io.EOF) && err != nil) || (!errors.Is(readErr, io.EOF) && err == nil) {
				t.Fatalf("read error %v produced body %q, downstream error %v", readErr, body, err)
			}
		})
	}
}

func TestProxyStatsAndPushStayStreaming(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1.44/containers/example/stats"},
		{http.MethodPost, "/v1.44/images/library/nginx/push"},
	} {
		t.Run(route.path, func(t *testing.T) {
			canceled := make(chan struct{})
			s, ts := newStreamRouteServer(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(canceled)
				_, _ = io.WriteString(w, "first\n")
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(1200 * time.Millisecond):
				}
				_, _ = io.WriteString(w, "later\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}, 1, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			start := time.Now()
			resp := streamRouteRequest(t, ctx, ts, route.method, route.path)
			defer resp.Body.Close()
			first := make([]byte, 6)
			if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "first\n" {
				t.Fatalf("first record %q: %v", first, err)
			}
			if time.Since(start) >= time.Second {
				t.Fatal("initial record was buffered")
			}
			busy := streamRouteRequest(t, ctx, ts, route.method, route.path)
			_ = busy.Body.Close()
			if busy.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("second stream status %d", busy.StatusCode)
			}
			later := make([]byte, 6)
			if _, err := io.ReadFull(resp.Body, later); err != nil || string(later) != "later\n" {
				t.Fatalf("record past unary timeout %q: %v", later, err)
			}
			cancel()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("upstream did not stop on cancellation")
			}
			deadline := time.Now().Add(time.Second)
			for !s.streamSem.acquire() {
				if time.Now().After(deadline) {
					t.Fatal("stream slot leaked after cancellation")
				}
				time.Sleep(time.Millisecond)
			}
			s.streamSem.release()
		})
	}
}

func TestProxyStatsExplicitFalseRetainsUnaryTimeout(t *testing.T) {
	canceled := make(chan struct{})
	_, ts := newStreamRouteServer(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(canceled)
		<-r.Context().Done()
	}, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp := streamRouteRequest(t, ctx, ts, http.MethodGet, "/containers/example/stats?stream=false")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want upstream timeout", resp.StatusCode)
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("unary upstream was not canceled")
	}
}

type streamShortWriter struct{ http.ResponseWriter }

func (streamShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestStreamStopsAfterShortWrite(t *testing.T) {
	body := &streamReadSequence{}
	if err := (&Server{}).streamResponse(streamShortWriter{httptest.NewRecorder()}, body); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %v", err)
	}
	if body.reads != 1 {
		t.Fatalf("read %d times after short write", body.reads)
	}
}
