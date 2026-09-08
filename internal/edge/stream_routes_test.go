package edge

import (
	"encoding/base64"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/protocol"
)

func TestHandleRequestStatsAndPushStreamBeforeEOF(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"default stats", http.MethodGet, "/containers/abc/stats"},
		{"versioned stats", http.MethodGet, "/v1.44/containers/abc/stats"},
		{"named push", http.MethodPost, "/images/nginx/push?tag=latest"},
		{"namespaced push", http.MethodPost, "/v1.44/images/registry.example/team/app/push?tag=latest"},
		{"encoded push", http.MethodPost, "/v1.44/images/registry.example%2Fteam%2Fapp/push?tag=latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, ctrl := newTestClient(t)
			reader, writer := io.Pipe()
			buffered := mkResp(http.StatusOK, "application/json", `{"buffered":true}`)
			fd := &fakeDocker{
				doResp: buffered,
				streamResp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       reader,
				},
			}
			c.dockerClient = fd
			release := make(chan struct{})
			var releaseOnce sync.Once
			finish := func() { releaseOnce.Do(func() { close(release) }) }
			handlerDone := make(chan struct{})
			writerDone := make(chan struct{})
			t.Cleanup(func() {
				finish()
				_ = reader.Close()
				_ = writer.Close()
				_ = buffered.Body.Close()
				for _, done := range []chan struct{}{handlerDone, writerDone} {
					select {
					case <-done:
					case <-time.After(readTimeout):
						t.Error("stream test goroutine did not stop")
					}
				}
			})
			chunks := []string{"{\"progress\":1}\n", "{\"progress\":2}\n"}
			go func() {
				defer close(writerDone)
				defer func() { _ = writer.Close() }()
				for _, chunk := range chunks {
					if _, err := io.WriteString(writer, chunk); err != nil {
						return
					}
				}
				<-release
			}()
			go func() {
				defer close(handlerDone)
				c.handleRequest(t.Context(), protocol.RequestMessage{
					RequestID: "progressive-route", Method: tc.method, Path: tc.path,
				})
			}()

			var response protocol.ResponseMessage
			decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &response)
			if !response.IsStream || response.RequestID != "progressive-route" || response.StatusCode != http.StatusOK || response.ContentType != "application/json" {
				t.Fatalf("response = %+v, want streaming JSON metadata", response)
			}
			for _, want := range chunks {
				var chunk protocol.StreamMessage
				decodeData(t, expectType(t, ctrl, protocol.TypeStream), &chunk)
				data, err := base64.StdEncoding.DecodeString(chunk.Data)
				if err != nil || string(data) != want || chunk.RequestID != "progressive-route" {
					t.Fatalf("chunk = %+v, decoded = %q, err = %v, want %q", chunk, data, err, want)
				}
			}
			select {
			case <-handlerDone:
				t.Fatal("stream handler completed before upstream EOF")
			default:
			}
			finish()
			var end protocol.StreamEndMessage
			decodeData(t, expectType(t, ctrl, protocol.TypeStreamEnd), &end)
			if end.RequestID != "progressive-route" || end.Reason != "complete" {
				t.Fatalf("stream end = %+v, want complete for progressive-route", end)
			}
			fd.mu.Lock()
			defer fd.mu.Unlock()
			if len(fd.doCalls) != 1 || !fd.doCalls[0].stream {
				t.Fatalf("Docker calls = %+v, want one streaming call", fd.doCalls)
			}
		})
	}
}

func TestHandleRequestStatsExplicitFalseStaysUnary(t *testing.T) {
	t.Parallel()
	c, ctrl := newTestClient(t)
	const body = `{"cpu_stats":{"cpu_usage":{"total_usage":42}}}`
	unary := mkResp(http.StatusOK, "application/json", body)
	stream := mkResp(http.StatusOK, "application/json", body)
	t.Cleanup(func() { _ = unary.Body.Close(); _ = stream.Body.Close() })
	fd := &fakeDocker{doResp: unary, streamResp: stream}
	c.dockerClient = fd
	c.handleRequest(t.Context(), protocol.RequestMessage{
		RequestID: "unary-stats", Method: http.MethodGet, Path: "/v1.44/containers/abc/stats?stream=false",
	})
	var response protocol.ResponseMessage
	decodeData(t, expectType(t, ctrl, protocol.TypeResponse), &response)
	if response.IsStream || response.RequestID != "unary-stats" || string(response.Body) != body {
		t.Fatalf("response = %+v, want unary stats body %s", response, body)
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.doCalls) != 1 || fd.doCalls[0].stream {
		t.Fatalf("Docker calls = %+v, want one unary call", fd.doCalls)
	}
}
