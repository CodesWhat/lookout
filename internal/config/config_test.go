package config

import (
	"bytes"
	"errors"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// setEnv sets environment variables for the duration of a test.
func setEnv(t *testing.T, kv ...string) {
	t.Helper()
	if len(kv)%2 != 0 {
		t.Fatal("setEnv requires an even number of key/value arguments")
	}
	for i := 0; i < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

// TestLoadBothTokenAndHashErrors ensures that setting TOKEN and TOKEN_HASH
// simultaneously returns an error.
func TestLoadBothTokenAndHashErrors(t *testing.T) {
	setEnv(t,
		"TOKEN", "rawtoken",
		"TOKEN_HASH", "$argon2id$v=19$m=19456,t=2,p=1$c29tZXNhbHQ$aGFzaGhhc2g",
	)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when both TOKEN and TOKEN_HASH are set, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected 'mutually exclusive' in error, got: %v", err)
	}
}

func TestLoadRejectsPartialTLSKeypair(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "certificate only", key: "TLS_CERT"},
		{name: "private key only", key: "TLS_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, "/run/secrets/tls")
			_, err := Load()
			if err == nil {
				t.Fatal("expected partial TLS keypair to fail configuration load")
			}
			if !strings.Contains(err.Error(), "TLS_CERT and TLS_KEY") {
				t.Fatalf("expected paired TLS guidance, got: %v", err)
			}
		})
	}
}

// TestLoadEdgeModeWithHashOnlyErrors ensures that DRYDOCK_URL + TOKEN_HASH
// (without TOKEN) returns an error.
func TestLoadEdgeModeWithHashOnlyErrors(t *testing.T) {
	setEnv(t,
		"DRYDOCK_URL", "https://drydock.example.com",
		"TOKEN_HASH", "$argon2id$v=19$m=19456,t=2,p=1$c29tZXNhbHQ$aGFzaGhhc2g",
	)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for edge mode with TOKEN_HASH only, got nil")
	}
	if !strings.Contains(err.Error(), "edge mode") {
		t.Fatalf("expected 'edge mode' in error, got: %v", err)
	}
}

// TestLoadTokenHashOnly ensures that TOKEN_HASH alone is accepted.
func TestLoadTokenHashOnly(t *testing.T) {
	const phc = "$argon2id$v=19$m=19456,t=2,p=1$c29tZXNhbHQ$aGFzaGhhc2g"
	setEnv(t, "TOKEN_HASH", phc)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TokenHash != phc {
		t.Fatalf("TokenHash: got %q, want %q", cfg.TokenHash, phc)
	}
	if cfg.Token != "" {
		t.Fatalf("Token should be empty, got %q", cfg.Token)
	}
}

// TestLoadTokenHashFileLoadsFromFile verifies TOKEN_HASH_FILE is read correctly.
func TestLoadTokenHashFileLoadsFromFile(t *testing.T) {
	const phc = "$argon2id$v=19$m=19456,t=2,p=1$c29tZXNhbHQ$aGFzaGhhc2g"

	dir := t.TempDir()
	path := dir + "/token_hash"
	if err := os.WriteFile(path, []byte(phc+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	setEnv(t, "TOKEN_HASH_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TokenHash != phc {
		t.Fatalf("TokenHash: got %q, want %q", cfg.TokenHash, phc)
	}
}

// TestLoadEdgeModeWithBothTokenAndURL verifies that DRYDOCK_URL + TOKEN + PRIVATE_KEY_FILE is valid.
// PRIVATE_KEY_FILE is required in edge mode because drydock rejects token-only agents.
func TestLoadEdgeModeWithBothTokenAndURL(t *testing.T) {
	setEnv(t,
		"DRYDOCK_URL", "https://drydock.example.com",
		"TOKEN", "rawtoken",
		"PRIVATE_KEY_FILE", "/etc/portwing/agent.key",
	)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.IsEdgeMode() {
		t.Fatal("expected IsEdgeMode() to be true")
	}
}

func TestLoadEdgeModeDefaultsOperationsListenerToLoopback(t *testing.T) {
	setEnv(t,
		"DRYDOCK_URL", "https://drydock.example.com",
		"PRIVATE_KEY_FILE", "/etc/portwing/agent.key",
	)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BindAddress != "127.0.0.1" {
		t.Fatalf("edge BindAddress default: got %q, want 127.0.0.1", cfg.BindAddress)
	}
}

// TestLoadEdgeModeHonorsExplicitOperationsBindAddress verifies a non-loopback
// BIND_ADDRESS is accepted once the operator opts in with
// ALLOW_UNAUTHENTICATED_REMOTE — the operations listener has no auth of its
// own, so the opt-in is required (see TestLoadEdgeModeRejectsUnauthenticatedNonLoopbackBind).
func TestLoadEdgeModeHonorsExplicitOperationsBindAddress(t *testing.T) {
	setEnv(t,
		"DRYDOCK_URL", "https://drydock.example.com",
		"PRIVATE_KEY_FILE", "/etc/portwing/agent.key",
		"BIND_ADDRESS", "0.0.0.0",
		"ALLOW_UNAUTHENTICATED_REMOTE", "true",
	)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BindAddress != "0.0.0.0" {
		t.Fatalf("explicit edge BindAddress: got %q, want 0.0.0.0", cfg.BindAddress)
	}
}

// TestLoadEdgeModeRejectsUnauthenticatedNonLoopbackBind ensures edge mode
// fails closed on a non-loopback operations bind without the explicit opt-in:
// health, metrics, and audit export carry no authentication of their own.
func TestLoadEdgeModeRejectsUnauthenticatedNonLoopbackBind(t *testing.T) {
	setEnv(t,
		"DRYDOCK_URL", "https://drydock.example.com",
		"PRIVATE_KEY_FILE", "/etc/portwing/agent.key",
		"BIND_ADDRESS", "0.0.0.0",
	)

	_, err := Load()
	if err == nil {
		t.Fatal("expected unauthenticated non-loopback operations bind to be rejected")
	}
	if !strings.Contains(err.Error(), "ALLOW_UNAUTHENTICATED_REMOTE") {
		t.Fatalf("expected remote opt-in guidance, got: %v", err)
	}
}

// TestLoadEdgeModeLoopbackBindUnaffectedByRemoteOptIn verifies the default
// loopback bind loads whether or not ALLOW_UNAUTHENTICATED_REMOTE is set —
// the opt-in only matters once the bind leaves loopback.
func TestLoadEdgeModeLoopbackBindUnaffectedByRemoteOptIn(t *testing.T) {
	setEnv(t,
		"DRYDOCK_URL", "https://drydock.example.com",
		"PRIVATE_KEY_FILE", "/etc/portwing/agent.key",
		"BIND_ADDRESS", "127.0.0.1",
	)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error for loopback bind: %v", err)
	}
	if cfg.BindAddress != "127.0.0.1" {
		t.Fatalf("edge BindAddress: got %q, want 127.0.0.1", cfg.BindAddress)
	}
}

// TestLoadAuthorizedKeysEnvVars verifies AUTHORIZED_KEYS and AUTHORIZED_KEYS_FILE.
func TestLoadAuthorizedKeysEnvVars(t *testing.T) {
	setEnv(t, "AUTHORIZED_KEYS", "/tmp/test_ak")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuthorizedKeysFile != "/tmp/test_ak" {
		t.Errorf("AuthorizedKeysFile: got %q want /tmp/test_ak", cfg.AuthorizedKeysFile)
	}
}

// TestLoadAuthorizedKeysFileAlias verifies AUTHORIZED_KEYS_FILE is an alias.
func TestLoadAuthorizedKeysFileAlias(t *testing.T) {
	setEnv(t, "AUTHORIZED_KEYS_FILE", "/tmp/test_akf")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuthorizedKeysFile != "/tmp/test_akf" {
		t.Errorf("AuthorizedKeysFile via alias: got %q want /tmp/test_akf", cfg.AuthorizedKeysFile)
	}
}

// TestLoadNonceLRUSizeDefault verifies NONCE_LRU_SIZE defaults to 10000.
func TestLoadNonceLRUSizeDefault(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.NonceLRUSize != 10000 {
		t.Errorf("NonceLRUSize default: got %d want 10000", cfg.NonceLRUSize)
	}
}

// TestLoadMaxClockSkewDefault verifies MAX_CLOCK_SKEW_SECONDS defaults to 60.
func TestLoadMaxClockSkewDefault(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxClockSkewSeconds != 60 {
		t.Errorf("MaxClockSkewSeconds default: got %d want 60", cfg.MaxClockSkewSeconds)
	}
}

// TestLoadEnrollmentToken verifies ENROLLMENT_TOKEN is loaded.
func TestLoadEnrollmentToken(t *testing.T) {
	setEnv(t, "ENROLLMENT_TOKEN", "topsecret")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.EnrollmentToken != "topsecret" {
		t.Errorf("EnrollmentToken: got %q want topsecret", cfg.EnrollmentToken)
	}
}

// TestLoadEnrollmentTokenFile verifies ENROLLMENT_TOKEN_FILE is read.
func TestLoadEnrollmentTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/et"
	if err := os.WriteFile(path, []byte("filetoken\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	setEnv(t, "ENROLLMENT_TOKEN_FILE", path)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.EnrollmentToken != "filetoken" {
		t.Errorf("EnrollmentToken from file: got %q want filetoken", cfg.EnrollmentToken)
	}
}

// TestIsEdgeModeWithAuthorizedKeys verifies IsEdgeMode with AUTHORIZED_KEYS.
// PRIVATE_KEY_FILE is also required because drydock rejects token-only agents.
func TestIsEdgeModeWithAuthorizedKeys(t *testing.T) {
	setEnv(t,
		"DRYDOCK_URL", "https://drydock.example.com",
		"AUTHORIZED_KEYS", "/tmp/ak",
		"PRIVATE_KEY_FILE", "/etc/portwing/agent.key",
	)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.IsEdgeMode() {
		t.Error("expected IsEdgeMode() true with DRYDOCK_URL + AUTHORIZED_KEYS")
	}
}

// TestLoadEdgeModeWithoutPrivateKeyErrors verifies that DRYDOCK_URL without PRIVATE_KEY_FILE
// always fails, even when TOKEN is set. Drydock rejects token-only agents.
func TestLoadEdgeModeWithoutPrivateKeyErrors(t *testing.T) {
	setEnv(t,
		"DRYDOCK_URL", "https://drydock.example.com",
		"TOKEN", "rawtoken",
	)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for edge mode without PRIVATE_KEY_FILE, got nil")
	}
	if !strings.Contains(err.Error(), "PRIVATE_KEY_FILE") {
		t.Fatalf("expected 'PRIVATE_KEY_FILE' in error, got: %v", err)
	}
}

// TestLoadEdgeModeRejectsPlaintextURL ensures a plaintext http:// or ws://
// DRYDOCK_URL is rejected: edge mode never verifies the controller's identity
// beyond TLS, so a plaintext scheme lets an on-path attacker who wins the
// connection race complete the handshake and drive dockerd (see
// internal/edge/client.go's http->ws / https->wss rewrite).
func TestLoadEdgeModeRejectsPlaintextURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{name: "http", url: "http://drydock.example.com"},
		{name: "ws", url: "ws://drydock.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t,
				"DRYDOCK_URL", tc.url,
				"PRIVATE_KEY_FILE", "/etc/portwing/agent.key",
			)

			_, err := Load()
			if err == nil {
				t.Fatal("expected plaintext controller URL to be rejected")
			}
			if !strings.Contains(err.Error(), "ALLOW_INSECURE_EDGE_URL") {
				t.Fatalf("expected opt-in guidance, got: %v", err)
			}
		})
	}
}

// TestLoadEdgeModePlaintextURLWithOptInStartsAndWarns verifies
// ALLOW_INSECURE_EDGE_URL=true lets a plaintext controller URL through, but
// only after logging a prominent warning — mirroring the standard-mode
// ALLOW_UNAUTHENTICATED path.
func TestLoadEdgeModePlaintextURLWithOptInStartsAndWarns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	setEnv(t,
		"DRYDOCK_URL", "http://drydock.example.com",
		"PRIVATE_KEY_FILE", "/etc/portwing/agent.key",
		"ALLOW_INSECURE_EDGE_URL", "true",
	)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error with ALLOW_INSECURE_EDGE_URL set: %v", err)
	}
	if cfg.DrydockURL != "http://drydock.example.com" {
		t.Fatalf("DrydockURL: got %q, want the plaintext URL unchanged", cfg.DrydockURL)
	}
	if !strings.Contains(buf.String(), "plaintext scheme") {
		t.Fatalf("expected a plaintext-scheme warning to be logged, got: %s", buf.String())
	}
}

// TestLoadEdgeModeSecureURLsUnaffected verifies https:// and wss:// URLs load
// without error or requiring the opt-in.
func TestLoadEdgeModeSecureURLsUnaffected(t *testing.T) {
	for _, u := range []string{"https://drydock.example.com", "wss://drydock.example.com"} {
		t.Run(u, func(t *testing.T) {
			setEnv(t,
				"DRYDOCK_URL", u,
				"PRIVATE_KEY_FILE", "/etc/portwing/agent.key",
			)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected error for secure URL %q: %v", u, err)
			}
			if cfg.DrydockURL != u {
				t.Fatalf("DrydockURL: got %q, want %q", cfg.DrydockURL, u)
			}
		})
	}
}

// TestListenAddressBracketsIPv6 covers the address form every listener is
// built from. Concatenating host and port with a colon produced "::1:3000"
// for the documented unbracketed IPv6 bind, which net.Listen rejects, and
// naive JoinHostPort double-brackets an already-bracketed host.
func TestListenAddressBracketsIPv6(t *testing.T) {
	for _, tc := range []struct {
		name string
		bind string
		port string
		want string
	}{
		{name: "ipv6 loopback unbracketed", bind: "::1", port: "3000", want: "[::1]:3000"},
		{name: "ipv6 loopback bracketed", bind: "[::1]", port: "3000", want: "[::1]:3000"},
		{name: "ipv6 wildcard", bind: "::", port: "3000", want: "[::]:3000"},
		{name: "ipv6 wildcard bracketed", bind: "[::]", port: "0", want: "[::]:0"},
		{name: "ipv4 wildcard", bind: "0.0.0.0", port: "3000", want: "0.0.0.0:3000"},
		{name: "ipv4 loopback", bind: "127.0.0.1", port: "3000", want: "127.0.0.1:3000"},
		{name: "hostname", bind: "localhost", port: "3000", want: "localhost:3000"},
		{name: "empty bind is wildcard", bind: "", port: "3000", want: ":3000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ListenAddress(tc.bind, tc.port); got != tc.want {
				t.Fatalf("ListenAddress(%q, %q) = %q, want %q", tc.bind, tc.port, got, tc.want)
			}
		})
	}
}

// TestListenAddressIsListenable proves the joined form is one net.Listen
// actually accepts, not just one that looks right.
func TestListenAddressIsListenable(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "::1", "::", "[::1]", "0.0.0.0"} {
		t.Run(bind, func(t *testing.T) {
			t.Parallel()
			addr := ListenAddress(bind, "0")
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				// A malformed join ("::1:0") surfaces as net.AddrError
				// ("too many colons in address"); a host without the
				// address family surfaces as a syscall error, which is
				// the environment's answer and not a regression.
				var addrErr *net.AddrError
				if errors.As(err, &addrErr) {
					t.Fatalf("ListenAddress(%q, \"0\") = %q is not a valid listen address: %v", bind, addr, err)
				}
				t.Skipf("net.Listen(tcp, %q): %v (address family unavailable on this host)", addr, err)
			}
			if err := ln.Close(); err != nil {
				t.Fatalf("close listener: %v", err)
			}
		})
	}
}

// TestLoadRejectsNonPositiveOrOverflowingIntervals covers the values that used
// to reach time.NewTicker and panic the agent at startup: NewTicker panics on a
// non-positive interval, and a seconds value large enough to overflow the
// conversion to a time.Duration wraps negative and panics the same way.
func TestLoadRejectsNonPositiveOrOverflowingIntervals(t *testing.T) {
	// A seconds count whose nanosecond conversion wraps past MaxInt64. Asserted
	// rather than assumed, so the case still means something if the constant
	// or the units ever move.
	const overflowing = "9223372037"
	overflowSeconds, err := strconv.Atoi(overflowing)
	if err != nil {
		t.Skipf("%s does not fit in an int on this platform: %v", overflowing, err)
	}
	if d := time.Duration(overflowSeconds) * time.Second; d > 0 {
		t.Fatalf("%s seconds converts to %v, expected the multiply to overflow", overflowing, d)
	}

	for _, key := range []string{"HEARTBEAT_INTERVAL", "DD_POLL_INTERVAL"} {
		for _, tc := range []struct {
			name  string
			value string
			want  string
		}{
			{name: "zero", value: "0", want: "positive"},
			{name: "negative", value: "-1", want: "positive"},
			{name: "overflows a duration", value: overflowing, want: "at most"},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				t.Setenv(key, tc.value)
				_, err := Load()
				if err == nil {
					t.Fatalf("Load() accepted %s=%s; it reaches time.NewTicker and panics", key, tc.value)
				}
				if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Load() error for %s=%s = %v, want it to name %s and say %q", key, tc.value, err, key, tc.want)
				}
			})
		}
	}
}

// TestLoadAcceptsIntervalBoundary pins the accepted side of the bound so the
// validation cannot drift into rejecting ordinary configuration.
func TestLoadAcceptsIntervalBoundary(t *testing.T) {
	for _, value := range []string{"1", "300", strconv.FormatInt(MaxIntervalSeconds, 10)} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("HEARTBEAT_INTERVAL", value)
			t.Setenv("DD_POLL_INTERVAL", value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() rejected valid intervals of %s seconds: %v", value, err)
			}
			for name, seconds := range map[string]int{
				"HEARTBEAT_INTERVAL": cfg.HeartbeatInterval,
				"DD_POLL_INTERVAL":   cfg.DDPollInterval,
			} {
				if d := time.Duration(seconds) * time.Second; d <= 0 {
					t.Fatalf("%s=%s survived Load but converts to %v, which panics time.NewTicker", name, value, d)
				}
			}
		})
	}
}
