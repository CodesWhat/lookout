package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newTestClient builds a Client whose httpClient and streamClient both route
// through the given httptest.Server. The apiVersion is set to "v1.44" directly
// so no negotiation request is made during construction.
func newTestClient(srv *httptest.Server) *Client {
	// Route every request to the test server by rewriting the scheme+host.
	rt := &rewriteHostTransport{
		base:    srv.Client().Transport,
		baseURL: srv.URL,
	}

	return &Client{
		socketPath:   "/var/run/docker.sock",
		apiVersion:   "v1.44",
		httpClient:   &http.Client{Transport: rt},
		streamClient: &http.Client{Transport: rt},
	}
}

// rewriteHostTransport replaces the scheme+host of every outbound request so
// it lands on the given httptest.Server instead of the real Docker daemon.
type rewriteHostTransport struct {
	base    http.RoundTripper
	baseURL string // e.g. "http://127.0.0.1:PORT"
}

func (r *rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	host := strings.TrimPrefix(r.baseURL, "http://")
	cloned.URL.Scheme = "http"
	cloned.URL.Host = host
	return r.base.RoundTrip(cloned)
}

// ---- GetAPIVersion / GetSocketPath ----

func TestGetAPIVersion(t *testing.T) {
	t.Parallel()

	c := &Client{apiVersion: "v1.45"}
	if got := c.GetAPIVersion(); got != "v1.45" {
		t.Fatalf("GetAPIVersion() = %q, want %q", got, "v1.45")
	}
}

func TestGetSocketPath(t *testing.T) {
	t.Parallel()

	c := &Client{socketPath: "/tmp/docker.sock"}
	if got := c.GetSocketPath(); got != "/tmp/docker.sock" {
		t.Fatalf("GetSocketPath() = %q, want %q", got, "/tmp/docker.sock")
	}
}

// ---- validateContainerRef ----

func TestValidateContainerRef(t *testing.T) {
	t.Parallel()

	valid := []string{
		"abc123",
		"my-container",
		"my.container",
		"my_container",
		"a",
		strings.Repeat("a", 128),
	}
	for _, id := range valid {
		if err := validateContainerRef(id); err != nil {
			t.Errorf("validateContainerRef(%q) = %v, want nil", id, err)
		}
	}

	invalid := []string{
		"",
		"-starts-with-dash",
		".starts-with-dot",
		strings.Repeat("a", 129),
		"has/slash",
		"has space",
	}
	for _, id := range invalid {
		if err := validateContainerRef(id); err == nil {
			t.Errorf("validateContainerRef(%q): expected error, got nil", id)
		}
	}
}

// ---- buildURL ----

func TestBuildURL(t *testing.T) {
	t.Parallel()

	c := &Client{apiVersion: "v1.44"}
	got := c.buildURL("/containers/json")
	want := "http://localhost/v1.44/containers/json"
	if got != want {
		t.Fatalf("buildURL = %q, want %q", got, want)
	}
}

// ---- NewClient: empty socket path ----

func TestNewClientEmptySocketPath(t *testing.T) {
	t.Parallel()

	_, err := NewClient("", 10)
	if err == nil {
		t.Fatal("NewClient with empty socket path: expected error, got nil")
	}
}

// ---- GetVersion ----

func TestGetVersion_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.44/version" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(VersionResponse{Version: "24.0.5", APIVersion: "1.44"}) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	ver, err := c.GetVersion(context.Background())
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if ver != "24.0.5" {
		t.Fatalf("GetVersion = %q, want %q", ver, "24.0.5")
	}
}

func TestGetVersion_BadJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not-json")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.GetVersion(context.Background())
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

// ---- Ping ----

func TestPing_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK")) //nolint:errcheck
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestPing_NonOK(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected error on non-200, got nil")
	}
}

// ---- ListContainers ----

func TestListContainers_Success(t *testing.T) {
	t.Parallel()

	containers := []ContainerJSON{
		{ID: "abc123", Names: []string{"/myapp"}, Image: "nginx:latest", State: "running"},
		{ID: "def456", Names: []string{"/mydb"}, Image: "postgres:15", State: "exited"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(containers) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListContainers(context.Background(), true)
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListContainers: got %d containers, want 2", len(got))
	}
	if got[0].ID != "abc123" {
		t.Fatalf("ListContainers[0].ID = %q, want %q", got[0].ID, "abc123")
	}
}

func TestListContainers_AllFalseOmitsParam(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("expected no query params when all=false, got %q", r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode([]ContainerJSON{}) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ListContainers(context.Background(), false)
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
}

func TestListContainers_AllTrueSetsParam(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.44/containers/json" {
			t.Errorf("expected path /v1.44/containers/json, got %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("all"); got != "1" {
			t.Errorf("expected all=1 query param, got %q (raw query %q)", got, r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode([]ContainerJSON{}) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ListContainers(context.Background(), true)
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
}

func TestListContainers_DockerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daemon error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ListContainers(context.Background(), true)
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestListContainers_BadJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not-json")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ListContainers(context.Background(), true)
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

// ---- InspectContainer ----

func TestInspectContainer_Success(t *testing.T) {
	t.Parallel()

	inspect := ContainerInspect{
		ID:   "abc123",
		Name: "/myapp",
		State: ContainerState{
			Status:  "running",
			Running: true,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(inspect) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.InspectContainer(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if got.ID != "abc123" {
		t.Fatalf("InspectContainer.ID = %q, want %q", got.ID, "abc123")
	}
	if !got.State.Running {
		t.Fatal("InspectContainer.State.Running should be true")
	}
}

func TestInspectContainer_InvalidRef(t *testing.T) {
	t.Parallel()

	c := &Client{apiVersion: "v1.44"}
	_, err := c.InspectContainer(context.Background(), "../evil")
	if err == nil {
		t.Fatal("expected error for invalid container ref, got nil")
	}
}

func TestInspectContainer_DockerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such container", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.InspectContainer(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestInspectContainer_BadJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{broken")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.InspectContainer(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

// ---- RemoveContainer ----

func TestRemoveContainer_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.RemoveContainer(context.Background(), "abc123", false); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
}

func TestRemoveContainer_Force(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "force=1") {
			t.Errorf("expected force=1 query param, got %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.RemoveContainer(context.Background(), "abc123", true); err != nil {
		t.Fatalf("RemoveContainer(force): %v", err)
	}
}

func TestRemoveContainer_InvalidRef(t *testing.T) {
	t.Parallel()

	c := &Client{apiVersion: "v1.44"}
	err := c.RemoveContainer(context.Background(), "../evil", false)
	if err == nil {
		t.Fatal("expected error for invalid container ref, got nil")
	}
}

func TestRemoveContainer_DockerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "container in use", http.StatusConflict)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.RemoveContainer(context.Background(), "abc123", false); err == nil {
		t.Fatal("expected error on 409, got nil")
	}
}

// ---- GetContainerLogs ----

func TestGetContainerLogs_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("log line 1\nlog line 2\n")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	rc, err := c.GetContainerLogs(context.Background(), "abc123", "100", "", "", false, false)
	if err != nil {
		t.Fatalf("GetContainerLogs: %v", err)
	}
	defer rc.Close()

	data, _ := io.ReadAll(rc)
	if !strings.Contains(string(data), "log line 1") {
		t.Fatalf("GetContainerLogs: unexpected body %q", string(data))
	}
}

func TestGetContainerLogs_InvalidRef(t *testing.T) {
	t.Parallel()

	c := &Client{apiVersion: "v1.44"}
	_, err := c.GetContainerLogs(context.Background(), "../evil", "", "", "", false, false)
	if err == nil {
		t.Fatal("expected error for invalid ref, got nil")
	}
}

func TestGetContainerLogs_DockerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such container", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.GetContainerLogs(context.Background(), "abc123", "", "", "", false, false)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

// TestDockerError_NoCloseLogOnCleanClose is the shared table-driven test for
// GetContainerLogs and GetEvents: the resp.Body.Close() error branch must
// only log when Close actually fails. A real httptest response body closes
// cleanly, so no "closing docker response body" debug line should appear for
// either code path. Not parallel (and subtests don't call t.Parallel()
// either): swaps the global slog default logger for the test's duration (see
// TestCloseConn_Error for why that's safe with the surrounding
// serial/parallel test ordering).
func TestDockerError_NoCloseLogOnCleanClose(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		errMessage string
		call       func(c *Client) error
	}{
		{
			name:       "GetContainerLogs",
			status:     http.StatusNotFound,
			errMessage: "no such container",
			call: func(c *Client) error {
				_, err := c.GetContainerLogs(context.Background(), "abc123", "", "", "", false, false)
				return err
			},
		},
		{
			name:       "GetEvents",
			status:     http.StatusForbidden,
			errMessage: "forbidden",
			call: func(c *Client) error {
				_, err := c.GetEvents(context.Background())
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tt.errMessage, tt.status)
			}))
			defer srv.Close()

			var buf bytes.Buffer
			orig := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(orig)

			c := newTestClient(srv)
			if err := tt.call(c); err == nil {
				t.Fatalf("expected error on %d, got nil", tt.status)
			}
			if strings.Contains(buf.String(), "closing docker response body") {
				t.Fatalf("did not expect a body-close debug log on a clean close, got: %s", buf.String())
			}
		})
	}
}

func TestGetContainerLogs_WithTimestampsAndSince(t *testing.T) {
	t.Parallel()

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	rc, err := c.GetContainerLogs(context.Background(), "abc123", "50", "2024-01-01", "2024-12-31", false, true)
	if err != nil {
		t.Fatalf("GetContainerLogs: %v", err)
	}
	if rc != nil {
		rc.Close()
	}

	if !strings.Contains(gotQuery, "timestamps=1") {
		t.Errorf("expected timestamps=1 in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "since=2024-01-01") {
		t.Errorf("expected since param in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "until=2024-12-31") {
		t.Errorf("expected until param in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "tail=50") {
		t.Errorf("expected tail=50 in query, got %q", gotQuery)
	}
}

func TestGetContainerLogs_Follow(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "follow=1") {
			t.Errorf("expected follow=1 in query, got %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("streaming...\n")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	rc, err := c.GetContainerLogs(context.Background(), "abc123", "", "", "", true, false)
	if err != nil {
		t.Fatalf("GetContainerLogs(follow): %v", err)
	}
	if rc != nil {
		rc.Close()
	}
}

// ---- CreateExec ----

func TestCreateExec_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(struct { //nolint:errcheck
			ID string `json:"Id"`
		}{ID: "exec-id-123"})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	id, err := c.CreateExec(context.Background(), "abc123", []string{"sh", "-c", "echo hi"}, "", false)
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}
	if id != "exec-id-123" {
		t.Fatalf("CreateExec ID = %q, want %q", id, "exec-id-123")
	}
}

func TestCreateExec_WithUser(t *testing.T) {
	t.Parallel()

	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(struct { //nolint:errcheck
			ID string `json:"Id"`
		}{ID: "exec-id-456"})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.CreateExec(context.Background(), "abc123", []string{"id"}, "root", true)
	if err != nil {
		t.Fatalf("CreateExec with user: %v", err)
	}
	if gotBody["User"] != "root" {
		t.Fatalf("exec config User = %v, want %q", gotBody["User"], "root")
	}
	if gotBody["Tty"] != true {
		t.Fatalf("exec config Tty = %v, want true", gotBody["Tty"])
	}
}

func TestCreateExec_InvalidRef(t *testing.T) {
	t.Parallel()

	c := &Client{apiVersion: "v1.44"}
	_, err := c.CreateExec(context.Background(), "../evil", []string{"sh"}, "", false)
	if err == nil {
		t.Fatal("expected error for invalid ref, got nil")
	}
}

func TestCreateExec_DockerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such container", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.CreateExec(context.Background(), "abc123", []string{"sh"}, "", false)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestCreateExec_BadJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("{broken")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.CreateExec(context.Background(), "abc123", []string{"sh"}, "", false)
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

// ---- ResizeExec ----

func TestResizeExec_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "h=") || !strings.Contains(r.URL.RawQuery, "w=") {
			t.Errorf("expected h= and w= params, got %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.ResizeExec(context.Background(), "exec-id-123", 80, 24); err != nil {
		t.Fatalf("ResizeExec: %v", err)
	}
}

func TestResizeExec_Created(t *testing.T) {
	t.Parallel()

	// Some Docker versions return 201 for resize.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.ResizeExec(context.Background(), "exec-id-123", 80, 24); err != nil {
		t.Fatalf("ResizeExec (201): %v", err)
	}
}

func TestResizeExec_DockerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("exec not found")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.ResizeExec(context.Background(), "exec-id-123", 80, 24); err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

// ---- GetEvents ----

func TestGetEvents_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(DockerEvent{ //nolint:errcheck
			ID:     "abc123",
			Action: "start",
			Type:   "container",
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	rc, err := c.GetEvents(context.Background())
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	defer rc.Close()

	data, _ := io.ReadAll(rc)
	if len(data) == 0 {
		t.Fatal("GetEvents: got empty body")
	}
}

func TestGetEvents_DockerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.GetEvents(context.Background())
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

// ---- GetDockerInfo ----

func TestGetDockerInfo_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(DockerInfo{DockerRootDir: "/var/lib/docker"}) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	info, err := c.GetDockerInfo(context.Background())
	if err != nil {
		t.Fatalf("GetDockerInfo: %v", err)
	}
	if info.DockerRootDir != "/var/lib/docker" {
		t.Fatalf("GetDockerInfo.DockerRootDir = %q, want %q", info.DockerRootDir, "/var/lib/docker")
	}
}

func TestGetDockerInfo_DockerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.GetDockerInfo(context.Background())
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestGetDockerInfo_BadJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{broken")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.GetDockerInfo(context.Background())
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

// ---- ContainerStats ----

func TestContainerStats_Success(t *testing.T) {
	t.Parallel()

	stats := ContainerStatsResponse{}
	stats.CPUStats.CPUUsage.TotalUsage = 123456789
	stats.MemoryStats.Usage = 512 * 1024 * 1024
	stats.MemoryStats.Limit = 2 * 1024 * 1024 * 1024

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(stats) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ContainerStats(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	if got.CPUStats.CPUUsage.TotalUsage != 123456789 {
		t.Fatalf("CPUStats.TotalUsage = %d, want %d", got.CPUStats.CPUUsage.TotalUsage, 123456789)
	}
}

func TestContainerStats_InvalidRef(t *testing.T) {
	t.Parallel()

	c := &Client{apiVersion: "v1.44"}
	_, err := c.ContainerStats(context.Background(), "../evil")
	if err == nil {
		t.Fatal("expected error for invalid ref, got nil")
	}
}

func TestContainerStats_DockerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such container", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ContainerStats(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestContainerStats_BadJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{broken")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ContainerStats(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

// ---- Do / DoStream: Content-Type header ----

func TestDo_SetsContentTypeForBodyRequests(t *testing.T) {
	t.Parallel()

	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	body := strings.NewReader(`{"foo":"bar"}`)
	resp, err := c.Do(context.Background(), http.MethodPost, "/some/path", body)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want %q", gotContentType, "application/json")
	}
}

func TestDo_NoContentTypeForNilBody(t *testing.T) {
	t.Parallel()

	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	resp, err := c.Do(context.Background(), http.MethodGet, "/some/path", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if gotContentType != "" {
		t.Fatalf("expected no Content-Type for nil body, got %q", gotContentType)
	}
}

func TestDoWithHeadersPreservesDockerMetadata(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	headers := http.Header{
		"Content-Type":    []string{"application/vnd.docker.raw-stream"},
		"X-Registry-Auth": []string{"registry-credential"},
	}
	resp, err := c.DoWithHeaders(context.Background(), http.MethodPost, "/images/create", headers, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("DoWithHeaders: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	gotContentType := got.Get("Content-Type")
	gotRegistryAuth := got.Get("X-Registry-Auth")
	mu.Unlock()
	if gotContentType != "application/vnd.docker.raw-stream" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotRegistryAuth != "registry-credential" {
		t.Errorf("X-Registry-Auth = %q", gotRegistryAuth)
	}
}

func TestDoStream_SetsContentTypeForBodyRequests(t *testing.T) {
	t.Parallel()

	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	body := strings.NewReader(`{"foo":"bar"}`)
	resp, err := c.DoStream(context.Background(), http.MethodPost, "/stream/path", body)
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	resp.Body.Close()

	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want %q", gotContentType, "application/json")
	}
}

func TestDoStreamWithHeadersPreservesDockerMetadata(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotRegistryAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotRegistryAuth = r.Header.Get("X-Registry-Auth")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	resp, err := c.DoStreamWithHeaders(
		context.Background(),
		http.MethodPost,
		"/images/create",
		http.Header{"X-Registry-Auth": []string{"stream-registry-credential"}},
		nil,
	)
	if err != nil {
		t.Fatalf("DoStreamWithHeaders: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	gotAuth := gotRegistryAuth
	mu.Unlock()
	if gotAuth != "stream-registry-credential" {
		t.Errorf("X-Registry-Auth = %q", gotAuth)
	}
}

// ---- negotiateAPIVersion ----

func TestNegotiateAPIVersion_SetsVersion(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(VersionResponse{Version: "24.0.5", APIVersion: "1.45"}) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	c.apiVersion = ""

	if err := c.negotiateAPIVersion(context.Background()); err != nil {
		t.Fatalf("negotiateAPIVersion: %v", err)
	}
	if c.apiVersion != "v1.45" {
		t.Fatalf("apiVersion = %q, want %q", c.apiVersion, "v1.45")
	}
}

func TestNegotiateAPIVersion_EmptyAPIVersionFallback(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(VersionResponse{Version: "24.0.5", APIVersion: ""}) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	c.apiVersion = ""

	if err := c.negotiateAPIVersion(context.Background()); err != nil {
		t.Fatalf("negotiateAPIVersion: %v", err)
	}
	if c.apiVersion != "v1.44" {
		t.Fatalf("apiVersion = %q, want fallback %q", c.apiVersion, "v1.44")
	}
}

func TestNegotiateAPIVersion_BadJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not-json")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.negotiateAPIVersion(context.Background()); err == nil {
		t.Fatal("expected error on bad JSON, got nil")
	}
}

// ---- DoRaw / DoStreamRaw ----

func TestDoRaw_ForwardsRequest(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/some/raw/path", nil)
	resp, err := c.DoRaw(req)
	if err != nil {
		t.Fatalf("DoRaw: %v", err)
	}
	resp.Body.Close()

	if gotPath != "/some/raw/path" {
		t.Fatalf("DoRaw path = %q, want %q", gotPath, "/some/raw/path")
	}
}

func TestDoStreamRaw_ForwardsRequest(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/stream/raw/path", nil)
	resp, err := c.DoStreamRaw(req)
	if err != nil {
		t.Fatalf("DoStreamRaw: %v", err)
	}
	resp.Body.Close()

	if gotPath != "/stream/raw/path" {
		t.Fatalf("DoStreamRaw path = %q, want %q", gotPath, "/stream/raw/path")
	}
}

// ---- bufferedConn ----

func TestBufferedConn_Read(t *testing.T) {
	t.Parallel()

	// bufferedConn wraps a net.Conn with a bufio.Reader. We test the Read
	// path by constructing one directly with a pipe as the underlying conn.
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("hello buffered world")) //nolint:errcheck
		_ = pw.Close()
	}()

	br := bufio.NewReader(pr)
	// Read one byte to put something in the buffer.
	_, _ = br.ReadByte()
	_ = br.UnreadByte()

	bc := &bufferedConn{Conn: nil, reader: br}
	buf := make([]byte, 5)
	n, err := bc.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("bufferedConn.Read: %v", err)
	}
	if n == 0 {
		t.Fatal("bufferedConn.Read: got 0 bytes")
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("bufferedConn.Read: got %q, want %q", string(buf[:n]), "hello")
	}
}

// errTransport always returns a network error, simulating connection failures.
type errTransport struct{ err error }

func (e *errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, e.err
}

// newErrClient returns a Client whose both httpClient and streamClient always
// return a transport-level error (not a bad HTTP status).
func newErrClient() *Client {
	rt := &errTransport{err: fmt.Errorf("connection refused")}
	return &Client{
		socketPath:   "/var/run/docker.sock",
		apiVersion:   "v1.44",
		httpClient:   &http.Client{Transport: rt},
		streamClient: &http.Client{Transport: rt},
	}
}

// ---- Request-level errors (transport fails) ----

func TestGetVersion_RequestError(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	_, err := c.GetVersion(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestPing_RequestError(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestListContainers_RequestError(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	_, err := c.ListContainers(context.Background(), true)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestInspectContainer_RequestError(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	_, err := c.InspectContainer(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestRemoveContainer_RequestError(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	err := c.RemoveContainer(context.Background(), "abc123", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetContainerLogs_RequestError_NoFollow(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	_, err := c.GetContainerLogs(context.Background(), "abc123", "", "", "", false, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetContainerLogs_RequestError_Follow(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	_, err := c.GetContainerLogs(context.Background(), "abc123", "", "", "", true, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestCreateExec_RequestError(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	_, err := c.CreateExec(context.Background(), "abc123", []string{"sh"}, "", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestResizeExec_RequestError(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	err := c.ResizeExec(context.Background(), "exec-id-123", 80, 24)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetEvents_RequestError(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	_, err := c.GetEvents(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetDockerInfo_RequestError(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	_, err := c.GetDockerInfo(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestContainerStats_RequestError(t *testing.T) {
	t.Parallel()
	c := newErrClient()
	_, err := c.ContainerStats(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---- Do/DoStream request creation error (invalid method character) ----

func TestDo_InvalidMethod(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	// A null byte in the method name causes http.NewRequest to return an error.
	resp, err := c.Do(context.Background(), "INVALID\x00METHOD", "/path", nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error for invalid method, got nil")
	}
	if !strings.Contains(err.Error(), "creating Docker API request") {
		t.Fatalf("error = %q, want request-construction context", err)
	}
}

func TestDoStream_InvalidMethod(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	resp, err := c.DoStream(context.Background(), "INVALID\x00METHOD", "/path", nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error for invalid method, got nil")
	}
	if !strings.Contains(err.Error(), "creating Docker API request") {
		t.Fatalf("error = %q, want request-construction context", err)
	}
}

func TestDo_TransportErrorIncludesOperation(t *testing.T) {
	t.Parallel()

	c := newErrClient()
	resp, err := c.Do(context.Background(), http.MethodGet, "/containers/json", nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if !strings.Contains(err.Error(), "sending Docker API request GET /containers/json") {
		t.Fatalf("error = %q, want request-operation context", err)
	}
}

// ---- RemoveContainer returns 200 OK (treated as success) ----

func TestRemoveContainer_StatusOK(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.RemoveContainer(context.Background(), "abc123", false); err != nil {
		t.Fatalf("RemoveContainer with 200: %v", err)
	}
}

// ---- ResizeExec body read error ----

// errBodyTransport serves a response where the body errors when read.
type errBodyTransport struct {
	status  int
	bodyErr error
}

type errBody struct{ err error }

func (b *errBody) Read(_ []byte) (int, error) { return 0, b.err }
func (b *errBody) Close() error               { return nil }

func (t *errBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: t.status,
		Body:       &errBody{err: t.bodyErr},
		Header:     make(http.Header),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}, nil
}

func TestResizeExec_BodyReadError(t *testing.T) {
	t.Parallel()

	rt := &errBodyTransport{status: http.StatusNotFound, bodyErr: fmt.Errorf("body read failed")}
	c := &Client{
		socketPath:   "/var/run/docker.sock",
		apiVersion:   "v1.44",
		httpClient:   &http.Client{Transport: rt},
		streamClient: &http.Client{Transport: rt},
	}
	err := c.ResizeExec(context.Background(), "exec-id-123", 80, 24)
	if err == nil {
		t.Fatal("expected error on body read failure, got nil")
	}
	// The body couldn't be read, so no message is available; the error still
	// carries the status code via the shared dockerError helper.
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %q, expected to contain status 404", err.Error())
	}
}

// ---- DoStream with nil body (no Content-Type) ----

func TestDoStream_NoContentTypeForNilBody(t *testing.T) {
	t.Parallel()

	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	resp, err := c.DoStream(context.Background(), http.MethodGet, "/stream/path", nil)
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()

	if gotContentType != "" {
		t.Fatalf("expected no Content-Type for nil body, got %q", gotContentType)
	}
}

// ---- negotiateAPIVersion request error ----

func TestNegotiateAPIVersion_RequestError(t *testing.T) {
	t.Parallel()

	c := newErrClient()
	// Should return an error (version request fails)
	err := c.negotiateAPIVersion(context.Background())
	if err == nil {
		t.Fatal("expected error from negotiateAPIVersion, got nil")
	}
}

// ---- extractDockerErrorMessage ----

func TestExtractDockerErrorMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
		want string
	}{
		{
			name: "docker JSON message is extracted",
			body: []byte(`{"message":"exec denied: no commands are allowlisted"}`),
			want: "exec denied: no commands are allowlisted",
		},
		{
			name: "raw non-JSON body is trimmed and returned as-is",
			body: []byte("  no such container\n"),
			want: "no such container",
		},
		{
			name: "empty body yields empty message",
			body: []byte(""),
			want: "",
		},
		{
			name: "whitespace-only body yields empty message",
			body: []byte("   \n\t  "),
			want: "",
		},
		{
			name: "valid JSON with empty message falls back to raw trimmed body",
			body: []byte(`  {"message":""}  `),
			want: `{"message":""}`,
		},
		{
			name: "sockguard verbose denial: message and reason are combined",
			body: []byte(`{"message":"request denied by sockguard policy","method":"POST","path":"/containers/create","reason":"not allowed by portwing preset"}`),
			want: "request denied by sockguard policy: not allowed by portwing preset",
		},
		{
			name: "reason-only body yields the reason",
			body: []byte(`{"reason":"exec denied: privileged exec is not allowed"}`),
			want: "exec denied: privileged exec is not allowed",
		},
		{
			name: "identical message and reason are not duplicated",
			body: []byte(`{"message":"same text","reason":"same text"}`),
			want: "same text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractDockerErrorMessage(tt.body)
			if got != tt.want {
				t.Errorf("extractDockerErrorMessage(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

// ---- dockerError ----

func TestDockerError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		action string
		status int
		body   []byte
		want   string
	}{
		{
			name:   "message present",
			action: "create exec",
			status: http.StatusForbidden,
			body:   []byte(`{"message":"exec denied: no commands are allowlisted"}`),
			want:   "create exec: docker error (status 403): exec denied: no commands are allowlisted",
		},
		{
			name:   "empty body yields bare status error",
			action: "resize exec",
			status: http.StatusNotFound,
			body:   nil,
			want:   "resize exec: docker error (status 404)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := dockerError(tt.action, tt.status, tt.body)
			if err == nil {
				t.Fatal("expected non-nil error")
			}
			if err.Error() != tt.want {
				t.Errorf("dockerError(...) = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

// ---- Call-site propagation: the extracted body message reaches the caller ----

func TestCreateExec_DockerError_MessageBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"exec denied: no commands are allowlisted"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.CreateExec(context.Background(), "abc123", []string{"sh"}, "", false)
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %q, expected to contain status 403", err.Error())
	}
	if !strings.Contains(err.Error(), "exec denied: no commands are allowlisted") {
		t.Errorf("error = %q, expected to contain the docker error message", err.Error())
	}
}

func TestResizeExec_DockerError_MessageBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"exec denied: no commands are allowlisted"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	err := c.ResizeExec(context.Background(), "exec-id-123", 80, 24)
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %q, expected to contain status 403", err.Error())
	}
	if !strings.Contains(err.Error(), "exec denied: no commands are allowlisted") {
		t.Errorf("error = %q, expected to contain the docker error message", err.Error())
	}
}

func TestGetContainerLogs_DockerError_MessageBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"exec denied: no commands are allowlisted"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.GetContainerLogs(context.Background(), "abc123", "", "", "", false, false)
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %q, expected to contain status 403", err.Error())
	}
	if !strings.Contains(err.Error(), "exec denied: no commands are allowlisted") {
		t.Errorf("error = %q, expected to contain the docker error message", err.Error())
	}
}

func TestCreateExec_DockerError_MessageAndReasonBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"request denied by sockguard policy","method":"POST","path":"/containers/abc123/exec","reason":"exec denied: privileged exec is not allowed"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.CreateExec(context.Background(), "abc123", []string{"sh"}, "", false)
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %q, expected to contain status 403", err.Error())
	}
	if !strings.Contains(err.Error(), "request denied by sockguard policy: exec denied: privileged exec is not allowed") {
		t.Errorf("error = %q, expected to contain the combined message and reason", err.Error())
	}
}

// ---- Truncation: the error body is bounded to maxDockerErrorBodyBytes ----

func TestCreateExec_DockerError_BodyTruncated(t *testing.T) {
	t.Parallel()

	const tailMarker = "TAIL_MARKER_SHOULD_NOT_APPEAR"
	oversized := strings.Repeat("a", maxDockerErrorBodyBytes+1024) + tailMarker

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(oversized)) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.CreateExec(context.Background(), "abc123", []string{"sh"}, "", false)
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	if strings.Contains(err.Error(), tailMarker) {
		t.Errorf("error = %q, expected the body to be truncated before the tail marker", err.Error())
	}
	// The message portion of the error can carry at most maxDockerErrorBodyBytes
	// of body content; the rest of the error text (action/status wrapper) is
	// fixed overhead, so bound the whole error generously above that.
	if len(err.Error()) > maxDockerErrorBodyBytes+128 {
		t.Errorf("error length = %d, expected roughly bounded by maxDockerErrorBodyBytes (%d)", len(err.Error()), maxDockerErrorBodyBytes)
	}
}

// ---- StatusCodeForError ----

func TestStatusCodeForError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "404 maps to not found",
			err:  dockerError("remove container", http.StatusNotFound, []byte(`{"message":"No such container: abc"}`)),
			want: http.StatusNotFound,
		},
		{
			name: "409 maps to conflict",
			err:  dockerError("remove container", http.StatusConflict, []byte(`{"message":"container is running"}`)),
			want: http.StatusConflict,
		},
		{
			name: "other docker statuses collapse to 500",
			err:  dockerError("get logs", http.StatusBadRequest, []byte(`{"message":"bad parameter"}`)),
			want: http.StatusInternalServerError,
		},
		{
			name: "a non-docker error collapses to 500",
			err:  errors.New("dial unix /var/run/docker.sock: connect: connection refused"),
			want: http.StatusInternalServerError,
		},
		{
			name: "a nil error collapses to 500 without panicking",
			err:  nil,
			want: http.StatusInternalServerError,
		},
		{
			name: "a wrapped docker error is still resolved",
			err:  fmt.Errorf("getting logs: %w", dockerError("get logs", http.StatusNotFound, nil)),
			want: http.StatusNotFound,
		},
		{
			// The status used to be recovered by searching the formatted
			// message for "status 404". That message embeds the Docker error
			// body, which echoes the caller-supplied container name, so a
			// container named "status 404" turned a 500 into a 404.
			name: "a container name echoed in the body cannot forge the status",
			err:  dockerError("get logs", http.StatusInternalServerError, []byte(`{"message":"No such container: status 404"}`)),
			want: http.StatusInternalServerError,
		},
		{
			name: "an echoed name cannot forge a conflict either",
			err:  dockerError("remove container", http.StatusInternalServerError, []byte(`{"message":"No such container: status 409"}`)),
			want: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := StatusCodeForError(tt.err); got != tt.want {
				t.Fatalf("StatusCodeForError() = %d, want %d", got, tt.want)
			}
		})
	}
}
func TestNegotiateAPIVersion_NilContext(t *testing.T) {
	t.Parallel()

	c := &Client{apiVersion: "v1.44"}
	//nolint:staticcheck // nil context is intentional to exercise the error branch
	err := c.negotiateAPIVersion(nil) //nolint:staticcheck
	if err == nil {
		t.Fatal("expected error from nil context, got nil")
	}
}

// ---- Ping: NewRequestWithContext error (nil context) ----

// TestPing_NilContext forces the http.NewRequestWithContext error branch
// in Ping by passing a nil context.
func TestPing_NilContext(t *testing.T) {
	t.Parallel()

	c := &Client{apiVersion: "v1.44"}
	//nolint:staticcheck // nil context is intentional to exercise the error branch
	err := c.Ping(nil) //nolint:staticcheck
	if err == nil {
		t.Fatal("expected error from nil context, got nil")
	}
}

// ---- writeStackFiles: resolvePath error for .env.drydock ----
// The resolvePath(".env.drydock") call inside writeStackFiles can only fail
// if the ComposeManager has an empty stacksDir combined with a
// stackDir/stackName that causes resolveStackRoot to error.  We trigger this
// by supplying a StackDir that is an absolute path (rejected by
// resolveStackRoot before ".env.drydock" is even resolved).
//
// Note: validateRequest would normally catch this first, so we call
// writeStackFiles directly.
