package server

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestParsePHCRoundTrip verifies that HashToken produces a PHC string that
// ParsePHC can parse and that Verify accepts the original token.
func TestParsePHCRoundTrip(t *testing.T) {
	t.Parallel()

	const token = "supersecret"
	phc, err := HashToken(token)
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}

	params, err := ParsePHC(phc)
	if err != nil {
		t.Fatalf("ParsePHC(%q): %v", phc, err)
	}

	if !params.Verify(token) {
		t.Fatal("Verify returned false for correct token")
	}
	if params.Verify("wrongtoken") {
		t.Fatal("Verify returned true for wrong token")
	}
}

// TestParsePHCRejectsWrongVersion ensures version != 19 is rejected.
func TestParsePHCRejectsWrongVersion(t *testing.T) {
	t.Parallel()

	phc := "$argon2id$v=18$m=19456,t=2,p=1$c29tZXNhbHQ$aGFzaA"
	if _, err := ParsePHC(phc); err == nil {
		t.Fatal("expected error for wrong version, got nil")
	}
}

// TestParsePHCRejectsBadAlgorithm ensures non-argon2id strings are rejected.
func TestParsePHCRejectsBadAlgorithm(t *testing.T) {
	t.Parallel()

	phc := "$argon2i$v=19$m=19456,t=2,p=1$c29tZXNhbHQ$aGFzaA"
	if _, err := ParsePHC(phc); err == nil {
		t.Fatal("expected error for wrong algorithm, got nil")
	}
}

// TestParsePHCRejectsBadBase64Salt ensures malformed salt base64 is rejected.
func TestParsePHCRejectsBadBase64Salt(t *testing.T) {
	t.Parallel()

	phc := "$argon2id$v=19$m=19456,t=2,p=1$not!valid!base64$aGFzaA"
	if _, err := ParsePHC(phc); err == nil {
		t.Fatal("expected error for bad base64 salt, got nil")
	}
}

// TestParsePHCRejectsBadBase64Hash ensures malformed hash base64 is rejected.
func TestParsePHCRejectsBadBase64Hash(t *testing.T) {
	t.Parallel()

	phc := "$argon2id$v=19$m=19456,t=2,p=1$c29tZXNhbHQ$not!valid!base64"
	if _, err := ParsePHC(phc); err == nil {
		t.Fatal("expected error for bad base64 hash, got nil")
	}
}

// TestParsePHCRejectsMalformedSegments checks that strings with the wrong
// number of $ segments are rejected.
func TestParsePHCRejectsMalformedSegments(t *testing.T) {
	t.Parallel()

	cases := []string{
		"$argon2id$v=19$m=19456,t=2,p=1$c29tZXNhbHQ", // missing hash
		"$argon2id$v=19$m=19456,t=2,p=1",             // missing salt+hash
		"",
		"notaphcstring",
	}
	for _, phc := range cases {
		if _, err := ParsePHC(phc); err == nil {
			t.Errorf("expected error for %q, got nil", phc)
		}
	}
}

// TestHashTokenParams verifies that the PHC string produced by HashToken
// uses OWASP-recommended parameters.
func TestHashTokenParams(t *testing.T) {
	t.Parallel()

	phc, err := HashToken("testtoken")
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}

	params, err := ParsePHC(phc)
	if err != nil {
		t.Fatalf("ParsePHC: %v", err)
	}

	if params.Memory != 19456 {
		t.Errorf("memory: got %d, want 19456", params.Memory)
	}
	if params.Time != 2 {
		t.Errorf("time: got %d, want 2", params.Time)
	}
	if params.Parallelism != 1 {
		t.Errorf("parallelism: got %d, want 1", params.Parallelism)
	}
	if len(params.Salt) != 16 {
		t.Errorf("salt length: got %d, want 16", len(params.Salt))
	}
	if len(params.Hash) != 32 {
		t.Errorf("hash length: got %d, want 32", len(params.Hash))
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$") {
		t.Errorf("PHC prefix mismatch: %q", phc)
	}
}

// countingVerifier wraps argon2Verifier and counts Argon2id calls via a seam.
// It intercepts at the params.Verify level by substituting a custom verifyFn.
type countingVerifier struct {
	inner     *argon2Verifier
	argoCalls atomic.Int64
}

func newCountingVerifier(phc string) (*countingVerifier, error) {
	params, err := ParsePHC(phc)
	if err != nil {
		return nil, err
	}
	return &countingVerifier{inner: newArgon2Verifier(params)}, nil
}

func (c *countingVerifier) Verify(token string) bool {
	// We detect if the slow path ran by checking whether the cache was empty
	// before the call. The argon2Verifier stores the sum atomically on first
	// success, so cacheOnce is nil before and non-nil after.
	before := c.inner.cacheOnce.Load()
	result := c.inner.Verify(token)
	after := c.inner.cacheOnce.Load()
	if before == nil && after != nil {
		// The slow Argon2id path ran and succeeded.
		c.argoCalls.Add(1)
	}
	return result
}

// TestArgon2VerifierSuccessCache verifies that the Argon2id derivation is only
// called once across multiple successful verifications; subsequent calls use
// the SHA-256 cache.
func TestArgon2VerifierSuccessCache(t *testing.T) {
	t.Parallel()

	const token = "cachetest"
	phc, err := HashToken(token)
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}

	cv, err := newCountingVerifier(phc)
	if err != nil {
		t.Fatalf("newCountingVerifier: %v", err)
	}

	// First call: should run Argon2id.
	if !cv.Verify(token) {
		t.Fatal("first Verify returned false")
	}
	if cv.argoCalls.Load() != 1 {
		t.Fatalf("expected 1 argon2 call after first success, got %d", cv.argoCalls.Load())
	}

	// Subsequent calls: should use the cache, not Argon2id.
	for i := 0; i < 5; i++ {
		if !cv.Verify(token) {
			t.Fatalf("Verify call %d returned false", i+2)
		}
	}
	if cv.argoCalls.Load() != 1 {
		t.Fatalf("expected argon2 call count to remain 1, got %d", cv.argoCalls.Load())
	}

	// Wrong token should return false and not update the cache count.
	if cv.Verify("wrongtoken") {
		t.Fatal("wrong token should not verify")
	}
}

func TestArgon2VerifierBoundsConcurrentSlowVerifications(t *testing.T) {
	const requests = 12
	release := make(chan struct{})
	var active atomic.Int64
	var maximum atomic.Int64
	v := &argon2Verifier{
		slowVerify: func(string) bool {
			current := active.Add(1)
			for {
				seen := maximum.Load()
				if current <= seen || maximum.CompareAndSwap(seen, current) {
					break
				}
			}
			<-release
			active.Add(-1)
			return false
		},
		slowSlots: make(chan struct{}, 2),
	}

	start := make(chan struct{})
	var rejected atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, attempted := v.VerifyWithCapacity("wrong")
			if !attempted {
				rejected.Add(1)
			}
		}()
	}
	close(start)

	deadline := time.After(time.Second)
	for maximum.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for Argon2 verification slots")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(25 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := maximum.Load(); got > 2 {
		t.Fatalf("maximum concurrent slow verifications = %d, want at most 2", got)
	}
	if rejected.Load() == 0 {
		t.Fatal("expected excess slow verifications to be rejected before allocation")
	}
}

// TestParsePHCRejectsBelowMinimumParams verifies parameters below the argon2
// library minimums are rejected at parse time instead of panicking at verify.
func TestParsePHCRejectsBelowMinimumParams(t *testing.T) {
	t.Parallel()

	phc, err := HashToken("sometoken")
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}

	cases := map[string]struct {
		old, new string
	}{
		"zero time":        {"t=2", "t=0"},
		"zero parallelism": {"p=1", "p=0"},
		"memory too low":   {"m=19456", "m=4"},
	}

	for name, c := range cases {
		bad := strings.Replace(phc, c.old, c.new, 1)
		if bad == phc {
			t.Fatalf("%s: replacement %q not found in PHC", name, c.old)
		}
		if _, err := ParsePHC(bad); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// TestParsePHCRejectsOverflowParams verifies numeric parameters must fit the
// types passed into argon2.IDKey.
func TestParsePHCRejectsOverflowParams(t *testing.T) {
	t.Parallel()

	phc, err := HashToken("sometoken")
	if err != nil {
		t.Fatalf("HashToken: %v", err)
	}

	cases := map[string]struct {
		old, new string
	}{
		"memory over uint32": {"m=19456", "m=4294986752"},
		"time over uint32":   {"t=2", "t=4294967298"},
	}

	for name, c := range cases {
		bad := strings.Replace(phc, c.old, c.new, 1)
		if bad == phc {
			t.Fatalf("%s: replacement %q not found in PHC", name, c.old)
		}
		if _, err := ParsePHC(bad); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// TestParsePHCAcceptsMemoryAtUint32Max verifies m is accepted at exactly the
// uint32 maximum (parseParams' "m" overflow check), killing the
// CONDITIONALS_BOUNDARY mutant that turns `v > uint64(^uint32(0))` into `>=`.
// argon2.go:101:9
func TestParsePHCAcceptsMemoryAtUint32Max(t *testing.T) {
	t.Parallel()

	const good = "$argon2id$v=19$m=4294967295,t=2,p=1$c29tZXNhbHQ$aGFzaA"

	p, err := ParsePHC(good)
	if err != nil {
		t.Fatalf("ParsePHC with m at uint32 max: %v", err)
	}
	if p.Memory != 4294967295 {
		t.Fatalf("Memory = %d, want 4294967295", p.Memory)
	}
}

// TestParsePHCAcceptsTimeAtUint32Max verifies t is accepted at exactly the
// uint32 maximum, killing the CONDITIONALS_BOUNDARY mutant on the "t" overflow
// check. argon2.go:106:9
func TestParsePHCAcceptsTimeAtUint32Max(t *testing.T) {
	t.Parallel()

	const good = "$argon2id$v=19$m=19456,t=4294967295,p=1$c29tZXNhbHQ$aGFzaA"

	p, err := ParsePHC(good)
	if err != nil {
		t.Fatalf("ParsePHC with t at uint32 max: %v", err)
	}
	if p.Time != 4294967295 {
		t.Fatalf("Time = %d, want 4294967295", p.Time)
	}
}

// TestParsePHCAcceptsParallelismAt255 verifies p is accepted at exactly 255,
// killing the CONDITIONALS_BOUNDARY mutant on the "p" overflow check
// (`v > 255` -> `v >= 255`). argon2.go:111:9
func TestParsePHCAcceptsParallelismAt255(t *testing.T) {
	t.Parallel()

	const good = "$argon2id$v=19$m=19456,t=2,p=255$c29tZXNhbHQ$aGFzaA"

	p, err := ParsePHC(good)
	if err != nil {
		t.Fatalf("ParsePHC with p at 255: %v", err)
	}
	if p.Parallelism != 255 {
		t.Fatalf("Parallelism = %d, want 255", p.Parallelism)
	}
}

// TestParsePHCAcceptsTimeAtMinimumOne verifies t=1 (the documented minimum) is
// accepted, killing the CONDITIONALS_BOUNDARY mutant that turns
// `p.Time < 1` into `p.Time <= 1`. argon2.go:122:12
func TestParsePHCAcceptsTimeAtMinimumOne(t *testing.T) {
	t.Parallel()

	const good = "$argon2id$v=19$m=19456,t=1,p=1$c29tZXNhbHQ$aGFzaA"

	p, err := ParsePHC(good)
	if err != nil {
		t.Fatalf("ParsePHC with t=1: %v", err)
	}
	if p.Time != 1 {
		t.Fatalf("Time = %d, want 1", p.Time)
	}
}

// TestParsePHCMemoryParallelismBoundary exercises the
// `p.Memory < 8*uint32(p.Parallelism)` check exactly at and just below the
// threshold. It kills two mutants at argon2.go:128:
//   - CONDITIONALS_BOUNDARY (`<` -> `<=`): memory==8*parallelism must succeed.
//   - ARITHMETIC_BASE (`*` -> `/`): memory=10,parallelism=2 must fail against
//     the real threshold (16) even though 10 >= 8/2 (4) under the mutated
//     division.
func TestParsePHCMemoryParallelismBoundary(t *testing.T) {
	t.Parallel()

	const salt = "c29tZXNhbHQ"
	const hash = "aGFzaA"

	// Exactly at the threshold (8*2=16): must be accepted.
	atThreshold := "$argon2id$v=19$m=16,t=1,p=2$" + salt + "$" + hash
	p, err := ParsePHC(atThreshold)
	if err != nil {
		t.Fatalf("ParsePHC at memory==8*parallelism threshold: %v", err)
	}
	if p.Memory != 16 || p.Parallelism != 2 {
		t.Fatalf("parsed params = %+v, want memory=16 parallelism=2", p)
	}

	// Below the real threshold (16) but above the mutated-division threshold
	// (8/2=4): must be rejected. A `*` -> `/` mutant would accept this.
	belowThreshold := "$argon2id$v=19$m=10,t=1,p=2$" + salt + "$" + hash
	if _, err := ParsePHC(belowThreshold); err == nil {
		t.Fatal("ParsePHC with memory=10, parallelism=2 (below real 8*parallelism threshold) accepted, want error")
	}
}
