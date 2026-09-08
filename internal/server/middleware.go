package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/codeswhat/portwing/internal/audit"
	"github.com/codeswhat/portwing/internal/auth"
	applog "github.com/codeswhat/portwing/internal/log"
	"github.com/codeswhat/portwing/internal/metrics"
)

// tokenVerifier is the interface used by the authentication middleware to verify a presented
// token. It abstracts over plain-text and Argon2id verification.
type tokenVerifier interface {
	Verify(token string) bool
}

type capacityTokenVerifier interface {
	VerifyWithCapacity(token string) (valid bool, attempted bool)
}

func verifyTokenWithCapacity(verifier tokenVerifier, token string) (bool, bool) {
	if bounded, ok := verifier.(capacityTokenVerifier); ok {
		return bounded.VerifyWithCapacity(token)
	}
	return verifier.Verify(token), true
}

// rawTokenVerifier performs timing-safe comparison against a plain-text token.
type rawTokenVerifier struct {
	digest [sha256.Size]byte
}

func newRawTokenVerifier(token string) *rawTokenVerifier {
	return &rawTokenVerifier{digest: sha256.Sum256([]byte(token))}
}

func (v *rawTokenVerifier) Verify(token string) bool {
	presented := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(presented[:], v.digest[:]) == 1
}

const (
	// Header names only; these are not secret values.
	// #nosec G101 -- header name constant; not a credential.
	headerPortwingToken = "X-Portwing-Token"
	// #nosec G101 -- header name constant; not a credential.
	headerDrydockAgentSecret     = "X-Dd-Agent-Secret"
	defaultMaxEnrollmentInFlight = 32
)

// RateLimiter tracks failed authentication attempts by IP address and blocks
// IPs that exceed the failure threshold within a rolling window.
type RateLimiter struct {
	mu       sync.Mutex
	attempts map[string]*ipAttempts
	abuse    map[string]*ipAttempts
	maxFails int
	window   time.Duration
	maxIPs   int
	// maxInFlight bounds concurrent credential checks from one source before
	// any expensive verifier work begins.
	maxInFlight int
	// maxEnrollmentInFlight bounds unauthenticated enrollment handlers across
	// all sources, including addresses that cannot be added to attempts.
	maxEnrollmentInFlight int
	enrollmentInFlight    int

	trustedProxies []*net.IPNet

	done chan struct{}
}

type ipAttempts struct {
	count     int
	firstFail time.Time
	inFlight  int
}

// NewRateLimiter returns a RateLimiter that allows 10 failures per IP within
// a one-minute window. A background goroutine prunes expired entries every
// five minutes.
func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{
		attempts:              make(map[string]*ipAttempts),
		abuse:                 make(map[string]*ipAttempts),
		maxFails:              10,
		window:                time.Minute,
		maxIPs:                10000,
		maxInFlight:           2,
		maxEnrollmentInFlight: defaultMaxEnrollmentInFlight,
		done:                  make(chan struct{}),
	}
	go rl.cleanup()
	return rl
}

// Stop terminates the background cleanup goroutine. It is idempotent.
func (rl *RateLimiter) Stop() {
	select {
	case <-rl.done:
	default:
		close(rl.done)
	}
}

// cleanup runs every 5 minutes and removes entries whose window has expired.
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-rl.done:
			return
		case <-ticker.C:
			rl.sweepExpired(time.Now())
		}
	}
}

// sweepExpired drops entries whose window closed before now. It is split out of
// cleanup so the pruning rules can be exercised without waiting on the ticker.
// In-flight entries are kept regardless of age: their firstFail is only written
// once the check completes, so evicting one would lose the concurrency count.
func (rl *RateLimiter) sweepExpired(now time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	for ip, a := range rl.attempts {
		if a.inFlight == 0 && !a.firstFail.IsZero() && now.Sub(a.firstFail) > rl.window {
			delete(rl.attempts, ip)
		}
	}
	for ip, a := range rl.abuse {
		if now.Sub(a.firstFail) > rl.window {
			delete(rl.abuse, ip)
		}
	}
}

func (rl *RateLimiter) isAbuseRateLimited(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	a, ok := rl.abuse[ip]
	if !ok {
		return false
	}
	if time.Since(a.firstFail) > rl.window {
		delete(rl.abuse, ip)
		return false
	}
	return a.count >= rl.maxFails
}

func (rl *RateLimiter) recordAbuse(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.abuse == nil {
		rl.abuse = make(map[string]*ipAttempts)
	}
	a, ok := rl.abuse[ip]
	if !ok {
		if len(rl.abuse) >= rl.maxIPs {
			return
		}
		rl.abuse[ip] = &ipAttempts{count: 1, firstFail: time.Now()}
		return
	}
	if time.Since(a.firstFail) > rl.window {
		a.count = 1
		a.firstFail = time.Now()
		return
	}
	a.count++
}

// IsRateLimited returns true if the given IP has exceeded the failure threshold
// within the current window.
func (rl *RateLimiter) IsRateLimited(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	a, ok := rl.attempts[ip]
	if !ok {
		return false
	}

	if a.firstFail.IsZero() {
		return false
	}

	// If the window has expired, remove the entry and allow through when no
	// verification currently holds a reservation.
	if time.Since(a.firstFail) > rl.window {
		if a.inFlight == 0 {
			delete(rl.attempts, ip)
		} else {
			a.count = 0
			a.firstFail = time.Time{}
		}
		return false
	}

	return a.count >= rl.maxFails
}

// tryBeginAuth atomically reserves one per-IP verification slot. It combines
// the failure-limit check with in-flight accounting so a concurrent cold-start
// burst cannot race every request past a completed-failure counter.
func (rl *RateLimiter) tryBeginAuth(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.tryBeginAuthLocked(ip)
}

func (rl *RateLimiter) tryBeginAuthLocked(ip string) bool {
	a, ok := rl.attempts[ip]
	if ok && !a.firstFail.IsZero() && time.Since(a.firstFail) > rl.window {
		a.count = 0
		a.firstFail = time.Time{}
	}
	if !ok {
		if len(rl.attempts) >= rl.maxIPs {
			// Preserve the existing fail-open-for-tracking policy at the table
			// capacity. Expensive Argon2 verification remains protected by its
			// agent-wide semaphore even when this IP cannot be reserved.
			return true
		}
		a = &ipAttempts{}
		rl.attempts[ip] = a
	}

	maxInFlight := rl.maxInFlight
	if maxInFlight <= 0 {
		maxInFlight = 2
	}
	if a.count >= rl.maxFails || a.inFlight >= maxInFlight {
		return false
	}
	a.inFlight++
	return true
}

// finishAuth releases a verification reservation and records a failed check.
// Successful checks do not consume the rolling failure budget.
func (rl *RateLimiter) finishAuth(ip string, success bool) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.finishAuthLocked(ip, success)
}

func (rl *RateLimiter) finishAuthLocked(ip string, success bool) {
	a, ok := rl.attempts[ip]
	if !ok {
		return
	}
	if a.inFlight > 0 {
		a.inFlight--
	}
	if success {
		if a.count == 0 && a.inFlight == 0 {
			delete(rl.attempts, ip)
		}
		return
	}

	now := time.Now()
	if a.firstFail.IsZero() || now.Sub(a.firstFail) > rl.window {
		a.count = 1
		a.firstFail = now
		return
	}
	a.count++
}

func (rl *RateLimiter) tryBeginEnrollment(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	maxInFlight := rl.maxEnrollmentInFlight
	if maxInFlight <= 0 {
		maxInFlight = defaultMaxEnrollmentInFlight
	}
	if rl.enrollmentInFlight >= maxInFlight || !rl.tryBeginAuthLocked(ip) {
		return false
	}
	rl.enrollmentInFlight++
	return true
}

func (rl *RateLimiter) finishEnrollment(ip string, success bool) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.enrollmentInFlight > 0 {
		rl.enrollmentInFlight--
	}
	rl.finishAuthLocked(ip, success)
}

// RecordFailure records a failed authentication attempt for the given IP.
// If the total number of tracked IPs exceeds the limit, the failure is silently
// dropped to prevent memory exhaustion.
func (rl *RateLimiter) RecordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if len(rl.attempts) >= rl.maxIPs {
		return
	}

	a, ok := rl.attempts[ip]
	if !ok {
		rl.attempts[ip] = &ipAttempts{count: 1, firstFail: time.Now()}
		return
	}

	// If the window has expired, start a new one.
	if time.Since(a.firstFail) > rl.window {
		a.count = 1
		a.firstFail = time.Now()
		return
	}

	a.count++
}

// rateLimitOnly wraps a handler with rate limiting (by IP) but no auth check.
// This is used for the enrollment endpoint which does its own credential
// check. Authentication failures and malformed-body abuse are tracked in
// distinct rolling windows.
// reg receives request counters and duration histograms; it may be nil.
func (rl *RateLimiter) rateLimitOnly(next http.Handler, reg *metrics.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		clientIP := rl.clientIP(r)
		if rl.isAbuseRateLimited(clientIP) || !rl.tryBeginEnrollment(clientIP) {
			if reg != nil {
				reg.IncRequest(r.Method, http.StatusTooManyRequests)
				reg.IncRateLimited()
			}
			rejectUnadmitted(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		rw := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		defer func() {
			rl.finishEnrollment(clientIP, rw.code != http.StatusUnauthorized)
		}()
		if reg != nil {
			reg.IncInFlight()
			defer reg.DecInFlight()
		}
		next.ServeHTTP(rw, r)
		switch rw.code {
		case http.StatusBadRequest, http.StatusRequestTimeout, http.StatusRequestEntityTooLarge:
			rl.recordAbuse(clientIP)
		}
		if reg != nil {
			reg.IncRequest(r.Method, rw.code)
			reg.ObserveRequestDuration(time.Since(start).Seconds())
		}
	})
}

// Ed25519Config holds the optional Ed25519 verifier parameters for
// AuthMiddlewareWithEd25519. When Registry is nil the Ed25519 path is skipped.
type Ed25519Config struct {
	Registry       *auth.KeyRegistry
	Nonces         *auth.NonceLRU
	MaxSkewSeconds int
}

// authBodyReadDeadline bounds how long the Ed25519 auth path may block
// reading the (MaxBytesReader-capped) request body. This read happens before
// tryBeginAuth's per-IP concurrency gate, so without a deadline an
// unauthenticated caller could drip a signature-headed body slowly and pin
// a goroutine indefinitely, exhausting fds/goroutines with no credentials.
// Matches ReadHeaderTimeout in http.go: by the time headers (including the
// signature headers) have already arrived, reading up to 1 MiB of body
// should comfortably finish within the same 10 s budget.
// A var, not a const, so tests can shrink it rather than waiting out the
// real deadline.
var authBodyReadDeadline = 10 * time.Second

// isDeadlineExceeded reports whether err was caused by the read deadline set
// via http.ResponseController.SetReadDeadline, as opposed to some other I/O
// error (e.g. a reset connection) or the MaxBytesReader size cap.
func isDeadlineExceeded(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// rejectUnadmitted answers a request that was turned away before its body was
// read. net/http drains an unread request body once the handler returns
// (server.go's finishRequest, then transfer.go's body.Close), and that drain
// has no deadline of its own, so a declared-but-never-sent body pins the
// handler goroutine there indefinitely — exactly what an admission rejection
// exists to prevent. An already-elapsed read deadline aborts the drain on its
// first read from the connection, and "Connection: close" stops the connection
// being reused with a half-sent body still on the wire.
func rejectUnadmitted(w http.ResponseWriter, message string, status int) {
	// Best effort: a ResponseWriter with no underlying connection to bound
	// (httptest.ResponseRecorder, some wrappers) reports ErrNotSupported and
	// has no drain to abort either.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	w.Header().Set("Connection", "close")
	http.Error(w, message, status)
}

// AuthMiddlewareWithEd25519 validates raw or Argon2id credentials and supports
// an optional Ed25519 verification path. When a request carries X-Portwing-Signature,
// it is verified via Ed25519;
// otherwise the request falls through to the token verifier. Either path must
// succeed for the request to proceed.
//
// The body is consumed and buffered for signature verification; the downstream
// handler sees a fresh reader.
// reg receives request/auth/rate-limit counters and duration histograms;
// it may be nil (metrics are skipped in that case).
func (rl *RateLimiter) AuthMiddlewareWithEd25519(
	verifier tokenVerifier,
	ed Ed25519Config,
	auditor *audit.Logger,
	reg *metrics.Registry,
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// No authentication configured - pass through.
		if verifier == nil && ed.Registry == nil {
			rw := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
			if reg != nil {
				reg.IncInFlight()
				defer reg.DecInFlight()
			}
			next.ServeHTTP(rw, r)
			auditor.APIRequest("", r.Method, r.URL.Path, audit.OutcomeAllowed, rw.code, ms(start))
			if reg != nil {
				reg.IncRequest(r.Method, rw.code)
				reg.ObserveRequestDuration(time.Since(start).Seconds())
			}
			return
		}

		clientIP := rl.clientIP(r)

		if rl.IsRateLimited(clientIP) {
			auditor.RateLimited(clientIP, r.Method, r.URL.Path)
			if reg != nil {
				reg.IncRequest(r.Method, http.StatusTooManyRequests)
				reg.IncRateLimited()
			}
			rejectUnadmitted(w, "too many failed attempts", http.StatusTooManyRequests)
			return
		}

		// If Ed25519 is configured and the request carries a signature, use
		// that path exclusively. Reading the body first is required because
		// VerifyRequest needs it for the canonical message.
		if ed.Registry != nil && auth.HasSignature(r.Header) {
			// Reserve the per-IP verification slot BEFORE touching the body.
			// The read below is the attacker-controlled part of this path:
			// anyone can attach signature headers and then drip up to 1 MiB
			// for authBodyReadDeadline without holding a credential, so
			// reading first let N concurrent slow bodies from one IP occupy
			// N goroutines and N connections while maxInFlight was still
			// showing zero in use.
			if !rl.tryBeginAuth(clientIP) {
				auditor.RateLimited(clientIP, r.Method, r.URL.Path)
				if reg != nil {
					reg.IncRequest(r.Method, http.StatusTooManyRequests)
					reg.IncRateLimited()
				}
				rejectUnadmitted(w, "too many failed attempts", http.StatusTooManyRequests)
				return
			}
			// The slot covers the body read and the signature check only, not
			// the downstream handler: holding it through next.ServeHTTP would
			// cap one client's concurrent authenticated requests at
			// maxInFlight. releaseAuthSlot is idempotent, so the deferred call
			// covers every early return (and a panic) while the success path
			// still releases explicitly before next.ServeHTTP runs.
			authSlotReleased := false
			releaseAuthSlot := func(success bool) {
				if authSlotReleased {
					return
				}
				authSlotReleased = true
				rl.finishAuth(clientIP, success)
			}
			// A body that never arrives is not a credential failure, so the
			// fallback release does not spend the rolling failure budget.
			defer releaseAuthSlot(true)

			// Buffer the body (capped at 1 MB; MaxBytesReader also closes the
			// connection on overflow, preventing slow-drip memory exhaustion).
			var body []byte
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

				// Deadline the read itself, not just its size: a slow drip
				// under the 1 MiB cap would otherwise pin this goroutine for
				// as long as it liked, holding the slot reserved above.
				rc := http.NewResponseController(w)
				if err := rc.SetReadDeadline(time.Now().Add(authBodyReadDeadline)); err != nil {
					slog.Warn("setting auth body read deadline", "error", err)
				}
				var err error
				body, err = io.ReadAll(r.Body)
				if closeErr := r.Body.Close(); closeErr != nil {
					slog.Warn("closing request body", "error", closeErr)
				}
				// Clear the deadline so it doesn't linger onto downstream
				// handlers, some of which are deliberately unbounded
				// streaming endpoints (logs, events, stats, exec).
				if clearErr := rc.SetReadDeadline(time.Time{}); clearErr != nil {
					slog.Warn("clearing auth body read deadline", "error", clearErr)
				}
				if err != nil {
					if isDeadlineExceeded(err) {
						http.Error(w, "request body read timed out", http.StatusRequestTimeout)
						return
					}
					http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
					return
				}
				// Restore for downstream handlers.
				r.Body = io.NopCloser(bytes.NewReader(body))
			}

			skew := ed.MaxSkewSeconds
			if skew <= 0 {
				skew = 60
			}
			keyID, err := auth.VerifyRequest(r, body, ed.Registry, ed.Nonces, skew)
			releaseAuthSlot(err == nil)
			if err != nil {
				reason := auth.ReasonFor(err)
				slog.Warn("ed25519 authentication failed",
					"ip", applog.Sanitize(clientIP), "reason", applog.Sanitize(reason))
				auditor.AuthFailure(clientIP, r.Method, r.URL.Path)
				if reg != nil {
					reg.IncRequest(r.Method, http.StatusUnauthorized)
					reg.IncAuthFailure(reason)
				}
				setAuthReason(w.Header(), reason)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			slog.Debug("ed25519 authentication succeeded", "key_id", applog.Sanitize(keyID), "ip", applog.Sanitize(clientIP))
			rw := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
			if reg != nil {
				reg.IncInFlight()
				defer reg.DecInFlight()
			}
			next.ServeHTTP(rw, r)
			auditor.APIRequest(clientIP, r.Method, r.URL.Path, audit.OutcomeAllowed, rw.code, ms(start))
			if reg != nil {
				reg.IncRequest(r.Method, rw.code)
				reg.ObserveRequestDuration(time.Since(start).Seconds())
			}
			return
		}

		// Fall through to token verifier.
		if verifier == nil {
			// Ed25519 was configured but request had no signature, and there
			// is no token verifier — authentication required but none presented.
			rl.RecordFailure(clientIP)
			slog.Warn("authentication failed: no credentials presented", "ip", applog.Sanitize(clientIP))
			auditor.AuthFailure(clientIP, r.Method, r.URL.Path)
			if reg != nil {
				reg.IncRequest(r.Method, http.StatusUnauthorized)
				reg.IncAuthFailure("no_credentials")
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		provided := agentToken(r)
		if !rl.tryBeginAuth(clientIP) {
			auditor.RateLimited(clientIP, r.Method, r.URL.Path)
			if reg != nil {
				reg.IncRequest(r.Method, http.StatusTooManyRequests)
				reg.IncRateLimited()
			}
			rejectUnadmitted(w, "too many failed attempts", http.StatusTooManyRequests)
			return
		}
		valid, attempted := verifyTokenWithCapacity(verifier, provided)
		if !attempted {
			rl.finishAuth(clientIP, true)
			auditor.RateLimited(clientIP, r.Method, r.URL.Path)
			if reg != nil {
				reg.IncRequest(r.Method, http.StatusTooManyRequests)
				reg.IncRateLimited()
			}
			http.Error(w, "authentication verification capacity exceeded", http.StatusTooManyRequests)
			return
		}
		rl.finishAuth(clientIP, valid)

		if !valid {
			slog.Warn("authentication failed", "ip", applog.Sanitize(clientIP))
			auditor.AuthFailure(clientIP, r.Method, r.URL.Path)
			if reg != nil {
				reg.IncRequest(r.Method, http.StatusUnauthorized)
				reg.IncAuthFailure("bad_token")
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		rw := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		if reg != nil {
			reg.IncInFlight()
			defer reg.DecInFlight()
		}
		next.ServeHTTP(rw, r)
		auditor.APIRequest(clientIP, r.Method, r.URL.Path, audit.OutcomeAllowed, rw.code, ms(start))
		if reg != nil {
			reg.IncRequest(r.Method, rw.code)
			reg.ObserveRequestDuration(time.Since(start).Seconds())
		}
	})
}

// statusRecorder wraps ResponseWriter to capture the status code. It must
// forward Flush and Hijack so SSE streaming and Docker exec/attach hijacking
// keep working through the middleware chain.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.code = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := sr.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Unwrap supports http.ResponseController.
func (sr *statusRecorder) Unwrap() http.ResponseWriter {
	return sr.ResponseWriter
}

// ms returns elapsed milliseconds since start as a float64.
func ms(start time.Time) float64 {
	return float64(time.Since(start).Nanoseconds()) / 1e6
}

// RecoveryMiddleware catches panics in downstream handlers, logs the stack
// trace, and returns a 500 Internal Server Error.
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				if err == http.ErrAbortHandler { //nolint:errorlint // net/http recognizes only the exact abort sentinel.
					panic(err)
				}
				stack := debug.Stack()
				slog.Error("panic recovered",
					"error", applog.Sanitize(fmt.Sprintf("%v", err)),
					"stack", string(stack),
					"method", applog.Sanitize(r.Method),
					"path", applog.Sanitize(r.URL.Path),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts a token from the Authorization header if it uses the
// Bearer scheme.
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func agentToken(r *http.Request) string {
	if token := bearerToken(r); token != "" {
		return token
	}
	if token := r.Header.Get(headerPortwingToken); token != "" {
		return token
	}
	return r.Header.Get(headerDrydockAgentSecret)
}

func setAuthReason(h http.Header, reason string) {
	h.Set(auth.HeaderReason, reason)
}

// SetTrustedProxies configures the CIDR ranges whose forwarding headers are
// trusted when extracting the client IP. It must be called before the server
// starts handling requests. With no trusted proxies (the default),
// X-Forwarded-For and X-Real-IP are ignored so spoofed headers cannot evade
// rate limiting.
func (rl *RateLimiter) SetTrustedProxies(nets []*net.IPNet) {
	rl.trustedProxies = nets
}

// ParseTrustedProxies parses CIDR strings into networks for
// SetTrustedProxies. Bare IPs are treated as /32 (or /128 for IPv6).
func ParseTrustedProxies(entries []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(entries))
	for _, e := range entries {
		s := strings.TrimSpace(e)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			if ip := net.ParseIP(s); ip != nil {
				if ip.To4() != nil {
					s += "/32"
				} else {
					s += "/128"
				}
			}
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: %w", e, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// clientIP extracts the client IP for rate-limiting purposes. Forwarding
// headers are only consulted when the direct peer is a trusted proxy; the
// X-Forwarded-For chain is then walked right to left and the first hop that
// is not itself a trusted proxy wins.
//
// Every value taken from a forwarding header must parse as an IP address. The
// return value keys the auth rate limiter and lands in audit records as the
// actor, so accepting an arbitrary header string would let a caller behind a
// trusted proxy mint a fresh limiter bucket per request — defeating the
// failed-attempt throttle it is supposed to enforce — and write arbitrary
// actor values into the audit trail.
func (rl *RateLimiter) clientIP(r *http.Request) string {
	remote := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}

	remoteIP := net.ParseIP(remote)
	if remoteIP == nil || !ipInNets(remoteIP, rl.trustedProxies) {
		return remote
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		hops := strings.Split(xff, ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop := strings.TrimSpace(hops[i])
			if ip := net.ParseIP(hop); ip != nil && !ipInNets(ip, rl.trustedProxies) {
				return hop
			}
		}
	}

	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		if ip := net.ParseIP(xri); ip != nil && !ipInNets(ip, rl.trustedProxies) {
			return xri
		}
	}

	return remote
}

func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
