package drydock

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/protocol"
)

// syncCaptureSender is a race-safe sender that captures the most recently sent
// message type and data. HandleMessage dispatches into a goroutine, so reads
// from the test goroutine and writes from the handler goroutine are concurrent.
type syncCaptureSender struct {
	mu      sync.Mutex
	msgType string
	data    any
	ready   chan struct{}
}

func newSyncCaptureSender() *syncCaptureSender {
	return &syncCaptureSender{ready: make(chan struct{}, 1)}
}

func (s *syncCaptureSender) SendTypedMessage(msgType string, data any) error {
	s.mu.Lock()
	s.msgType = msgType
	s.data = data
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
	return nil
}

func (s *syncCaptureSender) MsgType() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgType
}

func (s *syncCaptureSender) Data() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

// TestHandleMessage_RecognizedTypes verifies that every supported message type
// returns true (handled) and dispatches to the right internal handler by
// checking that the sender receives the expected response message type.
func TestHandleMessage_RecognizedTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		msgType     string
		payload     json.RawMessage
		wantMsgType string
	}{
		{
			name:        "watch_request routes to watch_response",
			msgType:     protocol.TypeDDWatchRequest,
			payload:     json.RawMessage(`{"watcherType":"docker","watcherName":"main"}`),
			wantMsgType: protocol.TypeDDWatchResponse,
		},
		{
			name:        "watch_container_request routes to watch_container_response",
			msgType:     protocol.TypeDDWatchContainerRequest,
			payload:     json.RawMessage(`{"watcherType":"docker","watcherName":"main","containerId":"c1"}`),
			wantMsgType: protocol.TypeDDWatchContainerResponse,
		},
		{
			name:        "trigger_request routes to trigger_response",
			msgType:     protocol.TypeDDTriggerRequest,
			payload:     json.RawMessage(`{"triggerType":"restart","triggerName":"web"}`),
			wantMsgType: protocol.TypeDDTriggerResponse,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sender := newSyncCaptureSender()
			a := &Adapter{
				messageSem: make(chan struct{}, defaultMessageHandlerConcurrency),
			}

			handled := a.HandleMessage(context.Background(), sender, tc.msgType, tc.payload)
			if !handled {
				t.Fatalf("HandleMessage(%q): expected true (handled), got false", tc.msgType)
			}

			// Wait for the async goroutine to call SendTypedMessage.
			select {
			case <-sender.ready:
			case <-time.After(2 * time.Second):
				t.Fatalf("timed out waiting for response message (want %q)", tc.wantMsgType)
			}

			if got := sender.MsgType(); got != tc.wantMsgType {
				t.Errorf("response message type = %q, want %q", got, tc.wantMsgType)
			}
		})
	}
}

// TestHandleMessage_UnrecognizedType verifies that an unknown message type
// returns false and causes no panic.
func TestHandleMessage_UnrecognizedType(t *testing.T) {
	t.Parallel()

	a := &Adapter{
		messageSem: make(chan struct{}, defaultMessageHandlerConcurrency),
	}
	sender := &captureSender{}

	handled := a.HandleMessage(context.Background(), sender, "unknown:type", json.RawMessage(`{}`))
	if handled {
		t.Fatal("HandleMessage for unknown type should return false")
	}
	// Sender must not have been called.
	if sender.msgType != "" {
		t.Errorf("unexpected message sent for unknown type: %q", sender.msgType)
	}
}

// TestHandleMessage_MalformedPayloadIsHandledGracefully verifies that a
// recognized type with a malformed (non-JSON) payload still returns true
// (the type was recognized) without panicking. The adapter logs the error
// and returns true for the known type.
func TestHandleMessage_MalformedPayloadIsHandledGracefully(t *testing.T) {
	t.Parallel()

	a := &Adapter{
		messageSem: make(chan struct{}, defaultMessageHandlerConcurrency),
	}
	sender := &captureSender{}

	// All recognized types should return true even on bad data.
	recognizedTypes := []struct {
		msgType string
		payload json.RawMessage
	}{
		{protocol.TypeDDWatchRequest, json.RawMessage(`not json`)},
		{protocol.TypeDDWatchContainerRequest, json.RawMessage(`not json`)},
		{protocol.TypeDDTriggerRequest, json.RawMessage(`not json`)},
		{protocol.TypeDDContainerLogRequest, json.RawMessage(`not json`)},
		{protocol.TypeDDContainerDeleteRequest, json.RawMessage(`not json`)},
	}

	for _, tc := range recognizedTypes {
		handled := a.HandleMessage(context.Background(), sender, tc.msgType, tc.payload)
		if !handled {
			t.Errorf("HandleMessage(%q) with bad payload: expected true (type recognized), got false", tc.msgType)
		}
	}
}

// TestHandleMessage_ContainerLogRequestRecognized verifies that the
// container_log_request type is recognized (returns true) by HandleMessage.
// The actual Docker log fetch is intentionally not exercised here because it
// requires a live Docker client; invoking the goroutine would nil-deref.
// We cancel the context immediately so spawnMessageHandler exits before the
// goroutine body calls dockerClient.
func TestHandleMessage_ContainerLogRequestRecognized(t *testing.T) {
	t.Parallel()

	a := &Adapter{
		messageSem:   make(chan struct{}, defaultMessageHandlerConcurrency),
		dockerClient: nil,
	}
	sender := &captureSender{}

	// Cancel the context immediately so spawnMessageHandler's goroutine bails
	// out at the ctx.Err() check before reaching the nil docker client.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	payload := json.RawMessage(`{"containerId":"nonexistent","tail":10}`)
	handled := a.HandleMessage(ctx, sender, protocol.TypeDDContainerLogRequest, payload)
	if !handled {
		t.Fatal("HandleMessage(dd:container_log_request): expected true (handled), got false")
	}
}

// TestHandleMessage_ContainerDeleteRequestRecognized verifies that the
// container_delete_request type is recognized (returns true) by HandleMessage.
// The actual Docker removal is intentionally not exercised here because it
// requires a live Docker client; invoking the goroutine would nil-deref.
// We cancel the context immediately so spawnMessageHandler exits before the
// goroutine body calls dockerClient (see TestHandleContainerDeleteRequest_Success
// and _Error in adapter_test.go for the exercised-handler coverage).
func TestHandleMessage_ContainerDeleteRequestRecognized(t *testing.T) {
	t.Parallel()

	a := &Adapter{
		messageSem:   make(chan struct{}, defaultMessageHandlerConcurrency),
		dockerClient: nil,
	}
	sender := &captureSender{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	payload := json.RawMessage(`{"containerId":"nonexistent"}`)
	handled := a.HandleMessage(ctx, sender, protocol.TypeDDContainerDeleteRequest, payload)
	if !handled {
		t.Fatal("HandleMessage(dd:container_delete_request): expected true (handled), got false")
	}
}

func TestHandleMessageRespondsWhileHandlerPoolFull(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, request, response string }{
		{"delete", protocol.TypeDDContainerDeleteRequest, protocol.TypeDDContainerDeleteResponse},
		{"watch", protocol.TypeDDWatchRequest, protocol.TypeDDWatchResponse},
		{"watch container", protocol.TypeDDWatchContainerRequest, protocol.TypeDDWatchContainerResponse},
		{"trigger", protocol.TypeDDTriggerRequest, protocol.TypeDDTriggerResponse},
		{"cancel logs", protocol.TypeDDContainerLogCancel, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := NewAdapter(nil, "test-agent", AgentInfo{})
			for i := 0; i < cap(a.messageSem); i++ {
				a.messageSem <- struct{}{}
			}
			canceled := make(chan struct{})
			a.logStreams["request"] = activeContainerLogStream{containerID: "container", cancel: func() { close(canceled) }}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sender := newSyncCaptureSender()
			returned := make(chan bool, 1)
			go func() {
				returned <- a.HandleMessage(ctx, sender, tc.request, json.RawMessage(`{"requestId":"request","containerId":"container","watcherType":"docker","watcherName":"main","triggerType":"restart","triggerName":"web"}`))
			}()
			select {
			case handled := <-returned:
				if !handled {
					t.Fatal("request not recognized")
				}
			case <-time.After(250 * time.Millisecond):
				cancel()
				<-returned
				t.Fatal("full pool blocked control or overload response")
			}
			if tc.response != "" {
				if sender.MsgType() != tc.response {
					t.Fatalf("response type=%q", sender.MsgType())
				}
			} else {
				select {
				case <-canceled:
				default:
					t.Fatal("log stream was not canceled")
				}
			}
			if tc.request == protocol.TypeDDContainerDeleteRequest {
				reply, ok := sender.Data().(protocol.DDContainerDeleteResponseMessage)
				if !ok || reply.RequestID != "request" || reply.ContainerID != "container" || reply.Success || reply.Error == "" {
					t.Fatalf("delete response=%+v", sender.Data())
				}
			}
			if len(a.messageSem) != defaultMessageHandlerConcurrency {
				t.Fatal("shared slot count changed")
			}
		})
	}
}
