package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var (
	envVarKeyPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

	envVarDenylist = map[string]bool{
		"LD_PRELOAD":        true,
		"LD_LIBRARY_PATH":   true,
		"PATH":              true,
		"DOCKER_HOST":       true,
		"DOCKER_CONFIG":     true,
		"DOCKER_CERT_PATH":  true,
		"DOCKER_TLS_VERIFY": true,
		"DOCKER_CONTEXT":    true,
		"HOME":              true,
		"SHELL":             true,
		"BASH_ENV":          true,
		"ENV":               true,
		"CDPATH":            true,
		"IFS":               true,
	}
)

type ComposeRequest struct {
	Operation     string            `json:"operation"`
	StackName     string            `json:"stackName"`
	StackDir      string            `json:"stackDir,omitempty"`
	Services      []string          `json:"services,omitempty"`
	Build         bool              `json:"build,omitempty"`
	ForceRecreate bool              `json:"forceRecreate,omitempty"`
	RemoveVolumes bool              `json:"removeVolumes,omitempty"`
	NoDeps        bool              `json:"noDeps,omitempty"`
	Tail          int               `json:"tail,omitempty"`
	EnvVars       map[string]string `json:"envVars,omitempty"`
	Files         map[string]string `json:"files,omitempty"`
	RegistryAuth  *RegistryAuth     `json:"registryAuth,omitempty"`
}

type RegistryAuth struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type ComposeResponse struct {
	Success bool   `json:"success"`
	Output  string `json:"output"`
	Error   string `json:"error,omitempty"`
}

const (
	// defaultComposeLogsTail and maxComposeLogsTail bound the "logs"
	// operation's --tail flag, which is otherwise unbounded and can dump an
	// entire container's log history into memory. Mirrors the MCP
	// container-logs default/cap in internal/mcp/mcp.go so the two log-tail
	// entry points behave the same way.
	defaultComposeLogsTail = 100
	maxComposeLogsTail     = 500

	// maxComposeOutputBytes bounds the combined stdout+stderr captured from a
	// compose subprocess. A verbose "up --build" or an unfiltered "logs" call
	// can otherwise write without limit, and since Execute buffers the whole
	// thing before returning, that's an easy way to OOM the agent process
	// that's managing every other stack too. 10MB is generous for a compose
	// invocation's output while still bounding worst case.
	maxComposeOutputBytes = 10 * 1024 * 1024
)

// composeLogsTail clamps a requested --tail value to
// [1, maxComposeLogsTail], defaulting to defaultComposeLogsTail when the
// request didn't specify one (or specified a nonsense non-positive value).
// The logs operation always passes --tail so an unbounded dump is never the
// default behavior.
func composeLogsTail(requested int) int {
	if requested <= 0 {
		return defaultComposeLogsTail
	}
	if requested > maxComposeLogsTail {
		return maxComposeLogsTail
	}
	return requested
}

// outputLimiter enforces a shared byte budget across a compose subprocess's
// stdout and stderr writers, so the two streams are bounded together rather
// than each independently up to the cap (which would let combined output run
// to 2x the intended limit). os/exec drains stdout and stderr concurrently
// from separate goroutines, so the budget is mutex-guarded.
type outputLimiter struct {
	mu        sync.Mutex
	remaining int
	truncated bool
}

func newOutputLimiter(limit int) *outputLimiter {
	return &outputLimiter{remaining: limit}
}

// reserve claims up to n bytes from the remaining budget and returns how
// many were actually granted, flipping truncated once the budget runs out.
func (l *outputLimiter) reserve(n int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > l.remaining {
		l.truncated = true
		n = l.remaining
	}
	l.remaining -= n
	return n
}

func (l *outputLimiter) isTruncated() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.truncated
}

// boundedWriter retains bytes up to its shared limiter's remaining budget and
// silently drops the rest. Write always reports success and the full length
// written even when it drops bytes: the caller here is os/exec's internal
// pipe-copy goroutine, and returning an error would stop it draining the
// pipe, which would leave the subprocess blocked writing to a full pipe and
// cmd.Run never returning — trading an OOM for a permanent hang.
type boundedWriter struct {
	buf     bytes.Buffer
	limiter *outputLimiter
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if n := w.limiter.reserve(len(p)); n > 0 {
		w.buf.Write(p[:n])
	}
	return len(p), nil
}

// ComposeManager executes Docker Compose operations in a managed stacks
// directory.
type ComposeManager struct {
	stacksDir    string
	composeBin   string
	isV2         bool
	apiVersion   string
	dockerSocket string

	// stackLocks holds one reference-counted mutex per active stack directory
	// (canonical, cleaned, absolute path), not per StackName: file and
	// process operations are keyed by directory, so the lock must be too, or
	// two requests naming the same directory under different StackNames can
	// interleave. Entries are removed after the final owner or waiter
	// releases them.
	stackLocksMu sync.Mutex
	stackLocks   map[string]*stackLockEntry
}

type stackLockEntry struct {
	mu   sync.Mutex
	refs int
}

// NewComposeManager creates a ComposeManager. It auto-detects whether the
// system has Compose v2 (docker compose) or v1 (docker-compose).
func NewComposeManager(stacksDir, apiVersion, dockerSocket string) *ComposeManager {
	cm := &ComposeManager{
		stacksDir:    stacksDir,
		apiVersion:   apiVersion,
		dockerSocket: dockerSocket,
	}
	cm.detectCompose()
	return cm
}

// detectCompose checks for Compose v2 first, then falls back to v1.
func (cm *ComposeManager) detectCompose() {
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err == nil {
		if strings.Contains(string(out), "v2") || strings.Contains(string(out), "V2") {
			cm.composeBin = "docker"
			cm.isV2 = true
			return
		}
	}

	if _, err := exec.LookPath("docker-compose"); err == nil {
		cm.composeBin = "docker-compose"
		cm.isV2 = false
		return
	}

	// Default to v2.
	cm.composeBin = "docker"
	cm.isV2 = true
}

// lockStack acquires the per-stack-directory mutex, creating it on first
// use, and returns a func that releases it. The key must be a canonical
// (cleaned, absolute) directory path: requests for different directories get
// independent mutexes and never block each other.
func (cm *ComposeManager) lockStack(stackDirKey string) func() {
	cm.stackLocksMu.Lock()
	if cm.stackLocks == nil {
		cm.stackLocks = make(map[string]*stackLockEntry)
	}
	entry := cm.stackLocks[stackDirKey]
	if entry == nil {
		entry = &stackLockEntry{}
		cm.stackLocks[stackDirKey] = entry
	}
	entry.refs++
	cm.stackLocksMu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()

		cm.stackLocksMu.Lock()
		entry.refs--
		if entry.refs == 0 && cm.stackLocks[stackDirKey] == entry {
			delete(cm.stackLocks, stackDirKey)
		}
		cm.stackLocksMu.Unlock()
	}
}

// effectiveStackDir returns the stack directory a request operates on: the
// explicit StackDir if set, else StackName (matching validateRequest,
// writeStackFiles, and buildCommand).
func effectiveStackDir(req ComposeRequest) string {
	if req.StackDir != "" {
		return req.StackDir
	}
	return req.StackName
}

// Execute dispatches a compose operation and returns the result.
func (cm *ComposeManager) Execute(ctx context.Context, req ComposeRequest) (*ComposeResponse, error) {
	if err := validateComposeOperation(req.Operation); err != nil {
		return &ComposeResponse{Success: false, Error: err.Error()}, nil
	}
	// validateRequest also resolves and returns the canonical stack
	// directory, so Execute doesn't re-derive and re-validate it a second
	// time for the lock key below: a second, independent resolveStackRoot
	// call here could never disagree with validateRequest's (both resolve
	// the same StackDir/StackName fallback against the same immutable
	// cm.stacksDir), which left that second call's error branch dead code.
	stackDirKey, err := cm.validateRequest(req)
	if err != nil {
		return &ComposeResponse{Success: false, Error: err.Error()}, nil
	}

	// Serialize the write-then-exec sequence below per stack directory:
	// without this, two concurrent "up" requests for the same directory can
	// interleave — request A writes its compose files, request B overwrites
	// them before A runs "docker compose", and A deploys B's configuration
	// while reporting success for A's request. This is keyed by the
	// canonical stack directory rather than StackName because file and
	// process operations below are keyed by directory: two requests with
	// different StackNames but the same StackDir must still serialize. Both
	// requests are already authenticated with full compose control over this
	// stack, so the lock is about correctness (deploying the config you
	// asked for) rather than access control.
	unlock := cm.lockStack(stackDirKey)
	defer unlock()

	if req.Files != nil {
		if err := cm.writeStackFiles(req); err != nil {
			return &ComposeResponse{Success: false, Error: fmt.Sprintf("writing stack files: %v", err)}, nil
		}
	}

	if req.RegistryAuth != nil {
		if err := cm.registryLogin(ctx, req.RegistryAuth); err != nil {
			return &ComposeResponse{Success: false, Error: fmt.Sprintf("registry login: %v", err)}, nil
		}
	}

	cmd, err := cm.buildCommand(ctx, req)
	if err != nil {
		return &ComposeResponse{Success: false, Error: err.Error()}, nil
	}

	cmd.Env = cm.buildEnv()

	// stdout and stderr share one limiter so the combined capture is bounded,
	// not each stream up to the cap independently.
	limiter := newOutputLimiter(maxComposeOutputBytes)
	stdout := &boundedWriter{limiter: limiter}
	stderr := &boundedWriter{limiter: limiter}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()

	output := stdout.buf.String()
	if stderr.buf.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.buf.String()
	}
	if limiter.isTruncated() {
		output += fmt.Sprintf("\n[output truncated: exceeded %d MB combined output limit]", maxComposeOutputBytes/(1024*1024))
	}

	if err != nil {
		return &ComposeResponse{
			Success: false,
			Output:  output,
			Error:   err.Error(),
		}, nil
	}

	return &ComposeResponse{
		Success: true,
		Output:  output,
	}, nil
}

// validateRequest checks the request for invalid or dangerous inputs. On
// success it also returns the request's canonical (cleaned, absolute) stack
// directory, so a caller that needs it (Execute, for the per-stack lock key)
// doesn't have to re-derive and re-validate it with a second call that could
// never disagree with this one.
func (cm *ComposeManager) validateRequest(req ComposeRequest) (string, error) {
	if req.StackName == "" {
		return "", fmt.Errorf("stack name is required")
	}
	if req.Operation != "" {
		if err := validateComposeOperation(req.Operation); err != nil {
			return "", err
		}
	}

	// Validate env var keys and values.
	for key, val := range req.EnvVars {
		if !envVarKeyPattern.MatchString(key) {
			return "", fmt.Errorf("invalid env var key: %q", key)
		}
		if envVarDenylist[key] {
			return "", fmt.Errorf("env var %q is not allowed", key)
		}
		if strings.ContainsAny(val, "\n\r\x00") {
			return "", fmt.Errorf("env var %q value contains invalid characters (newline, carriage return, or null)", key)
		}
	}

	// Validate service names (reject names starting with "-").
	for _, svc := range req.Services {
		if strings.HasPrefix(svc, "-") {
			return "", fmt.Errorf("invalid service name: %q", svc)
		}
	}

	// Validate registry auth server if present.
	if req.RegistryAuth != nil {
		if err := validateRegistryServer(req.RegistryAuth.Server); err != nil {
			return "", err
		}
	}

	// Validate stack path is within stacksDir, capturing its canonical form.
	stackDir := effectiveStackDir(req)
	stackDirKey, err := cm.resolveStackRoot(stackDir)
	if err != nil {
		return "", fmt.Errorf("invalid stack path: %w", err)
	}
	for relPath := range req.Files {
		if _, err := cm.resolvePath(stackDir, relPath); err != nil {
			return "", fmt.Errorf("invalid stack file path %q: %w", relPath, err)
		}
	}

	return stackDirKey, nil
}

func validateComposeOperation(operation string) error {
	switch operation {
	case "up", "down", "pull", "ps", "logs", "restart", "stop", "start":
		return nil
	default:
		return fmt.Errorf("unsupported compose operation: %q", operation)
	}
}

func validateRegistryServer(server string) error {
	if server == "" {
		return fmt.Errorf("registryAuth.server is required and must be an https URI or bare registry host")
	}

	effectiveURI := server
	if !strings.Contains(server, "://") {
		effectiveURI = "https://" + server
	}
	u, err := url.ParseRequestURI(effectiveURI)
	if err != nil {
		return fmt.Errorf("registryAuth.server is not valid: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("registryAuth.server must use https scheme, got %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("registryAuth.server must include a host")
	}
	if u.User != nil {
		return fmt.Errorf("registryAuth.server must not include user info")
	}
	if u.Path != "" || u.RawPath != "" {
		return fmt.Errorf("registryAuth.server must not include a path")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("registryAuth.server must not include a query")
	}
	if u.Fragment != "" {
		return fmt.Errorf("registryAuth.server must not include a fragment")
	}
	if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("registryAuth.server has an invalid port")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("registryAuth.server has an invalid port")
		}
	}
	return nil
}

// writeStackFiles writes the files from the request into the stack directory.
// File contents prefixed with "base64:" are decoded before writing.
func (cm *ComposeManager) writeStackFiles(req ComposeRequest) error {
	stackDir := req.StackDir
	if stackDir == "" {
		stackDir = req.StackName
	}
	if err := os.MkdirAll(cm.stacksDir, 0o750); err != nil {
		return fmt.Errorf("creating stacks directory: %w", err)
	}
	root, err := os.OpenRoot(cm.stacksDir)
	if err != nil {
		return fmt.Errorf("opening stacks directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	for relPath, content := range req.Files {
		rootedPath, err := cm.rootedPath(stackDir, relPath)
		if err != nil {
			return fmt.Errorf("resolving path %q: %w", relPath, err)
		}

		var data []byte
		if strings.HasPrefix(content, "base64:") {
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(content, "base64:"))
			if err != nil {
				return fmt.Errorf("decoding base64 for %q: %w", relPath, err)
			}
			data = decoded
		} else {
			data = []byte(content)
		}

		if err := writeRootedFile(root, rootedPath, data); err != nil {
			return fmt.Errorf("writing %q: %w", relPath, err)
		}
	}

	// Write .env.drydock if env vars are provided.
	if len(req.EnvVars) > 0 {
		envPath, err := cm.rootedPath(stackDir, ".env.drydock")
		if err != nil {
			return fmt.Errorf("resolving .env.drydock path: %w", err)
		}

		var buf bytes.Buffer
		for key, val := range req.EnvVars {
			fmt.Fprintf(&buf, "%s=%s\n", key, val)
		}

		if err := writeRootedFile(root, envPath, buf.Bytes()); err != nil {
			return fmt.Errorf("writing .env.drydock: %w", err)
		}
	}

	return nil
}

func (cm *ComposeManager) rootedPath(stackDir, path string) (string, error) {
	absPath, err := cm.resolvePath(stackDir, path)
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(cm.stacksDir)
	if err != nil {
		return "", fmt.Errorf("resolving stacks directory: %w", err)
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return "", fmt.Errorf("resolving rooted path: %w", err)
	}
	if rel == "." || !pathWithin(".", rel) {
		return "", fmt.Errorf("path %q is not a file below stacks directory", path)
	}
	return rel, nil
}

// writeRootedFile creates each directory component without following symlinks
// and writes through os.Root, which keeps the operation beneath STACKS_DIR even
// if a path is swapped concurrently.
func writeRootedFile(root *os.Root, name string, data []byte) error {
	if err := mkdirRootedNoSymlinks(root, filepath.Dir(name)); err != nil {
		return err
	}
	if info, err := root.Lstat(name); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink file %q", name)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular file %q", name)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking file %q: %w", name, err)
	}

	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return nil
}

func mkdirRootedNoSymlinks(root *os.Root, dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	current := ""
	for _, component := range strings.Split(filepath.Clean(dir), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		switch {
		case err == nil && info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("refusing symlink directory %q", current)
		case err == nil && !info.IsDir():
			return fmt.Errorf("path component %q is not a directory", current)
		case err == nil:
			continue
		case os.IsNotExist(err):
			if err := root.Mkdir(current, 0o750); err != nil {
				return fmt.Errorf("creating directory %q: %w", current, err)
			}
		default:
			return fmt.Errorf("checking directory %q: %w", current, err)
		}
	}
	return nil
}

// statRootedEnvFile reports the absolute path of the stack's .env.drydock file
// when it exists as a regular file beneath STACKS_DIR. The lookup is performed
// through os.Root so path resolution stays contained even if a component is
// swapped for a symlink between resolution and use. An empty path and a nil
// error mean the stack simply has no env file.
func (cm *ComposeManager) statRootedEnvFile(stackDir string) (string, error) {
	rootedEnv, err := cm.rootedPath(stackDir, ".env.drydock")
	if err != nil {
		return "", err
	}

	root, err := os.OpenRoot(cm.stacksDir)
	if err != nil {
		return "", fmt.Errorf("opening stacks directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	info, err := root.Stat(rootedEnv)
	switch {
	case os.IsNotExist(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("checking env file: %w", err)
	case !info.Mode().IsRegular():
		return "", nil
	}

	absRoot, err := filepath.Abs(cm.stacksDir)
	if err != nil {
		return "", fmt.Errorf("resolving stacks directory: %w", err)
	}
	return filepath.Join(absRoot, rootedEnv), nil
}

// buildCommand constructs the exec.Cmd for the requested compose operation.
func (cm *ComposeManager) buildCommand(ctx context.Context, req ComposeRequest) (*exec.Cmd, error) {
	if err := validateComposeOperation(req.Operation); err != nil {
		return nil, err
	}

	stackDir := req.StackDir
	if stackDir == "" {
		stackDir = req.StackName
	}

	projectDir, err := cm.resolvePath(stackDir, ".")
	if err != nil {
		return nil, fmt.Errorf("resolving project dir: %w", err)
	}

	var args []string

	if cm.isV2 {
		args = append(args, "compose")
	}

	// Project directory.
	args = append(args, "--project-directory", projectDir)

	// Env file if it exists. The existence check goes through os.Root so a
	// swapped symlink cannot make it stat a file outside STACKS_DIR.
	if envFile, err := cm.statRootedEnvFile(stackDir); err == nil && envFile != "" {
		args = append(args, "--env-file", envFile)
	}

	switch req.Operation {
	case "up":
		args = append(args, "up", "-d", "--remove-orphans")
		if req.Build {
			args = append(args, "--build")
		}
		if req.ForceRecreate {
			args = append(args, "--force-recreate")
		}
		if req.NoDeps {
			args = append(args, "--no-deps")
		}
		args = append(args, req.Services...)

	case "down":
		args = append(args, "down", "--remove-orphans")
		if req.RemoveVolumes {
			args = append(args, "--volumes")
		}

	case "pull":
		args = append(args, "pull")
		args = append(args, req.Services...)

	case "ps":
		args = append(args, "ps", "--format", "json")

	case "logs":
		args = append(args, "logs")
		// --tail is always passed (never omitted) so an unset or unbounded
		// request can't dump a container's entire log history through the
		// bounded-but-still-finite output capture above.
		args = append(args, "--tail", fmt.Sprintf("%d", composeLogsTail(req.Tail)))
		args = append(args, req.Services...)

	case "restart":
		args = append(args, "restart")
		args = append(args, req.Services...)

	case "stop":
		args = append(args, "stop")
		args = append(args, req.Services...)

	case "start":
		args = append(args, "start")
		args = append(args, req.Services...)

	default:
		return nil, fmt.Errorf("unsupported compose operation: %q", req.Operation)
	}

	// #nosec G204 -- compose binary is detected locally and args are built from validated request fields without shell expansion.
	cmd := exec.CommandContext(ctx, cm.composeBin, args...)
	cmd.Dir = projectDir
	return cmd, nil
}

// registryLogin performs a docker login using --password-stdin.
func (cm *ComposeManager) registryLogin(ctx context.Context, auth *RegistryAuth) error {
	// #nosec G204 -- arguments are passed directly to docker without a shell; password is sent on stdin.
	cmd := exec.CommandContext(ctx, "docker", "login", "--username", auth.Username, "--password-stdin", auth.Server)
	cmd.Stdin = strings.NewReader(auth.Password)
	cmd.Env = cm.buildEnv()

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker login: %s: %w", stderr.String(), err)
	}
	return nil
}

// resolvePath resolves a relative path within a single stack directory.
func (cm *ComposeManager) resolvePath(stackDir, path string) (string, error) {
	base, err := cm.resolveStackRoot(stackDir)
	if err != nil {
		return "", err
	}

	if filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q must be relative", path)
	}
	cleanPath := filepath.Clean(path)
	full, err := filepath.Abs(filepath.Join(base, cleanPath))
	if err != nil {
		return "", fmt.Errorf("resolving absolute path: %w", err)
	}
	if !pathWithin(base, full) {
		return "", fmt.Errorf("path %q escapes stack directory", path)
	}
	return full, nil
}

func (cm *ComposeManager) resolveStackRoot(stackDir string) (string, error) {
	if filepath.IsAbs(stackDir) {
		return "", fmt.Errorf("stack path %q must be relative", stackDir)
	}
	cleanStackDir := filepath.Clean(stackDir)
	base := filepath.Join(cm.stacksDir, cleanStackDir)
	resolved, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolving stack path: %w", err)
	}
	absBase, err := filepath.Abs(cm.stacksDir)
	if err != nil {
		return "", fmt.Errorf("resolving stacks dir: %w", err)
	}
	if !pathWithin(absBase, resolved) {
		return "", fmt.Errorf("stack path %q escapes stacks directory", stackDir)
	}
	return resolved, nil
}

func pathWithin(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel))
}

// buildEnv constructs the subprocess environment, setting DOCKER_API_VERSION
// and DOCKER_HOST so compose commands target the correct daemon.
func (cm *ComposeManager) buildEnv() []string {
	env := os.Environ()

	env = append(env, "DOCKER_API_VERSION="+strings.TrimPrefix(cm.apiVersion, "v"))

	if cm.dockerSocket != "" {
		env = append(env, "DOCKER_HOST=unix://"+cm.dockerSocket)
	}

	return env
}
