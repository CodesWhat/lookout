package docker

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteStackFilesUsesPrivatePermissions(t *testing.T) {
	t.Parallel()

	stacksDir := t.TempDir()
	cm := &ComposeManager{stacksDir: stacksDir}

	req := ComposeRequest{
		StackName: "app",
		EnvVars: map[string]string{
			"PORTWING_ENV": "production",
		},
		Files: map[string]string{
			"docker-compose.yml": "services: {}\n",
			"nested/config.yml":  "x: y\n",
		},
	}

	if err := cm.writeStackFiles(req); err != nil {
		t.Fatalf("writeStackFiles: %v", err)
	}

	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on windows")
	}

	assertMode(t, filepath.Join(stacksDir, "app"), 0o750)
	assertMode(t, filepath.Join(stacksDir, "app", "nested"), 0o750)
	assertMode(t, filepath.Join(stacksDir, "app", "docker-compose.yml"), 0o600)
	assertMode(t, filepath.Join(stacksDir, "app", "nested", "config.yml"), 0o600)
	assertMode(t, filepath.Join(stacksDir, "app", ".env.drydock"), 0o600)
}

func TestResolvePathRejectsStackFileTraversal(t *testing.T) {
	t.Parallel()

	stacksDir := t.TempDir()
	cm := &ComposeManager{stacksDir: stacksDir}

	if _, err := cm.resolvePath("app", "nested/config.yml"); err != nil {
		t.Fatalf("resolvePath valid nested file: %v", err)
	}

	cases := map[string]struct {
		stackDir string
		path     string
	}{
		"absolute stack dir":    {filepath.Join(stacksDir, "app"), "compose.yml"},
		"absolute file path":    {"app", filepath.Join(stacksDir, "app", "compose.yml")},
		"stack dir traversal":   {"../outside", "compose.yml"},
		"cross stack traversal": {"app", "../other/compose.yml"},
		"cleaned traversal":     {"app", "nested/../../other/compose.yml"},
	}

	for name, c := range cases {
		if _, err := cm.resolvePath(c.stackDir, c.path); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestWriteStackFilesRejectsSymlinkedStackRootEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires Windows privileges")
	}
	stacksDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(stacksDir, "app")); err != nil {
		t.Fatal(err)
	}
	cm := &ComposeManager{stacksDir: stacksDir}
	err := cm.writeStackFiles(ComposeRequest{
		StackName: "app",
		Files:     map[string]string{"compose.yml": "attacker-controlled"},
	})
	if err == nil {
		t.Fatal("expected symlinked stack root to be rejected")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "compose.yml")); !os.IsNotExist(statErr) {
		t.Fatalf("write escaped STACKS_DIR through symlink: %v", statErr)
	}
}

func TestWriteStackFilesRejectsSymlinkedNestedFileEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires Windows privileges")
	}
	stacksDir := t.TempDir()
	stackDir := filepath.Join(stacksDir, "app")
	if err := os.Mkdir(stackDir, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "protected")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stackDir, "compose.yml")); err != nil {
		t.Fatal(err)
	}
	cm := &ComposeManager{stacksDir: stacksDir}
	err := cm.writeStackFiles(ComposeRequest{
		StackName: "app",
		Files:     map[string]string{"compose.yml": "overwritten"},
	})
	if err == nil {
		t.Fatal("expected symlinked nested file to be rejected")
	}
	got, readErr := os.ReadFile(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "original" {
		t.Fatalf("outside file was modified: %q", got)
	}
}

// TestWriteStackFiles_NoEnvVars_NoEnvFileWritten verifies the exact boundary
// of the EnvVars-present check: an empty (nil) EnvVars map must NOT produce
// a .env.drydock file. The nonempty case is already covered by
// TestWriteStackFilesUsesPrivatePermissions above.
func TestWriteStackFiles_NoEnvVars_NoEnvFileWritten(t *testing.T) {
	t.Parallel()

	stacksDir := t.TempDir()
	cm := &ComposeManager{stacksDir: stacksDir}

	req := ComposeRequest{
		StackName: "app",
		Files: map[string]string{
			"docker-compose.yml": "services: {}\n",
		},
	}

	if err := cm.writeStackFiles(req); err != nil {
		t.Fatalf("writeStackFiles: %v", err)
	}

	if _, err := os.Stat(filepath.Join(stacksDir, "app", ".env.drydock")); !os.IsNotExist(err) {
		t.Fatalf("expected no .env.drydock file for empty EnvVars, stat err = %v", err)
	}
}

// ---- composeLogsTail ----

func TestComposeLogsTail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requested int
		want      int
	}{
		{name: "zero defaults", requested: 0, want: defaultComposeLogsTail},
		{name: "negative defaults", requested: -5, want: defaultComposeLogsTail},
		{name: "within range is passed through", requested: 250, want: 250},
		{name: "exactly at cap is passed through", requested: maxComposeLogsTail, want: maxComposeLogsTail},
		{name: "one over cap is clamped", requested: maxComposeLogsTail + 1, want: maxComposeLogsTail},
		{name: "well over cap is clamped", requested: 10000, want: maxComposeLogsTail},
		{name: "exactly one is passed through", requested: 1, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := composeLogsTail(tt.requested); got != tt.want {
				t.Errorf("composeLogsTail(%d) = %d, want %d", tt.requested, got, tt.want)
			}
		})
	}
}

// ---- outputLimiter / boundedWriter ----

func TestOutputLimiter_ReserveWithinBudget(t *testing.T) {
	t.Parallel()

	l := newOutputLimiter(10)
	if got := l.reserve(4); got != 4 {
		t.Fatalf("reserve(4) = %d, want 4", got)
	}
	if l.isTruncated() {
		t.Fatal("expected not truncated after reserving within budget")
	}
	if l.remaining != 6 {
		t.Fatalf("remaining = %d, want 6", l.remaining)
	}
}

// TestOutputLimiter_ReserveExactlyAtBudget exercises the exact boundary of
// the "n > remaining" check: requesting exactly the remaining budget must be
// granted in full and must NOT flip truncated.
func TestOutputLimiter_ReserveExactlyAtBudget(t *testing.T) {
	t.Parallel()

	l := newOutputLimiter(10)
	if got := l.reserve(10); got != 10 {
		t.Fatalf("reserve(10) = %d, want 10", got)
	}
	if l.isTruncated() {
		t.Fatal("expected not truncated when a reservation exactly exhausts the budget")
	}
	if l.remaining != 0 {
		t.Fatalf("remaining = %d, want 0", l.remaining)
	}
}

// TestOutputLimiter_ReserveOverBudget exercises the other side of the same
// boundary: requesting one more than the remaining budget must be capped and
// must flip truncated.
func TestOutputLimiter_ReserveOverBudget(t *testing.T) {
	t.Parallel()

	l := newOutputLimiter(10)
	if got := l.reserve(11); got != 10 {
		t.Fatalf("reserve(11) = %d, want 10 (capped)", got)
	}
	if !l.isTruncated() {
		t.Fatal("expected truncated after reserving over budget")
	}
	if l.remaining != 0 {
		t.Fatalf("remaining = %d, want 0", l.remaining)
	}
}

// TestBoundedWriter_WriteExhaustedBudgetDropsBytes exercises the boundary of
// boundedWriter.Write's "n > 0" check: once the shared limiter's budget is
// exhausted (reserve returns exactly 0), Write must report success and the
// full length written to the caller (so os/exec's pipe-copy goroutine keeps
// draining) but must not buffer anything.
func TestBoundedWriter_WriteExhaustedBudgetDropsBytes(t *testing.T) {
	t.Parallel()

	l := newOutputLimiter(0)
	w := &boundedWriter{limiter: l}

	n, err := w.Write([]byte("dropped"))
	if err != nil {
		t.Fatalf("Write: unexpected error %v", err)
	}
	if n != len("dropped") {
		t.Fatalf("Write returned n = %d, want %d", n, len("dropped"))
	}
	if w.buf.Len() != 0 {
		t.Fatalf("buf.Len() = %d, want 0 (nothing should be buffered once budget is exhausted)", w.buf.Len())
	}
}

// TestBoundedWriter_WriteWithinBudgetBuffers is the positive counterpart to
// TestBoundedWriter_WriteExhaustedBudgetDropsBytes: bytes within budget must
// actually be buffered.
func TestBoundedWriter_WriteWithinBudgetBuffers(t *testing.T) {
	t.Parallel()

	l := newOutputLimiter(100)
	w := &boundedWriter{limiter: l}

	n, err := w.Write([]byte("kept"))
	if err != nil {
		t.Fatalf("Write: unexpected error %v", err)
	}
	if n != len("kept") {
		t.Fatalf("Write returned n = %d, want %d", n, len("kept"))
	}
	if w.buf.String() != "kept" {
		t.Fatalf("buf = %q, want %q", w.buf.String(), "kept")
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %#o, want %#o", path, got, want)
	}
}
