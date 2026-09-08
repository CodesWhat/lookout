package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/auth"
)

// TestStripPortwingAuthHeaders verifies that every Portwing credential header is
// removed before a request is proxied to the Docker socket, while unrelated
// headers are preserved.
func TestStripPortwingAuthHeaders(t *testing.T) {
	h := http.Header{}
	stripped := []string{
		"Authorization",
		"X-Portwing-Token",
		"X-Dd-Agent-Secret",
		auth.HeaderKeyID,
		auth.HeaderTimestamp,
		auth.HeaderNonce,
		auth.HeaderSignature,
		auth.HeaderSignatureVersion,
	}
	for _, name := range stripped {
		h.Set(name, "secret")
	}
	h.Set("Content-Type", "application/json")
	h.Set("X-Registry-Auth", "keep-me") // a legitimate Docker header

	stripPortwingAuthHeaders(h)

	for _, name := range stripped {
		if got := h.Get(name); got != "" {
			t.Errorf("auth header %q leaked to Docker: %q", name, got)
		}
	}
	if h.Get("Content-Type") != "application/json" {
		t.Error("Content-Type was wrongly stripped")
	}
	if h.Get("X-Registry-Auth") != "keep-me" {
		t.Error("X-Registry-Auth (a Docker header) was wrongly stripped")
	}
}

// TestListenerBindsIPv6BindAddress is a regression test for the address the
// standard-mode listener is built from: joining BIND_ADDRESS and PORT with a
// bare colon turned the documented "::1" into "::1:3000", which net.Listen
// rejects with "too many colons in address", so an IPv6 bind never came up.
func TestListenerBindsIPv6BindAddress(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bind   string
		remote bool
	}{
		{name: "ipv4 loopback", bind: "127.0.0.1"},
		{name: "ipv6 loopback", bind: "::1"},
		{name: "ipv6 wildcard", bind: "::", remote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, stop := newStubDockerClient(t)
			defer stop()

			cfg := minimalConfig()
			cfg.BindAddress = tc.bind
			cfg.Port = "0"
			cfg.AllowUnauthenticatedRemote = tc.remote

			s, err := NewServer(cfg, client, &stubServerAdapter{})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
			})

			errCh := make(chan error, 1)
			go func() { errCh <- s.ListenAndServe() }()

			deadline := time.Now().Add(3 * time.Second)
			var addr net.Addr
			for time.Now().Before(deadline) {
				if addr = s.Addr(); addr != nil {
					break
				}
				select {
				case err := <-errCh:
					if (tc.bind == "::1" || tc.bind == "::") && errors.Is(err, syscall.EAFNOSUPPORT) {
						t.Skipf("IPv6 address family unavailable: %v", err)
					}
					t.Fatalf("ListenAndServe(%q) failed instead of binding: %v", cfg.BindAddress, err)
				default:
				}
				time.Sleep(2 * time.Millisecond)
			}
			if addr == nil {
				t.Fatalf("ListenAndServe(%q) never bound a listener", cfg.BindAddress)
			}

			tcpAddr, ok := addr.(*net.TCPAddr)
			if !ok {
				t.Fatalf("bound address %v is not a *net.TCPAddr", addr)
			}
			want := net.ParseIP(strings.Trim(tc.bind, "[]"))
			if !tcpAddr.IP.Equal(want) {
				t.Fatalf("bound IP = %v, want %v (from BIND_ADDRESS %q)", tcpAddr.IP, want, tc.bind)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := s.Shutdown(ctx); err != nil {
				t.Errorf("Shutdown: %v", err)
			}
			select {
			case err := <-errCh:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					t.Errorf("ListenAndServe returned unexpectedly: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ListenAndServe did not return after Shutdown")
			}
		})
	}
}
