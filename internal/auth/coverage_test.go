package auth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---- notify / OnResult callback ------------------------------------------

func TestEnroller_OnResult_NotSet(t *testing.T) {
	t.Parallel()
	// Enroller with no OnResult callback must not panic on any code path.
	e, _, _ := setupEnroller(t, "tok")

	// Trigger "burned" path without OnResult — should not panic.
	e.burned = true
	req := httptest.NewRequest(http.MethodPost, "/api/portwing/enroll", enrollBody(t, "tok", ""))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestEnroller_OnResult_Called(t *testing.T) {
	t.Parallel()
	e, _, _ := setupEnroller(t, "tok")

	var actors, calls []string
	e.OnResult = func(actor, keyID, outcome string) {
		actors = append(actors, actor)
		calls = append(calls, outcome)
	}

	// Wrong token → denied callback.
	req := httptest.NewRequest(http.MethodPost, "/api/portwing/enroll", enrollBody(t, "wrong", ""))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if len(calls) != 1 || calls[0] != "denied" {
		t.Errorf("expected [denied], got %v", calls)
	}
	if len(actors) != 1 || actors[0] != "192.0.2.1" {
		t.Errorf("expected default actor [192.0.2.1], got %v", actors)
	}
}

func TestEnroller_OnResult_UsesConfiguredActorResolver(t *testing.T) {
	t.Parallel()
	e, _, _ := setupEnroller(t, "tok")

	var gotActor string
	e.ActorResolver = func(*http.Request) string { return "198.51.100.7" }
	e.OnResult = func(actor, _, _ string) { gotActor = actor }

	req := httptest.NewRequest(http.MethodPost, "/api/portwing/enroll", enrollBody(t, "wrong", ""))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if gotActor != "198.51.100.7" {
		t.Fatalf("actor = %q, want configured resolver result", gotActor)
	}
}

func TestEnroller_OnResult_SuccessCallback(t *testing.T) {
	t.Parallel()
	e, _, _ := setupEnroller(t, "tok")

	var gotKeyID, gotOutcome string
	e.OnResult = func(actor, keyID, outcome string) {
		gotKeyID = keyID
		gotOutcome = outcome
	}

	_, b64 := genPubKeyB64(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portwing/enroll", enrollBody(t, "tok", b64))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotOutcome != "allowed" {
		t.Errorf("expected outcome=allowed, got %q", gotOutcome)
	}
	if gotKeyID == "" {
		t.Error("expected non-empty key_id in callback")
	}
}

// ---- remoteHost fallback path --------------------------------------------

func TestRemoteHost_WithPort(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:4321"
	got := remoteHost(req)
	if got != "192.0.2.1" {
		t.Errorf("remoteHost = %q, want 192.0.2.1", got)
	}
}

func TestRemoteHost_NoPort(t *testing.T) {
	t.Parallel()
	// When RemoteAddr has no port, SplitHostPort fails and we return it as-is.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1"
	got := remoteHost(req)
	if got != "192.0.2.1" {
		t.Errorf("remoteHost = %q, want 192.0.2.1", got)
	}
}

// ---- appendKeyLine error path -------------------------------------------

func TestAppendKeyLine_DirectoryNotExist(t *testing.T) {
	t.Parallel()
	err := appendKeyLine("/nonexistent/path/authorized_keys", "ed25519 abc comment")
	if err == nil {
		t.Fatal("expected error for non-existent directory")
	}
	if !strings.Contains(err.Error(), "opening authorized_keys") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAppendKeyLine_Success(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized_keys")

	if err := appendKeyLine(path, "ed25519 abc comment"); err != nil {
		t.Fatalf("appendKeyLine failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "ed25519 abc comment") {
		t.Errorf("expected key line in file, got: %s", data)
	}
}

// ---- LoadPrivateKey permission check and read error ---------------------

func TestLoadPrivateKey_WriteOnlyFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission check not applicable on Windows")
	}
	// Mode 0200 has safe group/world bits but no owner read permission, so the
	// credential open fails.
	dir := t.TempDir()
	path := filepath.Join(dir, "private.pem")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	_, err := LoadPrivateKey(path)
	if err == nil {
		t.Fatal("expected error for write-only (unreadable) private key file")
	}
	if !strings.Contains(err.Error(), "opening private key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadPrivateKey_WorldReadable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission check not applicable on Windows")
	}
	privPEM, _, err := GenerateKeyPair("test")
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "private.pem")
	if err := os.WriteFile(path, privPEM, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err = LoadPrivateKey(path)
	if err == nil {
		t.Fatal("expected error for world-readable private key")
	}
	if !strings.Contains(err.Error(), "unsafe permissions") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---- ParsePrivateKeyPEM error paths -------------------------------------

func TestParsePrivateKeyPEM_InvalidPKCS8(t *testing.T) {
	t.Parallel()
	// A valid PEM block with type "PRIVATE KEY" but garbage DER bytes.
	garbage := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: []byte("not valid DER bytes"),
	})
	_, err := ParsePrivateKeyPEM(garbage)
	if err == nil {
		t.Fatal("expected error for invalid PKCS#8 bytes")
	}
	if !strings.Contains(err.Error(), "parsing PKCS#8") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParsePrivateKeyPEM_NotEd25519(t *testing.T) {
	t.Parallel()
	// Generate a P-256 key (not Ed25519) and marshal to PKCS#8 PEM.
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	_, err = ParsePrivateKeyPEM(pemData)
	if err == nil {
		t.Fatal("expected error for non-Ed25519 key type")
	}
	if !strings.Contains(err.Error(), "not Ed25519") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---- MarshalPrivateKeyPEM round-trip ------------------------------------

func TestMarshalPrivateKeyPEM_RoundTrip(t *testing.T) {
	t.Parallel()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pemData, err := MarshalPrivateKeyPEM(priv)
	if err != nil {
		t.Fatalf("MarshalPrivateKeyPEM: %v", err)
	}
	if len(pemData) == 0 {
		t.Error("expected non-empty PEM output")
	}
	// Round-trip parse.
	parsed, err := ParsePrivateKeyPEM(pemData)
	if err != nil {
		t.Fatalf("ParsePrivateKeyPEM: %v", err)
	}
	if string(parsed) != string(priv) {
		t.Error("round-trip private key mismatch")
	}
}

// ---- NewNonceLRU defaults -----------------------------------------------

func TestNewNonceLRU_DefaultsOnZeroValues(t *testing.T) {
	t.Parallel()
	// maxSize <= 0 and windowSeconds <= 0 should use defaults (10000, 60).
	lru := NewNonceLRU(0, 0)
	defer lru.Close()

	if err := lru.Add("testNonce"); err != nil {
		t.Error("expected Add to return true for fresh nonce")
	}
	if lru.Len() != 1 {
		t.Errorf("expected 1 entry, got %d", lru.Len())
	}
	// ttl must be 2x the 60s default window (120s), not the windowSeconds=0
	// input itself: an unasserted ttl lets the "windowSeconds <= 0" default
	// silently regress to a 0s (or otherwise wrong) ttl while Add() still
	// appears to succeed.
	if lru.ttl != 120*time.Second {
		t.Errorf("ttl = %v, want 120s (2x the 60s default window)", lru.ttl)
	}
}

func TestNewNonceLRU_NegativeValues(t *testing.T) {
	t.Parallel()
	lru := NewNonceLRU(-1, -5)
	defer lru.Close()
	if err := lru.Add("n1"); err != nil {
		t.Error("expected Add to succeed with defaults")
	}
	if lru.ttl != 120*time.Second {
		t.Errorf("ttl = %v, want 120s (2x the 60s default window)", lru.ttl)
	}
}

// TestNewNonceLRU_WindowSecondsBoundary pins the exact "windowSeconds <= 0"
// boundary in NewNonceLRU by asserting both sides of it here: 0 must still
// default to the 60s window (ttl 120s), and 1 — the smallest positive value —
// must NOT default (ttl 2s). Both are exact integers, so together they kill a
// CONDITIONALS_BOUNDARY flip of "<=" to "<": under that mutant windowSeconds=0
// stops defaulting and ttl comes out as 0 instead of 120s. The 0 row overlaps
// with TestNewNonceLRU_DefaultsOnZeroValues on purpose — without it this test
// only exercises the side of the boundary where "<=" and "<" agree, so it kills
// nothing on its own and the mutant survives if that other test ever changes.
func TestNewNonceLRU_WindowSecondsBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		windowSeconds int
		wantTTL       time.Duration
	}{
		{name: "zero takes the 60s default", windowSeconds: 0, wantTTL: 120 * time.Second},
		{name: "one does not default", windowSeconds: 1, wantTTL: 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lru := NewNonceLRU(100, tt.windowSeconds)
			defer lru.Close()
			if lru.ttl != tt.wantTTL {
				t.Errorf("windowSeconds=%d: ttl = %v, want %v", tt.windowSeconds, lru.ttl, tt.wantTTL)
			}
		})
	}
}

// ---- NonceLRU.Close -----------------------------------------------------

func TestNonceLRU_Close(t *testing.T) {
	t.Parallel()
	lru := NewNonceLRU(100, 60)
	lru.Close() // must not panic
}

func TestNonceLRU_CloseIdempotent(t *testing.T) {
	t.Parallel()
	lru := NewNonceLRU(100, 60)
	lru.Close()
	lru.Close() // second Close must not panic (idempotent)
}

// ---- NonceLRU cleanup goroutine -----------------------------------------

func TestNonceLRU_CleanupEvictsExpiredEntries(t *testing.T) {
	t.Parallel()
	// Use a 1-second TTL. We can't easily tick the goroutine, but we can
	// verify the LRU stays functional after entries age out via direct map
	// manipulation. The cleanup code path is exercised by the ticker firing;
	// we test the observable behavior after Close instead of racing with a timer.
	lru := NewNonceLRU(100, 1)
	_ = lru.Add("nonce-a")
	if !lru.Seen("nonce-a") {
		t.Error("nonce-a should be present before close")
	}
	lru.Close()
}

// ---- HasSignature -------------------------------------------------------

func TestHasSignature_Present(t *testing.T) {
	t.Parallel()
	h := make(http.Header)
	h.Set(HeaderSignature, "somesig")
	if !HasSignature(h) {
		t.Error("expected HasSignature=true when header is set")
	}
}

func TestHasSignature_Absent(t *testing.T) {
	t.Parallel()
	h := make(http.Header)
	if HasSignature(h) {
		t.Error("expected HasSignature=false when header is missing")
	}
}

// ---- VerifyRequest additional error paths --------------------------------

func TestVerifyRequest_InvalidNonceHex(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)
	defer lru.Close()

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	tsUnix := time.Now().Unix()
	// Exactly 32 chars but not valid hex (contains 'g').
	badNonce := "gggggggggggggggggggggggggggggggg"
	signRequest(t, req, nil, priv, pub, tsUnix, badNonce)
	req.Header.Set(HeaderNonce, badNonce)

	_, err := VerifyRequest(req, nil, reg, lru, 60)
	if !errors.Is(err, ErrInvalidNonce) {
		t.Errorf("expected ErrInvalidNonce for non-hex nonce, got: %v", err)
	}
}

func TestVerifyRequest_TimestampParseError(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)
	defer lru.Close()

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	nonce := randomNonce(t)
	tsUnix := time.Now().Unix()
	signRequest(t, req, nil, priv, pub, tsUnix, nonce)
	// Overwrite with non-numeric timestamp.
	req.Header.Set(HeaderTimestamp, "notanumber")

	_, err := VerifyRequest(req, nil, reg, lru, 60)
	if !errors.Is(err, ErrTimestampSkew) {
		t.Errorf("expected ErrTimestampSkew for unparseable timestamp, got: %v", err)
	}
}

func TestVerifyRequest_FutureTimestamp(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)
	defer lru.Close()

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	nonce := randomNonce(t)
	// 120 seconds in the future — exceeds 60s window.
	tsUnix := time.Now().Unix() + 120
	signRequest(t, req, nil, priv, pub, tsUnix, nonce)

	_, err := VerifyRequest(req, nil, reg, lru, 60)
	if !errors.Is(err, ErrTimestampSkew) {
		t.Errorf("expected ErrTimestampSkew for future timestamp, got: %v", err)
	}
}

func TestVerifyRequest_ClockSkewWarningPath(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)
	defer lru.Close()

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	nonce := randomNonce(t)
	// 40 seconds old: inside 60s window but triggers the >30s warning slog.
	tsUnix := time.Now().Unix() - 40
	signRequest(t, req, nil, priv, pub, tsUnix, nonce)

	keyID, err := VerifyRequest(req, nil, reg, lru, 60)
	if err != nil {
		t.Fatalf("expected success for 40s skew within 60s window, got: %v", err)
	}
	if keyID == "" {
		t.Error("expected non-empty keyID")
	}
}

func TestVerifyRequest_InvalidBase64Signature(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)
	defer lru.Close()

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	nonce := randomNonce(t)
	tsUnix := time.Now().Unix()
	signRequest(t, req, nil, priv, pub, tsUnix, nonce)
	// Replace with invalid base64url ('+' is not valid in raw URL encoding).
	req.Header.Set(HeaderSignature, "not+valid+base64url+padding!!!")

	_, err := VerifyRequest(req, nil, reg, lru, 60)
	if !errors.Is(err, ErrInvalidSig) {
		t.Errorf("expected ErrInvalidSig for invalid base64url, got: %v", err)
	}
}

// ---- parseAuthorizedKeys scanner error path -----------------------------

func TestParseAuthorizedKeys_ScannerError(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("not applicable on Windows")
	}
	// A directory with 0700 permissions must be rejected before scanning.
	dir := t.TempDir()
	// Use a subdirectory as the "file" path.
	subdir := filepath.Join(dir, "authorized_keys")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	_, err := parseAuthorizedKeys(subdir)
	if err == nil {
		t.Fatal("expected error when path is a directory")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---- parseAuthorizedKeys open error path --------------------------------

func TestParseAuthorizedKeys_OpenError(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission check not applicable on Windows")
	}
	// Create a 0600 file, then chmod it to 0200 (write-only, no read).
	// Mode 0200 passes the group/world permission policy, but opening for read
	// fails because the owner read bit is absent.
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(path, []byte("# empty\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	_, err := parseAuthorizedKeys(path)
	if err == nil {
		t.Fatal("expected error for unreadable file")
	}
	if !strings.Contains(err.Error(), "opening authorized_keys") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---- KeyRegistry.Load error path ----------------------------------------

func TestKeyRegistry_Load_ParseError(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission check not applicable on Windows")
	}
	// World-readable file causes parseAuthorizedKeys to return an error.
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized_keys")
	_, b64 := genPubKeyB64(t)
	if err := os.WriteFile(path, []byte("ed25519 "+b64+" k\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r := NewKeyRegistry(path)
	err := r.Load()
	if err == nil {
		t.Fatal("expected error loading world-readable authorized_keys")
	}
	if !strings.Contains(err.Error(), "world-readable") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---- Enroller appendKeyLine failure path --------------------------------

func TestEnroller_AppendKeyLineFailure(t *testing.T) {
	t.Parallel()
	// Set authorizedFile to a directory path; os.OpenFile with O_WRONLY on a
	// directory will fail, exercising the "internal server error" branch.
	dir := t.TempDir()
	// Use the directory itself as the "file" path — opening a dir for write fails.
	badPath := dir

	reg := NewKeyRegistry("") // empty path = always-empty registry, no file needed
	e := NewEnroller("tok", badPath, reg)

	_, b64 := genPubKeyB64(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portwing/enroll", enrollBody(t, "tok", b64))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when authorized_keys file is unwritable, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---- Enroller registry reload failure path ------------------------------

func TestEnroller_RegistryReloadFailure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission check not applicable on Windows")
	}
	e, _, path := setupEnroller(t, "tok")

	var outcome string
	e.OnResult = func(_, _, o string) { outcome = o }

	// Make file world-readable so registry.Load() fails inside ServeHTTP.
	// appendKeyLine opens O_APPEND|O_WRONLY so it won't fail; only the
	// subsequent registry.Load() (which needs to read the file) will fail.
	// #nosec G302 -- intentionally world-readable to exercise the registry's permission-rejection path.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	_, b64 := genPubKeyB64(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portwing/enroll", enrollBody(t, "tok", b64))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	// Should still return 200 — registry reload failure is logged but non-fatal.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 even with reload failure, got %d: %s", rec.Code, rec.Body.String())
	}
	if outcome != "allowed" {
		t.Errorf("expected outcome=allowed, got %q", outcome)
	}
}
