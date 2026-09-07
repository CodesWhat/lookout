package metrics

import "time"

// WaitForContainerWaiters blocks until the collector's in-flight collection
// has at least n scrapes waiting on it, reporting whether it saw them before
// the timeout. It exists so a concurrency test can prove a second scrape
// joined the collection already running instead of timing the join and hoping,
// which would silently stop testing anything the moment the race went the
// other way.
func (c *ContainerCollector) WaitForContainerWaiters(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		waiters := 0
		if c.flight != nil {
			waiters = c.flight.waiters
		}
		c.mu.Unlock()
		if waiters >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}
