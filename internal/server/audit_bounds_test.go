package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codeswhat/portwing/internal/audit"
)

func TestAuditMiddlewareBoundsRequestLineRetention(t *testing.T) {
	t.Parallel()
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("rate-limited=%t", blocked), func(t *testing.T) {
			logger, cleanup, err := audit.New("", 4)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup)
			rl := NewRateLimiter()
			t.Cleanup(rl.Stop)
			if blocked {
				for i := 0; i < 10; i++ {
					rl.RecordFailure("127.0.0.1")
				}
			}
			handler := rl.AuthMiddlewareWithEd25519(newRawTokenVerifier("correct"), Ed25519Config{}, logger, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("unauthenticated request reached downstream handler")
				w.WriteHeader(http.StatusInternalServerError)
			}))
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			client := server.Client()
			client.Timeout = 5 * time.Second
			for i, request := range []struct{ method, path, retainedPath string }{
				{"GET", "/first/" + strings.Repeat("a", (1<<20)-4096), ""},
				{"GET", "/second/" + strings.Repeat("b", (1<<20)-4096), ""},
				{"GET", "/short?query=" + strings.Repeat("c", (1<<20)-4096), "/short"},
				{strings.Repeat("M", 64<<10), "/method", "/method"},
			} {
				req, err := http.NewRequest(request.method, server.URL+request.path, nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, readErr := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				wantStatus, wantEvent := http.StatusUnauthorized, audit.EventAuthFailure
				if blocked {
					wantStatus, wantEvent = http.StatusTooManyRequests, audit.EventRateLimited
				}
				if resp.StatusCode != wantStatus {
					t.Fatalf("request %d: status=%d, want %d", i, resp.StatusCode, wantStatus)
				}
				records := logger.Records(0)
				if len(records) != i+1 {
					t.Fatalf("records=%d, want %d", len(records), i+1)
				}
				r := records[0]
				if r.Event != wantEvent {
					t.Errorf("event=%s, want %s", r.Event, wantEvent)
				}
				if len(r.Path) > 4096 || len(r.Method) > 64 || len(r.Actor) > 256 {
					t.Errorf("request %d retained path=%d method=%d actor=%d", i, len(r.Path), len(r.Method), len(r.Actor))
				}
				if request.retainedPath != "" && r.Path != request.retainedPath {
					t.Error("short path changed or retained query")
				}
			}
		})
	}
}
