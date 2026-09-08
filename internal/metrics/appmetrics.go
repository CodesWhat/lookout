package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// numHistBuckets is the number of fixed histogram buckets (excluding +Inf).
const numHistBuckets = 12

// histogramBuckets are the fixed upper bounds for the request-duration
// histogram in seconds. The slice is sorted ascending; the last entry acts as
// +Inf and is always equal to the total count.
var histogramBuckets = [numHistBuckets]float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Registry holds in-process, concurrency-safe application-level metrics for
// the Portwing HTTP server. It deliberately avoids any third-party dependency
// and hand-writes Prometheus text exposition format v0.0.4.
type Registry struct {
	// requestsTotal counts HTTP requests by method+code label pair.
	requestsMu    sync.Mutex
	requestsTotal map[string]uint64 // key: "METHOD\x00CODE"

	// authFailuresTotal counts authentication failures by reason label.
	authMu            sync.Mutex
	authFailuresTotal map[string]uint64

	// rateLimitedTotal is a simple counter with no labels.
	rateLimitedTotal atomic.Uint64

	// inFlight is the current number of requests being handled.
	inFlight atomic.Int64

	// histogram for request durations in seconds.
	histMu      sync.Mutex
	histBuckets [numHistBuckets]uint64 // cumulative counts per bucket (one per bucket bound)
	histSum     float64
	histCount   uint64

	// Edge connection and backpressure state. edgeMode gates their exposition
	// so standard-mode agents do not emit a misleading disconnected gauge.
	edgeMode            atomic.Bool
	controllerConnected atomic.Bool
	reconnectsTotal     atomic.Uint64
	backpressureTotal   atomic.Uint64

	// Audit export health is sampled when metrics or export endpoints run.
	auditBufferRecords  atomic.Int64
	auditBufferCapacity atomic.Int64
	auditSinkEnabled    atomic.Bool
	auditExportSuccess  atomic.Uint64
	auditExportErrors   atomic.Uint64
}

// NewRegistry creates and returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		requestsTotal:     make(map[string]uint64),
		authFailuresTotal: make(map[string]uint64),
	}
}

// otherMethodLabel is the method label every unrecognised HTTP method shares.
const otherMethodLabel = "OTHER"

// knownMethods is the set of methods that keep their own method label. The
// method comes straight off the request line, which an unauthenticated caller
// controls, and net/http passes any RFC 9110 token through to the handler
// verbatim ("ZORKMID", "!#$%&'*+-.^_`|~9Az", ...). Labelling by the raw value
// would let such a caller add a series per request, growing this map and the
// sort WritePrometheus runs over it without bound.
var knownMethods = map[string]struct{}{
	http.MethodGet:     {},
	http.MethodHead:    {},
	http.MethodPost:    {},
	http.MethodPut:     {},
	http.MethodPatch:   {},
	http.MethodDelete:  {},
	http.MethodOptions: {},
	http.MethodConnect: {},
	http.MethodTrace:   {},
}

// methodLabel bounds the method label set: a known method keeps its own label
// and everything else collapses to otherMethodLabel. Matching is exact because
// HTTP methods are case-sensitive, so "get" is a different method from GET
// rather than a spelling of it.
func methodLabel(method string) string {
	if _, ok := knownMethods[method]; ok {
		return method
	}
	return otherMethodLabel
}

// IncRequest records a completed HTTP request with the given method and
// numeric status code. Methods outside the standard set are recorded under a
// single "OTHER" label so the label set cannot grow with caller input.
func (reg *Registry) IncRequest(method string, code int) {
	key := methodLabel(method) + "\x00" + strconv.Itoa(code)
	reg.requestsMu.Lock()
	reg.requestsTotal[key]++
	reg.requestsMu.Unlock()
}

// IncAuthFailure records an authentication failure with the supplied reason
// label. An empty reason is normalised to "unknown".
func (reg *Registry) IncAuthFailure(reason string) {
	if reason == "" {
		reason = "unknown"
	}
	reg.authMu.Lock()
	reg.authFailuresTotal[reason]++
	reg.authMu.Unlock()
}

// IncRateLimited increments the rate-limited request counter.
func (reg *Registry) IncRateLimited() {
	reg.rateLimitedTotal.Add(1)
}

// SetEdgeMode enables edge-only controller connection metrics.
func (reg *Registry) SetEdgeMode(enabled bool) {
	reg.edgeMode.Store(enabled)
}

// SetControllerConnected updates the current edge-controller connection gauge.
func (reg *Registry) SetControllerConnected(connected bool) {
	reg.controllerConnected.Store(connected)
}

// ControllerConnected reports the current edge-controller connection state.
func (reg *Registry) ControllerConnected() bool {
	return reg.controllerConnected.Load()
}

// IncReconnect records a scheduled edge-controller reconnect.
func (reg *Registry) IncReconnect() {
	reg.reconnectsTotal.Add(1)
}

// IncBackpressure records an edge connection eviction caused by outbound
// backpressure.
func (reg *Registry) IncBackpressure() {
	reg.backpressureTotal.Add(1)
}

// SetAuditState updates the current in-memory audit buffer and sink gauges.
func (reg *Registry) SetAuditState(records, capacity int, sinkEnabled bool) {
	reg.auditBufferRecords.Store(int64(records))
	reg.auditBufferCapacity.Store(int64(capacity))
	reg.auditSinkEnabled.Store(sinkEnabled)
}

// IncAuditExport records the outcome of an audit export request.
func (reg *Registry) IncAuditExport(success bool) {
	if success {
		reg.auditExportSuccess.Add(1)
		return
	}
	reg.auditExportErrors.Add(1)
}

// IncInFlight increments the in-flight gauge.
func (reg *Registry) IncInFlight() {
	reg.inFlight.Add(1)
}

// DecInFlight decrements the in-flight gauge.
func (reg *Registry) DecInFlight() {
	reg.inFlight.Add(-1)
}

// ObserveRequestDuration records a request duration (in seconds) into the
// histogram.
func (reg *Registry) ObserveRequestDuration(seconds float64) {
	reg.histMu.Lock()
	defer reg.histMu.Unlock()

	for i, bound := range histogramBuckets {
		if seconds <= bound {
			reg.histBuckets[i]++
		}
	}
	reg.histSum += seconds
	reg.histCount++
}

// EscapeLabelValue escapes a Prometheus label value per the exposition
// format spec: backslash -> \\, double-quote -> \", newline -> \n.
func EscapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// WritePrometheus appends all application metrics in Prometheus text
// exposition format v0.0.4 to b. escapeLabel must be EscapeLabelValue (or an
// equivalent) so label values are correctly escaped.
func (reg *Registry) WritePrometheus(b *strings.Builder, escapeLabel func(string) string) {
	// --- portwing_http_requests_total ---
	fmt.Fprintf(b, "# HELP portwing_http_requests_total Total HTTP requests handled by the agent, labeled by method and status code.\n")
	fmt.Fprintf(b, "# TYPE portwing_http_requests_total counter\n")

	reg.requestsMu.Lock()
	reqKeys := make([]string, 0, len(reg.requestsTotal))
	for k := range reg.requestsTotal {
		reqKeys = append(reqKeys, k)
	}
	reqCopy := make(map[string]uint64, len(reg.requestsTotal))
	for k, v := range reg.requestsTotal {
		reqCopy[k] = v
	}
	reg.requestsMu.Unlock()

	sort.Strings(reqKeys)
	for _, key := range reqKeys {
		parts := strings.SplitN(key, "\x00", 2)
		method, code := parts[0], parts[1]
		fmt.Fprintf(b, "portwing_http_requests_total{method=\"%s\",code=\"%s\"} %d\n",
			escapeLabel(method), escapeLabel(code), reqCopy[key])
	}

	// --- portwing_http_request_duration_seconds histogram ---
	fmt.Fprintf(b, "# HELP portwing_http_request_duration_seconds Histogram of HTTP request durations in seconds.\n")
	fmt.Fprintf(b, "# TYPE portwing_http_request_duration_seconds histogram\n")

	reg.histMu.Lock()
	bucketCounts := reg.histBuckets
	histSum := reg.histSum
	histCount := reg.histCount
	reg.histMu.Unlock()

	// Compute cumulative counts (buckets are already incremented cumulatively
	// in ObserveRequestDuration, so we write them directly).
	for i, bound := range histogramBuckets {
		le := formatBound(bound)
		fmt.Fprintf(b, "portwing_http_request_duration_seconds_bucket{le=\"%s\"} %d\n", le, bucketCounts[i])
	}
	fmt.Fprintf(b, "portwing_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", histCount)
	fmt.Fprintf(b, "portwing_http_request_duration_seconds_sum %g\n", histSum)
	fmt.Fprintf(b, "portwing_http_request_duration_seconds_count %d\n", histCount)

	// --- portwing_http_requests_in_flight ---
	fmt.Fprintf(b, "# HELP portwing_http_requests_in_flight Number of HTTP requests currently being handled.\n")
	fmt.Fprintf(b, "# TYPE portwing_http_requests_in_flight gauge\n")
	fmt.Fprintf(b, "portwing_http_requests_in_flight %d\n", reg.inFlight.Load())

	// --- portwing_auth_failures_total ---
	fmt.Fprintf(b, "# HELP portwing_auth_failures_total Total authentication failures, labeled by reason.\n")
	fmt.Fprintf(b, "# TYPE portwing_auth_failures_total counter\n")

	reg.authMu.Lock()
	authKeys := make([]string, 0, len(reg.authFailuresTotal))
	for k := range reg.authFailuresTotal {
		authKeys = append(authKeys, k)
	}
	authCopy := make(map[string]uint64, len(reg.authFailuresTotal))
	for k, v := range reg.authFailuresTotal {
		authCopy[k] = v
	}
	reg.authMu.Unlock()

	sort.Strings(authKeys)
	for _, reason := range authKeys {
		fmt.Fprintf(b, "portwing_auth_failures_total{reason=\"%s\"} %d\n",
			escapeLabel(reason), authCopy[reason])
	}

	// --- portwing_rate_limited_total ---
	fmt.Fprintf(b, "# HELP portwing_rate_limited_total Total requests rejected due to rate limiting.\n")
	fmt.Fprintf(b, "# TYPE portwing_rate_limited_total counter\n")
	fmt.Fprintf(b, "portwing_rate_limited_total %d\n", reg.rateLimitedTotal.Load())

	if reg.edgeMode.Load() {
		controllerConnected := 0
		if reg.controllerConnected.Load() {
			controllerConnected = 1
		}
		fmt.Fprintf(b, "# HELP portwing_edge_controller_connected Whether the edge controller WebSocket is connected (1) or disconnected (0).\n")
		fmt.Fprintf(b, "# TYPE portwing_edge_controller_connected gauge\n")
		fmt.Fprintf(b, "portwing_edge_controller_connected %d\n", controllerConnected)
		fmt.Fprintf(b, "# HELP portwing_edge_reconnects_total Total edge-controller reconnects scheduled by the agent.\n")
		fmt.Fprintf(b, "# TYPE portwing_edge_reconnects_total counter\n")
		fmt.Fprintf(b, "portwing_edge_reconnects_total %d\n", reg.reconnectsTotal.Load())
		fmt.Fprintf(b, "# HELP portwing_edge_backpressure_events_total Total edge connections evicted because the outbound queue was full.\n")
		fmt.Fprintf(b, "# TYPE portwing_edge_backpressure_events_total counter\n")
		fmt.Fprintf(b, "portwing_edge_backpressure_events_total %d\n", reg.backpressureTotal.Load())
	}

	auditSinkEnabled := 0
	if reg.auditSinkEnabled.Load() {
		auditSinkEnabled = 1
	}
	fmt.Fprintf(b, "# HELP portwing_audit_buffer_records Current number of records retained in the audit ring buffer.\n")
	fmt.Fprintf(b, "# TYPE portwing_audit_buffer_records gauge\n")
	fmt.Fprintf(b, "portwing_audit_buffer_records %d\n", reg.auditBufferRecords.Load())
	fmt.Fprintf(b, "# HELP portwing_audit_buffer_capacity Configured capacity of the audit ring buffer.\n")
	fmt.Fprintf(b, "# TYPE portwing_audit_buffer_capacity gauge\n")
	fmt.Fprintf(b, "portwing_audit_buffer_capacity %d\n", reg.auditBufferCapacity.Load())
	fmt.Fprintf(b, "# HELP portwing_audit_sink_enabled Whether an external audit log sink is enabled (1) or disabled (0).\n")
	fmt.Fprintf(b, "# TYPE portwing_audit_sink_enabled gauge\n")
	fmt.Fprintf(b, "portwing_audit_sink_enabled %d\n", auditSinkEnabled)
	fmt.Fprintf(b, "# HELP portwing_audit_exports_total Total audit export requests by outcome.\n")
	fmt.Fprintf(b, "# TYPE portwing_audit_exports_total counter\n")
	fmt.Fprintf(b, "portwing_audit_exports_total{outcome=\"success\"} %d\n", reg.auditExportSuccess.Load())
	fmt.Fprintf(b, "portwing_audit_exports_total{outcome=\"error\"} %d\n", reg.auditExportErrors.Load())
}

// formatBound formats a histogram bucket upper bound without trailing zeros
// beyond what is needed for readability. Integer values like 1.0 are emitted
// as "1", fractional values retain their significant digits.
func formatBound(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	return s
}
