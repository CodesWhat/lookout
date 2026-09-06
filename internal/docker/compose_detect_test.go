package docker

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetectCompose_V2Detection exercises the branch where `docker compose version`
// outputs a string containing "v2", causing detectCompose to choose Compose v2.
// Note: not parallel because it mutates os.Getenv("PATH").
func TestDetectCompose_V2Detection(t *testing.T) {

	// Create a fake "docker" binary in a temp dir that prints "Docker Compose v2.x"
	// to stdout when called with "compose version".
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "docker")

	script := "#!/bin/sh\necho 'Docker Compose version v2.99.0'\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}

	// Put our fake dir at the front of PATH so exec.Command("docker",...) finds it.
	origPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPath) })                 //nolint:errcheck
	os.Setenv("PATH", binDir+string(filepath.ListSeparator)+origPath) //nolint:errcheck

	cm := &ComposeManager{stacksDir: t.TempDir()}
	cm.detectCompose()

	if cm.composeBin != "docker" {
		t.Fatalf("composeBin = %q, want %q", cm.composeBin, "docker")
	}
	if !cm.isV2 {
		t.Fatal("isV2 = false, want true")
	}
}

// TestDetectCompose_DefaultFallback exercises the fallback where `docker compose version`
// fails (or produces no v2 output) AND `docker-compose` is not in PATH.
// Note: not parallel because it mutates os.Getenv("PATH").
func TestDetectCompose_DefaultFallback(t *testing.T) {

	// Create a temp dir with a "docker" binary that always exits 1 (simulate failure).
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}

	// Use ONLY our fake binDir in PATH — no docker-compose available.
	origPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPath) }) //nolint:errcheck
	os.Setenv("PATH", binDir)                         //nolint:errcheck

	cm := &ComposeManager{stacksDir: t.TempDir()}
	cm.detectCompose()

	// Should fall through to default: docker, v2.
	if cm.composeBin != "docker" {
		t.Fatalf("composeBin = %q, want %q", cm.composeBin, "docker")
	}
	if !cm.isV2 {
		t.Fatal("isV2 = false, want true for default fallback")
	}
}

// TestDetectCompose_V1Fallback exercises the branch where docker-compose v1 is found.
// Note: not parallel because it mutates os.Getenv("PATH").
func TestDetectCompose_V1Fallback(t *testing.T) {

	// Create a temp dir with:
	// - a "docker" binary that outputs something without "v2"
	// - a "docker-compose" binary (any executable)
	binDir := t.TempDir()

	fakeBin := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\necho 'Docker Compose version 1.29.0'\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}

	fakeV1 := filepath.Join(binDir, "docker-compose")
	if err := os.WriteFile(fakeV1, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing fake docker-compose: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPath) }) //nolint:errcheck
	os.Setenv("PATH", binDir)                         //nolint:errcheck

	cm := &ComposeManager{stacksDir: t.TempDir()}
	cm.detectCompose()

	if cm.composeBin != "docker-compose" {
		t.Fatalf("composeBin = %q, want %q", cm.composeBin, "docker-compose")
	}
	if cm.isV2 {
		t.Fatal("isV2 = true, want false for v1")
	}
}

// TestDetectCompose_V2DetectionPrefersV2OverV1Fallback exercises the
// negation boundary of the `err == nil` check that guards the v2-detection
// branch: `docker compose version` succeeds AND reports v2, AND a
// docker-compose v1 binary is also present in PATH. detectCompose must
// return early on the v2 match rather than falling through to the v1
// fallback below. TestDetectCompose_V2Detection above doesn't have a
// docker-compose binary in PATH at all, so it can't tell an early return
// from an accidental fallthrough that happens to land on the same result.
// Note: not parallel because it mutates os.Getenv("PATH").
func TestDetectCompose_V2DetectionPrefersV2OverV1Fallback(t *testing.T) {

	binDir := t.TempDir()

	fakeBin := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\necho 'Docker Compose version v2.99.0'\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}

	// A docker-compose v1 binary is ALSO present: if detectCompose fails to
	// return early on the v2 match, it will fall through and pick this up
	// instead, giving a different (wrong) result.
	fakeV1 := filepath.Join(binDir, "docker-compose")
	if err := os.WriteFile(fakeV1, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing fake docker-compose: %v", err)
	}

	origPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPath) }) //nolint:errcheck
	os.Setenv("PATH", binDir)                         //nolint:errcheck

	cm := &ComposeManager{stacksDir: t.TempDir()}
	cm.detectCompose()

	if cm.composeBin != "docker" {
		t.Fatalf("composeBin = %q, want %q (must prefer v2 detection over v1 fallback)", cm.composeBin, "docker")
	}
	if !cm.isV2 {
		t.Fatal("isV2 = false, want true (must prefer v2 detection over v1 fallback)")
	}
}
