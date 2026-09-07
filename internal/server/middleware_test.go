package server

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/audit"
	"github.com/codeswhat/portwing/internal/auth"
)

// noAudit returns a disabled audit.Logger for tests that only care about HTTP
// status codes and don't need to verify audit output.
func noAudit(t *testing.T) *audit.Logger {
	t.Helper()
	l, _, err := audit.New("", 0)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	return l
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

type blockingTokenVerifier struct {
	entered chan struct{}
	release <-chan struct{}
	active  atomic.Int64
	maximum atomic.Int64
}

type saturatedTokenVerifier struct{}

func (saturatedTokenVerifier) Verify(string) bool {
	panic("Verify must not run when VerifyWithCapacity is available")
}

func (saturatedTokenVerifier) VerifyWithCapacity(string) (bool, bool) {
	return false, false
}

func (v *blockingTokenVerifier) Verify(string) bool {
	active := v.active.Add(1)
	for {
		maximum := v.maximum.Load()
		if active <= maximum || v.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	v.entered <- struct{}{}
	<-v.release
	v.active.Add(-1)
	return false
}

func TestAuthMiddlewareBoundsConcurrentVerificationsPerIP(t *testing.T) {
	rl := NewRateLimiter()
	defer rl.Stop()

	const requests = 12
	release := make(chan struct{})
	verifier := &blockingTokenVerifier{
		entered: make(chan struct{}, requests),
		release: release,
	}
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, noAudit(t), nil, http.HandlerFunc(okHandler))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "192.0.2.10:12345"
			req.Header.Set(headerPortwingToken, "wrong")
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	close(start)

	for i := 0; i < 2; i++ {
		select {
		case <-verifier.entered:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for bounded verification slots")
		}
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := verifier.maximum.Load(); got > 2 {
		t.Fatalf("maximum concurrent verifications = %d, want at most 2", got)
	}
}

func TestAuthMiddlewareReturns429WhenVerifierCapacityIsExhausted(t *testing.T) {
	rl := NewRateLimiter()
	defer rl.Stop()

	h := rl.AuthMiddlewareWithEd25519(saturatedTokenVerifier{}, Ed25519Config{}, noAudit(t), nil, http.HandlerFunc(okHandler))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.12:12345"
	req.Header.Set(headerPortwingToken, "presented")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when verifier capacity is exhausted, got %d", rec.Code)
	}
	if _, ok := rl.attempts["192.0.2.12"]; ok {
		t.Fatal("capacity rejection must release the per-IP verification reservation")
	}
}

func TestRateLimiterAbuseWindowStateTransitions(t *testing.T) {
	rl := &RateLimiter{
		attempts: make(map[string]*ipAttempts),
		maxFails: 2,
		window:   time.Minute,
		maxIPs:   1,
	}

	if rl.isAbuseRateLimited("missing") {
		t.Fatal("missing abuse record must not be rate limited")
	}

	rl.recordAbuse("client")
	if rl.isAbuseRateLimited("client") {
		t.Fatal("one malformed request must remain below the abuse limit")
	}
	rl.recordAbuse("client")
	if !rl.isAbuseRateLimited("client") {
		t.Fatal("client must be rate limited at the abuse threshold")
	}

	rl.abuse["client"].firstFail = time.Now().Add(-2 * rl.window)
	if rl.isAbuseRateLimited("client") {
		t.Fatal("expired abuse window must not remain rate limited")
	}
	if _, ok := rl.abuse["client"]; ok {
		t.Fatal("expired abuse record must be removed during lookup")
	}

	rl.recordAbuse("existing")
	rl.recordAbuse("overflow")
	if _, ok := rl.abuse["overflow"]; ok {
		t.Fatal("abuse table must not grow beyond maxIPs")
	}
	rl.abuse["existing"].count = rl.maxFails
	rl.abuse["existing"].firstFail = time.Now().Add(-2 * rl.window)
	rl.recordAbuse("existing")
	if got := rl.abuse["existing"].count; got != 1 {
		t.Fatalf("expired abuse record count = %d, want 1", got)
	}
}

func TestRateLimiterAuthReservationStateTransitions(t *testing.T) {
	rl := &RateLimiter{
		attempts: make(map[string]*ipAttempts),
		maxFails: 2,
		window:   time.Minute,
		maxIPs:   10,
	}

	if !rl.tryBeginAuth("client") {
		t.Fatal("default per-IP capacity must allow the first in-flight verification")
	}
	if !rl.tryBeginAuth("client") {
		t.Fatal("default per-IP capacity must allow the second in-flight verification")
	}
	if rl.tryBeginAuth("client") {
		t.Fatal("third in-flight verification must be rejected")
	}
	rl.finishAuth("client", true)
	if got := rl.attempts["client"].inFlight; got != 1 {
		t.Fatalf("in-flight reservations after first success = %d, want 1", got)
	}
	rl.finishAuth("client", true)
	if _, ok := rl.attempts["client"]; ok {
		t.Fatal("successful final reservation must remove an empty tracking record")
	}
	rl.finishAuth("missing", false)

	for i := 0; i < rl.maxFails; i++ {
		if !rl.tryBeginAuth("failing") {
			t.Fatalf("failure reservation %d was unexpectedly rejected", i+1)
		}
		rl.finishAuth("failing", false)
	}
	if !rl.IsRateLimited("failing") {
		t.Fatal("completed failures at the threshold must rate limit the client")
	}
	if rl.tryBeginAuth("failing") {
		t.Fatal("rate-limited client must not reserve another verification")
	}

	rl.attempts["failing"].firstFail = time.Now().Add(-2 * rl.window)
	if rl.IsRateLimited("failing") {
		t.Fatal("expired failure window must not remain rate limited")
	}
	if _, ok := rl.attempts["failing"]; ok {
		t.Fatal("expired idle failure record must be removed")
	}

	rl.attempts["busy"] = &ipAttempts{
		count:     rl.maxFails,
		firstFail: time.Now().Add(-2 * rl.window),
		inFlight:  1,
	}
	if rl.IsRateLimited("busy") {
		t.Fatal("expired busy record must reset instead of remaining rate limited")
	}
	if got := rl.attempts["busy"]; got.count != 0 || !got.firstFail.IsZero() || got.inFlight != 1 {
		t.Fatalf("expired busy record was not reset safely: %+v", got)
	}

	rl.attempts["expired"] = &ipAttempts{
		count:     rl.maxFails,
		firstFail: time.Now().Add(-2 * rl.window),
	}
	if !rl.tryBeginAuth("expired") {
		t.Fatal("expired failure record must allow a fresh reservation")
	}
	if got := rl.attempts["expired"]; got.count != 0 || !got.firstFail.IsZero() || got.inFlight != 1 {
		t.Fatalf("fresh reservation did not reset expired state: %+v", got)
	}
}

// TestAuthMiddlewareRawTokenAccept verifies the raw-token path accepts a correct token.
func TestAuthMiddlewareRawTokenAccept(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	verifier := newRawTokenVerifier("correct")
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, noAudit(t), nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(headerPortwingToken, "correct")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestRawTokenVerifierUsesFixedLengthDigest(t *testing.T) {
	t.Parallel()
	verifier := newRawTokenVerifier("correct horse battery staple")
	if !verifier.Verify("correct horse battery staple") {
		t.Fatal("correct token did not verify")
	}
	for _, token := range []string{"", "x", strings.Repeat("x", 4096)} {
		if verifier.Verify(token) {
			t.Fatalf("incorrect token of length %d verified", len(token))
		}
	}
}

func TestTryBeginAuthPreservesFailOpenTrackingAtIPCapacity(t *testing.T) {
	rl := &RateLimiter{
		attempts:    map[string]*ipAttempts{"existing": {count: 1, firstFail: time.Now()}},
		maxFails:    10,
		window:      time.Minute,
		maxIPs:      1,
		maxInFlight: 2,
	}

	if !rl.tryBeginAuth("untracked") {
		t.Fatal("IP table capacity must not turn tracking exhaustion into an auth denial")
	}
	rl.finishAuth("untracked", false)
	if _, ok := rl.attempts["untracked"]; ok {
		t.Fatal("capacity-overflow IP should remain untracked")
	}
}

// TestAuthMiddlewareRawTokenReject verifies the raw-token path rejects a wrong token.
func TestAuthMiddlewareRawTokenReject(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	verifier := newRawTokenVerifier("correct")
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, noAudit(t), nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(headerPortwingToken, "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// TestAuthMiddlewareArgon2idAccept verifies that a correct token is accepted
// when the verifier uses Argon2id.
func TestAuthMiddlewareArgon2idAccept(t *testing.T) {
	t.Parallel()

	const token = "argon2correct"
	phc, err := HashToken(token)
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}

	params, err := ParsePHC(phc)
	if err != nil {
		t.Fatalf("ParsePHC: %v", err)
	}

	rl := NewRateLimiter()
	verifier := newArgon2Verifier(params)
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, noAudit(t), nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(headerPortwingToken, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// TestAuthMiddlewareArgon2idReject verifies that a wrong token is rejected
// when the verifier uses Argon2id.
func TestAuthMiddlewareArgon2idReject(t *testing.T) {
	t.Parallel()

	const token = "argon2secret"
	phc, err := HashToken(token)
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}

	params, err := ParsePHC(phc)
	if err != nil {
		t.Fatalf("ParsePHC: %v", err)
	}

	rl := NewRateLimiter()
	verifier := newArgon2Verifier(params)
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, noAudit(t), nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(headerPortwingToken, "wrongtoken")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// TestAuthMiddlewareNilVerifier verifies that no-auth mode passes all requests.
func TestAuthMiddlewareNilVerifier(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	h := rl.AuthMiddlewareWithEd25519(nil, Ed25519Config{}, noAudit(t), nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for no-auth mode, got %d", rec.Code)
	}
}

// TestAuthMiddlewareBearerHeader verifies that Authorization: Bearer is
// accepted as the primary token header.
func TestAuthMiddlewareBearerHeader(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	verifier := newRawTokenVerifier("bearer-secret")
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, noAudit(t), nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer bearer-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for Authorization: Bearer, got %d", rec.Code)
	}
}

// TestClientIPIgnoresForwardedForByDefault verifies that with no trusted
// proxies configured, spoofed forwarding headers do not change the
// rate-limiting identity.
func TestClientIPIgnoresForwardedForByDefault(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:50000"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("X-Real-IP", "203.0.113.8")

	if got := rl.clientIP(req); got != "192.0.2.1" {
		t.Fatalf("expected remote addr 192.0.2.1, got %q", got)
	}
}

// TestClientIPTrustedProxy verifies that forwarding headers are honored when
// the direct peer is a trusted proxy, walking the chain right to left past
// other trusted hops.
func TestClientIPTrustedProxy(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	nets, err := ParseTrustedProxies([]string{"192.0.2.0/24"})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	rl.SetTrustedProxies(nets)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:50000"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 192.0.2.5")

	if got := rl.clientIP(req); got != "203.0.113.7" {
		t.Fatalf("expected forwarded client 203.0.113.7, got %q", got)
	}
}

// TestRateLimiterNotBypassedBySpoofedXFF verifies that rotating
// X-Forwarded-For values cannot reset the failure counter for the real peer.
func TestRateLimiterNotBypassedBySpoofedXFF(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	verifier := newRawTokenVerifier("correct")
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, noAudit(t), nil, http.HandlerFunc(okHandler))

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.1:50000"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		req.Header.Set(headerPortwingToken, "wrong")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:50000"
	req.Header.Set("X-Forwarded-For", "203.0.113.250")
	req.Header.Set(headerPortwingToken, "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after 10 failures from same peer, got %d", rec.Code)
	}
}

// TestAuthMiddlewareFallbackHeader verifies that X-Dd-Agent-Secret is accepted
// when Portwing token headers are absent.
func TestAuthMiddlewareFallbackHeader(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	verifier := newRawTokenVerifier("legacytoken")
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, noAudit(t), nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Dd-Agent-Secret", "legacytoken")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for X-Dd-Agent-Secret header, got %d", rec.Code)
	}
}

// TestAuditMiddlewareEmitsAuthFailure verifies that the middleware emits an
// auth_failure audit event when a wrong token is presented.
func TestAuditMiddlewareEmitsAuthFailure(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir() + "/audit.log"
	l, close, err := audit.New(tmp, 0)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(close)

	rl := NewRateLimiter()
	verifier := newRawTokenVerifier("correct")
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, l, nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/_portwing/info", nil)
	req.Header.Set(headerPortwingToken, "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	data, _ := readFile(tmp)
	if !contains(data, audit.EventAuthFailure) {
		t.Errorf("expected %q in audit log, got: %s", audit.EventAuthFailure, data)
	}
}

// TestAuditMiddlewareEmitsRateLimited verifies that a rate-limited request
// produces a rate_limited audit event.
func TestAuditMiddlewareEmitsRateLimited(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir() + "/audit.log"
	l, close, err := audit.New(tmp, 0)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(close)

	rl := NewRateLimiter()
	verifier := newRawTokenVerifier("correct")
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, l, nil, http.HandlerFunc(okHandler))

	// Exhaust the 10-failure limit.
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set(headerPortwingToken, "bad")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}

	// This request should now be rate-limited.
	req := httptest.NewRequest(http.MethodGet, "/_portwing/compose", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set(headerPortwingToken, "bad")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}

	data, _ := readFile(tmp)
	if !contains(data, audit.EventRateLimited) {
		t.Errorf("expected %q in audit log, got: %s", audit.EventRateLimited, data)
	}
}

// TestAuditMiddlewareEmitsAPIRequest verifies that an allowed request emits
// an api_request event.
func TestAuditMiddlewareEmitsAPIRequest(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir() + "/audit.log"
	l, close, err := audit.New(tmp, 0)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(close)

	rl := NewRateLimiter()
	verifier := newRawTokenVerifier("correct")
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, l, nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/_portwing/info", nil)
	req.Header.Set(headerPortwingToken, "correct")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	data, _ := readFile(tmp)
	if !contains(data, audit.EventAPIRequest) {
		t.Errorf("expected %q in audit log, got: %s", audit.EventAPIRequest, data)
	}
}

// TestAuthMiddlewarePreservesStreamingInterfaces verifies that the
// statusRecorder wrapper still exposes http.Flusher and http.Hijacker to
// downstream handlers — SSE streaming and Docker exec/attach hijacking
// depend on them.
func TestAuthMiddlewarePreservesStreamingInterfaces(t *testing.T) {
	t.Parallel()

	var sawFlusher, sawHijacker bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawFlusher = w.(http.Flusher)
		_, sawHijacker = w.(http.Hijacker)
		w.WriteHeader(http.StatusOK)
	})

	rl := NewRateLimiter()
	verifier := newRawTokenVerifier("correct")
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, noAudit(t), nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set(headerPortwingToken, "correct")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !sawFlusher {
		t.Error("http.Flusher lost through authentication middleware")
	}
	if !sawHijacker {
		t.Error("http.Hijacker lost through authentication middleware")
	}

	// No-auth mode wraps with the same recorder; verify that path too.
	sawFlusher, sawHijacker = false, false
	h = rl.AuthMiddlewareWithEd25519(nil, Ed25519Config{}, noAudit(t), nil, inner)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events", nil))

	if !sawFlusher {
		t.Error("http.Flusher lost through authentication middleware (no-auth mode)")
	}
	if !sawHijacker {
		t.Error("http.Hijacker lost through authentication middleware (no-auth mode)")
	}
}

// ---- Ed25519 middleware integration tests ---------------------------------

// setupEd25519 creates an Ed25519 keypair, writes the authorized_keys file,
// returns a loaded Ed25519Config plus the private key.
func setupEd25519(t *testing.T) (Ed25519Config, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(pub)
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(path, []byte("ed25519 "+b64+" testkey\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	reg := auth.NewKeyRegistry(path)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := Ed25519Config{
		Registry:       reg,
		Nonces:         auth.NewNonceLRU(1000, 60),
		MaxSkewSeconds: 60,
	}
	return cfg, priv
}

// signEd25519Request attaches Ed25519 headers to req.
func signEd25519Request(t *testing.T, req *http.Request, body []byte, priv ed25519.PrivateKey, tsUnix int64, nonce string) {
	t.Helper()
	pub := priv.Public().(ed25519.PublicKey)
	bodyHash := auth.BodyHashHex(body)
	msg := auth.CanonicalMessage(req.Method, auth.CanonicalRequestTarget(req.URL), bodyHash, tsUnix, nonce)
	sig := ed25519.Sign(priv, msg)

	h := sha256.Sum256(pub)
	keyID := hex.EncodeToString(h[:8])

	req.Header.Set(auth.HeaderKeyID, keyID)
	req.Header.Set(auth.HeaderTimestamp, strconv.FormatInt(tsUnix, 10))
	req.Header.Set(auth.HeaderNonce, nonce)
	req.Header.Set(auth.HeaderSignature, base64.RawURLEncoding.EncodeToString(sig))
	req.Header.Set(auth.HeaderSignatureVersion, auth.SignatureVersion2)
}

func freshNonce(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

// TestEd25519MiddlewareAccept verifies a correctly signed request is accepted.
func TestEd25519MiddlewareAccept(t *testing.T) {
	t.Parallel()
	ed, priv := setupEd25519(t)
	rl := NewRateLimiter()
	h := rl.AuthMiddlewareWithEd25519(nil, ed, noAudit(t), nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/_portwing/info", nil)
	signEd25519Request(t, req, nil, priv, time.Now().Unix(), freshNonce(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestEd25519MiddlewareRejectsWhenVerificationCapacityFull(t *testing.T) {
	t.Parallel()

	ed, priv := setupEd25519(t)
	rl := NewRateLimiter()
	defer rl.Stop()

	const clientIP = "198.51.100.10"
	rl.maxInFlight = 1
	rl.attempts[clientIP] = &ipAttempts{inFlight: 1}

	h := rl.AuthMiddlewareWithEd25519(nil, ed, noAudit(t), nil, http.HandlerFunc(okHandler))
	req := httptest.NewRequest(http.MethodGet, "/_portwing/info", nil)
	req.RemoteAddr = clientIP + ":1234"
	signEd25519Request(t, req, nil, priv, time.Now().Unix(), freshNonce(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when Ed25519 verification capacity is full, got %d", rec.Code)
	}
	rl.mu.Lock()
	attempt := *rl.attempts[clientIP]
	rl.mu.Unlock()
	if attempt.inFlight != 1 || attempt.count != 0 {
		t.Fatalf("rejected verification changed rate-limit state: %+v", attempt)
	}
}

// TestEd25519MiddlewareAuthBodyReadDeadline is a regression test for the
// pre-auth slowloris finding: a client that sends signature headers plus a
// Content-Length body but never actually sends the body bytes must not pin
// the handler goroutine in io.ReadAll forever. It must instead get a 408
// once authBodyReadDeadline elapses.
//
// Not t.Parallel(): it mutates the package-level authBodyReadDeadline var,
// which every request through this middleware reads.
func TestEd25519MiddlewareAuthBodyReadDeadline(t *testing.T) {
	ed, priv := setupEd25519(t)
	rl := NewRateLimiter()
	defer rl.Stop()
	h := rl.AuthMiddlewareWithEd25519(nil, ed, noAudit(t), nil, http.HandlerFunc(okHandler))

	ts := httptest.NewServer(h)
	defer ts.Close()

	orig := authBodyReadDeadline
	authBodyReadDeadline = 200 * time.Millisecond
	t.Cleanup(func() { authBodyReadDeadline = orig })

	// Sign a request for a body we will declare (via Content-Length) but
	// never send, simulating a slow drip that never completes.
	body := []byte("hello")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	signEd25519Request(t, req, body, priv, time.Now().Unix(), freshNonce(t))

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	defer conn.Close()

	var raw strings.Builder
	raw.WriteString("POST / HTTP/1.1\r\n")
	raw.WriteString("Host: " + u.Host + "\r\n")
	fmt.Fprintf(&raw, "Content-Length: %d\r\n", len(body))
	for name, values := range req.Header {
		for _, v := range values {
			raw.WriteString(name + ": " + v + "\r\n")
		}
	}
	raw.WriteString("\r\n")
	// Deliberately do not write the body.

	if _, err := conn.Write([]byte(raw.String())); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Safety net: if the deadline logic regresses to blocking forever, fail
	// with a clear timeout instead of hanging the test suite.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("expected 408 once the auth body read deadline fires, got %d", resp.StatusCode)
	}
}

// TestEd25519MiddlewareTokenFallback verifies that if no Ed25519 config is set,
// the token verifier is still used.
func TestEd25519MiddlewareTokenFallback(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter()
	verifier := newRawTokenVerifier("mysecret")
	h := rl.AuthMiddlewareWithEd25519(verifier, Ed25519Config{}, noAudit(t), nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	req.Header.Set(headerPortwingToken, "mysecret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for token fallback, got %d", rec.Code)
	}
}

// TestEd25519MiddlewareBothConfigured verifies that when both Ed25519 and token
// are configured, a signed request uses the Ed25519 path (not the token path).
func TestEd25519MiddlewareBothConfigured(t *testing.T) {
	t.Parallel()
	ed, priv := setupEd25519(t)
	rl := NewRateLimiter()
	verifier := newRawTokenVerifier("mysecret")
	h := rl.AuthMiddlewareWithEd25519(verifier, ed, noAudit(t), nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	signEd25519Request(t, req, nil, priv, time.Now().Unix(), freshNonce(t))
	// Do not set the token — should succeed via Ed25519 alone.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via Ed25519 when both configured, got %d", rec.Code)
	}
}

// TestEd25519MiddlewareBadTimestamp verifies that a skewed timestamp returns
// 401 with X-Portwing-Reason: timestamp-skew.
func TestEd25519MiddlewareBadTimestamp(t *testing.T) {
	t.Parallel()
	ed, priv := setupEd25519(t)
	rl := NewRateLimiter()
	h := rl.AuthMiddlewareWithEd25519(nil, ed, noAudit(t), nil, http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	signEd25519Request(t, req, nil, priv, time.Now().Unix()-200, freshNonce(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if got := rec.Header().Get(auth.HeaderReason); got != "timestamp-skew" {
		t.Errorf("X-Portwing-Reason: got %q want %q", got, "timestamp-skew")
	}
}

// TestEd25519MiddlewareReplayedNonce verifies replay protection.
func TestEd25519MiddlewareReplayedNonce(t *testing.T) {
	t.Parallel()
	ed, priv := setupEd25519(t)
	rl := NewRateLimiter()
	h := rl.AuthMiddlewareWithEd25519(nil, ed, noAudit(t), nil, http.HandlerFunc(okHandler))

	nonce := freshNonce(t)
	tsUnix := time.Now().Unix()

	req1 := httptest.NewRequest(http.MethodGet, "/path", nil)
	signEd25519Request(t, req1, nil, priv, tsUnix, nonce)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", rec1.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/path", nil)
	signEd25519Request(t, req2, nil, priv, tsUnix, nonce)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("replayed request: expected 401, got %d", rec2.Code)
	}
	if got := rec2.Header().Get(auth.HeaderReason); got != "replay" {
		t.Errorf("X-Portwing-Reason: got %q want %q", got, "replay")
	}
}

// TestEd25519MiddlewareUnknownKey verifies that an unknown key returns 401.
func TestEd25519MiddlewareUnknownKey(t *testing.T) {
	t.Parallel()
	ed, _ := setupEd25519(t)
	rl := NewRateLimiter()
	h := rl.AuthMiddlewareWithEd25519(nil, ed, noAudit(t), nil, http.HandlerFunc(okHandler))

	// Generate a key that is NOT in the registry.
	_, priv2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	signEd25519Request(t, req, nil, priv2, time.Now().Unix(), freshNonce(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown key, got %d", rec.Code)
	}
	if got := rec.Header().Get(auth.HeaderReason); got != "unknown-key" {
		t.Errorf("X-Portwing-Reason: got %q want %q", got, "unknown-key")
	}
}

// TestRateLimitOnlyRecordsFailures verifies that the enrollment guard counts
// downstream 401s toward the rate limit, so the enrollment token cannot be
// brute-forced past the limiter.
func TestRateLimitOnlyRecordsFailures(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	deny := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	h := rl.rateLimitOnly(deny, nil)

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/portwing/enroll", nil)
		req.RemoteAddr = "192.0.2.9:40000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/portwing/enroll", nil)
	req.RemoteAddr = "192.0.2.9:40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after 10 enrollment failures, got %d", rec.Code)
	}
}

func TestRateLimitOnlyRecordsMalformedEnrollmentAbuse(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	defer rl.Stop()
	malformed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
	})
	h := rl.rateLimitOnly(malformed, nil)

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/portwing/enroll", nil)
		req.RemoteAddr = "192.0.2.11:40000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d: expected 400, got %d", i, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/portwing/enroll", nil)
	req.RemoteAddr = "192.0.2.11:40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after malformed enrollment abuse, got %d", rec.Code)
	}
}

// helpers

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// dialSignatureHeadedSlowBody opens a raw connection to addr and writes a
// request that declares a body via Content-Length and carries Ed25519
// signature headers, but never sends the body bytes. Signature headers cost
// nothing to attach — no credential is needed to reach the Ed25519 branch —
// so this is exactly the shape of the pre-admission slowloris.
func dialSignatureHeadedSlowBody(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var raw strings.Builder
	raw.WriteString("POST / HTTP/1.1\r\n")
	raw.WriteString("Host: " + addr + "\r\n")
	raw.WriteString("Content-Length: 64\r\n")
	raw.WriteString(auth.HeaderKeyID + ": deadbeefdeadbeef\r\n")
	raw.WriteString(auth.HeaderTimestamp + ": " + strconv.FormatInt(time.Now().Unix(), 10) + "\r\n")
	raw.WriteString(auth.HeaderNonce + ": " + freshNonce(t) + "\r\n")
	raw.WriteString(auth.HeaderSignature + ": bogus\r\n")
	raw.WriteString(auth.HeaderSignatureVersion + ": " + auth.SignatureVersion2 + "\r\n")
	raw.WriteString("\r\n")
	// Deliberately do not write the 64 declared body bytes.
	if _, err := conn.Write([]byte(raw.String())); err != nil {
		t.Fatalf("write request headers: %v", err)
	}
	return conn
}

// waitForInFlightSlots polls the per-IP in-flight verification count until it
// reaches want, or fails the test. Reading it directly is what makes the
// admission ordering observable: the count is the bound the finding says was
// bypassed.
func waitForInFlightSlots(t *testing.T, rl *RateLimiter, ip string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	got := -1
	for time.Now().Before(deadline) {
		rl.mu.Lock()
		got = 0
		if a, ok := rl.attempts[ip]; ok {
			got = a.inFlight
		}
		rl.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("in-flight verification slots for %s = %d, want %d", ip, got, want)
}

// TestEd25519MiddlewareBoundsConcurrentSlowBodies is a regression test for the
// pre-admission body read: the Ed25519 path buffered up to 1 MiB before it
// reserved the per-IP verification slot, so N concurrent signature-headered
// slow bodies from one IP each held a goroutine and a connection while
// maxInFlight still reported zero slots in use. The read must happen inside
// the slot, and the slot must come back on the read-failure path.
//
// Not t.Parallel(): it mutates the package-level authBodyReadDeadline var,
// which every request through this middleware reads.
func TestEd25519MiddlewareBoundsConcurrentSlowBodies(t *testing.T) {
	ed, _ := setupEd25519(t)
	rl := NewRateLimiter()
	defer rl.Stop()
	const maxInFlight = 2
	rl.maxInFlight = maxInFlight

	orig := authBodyReadDeadline
	authBodyReadDeadline = 2 * time.Second
	t.Cleanup(func() { authBodyReadDeadline = orig })

	h := rl.AuthMiddlewareWithEd25519(nil, ed, noAudit(t), nil, http.HandlerFunc(okHandler))
	ts := httptest.NewServer(h)
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	const clientIP = "127.0.0.1"

	// Saturate the bound with slow bodies. Before the fix these never
	// occupied a slot at all, so this wait times out on the old code.
	slow := make([]net.Conn, maxInFlight)
	for i := range slow {
		slow[i] = dialSignatureHeadedSlowBody(t, u.Host)
	}
	waitForInFlightSlots(t, rl, clientIP, maxInFlight, 1500*time.Millisecond)

	// One more from the same IP must be turned away immediately rather than
	// admitted into another 1 MiB read.
	extra := dialSignatureHeadedSlowBody(t, u.Host)
	if err := extra.SetReadDeadline(time.Now().Add(1500 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(extra), nil)
	if err != nil {
		t.Fatalf("read response for the over-bound request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-bound slow body got %d, want 429 (admission bound bypassed)", resp.StatusCode)
	}

	// Every saturating request must hand its slot back when its read times
	// out, or the bound leaks shut after one burst.
	for i, conn := range slow {
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatalf("SetReadDeadline on slow conn %d: %v", i, err)
		}
		slowResp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read response for slow conn %d: %v", i, err)
		}
		slowResp.Body.Close()
		if slowResp.StatusCode != http.StatusRequestTimeout {
			t.Fatalf("slow conn %d got %d, want 408", i, slowResp.StatusCode)
		}
	}
	waitForInFlightSlots(t, rl, clientIP, 0, 2*time.Second)
}
