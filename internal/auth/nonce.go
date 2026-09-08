package auth

import (
	"sync"
	"time"
)

// NonceLRU is an in-memory nonce cache that provides replay protection within
// the timestamp window. It is modelled on RateLimiter in
// internal/server/middleware.go: a Mutex-protected map with a background
// cleanup goroutine.
//
// Capacity is bounded to maxSize entries. At the cap the cache first evicts
// every nonce whose TTL has expired; if that frees nothing it rejects rather
// than admitting a nonce it cannot record. Accepting an unrecorded nonce made
// that one request replayable for the rest of the timestamp window, which is
// the whole thing this cache exists to prevent.
type NonceLRU struct {
	mu      sync.Mutex
	seen    map[string]time.Time // nonce → time first seen
	maxSize int
	// ttl is how long a nonce must be retained before it is safe to evict.
	// Invariant: ttl must be >= the widest span a signed timestamp can stay
	// valid for, measured from when the nonce was first recorded, or an
	// evicted nonce lets a still-valid signature replay. See NewNonceLRU.
	ttl  time.Duration
	done chan struct{}
}

// NewNonceLRU returns a NonceLRU with the given capacity. windowSeconds is
// the caller's clock-skew window (verify.go's maxSkewSeconds). The TTL is
// set to 2×windowSeconds, not windowSeconds itself: a request's timestamp
// stays signature-valid as long as it is within windowSeconds of "now" in
// EITHER direction, so a timestamp signed windowSeconds in the future is
// still accepted right up until 2×windowSeconds after the nonce was first
// recorded. A nonce must outlive that entire span, or a captured
// byte-identical replay can pass the timestamp check again after its nonce
// has already been evicted. A background goroutine starts immediately.
func NewNonceLRU(maxSize int, windowSeconds int) *NonceLRU {
	if maxSize <= 0 {
		maxSize = 10000
	}
	if windowSeconds <= 0 {
		windowSeconds = 60
	}
	lru := &NonceLRU{
		seen:    make(map[string]time.Time),
		maxSize: maxSize,
		ttl:     2 * time.Duration(windowSeconds) * time.Second,
		done:    make(chan struct{}),
	}
	go lru.cleanup()
	return lru
}

// Close stops the background cleanup goroutine. It is idempotent.
func (l *NonceLRU) Close() {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
}

// Add records the nonce if it has not been seen before and there is room for
// it. Returns true if the nonce was freshly added (not a replay), false if it
// has been seen before or could not be recorded.
//
// A false return at capacity is a rejection, not a replay verdict, and the
// caller must treat it as one: admitting a nonce without recording it leaves
// that exact request replayable until its timestamp falls out of the window.
// Only entries past their TTL are evicted to make room — every other tracked
// nonce is still inside its own replay window, so evicting one would hand
// back the same hole from the other end.
func (l *NonceLRU) Add(nonce string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, exists := l.seen[nonce]; exists {
		return false
	}

	if len(l.seen) >= l.maxSize {
		l.evictExpiredLocked(time.Now())
	}
	if len(l.seen) >= l.maxSize {
		return false
	}

	l.seen[nonce] = time.Now()
	return true
}

// Seen reports whether the nonce has been recorded in the cache.
func (l *NonceLRU) Seen(nonce string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, exists := l.seen[nonce]
	return exists
}

// Len returns the current number of tracked nonces.
func (l *NonceLRU) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen)
}

// cleanup runs every window/2 and removes nonces whose TTL has expired.
func (l *NonceLRU) cleanup() {
	// Cleanup interval: half the TTL, or at least 10 s.
	interval := l.ttl / 2
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			l.evictExpired()
		}
	}
}

// evictExpired removes every nonce whose recorded time is older than the
// TTL. Factored out of cleanup so tests can force an eviction pass
// deterministically instead of waiting on the ticker.
func (l *NonceLRU) evictExpired() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evictExpiredLocked(time.Now())
}

// evictExpiredLocked drops every nonce recorded more than ttl before now. The
// caller must hold l.mu. Add calls it to reclaim space at capacity rather than
// waiting for the cleanup ticker.
func (l *NonceLRU) evictExpiredLocked(now time.Time) {
	cutoff := now.Add(-l.ttl)
	for nonce, t := range l.seen {
		if t.Before(cutoff) {
			delete(l.seen, nonce)
		}
	}
}
