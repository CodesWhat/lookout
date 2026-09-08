package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testSetup generates an Ed25519 keypair, writes the public key to an
// authorized_keys file, returns the loaded registry, a nonce LRU, the public
// key, and the private key.
func testSetup(t *testing.T) (
	*KeyRegistry, *NonceLRU, ed25519.PublicKey, ed25519.PrivateKey,
) {
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

	r := NewKeyRegistry(path)
	if err := r.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// NewNonceLRU starts a cleanup goroutine, so every LRU handed out here
	// needs closing or the goroutine outlives its test and the package
	// accumulates them until the binary exits. Closing in the helper covers
	// every caller; Close is idempotent, so a caller may still close early.
	lru := NewNonceLRU(1000, 60)
	t.Cleanup(lru.Close)

	return r, lru, pub, priv
}

// signRequest signs a request and sets Ed25519 headers on it.
func signRequest(t *testing.T, r *http.Request, body []byte, priv ed25519.PrivateKey, pub ed25519.PublicKey, tsUnix int64, nonce string) {
	t.Helper()
	bodyHash := BodyHashHex(body)
	msg := CanonicalMessage(r.Method, CanonicalRequestTarget(r.URL), bodyHash, tsUnix, nonce)
	sig := ed25519.Sign(priv, msg)

	keyIDBytes := sha256.Sum256(pub)
	keyID := hex.EncodeToString(keyIDBytes[:8])

	r.Header.Set(HeaderKeyID, keyID)
	r.Header.Set(HeaderTimestamp, strconv.FormatInt(tsUnix, 10))
	r.Header.Set(HeaderNonce, nonce)
	r.Header.Set(HeaderSignature, base64.RawURLEncoding.EncodeToString(sig))
	r.Header.Set(HeaderSignatureVersion, SignatureVersion2)
}

// randomNonce generates a fresh 32-hex-char nonce.
func randomNonce(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

// ---- CanonicalMessage tests ------------------------------------------------

func TestCanonicalMessage_EmptyBody(t *testing.T) {
	t.Parallel()
	msg := CanonicalMessage("GET", "/api/test", emptyBodyHash, 1749600000, "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6")
	want := "GET\n/api/test\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n1749600000\na1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	if string(msg) != want {
		t.Errorf("canonical message mismatch\ngot:  %q\nwant: %q", string(msg), want)
	}
}

func TestCanonicalRequestTarget_PreservesEscapingAndRawQuery(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/v1.44/containers/a%2Fb/logs?tail=10&stdout=1", nil)
	if got, want := CanonicalRequestTarget(req.URL), "/v1.44/containers/a%2Fb/logs?tail=10&stdout=1"; got != want {
		t.Fatalf("canonical request target = %q, want %q", got, want)
	}
}

func TestBodyHashHex_EmptyBody(t *testing.T) {
	t.Parallel()
	h := BodyHashHex(nil)
	if h != emptyBodyHash {
		t.Errorf("empty body hash: got %q want %q", h, emptyBodyHash)
	}
	h2 := BodyHashHex([]byte{})
	if h2 != emptyBodyHash {
		t.Errorf("empty slice body hash: got %q want %q", h2, emptyBodyHash)
	}
}

func TestBodyHashHex_NonEmptyBody(t *testing.T) {
	t.Parallel()
	body := []byte(`{"test":true}`)
	h := BodyHashHex(body)
	// Verify it matches crypto/sha256 directly.
	raw := sha256.Sum256(body)
	want := hex.EncodeToString(raw[:])
	if h != want {
		t.Errorf("body hash: got %q want %q", h, want)
	}
}

// ---- VerifyRequest tests ---------------------------------------------------

func TestVerifyRequest_HappyPath(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
	nonce := randomNonce(t)
	tsUnix := time.Now().Unix()
	signRequest(t, req, nil, priv, pub, tsUnix, nonce)

	keyID, err := VerifyRequest(req, nil, reg, lru, 60)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if keyID == "" {
		t.Error("expected non-empty key ID")
	}
}

func TestVerifyRequest_QueryMutationInvalidatesSignature(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		signed  string
		mutated string
	}{
		{name: "add", signed: "/v1.44/containers/example", mutated: "force=1"},
		{name: "remove", signed: "/v1.44/containers/example?force=1", mutated: ""},
		{name: "change", signed: "/v1.44/containers/example?force=0", mutated: "force=1"},
		{name: "reorder", signed: "/v1.44/containers/example?a=1&b=2", mutated: "b=2&a=1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg, lru, pub, priv := testSetup(t)
			req := httptest.NewRequest(http.MethodDelete, tc.signed, nil)
			signRequest(t, req, nil, priv, pub, time.Now().Unix(), randomNonce(t))
			req.URL.RawQuery = tc.mutated

			_, err := VerifyRequest(req, nil, reg, lru, 60)
			if !errors.Is(err, ErrBadSignature) {
				t.Fatalf("expected query mutation to invalidate signature, got: %v", err)
			}
		})
	}
}

func TestVerifyRequest_Version2AcceptsExactQuery(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)
	req := httptest.NewRequest(http.MethodGet, "/v1.44/images/json?all=1&filters=%7B%7D", nil)
	signRequest(t, req, nil, priv, pub, time.Now().Unix(), randomNonce(t))

	if _, err := VerifyRequest(req, nil, reg, lru, 60); err != nil {
		t.Fatalf("expected exact version 2 query to verify: %v", err)
	}
}

func TestVerifyRequest_LegacyCompatibilityIsQueryFreeOnly(t *testing.T) {
	t.Parallel()
	t.Run("query-free request remains accepted", func(t *testing.T) {
		reg, lru, pub, priv := testSetup(t)
		req := httptest.NewRequest(http.MethodGet, "/v1.44/info", nil)
		signRequest(t, req, nil, priv, pub, time.Now().Unix(), randomNonce(t))
		req.Header.Del(HeaderSignatureVersion)
		if _, err := VerifyRequest(req, nil, reg, lru, 60); err != nil {
			t.Fatalf("expected legacy query-free signature to verify: %v", err)
		}
	})
	t.Run("query request requires version 2", func(t *testing.T) {
		reg, lru, pub, priv := testSetup(t)
		req := httptest.NewRequest(http.MethodGet, "/v1.44/images/json?all=1", nil)
		signRequest(t, req, nil, priv, pub, time.Now().Unix(), randomNonce(t))
		req.Header.Del(HeaderSignatureVersion)
		if _, err := VerifyRequest(req, nil, reg, lru, 60); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("expected legacy query signature rejection, got: %v", err)
		}
	})
}

func TestVerifyRequest_BadSignature(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
	nonce := randomNonce(t)
	tsUnix := time.Now().Unix()
	signRequest(t, req, nil, priv, pub, tsUnix, nonce)

	// Corrupt the signature.
	req.Header.Set(HeaderSignature, base64.RawURLEncoding.EncodeToString(make([]byte, 64)))

	_, err := VerifyRequest(req, nil, reg, lru, 60)
	if !errors.Is(err, ErrBadSignature) {
		t.Errorf("expected ErrBadSignature, got: %v", err)
	}
}

func TestVerifyRequest_SkewedTimestamp(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
	nonce := randomNonce(t)
	// 120 seconds in the past — exceeds 60s window.
	tsUnix := time.Now().Unix() - 120
	signRequest(t, req, nil, priv, pub, tsUnix, nonce)

	_, err := VerifyRequest(req, nil, reg, lru, 60)
	if !errors.Is(err, ErrTimestampSkew) {
		t.Errorf("expected ErrTimestampSkew, got: %v", err)
	}
}

func TestVerifyRequest_ExtremeTimestampSkew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		timestamp int64
		wantErr   bool
	}{
		{name: "extreme future", timestamp: 1 << 62, wantErr: true},
		{name: "extreme past", timestamp: -(1 << 62), wantErr: true},
		{name: "ordinary future", timestamp: time.Now().Unix() + 30},
		{name: "ordinary past", timestamp: time.Now().Unix() - 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reg, lru, pub, priv := testSetup(t)
			nonce := randomNonce(t)
			req := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
			signRequest(t, req, nil, priv, pub, tt.timestamp, nonce)

			_, err := VerifyRequest(req, nil, reg, lru, 60)
			if tt.wantErr {
				if !errors.Is(err, ErrTimestampSkew) {
					t.Fatalf("VerifyRequest error = %v, want ErrTimestampSkew", err)
				}
				if lru.Seen(nonce) {
					t.Fatal("timestamp-rejected nonce was admitted to replay cache")
				}
				return
			}
			if err != nil {
				t.Fatalf("VerifyRequest error = %v, want nil", err)
			}
		})
	}
}

// ---- Skew boundary tests (PW-6.5) ------------------------------------------
//
// These pin the literal boundaries: skew exactly equal to maxSkew, and one
// nanosecond past it, in both directions. Neither is reachable off the wall
// clock. The wire timestamp is a whole number of seconds, so a skew measured
// against time.Now() always carries the fraction of a second that
// time.Now().Unix() discarded at signing time, plus whatever scheduling delay
// falls between signing and verifying. "Exactly maxSkew" therefore lands a
// fraction short of the boundary and never tests it, and "one second outside"
// keeps only that same fraction of margin, so a sub-second stall on a loaded
// runner flips the result. Both tests below pin timeNow instead and derive the
// wire timestamp from it, which makes every row exact and the outcome
// independent of how long the test takes to run.

// pinVerifyClock pins the clock VerifyRequest measures skew against to at, and
// restores the previous clock when the test ends. timeNow is package-global,
// so a test calling this must NOT be t.Parallel(): the testing package runs
// every sequential test to completion before it resumes any parallel one,
// which is what keeps the override invisible to the rest of the package.
func pinVerifyClock(t *testing.T, at time.Time) {
	t.Helper()
	prev := timeNow
	timeNow = func() time.Time { return at }
	t.Cleanup(func() { timeNow = prev })
}

// skewFor splits a target skew into the two values that produce it exactly: the
// whole-second timestamp a client would put on the wire, and the instant the
// server clock must read. VerifyRequest computes skew as now - ts, so with
// ts = base - trunc(want) and now = base + (want - trunc(want)), the measured
// skew is want to the nanosecond, for either sign. base must be a whole second.
func skewFor(base time.Time, want time.Duration) (now time.Time, tsUnix int64) {
	whole := want.Truncate(time.Second) // rounds toward zero, so this works for want < 0
	return base.Add(want - whole), base.Unix() - int64(whole/time.Second)
}

// skewTestBase is an arbitrary whole second used as the pinned "now" origin.
var skewTestBase = time.Unix(1700000000, 0)

func TestVerifyRequest_SkewBoundaries(t *testing.T) {
	// Deliberately NOT t.Parallel(): pinVerifyClock swaps the package-global
	// timeNow, which every other VerifyRequest test reads.
	const maxSkewSeconds = 60
	const maxSkew = maxSkewSeconds * time.Second

	tests := []struct {
		name string
		// skew is the exact value VerifyRequest will measure. Positive means
		// the timestamp is in the past (client clock behind), negative means
		// the future (client clock ahead).
		skew    time.Duration
		wantErr bool
	}{
		{name: "past exactly at the window edge", skew: maxSkew, wantErr: false},
		{name: "past one nanosecond outside the window", skew: maxSkew + 1, wantErr: true},
		{name: "past a full second outside the window", skew: maxSkew + time.Second, wantErr: true},
		{name: "future exactly at the window edge", skew: -maxSkew, wantErr: false},
		{name: "future one nanosecond outside the window", skew: -maxSkew - 1, wantErr: true},
		{name: "future a full second outside the window", skew: -maxSkew - time.Second, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now, tsUnix := skewFor(skewTestBase, tt.skew)
			pinVerifyClock(t, now)

			reg, lru, pub, priv := testSetup(t)
			req := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
			nonce := randomNonce(t)
			signRequest(t, req, nil, priv, pub, tsUnix, nonce)

			_, err := VerifyRequest(req, nil, reg, lru, maxSkewSeconds)
			if tt.wantErr {
				if !errors.Is(err, ErrTimestampSkew) {
					t.Fatalf("skew %v: VerifyRequest error = %v, want ErrTimestampSkew", tt.skew, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("skew %v: VerifyRequest error = %v, want nil", tt.skew, err)
			}
		})
	}
}

// TestVerifyRequest_SkewWarnThreshold pins the "skew > 30s" clock-skew warning
// at its exact threshold, in both timestamp directions. The row at exactly 30s
// is the one that matters: it is the only skew that distinguishes ">" from
// ">=", and it is unreachable without a pinned clock. The future rows matter on
// their own too, as the only ones that exercise the "skew < 0 { skew = -skew }"
// sign flip verify.go applies before the warn check; a past timestamp never
// takes that branch.
func TestVerifyRequest_SkewWarnThreshold(t *testing.T) {
	// Deliberately NOT t.Parallel(): swaps the process-global slog default to
	// capture log output, which races with any other test in this package that
	// logs concurrently via package-level slog.* helpers, and pinVerifyClock
	// swaps the package-global timeNow on top of that.
	const maxSkewSeconds = 60
	const warnThreshold = 30 * time.Second

	tests := []struct {
		name     string
		skew     time.Duration // same convention as TestVerifyRequest_SkewBoundaries
		wantWarn bool
	}{
		{name: "past a full second under the warn threshold", skew: warnThreshold - time.Second, wantWarn: false},
		{name: "past exactly at the warn threshold", skew: warnThreshold, wantWarn: false},
		{name: "past one nanosecond over the warn threshold", skew: warnThreshold + 1, wantWarn: true},
		{name: "future exactly at the warn threshold", skew: -warnThreshold, wantWarn: false},
		{name: "future one nanosecond over the warn threshold", skew: -warnThreshold - 1, wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now, tsUnix := skewFor(skewTestBase, tt.skew)
			pinVerifyClock(t, now)

			reg, lru, pub, priv := testSetup(t)
			req := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
			nonce := randomNonce(t)
			signRequest(t, req, nil, priv, pub, tsUnix, nonce)

			logBuf := &bytes.Buffer{}
			oldLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
			defer slog.SetDefault(oldLogger)

			if _, err := VerifyRequest(req, nil, reg, lru, maxSkewSeconds); err != nil {
				t.Fatalf("skew %v: VerifyRequest error = %v, want nil", tt.skew, err)
			}

			gotWarn := strings.Contains(logBuf.String(), "clock skew warning")
			if gotWarn != tt.wantWarn {
				t.Errorf("skew %v: warn logged = %v, want %v (log: %s)", tt.skew, gotWarn, tt.wantWarn, logBuf.String())
			}
		})
	}
}

func TestVerifyRequest_ReplayedNonce(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)

	nonce := randomNonce(t)
	tsUnix := time.Now().Unix()

	// First request succeeds.
	req1 := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
	signRequest(t, req1, nil, priv, pub, tsUnix, nonce)
	if _, err := VerifyRequest(req1, nil, reg, lru, 60); err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	// Second request with same nonce must be rejected as replay.
	req2 := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
	signRequest(t, req2, nil, priv, pub, tsUnix, nonce)
	_, err := VerifyRequest(req2, nil, reg, lru, 60)
	if !errors.Is(err, ErrNonceReplay) {
		t.Errorf("expected ErrNonceReplay, got: %v", err)
	}
}

// TestVerifyRequest_NonceOutlivesTimestampWindow is a regression test for the
// nonce-replay window: a request signed with a clock that runs fast stays
// timestamp-valid until nearly 2×maxSkew after it was first seen (skew is
// checked symmetrically), so the nonce must be retained for that entire
// span. Before the fix, NonceLRU's TTL equalled maxSkew, so a nonce could be
// evicted while its signature was still valid, letting a captured
// byte-identical replay through.
func TestVerifyRequest_NonceOutlivesTimestampWindow(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)

	const maxSkew = 60

	// A client clock that is fast by nearly the full skew window: still
	// within maxSkew of "now", but at the future edge, so the signature
	// stays valid up to nearly 2*maxSkew after the nonce is first recorded.
	nonce := randomNonce(t)
	tsUnix := time.Now().Unix() + maxSkew - 1

	req := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
	signRequest(t, req, nil, priv, pub, tsUnix, nonce)
	if _, err := VerifyRequest(req, nil, reg, lru, maxSkew); err != nil {
		t.Fatalf("first verification failed: %v", err)
	}

	// Simulate the nonce having sat in the cache for longer than the OLD
	// (pre-fix) ttl of maxSkew seconds, but less than the current
	// 2*maxSkew ttl, then force an eviction pass. Under the old ttl this
	// entry would already be gone.
	lru.mu.Lock()
	lru.seen[nonce] = time.Now().Add(-(maxSkew + 30) * time.Second)
	lru.mu.Unlock()
	lru.evictExpired()

	// Replay the identical, still timestamp-valid request. It must be
	// rejected: the nonce must outlive the widest timestamp-valid window,
	// else an evicted nonce lets a still-valid signature replay.
	req2 := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
	signRequest(t, req2, nil, priv, pub, tsUnix, nonce)
	if _, err := VerifyRequest(req2, nil, reg, lru, maxSkew); !errors.Is(err, ErrNonceReplay) {
		t.Fatalf("expected ErrNonceReplay for replay within timestamp-valid window, got: %v", err)
	}
}

// TestVerifyRequest_NonceRecordedAtCapacity is a regression test for the
// fail-open capacity path: a full nonce cache accepted a fresh nonce and then
// did not record it, so the very request admitted at capacity could be
// captured and replayed for the rest of its timestamp window. A nonce that is
// accepted must be recorded, or it must not be accepted.
func TestVerifyRequest_NonceRecordedAtCapacity(t *testing.T) {
	t.Parallel()
	reg, _, pub, priv := testSetup(t)

	const capacity = 4
	lru := NewNonceLRU(capacity, 60)
	t.Cleanup(lru.Close)

	// Fill to capacity with entries that are past their TTL, the state a
	// busy agent sits in between cleanup ticks.
	for i := 0; i < capacity; i++ {
		if !lru.Add(fmt.Sprintf("filler%d", i)) {
			t.Fatalf("filler %d refused while filling the cache", i)
		}
	}
	lru.mu.Lock()
	stale := time.Now().Add(-2 * lru.ttl)
	for nonce := range lru.seen {
		lru.seen[nonce] = stale
	}
	lru.mu.Unlock()
	if lru.Len() != capacity {
		t.Fatalf("cache holds %d entries, want %d before the request under test", lru.Len(), capacity)
	}

	nonce := randomNonce(t)
	tsUnix := time.Now().Unix()
	req := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
	signRequest(t, req, nil, priv, pub, tsUnix, nonce)
	if _, err := VerifyRequest(req, nil, reg, lru, 60); err != nil {
		t.Fatalf("request admitted at capacity failed to verify: %v", err)
	}

	// Replay the identical, still timestamp-valid request. Accepting it is
	// exactly the hole the unrecorded-accept left open.
	replay := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
	signRequest(t, replay, nil, priv, pub, tsUnix, nonce)
	if _, err := VerifyRequest(replay, nil, reg, lru, 60); !errors.Is(err, ErrNonceReplay) {
		t.Fatalf("replay of a request accepted at cache capacity returned %v, want ErrNonceReplay", err)
	}
}

func TestVerifyRequest_UnknownKey(t *testing.T) {
	t.Parallel()
	reg, lru, _, _ := testSetup(t)

	// Generate a completely different keypair.
	pub2, priv2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
	nonce := randomNonce(t)
	tsUnix := time.Now().Unix()
	signRequest(t, req, nil, priv2, pub2, tsUnix, nonce)

	_, err = VerifyRequest(req, nil, reg, lru, 60)
	if !errors.Is(err, ErrUnknownKey) {
		t.Errorf("expected ErrUnknownKey, got: %v", err)
	}
}

func TestVerifyRequest_MissingHeaders(t *testing.T) {
	t.Parallel()
	reg, lru, _, _ := testSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/api/portwing/health", nil)
	// No signature headers at all.
	_, err := VerifyRequest(req, nil, reg, lru, 60)
	if !errors.Is(err, ErrMissingHeaders) {
		t.Errorf("expected ErrMissingHeaders, got: %v", err)
	}
}

func TestVerifyRequest_MissingIndividualHeaders(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)

	nonce := randomNonce(t)
	tsUnix := time.Now().Unix()

	// Build a base request.
	makeReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/path", nil)
		signRequest(t, req, nil, priv, pub, tsUnix, nonce)
		return req
	}

	tests := []struct {
		name   string
		remove string
	}{
		{"no key-id", HeaderKeyID},
		{"no timestamp", HeaderTimestamp},
		{"no nonce", HeaderNonce},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := makeReq()
			req.Header.Del(tc.remove)
			_, err := VerifyRequest(req, nil, reg, lru, 60)
			if !errors.Is(err, ErrMissingHeaders) {
				t.Errorf("expected ErrMissingHeaders, got: %v", err)
			}
		})
	}
}

func TestVerifyRequest_InvalidNonceLength(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	tsUnix := time.Now().Unix()
	signRequest(t, req, nil, priv, pub, tsUnix, "short") // only 5 chars
	req.Header.Set(HeaderNonce, "short")

	_, err := VerifyRequest(req, nil, reg, lru, 60)
	if !errors.Is(err, ErrInvalidNonce) {
		t.Errorf("expected ErrInvalidNonce, got: %v", err)
	}
}

func TestVerifyRequest_WithBody(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)

	body := []byte(`{"key":"value"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/portwing/containers", nil)
	nonce := randomNonce(t)
	tsUnix := time.Now().Unix()
	signRequest(t, req, body, priv, pub, tsUnix, nonce)

	keyID, err := VerifyRequest(req, body, reg, lru, 60)
	if err != nil {
		t.Fatalf("expected success with body, got: %v", err)
	}
	if keyID == "" {
		t.Error("expected non-empty key ID")
	}
}

func TestVerifyRequest_BodyMismatch(t *testing.T) {
	t.Parallel()
	reg, lru, pub, priv := testSetup(t)

	body := []byte(`{"key":"value"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/portwing/containers", nil)
	nonce := randomNonce(t)
	tsUnix := time.Now().Unix()
	signRequest(t, req, body, priv, pub, tsUnix, nonce)

	// Verify with a different body — signature should not match.
	_, err := VerifyRequest(req, []byte(`{"key":"other"}`), reg, lru, 60)
	if !errors.Is(err, ErrBadSignature) {
		t.Errorf("expected ErrBadSignature for body mismatch, got: %v", err)
	}
}

func TestReasonFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err    error
		reason string
	}{
		{ErrTimestampSkew, "timestamp-skew"},
		{ErrNonceReplay, "replay"},
		{ErrUnknownKey, "unknown-key"},
		{ErrBadSignature, "invalid-signature"},
		{ErrMissingHeaders, "invalid-signature"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			if got := ReasonFor(tc.err); got != tc.reason {
				t.Errorf("ReasonFor(%v) = %q, want %q", tc.err, got, tc.reason)
			}
		})
	}
}

// TestVerifyRequest_ConcurrentReplay fires N copies of the same signed
// request concurrently and asserts exactly one is accepted. This guards the
// atomic check-and-set in the nonce LRU: a Seen()-then-Add() pair that is not
// atomic would let two racing copies both pass.
func TestVerifyRequest_ConcurrentReplay(t *testing.T) {
	t.Parallel()

	registry, lru, pub, priv := testSetup(t)
	tsUnix := time.Now().Unix()
	nonce := randomNonce(t)

	const n = 16
	var wg sync.WaitGroup
	var accepted atomic.Int32

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/containers", nil)
			signRequest(t, req, nil, priv, pub, tsUnix, nonce)
			if _, err := VerifyRequest(req, nil, registry, lru, 60); err == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := accepted.Load(); got != 1 {
		t.Fatalf("expected exactly 1 accepted request, got %d", got)
	}
}
