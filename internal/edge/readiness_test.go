package edge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDockerReadinessRejectsPingError(t *testing.T) {
	t.Parallel()
	c := &Client{dockerClient: &fakeDocker{doErr: errors.New("daemon unavailable")}}
	for range 2 {
		if c.dockerReady(context.Background()) {
			t.Fatal("failed Docker ping reported ready")
		}
	}
}

type readinessDocker struct {
	fakeDocker
	pings   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (d *readinessDocker) Do(ctx context.Context, _, _ string, _ io.Reader) (*http.Response, error) {
	if d.pings.Add(1) == 1 {
		close(d.entered)
	}
	select {
	case <-d.release:
		return mkResp(http.StatusOK, "", ""), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestDockerReadinessCollapsesConcurrentPings(t *testing.T) {
	t.Parallel()
	d := &readinessDocker{entered: make(chan struct{}), release: make(chan struct{})}
	c := &Client{dockerClient: d}
	var once sync.Once
	release := func() { once.Do(func() { close(d.release) }) }
	t.Cleanup(release)
	results := make(chan bool, 32)
	for range 32 {
		go func() { results <- c.dockerReady(context.Background()) }()
	}
	select {
	case <-d.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Docker ping never started")
	}
	release()
	for range 32 {
		select {
		case ready := <-results:
			if !ready {
				t.Error("readiness = false, want true")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("readiness request never finished")
		}
	}
	if got := d.pings.Load(); got != 1 {
		t.Fatalf("concurrent readiness requests made %d pings, want 1", got)
	}
}
