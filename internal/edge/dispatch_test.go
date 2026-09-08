package edge

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/codeswhat/portwing/internal/adapter/drydock"
	"github.com/codeswhat/portwing/internal/docker"
	"github.com/codeswhat/portwing/internal/protocol"
)

type completedDeleteSender chan struct{}

func (s completedDeleteSender) SendTypedMessage(string, any) error {
	s <- struct{}{}
	return nil
}

func TestReadPumpControlsContinueWhenAdapterPoolFull(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "lk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 33)
	release := make(chan struct{})
	var requests atomic.Int32
	var handlers sync.WaitGroup
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"ApiVersion":"1.44"}`))
			return
		}
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected fake Docker request: %s %s", r.Method, r.URL.Path)
		}
		handlers.Add(1)
		defer handlers.Done()
		requests.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	_ = srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)
	dc, err := docker.NewClient(socket, 10)
	if err != nil {
		t.Fatal(err)
	}
	a := drydock.NewAdapter(dc, "test-agent", drydock.AgentInfo{})
	ctx, cancel := context.WithCancel(context.Background())
	completed := make(completedDeleteSender, 32)
	admitted := 0
	defer func() {
		cancel()
		close(release)
		for i := 0; i < admitted; i++ {
			select {
			case <-completed:
			case <-time.After(2 * time.Second):
				t.Error("adapter worker did not finish")
				return
			}
		}
		handlers.Wait()
	}()
	for i := 0; i < 32; i++ {
		if !a.HandleMessage(ctx, completed, protocol.TypeDDContainerDeleteRequest, json.RawMessage(`{"requestId":"occupied","containerId":"container"}`)) {
			t.Fatal("delete not recognized")
		}
		admitted++
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("fake delete did not start")
		}
	}
	c, ctrl := newTestClient(t)
	c.adapter = a
	pipe, peer := net.Pipe()
	defer peer.Close()
	session := newExecSession(c, "active-exec", pipe)
	defer session.Close()
	readDone := make(chan struct{})
	go func() { defer close(readDone); _ = c.readPump(ctx) }()
	defer func() {
		cancel()
		_ = ctrl.Close()
		select {
		case <-readDone:
		case <-time.After(2 * time.Second):
			t.Error("read pump did not stop")
		}
	}()
	sendEnvelope(t, ctrl, protocol.TypeDDContainerDeleteRequest, protocol.DDContainerDeleteRequestMessage{RequestID: "overloaded", ContainerID: "container"})
	sendEnvelope(t, ctrl, protocol.TypeExecEnd, protocol.ExecEndMessage{ExecID: "active-exec"})
	sendEnvelope(t, ctrl, protocol.TypePing, protocol.PingMessage{Timestamp: 456})
	var reply protocol.DDContainerDeleteResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeDDContainerDeleteResponse), &reply)
	if reply.RequestID != "overloaded" || reply.ContainerID != "container" || reply.Success || reply.Error == "" {
		t.Fatalf("overload response=%+v", reply)
	}
	var pong protocol.PongMessage
	decodeData(t, expectType(t, ctrl, protocol.TypePong), &pong)
	if pong.Timestamp != 456 {
		t.Fatalf("pong=%+v", pong)
	}
	select {
	case <-session.done:
	default:
		t.Fatal("exec-end was not processed")
	}
	if requests.Load() != 32 {
		t.Fatalf("Docker requests=%d, want 32", requests.Load())
	}
	_ = ctrl.Close()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("peer closure did not stop read pump while pool full")
	}
}

// runReadPump starts the read pump against the test client and returns a cancel
// func. The pump exits when the context is cancelled or the conn closes (test
// cleanup closes both ends).
func runReadPump(t *testing.T, c *Client) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.readPump(ctx) }()
	t.Cleanup(cancel)
	return cancel
}

// sendEnvelope marshals data into an envelope and sends it from the controller
// to the agent.
func sendEnvelope(t *testing.T, ctrl *websocket.Conn, msgType string, data any) {
	t.Helper()
	env := protocol.Envelope{Type: msgType}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("marshal %s: %v", msgType, err)
		}
		env.Data = raw
	}
	if err := ctrl.WriteJSON(env); err != nil {
		t.Fatalf("write %s: %v", msgType, err)
	}
}

// A ping from the controller is answered with a pong that echoes the timestamp.
func TestReadPumpAnswersPingWithPong(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	runReadPump(t, c)

	sendEnvelope(t, ctrl, protocol.TypePing, protocol.PingMessage{Timestamp: 12345})

	var pong protocol.PongMessage
	decodeData(t, expectType(t, ctrl, protocol.TypePong), &pong)
	if pong.Timestamp != 12345 {
		t.Errorf("pong timestamp = %d, want 12345", pong.Timestamp)
	}
}

// A malformed envelope is skipped and the read loop keeps serving — proven by a
// subsequent ping still drawing a pong.
func TestReadPumpSkipsMalformedEnvelopeAndKeepsServing(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	runReadPump(t, c)

	if err := ctrl.WriteMessage(websocket.TextMessage, []byte("{ not valid json")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	sendEnvelope(t, ctrl, protocol.TypePing, protocol.PingMessage{Timestamp: 7})
	expectType(t, ctrl, protocol.TypePong)
}

// Once the in-flight request semaphore is saturated, further requests are
// rejected with an error rather than blocking the read loop — the backpressure
// guarantee for tunneled request fan-out.
func TestReadPumpRejectsRequestsWhenStreamLimitReached(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)

	// Saturate the stream semaphore so the dispatch hits the default (reject)
	// branch without ever reaching the Docker-backed request handler.
	for i := 0; i < maxStreams; i++ {
		c.streamSem <- struct{}{}
	}

	runReadPump(t, c)

	sendEnvelope(t, ctrl, protocol.TypeRequest, protocol.RequestMessage{
		RequestID: "req-1",
		Method:    "GET",
		Path:      "/containers/json",
	})

	var errMsg protocol.ErrorMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeError), &errMsg)
	if errMsg.RequestID != "req-1" {
		t.Errorf("error RequestID = %q, want req-1", errMsg.RequestID)
	}
	if errMsg.Message == "" {
		t.Error("rejection carried no message")
	}
}

// Exec control messages for an unknown session are dispatched without crashing
// the read loop; liveness is confirmed by a following ping/pong.
func TestReadPumpDispatchesExecControlForUnknownSession(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	runReadPump(t, c)

	sendEnvelope(t, ctrl, protocol.TypeExecInput, protocol.ExecInputMessage{ExecID: "ghost", Data: "Zm9v"})
	sendEnvelope(t, ctrl, protocol.TypeExecResize, protocol.ExecResizeMessage{ExecID: "ghost", Cols: 80, Rows: 24})
	sendEnvelope(t, ctrl, protocol.TypeExecEnd, protocol.ExecEndMessage{ExecID: "ghost"})

	sendEnvelope(t, ctrl, protocol.TypePing, protocol.PingMessage{Timestamp: 99})
	var pong protocol.PongMessage
	decodeData(t, expectType(t, ctrl, protocol.TypePong), &pong)
	if pong.Timestamp != 99 {
		t.Errorf("pong timestamp = %d, want 99", pong.Timestamp)
	}
}
