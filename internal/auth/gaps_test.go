package auth

// gaps_test.go — targeted tests to reach the remaining uncovered statements
// after the initial 95% run.

import (
	"testing"
	"time"
)

// ---- NonceLRU cleanup goroutine: ticker.C eviction branch ------------------
//
// The cleanup() goroutine uses a ticker whose minimum interval is 10 s
// (clamped by the production code). This test waits up to 12 s for at least
// one tick to fire, which evicts an already-expired entry. The test is NOT
// marked t.Parallel() so it does not inflate wall-clock time for the parallel
// batch, and it is skipped in short mode (-test.short).

func TestNonceLRU_CleanupTickerFires(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow cleanup ticker test in -short mode")
	}

	// Build the struct directly rather than via NewNonceLRU: Close() does not
	// wait for the constructor-started goroutine to exit, so reassigning
	// lru.done after Close() races with that goroutine's select. Constructing
	// by hand means the only cleanup goroutine is the one started below.
	lru := &NonceLRU{
		// Seed a stale entry (born 5 s ago, well past the 1 s TTL).
		seen:    map[string]time.Time{"stale": time.Now().Add(-5 * time.Second)},
		maxSize: 1000,
		ttl:     time.Second, // interval clamps to the 10 s floor
		done:    make(chan struct{}),
	}

	lru.order.PushBack("stale")

	// Start the cleanup goroutine. The ticker will fire after ~10 s.
	go lru.cleanup()

	// Poll until the stale entry is gone or 12 s elapse.
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		lru.mu.Lock()
		_, found := lru.seen["stale"]
		lru.mu.Unlock()
		if !found {
			lru.Close()
			return // entry was evicted — ticker.C branch exercised
		}
		time.Sleep(200 * time.Millisecond)
	}

	lru.Close()
	t.Error("cleanup goroutine did not evict expired entry within 12 s")
}
