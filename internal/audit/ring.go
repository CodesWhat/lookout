package audit

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Record holds a single structured audit event captured in the ring buffer.
// Field names match the stable on-wire JSON schema documented in the package comment.
type Record struct {
	Cursor     uint64    `json:"cursor"`
	Time       time.Time `json:"ts"`
	Event      string    `json:"event"`
	Actor      string    `json:"actor,omitempty"`
	Method     string    `json:"method,omitempty"`
	Path       string    `json:"path,omitempty"`
	Outcome    string    `json:"outcome,omitempty"`
	Status     int       `json:"status,omitempty"`
	DurationMs float64   `json:"duration_ms,omitempty"`
	Operation  string    `json:"operation,omitempty"`
	Stack      string    `json:"stack,omitempty"`
	Container  string    `json:"container,omitempty"`
	ExecID     string    `json:"exec_id,omitempty"`
	KeyID      string    `json:"key_id,omitempty"`
}

// ring is a fixed-capacity circular buffer of Records. It is safe for
// concurrent use via its embedded mutex.
type ring struct {
	mu       sync.Mutex
	buf      []Record
	head     int // index of the next write slot
	size     int // number of valid entries currently stored
	capacity int
	next     uint64 // cursor assigned to the next record
}

// newRing creates a ring buffer with the given capacity.
func newRing(capacity int) *ring {
	return &ring{
		buf:      make([]Record, capacity),
		capacity: capacity,
	}
}

// push appends r to the buffer, overwriting the oldest entry when full.
func (rb *ring) push(r Record) {
	r.Path = retainedDisplay(r.Path, 4096)
	r.Method = retainedDisplay(r.Method, 64)
	r.Actor = retainedDisplay(r.Actor, 256)
	rb.mu.Lock()
	rb.next++
	r.Cursor = rb.next
	rb.buf[rb.head] = r
	rb.head = (rb.head + 1) % rb.capacity
	if rb.size < rb.capacity {
		rb.size++
	}
	rb.mu.Unlock()
}

// retainedDisplay owns its storage so even a short request-line substring cannot
// keep the entire input allocation alive. The marker counts toward the byte cap.
func retainedDisplay(value string, limit int) string {
	if len(value) <= limit {
		return strings.Clone(value)
	}
	const marker = "[truncated]"
	end := limit - len(marker)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + marker
}

// recordsAfter returns records with a cursor greater than after, oldest-first,
// capped at limit. It also returns the retained cursor window so callers can
// detect when a requested cursor has already been overwritten.
func (rb *ring) recordsAfter(after uint64, limit int) ([]Record, uint64, uint64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.size == 0 {
		return []Record{}, 0, rb.next
	}

	oldestIndex := (rb.head - rb.size + rb.capacity) % rb.capacity
	oldestCursor := rb.buf[oldestIndex].Cursor
	latestCursor := rb.next
	out := make([]Record, 0, rb.size)
	for i := 0; i < rb.size; i++ {
		record := rb.buf[(oldestIndex+i)%rb.capacity]
		if record.Cursor <= after {
			continue
		}
		out = append(out, record)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, oldestCursor, latestCursor
}

func (rb *ring) stats() (records, capacity int, oldestCursor, latestCursor uint64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.size > 0 {
		oldestIndex := (rb.head - rb.size + rb.capacity) % rb.capacity
		oldestCursor = rb.buf[oldestIndex].Cursor
	}
	return rb.size, rb.capacity, oldestCursor, rb.next
}

// records returns a copy of the buffered records, newest-first, capped at
// min(limit, size). A limit <= 0 means "return all currently buffered".
func (rb *ring) records(limit int) []Record {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	n := rb.size
	if limit > 0 && limit < n {
		n = limit
	}
	if n == 0 {
		return []Record{}
	}

	out := make([]Record, n)
	// The newest entry is at (head-1+capacity)%capacity; walk backwards.
	for i := 0; i < n; i++ {
		idx := (rb.head - 1 - i + rb.capacity) % rb.capacity
		out[i] = rb.buf[idx]
	}
	return out
}
