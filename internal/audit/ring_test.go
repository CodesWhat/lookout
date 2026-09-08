package audit

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"
)

// TestRingPushAndOrder verifies that records come back newest-first.
func TestRingPushAndOrder(t *testing.T) {
	t.Parallel()

	rb := newRing(4)
	for i := 0; i < 3; i++ {
		rb.push(Record{Event: string(rune('A' + i))})
	}

	got := rb.records(0)
	if len(got) != 3 {
		t.Fatalf("expected 3 records, got %d", len(got))
	}
	// Newest-first: C, B, A
	want := []string{"C", "B", "A"}
	for i, r := range got {
		if r.Event != want[i] {
			t.Errorf("records[%d].Event = %q, want %q", i, r.Event, want[i])
		}
	}
}

// TestRingOverwrite verifies that oldest entries are dropped when the buffer is full.
func TestRingOverwrite(t *testing.T) {
	t.Parallel()

	rb := newRing(3)
	// Push 5 events; only last 3 should remain.
	for i := 0; i < 5; i++ {
		rb.push(Record{Event: string(rune('A' + i))})
	}

	got := rb.records(0)
	if len(got) != 3 {
		t.Fatalf("expected 3 records (capacity), got %d", len(got))
	}
	// Newest-first: E, D, C; A and B were overwritten.
	want := []string{"E", "D", "C"}
	for i, r := range got {
		if r.Event != want[i] {
			t.Errorf("records[%d].Event = %q, want %q", i, r.Event, want[i])
		}
	}
}

// TestRingLimitCapping verifies that limit caps the returned slice.
func TestRingLimitCapping(t *testing.T) {
	t.Parallel()

	rb := newRing(10)
	for i := 0; i < 6; i++ {
		rb.push(Record{Event: string(rune('A' + i))})
	}

	got := rb.records(2)
	if len(got) != 2 {
		t.Fatalf("expected 2 records with limit=2, got %d", len(got))
	}
	// Should be the two newest: F, E
	if got[0].Event != "F" || got[1].Event != "E" {
		t.Errorf("unexpected records: %v", got)
	}
}

// TestRingEmptyBuffer verifies that an empty ring returns an empty slice.
func TestRingEmptyBuffer(t *testing.T) {
	t.Parallel()

	rb := newRing(8)
	got := rb.records(0)
	if len(got) != 0 {
		t.Fatalf("expected 0 records from empty ring, got %d", len(got))
	}
}

func TestRingRecordsAfterRetainedWindow(t *testing.T) {
	t.Parallel()

	rb := newRing(3)
	for i := 0; i < 5; i++ {
		rb.push(Record{Event: string(rune('A' + i))})
	}

	got, oldest, latest := rb.recordsAfter(2, 2)
	if oldest != 3 || latest != 5 {
		t.Fatalf("window = %d..%d, want 3..5", oldest, latest)
	}
	if len(got) != 2 || got[0].Cursor != 3 || got[1].Cursor != 4 {
		t.Fatalf("limited records = %+v, want cursors 3,4", got)
	}

	got, oldest, latest = rb.recordsAfter(4, 0)
	if oldest != 3 || latest != 5 || len(got) != 1 || got[0].Cursor != 5 {
		t.Fatalf("records after 4 = %+v with window %d..%d", got, oldest, latest)
	}

	got, _, _ = rb.recordsAfter(99, 0)
	if len(got) != 0 {
		t.Fatalf("records after future cursor = %+v, want empty", got)
	}
}

func TestRingRecordsAfterAndStatsEmpty(t *testing.T) {
	t.Parallel()

	rb := newRing(4)
	got, oldest, latest := rb.recordsAfter(0, 0)
	if got == nil || len(got) != 0 || oldest != 0 || latest != 0 {
		t.Fatalf("empty recordsAfter = %+v, %d..%d", got, oldest, latest)
	}
	records, capacity, oldest, latest := rb.stats()
	if records != 0 || capacity != 4 || oldest != 0 || latest != 0 {
		t.Fatalf("empty stats = records:%d capacity:%d window:%d..%d",
			records, capacity, oldest, latest)
	}

	rb.push(Record{Event: "A"})
	records, capacity, oldest, latest = rb.stats()
	if records != 1 || capacity != 4 || oldest != 1 || latest != 1 {
		t.Fatalf("populated stats = records:%d capacity:%d window:%d..%d",
			records, capacity, oldest, latest)
	}
}

// TestLoggerBufferWithoutSink verifies that events are captured in the buffer
// even when no slog sink is configured.
func TestLoggerBufferWithoutSink(t *testing.T) {
	t.Parallel()

	l, cleanup, err := New("", 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cleanup()

	if l.Enabled() {
		t.Error("Enabled() should be false when dest is empty")
	}

	l.AuthFailure("1.2.3.4", "GET", "/secret")
	l.RateLimited("1.2.3.4", "POST", "/api")
	l.ExecStart("1.2.3.4", "/exec/abc/start", "abc123")

	recs := l.Records(0)
	if len(recs) != 3 {
		t.Fatalf("expected 3 buffered records, got %d", len(recs))
	}
	// Newest-first: ExecStart, RateLimited, AuthFailure
	if recs[0].Event != EventExecStart {
		t.Errorf("records[0].Event = %q, want %q", recs[0].Event, EventExecStart)
	}
	if recs[1].Event != EventRateLimited {
		t.Errorf("records[1].Event = %q, want %q", recs[1].Event, EventRateLimited)
	}
	if recs[2].Event != EventAuthFailure {
		t.Errorf("records[2].Event = %q, want %q", recs[2].Event, EventAuthFailure)
	}
}

// TestLoggerDisabledBuffer verifies that Records returns empty slice when
// bufferSize is 0.
func TestLoggerDisabledBuffer(t *testing.T) {
	t.Parallel()

	l, cleanup, err := New("", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cleanup()

	l.AuthFailure("x", "GET", "/y")
	l.APIRequest("x", "GET", "/y", OutcomeAllowed, 200, 1.0)

	recs := l.Records(0)
	if len(recs) != 0 {
		t.Errorf("expected 0 records from disabled buffer, got %d", len(recs))
	}
}

// TestRecordJSONMarshal verifies JSON field names and omitempty behavior.
func TestRecordJSONMarshal(t *testing.T) {
	t.Parallel()

	ts := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	r := Record{
		Time:  ts,
		Event: EventAuthFailure,
		Actor: "10.0.0.1",
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	s := string(b)

	if !strings.Contains(s, `"ts"`) {
		t.Errorf("expected ts field, got: %s", s)
	}
	if !strings.Contains(s, `"event"`) {
		t.Errorf("expected event field, got: %s", s)
	}
	if !strings.Contains(s, `"actor"`) {
		t.Errorf("expected actor field, got: %s", s)
	}
	// omitempty fields with zero values must not appear
	for _, absent := range []string{`"method"`, `"path"`, `"outcome"`, `"status"`, `"duration_ms"`, `"operation"`, `"stack"`, `"container"`, `"exec_id"`, `"key_id"`} {
		if strings.Contains(s, absent) {
			t.Errorf("expected %q to be omitted, got: %s", absent, s)
		}
	}
}

func TestRingRetainedDisplayFields(t *testing.T) {
	t.Parallel()
	for _, field := range []struct {
		name       string
		limit      int
		makeRecord func(string) Record
		read       func(Record) string
	}{
		{"path", 4096, func(s string) Record { return Record{Path: s} }, func(r Record) string { return r.Path }},
		{"method", 64, func(s string) Record { return Record{Method: s} }, func(r Record) string { return r.Method }},
		{"actor", 256, func(s string) Record { return Record{Actor: s} }, func(r Record) string { return r.Actor }},
	} {
		t.Run(field.name, func(t *testing.T) {
			for _, input := range []struct{ name, value string }{
				{"ordinary", "normal"},
				{"at-limit", strings.Repeat("a", field.limit)},
				{"oversized", strings.Repeat("a", 1<<20)},
				{"unicode-two-byte", strings.Repeat("é", field.limit)},
				{"unicode-three-byte", strings.Repeat("界", field.limit)},
				{"unicode-four-byte", strings.Repeat("🙂", field.limit)},
			} {
				t.Run(input.name, func(t *testing.T) {
					backing := "prefix" + input.value + strings.Repeat("z", 1<<20)
					value := backing[len("prefix") : len("prefix")+len(input.value)]
					rb := newRing(1)
					rb.push(field.makeRecord(value))
					got := field.read(rb.records(1)[0])
					if len(got) > field.limit {
						t.Errorf("retained %d bytes, limit %d", len(got), field.limit)
					}
					if len(value) <= field.limit {
						if got != value {
							t.Error("ordinary value changed")
						}
					} else {
						if !strings.HasSuffix(got, "[truncated]") {
							t.Error("missing truncation marker")
						}
						if !utf8.ValidString(got) {
							t.Error("truncation split a UTF-8 character")
						}
						prefix := strings.TrimSuffix(got, "[truncated]")
						if !strings.HasPrefix(value, prefix) {
							t.Error("retained content is not an input prefix")
						}
					}
					retained := uintptr(unsafe.Pointer(unsafe.StringData(got)))  // #nosec G103 -- read-only address comparison verifies owned storage; no dereference or mutation.
					start := uintptr(unsafe.Pointer(unsafe.StringData(backing))) // #nosec G103 -- read-only backing range for the ownership assertion; no dereference or mutation.
					if retained >= start && retained < start+uintptr(len(backing)) {
						t.Error("retained display value aliases the large input allocation")
					}
					runtime.KeepAlive(backing)
				})
			}
		})
	}
}

func TestLoggerRetainsBoundedDisplayFields(t *testing.T) {
	t.Parallel()
	l, cleanup, err := New("", 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	oversized := strings.Repeat("x", 1<<20)
	l.AuthFailure(oversized, oversized, oversized)
	l.RateLimited(oversized, oversized, oversized)
	for _, r := range l.Records(0) {
		if len(r.Path) > 4096 || len(r.Method) > 64 || len(r.Actor) > 256 {
			t.Errorf("unbounded %s: path=%d method=%d actor=%d", r.Event, len(r.Path), len(r.Method), len(r.Actor))
		}
	}
}
