package metrics_test

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/codeswhat/portwing/internal/metrics"
)

// noEscape is a no-op label escaper used when we want to test raw output.
func noEscape(s string) string { return s }

func output(reg *metrics.Registry) string {
	var b strings.Builder
	reg.WritePrometheus(&b, metrics.EscapeLabelValue)
	return b.String()
}

// TestRegistryCounters checks that IncRequest and IncAuthFailure and
// IncRateLimited are reflected in WritePrometheus output.
func TestRegistryCounters(t *testing.T) {
	t.Parallel()

	reg := metrics.NewRegistry()
	reg.IncRequest("GET", 200)
	reg.IncRequest("GET", 200)
	reg.IncRequest("POST", 201)
	reg.IncAuthFailure("bad_token")
	reg.IncAuthFailure("bad_token")
	reg.IncAuthFailure("replay")
	reg.IncRateLimited()
	reg.IncRateLimited()
	reg.IncRateLimited()

	body := output(reg)

	// TYPE lines.
	if !strings.Contains(body, "# TYPE portwing_http_requests_total counter") {
		t.Errorf("missing TYPE for http_requests_total; output:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE portwing_auth_failures_total counter") {
		t.Errorf("missing TYPE for auth_failures_total; output:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE portwing_rate_limited_total counter") {
		t.Errorf("missing TYPE for rate_limited_total; output:\n%s", body)
	}

	// Counter values.
	if !strings.Contains(body, `portwing_http_requests_total{method="GET",code="200"} 2`) {
		t.Errorf("expected GET/200 count 2; output:\n%s", body)
	}
	if !strings.Contains(body, `portwing_http_requests_total{method="POST",code="201"} 1`) {
		t.Errorf("expected POST/201 count 1; output:\n%s", body)
	}
	if !strings.Contains(body, `portwing_auth_failures_total{reason="bad_token"} 2`) {
		t.Errorf("expected bad_token count 2; output:\n%s", body)
	}
	if !strings.Contains(body, `portwing_auth_failures_total{reason="replay"} 1`) {
		t.Errorf("expected replay count 1; output:\n%s", body)
	}
	if !strings.Contains(body, "portwing_rate_limited_total 3") {
		t.Errorf("expected rate_limited_total 3; output:\n%s", body)
	}
}

// TestRegistryInFlight verifies the in-flight gauge increases and decreases.
func TestRegistryInFlight(t *testing.T) {
	t.Parallel()

	reg := metrics.NewRegistry()
	reg.IncInFlight()
	reg.IncInFlight()

	body := output(reg)
	if !strings.Contains(body, "# TYPE portwing_http_requests_in_flight gauge") {
		t.Errorf("missing TYPE for in_flight gauge; output:\n%s", body)
	}
	if !strings.Contains(body, "portwing_http_requests_in_flight 2") {
		t.Errorf("expected in_flight 2; output:\n%s", body)
	}

	reg.DecInFlight()
	body = output(reg)
	if !strings.Contains(body, "portwing_http_requests_in_flight 1") {
		t.Errorf("expected in_flight 1 after decrement; output:\n%s", body)
	}
}

// TestRegistryHistogram checks histogram output:
// - cumulative bucket counts (later buckets >= earlier ones)
// - +Inf bucket equals total count
// - correct _sum and _count
// - TYPE line present
func TestRegistryHistogram(t *testing.T) {
	t.Parallel()

	reg := metrics.NewRegistry()
	// 0.002s falls in the 0.005 bucket and all higher buckets.
	reg.ObserveRequestDuration(0.002)
	// 0.03s falls in the 0.05 bucket and all higher buckets.
	reg.ObserveRequestDuration(0.03)
	// 1.5s falls only in the 2.5, 5, 10 buckets.
	reg.ObserveRequestDuration(1.5)

	body := output(reg)

	if !strings.Contains(body, "# TYPE portwing_http_request_duration_seconds histogram") {
		t.Errorf("missing histogram TYPE line; output:\n%s", body)
	}
	if !strings.Contains(body, "# HELP portwing_http_request_duration_seconds") {
		t.Errorf("missing histogram HELP line; output:\n%s", body)
	}

	// 0.001 bucket: only values <= 0.001 — none of our three samples qualify.
	if !strings.Contains(body, `portwing_http_request_duration_seconds_bucket{le="0.001"} 0`) {
		t.Errorf("expected le=0.001 bucket 0; output:\n%s", body)
	}
	// 0.005 bucket: 0.002 qualifies (1 observation).
	if !strings.Contains(body, `portwing_http_request_duration_seconds_bucket{le="0.005"} 1`) {
		t.Errorf("expected le=0.005 bucket 1; output:\n%s", body)
	}
	// 0.05 bucket: 0.002 and 0.03 qualify (2 observations).
	if !strings.Contains(body, `portwing_http_request_duration_seconds_bucket{le="0.05"} 2`) {
		t.Errorf("expected le=0.05 bucket 2; output:\n%s", body)
	}
	// 10 bucket: all three qualify.
	if !strings.Contains(body, `portwing_http_request_duration_seconds_bucket{le="10"} 3`) {
		t.Errorf("expected le=10 bucket 3; output:\n%s", body)
	}
	// +Inf must equal total count (3).
	if !strings.Contains(body, `portwing_http_request_duration_seconds_bucket{le="+Inf"} 3`) {
		t.Errorf("expected +Inf bucket 3; output:\n%s", body)
	}
	// _count must equal 3.
	if !strings.Contains(body, "portwing_http_request_duration_seconds_count 3") {
		t.Errorf("expected _count 3; output:\n%s", body)
	}
	// _sum must be 0.002 + 0.03 + 1.5 = 1.532.
	if !strings.Contains(body, "portwing_http_request_duration_seconds_sum 1.532") {
		t.Errorf("expected _sum 1.532; output:\n%s", body)
	}
}

// TestRegistryHistogramCumulativeMonotonic verifies that each bucket count is
// >= the count of every preceding bucket (i.e. cumulative / monotone).
func TestRegistryHistogramCumulativeMonotonic(t *testing.T) {
	t.Parallel()

	reg := metrics.NewRegistry()
	for _, d := range []float64{0.001, 0.004, 0.009, 0.02, 0.04, 0.09, 0.2, 0.4, 0.9, 2, 4, 9, 15} {
		reg.ObserveRequestDuration(d)
	}

	var b strings.Builder
	reg.WritePrometheus(&b, noEscape)
	body := b.String()

	// Extract bucket lines in order and assert monotonicity.
	lines := strings.Split(body, "\n")
	prev := -1
	for _, line := range lines {
		if !strings.HasPrefix(line, "portwing_http_request_duration_seconds_bucket{") {
			continue
		}
		// Last token is the count.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var count int
		if _, err := fmt.Sscanf(fields[len(fields)-1], "%d", &count); err != nil {
			continue
		}
		if le := fields[0]; strings.Contains(le, "+Inf") {
			// +Inf is checked separately — skip monotonicity here.
			continue
		}
		if prev >= 0 && count < prev {
			t.Errorf("bucket count went backwards: %d < %d in line %q", count, prev, line)
		}
		prev = count
	}
}

// TestRegistryAuthFailureEmptyReasonNormalized verifies that an empty reason
// is normalised to "unknown".
func TestRegistryAuthFailureEmptyReasonNormalized(t *testing.T) {
	t.Parallel()

	reg := metrics.NewRegistry()
	reg.IncAuthFailure("")

	body := output(reg)
	if !strings.Contains(body, `portwing_auth_failures_total{reason="unknown"} 1`) {
		t.Errorf("empty reason should normalise to unknown; output:\n%s", body)
	}
}

// TestRegistryLabelEscaping verifies that label values with special characters
// are escaped in the output.
func TestRegistryLabelEscaping(t *testing.T) {
	t.Parallel()

	reg := metrics.NewRegistry()
	// reason containing a backslash and a double-quote.
	weirdReason := `bad\sig"nal`
	reg.IncAuthFailure(weirdReason)

	body := output(reg)

	// Raw unescaped value must not appear.
	if strings.Contains(body, weirdReason) {
		t.Errorf("unescaped label value found in output; output:\n%s", body)
	}
	// Escaped form must appear.
	escaped := `bad\\sig\"nal`
	if !strings.Contains(body, escaped) {
		t.Errorf("escaped label value %q not found; output:\n%s", escaped, body)
	}
}

// TestRegistryDeterministicOutput verifies that WritePrometheus produces the
// same output on repeated calls (label keys are sorted).
func TestRegistryDeterministicOutput(t *testing.T) {
	t.Parallel()

	reg := metrics.NewRegistry()
	for _, m := range []string{"DELETE", "GET", "POST", "PUT"} {
		reg.IncRequest(m, 200)
	}
	for _, r := range []string{"replay", "bad_token", "unknown-key"} {
		reg.IncAuthFailure(r)
	}

	a := output(reg)
	b := output(reg)
	if a != b {
		t.Errorf("WritePrometheus is non-deterministic:\nfirst:\n%s\nsecond:\n%s", a, b)
	}
}

func TestRegistryOperationalMetrics(t *testing.T) {
	t.Parallel()

	reg := metrics.NewRegistry()
	if reg.ControllerConnected() {
		t.Fatal("new registry should report a disconnected controller")
	}
	reg.SetEdgeMode(true)
	reg.SetControllerConnected(true)
	if !reg.ControllerConnected() {
		t.Fatal("controller connected state was not retained")
	}
	reg.IncReconnect()
	reg.IncReconnect()
	reg.IncBackpressure()
	reg.SetAuditState(3, 8, true)
	reg.IncAuditExport(true)
	reg.IncAuditExport(false)

	body := output(reg)
	for _, want := range []string{
		"portwing_edge_controller_connected 1",
		"portwing_edge_reconnects_total 2",
		"portwing_edge_backpressure_events_total 1",
		"portwing_audit_buffer_records 3",
		"portwing_audit_buffer_capacity 8",
		"portwing_audit_sink_enabled 1",
		`portwing_audit_exports_total{outcome="success"} 1`,
		`portwing_audit_exports_total{outcome="error"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q; output:\n%s", want, body)
		}
	}
}

// TestRegistryConcurrency exercises all mutation methods concurrently and then
// asserts exact totals. Run with -race to catch data races.
func TestRegistryConcurrency(t *testing.T) {
	t.Parallel()

	const goroutines = 50
	const iters = 100

	reg := metrics.NewRegistry()

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			for range iters {
				reg.IncRequest("GET", 200)
				reg.ObserveRequestDuration(0.01)
				reg.IncAuthFailure("bad_token")
				reg.IncInFlight()
				reg.DecInFlight()
				reg.IncRateLimited()
			}
			_ = id
		}(i)
	}
	wg.Wait()

	body := output(reg)
	total := goroutines * iters

	// http_requests_total{GET,200} must equal total.
	want := fmt.Sprintf(`portwing_http_requests_total{method="GET",code="200"} %d`, total)
	if !strings.Contains(body, want) {
		t.Errorf("expected %q; output:\n%s", want, body)
	}

	// _count must equal total.
	wantCount := fmt.Sprintf("portwing_http_request_duration_seconds_count %d", total)
	if !strings.Contains(body, wantCount) {
		t.Errorf("expected %q; output:\n%s", wantCount, body)
	}

	// auth_failures_total{bad_token} must equal total.
	wantAuth := fmt.Sprintf(`portwing_auth_failures_total{reason="bad_token"} %d`, total)
	if !strings.Contains(body, wantAuth) {
		t.Errorf("expected %q; output:\n%s", wantAuth, body)
	}

	// rate_limited_total must equal total.
	wantRL := fmt.Sprintf("portwing_rate_limited_total %d", total)
	if !strings.Contains(body, wantRL) {
		t.Errorf("expected %q; output:\n%s", wantRL, body)
	}

	// in-flight must be back to 0 after all goroutines balanced Inc/Dec.
	if !strings.Contains(body, "portwing_http_requests_in_flight 0") {
		t.Errorf("expected in_flight 0 after balanced inc/dec; output:\n%s", body)
	}
}

// TestRegistryBoundsRequestMethodLabels hammers IncRequest with the methods an
// unauthenticated caller can put on the request line and asserts the method
// label set stays bounded. net/http hands any RFC 9110 token to the handler
// verbatim, so labelling by the raw method would grow the counter map — and
// the sort every later scrape pays for — once per distinct method sent.
func TestRegistryBoundsRequestMethodLabels(t *testing.T) {
	t.Parallel()

	standard := []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE"}

	reg := metrics.NewRegistry()
	for _, method := range standard {
		reg.IncRequest(method, 200)
	}
	const hammered = 5000
	for i := range hammered {
		reg.IncRequest("ZORKMID"+strconv.Itoa(i), 200)
	}
	// Case variants and near-misses are distinct methods to net/http, not
	// spellings of the standard ones, so they collapse rather than merging
	// into GET's series.
	other := []string{"get", "Get", "GETX", "", "PROPFIND", "!#$%&'*+-.^_`|~9Az"}
	for _, method := range other {
		reg.IncRequest(method, 200)
	}

	body := output(reg)
	seen := make(map[string]struct{})
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(line, `portwing_http_requests_total{method="`)
		if !ok {
			continue
		}
		method, _, ok := strings.Cut(rest, `"`)
		if !ok {
			t.Fatalf("malformed requests_total line %q; output:\n%s", line, body)
		}
		seen[method] = struct{}{}
	}

	want := append(append([]string{}, standard...), "OTHER")
	if len(seen) != len(want) {
		t.Errorf("method label set has %d values %v, want %d", len(seen), seen, len(want))
	}
	for _, method := range want {
		if _, ok := seen[method]; !ok {
			t.Errorf("missing method label %q; output:\n%s", method, body)
		}
	}

	// Every unrecognised method must land on the one shared series.
	wantOther := fmt.Sprintf(`portwing_http_requests_total{method="OTHER",code="200"} %d`, hammered+len(other))
	if !strings.Contains(body, wantOther) {
		t.Errorf("expected %q; output:\n%s", wantOther, body)
	}
	if !strings.Contains(body, `portwing_http_requests_total{method="GET",code="200"} 1`) {
		t.Errorf("GET must keep its own series; output:\n%s", body)
	}
}
