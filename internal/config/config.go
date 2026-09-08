package config

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Config struct {
	// Connection
	DrydockURL     string
	Token          string
	TokenHash      string
	CACert         string
	TLSSkipVerify  bool
	Port           string
	BindAddress    string
	TLSCert        string
	TLSKey         string
	TrustedProxies []string
	// AllowUnauthenticated is an explicit development-only opt-in for standard
	// mode. Non-loopback binds additionally require AllowUnauthenticatedRemote.
	AllowUnauthenticated       bool
	AllowUnauthenticatedRemote bool

	// Docker
	DockerSocket string
	StacksDir    string

	// Identity
	AgentID   string
	AgentName string

	// Operational
	HeartbeatInterval int
	RequestTimeout    int
	ReconnectDelay    int
	MaxReconnectDelay int
	WelcomeTimeout    int
	LogLevel          string
	SkipDFCollection  bool
	// MaxStreamSessions and MaxExecSessions bound standard mode's concurrent
	// streaming proxies and hijacked exec/attach sessions (SPEC 7.3). A
	// non-positive value disables the bound, which a controller driving more
	// than the default 100 concurrent streams needs.
	MaxStreamSessions int
	MaxExecSessions   int

	// Adapter
	Adapter string

	// Drydock compat
	DDPollInterval int

	// Audit
	AuditLog        string
	AuditBufferSize int

	// Ed25519 key authentication (Model B: operator-provisioned authorized_keys)
	AuthorizedKeysFile  string // AUTHORIZED_KEYS / AUTHORIZED_KEYS_FILE
	MaxClockSkewSeconds int    // MAX_CLOCK_SKEW_SECONDS (default 60)
	NonceLRUSize        int    // NONCE_LRU_SIZE (default 10000)

	// Model C enrollment token (consumed on first use)
	EnrollmentToken string // ENROLLMENT_TOKEN / ENROLLMENT_TOKEN_FILE

	// Edge-mode private key for signed hello (PRIVATE_KEY_FILE)
	PrivateKeyFile string
}

func Load() (*Config, error) {
	token := getEnv("TOKEN", "")
	if token == "" {
		token = getEnv("DD_AGENT_SECRET", "")
	}

	// Support TOKEN_FILE / DD_AGENT_SECRET_FILE
	if tokenFile := getEnv("TOKEN_FILE", ""); tokenFile != "" {
		t, err := loadTokenFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading TOKEN_FILE: %w", err)
		}
		token = t
	} else if tokenFile := getEnv("DD_AGENT_SECRET_FILE", ""); tokenFile != "" {
		t, err := loadTokenFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading DD_AGENT_SECRET_FILE: %w", err)
		}
		token = t
	}

	// Support TOKEN_HASH / TOKEN_HASH_FILE
	tokenHash := getEnv("TOKEN_HASH", "")
	if tokenHashFile := getEnv("TOKEN_HASH_FILE", ""); tokenHashFile != "" {
		h, err := loadTokenFile(tokenHashFile)
		if err != nil {
			return nil, fmt.Errorf("reading TOKEN_HASH_FILE: %w", err)
		}
		tokenHash = h
	}

	if token != "" && tokenHash != "" {
		return nil, fmt.Errorf("TOKEN and TOKEN_HASH are mutually exclusive: choose one")
	}

	// Support AUTHORIZED_KEYS / AUTHORIZED_KEYS_FILE (aliases).
	authorizedKeysFile := getEnv("AUTHORIZED_KEYS", "")
	if authorizedKeysFile == "" {
		authorizedKeysFile = getEnv("AUTHORIZED_KEYS_FILE", "")
	}

	// Support ENROLLMENT_TOKEN / ENROLLMENT_TOKEN_FILE.
	enrollmentToken := getEnv("ENROLLMENT_TOKEN", "")
	if enrollmentToken == "" {
		if etFile := getEnv("ENROLLMENT_TOKEN_FILE", ""); etFile != "" {
			t, err := loadTokenFile(etFile)
			if err != nil {
				return nil, fmt.Errorf("reading ENROLLMENT_TOKEN_FILE: %w", err)
			}
			enrollmentToken = t
		}
	}

	drydockURL := getEnv("DRYDOCK_URL", "")
	if drydockURL != "" && token == "" && tokenHash != "" && authorizedKeysFile == "" {
		return nil, fmt.Errorf("edge mode (DRYDOCK_URL) requires TOKEN or AUTHORIZED_KEYS, not TOKEN_HASH alone")
	}
	if drydockURL != "" && token == "" && tokenHash == "" && authorizedKeysFile == "" {
		privateKeyFile := getEnv("PRIVATE_KEY_FILE", "")
		if privateKeyFile == "" {
			return nil, fmt.Errorf("edge mode (DRYDOCK_URL) requires TOKEN, AUTHORIZED_KEYS, or PRIVATE_KEY_FILE")
		}
	}
	// Drydock categorically rejects token-only agents (ed25519-required); a
	// PRIVATE_KEY_FILE must always be present in edge mode so startup fails fast
	// with a clear message rather than looping forever on a rejected hello.
	if drydockURL != "" && getEnv("PRIVATE_KEY_FILE", "") == "" {
		return nil, fmt.Errorf("edge mode (DRYDOCK_URL) requires PRIVATE_KEY_FILE for Ed25519 authentication; drydock rejects token-only agents")
	}

	// Edge mode's controller auth is one-directional: portwing signs its hello
	// so the controller can verify the agent, but nothing verifies the
	// controller's identity in return — that trust rests entirely on TLS. A
	// plaintext http(s)/ws(s) scheme lets an on-path attacker who wins the
	// connection race complete the handshake and drive dockerd, so fail closed
	// the same way standard mode's unauthenticated bind does, unless the
	// operator explicitly opts in.
	allowInsecureEdgeURL := getEnvBool("ALLOW_INSECURE_EDGE_URL", false)
	if drydockURL != "" {
		scheme := getURLScheme(drydockURL)
		if scheme == "http" || scheme == "ws" {
			if !allowInsecureEdgeURL {
				return nil, fmt.Errorf("refusing plaintext controller URL %q: use https:// or wss://, or set ALLOW_INSECURE_EDGE_URL=true", drydockURL)
			}
			slog.Warn("connecting to the controller over a plaintext scheme: the controller's identity is unverified and an on-path attacker can drive dockerd — set ALLOW_INSECURE_EDGE_URL=true only for trusted local testing", "drydockURL", drydockURL)
		}
	}

	agentID := getEnv("AGENT_ID", "")
	if agentID == "" {
		agentID = uuid.New().String()
	}

	agentName := getEnv("AGENT_NAME", "")
	if agentName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			agentName = "portwing"
		} else {
			agentName = hostname
		}
	}

	dockerSocket := getEnv("DOCKER_SOCKET", "")
	if dockerSocket == "" {
		dockerSocket = detectDockerSocket()
	}

	tlsCert := getEnv("TLS_CERT", "")
	tlsKey := getEnv("TLS_KEY", "")
	if (tlsCert == "") != (tlsKey == "") {
		return nil, fmt.Errorf("TLS_CERT and TLS_KEY must either both be set or both be empty")
	}

	// Standard mode must accept inbound controller traffic by default. Edge
	// mode's HTTP listener is operational-only and intentionally unauthenticated,
	// so keep its health, metrics, and audit-export surface on loopback unless an
	// operator explicitly opts into a broader BIND_ADDRESS.
	bindAddressDefault := "0.0.0.0"
	if drydockURL != "" {
		bindAddressDefault = "127.0.0.1"
	}

	cfg := &Config{
		DrydockURL:                 drydockURL,
		Token:                      token,
		TokenHash:                  tokenHash,
		CACert:                     getEnv("CA_CERT", ""),
		TLSSkipVerify:              getEnvBool("TLS_SKIP_VERIFY", false),
		Port:                       getEnv("PORT", "3000"),
		BindAddress:                getEnv("BIND_ADDRESS", bindAddressDefault),
		TLSCert:                    tlsCert,
		TLSKey:                     tlsKey,
		TrustedProxies:             splitCSV(getEnv("TRUSTED_PROXIES", "")),
		AllowUnauthenticated:       getEnvBool("ALLOW_UNAUTHENTICATED", false),
		AllowUnauthenticatedRemote: getEnvBool("ALLOW_UNAUTHENTICATED_REMOTE", false),

		DockerSocket: dockerSocket,
		StacksDir:    getEnv("STACKS_DIR", "/data/stacks"),

		AgentID:   agentID,
		AgentName: agentName,

		HeartbeatInterval: getEnvInt("HEARTBEAT_INTERVAL", 30),
		RequestTimeout:    getEnvInt("REQUEST_TIMEOUT", 30),
		ReconnectDelay:    getEnvInt("RECONNECT_DELAY", 1),
		MaxReconnectDelay: getEnvInt("MAX_RECONNECT_DELAY", 60),
		WelcomeTimeout:    getEnvInt("WELCOME_TIMEOUT", 30),
		LogLevel:          getEnv("LOG_LEVEL", "info"),
		SkipDFCollection:  getEnvBool("SKIP_DF_COLLECTION", false),
		MaxStreamSessions: getEnvInt("MAX_STREAM_SESSIONS", 100),
		MaxExecSessions:   getEnvInt("MAX_EXEC_SESSIONS", 100),

		Adapter: getEnv("ADAPTER", "drydock"),

		DDPollInterval: getEnvInt("DD_POLL_INTERVAL", 300),

		AuditLog:        getEnv("AUDIT_LOG", ""),
		AuditBufferSize: getEnvInt("AUDIT_BUFFER_SIZE", 256),

		AuthorizedKeysFile:  authorizedKeysFile,
		MaxClockSkewSeconds: getEnvInt("MAX_CLOCK_SKEW_SECONDS", 60),
		NonceLRUSize:        getEnvInt("NONCE_LRU_SIZE", 10000),

		EnrollmentToken: enrollmentToken,

		PrivateKeyFile: getEnv("PRIVATE_KEY_FILE", ""),
	}

	// HEARTBEAT_INTERVAL and DD_POLL_INTERVAL are converted to a
	// time.Duration and handed straight to time.NewTicker, which panics on a
	// non-positive interval — so a zero or negative value crashed the agent at
	// startup rather than being rejected. A value large enough to overflow the
	// seconds-to-Duration multiply wraps negative and panics the same way, so
	// both ends are checked here, once, instead of at each ticker.
	for _, interval := range []struct {
		name    string
		seconds int
	}{
		{name: "HEARTBEAT_INTERVAL", seconds: cfg.HeartbeatInterval},
		{name: "DD_POLL_INTERVAL", seconds: cfg.DDPollInterval},
	} {
		if err := validateIntervalSeconds(interval.name, interval.seconds); err != nil {
			return nil, err
		}
	}

	// Edge mode's operations listener (health, metrics, audit export) carries
	// no authentication of its own — see the bindAddressDefault comment above.
	// A non-loopback bind hands the full audit trail and metrics to anyone
	// routable, so fail closed the same way standard mode's unauthenticated
	// Docker proxy does, unless the operator explicitly opts in.
	if drydockURL != "" && !IsLoopbackBind(cfg.BindAddress) {
		if !cfg.AllowUnauthenticatedRemote {
			return nil, fmt.Errorf("refusing unauthenticated non-loopback operations bind %q: bind to loopback or set ALLOW_UNAUTHENTICATED_REMOTE=true", cfg.BindAddress)
		}
		slog.Warn("operations listener (health, metrics, audit export) bound to a non-loopback address with no authentication: reachable by anyone routable — set ALLOW_UNAUTHENTICATED_REMOTE=true only on an isolated monitoring network", "bindAddress", cfg.BindAddress)
	}

	return cfg, nil
}

// maxIntervalSeconds is the largest seconds value that survives every
// time.Duration conversion applied to a configured interval. The tightest is
// edge mode's read deadline, which doubles the heartbeat before scaling it to
// nanoseconds (readDeadline in internal/edge/client.go), so the bound is
// math.MaxInt64 divided by two seconds' worth of nanoseconds. That is roughly
// 146 years, far past any real interval and short of the overflow.
const maxIntervalSeconds = int64(math.MaxInt64) / (2 * int64(time.Second))

// validateIntervalSeconds rejects a seconds-valued interval that time.NewTicker
// would panic on: non-positive, or large enough that the conversion to a
// time.Duration overflows and wraps negative.
func validateIntervalSeconds(name string, seconds int) error {
	if seconds <= 0 {
		return fmt.Errorf("%s must be a positive number of seconds, got %d", name, seconds)
	}
	if int64(seconds) > maxIntervalSeconds {
		return fmt.Errorf("%s must be at most %d seconds (larger values overflow the conversion to a duration), got %d",
			name, maxIntervalSeconds, seconds)
	}
	return nil
}

// ListenAddress joins a configured BIND_ADDRESS and PORT into an address
// net.Listen accepts. Concatenating them with a colon is wrong for IPv6: the
// documented unbracketed form ("::1", "::") produces "::1:3000", which
// net.SplitHostPort reads as host "::1:3000" with no port and the listen
// fails. net.JoinHostPort adds the brackets, but it adds them to any host
// containing a colon, so an already-bracketed "[::1]" would come back as
// "[[::1]]:3000" — strip one layer first. Shared by standard mode's
// Docker-proxy listener, edge mode's operations listener, and the startup log
// line, so all three agree on the address that is actually bound.
func ListenAddress(bindAddress, port string) string {
	host := bindAddress
	if len(host) >= 2 && strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return net.JoinHostPort(host, port)
}

// IsLoopbackBind reports whether address is a loopback bind (127.0.0.1,
// ::1, or "localhost"). Shared by standard mode's Docker-proxy listener and
// edge mode's operations listener, both of which fail closed on an
// unauthenticated non-loopback bind unless ALLOW_UNAUTHENTICATED_REMOTE is set.
func IsLoopbackBind(address string) bool {
	if strings.EqualFold(address, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(address, "[]"))
	return ip != nil && ip.IsLoopback()
}

// getURLScheme returns the lowercase scheme of rawURL, or "" if it cannot be
// parsed as a URL. Used to detect a plaintext http/ws controller URL before
// client.connect rewrites it to wss/ws (see internal/edge/client.go).
func getURLScheme(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Scheme
}

func (c *Config) IsEdgeMode() bool {
	if c.DrydockURL == "" {
		return false
	}
	return c.Token != "" || c.AuthorizedKeysFile != "" || c.PrivateKeyFile != ""
}

func detectDockerSocket() string {
	candidates := []string{
		"/var/run/docker.sock",
	}

	home := os.Getenv("HOME")
	if home != "" {
		candidates = append(candidates,
			home+"/.docker/run/docker.sock",
			home+"/.orbstack/run/docker.sock",
		)
	}

	candidates = append(candidates, "/run/docker.sock")

	for _, path := range candidates {
		// #nosec G703 -- candidates are fixed well-known Docker socket paths plus HOME-derived local Docker paths.
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return "/var/run/docker.sock"
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return fallback
	}
	return n
}

func getEnvBool(key string, fallback bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	switch strings.ToLower(val) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		return fallback
	}
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadTokenFile(path string) (string, error) {
	// #nosec G304 -- token file path is explicit operator configuration.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
