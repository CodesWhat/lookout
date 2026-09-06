package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- NewEventStream ----

func TestNewEventStream_DefaultDelays(t *testing.T) {
	t.Parallel()

	c := &Client{apiVersion: "v1.44"}
	es := NewEventStream(c)
	if es.client != c {
		t.Fatal("NewEventStream: client not set")
	}
	if es.initialDelay != 5*time.Second {
		t.Fatalf("initialDelay = %v, want %v", es.initialDelay, 5*time.Second)
	}
	if es.maxDelay != 60*time.Second {
		t.Fatalf("maxDelay = %v, want %v", es.maxDelay, 60*time.Second)
	}
}

// ---- allowedActions ----

func TestAllowedActions_ContainsExpected(t *testing.T) {
	t.Parallel()

	expected := []string{
		"create", "start", "stop", "die", "kill",
		"restart", "pause", "unpause", "destroy",
		"rename", "update", "oom", "health_status",
	}
	for _, action := range expected {
		if !allowedActions[action] {
			t.Errorf("allowedActions[%q] = false, want true", action)
		}
	}

	notAllowed := []string{"exec_create", "exec_start", "attach", "commit"}
	for _, action := range notAllowed {
		if allowedActions[action] {
			t.Errorf("allowedActions[%q] = true, want false", action)
		}
	}
}

func TestEventStreamReadEventsPreservesHealthStatusAction(t *testing.T) {
	t.Parallel()

	want := DockerEvent{
		ID:     "ctr1",
		Type:   "container",
		Action: "health_status: healthy",
		Actor:  Actor{ID: "ctr1"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	es := &EventStream{client: newTestClient(srv)}
	events := make(chan DockerEvent, 1)
	if err := es.readEvents(t.Context(), events); err != nil {
		t.Fatalf("readEvents: %v", err)
	}

	select {
	case got := <-events:
		if got.Action != want.Action {
			t.Fatalf("action = %q, want %q", got.Action, want.Action)
		}
	default:
		t.Fatalf("health event with action %q was filtered", want.Action)
	}
}

// ---- Subscribe: events are received and filtered ----

func TestEventStream_Subscribe_ReceivesFilteredEvents(t *testing.T) {
	t.Parallel()

	// Events to serve: one allowed, one not.
	events := []DockerEvent{
		{ID: "ctr1", Action: "start", Type: "container", Actor: Actor{ID: "ctr1", Attributes: map[string]string{"name": "app"}}},
		{ID: "ctr1", Action: "exec_create", Type: "container"}, // not in allowedActions — must be filtered
		{ID: "ctr1", Action: "stop", Type: "container"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		for _, e := range events {
			enc.Encode(e) //nolint:errcheck
		}
		// Body ends here; the decoder will get EOF and readEvents returns nil.
	}))
	defer srv.Close()

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 100 * time.Millisecond,
		maxDelay:     100 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch, err := es.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	var received []DockerEvent
	// Collect up to 2 events (the 2 allowed ones). The goroutine will reconnect
	// after EOF, so we cancel as soon as we have enough.
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				goto done
			}
			received = append(received, ev)
			if len(received) >= 2 {
				cancel()
			}
		case <-ctx.Done():
			goto done
		}
	}
done:

	// We should have received exactly the allowed events.
	if len(received) < 2 {
		t.Fatalf("expected at least 2 events, got %d: %v", len(received), received)
	}
	if received[0].Action != "start" {
		t.Fatalf("first event Action = %q, want %q", received[0].Action, "start")
	}
	if received[1].Action != "stop" {
		t.Fatalf("second event Action = %q, want %q", received[1].Action, "stop")
	}
}

// TestEventStream_Subscribe_ChannelClosedOnCancel verifies that cancelling the
// context causes the channel to eventually close.
func TestEventStream_Subscribe_ChannelClosedOnCancel(t *testing.T) {
	t.Parallel()

	// Serve a slow stream that blocks until the context is cancelled.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write headers but don't send any events; just keep the connection open.
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 10 * time.Millisecond,
		maxDelay:     10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())

	ch, err := es.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Cancel after a tiny delay.
	time.AfterFunc(50*time.Millisecond, cancel)

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed, got a value")
		}
		// Channel closed as expected.
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for channel to close after context cancel")
	}
}

// TestEventStream_Subscribe_ReconnectsOnError verifies that the stream
// reconnects when the server closes the connection after sending one event,
// and continues delivering events from the new connection.
func TestEventStream_Subscribe_ReconnectsOnError(t *testing.T) {
	t.Parallel()

	// Count how many times we've been called.
	var requestCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.Encode(DockerEvent{ID: "ctr1", Action: "start"}) //nolint:errcheck
		// Close the connection (EOF) to trigger reconnect.
	}))
	defer srv.Close()

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 10 * time.Millisecond,
		maxDelay:     20 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch, err := es.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Collect 3 events across reconnections.
	var count int
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				goto reconnectDone
			}
			count++
			if count >= 3 {
				cancel()
				goto reconnectDone
			}
		case <-ctx.Done():
			goto reconnectDone
		}
	}
reconnectDone:

	if count < 3 {
		t.Fatalf("expected at least 3 events across reconnections, got %d (requests: %d)", count, requestCount)
	}
	if requestCount < 3 {
		t.Fatalf("expected at least 3 reconnections, got %d", requestCount)
	}
}

// TestEventStream_readEvents_ContextCancelDuringDecode verifies that context
// cancellation during decode causes readEvents to return promptly.
func TestEventStream_readEvents_ContextCancelDuringDecode(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Block until the request context is cancelled (client disconnects).
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 10 * time.Millisecond,
		maxDelay:     10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan DockerEvent, 8)

	go func() {
		defer close(ch)
		es.readEvents(ctx, ch) //nolint:errcheck
	}()

	time.AfterFunc(50*time.Millisecond, cancel)

	select {
	case <-ch:
		// Channel closed after context cancel — expected.
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for readEvents to return after context cancel")
	}
}

// TestEventStream_readEvents_GetEventsError exercises the error path where the
// server returns a non-200 status, making GetEvents return an error.
func TestEventStream_readEvents_GetEventsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 10 * time.Millisecond,
		maxDelay:     10 * time.Millisecond,
	}

	ctx := context.Background()
	ch := make(chan DockerEvent, 8)
	err := es.readEvents(ctx, ch)
	if err == nil {
		t.Fatal("expected error from GetEvents (403), got nil")
	}
}

// TestDockerEvent_JSONDecoding verifies that the DockerEvent struct decodes
// correctly from a real Docker-shaped JSON payload.
func TestDockerEvent_JSONDecoding(t *testing.T) {
	t.Parallel()

	payload := `{
		"status": "start",
		"id": "abc123def456",
		"from": "nginx:latest",
		"Type": "container",
		"Action": "start",
		"Actor": {
			"ID": "abc123def456",
			"Attributes": {
				"name": "my-nginx",
				"image": "nginx:latest"
			}
		},
		"time": 1700000000,
		"timeNano": 1700000000000000000
	}`

	var ev DockerEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if ev.ID != "abc123def456" {
		t.Errorf("ID = %q, want %q", ev.ID, "abc123def456")
	}
	if ev.Action != "start" {
		t.Errorf("Action = %q, want %q", ev.Action, "start")
	}
	if ev.Type != "container" {
		t.Errorf("Type = %q, want %q", ev.Type, "container")
	}
	if ev.Actor.Attributes["name"] != "my-nginx" {
		t.Errorf("Actor.Attributes[name] = %q, want %q", ev.Actor.Attributes["name"], "my-nginx")
	}
	if ev.Time != 1700000000 {
		t.Errorf("Time = %d, want %d", ev.Time, 1700000000)
	}
}

// TestEventStream_run_ContextAlreadyCancelled verifies that run() returns
// immediately (without trying to open the event stream) when the context is
// already cancelled before run() is called.
func TestEventStream_run_ContextAlreadyCancelled(t *testing.T) {
	t.Parallel()

	var requested atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 10 * time.Millisecond,
		maxDelay:     10 * time.Millisecond,
	}

	// Cancel context BEFORE calling run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch := make(chan DockerEvent, 8)
	done := make(chan struct{})
	go func() {
		es.run(ctx, ch)
		close(done)
	}()

	select {
	case <-done:
		// run() returned promptly — expected.
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return promptly with pre-cancelled context")
	}

	if requested.Load() {
		t.Fatal("run() made a request despite pre-cancelled context")
	}
}

// TestEventStream_run_ErrorLogging verifies the non-EOF error path (slog.Warn
// branch) and that the stream reconnects after an error.
func TestEventStream_run_ErrorLogging(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			// First call: return error status to trigger error path.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Subsequent calls: return a valid event then EOF.
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.Encode(DockerEvent{ID: "ctr1", Action: "start"}) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 10 * time.Millisecond,
		maxDelay:     20 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch, err := es.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Wait to receive one event (from the second+ connection).
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before receiving event")
		}
		if ev.Action != "start" {
			t.Fatalf("Action = %q, want %q", ev.Action, "start")
		}
		cancel()
	case <-ctx.Done():
		t.Fatal("timed out waiting for event after reconnect")
	}
}

// TestEventStream_run_LogsOnlyOnConnectionError exercises the negation
// boundary of the "err != nil" check that guards the slog.Warn call: it
// must log only when readEvents actually returned an error, not on every
// reconnect. Not parallel: swaps the global slog default logger for the
// test's duration (see TestCloseConn_Error in client_unix_test.go for why
// that's safe with the surrounding serial/parallel test ordering).
func TestEventStream_run_LogsOnlyOnConnectionError(t *testing.T) {
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			// First connection: error, so the disconnect log SHOULD fire.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Second connection: succeeds, decodes one event, then hits EOF —
		// readEvents returns nil, so the disconnect log must NOT fire for it.
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.Encode(DockerEvent{ID: "ctr1", Action: "start"}) //nolint:errcheck
	}))
	defer srv.Close()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	// cleanReconnect closes on the second call to after: the backoff fired
	// after the first (errored) connection, so a second call means run() has
	// already finished readEvents for the second (clean) connection and
	// observed err == nil. Only cancel once that's signaled, so the test
	// doesn't race cancellation against readEvents still decoding the
	// trailing EOF for the clean connection.
	cleanReconnect := make(chan struct{})
	var afterCalls atomic.Int32

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 10 * time.Millisecond,
		maxDelay:     20 * time.Millisecond,
		// Fire the reconnect backoff immediately instead of sleeping in real
		// time: this test is serial and Gremlins reruns it per mutant, so a
		// real wait here only matters if the backoff itself is broken
		// (covered separately by TestEventStream_run_BackoffCapped).
		after: func(time.Duration) <-chan time.Time {
			if afterCalls.Add(1) == 2 {
				close(cleanReconnect)
			}
			ready := make(chan time.Time, 1)
			ready <- time.Now()
			return ready
		},
	}

	// 1s is plenty once the backoff fires immediately; it only matters if a
	// mutant breaks the reconnect loop outright.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	ch, err := es.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before receiving event")
		}
		if ev.Action != "start" {
			t.Fatalf("Action = %q, want %q", ev.Action, "start")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event after reconnect")
	}

	select {
	case <-cleanReconnect:
		cancel()
	case <-ctx.Done():
		t.Fatal("timed out waiting for clean reconnect")
	}
	for range ch {
	}

	logged := buf.String()
	got := strings.Count(logged, "docker event stream disconnected")
	if got != 1 {
		t.Fatalf("disconnect log count = %d, want exactly 1 (only the errored connection), log:\n%s", got, logged)
	}
	// The one log line must carry the actual connection error (from the 403
	// response), not a nil error: a mutant that flips the guard to fire on
	// success (err == nil) instead of failure would still log exactly once,
	// but with error=<nil> for the successful second connection instead of
	// the forbidden-response error for the first.
	if !strings.Contains(logged, "docker error (status 403)") {
		t.Fatalf("expected the disconnect log to carry the 403 connection error, got:\n%s", logged)
	}
	if strings.Contains(logged, "error=<nil>") {
		t.Fatalf("disconnect log fired with a nil error, want it only on the failed connection:\n%s", logged)
	}
}

// TestEventStream_run_BackoffCapped verifies that the backoff delay is capped
// at maxDelay.
func TestEventStream_run_BackoffCapped(t *testing.T) {
	t.Parallel()

	var waits []time.Duration
	cancelAfterWaits := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return 403 every time so each attempt errors and backoff doubles.
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	// Use very short delays so test is fast; initialDelay < maxDelay so cap is exercised.
	es := &EventStream{
		client:       c,
		initialDelay: 5 * time.Millisecond,
		maxDelay:     10 * time.Millisecond,
		after: func(delay time.Duration) <-chan time.Time {
			waits = append(waits, delay)
			ready := make(chan time.Time)
			if len(waits) == 4 {
				close(cancelAfterWaits)
				return ready
			}
			close(ready)
			return ready
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-cancelAfterWaits
		cancel()
	}()

	ch, err := es.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	select {
	case <-cancelAfterWaits:
	case <-time.After(time.Second):
		t.Fatal("backoff did not request four waits")
	}
	for range ch {
	}

	want := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	if !slices.Equal(waits, want) {
		t.Fatalf("backoff waits = %v, want %v", waits, want)
	}
}

// TestEventStream_readEvents_ContextCancelledAtLoopTop verifies that readEvents
// returns ctx.Err() when the context is cancelled after one event is processed
// but before the next Decode call — hitting the ctx.Err() check at the top of
// the decode loop (events.go line 124).
func TestEventStream_readEvents_ContextCancelledAtLoopTop(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Synchronization: the server signals when it has sent the first event.
	firstEventSent := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Send ONE allowed event.
		enc.Encode(DockerEvent{ID: "ctr1", Action: "start"}) //nolint:errcheck
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(firstEventSent)
		// Block until client disconnects (context cancelled).
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 10 * time.Millisecond,
		maxDelay:     10 * time.Millisecond,
	}

	// Use a buffered channel so the send succeeds immediately.
	ch := make(chan DockerEvent, 8)

	done := make(chan error, 1)
	go func() {
		done <- es.readEvents(ctx, ch)
	}()

	// Wait for the first event to be sent, drain the channel, then cancel.
	// After the send to ch succeeds, readEvents loops back to the ctx.Err()
	// check. Cancelling now means the next iteration hits line 124.
	select {
	case <-firstEventSent:
		// Drain the buffered event.
		select {
		case <-ch:
		default:
		}
		// Now cancel — readEvents will see ctx.Err() != nil at the loop top.
		cancel()
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first event")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error after context cancel, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for readEvents to return")
	}
}

// TestEventStream_readEvents_SendCancelledByContext exercises the select in
// readEvents where ctx.Done() fires while trying to send an event to the channel.
func TestEventStream_readEvents_SendCancelledByContext(t *testing.T) {
	t.Parallel()

	// Server sends a continuous stream of events.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		flusher, _ := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				return
			default:
				enc.Encode(DockerEvent{ID: "ctr1", Action: "start"}) //nolint:errcheck
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}))
	defer srv.Close()

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 10 * time.Millisecond,
		maxDelay:     10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Use a zero-buffer channel so sends block when the receiver isn't reading.
	ch := make(chan DockerEvent)

	done := make(chan error, 1)
	go func() {
		done <- es.readEvents(ctx, ch)
	}()

	// Cancel immediately so the send-to-channel select hits ctx.Done().
	time.AfterFunc(10*time.Millisecond, cancel)

	// Do NOT drain the channel so the send blocks.
	select {
	case <-done:
		// readEvents returned after context cancel — expected.
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for readEvents to return")
	}
}

// TestEventStream_readEvents_BadJSON exercises the non-EOF decode error path
// (neither io.EOF nor ctx.Err, so the raw error is returned).
func TestEventStream_readEvents_BadJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Write a valid JSON event followed by garbage.
		w.Write([]byte("{\"Action\":\"start\"}\nNOT_JSON\n")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 10 * time.Millisecond,
		maxDelay:     10 * time.Millisecond,
	}

	ctx := context.Background()
	ch := make(chan DockerEvent, 8)
	err := es.readEvents(ctx, ch)

	// Should return a non-nil, non-EOF error from the malformed JSON.
	if err == nil {
		t.Fatal("expected non-nil error from bad JSON, got nil")
	}
}
func TestReadEvents_CtxErrAtLoopTop(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// readyToCancel is closed once the server has flushed all events.
	readyToCancel := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		enc := json.NewEncoder(w)
		// Write many non-allowed events so the client spends time iterating
		// through them with `continue`.  The context is cancelled while the
		// client is in this hot loop, ensuring ctx.Err() fires at the loop top.
		const count = 500
		for i := 0; i < count; i++ {
			enc.Encode(DockerEvent{ID: "ctr1", Action: "exec_create", Type: "container"}) //nolint:errcheck
		}
		if flusher != nil {
			flusher.Flush()
		}
		close(readyToCancel)

		// Keep the connection open so readEvents doesn't get an EOF before
		// we've had a chance to trigger the ctx.Err() path.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	c := newTestClient(srv)
	es := &EventStream{
		client:       c,
		initialDelay: 10 * time.Millisecond,
		maxDelay:     10 * time.Millisecond,
	}

	ch := make(chan DockerEvent, 8)
	done := make(chan error, 1)
	go func() {
		done <- es.readEvents(ctx, ch)
	}()

	// Cancel the context once the server has sent all events so that the
	// client goroutine sees the cancellation during its continue-loop.
	go func() {
		<-readyToCancel
		cancel()
	}()

	select {
	case err := <-done:
		// readEvents should return ctx.Err() (non-nil) after hitting the
		// ctx.Err() check at the top of the decode loop.
		if err == nil {
			t.Fatal("expected non-nil error (context cancelled), got nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for readEvents to return")
	}
}
