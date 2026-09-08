package docker

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsStreamingPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "container logs", path: "/v1.44/containers/abc/logs", want: true},
		{name: "logs with query", path: "/v1.44/containers/abc/logs?follow=1", want: true},
		{name: "attach", path: "/v1.44/containers/abc/attach", want: true},
		{name: "events", path: "/v1.44/events", want: true},
		{name: "build", path: "/v1.44/build", want: true},
		{name: "images create", path: "/v1.44/images/create?fromImage=nginx", want: true},
		{name: "images push", path: "/v1.44/images/nginx/push", want: true},
		{name: "exec start", path: "/v1.44/exec/abc/start", want: true},
		{name: "non-stream endpoint", path: "/v1.44/containers/json", want: false},
		{name: "exec inspect not stream", path: "/v1.44/exec/abc/json", want: false},

		// Large-body export endpoints: docker save (single and multi-image)
		// and docker export (container filesystem tar) must stream instead
		// of buffering, both to avoid the 100MB memory spike and because the
		// body is a binary tar, not the JSON these responses get wrapped as
		// on the non-streaming path.
		{name: "container export", path: "/v1.44/containers/abc/export", want: true},
		{name: "images get (single, named)", path: "/v1.44/images/nginx/get", want: true},
		{name: "images get (single, namespaced repo)", path: "/v1.44/images/library%2Fnginx/get", want: true},
		{name: "images get (multi-image)", path: "/v1.44/images/get?names=nginx&names=alpine", want: true},

		// Near misses: paths that share a segment with the export endpoints
		// above but are not themselves streaming responses.
		{name: "images json is not an export", path: "/v1.44/images/json", want: false},
		{name: "image inspect is not an export", path: "/v1.44/images/nginx/json", want: false},
		{name: "container archive", path: "/v1.44/containers/abc/archive", want: true},
		{name: "container archive with query", path: "/v1.44/containers/abc/archive?path=%2Fvar%2Flib%2Fdata", want: true},
		{name: "archive only in query", path: "/v1.44/containers/json?filter=%2Farchive%3F", want: false},
		{name: "images load is not an export", path: "/v1.44/images/load", want: false},
		{name: "bare /get without /images/ does not match", path: "/v1.44/secrets/abc/get", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsStreamingPath(tt.path); got != tt.want {
				t.Fatalf("IsStreamingPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsStreamingRequestContainerArchiveMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{name: "GET archive download streams", method: http.MethodGet, path: "/v1.44/containers/abc/archive?path=%2Fvar%2Flib%2Fdata", want: true},
		{name: "PUT archive upload does not stream", method: http.MethodPut, path: "/v1.44/containers/abc/archive?path=%2Fvar%2Flib%2Fdata", want: false},
		{name: "GET logs delegates to path classifier", method: http.MethodGet, path: "/v1.44/containers/abc/logs?follow=1", want: true},
		{name: "GET container list does not stream", method: http.MethodGet, path: "/v1.44/containers/json", want: false},
	}
	for _, tt := range tests {
		tc := tt
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if got := IsStreamingRequest(req.Method, req.URL.RequestURI()); got != tc.want {
				t.Fatalf("IsStreamingRequest(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

func TestStreamingStatsAndPush(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/containers/id/stats", true},
		{"GET", "/v1.44/containers/id/stats?one-shot=true", true},
		{"GET", "/containers/id/stats?stream=1", true},
		{"GET", "/containers/id/stats?stream=true", true},
		{"GET", "/containers/id/stats?stream=false&one-shot=true", false},
		{"GET", "/containers/id/stats?stream=0", false},
		{"GET", "/containers/id/stats?stream=%20FaLsE%20", false},
		{"GET", "/containers/id/stats?stream=NO", false},
		{"GET", "/containers/id/stats?stream=none", false},
		{"GET", "/containers/id/stats?stream=", false},
		{"GET", "/containers/id/stats?stream=false&stream=true", false},
		{"GET", "/containers/id/stats?stream=true&stream=false", true},
		{"POST", "/containers/id/stats", false},
		{"GET", "/containers//stats", false},
		{"GET", "/other/containers/id/stats", false},
		{"GET", "/v.44/containers/id/stats", false},
		{"GET", "/v1.x/containers/id/stats", false},
		{"POST", "/v1./images/nginx/push", false},
		{"POST", "/images/nginx/push", true},
		{"POST", "/v1.44/images/library/nginx/push", true},
		{"POST", "/images/library%2Fnginx/push", true},
		{"GET", "/images/nginx/push", false},
		{"POST", "/images/push", false},
		{"POST", "/images//push", false},
		{"POST", "/other/images/nginx/push", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			if got := IsStreamingRequest(tc.method, tc.path); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
