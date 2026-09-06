package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- NewComposeManager ----

func TestNewComposeManager_ReturnsNonNil(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cm := NewComposeManager(dir, "v1.44", "/var/run/docker.sock")
	if cm == nil {
		t.Fatal("NewComposeManager returned nil")
	}
	if cm.stacksDir != dir {
		t.Fatalf("stacksDir = %q, want %q", cm.stacksDir, dir)
	}
	// detectCompose sets either "docker" or "docker-compose" — just check non-empty.
	if cm.composeBin == "" {
		t.Fatal("composeBin not set by detectCompose")
	}
}

// ---- Execute: validation failure ----

func TestExecute_ValidationFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cm := &ComposeManager{stacksDir: dir, composeBin: "docker", isV2: true}

	// A supported operation reaches request validation, where the empty stack
	// name is rejected before any filesystem or command side effects.
	resp, err := cm.Execute(t.Context(), ComposeRequest{Operation: "up"})
	if err != nil {
		t.Fatalf("Execute: unexpected error %v", err)
	}
	if resp.Success {
		t.Fatal("Execute: expected Success=false for invalid request, got true")
	}
	if resp.Error == "" {
		t.Fatal("Execute: expected non-empty Error for invalid request")
	}
	if !strings.Contains(resp.Error, "stack name is required") {
		t.Fatalf("Execute error = %q, want stack-name validation error", resp.Error)
	}
}

// ---- Execute: writeStackFiles failure ----

func TestExecute_WriteStackFilesFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cm := &ComposeManager{stacksDir: dir, composeBin: "docker", isV2: true}

	// Create the stack dir as a file (not a directory) so MkdirAll on it fails.
	stackPath := filepath.Join(dir, "app")
	if err := os.WriteFile(stackPath, []byte("not-a-dir"), 0o600); err != nil {
		t.Fatal(err)
	}

	req := ComposeRequest{
		StackName: "app",
		Operation: "up",
		Files: map[string]string{
			"nested/docker-compose.yml": "services: {}\n",
		},
	}
	resp, err := cm.Execute(t.Context(), req)
	if err != nil {
		t.Fatalf("Execute: unexpected error %v", err)
	}
	if resp.Success {
		t.Fatal("Execute: expected Success=false when writeStackFiles fails")
	}
}

// ---- Execute: buildCommand error (unsupported operation) ----

func TestExecute_BuildCommandError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	cm := &ComposeManager{stacksDir: dir, composeBin: "docker", isV2: true}

	resp, err := cm.Execute(t.Context(), ComposeRequest{StackName: "app", Operation: "nuke"})
	if err != nil {
		t.Fatalf("Execute: unexpected error %v", err)
	}
	if resp.Success {
		t.Fatal("Execute: expected Success=false for unsupported operation")
	}
}

func TestExecuteRejectsUnsupportedOperationBeforeSideEffects(t *testing.T) {
	dir := t.TempDir()
	stackDir := filepath.Join(dir, "app")
	if err := os.MkdirAll(stackDir, 0o750); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(stackDir, "docker-compose.yml")
	const original = "services:\n  web:\n    image: nginx:stable\n"
	if err := os.WriteFile(composePath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	callsPath := filepath.Join(binDir, "calls")
	fakeDocker := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PORTWING_TEST_CALLS\"\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTWING_TEST_CALLS", callsPath)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	cm := &ComposeManager{stacksDir: dir, composeBin: fakeDocker}
	req := ComposeRequest{
		StackName: "app",
		Operation: "nuke",
		Files: map[string]string{
			"docker-compose.yml": "services:\n  web:\n    image: attacker.invalid/replacement\n",
		},
		RegistryAuth: &RegistryAuth{
			Server:   "https://registry.example.com",
			Username: "user",
			Password: "pass",
		},
	}

	if err := cm.validateRequest(req); err == nil {
		t.Error("validateRequest accepted an unsupported compose operation")
	}
	resp, err := cm.Execute(t.Context(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Success || !strings.Contains(resp.Error, "unsupported compose operation") {
		t.Errorf("response = %+v, want an unsupported-operation error", resp)
	}
	cm.stackLocksMu.Lock()
	_, loaded := cm.stackLocks[req.StackName]
	cm.stackLocksMu.Unlock()
	if loaded {
		t.Error("unsupported operation created a stack lock before rejection")
	}
	if got, err := os.ReadFile(composePath); err != nil {
		t.Errorf("read preexisting compose file: %v", err)
	} else if string(got) != original {
		t.Errorf("preexisting compose file was mutated before rejection: got %q", got)
	}
	if calls, err := os.ReadFile(callsPath); err == nil {
		t.Errorf("Docker was invoked before rejection: %q", calls)
	} else if !os.IsNotExist(err) {
		t.Errorf("read Docker invocation record: %v", err)
	}
}

// ---- buildCommand: project dir resolve failure ----

// TestBuildCommand_ResolveProjectDirError exercises the error path in
// buildCommand when resolvePath fails for the project directory.
func TestBuildCommand_ResolveProjectDirError(t *testing.T) {
	t.Parallel()

	cm := &ComposeManager{stacksDir: t.TempDir(), composeBin: "docker", isV2: true}

	// Use a traversal stack dir so resolvePath fails.
	req := ComposeRequest{StackName: "ignored", StackDir: "../escape", Operation: "up"}
	_, err := cm.buildCommand(t.Context(), req)
	if err == nil {
		t.Fatal("expected error for escaping stack dir in buildCommand, got nil")
	}
}

// ---- Execute: command failure (binary returns non-zero) ----

func TestExecute_CommandFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	// Use /usr/bin/false (always exits 1) as composeBin.
	cm := &ComposeManager{stacksDir: dir, composeBin: "/usr/bin/false", isV2: false}

	resp, err := cm.Execute(t.Context(), ComposeRequest{StackName: "app", Operation: "up"})
	if err != nil {
		t.Fatalf("Execute: unexpected error %v", err)
	}
	if resp.Success {
		t.Fatal("Execute: expected Success=false when command fails")
	}
	if resp.Error == "" {
		t.Fatal("Execute: expected non-empty Error")
	}
}

// ---- Execute: command success ----

func TestExecute_CommandSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	// Use /usr/bin/true (always exits 0) as composeBin.
	cm := &ComposeManager{stacksDir: dir, composeBin: "/usr/bin/true", isV2: false}

	resp, err := cm.Execute(t.Context(), ComposeRequest{StackName: "app", Operation: "up"})
	if err != nil {
		t.Fatalf("Execute: unexpected error %v", err)
	}
	if !resp.Success {
		t.Fatalf("Execute: expected Success=true when command succeeds, got Error=%q", resp.Error)
	}
}

// ---- Execute: command produces both stdout and stderr (merge branch) ----

// TestExecute_MergesStdoutAndStderr exercises the branch where both stdout
// and stderr are non-empty (output != "" when stderr is appended).
//
// Note: not parallel because it execs a script it just wrote. Any concurrent
// fork in this process inherits the still-open write descriptor, and exec of a
// file held open for writing fails with ETXTBSY (golang/go#22315). It failed
// that way in CI on 2026-08-21 with "text file busy". The only other test here
// that writes and then execs a fake binary, TestRegistryLogin_Success, is
// already serial because it mutates PATH, so this was the one exposed case.
func TestExecute_MergesStdoutAndStderr(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o750); err != nil {
		t.Fatal(err)
	}

	// Write a tiny script that produces both stdout and stderr output.
	scriptPath := filepath.Join(dir, "compose-both.sh")
	script := "#!/usr/bin/env sh\nprintf 'stdout output'\nprintf 'stderr output' >&2\nexit 1\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cm := &ComposeManager{stacksDir: dir, composeBin: scriptPath, isV2: false}

	resp, err := cm.Execute(t.Context(), ComposeRequest{StackName: "app", Operation: "up"})
	if err != nil {
		t.Fatalf("Execute: unexpected error %v", err)
	}
	// Success should be false (exit 1), and output should be stdout, a
	// newline separator, then stderr — exact content, not just non-empty, so
	// the separator logic (output != "" before appending stderr) is pinned.
	if resp.Success {
		t.Fatal("Execute: expected Success=false")
	}
	want := "stdout output\nstderr output"
	if resp.Output != want {
		t.Fatalf("Execute: Output = %q, want %q", resp.Output, want)
	}
}

// TestExecute_StdoutOnly_NoSeparatorNewline exercises the boundary of the
// "stderr.buf.Len() > 0" check: when stderr produced nothing, Output must be
// exactly the stdout content with no trailing separator appended.
//
// Note: not parallel, same ETXTBSY reasoning as TestExecute_MergesStdoutAndStderr.
func TestExecute_StdoutOnly_NoSeparatorNewline(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o750); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(dir, "compose-stdout-only.sh")
	script := "#!/usr/bin/env sh\nprintf 'stdout only'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cm := &ComposeManager{stacksDir: dir, composeBin: scriptPath, isV2: false}

	resp, err := cm.Execute(t.Context(), ComposeRequest{StackName: "app", Operation: "up"})
	if err != nil {
		t.Fatalf("Execute: unexpected error %v", err)
	}
	if !resp.Success {
		t.Fatalf("Execute: expected Success=true, got Error=%q", resp.Error)
	}
	if resp.Output != "stdout only" {
		t.Fatalf("Execute: Output = %q, want %q (no stderr means no separator)", resp.Output, "stdout only")
	}
}

// TestExecute_StderrOnly_NoLeadingSeparator exercises the boundary of the
// "output != \"\"" check from the other side: when stdout produced nothing,
// Output must be exactly the stderr content with no leading separator.
//
// Note: not parallel, same ETXTBSY reasoning as TestExecute_MergesStdoutAndStderr.
func TestExecute_StderrOnly_NoLeadingSeparator(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o750); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(dir, "compose-stderr-only.sh")
	script := "#!/usr/bin/env sh\nprintf 'stderr only' >&2\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cm := &ComposeManager{stacksDir: dir, composeBin: scriptPath, isV2: false}

	resp, err := cm.Execute(t.Context(), ComposeRequest{StackName: "app", Operation: "up"})
	if err != nil {
		t.Fatalf("Execute: unexpected error %v", err)
	}
	if !resp.Success {
		t.Fatalf("Execute: expected Success=true, got Error=%q", resp.Error)
	}
	if resp.Output != "stderr only" {
		t.Fatalf("Execute: Output = %q, want %q (empty stdout means no leading separator)", resp.Output, "stderr only")
	}
}

// ---- Execute: output truncation ----

// TestExecute_TruncatesOversizedOutput exercises the bounded-writer cap using
// a fake compose binary that writes well past maxComposeOutputBytes on
// stdout. Execute must not buffer the whole thing: the captured output is
// bounded and carries a truncation marker.
//
// Note: not parallel for the same ETXTBSY reason as TestExecute_MergesStdoutAndStderr
// above — this test writes a script and immediately execs it.
func TestExecute_TruncatesOversizedOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o750); err != nil {
		t.Fatal(err)
	}

	// Write a script that emits well over the cap (10MB) in 1MB chunks so the
	// test doesn't depend on any single write() being larger than the cap.
	scriptPath := filepath.Join(dir, "compose-chatty.sh")
	script := "#!/usr/bin/env sh\n" +
		"i=0\n" +
		"chunk=$(printf 'x%.0s' $(seq 1 1000000))\n" +
		"while [ $i -lt 12 ]; do\n" +
		"  printf '%s' \"$chunk\"\n" +
		"  i=$((i + 1))\n" +
		"done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cm := &ComposeManager{stacksDir: dir, composeBin: scriptPath, isV2: false}

	resp, err := cm.Execute(t.Context(), ComposeRequest{StackName: "app", Operation: "up"})
	if err != nil {
		t.Fatalf("Execute: unexpected error %v", err)
	}
	if !resp.Success {
		t.Fatalf("Execute: expected Success=true, got Error=%q", resp.Error)
	}
	// Captured output must be bounded well below the ~12MB the script wrote,
	// and carry a marker so operators know it was cut short.
	if len(resp.Output) > maxComposeOutputBytes+1024 {
		t.Fatalf("Execute: output not bounded, got %d bytes", len(resp.Output))
	}
	if !strings.Contains(resp.Output, "truncated") {
		t.Fatalf("Execute: expected a truncation marker in output, got %d bytes with no marker", len(resp.Output))
	}
	// The marker names the exact limit in MB (maxComposeOutputBytes/(1024*1024)
	// == 10), not just any number, so an arithmetic change to that
	// computation is caught rather than just the presence of "truncated".
	wantMarker := "exceeded 10 MB combined output limit]"
	if !strings.Contains(resp.Output, wantMarker) {
		t.Fatalf("Execute: expected marker %q in output, got %q (last 200 bytes)", wantMarker, resp.Output[len(resp.Output)-200:])
	}
}

// TestMaxComposeOutputBytesEqualsTenMiB pins maxComposeOutputBytes's value,
// same reasoning as the defaultDialTimeout/maxDockerErrorBodyBytes const
// checks in client_unix_test.go.
func TestMaxComposeOutputBytesEqualsTenMiB(t *testing.T) {
	t.Parallel()

	if maxComposeOutputBytes != 10*1024*1024 {
		t.Fatalf("maxComposeOutputBytes = %d, want %d", maxComposeOutputBytes, 10*1024*1024)
	}
}

// ---- Execute: concurrent operations on the same stack serialize ----

func TestComposeManagerLockStackRemovesUnusedEntries(t *testing.T) {
	t.Parallel()
	cm := &ComposeManager{}

	for i := 0; i < 1000; i++ {
		unlock := cm.lockStack(fmt.Sprintf("stack-%d", i))
		unlock()
	}

	cm.stackLocksMu.Lock()
	defer cm.stackLocksMu.Unlock()
	if got := len(cm.stackLocks); got != 0 {
		t.Fatalf("retained stack locks = %d, want 0", got)
	}
}

func TestComposeManagerLockStackWaiterPreventsEarlyRemoval(t *testing.T) {
	t.Parallel()
	cm := &ComposeManager{}
	unlockFirst := cm.lockStack("app")
	secondAcquired := make(chan func(), 1)
	go func() {
		secondAcquired <- cm.lockStack("app")
	}()

	deadline := time.Now().Add(time.Second)
	for {
		cm.stackLocksMu.Lock()
		refs := cm.stackLocks["app"].refs
		cm.stackLocksMu.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			unlockFirst()
			t.Fatal("timed out waiting for second lock reference")
		}
		runtime.Gosched()
	}
	unlockFirst()

	unlockSecond := <-secondAcquired
	cm.stackLocksMu.Lock()
	if got := len(cm.stackLocks); got != 1 {
		cm.stackLocksMu.Unlock()
		t.Fatalf("stack locks while waiter owns lock = %d, want 1", got)
	}
	cm.stackLocksMu.Unlock()
	unlockSecond()

	cm.stackLocksMu.Lock()
	defer cm.stackLocksMu.Unlock()
	if got := len(cm.stackLocks); got != 0 {
		t.Fatalf("retained stack locks after final release = %d, want 0", got)
	}
}

// TestComposeManagerExecute_ConcurrentSameStackSerializes exercises finding
// C4: without a per-stack lock, two concurrent "up" requests for the same
// StackName can interleave — request B's writeStackFiles can overwrite
// request A's compose file before A's "docker compose" process reads it, so
// A ends up deploying B's configuration while reporting its own success. This
// drives two concurrent Execute calls for the same stack with distinct file
// contents through a fake compose binary that sleeps briefly before reading
// the compose file, widening the write/exec race window so the bug would
// show up reliably if the two operations weren't serialized. Each Execute
// call must observe only the content it wrote itself.
//
// Note: not parallel, for the same ETXTBSY reason documented on
// TestExecute_MergesStdoutAndStderr above — it writes and immediately execs
// a script.
func TestComposeManagerExecute_ConcurrentSameStackSerializes(t *testing.T) {
	dir := t.TempDir()

	// Fake compose binary: sleep to widen the write/exec race window, then
	// print whatever docker-compose.yml currently holds in the project
	// directory (buildCommand sets cmd.Dir to it).
	scriptPath := filepath.Join(dir, "compose-lock-test.sh")
	script := "#!/usr/bin/env sh\nsleep 0.1\ncat docker-compose.yml\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cm := &ComposeManager{stacksDir: dir, composeBin: scriptPath, isV2: false}

	const runs = 5
	contents := []string{"content-A", "content-B"}
	for i := 0; i < runs; i++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make([]*ComposeResponse, len(contents))
		errs := make([]error, len(contents))

		for idx, content := range contents {
			wg.Add(1)
			go func(idx int, content string) {
				defer wg.Done()
				<-start
				resp, err := cm.Execute(t.Context(), ComposeRequest{
					StackName: "app",
					Operation: "up",
					Files: map[string]string{
						"docker-compose.yml": content,
					},
				})
				results[idx] = resp
				errs[idx] = err
			}(idx, content)
		}

		close(start)
		wg.Wait()

		for idx, resp := range results {
			if errs[idx] != nil {
				t.Fatalf("run %d: Execute: unexpected error %v", i, errs[idx])
			}
			if !resp.Success {
				t.Fatalf("run %d: Execute: expected Success=true, got Error=%q", i, resp.Error)
			}
			if got, want := strings.TrimSpace(resp.Output), contents[idx]; got != want {
				t.Fatalf("run %d: Execute observed %q, want its own content %q (cross-contamination from a concurrent write to the same stack)", i, got, want)
			}
		}
	}
}

// ---- Execute: registryLogin failure path ----

func TestExecute_RegistryLoginFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	cm := &ComposeManager{stacksDir: dir, composeBin: "docker", isV2: true}

	req := ComposeRequest{
		StackName: "app",
		Operation: "up",
		RegistryAuth: &RegistryAuth{
			Server:   "https://registry.example.com",
			Username: "user",
			Password: "wrongpassword",
		},
	}
	// docker login will fail (no such registry), so Execute should return Success=false.
	resp, err := cm.Execute(t.Context(), req)
	if err != nil {
		t.Fatalf("Execute: unexpected error %v", err)
	}
	if resp.Success {
		// If this machine actually has docker and it somehow succeeds, skip.
		t.Skip("docker login unexpectedly succeeded; skipping")
	}
	if resp.Error == "" {
		t.Fatal("Execute: expected non-empty Error when registry login fails")
	}
}

// ---- registryLogin: success path ----

// TestRegistryLogin_Success exercises the happy path of registryLogin by using
// a fake "docker" binary that always exits 0.
// Note: not parallel because it mutates os.Setenv("PATH").
func TestRegistryLogin_Success(t *testing.T) {
	// Create a fake docker binary that exits 0.
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "docker")
	script := "#!/usr/bin/env sh\nexit 0\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPath) })                 //nolint:errcheck
	os.Setenv("PATH", binDir+string(filepath.ListSeparator)+origPath) //nolint:errcheck

	cm := &ComposeManager{
		stacksDir:  t.TempDir(),
		apiVersion: "v1.44",
	}

	auth := &RegistryAuth{
		Server:   "https://registry.example.com",
		Username: "user",
		Password: "pass",
	}

	if err := cm.registryLogin(t.Context(), auth); err != nil {
		t.Fatalf("registryLogin: unexpected error %v", err)
	}
}

func TestExecuteRegistryLoginPreservesBareHostname(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o750); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	callsPath := filepath.Join(binDir, "calls")
	fakeDocker := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PORTWING_TEST_CALLS\"\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTWING_TEST_CALLS", callsPath)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	cm := &ComposeManager{stacksDir: dir, composeBin: fakeDocker}
	resp, err := cm.Execute(t.Context(), ComposeRequest{
		StackName: "app",
		Operation: "up",
		RegistryAuth: &RegistryAuth{
			Server:   "registry.example.com:5443",
			Username: "alice",
			Password: "secret",
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Execute rejected a documented bare registry hostname: %s", resp.Error)
	}

	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatalf("read Docker invocation record: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) < 1 {
		t.Fatal("Docker invocation record is empty")
	}
	const wantLogin = "login --username alice --password-stdin registry.example.com:5443"
	if lines[0] != wantLogin {
		t.Fatalf("Docker login invocation = %q, want %q", lines[0], wantLogin)
	}
}

// ---- validateRequest: file path traversal ----

func TestValidateRequest_FilePathTraversal(t *testing.T) {
	t.Parallel()

	cm := &ComposeManager{stacksDir: t.TempDir()}

	req := ComposeRequest{
		StackName: "app",
		Files: map[string]string{
			"../evil/compose.yml": "services: {}\n",
		},
	}
	if err := cm.validateRequest(req); err == nil {
		t.Fatal("expected error for file path traversal, got nil")
	}
}

// ---- validateRequest: stack path escapes stacks dir ----

func TestValidateRequest_StackPathTraversal(t *testing.T) {
	t.Parallel()

	cm := &ComposeManager{stacksDir: t.TempDir()}

	req := ComposeRequest{
		StackName: "../outside",
	}
	if err := cm.validateRequest(req); err == nil {
		t.Fatal("expected error for stack path traversal, got nil")
	}
}

// ---- writeStackFiles: resolve path error for a file ----

func TestWriteStackFiles_ResolvePathErrorForFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cm := &ComposeManager{stacksDir: dir}

	req := ComposeRequest{
		StackName: "app",
		Files: map[string]string{
			"../../escape.yml": "services: {}\n",
		},
	}
	if err := cm.writeStackFiles(req); err == nil {
		t.Fatal("expected error when file path escapes stack dir, got nil")
	}
}

// ---- writeStackFiles: WriteFile failure (target is a directory) ----

func TestWriteStackFiles_WriteFileFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cm := &ComposeManager{stacksDir: dir}

	// Create target as a directory so WriteFile fails.
	targetDir := filepath.Join(dir, "app", "compose.yml")
	if err := os.MkdirAll(targetDir, 0o750); err != nil {
		t.Fatal(err)
	}

	req := ComposeRequest{
		StackName: "app",
		Files: map[string]string{
			"compose.yml": "services: {}\n",
		},
	}
	if err := cm.writeStackFiles(req); err == nil {
		t.Fatal("expected error when WriteFile target is a directory, got nil")
	}
}

// ---- writeStackFiles: .env.drydock write failure ----

func TestWriteStackFiles_EnvFileDrydockWriteFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cm := &ComposeManager{stacksDir: dir}

	// Create .env.drydock as a directory so WriteFile on it fails.
	envFileAsDir := filepath.Join(dir, "app", ".env.drydock")
	if err := os.MkdirAll(envFileAsDir, 0o750); err != nil {
		t.Fatal(err)
	}

	req := ComposeRequest{
		StackName: "app",
		EnvVars: map[string]string{
			"MY_VAR": "value",
		},
		Files: map[string]string{}, // non-nil so writeStackFiles is reached via Execute's Files != nil branch
	}

	// Call writeStackFiles directly.
	if err := cm.writeStackFiles(req); err == nil {
		t.Fatal("expected error when .env.drydock target is a directory, got nil")
	}
}
