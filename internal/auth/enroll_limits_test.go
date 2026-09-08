package auth

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

type enrollmentDeadlineWriter struct {
	*httptest.ResponseRecorder

	mu       sync.Mutex
	calls    []enrollmentDeadlineCall
	armed    chan struct{}
	armOnce  sync.Once
	clearErr error
}

type enrollmentDeadlineCall struct {
	at       time.Time
	deadline time.Time
}

func newEnrollmentDeadlineWriter() *enrollmentDeadlineWriter {
	return &enrollmentDeadlineWriter{
		ResponseRecorder: httptest.NewRecorder(),
		armed:            make(chan struct{}),
	}
}

func (w *enrollmentDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	calledAt := time.Now()
	w.mu.Lock()
	w.calls = append(w.calls, enrollmentDeadlineCall{at: calledAt, deadline: deadline})
	w.mu.Unlock()
	if !deadline.IsZero() {
		w.armOnce.Do(func() { close(w.armed) })
	} else if w.clearErr != nil {
		return w.clearErr
	}
	return nil
}

func (w *enrollmentDeadlineWriter) recordedDeadlineCalls() []enrollmentDeadlineCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]enrollmentDeadlineCall(nil), w.calls...)
}

type enrollmentDeadlineBody struct {
	prefix  []byte
	armed   <-chan struct{}
	abort   <-chan struct{}
	prefixN int
}

func (b *enrollmentDeadlineBody) Read(p []byte) (int, error) {
	if b.prefixN < len(b.prefix) {
		n := copy(p, b.prefix[b.prefixN:])
		b.prefixN += n
		return n, nil
	}
	select {
	case <-b.armed:
		return 0, os.ErrDeadlineExceeded
	case <-b.abort:
		return 0, io.ErrUnexpectedEOF
	}
}

func (*enrollmentDeadlineBody) Close() error { return nil }

func TestEnrollerBodyReadDeadline(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
	}{
		{name: "first JSON value", prefix: ""},
		{
			name:   "trailing JSON value",
			prefix: `{"enrollment_token":"wrong","public_key":""}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _, _ := setupEnroller(t, "expected")
			writer := newEnrollmentDeadlineWriter()
			abort := make(chan struct{})
			body := &enrollmentDeadlineBody{
				prefix: []byte(tt.prefix),
				armed:  writer.armed,
				abort:  abort,
			}
			req := httptest.NewRequest(http.MethodPost, "/api/portwing/enroll", body)

			const testDeadline = 250 * time.Millisecond
			originalDeadline := enrollmentBodyReadDeadline
			enrollmentBodyReadDeadline = testDeadline
			t.Cleanup(func() { enrollmentBodyReadDeadline = originalDeadline })

			done := make(chan struct{})
			go func() {
				defer close(done)
				e.ServeHTTP(writer, req)
			}()

			select {
			case <-done:
			case <-time.After(time.Second):
				close(abort)
				<-done
				t.Fatal("enrollment handler did not establish a body read deadline")
			}

			if writer.Code != http.StatusRequestTimeout {
				t.Fatalf("expected 408 after enrollment body read deadline, got %d: %s", writer.Code, writer.Body.String())
			}

			calls := writer.recordedDeadlineCalls()
			if len(calls) < 2 {
				t.Fatalf("expected enrollment deadline to be set and expired, got %v", calls)
			}
			if got := calls[0].deadline.Sub(calls[0].at); got < testDeadline-50*time.Millisecond || got > testDeadline+50*time.Millisecond {
				t.Fatalf("body read deadline offset = %v, want approximately %v", got, testDeadline)
			}
			last := calls[len(calls)-1]
			if last.deadline.IsZero() || last.deadline.After(last.at) {
				t.Fatalf("failed decode must expire the body read deadline, got %v", last.deadline)
			}
			if writer.Header().Get("Connection") != "close" {
				t.Fatal("failed decode must close the connection")
			}
		})
	}
}

func TestEnrollerBodyReadDeadlineClearFailureDoesNotChangeResponse(t *testing.T) {
	e, _, _ := setupEnroller(t, "expected")
	writer := newEnrollmentDeadlineWriter()
	writer.clearErr = errors.New("simulated clear failure")
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/portwing/enroll",
		enrollBody(t, "wrong", ""),
	)

	e.ServeHTTP(writer, req)

	if writer.Code != http.StatusUnauthorized {
		t.Fatalf("enrollment status = %d, want 401", writer.Code)
	}
	calls := writer.recordedDeadlineCalls()
	if len(calls) != 2 || !calls[1].deadline.IsZero() {
		t.Fatalf("deadline calls = %v, want one arm followed by one clear attempt", calls)
	}
}

func TestEnrollerBodyReadDeadlineOverTCP(t *testing.T) {
	e, _, _ := setupEnroller(t, "expected")
	server := httptest.NewServer(e)
	t.Cleanup(server.Close)

	const testDeadline = 100 * time.Millisecond
	originalDeadline := enrollmentBodyReadDeadline
	enrollmentBodyReadDeadline = testDeadline
	t.Cleanup(func() { enrollmentBodyReadDeadline = originalDeadline })

	tests := []struct {
		name   string
		prefix string
	}{
		{name: "first JSON value", prefix: "{"},
		{
			name:   "trailing JSON value",
			prefix: `{"enrollment_token":"wrong","public_key":""}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatalf("dial enrollment server: %v", err)
			}
			defer conn.Close()

			request := "POST /api/portwing/enroll HTTP/1.1\r\n" +
				"Host: " + server.Listener.Addr().String() + "\r\n" +
				"Content-Type: application/json\r\n" +
				"Content-Length: " + fmt.Sprint(len(tt.prefix)+1) + "\r\n" +
				"Connection: close\r\n\r\n" + tt.prefix
			if _, err := io.WriteString(conn, request); err != nil {
				t.Fatalf("write partial enrollment request: %v", err)
			}
			if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatalf("set client read deadline: %v", err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("read enrollment response: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusRequestTimeout {
				t.Fatalf("enrollment status = %d, want 408", resp.StatusCode)
			}
		})
	}
}
