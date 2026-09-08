package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codeswhat/portwing/internal/metrics"
	"github.com/codeswhat/portwing/internal/protocol"
)

// handleMetrics emits host and per-container metrics in Prometheus text
// exposition format (version 0.0.4). It is registered at both
// GET /_portwing/metrics and GET /metrics.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder

	fmt.Fprintf(&b, "# HELP portwing_build_info Portwing agent build metadata.\n")
	fmt.Fprintf(&b, "# TYPE portwing_build_info gauge\n")
	fmt.Fprintf(&b, "portwing_build_info{version=\"%s\"} 1\n", metrics.EscapeLabelValue(protocol.AgentVersion))
	fmt.Fprintf(&b, "# HELP portwing_uptime_seconds Seconds since the agent started.\n")
	fmt.Fprintf(&b, "# TYPE portwing_uptime_seconds gauge\n")
	fmt.Fprintf(&b, "portwing_uptime_seconds %g\n", time.Since(s.startTime).Seconds())

	metrics.WriteHostPrometheus(&b, s.collector)
	metrics.WriteContainerPrometheus(r.Context(), &b, s.containerMetrics(), metrics.EscapeLabelValue)

	if s.metrics != nil {
		if s.auditor != nil {
			s.setAuditMetrics(s.auditor.Stats())
		}
		s.metrics.WritePrometheus(&b, metrics.EscapeLabelValue)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, b.String())
}

// containerMetrics returns the process-wide container collector, building it
// on first use so a Server assembled directly in a test shares one too. Every
// scrape must go through the same collector: that is what keeps concurrent
// scrapes down to a single Docker stats pool.
func (s *Server) containerMetrics() *metrics.ContainerCollector {
	s.containerMetricsOnce.Do(func() {
		// A nil client has to be dropped here, while it is still a typed
		// pointer: wrapped in the collector's interface field it would read as
		// non-nil, and the collection now runs on a goroutine of its own where
		// the resulting nil dereference would take the agent down instead of
		// failing the one scrape.
		if s.dockerClient == nil {
			return
		}
		s.containerCollector = metrics.NewContainerCollector(s.dockerClient)
	})
	return s.containerCollector
}
