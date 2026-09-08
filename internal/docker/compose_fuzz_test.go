package docker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestComposeManager creates a ComposeManager with a temp stacksDir for
// validation tests. It mirrors the approach in compose_test.go (direct struct
// literal) — the compose binary detection is irrelevant for validateRequest.
//
// It lives in the fuzz file rather than compose_validate_test.go because
// ClusterFuzzLite builds this target with go-118-fuzz-build, which lifts only
// the single _test.go file holding the Fuzz function into a normal package
// build. A helper in a sibling _test.go file is undefined there, so the target
// has to be self-contained. Ordinary tests are unaffected: same package, so
// compose_validate_test.go still sees it.
func newTestComposeManager(t *testing.T) *ComposeManager {
	t.Helper()
	return &ComposeManager{stacksDir: t.TempDir()}
}

// The fuzz target's stacks directory sits one level below a sentinel parent so
// that an escape has an observable place to land:
//
//	parent/
//	  stacks/           <- ComposeManager.stacksDir
//	    escape -> ../outside
//	  outside/          <- must stay empty, forever
//
// A lexical escape (resolvePath or rootedPath letting "../.." through) lands
// beside stacks/; an escape through a filesystem symlink (mkdirRootedNoSymlinks
// or os.Root letting stacks/escape be traversed) lands inside outside/. Both
// are visible from parent, which is why the oracle walks parent. Walking
// stacks/ instead — what this target used to do — cannot fail: filepath.WalkDir
// rooted at stacks/ only ever yields lexically in-root paths and does not
// follow directory symlinks, so it reports an in-root path for a file that
// physically landed in outside/.
const (
	fuzzStacksDirName  = "stacks"
	fuzzOutsideDirName = "outside"
	fuzzEscapeLinkName = "escape"
)

// plantEscapeTree builds (or rebuilds, after a reset) the sentinel tree under
// parent and reports whether the symlink trap was planted. Windows needs
// privileges for os.Symlink, so the trap is skipped there the same way
// compose_test.go's symlink cases are; the lexical half of the oracle still
// runs.
//
// It returns an error rather than taking a testing.TB and calling Fatalf,
// because this file is also compiled outside the test binary: the
// ClusterFuzzLite tier builds it with go-118-fuzz-build, whose drop-in testing
// package defines T and F but no TB interface, and whose F has no Fatalf.
func plantEscapeTree(parent string) (bool, error) {
	if err := os.MkdirAll(filepath.Join(parent, fuzzStacksDirName), 0o750); err != nil {
		return false, fmt.Errorf("creating stacks directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(parent, fuzzOutsideDirName), 0o750); err != nil {
		return false, fmt.Errorf("creating sentinel directory: %w", err)
	}
	if runtime.GOOS == "windows" {
		return false, nil
	}
	link := filepath.Join(parent, fuzzStacksDirName, fuzzEscapeLinkName)
	if err := os.Symlink(filepath.Join(parent, fuzzOutsideDirName), link); err != nil {
		return false, fmt.Errorf("planting escape symlink %q: %w", link, err)
	}
	return true, nil
}

// assertNoEscape is the fuzz target's real containment oracle: nothing may
// exist inside parent/outside, and nothing may exist directly under parent
// except the two directories the harness created. It also checks the trap is
// still a symlink, because a run in which the trap had been replaced by a real
// directory would report success while no longer being able to observe an
// escape through it.
func assertNoEscape(t *testing.T, parent string, trapPlanted bool) {
	t.Helper()

	outsideDir := filepath.Join(parent, fuzzOutsideDirName)
	var escaped string
	walkErr := filepath.WalkDir(outsideDir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != outsideDir && escaped == "" {
			escaped = path
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking sentinel directory %q: %v", outsideDir, walkErr)
	}
	if escaped != "" {
		t.Fatalf("writeStackFiles wrote %q outside the stacks directory", escaped)
	}

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("reading sentinel parent %q: %v", parent, err)
	}
	for _, entry := range entries {
		if entry.Name() != fuzzStacksDirName && entry.Name() != fuzzOutsideDirName {
			t.Fatalf("writeStackFiles created %q beside the stacks directory", filepath.Join(parent, entry.Name()))
		}
	}

	if !trapPlanted {
		return
	}
	link := filepath.Join(parent, fuzzStacksDirName, fuzzEscapeLinkName)
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("escape trap %q is gone, the symlink oracle is no longer live: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("escape trap %q is no longer a symlink (mode %v), the symlink oracle is no longer live", link, info.Mode())
	}
}

// isWriteStackFilesRejection reports whether err is a rejection writeStackFiles
// can legitimately return for an input validateRequest accepted. Those are:
//
//   - base64.CorruptInputError, since validateRequest never looks at content;
//   - any *fs.PathError, which covers every kernel-level refusal a fuzzer can
//     provoke with a hostile path (ENAMETOOLONG, EINVAL from a NUL byte,
//     EISDIR, ENOTDIR) as well as environmental failures like ENOSPC, none of
//     which are bugs in this package;
//   - the package's own deliberate refusals of a symlinked or non-regular
//     path component, which are exactly what the escape seeds below provoke;
//   - "is not a file below stacks directory", reachable and harmless when a
//     request resolves the stack root itself as a file, e.g. stackName "." with
//     a "." file key.
//
// What is left over is the interesting class: writeStackFiles rejecting a path
// as absolute or as escaping the stack directory, after validateRequest ran the
// same resolvePath over the same input and accepted it. That is a contradiction
// between the two, so the target fails on it.
func isWriteStackFilesRejection(err error) bool {
	var corrupt base64.CorruptInputError
	if errors.As(err, &corrupt) {
		return true
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return true
	}
	for _, refusal := range []string{
		"refusing symlink directory",
		"refusing symlink file",
		"refusing non-regular file",
		" is not a directory",
		"is not a file below stacks directory",
	} {
		if strings.Contains(err.Error(), refusal) {
			return true
		}
	}
	return false
}

// FuzzComposeRequestValidate feeds fuzzed JSON through the same decode ->
// validateRequest -> writeStackFiles pipeline that internal/server/http.go's
// handleCompose and internal/edge/client.go's handleComposeRequestTo run on
// an authenticated caller's request body. ComposeRequest is the largest,
// most security-sensitive JSON struct in the repo — validateRequest and
// writeStackFiles carry the path-traversal defense and credential handling
// that make StackDir/Files/RegistryAuth safe to act on — so the properties
// under test are: never panic; never let a request validateRequest accepted
// carry an empty stack name; never let writeStackFiles fail for a reason that
// contradicts validateRequest; and never let writeStackFiles put a file
// outside the manager's stacksDir, by any route including a symlinked path
// component.
func FuzzComposeRequestValidate(f *testing.F) {
	parent := f.TempDir()
	trapPlanted, err := plantEscapeTree(parent)
	if err != nil {
		// The seeding scope has no Fatalf under the ClusterFuzzLite testing
		// shim, and a harness that could not plant its trap must not fuzz on
		// anyway: it would report success while blind to a symlink escape.
		panic(err)
	}
	stacksDir := filepath.Join(parent, fuzzStacksDirName)
	cm := &ComposeManager{stacksDir: stacksDir}
	// stacksDir is shared by every iteration in this worker process, so
	// without a bound a long -fuzztime budget grows the tree without limit.
	filesWritten := 0

	// Seed: valid JSON, minimal.
	f.Add([]byte(`{"stackName":"myapp"}`))
	// Seed: valid JSON, full shape.
	f.Add([]byte(`{"operation":"up","stackName":"myapp","stackDir":"myapp","services":["web","db"],"build":true,"envVars":{"FOO":"bar"},"files":{"docker-compose.yml":"version: \"3\""},"registryAuth":{"server":"https://registry.example.com","username":"u","password":"p"}}`))
	// Seed: empty object — missing required stackName.
	f.Add([]byte(`{}`))
	// Seed: unknown fields.
	f.Add([]byte(`{"stackName":"myapp","unknownField":"value","nested":{"a":1}}`))
	// Seed: path traversal in stackDir.
	f.Add([]byte(`{"stackName":"myapp","stackDir":"../../etc"}`))
	// Seed: path traversal in a file name.
	f.Add([]byte(`{"stackName":"myapp","files":{"../../etc/passwd":"pwned"}}`))
	// Seed: absolute path in a file name.
	f.Add([]byte(`{"stackName":"myapp","files":{"/etc/passwd":"pwned"}}`))
	// Seed: oversized string.
	f.Add([]byte(`{"stackName":"` + strings.Repeat("a", 10000) + `"}`))
	// Seed: wrong types (tail as string, files as an array instead of a map).
	f.Add([]byte(`{"stackName":"myapp","tail":"not-a-number"}`))
	f.Add([]byte(`{"stackName":"myapp","files":["not","an","object"]}`))
	// Seed: malformed base64 file content.
	f.Add([]byte(`{"stackName":"myapp","files":{"a.txt":"base64:not-valid-base64!!"}}`))
	// Seed: env var keys crafted to just miss envVarKeyPattern/envVarDenylist.
	f.Add([]byte(`{"stackName":"myapp","envVars":{"PATH ":"x","1BAD":"y","ld_preload":"z"}}`))
	// Seed: registryAuth.server with userinfo and an unusual port.
	f.Add([]byte(`{"stackName":"myapp","registryAuth":{"server":"https://user:pass@registry.example.com:99999","username":"u","password":"p"}}`))
	// Seed: not JSON at all.
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Add([]byte(`null`))

	// Escape seeds. Each drives a write through the "escape" symlink planted
	// in stacksDir, which is the only way the symlink containment in
	// mkdirRootedNoSymlinks, writeRootedFile and os.Root gets exercised —
	// lexical traversal seeds never create a symlinked filesystem component.
	// Seed: the symlink as the stack root, taken from stackName.
	f.Add([]byte(`{"stackName":"escape","files":{"pwned.txt":"pwned"}}`))
	// Seed: the symlink as the stack root, taken from stackDir.
	f.Add([]byte(`{"stackName":"myapp","stackDir":"escape","files":{"pwned.txt":"pwned"}}`))
	// Seed: the symlink as an interior component of a file key.
	f.Add([]byte(`{"stackName":"myapp","stackDir":".","files":{"escape/pwned.txt":"pwned"}}`))
	// Seed: the symlink as the final component of a file key.
	f.Add([]byte(`{"stackName":"myapp","stackDir":".","files":{"escape":"pwned"}}`))
	// Seed: the symlink below the stack root, reached by .env.drydock.
	f.Add([]byte(`{"stackName":"escape","envVars":{"FOO":"bar"}}`))
	// Seed: stack root resolving to stacksDir itself, the one path where
	// writeStackFiles legitimately rejects what validateRequest accepted.
	f.Add([]byte(`{"stackName":".","files":{".":"x"}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var req ComposeRequest
		if err := json.Unmarshal(data, &req); err != nil {
			// Malformed JSON must produce an error, never a panic, and never
			// a usable ComposeRequest — nothing further to exercise.
			return
		}

		if _, err := cm.validateRequest(req); err != nil {
			// Rejected requests must never reach writeStackFiles.
			return
		}

		// validateRequest accepted the request, so it is not the zero value:
		// stackName is the one field it requires, and everything downstream
		// (the stack directory, the lock, the compose project name) is
		// derived from it.
		if req.StackName == "" {
			t.Fatalf("validateRequest accepted a request with an empty stack name: %q", data)
		}

		if err := cm.writeStackFiles(req); err != nil && !isWriteStackFilesRejection(err) {
			t.Fatalf("writeStackFiles returned an unexpected error for a validated request %q: %v", data, err)
		}

		assertNoEscape(t, parent, trapPlanted)

		filesWritten += len(req.Files) + 1
		if filesWritten > 512 {
			if err := os.RemoveAll(stacksDir); err != nil {
				t.Fatalf("resetting stacksDir: %v", err)
			}
			if _, err := plantEscapeTree(parent); err != nil {
				t.Fatalf("replanting the escape tree: %v", err)
			}
			filesWritten = 0
		}
	})
}

// TestComposeRequestValidateRejectsKnownBad pins the rejections the fuzz target
// depends on but cannot assert, since a fuzz target only sees inputs the
// engine generates. It runs the same decode -> validateRequest pipeline, so it
// also covers the JSON layer that TestValidateRequest in compose_validate_test.go
// skips by constructing ComposeRequest values directly.
func TestComposeRequestValidateRejectsKnownBad(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"empty object", `{}`},
		{"json null", `null`},
		{"empty stack name", `{"stackName":""}`},
		{"traversal in stack name", `{"stackName":"../escapee"}`},
		{"traversal in stack dir", `{"stackName":"myapp","stackDir":"../../etc"}`},
		{"traversal in a file key", `{"stackName":"myapp","files":{"../../etc/passwd":"pwned"}}`},
		{"cleaned traversal in a file key", `{"stackName":"myapp","files":{"nested/../../escapee":"pwned"}}`},
		{"absolute stack dir", `{"stackName":"myapp","stackDir":"/etc"}`},
		{"absolute file key", `{"stackName":"myapp","files":{"/etc/passwd":"pwned"}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var req ComposeRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("decoding %s: %v", tc.body, err)
			}
			cm := newTestComposeManager(t)
			if _, err := cm.validateRequest(req); err == nil {
				t.Fatalf("validateRequest accepted %s", tc.body)
			}
		})
	}
}
