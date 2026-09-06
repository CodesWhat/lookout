export type Milestone = {
  version: string;
  title: string;
  emoji: string;
  status: "released" | "next" | "planned";
  items: string[];
};

// Milestones in chronological order (oldest first).
// The roadmap component reverses this for display — newest/planned at the top.
// HEAD is automatically the last entry with status "released".
export const roadmap: Milestone[] = [
  {
    version: "v0.1.0",
    title: "Foundation",
    emoji: "🛡️",
    status: "released",
    items: [
      "Standard mode: HTTP/SSE inbound server, Docker Engine API proxy",
      "Token authentication — plaintext, file-based, and Argon2id hash-at-rest",
      "Docker Compose stack lifecycle (up / down / pull / ps)",
      "Exec tunnel: interactive container sessions streamed over HTTP",
      "Structured JSON audit log with in-memory ring buffer and pull endpoint",
      "Health endpoint (/_portwing/health) for Docker HEALTHCHECK directives",
      "Multi-arch image: linux/amd64 + linux/arm64, read_only + cap_drop:ALL runtime defaults",
    ],
  },
  {
    version: "v0.3.0",
    title: "Edge Mode",
    emoji: "🌐",
    status: "released",
    items: [
      "Edge mode: outbound WebSocket tunnel to Drydock — no inbound port required",
      "Ed25519 per-request signatures with nonce LRU replay protection",
      "Ordered exec I/O — input that arrives before the exec is up is buffered and replayed",
      "Outbound backpressure: bounded write queue, per-frame deadline, slow-consumer eviction",
      "Reproducible base images: Dockerfile and Dockerfile.release pinned by digest",
      "Three-tier fuzz harness: 60s PR smoke, 5m nightly, 1h monthly deep pass",
      "Weekly soak test: resident-set budget under sustained SSE+proxy+exec load",
    ],
  },
  {
    version: "v0.5.0",
    title: "Security Hardening",
    emoji: "🔒",
    status: "released",
    items: [
      "Pre-auth body limits tightened — oversized requests blocked before auth runs",
      "Registry-auth and proxy query parameters validated before reaching the daemon",
      "Private-key file-permission parity — startup fails on world-readable keys",
      "Consistent outbound TLS posture across all HTTP client paths",
      "Goroutine-lifecycle cleanup on shutdown — no leaks under SIGTERM",
      "Benchmark tracking: hot-path ns/op + allocs/op kept as a 90-day artifact",
      "Cosign-signed images, SBOMs, and build provenance on every release",
    ],
  },
  {
    version: "v0.6.0",
    title: "Non-Root & Cross-Repo Parity",
    emoji: "🔐",
    status: "released",
    items: [
      "Agent info surfaced in the Drydock UI — memoryGb, logLevel, and pollInterval reported on every dd:ack",
      "Edge-mode container deletion — dd:container_delete_request/response over the WebSocket tunnel",
      "Container environment variables in RuntimeDetails, parsed from Docker's Config.Env",
      "COMPATIBILITY.md — canonical cross-repo version matrix for Portwing × Drydock × sockguard",
      "BREAKING: container image now runs as non-root (UID 65532) by default — root is an explicit opt-in",
      "CI egress lockdown — harden-runner egress-policy: block across all jobs in every workflow",
    ],
  },
  {
    version: "v0.7.0",
    title: "Fail-Closed Security",
    emoji: "🛡️",
    status: "released",
    items: [
      "Standard mode fails closed without credentials — unauthenticated development requires explicit loopback and remote opt-ins",
      "Ed25519 HTTP signature v2 binds the escaped path and exact raw query; legacy signatures are query-free only",
      "Cold Argon2id work is capped agent-wide and per IP; raw-token verifiers retain fixed-size digests",
      "Credential files require regular-file descriptors and safe Unix permissions, checked after open",
      "Enrollment bodies are capped and abuse-limited; Compose uploads use root-confined, symlink-resistant writes",
      "Edge request correlation, bounded log-follow options, and terminal hello-rejection classification",
      "Generated CSP and browser security headers cover every exported website and docs route",
    ],
  },
  {
    version: "v0.8.0",
    title: "Toward v1.0",
    emoji: "🧭",
    status: "released",
    items: [
      "Edge mode graduated to production support — enabled by default in Drydock 1.6 and gated by a real cross-repo multi-agent load/soak",
      "Published stability policy — semver guarantees for the HTTP API, env-var surface, MCP tools, and DrydockCompat wire contract",
      "Package-manager distribution — a Homebrew tap and apt/rpm packages via the existing GoReleaser pipeline",
      "Continuous edge live-tail under the dd: namespace, with cancellation, stdout/stderr framing, and bounded backpressure",
      "Mode-aware liveness/readiness, edge/backpressure metrics, stable cursor-based NDJSON audit export, and runnable observability examples",
    ],
  },
  {
    version: "v0.9.0",
    title: "Controller-Owned Updates",
    emoji: "🔄",
    status: "released",
    items: [
      "Drydock watcher/update execution runs controller-side through Portwing's authenticated Docker proxy in Standard and Edge modes",
      "Watcher descriptor declares transport=docker-api, execution=controller, and events=portwing; no remote trigger is advertised",
      "Edge sends component ownership before raw inventory; full feature compatibility requires Drydock v1.6.0-rc.11+ while DrydockCompat remains 1.4.0",
      "Edge mode shares the cursor-based NDJSON audit exporter on its explicitly private, unauthenticated operations listener",
      "Exec and Docker proxy failures preserve bounded daemon/sockguard error details through the controller path",
      "Tagged Portwing × Drydock × Sockguard acceptance matrix passed in standard and edge modes — completed 2026-08-29",
      "Controller-backed enrollment, overlapping-key rotation, revocation, and clock-skew recovery exercised against a real Drydock controller — completed 2026-08-29",
    ],
  },
  {
    version: "v1.0",
    title: "Stable Release",
    emoji: "🚀",
    status: "planned",
    items: [
      "Stable HTTP API and environment-variable surface under semantic-versioning guarantees",
      "Stable agent ↔ controller wire protocol — no breaking changes without a major bump",
      "Threat model review; the HIGH+ CVE gate is already enforced on every release image, and v1.0 keeps it",
      "Final re-verify of the published competitive review against primary sources at the v1.0 cut",
    ],
  },
];
