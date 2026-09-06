package server

// hijack_upgrade_header_test.go targets the CONDITIONALS_NEGATION mutant at
// http.go:660:13 (`if upgrade == ""` -> `!=`), which decides whether
// handleDockerHijack defaults the outbound "Upgrade" header to "tcp" or
// preserves a client-supplied value.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/audit"
	"github.com/codeswhat/portwing/internal/docker"
)

// captureUpgradeHeaderFromExecRequest starts a fake Docker daemon on a Unix
// socket that answers the version-negotiation probe issued by
// docker.NewClient and then captures the Upgrade header of the next request
// (the exec-start hijack request), returning it on the channel.
func captureUpgradeHeaderFromExecRequest(t *testing.T) (sockPath string, upgradeCh chan string, cleanup func()) {
	t.Helper()

	sockPath, cleanupSocket := shortSocketPath(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		cleanupSocket()
		t.Fatalf("listen: %v", err)
	}

	upgradeCh = make(chan string, 1)
	go func() {
		versionConn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		versionReq, readErr := http.ReadRequest(bufio.NewReader(versionConn))
		if readErr != nil {
			_ = versionConn.Close()
			return
		}
		_ = versionReq.Body.Close()
		versionBody := `{"Version":"26.0.0","ApiVersion":"1.44"}`
		_, _ = fmt.Fprintf(
			versionConn,
			"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			len(versionBody),
			versionBody,
		)
		_ = versionConn.Close()

		execConn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer execConn.Close()
		execReq, readErr := http.ReadRequest(bufio.NewReader(execConn))
		if readErr != nil {
			return
		}
		_ = execReq.Body.Close()
		upgradeCh <- execReq.Header.Get("Upgrade")
		// Answer with a Bad Gateway-ish response; the test only needs the
		// captured header, not a working upgrade.
		_, _ = fmt.Fprint(execConn, "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\n\r\n")
	}()

	return sockPath, upgradeCh, func() {
		_ = listener.Close()
		cleanupSocket()
	}
}

// TestHandleDockerHijackUpgradeHeader exercises both sides of the
// CONDITIONALS_NEGATION mutant at http.go:660:13: a request without an
// Upgrade header must be forwarded to Docker with "Upgrade: tcp", and a
// request that supplies one must have that value preserved unchanged.
func TestHandleDockerHijackUpgradeHeader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		clientUpgrade string // "" means the client sends no Upgrade header
		wantOnTheWire string
	}{
		{"absent defaults to tcp", "", "tcp"},
		{"present is preserved", "vt100", "vt100"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sockPath, upgradeCh, cleanup := captureUpgradeHeaderFromExecRequest(t)
			defer cleanup()

			client, err := docker.NewClient(sockPath, 5)
			if err != nil {
				t.Fatalf("docker.NewClient: %v", err)
			}
			auditor, closeAudit, err := audit.New("", 0)
			if err != nil {
				t.Fatalf("audit.New: %v", err)
			}
			defer closeAudit()

			s := &Server{
				dockerClient: client,
				rateLimiter:  NewRateLimiter(),
				auditor:      auditor,
			}
			defer s.rateLimiter.Stop()

			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()

			hrw := &hijackableResponseWriter{
				conn: serverConn,
				buf:  bufio.NewReadWriter(bufio.NewReader(serverConn), bufio.NewWriter(serverConn)),
				hdr:  make(http.Header),
			}
			req := httptest.NewRequest(http.MethodPost, "/exec/abc123/start", nil)
			if tc.clientUpgrade != "" {
				req.Header.Set("Upgrade", tc.clientUpgrade)
			}

			handlerDone := make(chan struct{})
			go func() {
				defer close(handlerDone)
				s.handleExecHijack(hrw, req)
			}()

			// Drain whatever the fake daemon writes back so handleExecHijack
			// can finish relaying and return. resp.Write may issue several
			// Write calls, so this must keep reading (not stop after one
			// Read) until the connection closes.
			go func() {
				_, _ = io.Copy(io.Discard, clientConn)
			}()

			select {
			case got := <-upgradeCh:
				if got != tc.wantOnTheWire {
					t.Fatalf("Upgrade header sent to Docker = %q, want %q", got, tc.wantOnTheWire)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for exec request to reach the fake Docker daemon")
			}

			select {
			case <-handlerDone:
			case <-time.After(2 * time.Second):
				t.Fatal("handleExecHijack did not return in time")
			}
		})
	}
}
