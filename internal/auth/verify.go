package auth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	applog "github.com/codeswhat/portwing/internal/log"
)

// Header names used in Ed25519 request authentication.
const (
	HeaderKeyID     = "X-Portwing-Key-ID"
	HeaderTimestamp = "X-Portwing-Timestamp"
	HeaderNonce     = "X-Portwing-Nonce"
	HeaderSignature = "X-Portwing-Signature"
	// HeaderSignatureVersion selects the canonical request format. Version 2
	// signs the complete origin-form request target, including the raw query.
	HeaderSignatureVersion = "X-Portwing-Signature-Version"
	HeaderReason           = "X-Portwing-Reason"
	SignatureVersion2      = "2"
)

// timeNow is the clock VerifyRequest measures timestamp skew against. Tests pin
// it to a fixed whole second so they can construct a skew that lands exactly on
// a window or warn boundary. The wire timestamp is a whole number of seconds,
// so a skew read off the real wall clock always carries the sub-second
// remainder time.Now().Unix() discarded at signing time, which puts every
// measurement a fraction short of any whole-second target and makes a test that
// aims at one depend on how fast it runs. Production never reassigns it.
var timeNow = time.Now

// Sentinel errors returned by VerifyRequest. Callers can use errors.Is.
var (
	ErrMissingHeaders = errors.New("ed25519: missing required signature headers")
	ErrUnknownKey     = errors.New("ed25519: unknown key ID")
	ErrTimestampSkew  = errors.New("ed25519: timestamp outside allowed window")
	ErrNonceCapacity  = errors.New("ed25519: nonce cache at capacity")
	ErrNonceReplay    = errors.New("ed25519: nonce already seen (replay)")
	ErrBadSignature   = errors.New("ed25519: signature verification failed")
	ErrInvalidNonce   = errors.New("ed25519: nonce must be 32 hex characters")
	ErrInvalidSig     = errors.New("ed25519: signature is not valid base64url")
)

// ReasonFor maps a sentinel error to the value for the X-Portwing-Reason
// header returned with 401 responses.
func ReasonFor(err error) string {
	switch {
	case errors.Is(err, ErrTimestampSkew):
		return "timestamp-skew"
	case errors.Is(err, ErrNonceCapacity):
		return "nonce-capacity"
	case errors.Is(err, ErrNonceReplay):
		return "replay"
	case errors.Is(err, ErrUnknownKey):
		return "unknown-key"
	default:
		return "invalid-signature"
	}
}

// HasSignature reports whether the request carries the Portwing signature
// header.
func HasSignature(h http.Header) bool {
	return h.Get(HeaderSignature) != ""
}

// CanonicalMessage constructs the canonical byte string that is signed and
// verified for every authenticated request. In signature version 2, target is
// the complete origin-form request target returned by CanonicalRequestTarget.
// The format is:
//
//	METHOD\nREQUEST-TARGET\nbody-sha256-hex\nunix-timestamp\nnonce
//
// For an empty body, bodyHashHex must be the full SHA-256 of the empty string:
//
//	e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
func CanonicalMessage(method, target, bodyHashHex string, timestampUnix int64, nonce string) []byte {
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%d\n%s",
		method, target, bodyHashHex, timestampUnix, nonce))
}

// CanonicalRequestTarget returns the exact origin-form target covered by a
// version 2 HTTP signature: escaped path plus the unmodified raw query.
func CanonicalRequestTarget(u *url.URL) string {
	target := u.EscapedPath()
	if target == "" {
		target = "/"
	}
	if u.ForceQuery || u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	return target
}

// emptyBodyHash is the SHA-256 hex digest of the empty string, used when the
// request body is absent or has zero length.
const emptyBodyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// BodyHashHex computes the hex-encoded SHA-256 of b.
// If b is nil or zero-length, it returns the canonical empty-body hash.
func BodyHashHex(b []byte) string {
	if len(b) == 0 {
		return emptyBodyHash
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// VerifyRequest verifies an incoming HTTP request's Ed25519 signature.
//
// It reads the four Ed25519 headers (X-Portwing-Key-ID, X-Portwing-Timestamp,
// X-Portwing-Nonce, X-Portwing-Signature) and:
//  1. Looks up the key in registry.
//  2. Checks the timestamp against maxSkew.
//  3. Checks the nonce LRU for replay.
//  4. Verifies the Ed25519 signature over the canonical message.
//  5. Records the nonce on success.
//
// body is the raw request body (already read by the caller). The caller is
// responsible for replacing r.Body with a new reader if needed.
//
// Returns the key ID string and nil on success. Returns ("", err) on failure.
func VerifyRequest(
	r *http.Request,
	body []byte,
	registry *KeyRegistry,
	lru *NonceLRU,
	maxSkewSeconds int,
) (keyID string, err error) {
	sigHeader := r.Header.Get(HeaderSignature)
	if sigHeader == "" {
		return "", ErrMissingHeaders
	}

	kidHeader := r.Header.Get(HeaderKeyID)
	tsHeader := r.Header.Get(HeaderTimestamp)
	nonceHeader := r.Header.Get(HeaderNonce)

	if kidHeader == "" || tsHeader == "" || nonceHeader == "" {
		return "", ErrMissingHeaders
	}

	// Validate nonce format: must be exactly 32 hex characters.
	if len(nonceHeader) != 32 {
		return "", ErrInvalidNonce
	}
	if _, err := hex.DecodeString(nonceHeader); err != nil {
		return "", ErrInvalidNonce
	}

	// Look up key.
	key, ok := registry.LookupByID(kidHeader)
	if !ok {
		return "", ErrUnknownKey
	}

	// Parse and check timestamp.
	tsUnix, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return "", ErrTimestampSkew
	}
	skew := timeNow().Sub(time.Unix(tsUnix, 0))
	maxSkew := time.Duration(maxSkewSeconds) * time.Second
	if skew < -maxSkew || skew > maxSkew {
		return "", ErrTimestampSkew
	}
	if skew < 0 {
		skew = -skew
	}

	// Warn on large but still-acceptable skew.
	if skew > 30*time.Second {
		slog.Warn("clock skew warning",
			"skew_seconds", int(skew.Seconds()),
			"key_id", applog.Sanitize(kidHeader))
	}

	// Replay check.
	if lru.Seen(nonceHeader) {
		return "", ErrNonceReplay
	}

	// Decode signature (base64url, no padding).
	sig, err := base64.RawURLEncoding.DecodeString(sigHeader)
	if err != nil {
		return "", ErrInvalidSig
	}

	// Build the canonical message. Legacy signatures without a version header
	// remain valid only for query-free requests; accepting them with a query
	// would preserve the query-tampering vulnerability fixed by version 2.
	bodyHash := BodyHashHex(body)
	var target string
	switch r.Header.Get(HeaderSignatureVersion) {
	case SignatureVersion2:
		target = CanonicalRequestTarget(r.URL)
	case "":
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			return "", ErrBadSignature
		}
		target = r.URL.Path
	default:
		return "", ErrBadSignature
	}
	msg := CanonicalMessage(r.Method, target, bodyHash, tsUnix, nonceHeader)

	// Verify signature.
	if !ed25519.Verify(ed25519.PublicKey(key.PubKey), msg, sig) {
		return "", ErrBadSignature
	}

	// Record the nonce after successful verification. Add is the
	// authoritative atomic check-and-set: if two copies of the same request
	// race past the Seen() pre-check above, only one wins here.
	if err := lru.Add(nonceHeader); err != nil {
		return "", err
	}

	return kidHeader, nil
}
