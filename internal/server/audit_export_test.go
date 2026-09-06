package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codeswhat/portwing/internal/audit"
	"github.com/codeswhat/portwing/internal/metrics"
)

// makeAuditTestServer builds a minimal Server with a real audit Logger that
// has an in-memory ring buffer. The Docker client field is not needed by
// handleAudit, so we pass nil.
func makeAuditTestServer(t *testing.T, bufferSize int) *Server {
	t.Helper()
	l, cleanup, err := audit.New("", bufferSize)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(cleanup)
	return &Server{auditor: l}
}

// auditResponse matches the JSON envelope returned by handleAudit.
type auditResponse struct {
	Records []audit.Record `json:"records"`
	Count   int            `json:"count"`
}

func TestHandleAuditEmpty(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 8)

	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit", nil)
	rr := httptest.NewRecorder()
	s.handleAudit(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}

	var resp auditResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Count != 0 {
		t.Errorf("expected count=0, got %d", resp.Count)
	}
	if resp.Records == nil {
		t.Error("records must not be null on empty buffer")
	}
	if len(resp.Records) != 0 {
		t.Errorf("expected empty records slice, got %d entries", len(resp.Records))
	}
}

func TestHandleAuditRecordsNewestFirst(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 8)

	// Push two events in order: auth_failure then api_request.
	s.auditor.AuthFailure("1.2.3.4", "GET", "/first")
	s.auditor.APIRequest("1.2.3.4", "GET", "/second", audit.OutcomeAllowed, 200, 1.5)

	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit", nil)
	rr := httptest.NewRecorder()
	s.handleAudit(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp auditResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Count != 2 {
		t.Fatalf("expected count=2, got %d", resp.Count)
	}
	// Newest-first: api_request was pushed last.
	if resp.Records[0].Event != audit.EventAPIRequest {
		t.Errorf("records[0].Event = %q, want %q", resp.Records[0].Event, audit.EventAPIRequest)
	}
	if resp.Records[1].Event != audit.EventAuthFailure {
		t.Errorf("records[1].Event = %q, want %q", resp.Records[1].Event, audit.EventAuthFailure)
	}
}

func TestHandleAuditLimitParam(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 8)

	s.auditor.AuthFailure("a", "GET", "/1")
	s.auditor.AuthFailure("b", "GET", "/2")
	s.auditor.APIRequest("c", "GET", "/3", audit.OutcomeAllowed, 200, 0.5)

	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit?limit=1", nil)
	rr := httptest.NewRecorder()
	s.handleAudit(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp auditResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("expected count=1 with limit=1, got %d", resp.Count)
	}
	// Should be the newest record.
	if resp.Records[0].Event != audit.EventAPIRequest {
		t.Errorf("records[0].Event = %q, want api_request (newest)", resp.Records[0].Event)
	}
}

func TestHandleAuditDisabledBuffer(t *testing.T) {
	t.Parallel()

	// bufferSize=0 disables the buffer entirely.
	s := makeAuditTestServer(t, 0)

	s.auditor.AuthFailure("x", "GET", "/y")

	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit", nil)
	rr := httptest.NewRecorder()
	s.handleAudit(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp auditResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Count != 0 {
		t.Errorf("expected count=0 with disabled buffer, got %d", resp.Count)
	}
}

func TestHandleAuditInvalidLimitFallsBackToAll(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 8)

	s.auditor.AuthFailure("a", "GET", "/1")
	s.auditor.AuthFailure("b", "GET", "/2")

	// Invalid limit should fall back to all records.
	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit?limit=notanumber", nil)
	rr := httptest.NewRecorder()
	s.handleAudit(rr, req)

	var resp auditResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Count != 2 {
		t.Errorf("expected count=2 for invalid limit, got %d", resp.Count)
	}
}

func TestHandleAuditExportNDJSONCursor(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 8)
	s.metrics = metrics.NewRegistry()
	s.auditor.AuthFailure("a", "GET", "/1")
	s.auditor.AuthFailure("b", "GET", "/2")
	s.auditor.APIRequest("c", "GET", "/3", audit.OutcomeAllowed, 200, 0.5)

	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit/export?cursor=1&limit=1", nil)
	rr := httptest.NewRecorder()
	s.handleAuditExport(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/x-ndjson" {
		t.Fatalf("content type = %q, want application/x-ndjson", got)
	}
	if got := rr.Header().Get("X-Portwing-Next-Cursor"); got != "2" {
		t.Fatalf("next cursor = %q, want 2", got)
	}
	if got := rr.Header().Get("X-Portwing-Record-Count"); got != "1" {
		t.Fatalf("record count = %q, want 1", got)
	}

	scanner := bufio.NewScanner(rr.Body)
	if !scanner.Scan() {
		t.Fatal("expected one NDJSON record")
	}
	var record audit.Record
	if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
		t.Fatalf("decode NDJSON record: %v", err)
	}
	if record.Cursor != 2 || record.Path != "/2" {
		t.Fatalf("record = %+v, want cursor 2 and path /2", record)
	}
	if scanner.Scan() {
		t.Fatalf("unexpected second NDJSON record: %s", scanner.Text())
	}

	metricsBody := new(strings.Builder)
	s.metrics.WritePrometheus(metricsBody, func(value string) string { return value })
	if !strings.Contains(metricsBody.String(), `portwing_audit_exports_total{outcome="success"} 1`) {
		t.Fatalf("missing successful audit export metric:\n%s", metricsBody.String())
	}
}

func TestHandleAuditExportRejectsInvalidCursor(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 8)
	s.metrics = metrics.NewRegistry()
	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit/export?cursor=invalid", nil)
	rr := httptest.NewRecorder()
	s.handleAuditExport(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}

	var b strings.Builder
	s.metrics.WritePrometheus(&b, func(value string) string { return value })
	if !strings.Contains(b.String(), `portwing_audit_exports_total{outcome="error"} 1`) {
		t.Fatalf("missing failed audit export metric:\n%s", b.String())
	}
}

func TestHandleAuditExportRejectsInvalidLimit(t *testing.T) {
	t.Parallel()

	for _, limit := range []string{"invalid", "-1"} {
		s := makeAuditTestServer(t, 8)
		req := httptest.NewRequest(http.MethodGet, "/_portwing/audit/export?limit="+limit, nil)
		rr := httptest.NewRecorder()
		s.handleAuditExport(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("limit %q status = %d, want 400", limit, rr.Code)
		}
	}
}

func TestAuditResetCursorEmptyWindow(t *testing.T) {
	t.Parallel()

	if got := auditResetCursor(audit.Stats{}); got != 0 {
		t.Fatalf("reset cursor = %d, want 0", got)
	}
}

func TestHandleAuditExportReportsCursorGap(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 2)
	s.metrics = metrics.NewRegistry()
	s.auditor.AuthFailure("a", "GET", "/1")
	s.auditor.AuthFailure("b", "GET", "/2")
	s.auditor.AuthFailure("c", "GET", "/3")

	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit/export?cursor=0", nil)
	rr := httptest.NewRecorder()
	s.handleAuditExport(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for overwritten cursor: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Portwing-Oldest-Cursor"); got != "2" {
		t.Fatalf("oldest cursor = %q, want 2", got)
	}
}

func TestHandleAuditExportWithoutCursorBootstrapsAtOldestRetainedRecord(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 2)
	s.auditor.AuthFailure("a", "GET", "/1")
	s.auditor.AuthFailure("b", "GET", "/2")
	s.auditor.AuthFailure("c", "GET", "/3")

	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit/export", nil)
	rr := httptest.NewRecorder()
	s.handleAuditExport(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 bootstrap response, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Portwing-Next-Cursor"); got != "3" {
		t.Fatalf("next cursor = %q, want 3", got)
	}

	scanner := bufio.NewScanner(rr.Body)
	var cursors []uint64
	for scanner.Scan() {
		var record audit.Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode record: %v", err)
		}
		cursors = append(cursors, record.Cursor)
	}
	if len(cursors) != 2 || cursors[0] != 2 || cursors[1] != 3 {
		t.Fatalf("bootstrap cursors = %v, want [2 3]", cursors)
	}
}

func TestHandleAuditExportDetectsCursorAheadOfCurrentGeneration(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 2)
	s.auditor.AuthFailure("a", "GET", "/1")

	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit/export?cursor=99", nil)
	rr := httptest.NewRecorder()
	s.handleAuditExport(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for future cursor, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Portwing-Next-Cursor"); got != "0" {
		t.Fatalf("reset cursor = %q, want 0", got)
	}
}

func TestAuditRecordsCarryMonotonicCursors(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 8)
	s.auditor.AuthFailure("a", "GET", "/1")
	s.auditor.AuthFailure("b", "GET", "/2")

	records := s.auditor.Records(0)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if records[0].Cursor != 2 || records[1].Cursor != 1 {
		t.Fatalf("cursors = %d,%d, want 2,1", records[0].Cursor, records[1].Cursor)
	}
}

// failingWriteResponseWriter is an http.ResponseWriter whose Write always
// fails, forcing json.Encoder.Encode to return an error. Used to exercise
// handleAudit's encode-failure path.
type failingWriteResponseWriter struct {
	hdr http.Header
}

func (w *failingWriteResponseWriter) Header() http.Header { return w.hdr }
func (w *failingWriteResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
func (w *failingWriteResponseWriter) WriteHeader(int) {}

// TestHandleAuditEncodeFailureRecordsFailureMetric verifies both sides of the
// `err != nil` check in handleAudit's json.Encode: an encode failure records
// a failure metric and does not record a success, while a normal (encode
// succeeds) request records success and no failure. This kills the
// CONDITIONALS_NEGATION mutant at audit_export.go:42:49.
func TestHandleAuditEncodeFailureRecordsFailureMetric(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 8)
	s.metrics = metrics.NewRegistry()
	s.auditor.AuthFailure("a", "GET", "/1")

	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit", nil)
	w := &failingWriteResponseWriter{hdr: make(http.Header)}
	s.handleAudit(w, req)

	var b strings.Builder
	s.metrics.WritePrometheus(&b, func(value string) string { return value })
	body := b.String()
	if !strings.Contains(body, `portwing_audit_exports_total{outcome="error"} 1`) {
		t.Fatalf("missing failed audit export metric after encode failure:\n%s", body)
	}
	if strings.Contains(body, `portwing_audit_exports_total{outcome="success"} 1`) {
		t.Fatalf("unexpected success metric recorded after encode failure:\n%s", body)
	}
}

// TestHandleAuditEncodeSuccessRecordsSuccessMetric is the complementary case
// to TestHandleAuditEncodeFailureRecordsFailureMetric: a normal request that
// encodes successfully must record a success and no failure.
func TestHandleAuditEncodeSuccessRecordsSuccessMetric(t *testing.T) {
	t.Parallel()

	s := makeAuditTestServer(t, 8)
	s.metrics = metrics.NewRegistry()
	s.auditor.AuthFailure("a", "GET", "/1")

	req := httptest.NewRequest(http.MethodGet, "/_portwing/audit", nil)
	rr := httptest.NewRecorder()
	s.handleAudit(rr, req)

	var b strings.Builder
	s.metrics.WritePrometheus(&b, func(value string) string { return value })
	body := b.String()
	if !strings.Contains(body, `portwing_audit_exports_total{outcome="success"} 1`) {
		t.Fatalf("missing successful audit export metric:\n%s", body)
	}
	if strings.Contains(body, `portwing_audit_exports_total{outcome="error"} 1`) {
		t.Fatalf("unexpected failure metric recorded on successful encode:\n%s", body)
	}
}
