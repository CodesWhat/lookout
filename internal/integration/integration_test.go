//go:build integration

// Package integration runs the Portwing server against the runner's real
// Docker daemon. Each test starts a Portwing server on a random port with
// TOKEN auth pointing at /var/run/docker.sock (or the socket specified by
// PORTWING_TEST_DOCKER_SOCKET). Tests verify: health endpoints, auth
// enforcement, container list API, Prometheus metrics, MCP protocol, and
// the SSE events stream. A real alpine container is started and cleaned up
// so list assertions are non-trivial.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/auth"
)

const (
	testToken  = "integration-test-token-abc123"
	startupMax = 10 * time.Second
	testImage  = "alpine:3.20"
)

// startServer launches portwing as a subprocess and returns its base URL.
// It blocks until the health endpoint returns 200 or the deadline is hit.
func startServer(t *testing.T) (baseURL string, cleanup func()) {
	t.Helper()
	return startServerWithEnv(t, nil, testToken)
}

// startServerWithEnv launches portwing with extra env vars appended and a
// specific bearer token. Pass extraEnv as nil and token as testToken for the
// standard TOKEN-auth harness; pass token as "" to start without TOKEN (e.g.
// for Ed25519-only auth via an AUTHORIZED_KEYS entry in extraEnv). An ephemeral
// port is obtained via net.Listen(:0) to avoid port conflicts.
func startServerWithEnv(t *testing.T, extraEnv []string, token string) (baseURL string, cleanup func()) {
	t.Helper()

	dockerSocket := os.Getenv("PORTWING_TEST_DOCKER_SOCKET")
	if dockerSocket == "" {
		dockerSocket = "/var/run/docker.sock"
	}

	// Use os.MkdirTemp("", "lk") to stay within darwin's 104-byte unix socket path limit.
	tmpDir, err := os.MkdirTemp("", "lk")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}

	// Obtain an ephemeral port by binding on :0 and reading the resolved
	// address, then close the listener so portwing can bind the same port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen :0: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()

	// Build the binary first (go build ./cmd/portwing -o <tmpdir>/portwing).
	binPath := tmpDir + "/portwing"
	build := exec.Command("go", "build", "-o", binPath, "./cmd/portwing")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	env := append(os.Environ(),
		"PORT="+port,
		"BIND_ADDRESS=127.0.0.1",
		"DOCKER_SOCKET="+dockerSocket,
		"ADAPTER=drydock",
		"LOG_LEVEL=error",      // keep integration test output quiet
		"SKIP_DF_COLLECTION=1", // /proc/df not available in CI
		"DD_POLL_INTERVAL=1",   // refresh inventory every 1s so a freshly started container appears promptly (default is 300s)
	)
	if token != "" {
		env = append(env, "TOKEN="+token)
	}
	env = append(env, extraEnv...)

	cmd := exec.Command(binPath)
	cmd.Env = env
	cmd.Dir = tmpDir

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting portwing: %v", err)
	}

	base := "http://127.0.0.1:" + port

	// Wait for the server to become healthy.
	deadline := time.Now().Add(startupMax)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/_portwing/health") //nolint:noctx
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	return base, func() {
		cmd.Process.Kill()   //nolint:errcheck
		cmd.Wait()           //nolint:errcheck
		os.RemoveAll(tmpDir) //nolint:errcheck
	}
}

// moduleRoot walks up from this file's directory until it finds go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := dir[:strings.LastIndex(dir, "/")]
		if parent == dir {
			t.Fatal("could not find go.mod")
		}
		dir = parent
	}
}

// startAlpineContainer pulls alpine (already pulled by CI step) and runs a
// sleep container so the container list is non-trivial. Returns the container ID.
func startAlpineContainer(t *testing.T) (id string, cleanupFn func()) {
	t.Helper()
	cmd := exec.Command("docker", "run", "-d", "--name", "portwing-integ-test", testImage, "sleep", "300")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	id = strings.TrimSpace(string(out))
	return id, func() {
		exec.Command("docker", "rm", "-f", id).Run() //nolint:errcheck
	}
}

// get performs an authenticated GET request.
func get(t *testing.T, base, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("NewRequest GET %s: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// TestMain runs integration tests only when docker is available.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintln(os.Stderr, "docker not found in PATH; skipping integration tests")
		os.Exit(0)
	}
	socket := os.Getenv("PORTWING_TEST_DOCKER_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	if _, err := os.Stat(socket); err != nil {
		fmt.Fprintf(os.Stderr, "Docker socket %s not found; skipping integration tests\n", socket)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestHealthEndpoint(t *testing.T) {
	base, cleanup := startServer(t)
	defer cleanup()

	resp, err := http.Get(base + "/_portwing/health") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status: got %d, want 200", resp.StatusCode)
	}

	var body struct {
		Status     string `json:"status"`
		Live       bool   `json:"live"`
		Ready      bool   `json:"ready"`
		Mode       string `json:"mode"`
		Docker     string `json:"docker"`
		Controller string `json:"controller"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding health response: %v", err)
	}
	if body.Status != "healthy" || !body.Live || !body.Ready {
		t.Errorf("health state: got %+v, want healthy/live/ready", body)
	}
	if body.Mode != "standard" || body.Docker != "connected" || body.Controller != "not_applicable" {
		t.Errorf("operational health fields: got %+v", body)
	}
}

func TestAuth401WithoutToken(t *testing.T) {
	base, cleanup := startServer(t)
	defer cleanup()

	// /api/containers requires auth.
	resp, err := http.Get(base + "/api/containers") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /api/containers: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated request: got %d, want 401", resp.StatusCode)
	}
}

func TestContainerListWithAuth(t *testing.T) {
	base, cleanup := startServer(t)
	defer cleanup()

	// Start a real container so the list is non-trivial.
	ctrID, cleanupCtr := startAlpineContainer(t)
	defer cleanupCtr()

	// The drydock adapter serves /api/containers from a cached snapshot that a
	// background poller refreshes; the container we just started only becomes
	// visible after the next poll cycle. Poll with a deadline rather than relying
	// on a fixed delay, whose adequacy varies with load on CI runners.
	deadline := time.Now().Add(startupMax)
	var containers []map[string]any
	var found bool
	for time.Now().Before(deadline) {
		resp := get(t, base, "/api/containers")
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("GET /api/containers: got %d, want 200\nbody: %s", resp.StatusCode, body)
		}
		containers = nil
		err := json.NewDecoder(resp.Body).Decode(&containers)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decoding containers: %v", err)
		}
		for _, c := range containers {
			if id, _ := c["id"].(string); id == ctrID {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	// We must at least see the container we just started.
	if !found {
		t.Errorf("container %s not found in /api/containers within %s (saw %d containers)",
			ctrID[:min(12, len(ctrID))], startupMax, len(containers))
	}
}

func TestMetricsContainsBuildInfo(t *testing.T) {
	base, cleanup := startServer(t)
	defer cleanup()

	resp := get(t, base, "/_portwing/metrics")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /_portwing/metrics: got %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading metrics body: %v", err)
	}

	if !bytes.Contains(body, []byte("portwing_build_info")) {
		t.Errorf("metrics body does not contain portwing_build_info\nbody excerpt: %.500s", body)
	}
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	base, cleanup := startServer(t)
	defer cleanup()

	// initialize request.
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`
	req, err := http.NewRequest(http.MethodPost, base+"/_portwing/mcp", strings.NewReader(initBody))
	if err != nil {
		t.Fatalf("NewRequest MCP: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /_portwing/mcp initialize: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("MCP initialize: got %d, want 200\nbody: %s", resp.StatusCode, body)
	}

	var initResp map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&initResp); err != nil {
		t.Fatalf("decoding MCP initialize response: %v", err)
	}
	if string(initResp["jsonrpc"]) != `"2.0"` {
		t.Errorf("MCP initialize: jsonrpc = %s, want \"2.0\"", initResp["jsonrpc"])
	}

	// tools/list request.
	listBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	req2, err := http.NewRequest(http.MethodPost, base+"/_portwing/mcp", strings.NewReader(listBody))
	if err != nil {
		t.Fatalf("NewRequest MCP tools/list: %v", err)
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+testToken)

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("POST /_portwing/mcp tools/list: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("MCP tools/list: got %d, want 200\nbody: %s", resp2.StatusCode, body)
	}

	var listResp struct {
		JSONRPC string `json:"jsonrpc"`
		Result  struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&listResp); err != nil {
		t.Fatalf("decoding MCP tools/list response: %v", err)
	}
	if listResp.JSONRPC != "2.0" {
		t.Errorf("tools/list: jsonrpc = %q, want \"2.0\"", listResp.JSONRPC)
	}
	if len(listResp.Result.Tools) == 0 {
		t.Error("tools/list: expected at least one tool, got 0")
	}
}

func readInitialSSEEventTypes(r io.Reader) ([]string, error) {
	// The drydock SSE protocol frames every event as `data: <json>` with no
	// `event:` line; the discriminator is the JSON payload's "type" field
	// (so EventSource clients read JSON.parse(e.data).type). Accumulate each
	// event's data payload and inspect its type. The initial handshake contract
	// requires dd:ack first and dd:watcher-snapshot second.
	reader := bufio.NewReader(r)
	var eventTypes []string
	var data strings.Builder

	for len(eventTypes) < 2 {
		line, err := reader.ReadString('\n')
		if err != nil {
			return eventTypes, fmt.Errorf("read SSE line: %w", err)
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line, "data:"))
			continue
		}
		if line == "" && data.Len() > 0 {
			// Blank line = end of event: parse the accumulated data payload.
			var evt struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(data.String())), &evt); err != nil {
				return nil, fmt.Errorf("decode SSE event: %w", err)
			}
			eventTypes = append(eventTypes, evt.Type)
			data.Reset()
		}
	}
	return eventTypes, nil
}

func TestReadInitialSSEEventTypesAcceptsLargeSnapshot(t *testing.T) {
	largeField := strings.Repeat("x", 8*1024*1024)
	stream := "data: {\"type\":\"dd:ack\"}\n\n" +
		"data: {\"type\":\"dd:watcher-snapshot\",\"padding\":\"" + largeField + "\"}\n\n"

	eventTypes, err := readInitialSSEEventTypes(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("read large SSE snapshot: %v", err)
	}
	if len(eventTypes) != 2 || eventTypes[0] != "dd:ack" || eventTypes[1] != "dd:watcher-snapshot" {
		t.Fatalf("initial SSE events = %v, want [dd:ack dd:watcher-snapshot]", eventTypes)
	}
}

func TestSSEEventsFirstEventIsAck(t *testing.T) {
	base, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/events", nil)
	if err != nil {
		t.Fatalf("NewRequest SSE: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/events: got %d, want 200", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type: got %q, want text/event-stream", ct)
	}

	eventTypes, err := readInitialSSEEventTypes(resp.Body)
	if err != nil && ctx.Err() == nil {
		t.Errorf("read SSE events: %v", err)
	}

	if len(eventTypes) < 2 {
		t.Fatalf("initial SSE events = %v, want [dd:ack dd:watcher-snapshot]", eventTypes)
	}
	if eventTypes[0] != "dd:ack" || eventTypes[1] != "dd:watcher-snapshot" {
		t.Fatalf("initial SSE events = %v, want [dd:ack dd:watcher-snapshot]", eventTypes)
	}
}

// writeAuthorizedKeysFile writes pub to a fresh temp authorized_keys file
// (mode 0600 so the world-readable check in parseAuthorizedKeys passes) and
// returns its path. Format matches parseKeyLine: "ed25519 <base64-std-pubkey>
// <comment>".
func writeAuthorizedKeysFile(t *testing.T, pub ed25519.PublicKey, comment string) string {
	t.Helper()

	keysDir, err := os.MkdirTemp("", "lk-keys")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(keysDir) })

	b64 := base64.StdEncoding.EncodeToString(pub)
	keysPath := filepath.Join(keysDir, "authorized_keys")
	if err := os.WriteFile(keysPath, []byte("ed25519 "+b64+" "+comment+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile authorized_keys: %v", err)
	}
	return keysPath
}

// signEd25519Request signs req with the given Ed25519 keypair and sets the
// version 2 signature headers (X-Portwing-Key-ID, X-Portwing-Timestamp,
// X-Portwing-Nonce, X-Portwing-Signature, X-Portwing-Signature-Version). body
// must be the exact bytes that will be sent as the request body (nil/empty
// for none); the caller is responsible for attaching it to req separately,
// since req.Body has already been consumed for hashing here.
func signEd25519Request(t *testing.T, req *http.Request, body []byte, pub ed25519.PublicKey, priv ed25519.PrivateKey) {
	t.Helper()

	tsUnix := time.Now().Unix()
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	nonce := hex.EncodeToString(nonceBytes) // 32 hex characters

	// Version 2 canonical message covers the complete origin-form target.
	bodyHash := auth.BodyHashHex(body)
	msg := auth.CanonicalMessage(req.Method, auth.CanonicalRequestTarget(req.URL), bodyHash, tsUnix, nonce)
	sig := ed25519.Sign(priv, msg)

	req.Header.Set(auth.HeaderKeyID, auth.KeyIDForPublicKey(pub))
	req.Header.Set(auth.HeaderTimestamp, strconv.FormatInt(tsUnix, 10))
	req.Header.Set(auth.HeaderNonce, nonce)
	req.Header.Set(auth.HeaderSignature, base64.RawURLEncoding.EncodeToString(sig))
	req.Header.Set(auth.HeaderSignatureVersion, auth.SignatureVersion2)
}

// TestEd25519Auth verifies that portwing enforces Ed25519 signature auth on a
// protected endpoint when started with AUTHORIZED_KEYS and no TOKEN: an
// unsigned request is rejected with 401, and a properly signed request is
// accepted with 200. It targets /_portwing/info (an auth-gated endpoint) rather
// than /_portwing/health (which is intentionally unauthenticated), so the
// signature path is actually exercised.
func TestEd25519Auth(t *testing.T) {
	// Generate a fresh Ed25519 keypair for this test.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	keysPath := writeAuthorizedKeysFile(t, pub, "integ-test")

	// Start portwing with AUTHORIZED_KEYS set and no TOKEN (Ed25519-only auth).
	base, cleanup := startServerWithEnv(t,
		[]string{"AUTHORIZED_KEYS=" + keysPath},
		"", // no bearer token
	)
	defer cleanup()

	const target = "/_portwing/info" // auth-gated, unlike /_portwing/health

	// Negative control: an unsigned request must be rejected with 401. This
	// proves the endpoint is genuinely gated, so the positive case is meaningful.
	unsigned, err := http.NewRequest(http.MethodGet, base+target, nil)
	if err != nil {
		t.Fatalf("NewRequest (unsigned): %v", err)
	}
	unResp, err := http.DefaultClient.Do(unsigned)
	if err != nil {
		t.Fatalf("unsigned GET %s: %v", target, err)
	}
	unResp.Body.Close()
	if unResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned request: got %d, want 401 (endpoint must be auth-gated)", unResp.StatusCode)
	}

	// Positive: a correctly signed request must be accepted with 200.
	req, err := http.NewRequest(http.MethodGet, base+target, nil)
	if err != nil {
		t.Fatalf("NewRequest (signed): %v", err)
	}
	signEd25519Request(t, req, nil, pub, priv)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("signed GET %s: %v", target, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Ed25519 auth: got %d, want 200\nreason: %s\nbody: %s",
			resp.StatusCode, resp.Header.Get("X-Portwing-Reason"), body)
	}
}

// TestMCPUnderEd25519Auth verifies that /_portwing/mcp, the same endpoint
// TestMCPInitializeAndToolsList exercises under plain TOKEN auth, is also
// correctly gated by Ed25519 key auth: a request with a bad signature is
// rejected with 401 and an X-Portwing-Reason header (never reaching the MCP
// handler), and a correctly signed request drives a full initialize +
// tools/list exchange to completion, mirroring what
// TestMCPInitializeAndToolsList sends.
func TestMCPUnderEd25519Auth(t *testing.T) {
	// Generate a fresh Ed25519 keypair for this test.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	keysPath := writeAuthorizedKeysFile(t, pub, "integ-test-mcp")

	// Start portwing with AUTHORIZED_KEYS set and no TOKEN (Ed25519-only auth).
	base, cleanup := startServerWithEnv(t,
		[]string{"AUTHORIZED_KEYS=" + keysPath},
		"", // no bearer token
	)
	defer cleanup()

	const target = "/_portwing/mcp"
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`

	// Negative control: a request signed with a corrupted signature must be
	// rejected with 401 and the auth reason header, and must never reach the
	// MCP handler (the body must not be a JSON-RPC response).
	badReq, err := http.NewRequest(http.MethodPost, base+target, strings.NewReader(initBody))
	if err != nil {
		t.Fatalf("NewRequest (bad signature): %v", err)
	}
	badReq.Header.Set("Content-Type", "application/json")
	signEd25519Request(t, badReq, []byte(initBody), pub, priv)

	// Corrupt the signature after signing: flip a bit but keep it valid
	// base64url, so verification fails on the Ed25519 check itself (reason
	// "invalid-signature") rather than on header parsing.
	sigBytes, err := base64.RawURLEncoding.DecodeString(badReq.Header.Get(auth.HeaderSignature))
	if err != nil {
		t.Fatalf("decode signature for corruption: %v", err)
	}
	sigBytes[0] ^= 0xFF
	badReq.Header.Set(auth.HeaderSignature, base64.RawURLEncoding.EncodeToString(sigBytes))

	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("POST %s (bad signature): %v", target, err)
	}
	badBody, _ := io.ReadAll(badResp.Body)
	badResp.Body.Close()

	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("MCP with bad signature: got %d, want 401\nbody: %s", badResp.StatusCode, badBody)
	}
	wantReason := auth.ReasonFor(auth.ErrBadSignature)
	if reason := badResp.Header.Get(auth.HeaderReason); reason != wantReason {
		t.Errorf("MCP with bad signature: X-Portwing-Reason = %q, want %q", reason, wantReason)
	}
	// The rejected request must never reach the MCP handler: the body must
	// not be a JSON-RPC response carrying our request ID.
	var rejectedRPC map[string]json.RawMessage
	if err := json.Unmarshal(badBody, &rejectedRPC); err == nil {
		if _, ok := rejectedRPC["result"]; ok {
			t.Errorf("MCP with bad signature: got a JSON-RPC result, want the request rejected before the MCP handler\nbody: %s", badBody)
		}
	}

	// Positive: a correctly signed initialize request must be accepted with 200.
	initReq, err := http.NewRequest(http.MethodPost, base+target, strings.NewReader(initBody))
	if err != nil {
		t.Fatalf("NewRequest MCP initialize: %v", err)
	}
	initReq.Header.Set("Content-Type", "application/json")
	signEd25519Request(t, initReq, []byte(initBody), pub, priv)

	initResp, err := http.DefaultClient.Do(initReq)
	if err != nil {
		t.Fatalf("POST %s initialize: %v", target, err)
	}
	defer initResp.Body.Close()

	if initResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(initResp.Body)
		t.Fatalf("MCP initialize under Ed25519 auth: got %d, want 200\nreason: %s\nbody: %s",
			initResp.StatusCode, initResp.Header.Get(auth.HeaderReason), body)
	}

	var initRPC map[string]json.RawMessage
	if err := json.NewDecoder(initResp.Body).Decode(&initRPC); err != nil {
		t.Fatalf("decoding MCP initialize response: %v", err)
	}
	if string(initRPC["jsonrpc"]) != `"2.0"` {
		t.Errorf("MCP initialize: jsonrpc = %s, want \"2.0\"", initRPC["jsonrpc"])
	}
	if string(initRPC["id"]) != "1" {
		t.Errorf("MCP initialize: id = %s, want 1", initRPC["id"])
	}
	if _, ok := initRPC["result"]; !ok {
		t.Errorf("MCP initialize: response has no result key, want one\nbody: %v", initRPC)
	}
	if raw, ok := initRPC["error"]; ok {
		t.Errorf("MCP initialize: response has an error key, want none\nerror: %s", raw)
	}

	// tools/list request, same connection pattern as TestMCPInitializeAndToolsList.
	listBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	listReq, err := http.NewRequest(http.MethodPost, base+target, strings.NewReader(listBody))
	if err != nil {
		t.Fatalf("NewRequest MCP tools/list: %v", err)
	}
	listReq.Header.Set("Content-Type", "application/json")
	signEd25519Request(t, listReq, []byte(listBody), pub, priv)

	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatalf("POST %s tools/list: %v", target, err)
	}
	defer listResp.Body.Close()

	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("MCP tools/list under Ed25519 auth: got %d, want 200\nreason: %s\nbody: %s",
			listResp.StatusCode, listResp.Header.Get(auth.HeaderReason), body)
	}

	listRespBody, err := io.ReadAll(listResp.Body)
	if err != nil {
		t.Fatalf("reading MCP tools/list response: %v", err)
	}

	var listFields map[string]json.RawMessage
	if err := json.Unmarshal(listRespBody, &listFields); err != nil {
		t.Fatalf("decoding MCP tools/list response: %v", err)
	}
	if string(listFields["jsonrpc"]) != `"2.0"` {
		t.Errorf("tools/list: jsonrpc = %s, want \"2.0\"", listFields["jsonrpc"])
	}
	if string(listFields["id"]) != "2" {
		t.Errorf("tools/list: id = %s, want 2", listFields["id"])
	}
	if _, ok := listFields["result"]; !ok {
		t.Errorf("tools/list: response has no result key, want one\nbody: %v", listFields)
	}
	if raw, ok := listFields["error"]; ok {
		t.Errorf("tools/list: response has an error key, want none\nerror: %s", raw)
	}

	var listRPC struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listRespBody, &listRPC); err != nil {
		t.Fatalf("decoding MCP tools/list result: %v", err)
	}
	if len(listRPC.Result.Tools) == 0 {
		t.Error("tools/list: expected at least one tool, got 0")
	}
}
