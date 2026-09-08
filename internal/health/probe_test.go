package health

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// probeInflight reports whether the probe still holds an unfinished flight.
func probeInflight(p *Probe) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight != nil
}

// TestReadinessProbeReleasesFlightOnPanic is a regression test for a wedge:
// the probe published its result and closed the in-flight channel on the
// normal return path only, so a ping that panicked left inflight set forever
// and every later readiness request blocked on a channel nothing would close.
// One panicking request would have taken the readiness endpoint down for the
// life of the process.
func TestReadinessProbeReleasesFlightOnPanic(t *testing.T) {
	probe := Probe{Timeout: 2 * time.Second, TTL: time.Millisecond}

	// A waiter that arrives while the panicking ping is running must be
	// released by it, not left blocked. Its own context is deliberately not
	// cancellable, so only the flight handoff can free it.
	entered := make(chan struct{})
	unblock := make(chan struct{})
	waiterDone := make(chan error, 1)

	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_ = probe.Check(context.Background(), func(context.Context) error {
			close(entered)
			<-unblock
			panic("docker ping exploded")
		})
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the panicking ping never started")
	}
	go func() { waiterDone <- probe.Check(context.Background(), func(context.Context) error { return nil }) }()
	// Give the waiter time to reach the in-flight wait. Too short only makes
	// this assertion weaker, never wrong.
	time.Sleep(50 * time.Millisecond)
	close(unblock)

	if got := <-panicked; got == nil {
		t.Fatal("the panic was swallowed; it must still reach RecoveryMiddleware")
	}
	select {
	case err := <-waiterDone:
		if err == nil {
			t.Fatal("waiter got a healthy result from a ping that panicked")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a caller waiting on the panicking ping was never released")
	}

	// The flight must be gone, or the next request wedges on the same channel.
	if probeInflight(&probe) {
		t.Fatal("the probe still holds an in-flight ping after the ping panicked")
	}

	// And behaviourally: once the cached result expires, a later request runs
	// its own ping and gets a real answer.
	time.Sleep(probe.TTL + 20*time.Millisecond)
	pinged := false
	if err := probe.Check(context.Background(), func(context.Context) error {
		pinged = true
		return nil
	}); err != nil {
		t.Fatalf("readiness check after a panicking ping returned %v, want nil", err)
	}
	if !pinged {
		t.Fatal("the readiness check after the TTL did not run its own ping")
	}
}

// TestReadinessProbeWaiterHonoursCanceledContext covers the waiter's
// cancellation branch: a readiness client queued behind an in-flight ping that
// hangs up must be released by its own context rather than waiting out the
// ping it never asked for. Cancelling before the call makes the select
// deterministic — the leader's flight channel is still open, so only ctx.Done
// is ready.
func TestReadinessProbeWaiterHonoursCanceledContext(t *testing.T) {
	t.Parallel()

	var probe Probe

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLeader := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseLeader)

	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- probe.Check(context.Background(), func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the leading ping never started")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := probe.Check(ctx, func(context.Context) error {
		t.Error("a waiter must not start its own ping while one is in flight")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter with a canceled context returned %v, want context.Canceled", err)
	}

	releaseLeader()
	select {
	case err := <-leaderDone:
		if err != nil {
			t.Fatalf("the leading ping returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the leading ping never finished")
	}
}
