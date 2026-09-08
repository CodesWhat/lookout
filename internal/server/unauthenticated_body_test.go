package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUnauthenticatedRoutesRejectUnreadBodies(t *testing.T) {
	s, handler := newRouteTestServer(t, routeTestServerOpts{token: "health-secret", enroll: true})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	for _, path := range []string{"/health", "/ready", "/_portwing/health"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			for _, framing := range []string{"Content-Length: 1\r\n", "Transfer-Encoding: chunked\r\n"} {
				t.Run(method+path+strings.TrimSpace(framing), func(t *testing.T) {
					assertUnreadBodyRejected(t, ts.URL, method, path, framing, "", http.StatusBadRequest)
				})
			}
		}
	}
	t.Run("enrollment wrong method", func(t *testing.T) {
		assertUnreadBodyRejected(t, ts.URL, http.MethodGet, "/api/portwing/enroll", "Content-Length: 1\r\n", "", http.StatusMethodNotAllowed)
	})
	t.Run("enrollment malformed body", func(t *testing.T) {
		assertUnreadBodyRejected(t, ts.URL, http.MethodPost, "/api/portwing/enroll", "Content-Length: 2\r\n", "!", http.StatusBadRequest)
	})
	t.Run("enrollment admission", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			s.rateLimiter.RecordFailure("127.0.0.1")
		}
		assertUnreadBodyRejected(t, ts.URL, http.MethodPost, "/api/portwing/enroll", "Content-Length: 1\r\n", "", http.StatusTooManyRequests)
	})
}

func assertUnreadBodyRejected(t *testing.T, baseURL, method, path, framing, prefix string, status int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", strings.TrimPrefix(baseURL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(c, "%s %s HTTP/1.1\r\nHost: localhost\r\n%s\r\n%s", method, path, framing, prefix); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(c)
	resp, err := http.ReadResponse(reader, &http.Request{Method: method})
	if err != nil {
		t.Fatalf("rejection blocked on unread body: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != status || !resp.Close {
		t.Fatalf("status/close = %d/%v, want %d/true", resp.StatusCode, resp.Close, status)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("connection not released: %v", err)
	}
}
