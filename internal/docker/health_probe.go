package docker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// HealthProbe collapses concurrent readiness checks onto a single bounded
// Docker ping and reuses its result for ttl. N simultaneous
// readiness requests therefore cost one ping and one Docker connection, not N,
// and a caller never waits longer than timeout for one.
type HealthProbe struct {
	// Timeout and TTL must be set before the first Check. Zero uses 2s and 1s.
	Timeout time.Duration
	TTL     time.Duration
	mu      sync.Mutex
	// inflight is non-nil while a ping is running and is closed when it
	// finishes, so late arrivals wait for that ping's result instead of
	// starting their own.
	inflight chan struct{}
	at       time.Time
	err      error
	valid    bool
}

// errReadinessPingIncomplete is cached when a ping did not return normally,
// which today means it panicked. Recording it keeps a panic from being cached
// as a healthy result by the deferred publish, whose err is still nil at that
// point.
var errReadinessPingIncomplete = errors.New("readiness ping did not complete")

// Check returns the Docker reachability result, pinging at most once per
// ttl across all concurrent callers. ping is called with a
// context bounded by timeout.
func (p *HealthProbe) Check(ctx context.Context, ping func(context.Context) error) (err error) {
	timeout, ttl := p.Timeout, p.TTL
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	p.mu.Lock()
	if p.valid && time.Since(p.at) < ttl {
		cached := p.err
		p.mu.Unlock()
		return cached
	}
	if wait := p.inflight; wait != nil {
		p.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		p.mu.Lock()
		cached := p.err
		p.mu.Unlock()
		return cached
	}
	done := make(chan struct{})
	p.inflight = done
	p.mu.Unlock()

	// Publish and hand off from a defer so an unwinding ping cannot leave
	// inflight set: every later readiness request would block on a channel
	// nothing ever closes, turning one panicking request into a permanently
	// wedged readiness endpoint. RecoveryMiddleware turns the panic itself
	// into a 500 for the one request that caused it.
	completed := false
	defer func() {
		p.mu.Lock()
		if !completed {
			err = errReadinessPingIncomplete
		}
		p.err = err
		p.at = time.Now()
		p.valid = true
		p.inflight = nil
		p.mu.Unlock()
		close(done)
	}()

	// Detached from the caller's context: everyone waiting on this ping would
	// otherwise be cancelled by whichever client happened to start it hanging
	// up first. The timeout is what bounds it.
	pingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	err = ping(pingCtx)
	completed = true
	return err
}
