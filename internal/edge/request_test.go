package edge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/docker"
	"github.com/codeswhat/portwing/internal/protocol"
)

func mkResp(status int, contentType, body string) *http.Response {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// A non-streaming request is proxied via Do and returned as a single response
// envelope carrying the status, content type, and body.
func TestHandleRequestNonStream(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	//nolint:bodyclose // the response body is consumed and closed by handleRequest, the code under test.
	fd := &fakeDocker{doResp: mkResp(http.StatusCreated, "application/json", `{"ok":true}`)}
	c.dockerClient = fd

	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "r1",
		Method:    "POST",
		Path:      "/containers/create",
	})

	var resp protocol.ResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &resp)
	if resp.RequestID != "r1" {
		t.Errorf("RequestID = %q, want r1", resp.RequestID)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if resp.IsStream {
		t.Error("IsStream = true, want false for a unary request")
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Errorf("Body = %s, want {\"ok\":true}", resp.Body)
	}
	if resp.ContentType != "application/json" {
		t.Errorf("ContentType = %q, want application/json", resp.ContentType)
	}
}

func TestHandleRequestForwardsAllowedDockerHeaders(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t)
	//nolint:bodyclose // the response body is consumed and closed by handleRequest, the code under test.
	fd := &fakeDocker{streamResp: mkResp(http.StatusOK, "application/json", `{}`)}
	c.dockerClient = fd

	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "registry-auth",
		Method:    http.MethodPost,
		Path:      "/images/create?fromImage=registry.example/private/app",
		Headers: map[string]string{
			"Accept":            "application/json",
			"Content-Type":      "application/json",
			"X-Registry-Auth":   "base64-registry-credential",
			"X-Registry-Config": "base64-registry-config",
			"Authorization":     "must-not-reach-dockerd",
			"Connection":        "upgrade",
		},
	})

	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.doCalls) != 1 {
		t.Fatalf("Docker calls = %d, want 1", len(fd.doCalls))
	}
	got := fd.doCalls[0].headers
	for key, want := range map[string]string{
		"Accept":            "application/json",
		"Content-Type":      "application/json",
		"X-Registry-Auth":   "base64-registry-credential",
		"X-Registry-Config": "base64-registry-config",
	} {
		if value := got.Get(key); value != want {
			t.Errorf("%s = %q, want %q", key, value, want)
		}
	}
	for _, key := range []string{"Authorization", "Connection"} {
		if value := got.Get(key); value != "" {
			t.Errorf("unsafe %s forwarded as %q", key, value)
		}
	}
}

// A request that fails at the Docker client is reported as an error envelope
// tagged with the originating request id.
func TestHandleRequestError(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	fd := &fakeDocker{doErr: errors.New("dial fail")}
	c.dockerClient = fd

	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "r2",
		Method:    "GET",
		Path:      "/info",
	})

	var em protocol.ErrorMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeError), &em)
	if em.RequestID != "r2" {
		t.Errorf("error RequestID = %q, want r2", em.RequestID)
	}
	if em.Message != "dial fail" {
		t.Errorf("error Message = %q, want dial fail", em.Message)
	}
}

func TestDelayedRequestResponseCannotReachReplacementConnection(t *testing.T) {
	t.Parallel()

	c, _ := newHandshakeTestClient(t)
	oldCh := make(chan protocol.Envelope, 1)
	oldState := &outboundQueueState{}
	c.connMu.Lock()
	c.sendCh = oldCh
	c.sendState = oldState
	c.connMu.Unlock()

	entered := make(chan struct{})
	gate := make(chan struct{})
	//nolint:bodyclose // the response body is consumed and closed by the handler goroutine.
	fd := &fakeDocker{
		doResp:    mkResp(http.StatusOK, "application/json", `{"old":true}`),
		doEntered: entered,
		doGate:    gate,
	}
	c.dockerClient = fd
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.handleRequest(context.Background(), protocol.RequestMessage{
			RequestID: "old-request",
			Method:    http.MethodGet,
			Path:      "/info",
		})
	}()

	select {
	case <-entered:
	case <-time.After(readTimeout):
		t.Fatal("old request did not reach Docker")
	}

	newAgent, _ := newWSPair(t)
	newCh := make(chan protocol.Envelope, 1)
	newState := &outboundQueueState{}
	c.connMu.Lock()
	c.conn = newAgent
	c.sendCh = newCh
	c.sendState = newState
	c.connMu.Unlock()
	oldState.closeAndDiscard(oldCh)
	close(gate)

	select {
	case <-done:
	case <-time.After(readTimeout):
		t.Fatal("old request did not finish")
	}
	if got := len(newCh); got != 0 {
		t.Fatalf("replacement connection received %d delayed old-generation responses, want 0", got)
	}
}

// A streaming request is proxied via DoStream and tunneled as a stream-header
// response, one or more base64 stream chunks, and a terminal stream_end.
func TestHandleRequestStream(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	//nolint:bodyclose // the response body is consumed and closed by handleRequest, the code under test.
	fd := &fakeDocker{streamResp: mkResp(http.StatusOK, "application/octet-stream", "chunk-data")}
	c.dockerClient = fd

	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "r3",
		Method:    "GET",
		Path:      "/containers/abc/logs?follow=1",
	})

	var resp protocol.ResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &resp)
	if !resp.IsStream {
		t.Error("IsStream = false, want true for a streaming path")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}

	var chunk protocol.StreamMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeStream), &chunk)
	if chunk.RequestID != "r3" {
		t.Errorf("stream RequestID = %q, want r3", chunk.RequestID)
	}
	if decoded, _ := base64.StdEncoding.DecodeString(chunk.Data); string(decoded) != "chunk-data" {
		t.Errorf("stream payload = %q, want chunk-data", decoded)
	}

	var end protocol.StreamEndMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeStreamEnd), &end)
	if end.RequestID != "r3" || end.Reason != "complete" {
		t.Errorf("stream_end = %+v, want r3 / complete", end)
	}
}

// Docker's container archive endpoint returns a potentially large tar stream.
// Query parameters are part of every real GET archive request, so edge routing
// must classify the path before choosing the buffered response branch.
func TestHandleRequestContainerArchiveWithQueryUsesStreamingPath(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	bufferedResp := mkResp(http.StatusOK, "application/json", `{"buffered":true}`)
	streamResp := mkResp(http.StatusOK, "application/x-tar", "tar-data")
	t.Cleanup(func() {
		_ = bufferedResp.Body.Close()
		_ = streamResp.Body.Close()
	})
	fd := &fakeDocker{
		// Both responses are populated so the assertion reports a routing
		// failure instead of panicking if the buffered path is selected.
		doResp:     bufferedResp,
		streamResp: streamResp,
	}
	c.dockerClient = fd

	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "archive-1",
		Method:    http.MethodGet,
		Path:      "/containers/abc/archive?path=%2Fvar%2Flib%2Fdata",
	})

	var resp protocol.ResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &resp)
	if !resp.IsStream {
		t.Fatal("container archive response used the buffered path, want streaming")
	}

	fd.mu.Lock()
	calls := append([]doCall(nil), fd.doCalls...)
	fd.mu.Unlock()
	if len(calls) != 1 || !calls[0].stream {
		t.Fatalf("Docker calls = %+v, want one streaming call", calls)
	}

	var chunk protocol.StreamMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeStream), &chunk)
	decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
	if err != nil {
		t.Fatalf("decode archive chunk: %v", err)
	}
	if string(decoded) != "tar-data" {
		t.Fatalf("archive stream payload = %q, want %q", decoded, "tar-data")
	}
	expectType(t, ctrl, protocol.TypeStreamEnd)
}

// A non-stream response whose body isn't valid JSON (e.g. the plain-text "OK"
// from GET /_ping) can't be embedded in ResponseMessage.Body, a json.RawMessage
// field. Dropping that marshal error used to leave the controller waiting
// forever for a response envelope that would never arrive (finding
// C6-SAFE); it must now surface as an error envelope carrying the same
// requestId instead.
func TestHandleRequestNonJSONBodySendsErrorEnvelope(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	//nolint:bodyclose // the response body is consumed and closed by handleRequest, the code under test.
	fd := &fakeDocker{doResp: mkResp(http.StatusOK, "text/plain", "OK")}
	c.dockerClient = fd

	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "ping-1",
		Method:    http.MethodGet,
		Path:      "/_ping",
	})

	var em protocol.ErrorMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeError), &em)
	if em.RequestID != "ping-1" {
		t.Errorf("error RequestID = %q, want ping-1", em.RequestID)
	}
	if em.Message == "" {
		t.Error("error Message is empty, want an explanation of the encoding failure")
	}
}

// When the controller's welcome negotiated CapResponseBodyBase64, a
// non-JSON body (e.g. the plain-text "OK" from GET /_ping) that used to
// force the #201 TypeError stopgap now goes out as ResponseMessage.BodyBase64
// instead, with the legacy Body field left empty, and decodes back to the
// original bytes. Reverting the hasControllerCap gate in handleRequest (or
// the negotiation itself) makes this fail because either no bodyBase64
// arrives or a TypeError envelope arrives instead of a response.
func TestHandleRequestNonJSONBodyUsesBase64WhenNegotiated(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	c.controllerCaps = []string{protocol.CapResponseBodyBase64}
	//nolint:bodyclose // the response body is consumed and closed by handleRequest, the code under test.
	fd := &fakeDocker{doResp: mkResp(http.StatusOK, "text/plain", "OK")}
	c.dockerClient = fd

	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "ping-2",
		Method:    http.MethodGet,
		Path:      "/_ping",
	})

	var resp protocol.ResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &resp)
	if resp.RequestID != "ping-2" {
		t.Errorf("RequestID = %q, want ping-2", resp.RequestID)
	}
	if len(resp.Body) != 0 {
		t.Errorf("Body = %s, want empty when bodyBase64 is used", resp.Body)
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.BodyBase64)
	if err != nil {
		t.Fatalf("decode BodyBase64: %v", err)
	}
	if string(decoded) != "OK" {
		t.Errorf("decoded BodyBase64 = %q, want %q", decoded, "OK")
	}
}

// Without a negotiated capability (the default zero value of a fresh
// Client, matching an old controller whose welcome never mentions
// capabilities), a non-JSON body must still hit the #201 TypeError stopgap
// exactly as before this change — this is the regression guard for the
// "new portwing + old drydock: unchanged from today" leg of the compat
// matrix. Reverting the legacy branch (e.g. always sending BodyBase64
// unconditionally) makes this fail because no TypeError envelope arrives.
func TestHandleRequestNonJSONBodyWithoutCapabilityStillErrors(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	// c.controllerCaps left nil: no capability negotiated.
	//nolint:bodyclose // the response body is consumed and closed by handleRequest, the code under test.
	fd := &fakeDocker{doResp: mkResp(http.StatusOK, "text/plain", "OK")}
	c.dockerClient = fd

	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "ping-3",
		Method:    http.MethodGet,
		Path:      "/_ping",
	})

	var em protocol.ErrorMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeError), &em)
	if em.RequestID != "ping-3" {
		t.Errorf("error RequestID = %q, want ping-3", em.RequestID)
	}
}

// A normal JSON body still round-trips correctly through the negotiated
// bodyBase64 path: the controller decodes the base64 back to the exact same
// JSON bytes portwing read from Docker.
func TestHandleRequestJSONBodyRoundTripsThroughBase64WhenNegotiated(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	c.controllerCaps = []string{protocol.CapResponseBodyBase64}
	//nolint:bodyclose // the response body is consumed and closed by handleRequest, the code under test.
	fd := &fakeDocker{doResp: mkResp(http.StatusCreated, "application/json", `{"ok":true}`)}
	c.dockerClient = fd

	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "r4",
		Method:    "POST",
		Path:      "/containers/create",
	})

	var resp protocol.ResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &resp)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if len(resp.Body) != 0 {
		t.Errorf("Body = %s, want empty when bodyBase64 is used", resp.Body)
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.BodyBase64)
	if err != nil {
		t.Fatalf("decode BodyBase64: %v", err)
	}
	if string(decoded) != `{"ok":true}` {
		t.Errorf("decoded BodyBase64 = %q, want %s", decoded, `{"ok":true}`)
	}
}

// A request on /_portwing/compose must reach the compose manager rather than
// fall through to the dockerd proxy — dockerd has no such route and would
// otherwise 404 every compose deploy (finding C7). Reverting the
// composeRequestPrefix check in handleRequest routes this through
// c.dockerClient instead, which this test would catch via fd.doCalls.
func TestHandleRequestRoutesComposeToComposeManager(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	fd := &fakeDocker{} // no canned response: a call here means mis-routing to dockerd.
	c.dockerClient = fd
	c.compose = docker.NewComposeManager(t.TempDir(), "1.44", "")

	body, err := json.Marshal(docker.ComposeRequest{
		// Omitting StackName fails ComposeManager's own validation, so this
		// exercises the routing without shelling out to a real compose binary.
		Operation: "up",
	})
	if err != nil {
		t.Fatalf("marshal compose request: %v", err)
	}

	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "compose-1",
		Method:    http.MethodPost,
		Path:      "/_portwing/compose",
		Body:      body,
	})

	var resp protocol.ResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &resp)
	if resp.RequestID != "compose-1" {
		t.Errorf("RequestID = %q, want compose-1", resp.RequestID)
	}

	var composeResp docker.ComposeResponse
	decodeData(t, resp.Body, &composeResp)
	if composeResp.Success {
		t.Error("Success = true, want false for a request missing StackName")
	}
	if !strings.Contains(composeResp.Error, "stack name is required") {
		t.Errorf("Error = %q, want it to mention the missing stack name", composeResp.Error)
	}

	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.doCalls) != 0 {
		t.Errorf("Docker calls = %d, want 0 — compose requests must not reach dockerd", len(fd.doCalls))
	}
}

// A compose request whose body isn't valid JSON must surface as a TypeError
// envelope carrying the RequestID (handleComposeRequest's decode-failure
// branch), not silently drop the request or hang the controller waiting for a
// response that will never arrive.
func TestHandleComposeRequestInvalidBodyReturnsTypeError(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	c.compose = docker.NewComposeManager(t.TempDir(), "1.44", "")

	c.handleComposeRequest(context.Background(), protocol.RequestMessage{
		RequestID: "compose-bad-json",
		Method:    http.MethodPost,
		Path:      "/_portwing/compose",
		Body:      json.RawMessage(`{not valid json`),
	})

	var em protocol.ErrorMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeError), &em)
	if em.RequestID != "compose-bad-json" {
		t.Errorf("RequestID = %q, want compose-bad-json", em.RequestID)
	}
	if !strings.Contains(em.Message, "invalid compose request") {
		t.Errorf("Message = %q, want it to mention invalid compose request", em.Message)
	}
}

func TestHandleRequestNonStreamWithBody(t *testing.T) {
	t.Parallel()

	c, ctrl := newTestClient(t)
	//nolint:bodyclose
	fd := &fakeDocker{doResp: mkResp(http.StatusOK, "application/json", `{}`)}
	c.dockerClient = fd

	body := json.RawMessage(`{"image":"nginx"}`)
	c.handleRequest(context.Background(), protocol.RequestMessage{
		RequestID: "body-1",
		Method:    "POST",
		Path:      "/containers/create",
		Body:      body,
	})

	var resp protocol.ResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &resp)
	if resp.RequestID != "body-1" {
		t.Errorf("RequestID = %q, want body-1", resp.RequestID)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
}
