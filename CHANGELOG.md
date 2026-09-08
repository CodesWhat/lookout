# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [v0.9.17] - 2026-09-08

### Fixed

- Health and enrollment early responses bound unread request-body cleanup,
  preventing connections from waiting indefinitely after the handler returns.
- The audit buffer bounds and copies request-derived display fields, keeping
  long paths and shared request-line allocations from exhausting retained memory.
- Docker stats and named image-push endpoints use streaming transport in both
  modes. Explicit `stats?stream=false` requests retain the normal request timeout.
- Interrupted Standard-mode Docker downloads abort the downstream response
  instead of presenting a truncated archive as a completed transfer.
- Unary Edge responses reject Docker body-read failures and record an error
  audit outcome in both base64 and legacy response modes.
- Compose applies environment updates when unchanged Compose files are omitted
  from the request.
- Canceled Compose requests stop waiting for a stack lock and cannot write stack
  files after their canceled wait eventually acquires the lock.
- Paused and restarting containers retain their specific status even when
  Docker also marks them running, and status transitions emit inventory updates.
- Edge reconnects clear the previous controller's polling override before
  applying the new welcome or falling back to configured defaults.
- Slow inventory refreshes no longer delay periodic Edge metrics and pings.
- A full Drydock handler pool rejects additional work promptly, preserving
  control-message handling and balanced log-admission cleanup.
- OpenRC services export all configured settings, including private-key and
  file-authentication paths, instead of only the original five variables.
- Edge Docker response streams write their audit outcome when the body ends;
  truncated streams record `error` instead of an early `allowed` result.
- A full nonce cache reports `nonce-capacity` in auth response headers and
  metrics, distinguishing capacity pressure from replays. Expiry uses insertion
  order, avoiding a full-cache scan for each rejected request.
- Edge readiness checks share the bounded, cached Docker probe used by Standard
  mode, preventing concurrent operations requests from opening one ping each.
- Duration environment variables reject malformed, negative, and overflowing
  values at startup. Request timeouts and reconnect delays still accept zero;
  heartbeat, polling, welcome, and clock-skew settings require positive seconds.
- Container renames now emit an update immediately, and reordering Docker's
  listed names no longer invalidates the inspect cache. The OpenAPI container
  error schema no longer declares an unused timestamp field.
- **A truncated Docker response stream no longer reports itself as complete.**
  The edge agent's response relay ended every stream with `reason: "complete"`,
  including the ones that ended on `io.ErrUnexpectedEOF`, a dockerd that died
  mid-pull, or a declared `Content-Length` the body never reached. A controller
  had no way to tell a finished image pull, build, `logs?follow`, export or
  event stream from a truncated one, so a half-written layer or tar looked like
  the whole payload. `io.EOF` is now the only end that sends `complete`;
  anything else sends `reason: "error"` and logs the underlying read error. The
  two-value vocabulary is documented in the Drydock integration reference.
- **Non-TTY exec output is demultiplexed instead of shipped with its frame
  headers.** An `exec_start` with `tty: false` gets a stream-multiplexed
  attach from Docker, and the edge agent forwarded it byte for byte, so every
  chunk of `exec_output` began with an 8-byte `[stream type, 0, 0, 0,
  big-endian length]` header that the controller rendered as control bytes in
  the middle of the command's own output. The read loop now strips the frames
  for non-TTY sessions and merges stdout and stderr in arrival order, since
  `exec_output` carries no stream identifier; a stream that loses frame sync
  ends the session with an `exec_end` naming the desynchronization rather than
  forwarding unframeable bytes. TTY sessions are raw and are unchanged.
- **Unauthenticated callers can no longer grow the metrics registry.** The
  request counter labelled by raw HTTP method, and the 401 path counted before
  any credential check, so arbitrary method strings added label values (and
  scrape sort cost) without bound. The nine standard methods keep their label;
  anything else is counted as `method="OTHER"`.
- **Overlapping metrics scrapes share one Docker stats collection.** Each
  scrape used to start its own eight-worker stats pool, so concurrent scrapers
  multiplied Docker requests. Scrapes that arrive while a collection is in
  flight now wait for it and format the same result; the collection is
  cancelled only when the last waiter leaves.
- **A transient inspect failure no longer drops a container from the
  inventory.** A listed container whose inspect errored was removed from the
  snapshot and reported as `removed` until a later poll succeeded. It now keeps
  its last known entry and is re-inspected on the next poll. A `docker rename`
  is also picked up on the next poll instead of being masked by the inspect
  cache.
- **The Drydock log forwarder keeps partial final frames.** It used its own
  frame reader and discarded the payload of a frame cut short by the daemon;
  it now uses the shared decoder, and both adapters serve container logs
  through one code path.
- **Compose operations on the same directory serialise.** The stack lock was
  keyed by `StackName` while every file and project operation used
  `StackDir`, so two requests with different names for one directory could
  interleave. The lock now keys on the canonical stack directory.
- **Unbracketed IPv6 bind addresses work.** `BIND_ADDRESS=::1` produced
  `too many colons in address` on the API server, the edge operations
  listener and the CLI; addresses are now joined with proper bracketing, and
  the already-bracketed form still works.
- **Ed25519 admission is enforced before the body is read.** Signed requests
  buffered up to 1 MiB before taking the per-IP verification slot, so slow
  bodies bypassed the intended concurrency bound. The slot is taken first, and
  a rejected request closes the connection instead of draining the body.
- **Readiness pings are bounded.** `/_portwing/ready` ran an unbounded
  synchronous Docker ping per request; it now has a 2s timeout, one ping in
  flight shared by concurrent callers, and a 1s result cache.
- **Nonces are never accepted without being recorded.** At cache capacity a
  fresh nonce was accepted but not stored, so a captured signed request stayed
  replayable for the rest of its timestamp window. The cache now evicts
  expired entries and records, or rejects when nothing has expired.
- **Bad interval settings fail at startup instead of panicking.**
  `HEARTBEAT_INTERVAL` and `DD_POLL_INTERVAL` must be positive and within
  range, and a controller welcome carrying an out-of-range `pollInterval`
  logs a warning and keeps the configured interval rather than crashing the
  agent.

### Changed

- **`api/openapi.yaml` now matches what `/api/containers` serves.** The
  schema declared `image.tag` and `updateKind` as strings, but the wire has
  always carried the `{value, semver}` and `{kind}` objects Drydock stores
  verbatim under `DrydockCompat` 1.4.0. The schema moved to the objects and
  gained the `health`, `architecture`, `os` and `created` fields; a contract
  test now pins the served types to the file.
- **The MCP docs describe the credential honestly.** The MCP tools are
  read-only, but the credential that authenticates `/_portwing/mcp` is the
  same one that authenticates the Docker proxy, and there is no scope tier.
  The page now says so and points at per-client Ed25519 keys as the isolation
  the code provides.

## [v0.9.16] - 2026-09-07

### Changed

- **Web dependency refresh.** The website, docs and analytics packages move to
  Next.js 16.3.4, posthog-js 1.427.0, Biome 2.5.12, PostCSS 8.5.28 and the
  current `@types/node` and `@types/react-dom` pins, with `package-lock.json`
  and the analytics source test (posthog-js version and extension bundle size)
  synced to match. No Go, protocol or workflow changes.

## [v0.9.15] - 2026-09-07

### Changed

- **The competitive landscape lives only on the docs site now.** The root
  `COMPETITIVE-LANDSCAPE.md` working file is no longer in the repository;
  the published comparison at
  [portwing.codeswhat.com/docs/competitive-landscape](https://portwing.codeswhat.com/docs/competitive-landscape)
  is the one source, and README, ROADMAP and the design docs link there.
  Docs-only release, no binary or protocol change since v0.9.14.

## [v0.9.14] - 2026-09-06

### Fixed

- **A failed host-metrics collection now goes on the wire instead of being
  swallowed.** The edge agent logged the error at debug level and sent
  nothing, so on a host with no procfs the tunnel emitted zero metrics frames
  and nothing on the wire separated an unsupported host from a supported one
  that had stopped reporting. Liveness was never the gap: the agent pings on
  the same tick. The metrics tick now emits an `error` frame carrying
  `host-metrics-unavailable` and the collection error, on the same cadence as
  the `metrics` frame it stands in for. Whether that surfaces anywhere is the
  controller's call, and no controller acts on it yet: Drydock drops any
  `error` frame that carries no `requestId`, with no log and no state change,
  which is also what makes the frame safe to send unnegotiated. What changed
  is that the failing tick now carries a signal a controller can read instead
  of nothing at all. It deliberately does not send the partially populated
  snapshot collection returns alongside the error, because a zero-filled
  metrics frame reads as a real measurement of an idle host — the same reason
  the Prometheus endpoint omits the host series behind
  `portwing_host_metrics_supported 0`. The log follows the transitions
  rather than the ticks — the first failure warns, repeats while the host
  stays broken drop to debug, and a recovery says so once at info — because
  a permanent failure warning every 30 seconds buries the tick that changed.
  Documented in SPEC section 8.
- **A dropped tunnel no longer wedges a streamed request's `requestId` for 30
  seconds.** Connection teardown closed the exec sessions but left every
  `bodyStream: true` reassembly registered with its idle timer running, so a
  controller that reconnected and retried the same `requestId` was answered
  with a duplicate-`requestId` rejection until the 30-second idle timeout
  expired. Teardown now drains those pending bodies and stops their timers
  alongside the exec sessions, so the retry is accepted immediately.
- **SIGTERM now stops the edge agent promptly against a quiet controller.**
  The read pump only looked for shutdown between messages, so once it was
  parked inside a socket read a cancelled context was invisible to it until
  the controller said something or the read deadline expired — and that
  deadline floors at 60 seconds. A healthy controller with nothing to send is
  exactly that case, so a container told to stop sat there well past the 10
  seconds `docker stop` waits and the 30 Kubernetes waits before following up
  with SIGKILL. Ordinary restarts and rolling deploys ended in a killed
  process instead of a clean exit, with in-flight work dropped rather than
  finished. Shutdown now closes the controller connection as soon as the
  context is cancelled, which fails the blocked read at once and unwinds the
  agent in milliseconds. The close is not mistaken for a dropped tunnel: the
  agent still exits with the shutdown reason and schedules no reconnect on the
  way out, and a real drop still reconnects as before.

## [v0.9.13] - 2026-09-03

### Fixed

- **Docs now match the logs routes' real status-code and stream-cap
  behavior.** `security-model.mdx`'s concurrent-stream-cap row wrongly said
  the cap excluded the adapter's SSE and follow-mode logs; the transparent
  proxy gates every `/logs` request regardless of `follow`, and only the
  adapter's own non-follow reads skip the slot. `api/openapi.yaml` now
  documents the 404 and 409 Docker status mappings and the 503 the
  follow-mode stream gate returns on both logs paths.
- **GOVERNANCE.md now describes the branch model correctly.** Changes branch
  from and merge into the active dev branch; `main` only moves by promotion.
- **The built-in image's `HEALTHCHECK` path is corrected in both the standard
  compose example and getting-started.** It probes `/health`, not
  `/_portwing/health`.
- **Website copy dropped a route and fields that don't exist, and its
  hardened compose snippet is back in sync with getting-started.**
  `llms.txt` and the cli-demo no longer describe a `POST /exec/{container}`
  route — exec goes through the Engine API (create, then start with
  `Upgrade: tcp`) — or an `exec_start` audit record carrying method/path
  fields it never had. get-started's "Secure" tab snippet now matches the
  doc: loopback bind, explicit UID 65532, `DOCKER_SOCK_GID` group_add.
- **CHANGELOG's v0.9.12 entry now records the edge-example bind-guard fix.**
  `examples/kubernetes/edge.yaml` and the observability compose `edge`
  profile needing `ALLOW_UNAUTHENTICATED_REMOTE` alongside a non-loopback
  bind landed in #272 and was the only user-visible v0.9.12 fix without a
  bullet.

### Security

- **`.gitleaksignore` no longer pins a fingerprint to a working-tree line
  number.** The cli-demo private-key finding was pinned to a line that moved
  every time the file changed, so the PR Secrets check (which restores the
  ignore policy from the base ref before scanning) failed again on the next
  unrelated edit. The demo key's body is truncated with an inline
  `gitleaks:allow` marker instead, which travels with the line's content
  rather than its position; the release contract now rejects any
  `.gitleaksignore` line that isn't a commit-pinned history fingerprint.

## [v0.9.12] - 2026-09-03

### Added

- **ClusterFuzzLite now fuzzes the same ten targets against libFuzzer and
  AddressSanitizer.** The four existing tiers all run Go's native engine, and
  the two scheduled ones keep their corpus in the Actions cache, which evicts an
  entry after seven days without a hit. This one rebuilds the identical
  `func Fuzz*(f *testing.F)` targets through OSS-Fuzz's base-builder-go
  (`.clusterfuzzlite/`), runs a second mutation engine over them, and commits
  what it finds to a branch, so the corpus does not expire. 300 seconds
  across all ten on a pull request that touches Go, an hour every Saturday, and
  a corpus prune on Wednesday, which are separate workflows so a green prune
  cannot close the tracking issue a crashing batch run opened. The corpus lives
  on an orphan `clusterfuzzlite-corpus` branch of this repository, written with
  the workflow's own `GITHUB_TOKEN`, so there is no new secret and no second
  repository to keep. Only the two scheduled jobs can write; the pull-request
  lane reads the corpus and has `contents: read`, and the trigger is
  `pull_request` rather than `pull_request_target`, so a fork's token stays
  read-only. No fuzz body changed; two `internal/docker` test helpers moved into
  the fuzz files that use them, because this tier compiles a target's `_test.go`
  file on its own.
- **Edge mode agents can now accept a streamed request body via the new
  `edge-request-body-stream` capability (issue #205).** `request.body` is a
  JSON `RawMessage`, so it could never carry a binary or otherwise non-JSON
  body (a tar build context, or any payload too large for a single 16 MB
  WebSocket frame). A controller that has seen this token in the agent's
  `hello.capabilities` can now send `request.bodyStream: true` with the body
  omitted, followed by one or more `stream` chunks and a terminal
  `stream_end`, all keyed by the request's `requestId`; the agent reassembles
  them before dispatching the request to Docker, bounded three ways: 512 MB
  per request, 1 GB summed across every streamed body the agent is holding in
  memory, and 100 concurrent reassemblies. The aggregate byte budget is the
  one that bounds agent memory, since a per-request cap multiplies by however
  many requests are open, and it spans both stages: the buffers still
  reassembling and the reassembled bodies already dispatched, which keep
  their bytes until the Docker round trip ends. Charging only the reassembly
  stage would let a controller send `stream_end` on everything pending to
  drop the total to zero and immediately refill it. A reassembly doesn't
  claim a stream session slot until its `stream_end` lands. Over either
  limit, or past the 30s idle timeout between chunks, the agent answers with
  an `error` frame naming the `requestId` rather than dropping the request
  silently. Purely additive: a controller
  that never sends `bodyStream: true` is unaffected, and this does not by
  itself fix any controller that doesn't yet send it. Note: the matching
  controller-side chunker this depends on to actually close out #205 lives
  in a different repo and is not part of this change.
- **Standard mode now enforces the concurrent exec and stream session limits
  that SPEC 7.3 has always documented.** `MAX_STREAM_SESSIONS` and
  `MAX_EXEC_SESSIONS` (both default `100`, matching edge mode's fixed limits)
  bound concurrent streaming Docker proxy responses and hijacked exec/attach
  sessions respectively; a saturated agent answers with `503` instead of
  accepting an unbounded number of goroutines. Either variable can be set to a
  non-positive value to disable its bound, for a controller that legitimately
  drives more sessions than the default. `/api/events` SSE and follow-mode
  container log streaming share the stream bound too: both adapters check for a
  free slot before the stream starts, so a rejected SSE client is never
  registered with the broadcaster and a rejected log follow never reaches the
  daemon. Non-follow log reads are a single bounded response and stay ungated.
- **`portwing --help` and `portwing --version` now work at the top level.**
  Previously only the `hash-token`, `keygen`, and `version` subcommands were
  recognized; a bare `--help` or an unrecognized flag started the full agent and
  tried to connect to Docker instead of printing usage.
- **Web analytics captures UTM campaign labels and the referring domain.** The
  five `utm_*` parameters and `$referring_domain` now pass through the
  cookieless PostHog pipeline on an explicit allowlist; unrelated click-id
  parameters (`gclid`, `fbclid`, `msclkid`, ...) remain unreachable rather than
  filtered, and a `$referring_domain` that isn't a bare hostname or `$direct` is
  dropped.
- **The monthly benchmark lane now fails on a regression instead of only
  recording one.** `quality-bench-monthly.yml` compares each run against the
  `benchmark-results.txt` artifact of an earlier successful run using benchstat,
  and fails the job when `sec/op` or `B/op` regresses by more than 10% and
  benchstat calls the difference statistically significant, or when a metric
  that used to be zero starts allocating (benchstat reports that as `?` rather
  than a percentage, so a percentage-only check would miss the zero-allocation
  path that is most worth catching). The comparison table lands in the run
  summary with the regressing rows called out. The baseline is a prior artifact
  rather than a file the lane commits, so nothing writes to the repository on a
  cron; artifacts last 90 days against a monthly cadence, so a skipped month
  still leaves one in reach. GitHub-hosted runners rotate CPU models and
  benchstat will not compare across hardware, so the lane walks back through
  recent successful runs for one that matches and reports without gating when
  none does. Dispatching with `accept_new_baseline` records an intended slowdown
  as the new baseline. `allocs/op` is measured and reported but not gated: it is
  a small integer, so the smallest possible change is often more than 10% and a
  percentage threshold says nothing useful about it.
- **A local `CODE_OF_CONDUCT.md` is back at the repository root.** v0.9.7
  removed it in favour of the organisation-wide document the `.github`
  repository serves to any repo without its own. It returns as the full
  Contributor Covenant 2.1 verbatim rather than the fifteen-line summary that
  was there before, so the text a contributor reads lives in the repository
  they are contributing to. `README.md` and `CONTRIBUTING.md` link to it by
  relative path and the reporting address is unchanged at
  `security@codeswhat.com`.
- **The weekly soak and monthly mutation lanes now keep a queryable history of
  their headline numbers instead of only the most recent run's.** Both lanes
  print to the run log and step summary and nothing else, so a leak that adds
  a few MiB of RSS a week never trips the 64 MiB budget on any single run and
  is invisible in all of them; the same is true of a mutation score drifting
  down inside its floor. Each lane now ends with a separate job that appends
  that run's numbers to `quality-history`, an orphan branch in this repository
  holding one append-only JSONL file per lane: baseline/final/growth RSS with
  the request and error totals for soak, and per-package efficacy, mutator
  coverage, mutant counts and the floor they were measured against for
  mutation. Read a series with `scripts/quality-history.sh <lane> [--last N]`.
  Only `schedule` and `workflow_dispatch` runs record. `contents: write` is
  scoped to that recording job alone, which builds nothing and runs no product
  code, so the credential never sits in the job that spends four hours running
  the agent or the one that executes mutated source. An append that cannot
  reach the remote emits a warning and exits 0 rather than turning a green
  quality lane red, and a record already present is never written twice. The
  branch shares no history with `main` or any `dev/*` branch and is never
  merged, so no tracked file changes on a cron and nothing here reaches a
  released tree.

### Fixed

- **Host disk metrics use the Docker daemon's real data root and report a
  failed reading instead of a fake zero.** `host_metrics` and the Prometheus
  host series previously assumed `/var/lib/docker`; the data root is now
  resolved from the daemon's `/info` on first collection (2s timeout, retried
  at most once per 30s on failure, unaffected by `SKIP_DF_COLLECTION`). A
  `statfs` failure — wrong data root, unreadable filesystem — no longer reports
  0 bytes as though the disk were empty: the snapshot carries
  `diskMetricsAvailable: false` and a `diskError` string, and `/metrics` reports
  `portwing_host_disk_metrics_available 0` with the disk byte series omitted.
  `portwing_host_metrics_supported` continues to track only the `/proc`-backed
  fields (CPU, memory, network, uptime); disk collection runs after that pass
  and is skipped when it fails, so disk is never available while it reports 0.
- **`host_metrics` reports a missing procfs instead of a zero-filled
  snapshot.** A native macOS install (including the Homebrew cask) previously
  got back `isError:false` with `memoryTotal`, `diskTotal`, and `uptime` all
  `0` — indistinguishable from a real reading of an idle host. The MCP tool now
  returns an explicit error, and `/metrics` reports
  `portwing_host_metrics_supported 0` with the host resource series omitted
  rather than a flat zero line.
- **`GET /api/containers/{id}/logs` (both adapters) now maps Docker's 404 and
  409 through instead of collapsing both to 500.** The sibling delete handler
  already mapped these correctly; the log handlers didn't share the fix. The status is
  now carried on a typed Docker API error rather than pattern-matched out of
  the formatted error message, which a container literally named `status 404`
  could previously spoof into the wrong HTTP status.
- **Wrong-method requests to `/_portwing/mcp` and `/api/portwing/enroll` return
  405 instead of a bare Docker-proxy 404.** Both were registered as exact
  method+path patterns, which lets Go's `ServeMux` fall through to the
  Docker-proxy catch-all on any other method instead of reaching the handler's
  own 405 response.
- **The docs and marketing site footer copyright year no longer causes a hydration
  mismatch.** It was computed with `new Date().getFullYear()` at both build
  time and in the browser, which disagree for a visitor whose local clock
  crosses into January before the static export is rebuilt. The year is now
  baked into the build once and shared by server and client output.
- **UTM values can no longer smuggle a path, query string, fragment, or
  percent-escaped separator through the analytics allowlist.** The guard
  missed `?`, `#`, and `\`, and only decoded a value once, so a doubly-encoded
  separator (e.g. `%252Facme`) reached the sanitizer looking harmless while a
  downstream decode would have rebuilt the real path. A bare `%` still passes,
  so values like "50% off" are unaffected.
- **A closed edge exec session leaves the session registry before its `done`
  channel is signalled.** `ExecSession.Close` closed `done`, closed the
  connection, and drained the inbox before unregistering, so a waiter that saw
  `done` closed could still find the session registered and the session held
  one of the `maxExecSessions` slots for the length of the drain. The
  unregister now runs inside the same locked section, ahead of `close(done)`,
  so a waiter that observes `done` closed will not find the session still
  registered. This does not stop all enqueueing: `HandleInput` can no longer
  enqueue once `closed` is set, but a racing `HandleResize` may still land a
  zero-reservation item that `inputWriter` picks over `done`; that item is
  harmless because `doResize` then runs against the already-cancelled session
  context and aborts. The exec ID is released at the same point, which
  slightly widens the window in which a controller reusing that ID could see
  the old read loop's last frames.
- **The shipped edge examples start again under the non-loopback bind guard.**
  `examples/kubernetes/edge.yaml` and the `edge` profile in
  `examples/observability/docker-compose.yml` set `BIND_ADDRESS=0.0.0.0`
  alongside `DRYDOCK_URL` without `ALLOW_UNAUTHENTICATED_REMOTE`, which the
  edge operations listener's non-loopback bind guard rejects: the agent exits
  1 before serving anything. Both now set `ALLOW_UNAUTHENTICATED_REMOTE` to
  `true` next to the bind, with a comment on the trade-off, and the stale
  comments describing the old single-variable opt-in are corrected. A copy
  taken from either example before this release needs the variable added by
  hand; upgrading the agent does not repair it.

### Security

- **Adjudicated GHSA-vp52-pcj8-j9qc (CVE-2026-84304, HIGH) in the published
  container image.** The first live run of the weekly published-image rescan
  failed both gating legs of v0.9.11 on `google.golang.org/grpc` v1.83.0. The
  carrier is not portwing, which has no grpc in its module graph at all: it is
  the Wolfi-packaged `docker-compose` binary the image ships so stacks can be
  deployed. There is nothing to bump to. Compose's newest release, v5.5.0, pins
  grpc v1.83.0, Wolfi's newest package still embeds it, and the advisory's
  vulnerable range is `<= 1.83.0`, so an older Compose is no better. The
  advisory needs an unauthenticated peer fragmenting HTTP/2 DATA frames at a
  gRPC server, and the only gRPC server Compose can run is BuildKit's session
  over the hijacked Docker `/session` connection, never a network listener;
  portwing also spawns Compose as a short-lived subprocess per request, but that
  subprocess shares portwing's memory cgroup rather than one of its own, so a
  cgroup-level OOM kill could still take the whole container down. The argument
  here is unreachability, not isolation. Suppressed in `.grype.yaml` scoped to
  that binary and pinned to v1.83.0, so the ignore won't silently follow
  whatever grpc Wolfi ships next; the weekly rescan just stops matching once it
  drifts, which is why the entry carries a review-by date of 2026-11-15 and a
  contract test that fails if the scope is widened.
  Compose's `main` is already on grpc v1.83.2, so the real fix lands with the
  next Compose release.
- **Dropped the stale `.grype.yaml` OpenSSL suppression entries for
  CVE-2026-54876 and CVE-2026-14456.** Wolfi shipped `libcrypto3`/`libssl3`
  3.6.4-r0 and Alpine shipped 3.5.8-r0, both past the entries' pinned
  3.6.3-r4/3.5.7-r0 versions, and a fresh `grype` scan of the published
  0.9.11 image confirms neither CVE matches at the current versions on any
  scanned platform (amd64, arm64, armv7). The six dead entries (two CVEs
  across the Wolfi pin and, for CVE-2026-14456, the separate armv7 Alpine
  pin) are removed rather than left matching nothing.
- **`golang.org/x/crypto` bumped to v0.56.0**, clearing GO-2026-6354 and
  GO-2026-6355 (CVE-2026-78662, CVE-2026-56855), two `x/crypto/ssh` channel
  deadlock advisories. Same shape as the v0.55.0 move below: portwing imports
  only `x/crypto/argon2` and `govulncheck` reports zero called
  vulnerabilities, so this clears the scanner gate rather than a live
  exposure. The `.grype.yaml` GO-2026-5932 pins moved in lockstep.
- **`golang.org/x/crypto` bumped to v0.55.0**, clearing GO-2026-6303
  (CVE-2026-56854). The advisory is scoped to `x/crypto/ssh`; portwing imports
  only `x/crypto/argon2`, and `govulncheck` reported zero called vulnerabilities
  before and after. The `.grype.yaml` exclusions for portwing's own module and
  binary move to v0.55.0 in lockstep; the entry covering the Wolfi-packaged
  `docker-compose` binary's own embedded `x/crypto` is unaffected.

### Changed

- **Evaluated ML-DSA for the edge hello and the standard-mode request
  signature, and deferred it.** `docs/design/mldsa-edge-auth.md` records the
  decision with measured numbers: ML-DSA-87 costs 4,627 signature bytes and
  2,592 public-key bytes against Ed25519's 64 and 32, which is affordable once
  per connection on the hello and roughly +6,084 bytes on every standard-mode
  request. It sketches hybrid Ed25519 + ML-DSA-87 on the hello once Drydock
  ships a verifier that stores both public keys on one identity record, guards
  the new signature field separately from the existing one, and carries a
  per-identity flag making the post-quantum signature mandatory; that rollout is
  controller-first and is not backward compatible. It rejects a per-request
  post-quantum signature outright, and names a key rotation cadence as the
  mitigation that matches the threat today, with a documented caveat that
  zero-downtime rotation works in standard mode but not in edge mode.
- **Documented four real gaps between the API reference/OpenAPI spec and the
  handlers.** `GET /api/log/entries` and `POST /_portwing/mcp` were live,
  auth-required routes the reference omitted; the OpenAPI spec was missing the
  MCP handler's `202`/`400` responses; and the reference wrongly implied the
  MCP endpoint was available in edge mode, which never registers it.
- **The Prometheus label escaper has one implementation.** `server.go`,
  `edge/client.go`, and their tests each carried a copy of the same
  backslash/quote/newline escape; all now call the single exported
  `metrics.EscapeLabelValue`. No metric name, label, or value changes.
- **Removed dead code found with no production callers:** `docker.DemuxLogStream`
  (superseded by `DecodeContainerLogStream`) and `auth.checkFilePermissions`
  (a byte-for-byte duplicate of `openCredentialFile`'s permission check, which
  callers already use directly), along with the tests that existed solely to
  exercise them.
- **`protocol.MetricsMessage` matches `metrics.HostMetrics` again.**
  `DiskMetricsAvailable` and `DiskError` were added to `HostMetrics` without
  being mirrored on the message type that documents the host-metrics wire
  shape. The wire never regressed: `sendMetrics` and `toolHostMetrics` both
  marshal the collector's `HostMetrics` value directly and nothing in the agent
  ever constructs a `MetricsMessage`, so both fields have been going out over
  the tunnel all along. Nothing type-checked the declaration against the struct
  it documents, which is how it drifted unnoticed. A reflection-based parity
  test now fails on a JSON field or tag option present on one type and not the
  other, in either direction, backed by a wire-level round trip that pins
  `diskMetricsAvailable` as always present and `diskError` as `omitempty`.
- **Competitive claims re-verified against primary sources.** The market audit
  moved from its 2026-07-28 snapshot to 2026-08-29. Komodo Periphery is repinned
  to v2.3.2 and Arcane Agent to v2.9.0; Hawser stays at v0.2.46, still its
  latest release. The Komodo authentication claim is corrected: v2.0.0 replaced
  passkey auth outright rather than supplementing it, and Core and Periphery now
  use automatically generated public/private key pairs exchanged over a
  Noise-protocol handshake. The docs-site table regained the verifiable release
  artifacts and credential rotation rows it had dropped, and the matrix header
  now stamps the current v0.9.11 instead of v0.8.1.
- **Arcane repinned to v2.10.1, and the edge agent auth row is no longer a
  tie.** Arcane v2.10.0 moved session tokens, OIDC verification, passkeys, and
  edge mTLS to ML-DSA-87, the FIPS 204 post-quantum signature scheme, so all
  three comparison surfaces (the repo matrix, the docs-site table, and the
  `/compare/arcane` page) stamp v2.10.1 instead of v2.9.0, and the
  `/compare/arcane` "Agent authentication" verdict moves from `tie` to
  `competitor`. The claim is
  bounded on both sides. Arcane's edge mTLS is still opt-in, its agent token
  still bootstraps the first enrollment, and an existing ECDSA P-384 CA keeps
  issuing P-384 client certificates, so only a freshly generated CA is
  ML-DSA-87. Portwing's Ed25519 hello and per-request signatures are not weaker
  against any attacker that exists today, but they are classical and Portwing
  has no post-quantum option on any surface, which is the difference the row
  now records.
- **Direction parity is scoped to direction support, not robustness.** Every
  reviewed peer does support both inbound and outbound modes, so that claim
  stands, but two maintainer-confirmed gaps are now recorded alongside it:
  Hawser's edge mode still binds an HTTP listener on port 2376 across all
  interfaces unless `BIND_ADDRESS` is set, and Komodo's outbound leg ignores
  `https_proxy` and can stall reconnection on a hardcoded handshake timeout.
  Portwing's own edge listener defaults to loopback and refuses a non-loopback
  bind unless the operator sets `ALLOW_UNAUTHENTICATED_REMOTE`.
- **Corrected four authentication and TLS claims the docs stated as fact.** The
  authentication page said Drydock's bundled `AgentClient` does not implement
  Ed25519 request signing, leaving standard-mode signing available only to
  custom clients; it does, through `DD_AGENT_{name}_AUTHMODE=ed25519` with
  `SIGNINGKEYID` and `SIGNINGKEY`. Both Drydock integration pages named
  `X-Dd-Agent-Secret` and `X-Portwing-Token` as the only accepted credentials,
  omitting `Authorization: Bearer`, the five `X-Portwing-*` signature headers,
  and the rate-limited `POST /api/portwing/enroll` bootstrap exception that
  authenticates with the one-use enrollment token in its body. The edge
  connection sequence still offered a token SHA-256 hash as the fallback when
  `PRIVATE_KEY_FILE` is unset, which startup rejects outright: edge mode is
  Ed25519-only. And `security-grype.yml` justified having no testssl job with
  "Portwing does not operate a TLS listener of its own", which standard mode's
  `TLS_CERT`/`TLS_KEY` contradict.
- **Soak coverage is credited to the workflow that actually provides it.** The
  README and five docs pages attributed edge mode's multi-agent reconnect,
  exec, backpressure, and continuous-log soak to Portwing; that gate is
  Drydock's cross-repo `quality-portwing-fleet-soak.yml`. Portwing's own
  `quality-soak-weekly.yml` covers the standard/generic HTTP path and SSE
  connect/hold/disconnect churn, and it samples resident set size and nothing
  else — `scripts/soak.sh` fails when growth from the post-warmup baseline
  exceeds 64 MiB and never reads a thread or goroutine count, so both the "zero
  RSS/goroutine growth" wording and the "RSS + thread drift" job name
  overclaimed what the run measures. The job is renamed to "Soak (portwing RSS
  growth)"; the soak itself is unchanged.
- **Moved the Go toolchain to 1.27.0.** `go.mod`'s `toolchain` directive and the
  digest-pinned `golang:*-alpine` builder images in `Dockerfile`,
  `Dockerfile.armv7`, and `Dockerfile.dev` now match, as the release contract
  check requires. The CI lint script also moves to golangci-lint v2.13.2; this
  part isn't a routine version bump, since v2.12.2's vendored staticcheck
  panicked on Go 1.27's stdlib (`buildir: package "poll" ... unexpected expr:
  *ast.KeyValueExpr`) instead of reporting lint errors, and v2.13.2 is the
  first release built against a staticcheck that parses it.
- **Release archives are byte-reproducible.** GoReleaser stamps the binary with
  `mod_timestamp: {{ .CommitTimestamp }}`, and the `LICENSE*`, `README*` and
  `CHANGELOG*` entries are now named explicitly so each one's mtime pins to the
  commit date. They were auto-included with their own filesystem mtimes, which
  defeated the binary's pin and left two builds of the same tag producing
  different bytes. Archive contents are unchanged. `Dependency Review` also
  runs on pushes to the dev branch now, not only on pull requests, so a change
  that reaches the branch outside a PR is still scanned.

## [v0.9.11] - 2026-08-27

### Fixed

- **Published-image security gates remain stable across advisory aliases.**
  The reviewed Grype policy now pairs the Go vulnerability and GitHub advisory
  IDs for two Docker Engine findings embedded as module metadata in the
  third-party Docker Compose client. Every exclusion stays pinned to the exact
  module version and `/usr/bin/docker-compose` path, while source and binary
  guards fail if either affected daemon package is ever linked. Mutation tests
  reject a missing alias or any broadened package, version, type, or path scope.
  This release supersedes v0.9.10, whose published artifacts passed signing,
  provenance, install, and pull verification before its final published-image
  scan caught the identifier drift and failed the release run.

## [v0.9.10] - 2026-08-27

### Added

- **Privacy-first web analytics now records page exits and canonical paths.**
  The shared PostHog contract enables `$pageleave` so single-page reading time
  and bounce behavior are measurable, and supplies the `$pathname` field used
  by page, entry-page, and exit-page reports. Both navigation events keep the
  existing finite route allowlist and collapse unknown paths to `/_other`.

### Security

- **Release publishing now verifies provenance before granting write access.**
  The tag workflow first proves that the tagged commit is on `main` and has a
  successful `ci-verify.yml` run in a read-only job. The privileged publishing
  job depends on that result and uses the protected `Production` environment.
  The release-cut workflow also refuses to create a tag for a commit outside
  `main`.
- **Fresh installer configs are private from creation.** The installer creates
  `/etc/portwing` as root-owned mode `0700` and its generated config as
  root-owned mode `0600`, including when the directory already exists. It also
  normalizes existing config ownership and modes without replacing operator
  content.
- **Plaintext standard-mode examples bind to loopback.** Compose and `docker
  run` examples and newly generated service configs now use `127.0.0.1` and
  explain that remote access needs Portwing TLS or a private listener behind a
  TLS-terminating reverse proxy.
- **Enrollment bodies are time- and concurrency-bounded.** Unauthenticated
  enrollment JSON must arrive within 10 seconds, with at most two active
  requests per client and 32 across the agent. Enrollment audit actors now use
  the same validated trusted-proxy client resolution as rate limiting.
- **Ed25519 timestamp validation handles the full signed range.** Extreme
  future timestamps can no longer overflow duration negation and bypass the
  configured clock-skew window.
- **Edge exec input is byte-bounded and session IDs are unique.** Decoded input
  frames are capped at 64 KiB, each session retains at most 1 MiB across queued
  and in-flight writes, and empty or duplicate exec IDs are rejected before
  Docker work starts.
- **Edge outbound buffering has a connection-wide byte budget.** Queued and
  in-flight WebSocket envelopes are capped at 128 MiB, reservations follow the
  connection generation through write or discard, and legacy buffered Drydock
  log requests are limited to one at a time.

### Fixed

- **Container archive downloads stream through standard and edge modes.** GET
  archive responses, including versioned paths with query strings, no longer
  take the bounded whole-body proxy path; PUT archive uploads keep their normal
  non-streaming response handling.
- **Load tests fail closed.** The load generator exits unsuccessfully when it
  completes no requests, encounters a transport error, or receives any non-2xx
  response, so soak runs can no longer report success without exercising a
  healthy agent.
- **Compatibility contracts are enforced at their real boundaries.** CI now
  drives interactive edge exec against dockerd and requires exact hijack 502
  responses, generic log query forwarding, and ack-before-snapshot SSE order.
- **Standard-mode exec relays terminate when either side closes.** Client EOF
  half-closes Docker input so remaining output can drain, while Docker EOF or a
  fatal copy error closes both connections and unblocks the peer goroutine.
- **Compose operation locks are reclaimed.** Per-stack serialization now uses
  reference-counted entries that disappear after the final owner or waiter,
  instead of retaining every distinct stack name for the agent's lifetime.
- **Container metric scrapes use a fixed worker pool.** Large container fleets
  now create at most eight stats workers per scrape instead of one goroutine
  per container waiting behind a semaphore.
- **All authentication coverage exercises the production middleware.** The
  unused duplicate raw-credential wrapper is gone, and token, rate-limit,
  metrics, audit, streaming-interface, and benchmark paths use the combined
  credential and Ed25519 middleware.
- **Ed25519 key IDs have one implementation.** Registry loading, enrollment,
  and edge hello identity now share `KeyIDForPublicKey`, backed by a generated
  key registry round-trip regression.
- **Interactive Docker attach works through standard mode.** Exact exec-start
  and container-attach upgrades now share a bounded, credential-stripping Unix
  socket relay that preserves buffered client input and bidirectional traffic.
- **Shutdown retains final audit records.** Active HTTP and hijacked handlers
  drain before the audit sink closes, including successful retries after a
  timed-out shutdown.
- **The first container refresh is published immediately.** Standard-mode SSE
  clients connected during startup now receive the initial inventory correction
  instead of waiting for the first polling interval.
- **Container logs preserve both Docker stream formats.** Drydock REST, the
  generic API, and MCP now share one decoder for raw TTY output and multiplexed
  stdout/stderr frames, including short live output and fragmented lines.
- **Compose rejects unsupported operations before any side effect.** Invalid
  operations no longer create stack locks, replace stack files, or attempt a
  registry login. Registry authentication now accepts documented bare hosts and
  host-and-port forms while continuing to reject unsafe URI components.
- **Health-only container changes reach polling and event consumers.** Refresh
  diffs compare health details nil-safely, and Docker `health_status: ...`
  actions are forwarded without losing the reported value.

## [v0.9.9] - 2026-08-23

The output of a whole-app audit: five parallel review lanes plus an
outside-family pass over the codebase at v0.9.8, every finding adversarially
verified before it was accepted, and every accepted finding fixed. Nothing
here is a feature request; each entry below traces to a confirmed defect.

### Added

- **Non-JSON response bodies now cross the edge tunnel, when negotiated.** The
  edge response envelope's `body` is a `json.RawMessage`, so a Docker API
  response that isn't valid JSON — the plain-text `OK` from `GET /_ping` —
  could never be carried. The agent now advertises an `edge-response-body-b64`
  capability in its hello; a controller that echoes it in the welcome's new
  `capabilities` field gets every non-streaming response body as standard
  base64 in a `bodyBase64` field. Negotiated rather than versioned on purpose:
  a version bump is a terminal mismatch that breaks any disagreeing pairing,
  while an old controller's welcome simply lacks the value and the agent keeps
  the legacy encoding. Requires a Drydock with the matching decoder
  (CodesWhat/drydock#852) to take effect; without one, an unencodable body now
  produces an error envelope instead of leaving the controller waiting forever
  for a response that will never arrive.

### Security

- **Edge mode refuses a plaintext controller URL.** Edge mode never
  authenticates the controller — trust rests entirely on TLS — yet
  `DRYDOCK_URL` accepted `http://` and `ws://` verbatim, so an on-path
  attacker who won the connection race could drive dockerd. Startup now fails
  closed on a plaintext scheme unless `ALLOW_INSECURE_EDGE_URL=true`, which
  warns on use and is for trusted local testing only.
- **The edge operations listener refuses non-loopback binds.** The listener
  serving health, metrics, and the audit export has no inbound
  authentication, and `BIND_ADDRESS=0.0.0.0` silently exposed the full audit
  trail. It now refuses a non-loopback address unless
  `ALLOW_UNAUTHENTICATED_REMOTE` is set, mirroring standard mode's guard —
  same env var, same fail-closed semantics.
- **The nonce-replay window is closed.** The timestamp check accepts clock
  skew symmetrically, but the nonce TTL equalled the skew window, so a nonce
  was evicted before its signature stopped being timestamp-valid — a captured
  request replayed in that gap passed both checks. Nonce retention is now
  twice `MAX_CLOCK_SKEW_SECONDS`, the full span a signed timestamp can stay
  valid.
- **A slow-trickled body can no longer pin goroutines before
  authentication.** The Ed25519 auth path read the whole request body with no
  deadline (`ReadTimeout` is deliberately 0 for streaming), so unauthenticated
  slow-drip connections each held a goroutine indefinitely. The auth body read
  now carries a 10-second `ResponseController` deadline, cleared before the
  streaming handlers so their unbounded reads are untouched.
- **Edge response-encoding failures sanitize wire-derived log fields.** Remote
  request IDs and paths pass through the same log sanitizer as other
  tunnel-controlled values before an encoding failure is recorded.

### Fixed

- **Shutdown waits for in-flight handlers to drain.** On SIGTERM the process
  exited the instant the listener closed, cutting an in-progress compose
  deploy or image pull mid-flight and leaving it partially applied. `run()`
  now waits for `Shutdown` to return — handlers drain or the shutdown timeout
  elapses — before `os.Exit` is reached.
- **Compose operations are serialized per stack.** Two concurrent `up`
  requests for the same stack could interleave file writes, deploying one
  caller's configuration while reporting success to the other. A per-stack
  lock is held from file write through `docker compose`, so same-stack
  requests serialize while different stacks stay concurrent.
- **The exec handshake is bounded.** `StartExec` dialed the Docker socket and
  blocked on the 101 upgrade with no deadline; a daemon that accepted the
  connection but never answered hung the goroutine forever, and each wedged
  session counted against the 100-session exec cap until restart exhausted
  it. The dial now honors the request context and the handshake carries a
  connection deadline.
- **Compose output is capped and the logs tail clamped.** `Execute` buffered
  a subprocess's entire stdout+stderr unbounded, and a `logs` call without a
  positive `tail` dumped the full container log — either could OOM the agent
  managing every other stack. Combined output is now capped at 10 MB with a
  truncation marker, and `logs` always passes `--tail` (default 100, max 500).
- **One shared Docker event stream.** `EventBroadcaster`'s registry was dead
  code while every SSE client opened its own `/events` connection to dockerd
  with its own reconnect loop. One upstream subscription now fans out to all
  clients, started on first register and cancelled on last leave.
- **Finished edge log streams release their context registration.** Every log
  stream that ended normally leaked its child registration on the
  connection-lifetime context — unbounded growth on a tunnel designed to stay
  up for weeks. Stream cleanup now cancels the context it created.
- **Edge compose requests reach the ComposeManager.** Edge mode advertised
  the `compose` capability but forwarded `/_portwing/compose` to dockerd,
  which 404s — a stack deployed through the tunnel never deployed. The path
  is now routed to the ComposeManager, mirroring standard mode.
- **Large image and container exports stream.** `GET /images/{name}/get`,
  `GET /images/get`, and `GET /containers/{id}/export` fell through to the
  full-buffer branch — up to 100 MB of binary tar held in memory per request.
  They now take the streaming path.
- **The comparison pages take their version from `SITE_CONFIG`.** Four of the
  website's six `/compare` pages hardcoded "Portwing v0.9.2" in SEO metadata,
  JSON-LD, and hero copy, six patch releases stale. They now interpolate the
  shared version constant, so the string can't drift again.
- **Tests that accepted any outcome now assert.** Seven tests looked like
  coverage of security-critical paths but constrained nothing — `t.Logf`
  where `t.Errorf` belonged, `if x != nil` wrappers with no else, a De Morgan
  inversion that always passed, a proxy test that passed when the request
  never arrived. Each now asserts the exact contract, verified by
  hand-reverting the production behavior it guards.
- **The docs match the code.** A docs-site audit closed the gaps the release
  cycle opened: `ALLOW_INSECURE_EDGE_URL` in the env-var references, the
  capability negotiation in the wire-protocol pages and SPEC, the compose
  caps, the precise nonce-retention semantics, the auth read deadline in the
  resource-caps table, and the `enrollment` audit event in the README.

## [v0.9.8] - 2026-08-21

### Fixed

- **A test that raced its own script and failed a required gate.**
  `TestExecute_MergesStdoutAndStderr` writes a shell script and then execs it,
  while marked `t.Parallel()`. Any concurrent fork in the process inherits the
  still-open write descriptor, and exec of a file held open for writing returns
  `ETXTBSY` — `fork/exec .../compose-both.sh: text file busy`, which is what CI
  reported. Dropping `t.Parallel()` closes it deterministically, because Go
  resumes parallel tests only once the serial ones finish, so nothing else in
  the package can fork while this test writes and execs. The four other tests
  here that write a fake binary were already serial for their own reasons,
  which is why this was the only one exposed. Not a retry and not a skip.

- **The four open CodeQL alerts that extending the scan to JavaScript
  surfaced.** Adding JS/TS coverage found them on 2026-08-20 and nothing acted
  on them, which is the half of adding a scanner that actually matters. All
  four were in build and test tooling; none were in the shipped agent.

  The first attempt at all four moved them instead of fixing them. CodeQL closed
  two and opened two fresh ones on the shifted lines, and the other two never
  closed at all. An edit that keeps a rule quiet is not the same as one that
  removes the defect, and only re-running the scan tells the two apart.

  The one worth reading is `js/bad-tag-filter` in the website's CSP generator.
  It collected inline-script hashes with a pattern that recognised only
  `<script>...</script>` in lower case with an exact end tag. A browser also
  accepts `<SCRIPT>`, `</script >` and `</script foo="bar">`, and every form the
  scan missed got no hash, so the emitted CSP would block a script the page
  needs: silent at build time and fatal in the browser. The end tag now mirrors
  the start tag and the whole pattern is case-insensitive. Verified by behaviour
  rather than by the alert clearing, with a test that hashes five tag forms and
  that fails on each of the two earlier patterns.

  Two `js/file-system-race` findings were a path check followed by a read of the
  same path, in the page-weight gate and a CI config test. Both now open the
  file once and take size, mode and contents from that one descriptor, so the
  two lookups cannot land on different files. Measured before and after: the
  page-weight totals are byte-identical.

  The fourth was a test asserting that the CSP does not name Go Report Card.
  Naming one host in a deny check only catches the host you thought of, and
  writing that check as a substring or an unanchored regex is itself the bypass
  pattern the scanner flags. It now asserts the CSP's entire external origin
  list, which is the stronger claim and leaves the rule nothing to match.

- **The star-chart refresh now fires on the `v*` tag push, which is the only
  trigger that actually works.** v0.9.7 shipped it as an explicit dispatch from
  `release.yml`, and that failed on the first real release with
  `HTTP 403: Resource not accessible by personal access token`: `RELEASE_PAT`
  carries Contents RW, and creating a workflow dispatch needs Actions write.

  The two earlier attempts failed for different reasons, which is worth keeping
  apart because only one of them can be fixed by granting something. A
  `release: [published]` trigger is inert because GoReleaser publishes with
  `GITHUB_TOKEN` and GitHub creates no workflow run for an event emitted with
  that credential. The 403 is not that: creating a workflow dispatch is an
  Actions API write, and `contents: write` does not imply `actions: write`.
  `GITHUB_TOKEN` would have hit the same wall, because `workflow_dispatch` is
  exempt from the suppression rule, as is `repository_dispatch`. That exemption
  is not the same as "only those two ever run": a `GITHUB_TOKEN` `pull_request`
  also creates a run, just an approval-gated one. The dispatch pair is what
  fires unattended.

  The tag push needed nothing new: `release-cut.yml` already pushes the `v*`
  tag with `RELEASE_PAT` so that downstream workflows fire, which is how
  `release.yml` has always been triggered. The `starchart-refresh` job is
  removed from `release.yml` and the contract now asserts the working trigger is
  present as well as the broken one being absent.

### Changed

- **Both reusable-workflow callers re-pinned to `11004e4`.** The organisation's
  `starchart-refresh.yml` and `main-is-released.yml` moved together. Verified
  before adopting: the `on:` and inputs surface of both is byte-identical to the
  previous pin, so nothing in either caller changes but the SHA. The upstream
  changes tighten `main-is-released` so that a tag named `snapshot` no longer
  satisfies the invariant, and stop a promotion merge reporting drift for the
  seconds between the merge and the tag push.

## [v0.9.7] - 2026-08-21

### Added

- **A daily monitor asserts that `main` points at a release tag.** It calls the
  organisation's reusable `main-is-released` workflow, which fails when `main`
  is not exactly a tagged release, and reports how many commits past the newest
  reachable tag it has drifted. The invariant is that `main` is the version
  users actually run, so an untagged `main` head is itself the alarm: every
  scanner that describes this project, Scorecard and CodeQL and Dependabot and
  the README badges, reads the default branch and nothing else.

  It is deliberately **not** a required status check. A promotion PR's merge
  result is untagged by definition, so requiring it would make every promotion
  fail a check it cannot pass and deadlock the merge. It is a monitor, not a
  gate.

  It fails today, and that is the point rather than a defect in the monitor:
  `main` currently sits past `v0.9.6`. Clearing it is either a release cut or
  moving the unshipped work back to a dev branch, and both are decisions about
  what users see rather than something to quietly resolve.

### Removed

- **The local `CODE_OF_CONDUCT.md`, in favour of the organisation-wide one.**
  The `.github` repository serves a Code of Conduct to every repository that
  does not carry its own, so a local copy opts this repository out of that
  permanently and nobody remembers to edit two files. Both documents named the
  same reporting address, `security@codeswhat.com`, so nothing about where a
  report goes changes; the organisation-wide document is the full Contributor
  Covenant 2.1 with the enforcement ladder, where the local copy was a
  fifteen-line summary. The README and `GOVERNANCE.md` now link to it directly
  rather than by relative path, which would have 404'd once the file was gone.

### Changed

- **The star-history chart refresh is dispatched from `release.yml` instead of
  being triggered by the release event.** GitHub suppresses workflow runs for
  events emitted by `GITHUB_TOKEN`, and GoReleaser publishes the release with
  exactly that credential, so a `release: [published]` trigger on the chart
  workflow fires and starts nothing. The workflow would have read as correctly
  wired while silently never running, and the only symptom would have been a
  chart that quietly stopped updating, which is the failure a committed
  artifact was chosen to avoid in the first place. This repository already
  documented the same hazard for tag pushes in `release-cut.yml`, which is why
  the `v*` tag is pushed with `RELEASE_PAT`.

  The refresh is now a `starchart-refresh` job in `release.yml`, gated on the
  release job, dispatching the chart workflow with `RELEASE_PAT`. It needs no
  new credential and fails in its own job rather than after the release has
  already published. The release contract asserts both halves: that the
  dispatch exists, and that the chart workflow has no `release:` trigger.

- **The star-history chart ships as a light/dark pair, chosen by the README.**
  The renderer upstream now draws the chart to the house shape and emits both
  `docs/assets/star-history.svg` and `docs/assets/star-history-dark.svg` from
  one fetch, and the README selects between them with a `<picture>` element.
  A single self-theming SVG could not do this: a media query inside an SVG
  loaded through `<img>` resolves against the reader's OS preference, not
  GitHub's own theme toggle, so anyone reading GitHub in dark mode on a light
  OS got a white card. `<picture>` follows the toggle. The `<img>` stays as the
  fallback so raw file views and mirrors still render.

  The refresh workflow is re-pinned to the paired renderer and now passes this
  repository's accent colour, which upstream requires with no default so a
  caller cannot silently inherit another repository's brand. Its trigger moves
  from a weekly cron to a published release: a committed artifact refreshed on a
  schedule mutates underneath a tag that already points at it. Because
  `release: [published]` fires after the tag exists and the refresh commits to
  the dev branch, each chart ships with the following release, which is a
  one-release lag rather than a chart that is current as of its own tag.

  The release contract grew checks for all of it, each one verified by mutation
  rather than by reading it back. Two gaps turned up that way: a `<source>`
  outside its `<picture>` is inert markup that satisfied every other check
  while silently serving the light chart to everyone, and an external URL
  inside a CSS `url()` is not preceded by `=`, so it slipped the
  third-party-reference pattern entirely.

- **`Release Contract` is declared as a required check on `main`.**
  `scripts/apply-branch-protection.sh` now lists it alongside the other
  twelve. The job has been running and passing on `main` since the change
  below landed, so the context exists before anything requires it —
  declaring a context that never posts would block every PR on a check
  that can never go green. Applying it is still the one human-run step;
  the script is the declared state, not the live state.

- **The package release contract runs on every push and PR, not only when a
  tag is cut.** `scripts/package-release-config-test.sh` is what keeps the
  README, docs site, website, installation examples, Dockerfiles, and
  GoReleaser config agreeing with each other and with CHANGELOG's newest
  release. It was wired into `release-cut.yml` only, so a change that broke
  it stayed green from the PR that introduced it right up until someone
  tried to ship — and the person who then had to fix it was whoever was
  mid-release, not whoever wrote it. It now also runs as a `Release
  Contract` CI job and as a lefthook pre-push step. `release-cut.yml`
  keeps its copy: that's the gate that actually blocks a bad tag.

  The CI job carries no `if:` condition on purpose. A required context that
  evaluates to skipped reports SKIPPED rather than success, and on a
  promotion PR the push run and the PR run race to land last, which has
  already blocked a merge in this repo once.

- **The star-history chart is a committed SVG instead of a third-party
  embed.** The README pulled a live chart from `warpchart.dev` on every
  page view, which sent visitor IPs to a third party and made the section
  depend on someone else's uptime. It's now generated from GitHub's own
  stargazer timestamps and committed at `docs/assets/star-history.svg`,
  referenced by relative path, so rendering it costs no external request.
  No credential is needed: the Actions `GITHUB_TOKEN` already reads a
  public repo's stargazers, and a committed artifact needs nothing at
  runtime. `starchart.yml` refreshes it weekly through the org's reusable
  workflow, committing back only when the chart actually changed. The
  generator's output is deterministic, verified byte-for-byte across two
  runs, so a quiet week is a clean no-op rather than a churn commit.

  This is the second replacement for the same section, which is why
  `scripts/package-release-config-test.sh` now rejects **both**
  `star-history.com` and `warpchart.dev` by name in the README. Each was
  the prescribed answer at some point, and both fail the same quiet way: a
  live route serves a plausible card at HTTP 200 whether or not it has
  data, so nothing goes visibly red. A committed file fails loudly instead
  — stale is readable, missing is a broken image.

  Rejecting the two hosts turned out not to be enough on its own. Both
  rejects were case-sensitive, so `WARPCHART.DEV` made the identical
  request and passed, and the paired `require_file` only proved some file
  sat at the path — the suite stayed green with the chart deleted from the
  README, repointed elsewhere, or truncated to zero bytes. The contract now
  matches hostnames in any case, pins the `img` src to the committed path,
  and requires the SVG to carry this repo's title and a closing tag, which
  is what catches a scheduled refresh that dies mid-write.

- **CodeRabbit reviews PRs into the dev branch again.** `.coderabbit.yaml`
  set no `reviews.auto_review.base_branches`, which makes CodeRabbit
  auto-review only PRs targeting the default branch. Under the strict
  release flow every feature PR targets the active dev branch and only the
  promotion PR targets `main`, so each PR posted "Review skipped: reviews
  are disabled for this base branch" and the code was reviewed once, in
  aggregate, after it had already landed. drydock's config has carried the
  setting all along; this brings portwing to the same shape. The values are
  regexes rather than globs, so `dev/.*` covers nested dev branches the way
  `dev/**` does in the workflow triggers.

- **The Greptile second-opinion caller can fire on its own now.**
  `.github/workflows/greptile.yml` triggers on the `second-opinion` label,
  but `.coderabbit.yaml` had no `labeling_instructions` entry for it, so
  the label only ever got applied by hand and the caller never fired
  unprompted. Adding the instructions alone would have been inert:
  `auto_apply_labels` was `false`, which makes CodeRabbit evaluate the rule
  and discard the result. `suggested_labels` is on too, because auto-apply
  applies *suggested* labels — leaving suggestions off silently disables it
  and reproduces the same dead config one layer down. That's the
  combination sockguard and careerrat were verified on; drydock's
  `false`/`true` pair is the dead one.

- **README picked up the social-proof badge row and moved Documentation
  up.** `readme-shape.md` specifies a three-row badge wall; the repo had
  identity and quality/security but no third row. Release downloads, the
  GHCR image, and Sponsor fill it — no Awesome-list or localization badge,
  because this repo is in neither, and no discussions badge, because it
  renders "0 total". Documentation moves to the second slot with the
  ordering the shape gives, and gains rows for the two live site surfaces.

- **The skipped Node lint and build checks report real names.** The
  `node-ci` call left `lint-check-name` and `build-check-name` unset, and
  GitHub does not evaluate an expression in a job's name when that job
  skips, so the two reported as the literal `Node CI /
  inputs.lint-check-name` and `Node CI / inputs.build-check-name`.
  `run-lint` and `run-build` stay off: `Node CI / Web Contract` already
  runs `npm run check:web`, which covers both, and enabling them would
  gate the same property twice. Neither name is a required context. The
  two org reusable-workflow pins also gained the trailing version comment
  every other `uses:` in the repo carries.

- **CI job names converged off emoji.** All 57 emoji occurrences across the
  12 workflow files are gone, per the house no-emoji-in-CI standard:
  workflow names, job names, `run-name:` expressions, and one step's
  summary output. Renaming a job renames its check-run context, so three
  required contexts change with it (`🔑 Security: Secrets`,
  `📦 Dependency Review`, `🔍 CodeQL Analysis` lose their prefixes). The
  seven `Go CI / ...` contexts are untouched: that prefix comes from the
  caller job's own name, already emoji-free since #101, and five of the
  seven suffixes are defined in the upstream reusable workflow and are not
  this repo's to rename. `scripts/package-release-config-test.sh` now
  rejects reintroduction of the retired emoji names and fails on any emoji
  anywhere under `.github/workflows/`, so this stays converged instead of
  drifting back.

### Security

- **Nothing scanned the image users actually pull.** `security-grype.yml`'s
  container scan builds its own image from the root `Dockerfile` — a
  Wolfi-only from-source build, resolved to the runner's single
  architecture — so the multi-arch manifest `release.yml` pushes to GHCR
  went out unscanned. A new `grype-published-image` job scans the pushed
  manifest by digest, once per published platform. It uses
  `grype registry:...@sha256:...` with an explicit `--platform` rather than
  `anchore/scan-action`, which exposes no platform input and silently
  resolves only the runner's native arch out of a multi-arch manifest,
  leaving the other legs unscanned while reporting green. The digest
  resolution fails closed rather than falling back to the mutable tag, and
  the job is deliberately **not** gated on repository visibility — only its
  SARIF upload is, so the gate keeps working if the repo ever goes private.
  Against the real v0.9.6 manifest this found 2 HIGH on amd64/arm64 and 59
  findings including 1 CRITICAL on armv7, none of which any existing scan
  could see. Portwing's own binary carries zero findings on all three
  platforms.

- **`linux/arm/v7` is scanned report-only, and that is a recorded
  exception.** Wolfi publishes no armv7 repo, so `Dockerfile.release`
  builds that leg from `alpine:3.24` with Alpine's prebuilt `docker-cli`
  and `docker-cli-compose` in place of Wolfi's `docker-compose`. Those are
  compiled with Go 1.26.3 and carry ~29 Critical/High stdlib advisories,
  every one of them fixed in Go 1.26.6 — which portwing's own `go.mod`
  already pins. No Alpine branch ships a `go >= 1.26.6`-built docker
  package yet (edge is on 1.26.5, one patch short), and `musl` carries
  CVE-2026-40200 with no fix anywhere. Suppressing all of that to force the
  leg green would hide real, fixable CVEs behind entries nobody would
  revisit, so the gap is left visible instead: amd64 and arm64 fail the
  release on HIGH and above, armv7 uploads SARIF without failing.
  RELEASING.md names the upstream blocker and the flip condition, and
  `scripts/package-release-config-test.sh` asserts all three platforms and
  their exact gate values, so the leg cannot be quietly dropped or
  un-gated. An unknown `gate:` value fails the job rather than defaulting
  to report-only.

- **The release pipeline no longer pipes an unverified script to a shell.**
  The published-image scan installed Grype with
  `curl https://get.anchore.io/grype | sh -s -- -v ...`. That `-v` verifies
  the release archive, but the script carrying it is fetched over a mutable
  URL and executes before any of that verification runs, so the check sits
  inside code an attacker would already control. Replaced with a fetch of
  the pinned release assets, a `cosign verify-blob` against Grype's release
  signer, and a checksum check, all ahead of anything executing. The signer
  identity is exact rather than a regexp — read off v0.110.0's own
  certificate SAN and confirmed to reject a near-miss identity — and
  `get.anchore.io` came out of the job's egress allow-list along with the
  pipe, and `rekor.sigstore.dev` went in: `cosign verify-blob` against a
  detached certificate and signature looks the entry up in the transparency
  log before reporting success, so without that endpoint the whole release
  would have died under `egress-policy: block`. The sibling jobs in the same
  workflow already allowed it; this one was the odd one out. The binary is
  installed with `sudo` because `/usr/local/bin` is root-owned on
  GitHub-hosted runners. The job also logs in to GHCR now: the image is
  public today, so the anonymous path works, but a private repo means a
  private package, and the job exists precisely to keep scanning in that
  case.

- **CVE-2026-14456 suppressed across all three legs.** The OpenSSL QUIC
  *server* DoS matches `libcrypto3`/`libssl3` on every published platform.
  Nothing in the image is a QUIC server: `objdump -p` on the extracted
  per-platform binaries shows `docker`, `docker-compose`, and `busybox`
  link only libc, making `wget` the sole libssl consumer, and the shipped
  `wget` has no QUIC support at all. Its only invocation is the
  `HEALTHCHECK` against portwing's own loopback listener. Grype reports fix
  state "unknown" on both bases, so there is nothing to bump to. Four
  entries, version-scoped per base (Wolfi 3.6.3-r4, Alpine 3.5.7-r0) so
  either distro shipping a fix forces re-review. Review by 2026-09-15
  alongside the CVE-2026-54876 entries.

- **CodeQL now scans JavaScript and TypeScript.** The job analyzed only
  `actions` and `go`, leaving 89 tracked `.ts`/`.tsx`/`.mjs` files across
  `website/`, `docs/`, `analytics/`, and `scripts/` with no SAST coverage
  at all. `javascript-typescript` joins the existing job's `languages:`
  rather than arriving as a matrix leg or a second job: a matrix appends
  the leg value to the check-run name even when it has a single entry, so
  it would post `CodeQL Analysis (go)` and never the bare `CodeQL
  Analysis` the ruleset requires, blocking every PR on a status that can
  no longer arrive. Job name, trigger, and category are unchanged, so the
  required context is untouched. A new
  `.github/codeql/codeql-config.yml` excludes generated directories and
  deliberately sets no `paths:` allowlist, so `analytics/src` and the root
  `scripts/*.mjs` are covered too.

- **The gosec required check actually gates now.** `Security: Gosec SAST`
  was a required context running with `-no-fail` and no other pass/fail
  logic, so it could never report failure — the ruleset claimed a gate
  that did not exist. gosec has no severity cutoff of its own, so simply
  dropping `-no-fail` would also gate on LOW-severity heuristics; `G104`
  (unhandled errors) alone was 15 of this repo's 40 historical findings.
  `-no-fail` therefore stays, paired with an explicit gate on SARIF
  `level == "error"`, which is where gosec maps its own MEDIUM and HIGH
  (`getSarifLevel` in `report/sarif/formatter.go`); LOW maps to
  `"warning"` and stays advisory. The gate is ordered after the SARIF
  upload so findings still reach the Security tab on a failing run, and
  fails closed on a missing, empty, or unparseable SARIF rather than
  passing silently. No PRs are wedged by this: gosec currently reports
  zero findings, confirmed against the last five code-scanning uploads,
  the full alert history, and a local run of the pinned v2.28.0.

- **gosec and Grype dependency scanning now gate pull requests.**
  `Security: Gosec SAST` and `Security: Grype Dependency Scan (Go + npm)`
  become required contexts on `main`, taking the ruleset from 10 required
  checks to 12. This required removing the `paths:` filter from
  `security-grype.yml`'s `pull_request` trigger: a path-filtered workflow
  produces no check run at all on a PR it does not match, so requiring one
  of its jobs would leave every docs-only PR waiting forever on a status
  that never arrives. The container scan stays excluded on purpose since
  it carries `if: github.event_name != 'pull_request'` and always skips on
  PRs, and `Security: Govulncheck` stays excluded as a duplicate of the
  already-required `Go CI / Govulncheck`. The trigger's branch filter also
  moves from `dev/*` to `dev/**` to match `ci-verify.yml`, so a nested dev
  branch cannot run one gate without the other.

- **Lighthouse budgets re-recorded against the current sites.** The old
  baselines described pages carrying ~1 MB of oversized images and the
  ceilings were loose enough to hide it. The docs site sat under its limit
  the entire time it was a megabyte overweight, so it never went red.
  Marketing ceiling 2,180,000 to 1,656,000 and docs 2,630,000 to
  1,545,000, each keeping the ~7% margin over baseline the budgets always
  used. Each recorded value is now the worst case across both environments
  the gate runs in, which is what let the old numbers drift: marketing
  total byte weight measures ~144 KB higher on a local checkout than on a
  GitHub runner, and both performance scores measure lower on the runner.
  The old baseline recorded only the runner, so the local pre-push hook
  was failing on a budget CI reported as green. `performanceMin` rises to
  0.66 (marketing) and 0.65 (docs), tracking the recorded baseline within
  0.05 and carrying more headroom than the previous 0.67/0.64 pair.

### Fixed

- **Both sites were shipping logos an order of magnitude larger than they
  render.** `portwing.png` was 1102x1102 (616 KB) against a 200px maximum
  render, and `codeswhat-logo.png` was 830x835 (364 KB) against a 26px
  footer coin; each was tracked twice, once per site. This matters here
  in a way it wouldn't elsewhere: `next.config` sets `output: "export"`
  with `images: { unoptimized: true }`, so `next/image` never resizes and
  the source file size IS the delivered size. Downscaled to 400px and
  128px. Lighthouse median total byte weight: marketing 1,927,134 to
  1,548,014, docs to 1,444,340 against a 2,466,239 baseline — the docs
  site had been carrying the same ~980 KB while sitting just under its
  own ceiling. The `baseline` figures in `lighthouse/*.cjs` were left
  describing the heavier pages and are re-recorded above.

- **Marketing site was 1.9 MB over its own page-weight budget.** The
  ecosystem section's `sockguard-logo.png` and `drydock-logo.png` shipped
  at 1023x1023 (1.5 MB) and 1041x694 (472 KB) while rendering at 128x128.
  Both are now 256px, matching the `sockguard-logo-dark.png` variant
  already sitting beside them in the same component. Lighthouse median
  total byte weight drops from 2,556,783 to 1,927,134 against a 2,180,000
  ceiling — back under the 2,039,127 baseline, not just under the limit.
  The overage was failing the `web` lefthook pre-push lane, so it blocked
  every local push regardless of what the change touched.

- **Native package attestation verification was blocked by its own egress
  policy.** The `Verify Native Packages` job's harden-runner allow-list
  was missing `tmaproduction.blob.core.windows.net`, the Azure blob host
  `gh attestation verify` fetches provenance bundles from, so the step
  failed with a connection refused on the v0.9.6 cut. The sibling
  `Verify Published` job already allowed that host; the two lists now
  agree. The published artifacts were never affected — v0.9.6's packages
  verify cleanly against the same attestation off-runner.
- **Checked-in branch protection no longer under-declares the required
  checks.** `scripts/apply-branch-protection.sh` still listed the six
  original required contexts while the live ruleset had grown to ten.
  Because the script updates the ruleset in place, running it would have
  silently dropped `Go CI / Qlty Check`, `Security: Secrets`,
  `Dependency Review`, and `CodeQL Analysis` — a script named "apply
  branch protection" that weakened it. The list now mirrors the live
  ruleset, and the header documents that a stale list removes checks.

### Removed

- **Retired the duplicate govulncheck job from `security-grype.yml`.** It
  gated nothing that `Go CI / Govulncheck` did not already gate as a
  required context, and it pinned its own copy of the tool. Removing it
  would have quietly dropped the only pull-request run of
  `scripts/verify-scanner-exclusions.sh`, because the other caller lives
  in the container-scan job, which always skips on PRs. The source-level
  check therefore moved into `grype-deps`, which does run on PRs, with a
  `setup-go` step and the two extra egress hosts that needs.
  `scripts/package-release-config-test.sh` now asserts the check lives
  specifically in `grype-deps`, not merely somewhere in the file.

- **Dropped `check.trivy.dev` from the qlty job's egress allow-list.** The
  trivy plugin was retired in favour of Grype and `.qlty/qlty.toml`
  enables no trivy plugin, so the entry was a leftover hole in an
  otherwise tight block-mode list.

- **Retired the dead star-history.com chart from the README.** The embed
  had been returning an SVG that renders "GitHub restricted access to
  star data" — HTTP 200, so nothing flagged it — and it sat directly
  above the working Warpchart block, which stays. Confirmed dead rather
  than assumed: the endpoint returns byte-identical 60,125-byte responses
  for different repositories, so it serves one generic error card to
  everyone. The website copy of this embed was removed separately in
  #160, where it was also leaking visitor IPs to a third party.

## [v0.9.6] - 2026-08-19

### Added

- **Label-gated Greptile second-opinion review.** A new `Greptile second
  opinion` workflow calls the organization's reusable
  `greptile-summon.yml` whenever a PR is labeled `second-opinion`,
  passing the PR number and head SHA. `greptile.json` keeps Greptile
  pinned to `skipReview: AUTOMATIC` so it never reviews on its own —
  this is purely an on-demand tiebreaker alongside CodeRabbit's
  automatic review.
- **Go coverage now uploads to Codecov via OIDC.** A new `📊 Coverage:
  Codecov Upload` job in `ci-verify.yml` downloads the `coverage.out`
  produced by the `Build & Test` lane and uploads it tokenlessly, per the
  org's Codecov-as-coverage-cloud standard. Non-gating (`fail_ci_if_error:
  false`, `continue-on-error: true`) — the vendor-free coverage floor in
  `scripts/ci/go-test.sh` remains the real gate. Replaces the Codecov
  wiring removed in #35 when this repo briefly moved coverage to Qlty
  Cloud.

### Fixed

- **README badges point at the default branch again.** The CI and Codecov
  badges were repointed at `dev/v0.9` during PR #151 review; the CI badge
  now tracks `?branch=main` like the other workflow badges, and the Codecov
  badge drops the branch qualifier entirely so it renders correctly once the
  branch is retired.
- **Codecov upload was blocked by the egress policy.** The `📊 Coverage:
  Codecov Upload` job's harden-runner allow-list was missing
  `ingest.codecov.io`, the host the Codecov CLI actually uploads to;
  `continue-on-error` masked the failure so the job reported green while
  zero uploads reached Codecov.
- **Dropped the third-party star-history.com embed from the marketing
  site.** Every visitor's browser made a direct request to
  api.star-history.com, leaking their IP to a third party and
  contradicting the cookieless-analytics posture. The chart is removed
  rather than replaced; star-history.com had also broken upstream after
  GitHub restricted stargazer API access.

### Security

- **Dedicated gitleaks secrets gate in CI.** A new `🔑 Security: Secrets`
  job scans the full Git history and the tracked working tree with a
  SHA-pinned gitleaks binary on every push and PR, independent of the
  qlty trufflehog plugin. The four historical findings it surfaces are
  documented placeholders (docs examples and the marketing site's
  fabricated demo key), ignored by fingerprint.
- **Secrets gate now resists self-tampering on PRs.** A PR could previously
  edit `scripts/scan-secrets.sh`, `.gitleaks.toml`, or `.gitleaksignore` in
  the same change that introduces a secret, blinding the scan of its own
  diff. On `pull_request` events the `🔑 Security: Secrets` job now restores
  those three files from the base ref before scanning, so a PR under test
  can't alter the gate that checks it; legitimate scanner/policy changes
  take effect after merge. Closes the self-tamper window CodeRabbit flagged
  on #144.
- **Scheduled ZAP baseline scan for the public sites.** A new weekly
  `🕷️ Security: ZAP Baseline` workflow runs a passive-only OWASP ZAP
  baseline scan against portwing.codeswhat.com — the appropriate DAST
  tier for a static, no-auth, no-cookie marketing/docs export, not a PR
  gate. `.zap/rules.tsv` documents and ignores ten dry-run findings
  (cache-control advisories, the deliberate `style-src unsafe-inline`,
  fabricated demo/example data flagged as private-IP disclosure, and
  same-origin SRI/COEP advisories that don't apply to this site), each
  with an evidence-backed justification.
- **ZAP baseline scan now fails closed on untriaged findings.** Dropped
  `cmd_options: '-I'` from `security-zap-baseline.yml`. With `-I` set,
  any finding not already in `.zap/rules.tsv` defaulted to `WARN`, and
  `-I` suppressed `WARN` from failing the job — so a brand-new finding
  would report green every week instead of failing for a human to
  triage and either fix or add to the rules file with a justification,
  the way the existing ten entries were handled. The ten already-vetted
  rules stay `IGNORE` and don't fail; only unlisted/new findings do.
- **Supply-chain claims qualified as SLSA Build L2.** README.md and
  SECURITY.md now say "SLSA Build L2 provenance" instead of the
  unqualified "SLSA provenance," matching what the release pipeline
  actually attests.
- **Native package release verification now checks build-provenance
  attestation.** `release.yml`'s `verify-native-packages` job runs
  `gh attestation verify` against every downloaded `.deb`/`.rpm`
  alongside the existing cosign signature check, closing the loop on the
  SLSA Build L2 attestation the pipeline already produces via
  `actions/attest-build-provenance` (`subject-checksums:
  dist/checksums.txt`) for every GoReleaser binary, native package, and
  per-archive SBOM. SECURITY.md's scope section now documents that
  coverage explicitly, matching what RELEASING.md already described.
- **Documented the signed-release-tags decision.** RELEASING.md now
  explains why `release-cut.yml`'s pushed tag stays a plain annotated
  tag rather than GPG/SSH-signed: GitHub can't verify an Actions-minted
  tag signature without real key management, and the Cosign artifact
  chain identity-pinned to `release.yml` is already the signature of
  record. Matches the house decision landed in CodesWhat/drydock#759. A
  `refs/tags/v*` protection ruleset (deletion/update/non-fast-forward,
  no `required_signatures`) lands separately as an org-side settings
  change.

### Removed

- **Deprecated trivy qlty plugin.** Trivy is deprecated org-wide in favor
  of Grype (already the vuln-scanning tool in CI); the `trivy` plugin
  block and every `trivy:*` triage entry are gone from `.qlty/qlty.toml`.
  The DS*/KSV* misconfig rules it triaged were already flagged and
  triaged in parallel by checkov's `CKV_DOCKER_*`/`CKV_K8S_*` rules
  (checkov was already enabled and covers the same Dockerfile and
  Kubernetes example manifest surfaces), so those triage entries now
  reference checkov alone. trufflehog is unaffected.

### Changed

- **README migrated to the codified org README shape.** Header stack
  reordered to logo / h1 / grabber / three-row badge wall / thick rule,
  with the badge wall regrouped into identity (version, platforms,
  license), quality/security (CI, OpenSSF Scorecard, OpenSSF Best
  Practices, qlty maintainability, Codecov coverage, nightly fuzz), and
  release-critical warnings placed after the rule. Adds the qlty
  maintainability badge and swaps the coverage badge to Codecov ahead of
  its upload rollout. Drops decorative emoji from every heading, the
  Contents list, and the Features table's leading icon column, and
  consolidates the previously split Community sections into one with a
  single Issues/Discussions/Discord routing sentence.
- CI runners are pinned to `ubuntu-24.04` instead of `ubuntu-latest`
  across all workflows, Renovate now targets the `dev/v0.9` integration
  branch instead of `main`, and CONTRIBUTING.md describes the actual
  integration-branch flow (it previously claimed a trunk-based flow
  targeting `main`).
- Re-scoped the two CVE-2026-54876 grype suppressions from openssl 3.6.3-r3
  to 3.6.3-r4 after the v0.9.5 image picked up Wolfi's rebuild. The version
  pin fired as designed; the re-review confirmed r4 carries no secfix for
  this CVE and the no-OCSP-consumer rationale still holds.

## [v0.9.5] - 2026-08-16

### Added

- **Privacy-first PostHog telemetry on the public sites.** The marketing and
  docs sites capture versioned pageview, CTA, and Core Web Vitals events
  through a shared cookieless PostHog client behind the CodesWhat proxy — no
  cookies, no persistence, autocapture off, and a CSP restricted to match.
  Vercel Analytics is removed. A new root web CI lane gates the sites with
  contract, build, page-weight, and five-run Lighthouse checks.

### Fixed

- **Cookieless analytics events were dropped at ingestion.** Envelopes left
  the browser well-formed, but PostHog's cookieless mode requires envelope
  fields the client wasn't carrying, so the project ingested zero events.
  The client now sends the fields ingestion requires.
- **Web Vitals events never reached PostHog.** The pinned `posthog-js` slim
  build doesn't construct `webVitalsAutocapture`, so `$web_vitals` was never
  emitted. A Portwing-side buffer built on Next.js `useReportWebVitals` now
  emits at most one complete or five-second partial envelope per page load,
  keyed by the same canonical route identity as the emitted events, with the
  reporter scoped to the initial page load so SPA navigations stay independent.

### Changed

- README Star History section now includes a live [Warpchart](https://warpchart.dev/r/CodesWhat/portwing) growth chart alongside the existing chart.
- **Grype suppressions re-scoped after the Wolfi docker-compose bump.**
  Wolfi's docker-compose moved to v5.4.0, un-matching the version-pinned
  GO-2026-5932 ignore for the bundled binary (code-scanning alert 191). The
  shipped binary was re-audited — `x/crypto/openpgp` is still not linked —
  and the ignore re-scoped to x/crypto v0.54.0. The GO-2026-5841
  `klauspost/compress` entry was dropped as dead config: the binary now
  embeds v1.19.1, past the advisory's v1.18.7 fix line.
- **OpenSSL OCSP client leak suppressed pending a Wolfi fix.** CVE-2026-54876
  (libcrypto3/libssl3 3.6.3-r3) is a memory leak in OpenSSL's opt-in TLS
  client OCSP response checking. Nothing in the image can enable it — the Go
  binaries don't link libssl, and busybox/wget contain no OCSP code — and no
  fixed Wolfi package exists yet. The ignore is version-scoped so the
  eventual Wolfi openssl bump forces re-review, with a hard review date of
  2026-09-15.

### Security

- **Rebuilt with Go 1.26.6.** The v0.9.4 image was built with Go 1.26.5,
  whose stdlib has since accumulated eight high-severity advisories
  (GO-2026-5026, -5942, -5972, -6088 through -6091, -6218), all fixed in
  1.26.6. This release's binaries and image embed the patched toolchain.

## [v0.9.4] - 2026-08-12

### Fixed

- **The monthly benchmark, weekly soak, and monthly mutation jobs could not
  report failure.** Each piped its run into `tee`, and GitHub's default step
  shell is `bash -e {0}` — `-e` without `-o pipefail` — so the step took
  `tee`'s exit code and went green no matter what the tool underneath did.
  The benchmark job had been silently timing out for roughly three months
  behind that mask. All three steps now set `shell: bash`, which turns on
  `pipefail`. The fuzz jobs were already correct; they capture
  `${PIPESTATUS[0]}` by hand.
- **The commit-message gate could never fail.** The `💬 Commit Message` job is
  a required status check, but carried `continue-on-error: true` and only
  emitted a `::warning::` on a bad subject, so a malformed commit message
  passed a check that existed to reject it. It now errors and exits non-zero.

### Changed

- **Grype vulnerability suppressions re-reviewed.** Every entry had passed
  its stated review date. Each was re-checked against the current advisory:
  nine no longer applied and were deleted outright, and the nine that remain
  carry a specific justification and a real next-review date instead of a
  lapsed one.
- **README badges corrected.** The Go Report Card badge was dead and is
  gone, the integration-test workflow now has a badge, and the container
  image size was stated as ~10 MB in two places when it is ~47 MB.

## [v0.9.3] - 2026-08-11

### Changed

- **Commit convention migrated from gitmoji to plain Conventional Commits.**
  The commit-msg validator, the git hook, and the CI commit-check workflow
  all moved to the new convention. `release-cut`'s bump-math grammar was
  hardened to parse the full commit header, so subjects like `urgent!:` no
  longer falsely trigger a major version bump. CI's commit check now
  delegates to the same shared validator script the hook uses, instead of
  duplicating the logic.
- **Renovate dependency updates applied.** `biome`, `turbo`, `@types/react`,
  `@types/node`, `postcss`, `typescript`, and `@types/react-dom` were bumped
  to their current pinned versions, and a stale `wolfi-base` digest
  reference in `Dockerfile.release` — missed when `Dockerfile` got the same
  bump — was corrected.

### Fixed

- **Lockfile regenerated to restore cross-platform optional dependencies.**
  The lockfile only carried resolved entries for the `darwin-arm64`
  platform, so `npm ci` on Linux CI builders never installed
  `lightningcss`'s native binaries, breaking website deploys. A full
  regeneration restores the complete platform matrix.

## [v0.9.2] - 2026-08-04

### Changed

- **Dependency sweep.** Every pinned GitHub Action moved to its current
  release, including the `actions/checkout` and `actions/setup-go` v7 majors
  (ESM migrations; the checkout v7 fork-ref restriction does not apply because
  no workflow uses `pull_request_target` or `workflow_run`). The
  `cgr.dev/chainguard/wolfi-base` digest, the macOS runner the Homebrew cask
  gate uses (`macos-15` → `macos-26`), the docs/website npm tree,
  TypeScript 7, Next 16.3, `@types/node` 26, and npm 12 are all current.
  TypeScript 7 removed `baseUrl`, so `docs/tsconfig.json` now uses the
  `paths`-only form the website config already used, and `engines.node` states
  npm 12's actual requirement instead of a looser `>=22.0.0`. The dead
  `js-yaml` override was dropped — nothing in the tree depends on it. Renovate
  no longer reports `google/uuid`, `gorilla/websocket`,
  `class-variance-authority`, or `clsx` as abandoned; they are stable by
  design, not unmaintained.
- **Versioned competitive landscape.** Added a primary-source comparison of
  Portainer Agent, Komodo Periphery, Arcane Agent, Hawser, Docker-native access,
  socket proxies, and adjacent agents. Corrected stale Komodo authentication
  and edge-mode claims, added Arcane to the website, separated agent features
  from Drydock controller responsibilities, and recorded pre-v1 gates,
  candidate work, and explicit security non-goals.

### Security

- **`X-Real-IP` is now validated before it can key the rate limiter.**
  `clientIP` required every hop of an `X-Forwarded-For` chain to parse as an IP
  address, but returned the `X-Real-IP` fallback header verbatim. Behind a
  configured trusted proxy, a caller could therefore send a distinct arbitrary
  string per request, mint a fresh limiter bucket each time, and walk past the
  10-failures-per-minute throttle on the authentication path — and write that
  same arbitrary string into audit records as the actor. The fallback now
  applies the same rules as the chain walk: the value must parse as an IP and
  must not itself be a trusted proxy, otherwise the direct peer is used.
- **Compose env-file lookup is now contained by `os.Root`.** `buildCommand`
  checked for a stack's `.env.drydock` with a plain `os.Stat`, which follows
  symlinks. A symlink planted at that path by any means other than Portwing's
  own writer (which already refuses to create symlinks) would have been
  resolved outside `STACKS_DIR` and passed to `docker compose --env-file`. The
  existence check now goes through `os.Root`, matching the containment the
  stack-file writer already used, and non-regular files are ignored. Closes the
  `go/path-injection` scanning alert.
- **Edge exec resize failures no longer log unsanitized values.** The initial
  `ResizeExec` failure path logged `execID` and the error without
  `applog.Sanitize`, the only such call in `internal/edge/tunnel.go`. Closes the
  `go/log-injection` scanning alert.

## [v0.9.1] - 2026-08-01

### Fixed

- **Public website and package metadata no longer point to a protected Vercel
  team alias.** Canonical URLs, documentation links, agent discovery metadata,
  native package metadata, and the Homebrew cask now use the public
  `portwing.codeswhat.com` domain. The release contract rejects the protected
  fallback alias on active publication surfaces.

## [v0.9.0] - 2026-08-01

### Added

- **Controller-owned Drydock watcher and update execution.** The `docker`
  watcher descriptor now declares `transport: "docker-api"`,
  `execution: "controller"`, and `events: "portwing"`, telling Drydock to run
  its native watcher and update trigger through Portwing's authenticated Docker
  proxy while Portwing remains the lifecycle-event source. Standard Mode uses
  the HTTP proxy; Edge Mode uses correlated WebSocket `request`/`response`
  messages. Edge connections now send `dd:component_sync` before
  `dd:container_sync` so the controller establishes ownership before ingesting
  raw inventory, and Portwing advertises no remote trigger. Full feature
  compatibility requires Drydock `v1.6.0-rc.11` or later; the additive
  descriptor fields do not change `DrydockCompat` (`1.4.0`).
- **Edge audit export.** Edge Mode now serves the same cursor-based
  `GET /_portwing/audit/export` NDJSON exporter as Standard Mode on its private
  operations listener. The Edge route intentionally has no inbound
  authentication, so the documentation and Kubernetes example now treat the
  listener as a sensitive trust boundary and restrict it to the monitoring
  workload with a headless Service and NetworkPolicy.
- **Edge operations bind defaults to loopback.** Because Edge health, metrics,
  and audit export intentionally have no inbound authentication, Edge Mode now
  defaults `BIND_ADDRESS` to `127.0.0.1`. Operators must explicitly choose a
  broader address for an isolated container or Kubernetes monitoring network;
  Standard Mode retains its `0.0.0.0` default for inbound controller traffic.

- **Edge exec + sockguard example.** `examples/docker-compose.edge-with-exec.yml`
  pairs Portwing's edge mode with sockguard's `portwing-with-exec.yaml` preset
  (`examples/sockguard-with-exec.yaml`), for deployments where Drydock's edge
  exec feature needs to reach through the sockguard proxy. The plain
  `docker-compose.with-sockguard.yml` / `sockguard.yaml` pairing still denies
  all exec by design.

### Fixed

- **Exec/Docker API errors now include the daemon or proxy response body.**
  Non-2xx responses from the Docker daemon (or a filtering proxy like
  sockguard) previously surfaced as a bare `docker error (status N)`,
  discarding the response body. `CreateExec`, `StartExec`, `ResizeExec`,
  `InspectContainer`, and the other Docker API calls in
  `internal/docker/client.go` now read a bounded slice of the body and
  include its message, so a sockguard denial like `exec denied: no commands
  are allowlisted` reaches the caller instead of a bare status code. The
  edge exec `failStart` path already forwards `err` into the `exec_end`
  reason sent to the controller, so the enriched message now reaches Drydock
  too. `extractDockerErrorMessage` now also reads sockguard's `reason`
  field — sockguard's detailed denial cause (populated only when
  `deny_verbosity` is `verbose`) lives there, not in `message`, which is
  always the generic `request denied by sockguard policy` sentence.
- **Authenticated compatibility checks no longer drop credentials.**
  `scripts/drydock-compat-check.sh` now preserves the configured token for its
  expected-404 and SSE probes instead of accidentally sending an empty value.

### Changed

- **Drydock live compatibility gate expanded to 35 checks.** The live suite
  now asserts the exact `transport=docker-api`, `execution=controller`, and
  `events=portwing` watcher markers plus empty trigger discovery, alongside the
  authenticated 404/SSE coverage.
- **Release and verification surfaces aligned.** Current-version examples now
  target v0.9.0, the compatibility matrix separates wire compatibility from
  the Drydock feature minimum, release verification documents the actual
  per-archive SBOM/checksum/provenance model, and package/website links use the
  controlled `getportwing-codeswhat.vercel.app` domain.

## [v0.8.1] - 2026-07-28

### Fixed

- **Native-install version command.** `portwing version` now prints the
  GoReleaser-injected version and exits before loading configuration or
  contacting Docker, so Homebrew, deb, and rpm installs can be verified on a
  clean host.
- **Native-package release verification.** The hardened verification runner
  now permits the pinned cosign installer to download from `github.com`,
  allowing package signature and clean-container install checks to run.

## [v0.8.0] - 2026-07-28

### Added

- **Mode-aware operations endpoints.** `/health` is now process-only liveness,
  while `/ready` and its `/_portwing/health` compatibility alias report mode,
  version, uptime, Docker state, and edge-controller state. Edge mode exposes a
  private operations listener with the same build/host/container metrics as
  standard mode plus controller connection, reconnect, and backpressure
  series.
- **Cursor-based audit export.** `GET /_portwing/audit/export` streams the
  stable ring-buffer schema as oldest-first NDJSON, with cursor/window headers
  and explicit 409 responses for overwritten history or a restarted agent
  generation. Prometheus now reports audit ring, sink, and export health.
- **Runnable observability topologies.** The Compose and Kubernetes examples
  cover standard and edge modes, TLS/bearer or Ed25519 authentication,
  liveness/readiness probes, authenticated/private Prometheus scraping, and a
  digest-pinned Fluent Bit audit-forwarding sidecar.
- **Continuous Drydock edge logs.** `dd:container_log_request` now supports a `stream:true` mode with correlated stdout/stderr chunks, explicit end/error frames, and controller-initiated cancellation. Portwing admits at most 128 live log readers, skips Docker frames larger than 256 KiB, and relies on the existing bounded edge send queue so a stalled controller is evicted rather than consuming memory without limit.
- **Cross-repo fleet-soak target.** The mock Docker daemon now supports real exec hijacks and sustained multiplexed log output so Drydock can run Portwing processes through reconnect storms, concurrent exec, continuous logs, and controller backpressure.
- **Native package channels.** GoReleaser now builds individually keyless-signed and checksummed `deb`/`rpm` packages for every supported Linux architecture, publishes stable macOS archives through `CodesWhat/homebrew-tap`, and gates releases on clean-container package installs plus a macOS Homebrew smoke test. Prereleases do not update the stable cask.

### Changed

- **Container healthchecks now mean liveness.** All image variants probe
  `/health` instead of dependency readiness and automatically select HTTP or
  HTTPS from `TLS_CERT`, preventing Docker/controller outages from causing
  container restart loops.
- **Drydock wire objects track the current controller schema.** Drydock adapter responses and events now serialize nested registry, tag, digest, update-kind, and runtime-detail objects without changing the generic REST adapter's simpler public model.
- **Edge mode is production supported.** Project docs, the documentation site, and getportwing-codeswhat.vercel.app now describe the stable `portwing/1.0` contract and Drydock 1.6's default-on endpoint; Drydock 1.5 still requires `DD_EXPERIMENTAL_PORTWING=true`.
- **Real fleet evidence.** An eight-agent, 45-second local gate completed 48 concurrent exec sessions, recovered from two reconnect storms and a forced slow-consumer reconnect, and stayed within its 128 MiB aggregate agent-RSS and 64 MiB controller-heap growth budgets.

## [v0.7.1] - 2026-07-28

### Fixed

- **Security scanner findings and log injection hardening.** Untrusted values written by the HTTP server, edge tunnel, Docker client, drydock adapter, and audit logger are now sanitized before reaching structured logs, and Ed25519 verification capacity is reserved atomically. Vulnerable website/docs dependencies were upgraded, generated CSP hashes were refreshed, and narrowly scoped Grype exclusions for unreachable OpenPGP and Docker Compose code now have CI guards that fail if either package becomes imported or linked into the shipped binary.

## [v0.7.0] - 2026-07-20

### Added

- **Edge log/delete request correlation**: the `dd:container_log_request` / `dd:container_log_response` and `dd:container_delete_request` / `dd:container_delete_response` pairs now carry an optional `requestId` that the agent echoes back on the response. This lets a controller correlate concurrent requests for the same container by id instead of matching responses in arrival order. This is an agent-side enablement: the Drydock controller must be updated to read the echoed `requestId` to benefit — its current FIFO-per-container fallback (which mismatches two in-flight requests for the same container) keeps working unchanged until then.
- **Edge log options**: `dd:container_log_request` now honors `follow` and adds a `timestamps` field. `timestamps` prepends an RFC3339 timestamp per line. Because the response is a single buffered message, `follow` is served as a bounded live window (the daemon is asked to end the stream a few seconds out, via a Unix-timestamp `until`) rather than an indefinite stream — continuous tailing still uses the generic `request`/`stream`/`stream_end` path against `GET /containers/{id}/logs?follow=1`. Drydock does not send these options over the edge path yet; the agent honors them when a controller does.
- **Edge reconnect classification**: a hello rejection with a terminal code (`ed25519-required`, `unknown-key`, `bad-signature`, `protocol-mismatch`, `no-auth`, `invalid-agent-name`, `parse-error`, `expected-hello`, `agent-name-claimed`) now makes the agent exit with an actionable error — a distinct `fatal connection error, not retrying` log — instead of silently reconnecting forever inside the process; timing/capacity codes and any unrecognized code are still retried with backoff. Previously every rejection, including a revoked key (`unknown-key`), retried indefinitely. Note: under a container restart policy (e.g. `restart: unless-stopped`) the container may still restart after the agent exits, so alert on the fatal log line or the restart count rather than assuming the process stays down.

### Fixed

- **Security hardening pass (PW-SEC-001–010).** Standard mode now fails closed without credentials, with separate explicit opt-ins for loopback and remote unauthenticated development. Ed25519 HTTP signature version 2 covers the escaped path and exact raw query; legacy signatures are query-free only. Cold Argon2id verification is capped at two agent-wide derivations and two in-flight attempts per IP, raw-token verifiers retain only fixed-size digests, and credential files are validated as regular files with safe Unix permissions on the opened descriptor. Enrollment bodies are capped at 64 KiB with malformed-request abuse accounting, Compose uploads use symlink-resistant `os.Root` writes, partial TLS configuration fails at startup, audit-copy claims now distinguish local append-only logging from external tamper evidence, and the exported website/docs routes ship generated CSP plus browser security headers. Every from-source Docker builder now uses the patched Go 1.26.5 toolchain, removing the last High-severity finding from the container scan.

- **Corrupted edge log output**: `dd:container_log_response.logs` now strips Docker's 8-byte multiplexed stream-frame headers for non-TTY containers (the same de-muxing the HTTP `/logs` route does) while passing a TTY container's raw stream through unchanged, so logs are plain text instead of text interleaved with binary frame headers (non-TTY) and are no longer garbled by demuxing a header-less stream (TTY).
- **Kubernetes example manifests**: `examples/kubernetes/standard.yaml` and `edge.yaml` mounted the Compose stacks directory (`/data/stacks`) as an `emptyDir`, which Kubernetes wipes on pod recreation — silently breaking `down`/`ps`/`stop`/`restart`/`logs` for every stack a prior `up` had deployed. Both now use a `hostPath` volume (with a note to pre-create it owned by UID 65532, since `fsGroup` does not apply to `hostPath`). The edge manifest's `portwing keygen` setup command was also missing its `> portwing_ed25519.pem` redirect, leaving the following `kubectl create secret --from-file=…` step referencing a file that was never written.

### Removed

- **Dead `DOCKER_HOST` config surface**: the top-level `DOCKER_HOST` environment variable and the corresponding `Config.DockerHost` field never had a consumer — Portwing only ever dials the Docker daemon over the Unix socket (`DOCKER_SOCKET`). Documentation is now unix-socket-only; the unrelated `DOCKER_HOST` entry in the Compose child-process env var denylist (which blocks a stack from redirecting the daemon a compose operation targets) is unchanged.

### Changed

- **Edge-mode setup examples fixed.** The README and Watchtower-migration edge-mode compose snippets showed a token-only credential, which fails to start — `PRIVATE_KEY_FILE` has been mandatory for edge mode since v0.5.0 (config load errors when `DRYDOCK_URL` is set without it). Both now generate and mount an Ed25519 key, and the incorrect "falls back to Standard mode" claim is removed (a missing key in edge mode is a fatal startup error, not a fallback).
- **Docs and website synced to v0.6.0.** Refreshed stale v0.5.x/alpha version badges, `cosign` verification examples, the marketing-site hero version and roadmap, and the "Hardened Runtime" copy (which still described the pre-0.6.0 root-by-default behavior). Corrected the `SECURITY.md` signed-request body cap (1 MB, not 64 MB) and its supported-versions table, and pointed the `security-model.md` control list at the docs site's fuller, independently numbered set.
- **SPEC.md gaps filled.** Moved the edge hello-rejection classification under Edge Mode Reconnection (§13.1) where it belongs, and documented the `GET /api/watchers/:type/:name` and `GET /api/log/entries` routes, the optional `exec_start` `tty` field, and the `RuntimeDetails` wire shape (including the 0.6.0 `env` field).
- **OpenAPI spec completeness.** `api/openapi.yaml` now documents the `GET /_portwing/audit` and `POST /_portwing/mcp` agent endpoints, the `GET /api/watchers/{type}/{name}` and `GET /api/log/entries` Drydock-compat routes, the `dd:watcher-snapshot` SSE event, the `timestamps` log query parameter, the `404`/`409` responses on `DELETE /api/containers/{id}`, and the previously-missing `digest`/`link`/`timestamp` container-result fields. The `info.version` and example version strings were bumped to 0.6.0.
- **Docs-site drift corrected.** Fixed the `GET /_portwing/audit` response example (`ts` key, no `level`/`msg`), the `portwing_auth_failures_total` reason-label values (`bad_token`/`no_credentials`/… rather than `bad-token`/`missing-token`), the generic-adapter capabilities list, the audit `key_id` sample format, the "core fields" audit table, and remaining stale `0.1.0`/`0.3.0` example version strings across the audit-logging, observability, standalone-mode, and api-reference pages.
- **Process docs corrected.** `CONTRIBUTING.md` now describes the actual trunk-based flow (all PRs branch from and target `main`; there is no `dev/*` branch), `RELEASING.md` no longer claims a `BREAKING CHANGE` footer triggers a major bump (the release-cut workflow only reads commit subjects), `Dockerfile.armv7`'s header comment points at the real unified `Dockerfile.release`, and `SPEC.md`'s container-image package list is split into its accurate Wolfi and Alpine variants.
- **`FuzzEnvelope` added to every fuzz tier.** The wire-envelope fuzzer (`internal/protocol`), which exercises the `portwing/1.0` parser on the untrusted edge-WebSocket input path, existed but was wired into none of the coverage tiers. It now runs in the `ci-verify.yml` smoke tier, the nightly and monthly deep-fuzz matrices, and the lefthook pre-push hook, and is listed in the CONTRIBUTING local-fuzz instructions.

## [0.6.0] - 2026-07-10

### Added

- **Agent info in the Drydock UI**: the `dd:ack` event now reports real host memory (`memoryGb`, read from `/proc/meminfo` with no cgo, rounded to one decimal GiB; 0 on non-Linux hosts), the agent's `logLevel`, and its `pollInterval` (as a Go duration string), so standard-mode agents no longer show 0 GB / blank runtime details in Drydock.
- **Edge-mode container deletion**: added the `dd:container_delete_request` / `dd:container_delete_response` wire message pair so Drydock's edge-mode `AgentClient.deleteContainer()` can remove containers over the WebSocket tunnel, matching the existing `dd:container_log_request` support for logs.
- **Container environment variables**: `RuntimeDetails` now includes `env` (`[]{key, value}`), parsed from Docker's `Config.Env`, matching Drydock's `ContainerRuntimeEnv` shape. Redaction of sensitive values remains Drydock's responsibility.
- **`COMPATIBILITY.md`**: canonical cross-repo version matrix (portwing × Drydock × sockguard preset × wire-compat constant) at the repo root.

### Fixed

- **Edge-mode hello rejection diagnostics**: when the Drydock controller rejects an agent's hello (bad signature, unknown agent, etc.), the agent now logs and surfaces the controller's `code` and `message` instead of a generic "expected welcome, got \"error\"" message.

### Security

- **Go toolchain bumped to 1.26.5 and `golang.org/x` dependencies refreshed** to clear published advisories. `go1.26.5` fixes the reachable `crypto/tls` vulnerability GO-2026-5856 (CVE-2026-42505) — called from the HTTPS server, the Docker client, and the banner writer — and the `os.Root` symlink-handling issue GO-2026-4970 (CVE-2026-39822). `golang.org/x/crypto` is updated 0.53.0 → 0.54.0 and `golang.org/x/sys` 0.46.0 → 0.47.0. The remaining osv-scanner finding, GO-2026-5932 in `golang.org/x/crypto/openpgp`, is unreachable (the agent imports only `x/crypto/argon2`) and has no fixed version, so it is triaged out in `.qlty/qlty.toml` rather than chased.
- **CI egress lockdown**: every workflow job now runs harden-runner with `egress-policy: block` and a per-job `allowed-endpoints` allowlist (24 jobs across 11 workflows) — previously all jobs ran in audit mode, which logs but does not stop exfiltration. Allowlists were derived from harden-runner audit telemetry across recent runs of every workflow, cross-checked against StepSecurity's recommended per-job policies, and adversarially reviewed per file (notably: `sum.golang.org` is allowed only for jobs that `go install` a tool outside the repo's `go.sum`; Docker Hub's dual CDN hostnames are both listed for image-pulling jobs; speculative endpoints were dropped). A compromised action or dependency can no longer phone home from CI.

### Changed

- **BREAKING: the container image now runs as the non-root `portwing` user (UID 65532) by default.** Previously the agent ran as root inside the container and could open the host Docker socket implicitly; deployments must now grant the socket's group explicitly — `group_add: ["${DOCKER_SOCK_GID}"]` in compose, `--group-add $(stat -c '%g' /var/run/docker.sock)` with `docker run`, or `supplementalGroups` in Kubernetes (the shipped k8s manifests already ran non-root and need no change). Mounted credential files must be readable by UID 65532 (`chown 65532:65532` + `chmod 0400`). `/data/stacks` and `/home/portwing` are pre-owned by the user so read-only-rootfs deployments and volume initialization keep working, and `DOCKER_CONFIG` defaults to `/tmp/.docker` so `docker login` during compose deploys works with a read-only root filesystem. `user: "0:0"` restores the old behavior. All examples and docs updated; this closes the last open finding from the June security audit.
- **Hermetic artifact builds**: `setup-go` caching is disabled in the release workflow and the GoReleaser config check, so no restored module/build cache can influence published artifacts. This clears zizmor's cache-poisoning findings at the root, and the now-redundant suppression config (`.github/zizmor.yml`, whose rationale predated the repo going public) is deleted — the workflow audit runs suppression-free.

## [0.5.1] - 2026-07-03

### Changed

- **Coverage reporting moved from Codecov to Qlty Cloud.** Part of the org-wide consolidation onto Qlty (one vendor for code quality and coverage). CI now publishes the Go coverprofile to Qlty Cloud via GitHub OIDC — no stored coverage token — and enforces a vendor-free statement-coverage floor (96%) with `go tool cover`, replacing the Codecov project ratchet and `codecov.yml`. A coverage badge was added to the README.
- **Blocking `qlty check` gate in CI.** A new CI job fails the build on any new qlty finding (shellcheck, hadolint, markdownlint, yamllint, and friends), with the linter configs (`.qlty/qlty.toml`, `.markdownlint.json`, `.yamllint.yml`) checked in and the existing scripts and Dockerfiles cleaned up to pass it. Standardizes the quality tooling with the sibling CodesWhat repos.
- **Marketing and docs sites converted to the shared CodesWhat web shell**, bringing chrome parity with the sibling project sites (CLI demo, comparison hub, OG/share imagery, favicons), and the configured site domain corrected from `getportwing.dev` to the controlled `getportwing-codeswhat.vercel.app` fallback. Source-only — nothing is deployed yet.

### Fixed

- **Spurious edge compat-level mismatch warning**: the `welcome`-frame `serverCompatLevel` was compared to the agent's compat level as an exact string, so any minor/patch difference (e.g. Drydock's `1.4` vs the agent's `1.4.0`) logged a "controller compat level mismatch" warning on every connect. The comparison is now major-version-only, matching Drydock's semantics.
- **Container-poll startup/shutdown race**: `ListenAndServe` created the poll context and wrote the cancel field after startup, racing a concurrent `Shutdown` that reads it — a shutdown arriving early could also observe nil and leak the polling goroutine. Both fields are now set once at construction and never reassigned.

## [0.5.0] - 2026-06-23

### Added

- **Application/request Prometheus metrics**: `/metrics` and `/_portwing/metrics` now also expose `portwing_http_requests_total{method,code}`, `portwing_http_request_duration_seconds` (histogram), `portwing_http_requests_in_flight` (gauge), `portwing_auth_failures_total{reason}`, and `portwing_rate_limited_total`. The endpoints and their existing build/host/per-container series are unchanged.
- **Audit ring buffer and `GET /_portwing/audit`**: setting `AUDIT_BUFFER_SIZE` (default 256, 0 disables) retains the most recent audit records in memory for pull-based retrieval at the new authenticated endpoint `GET /_portwing/audit`, which returns `{"records":[...],"count":N}` newest-first with an optional `?limit=N` query parameter. The buffer is independent of `AUDIT_LOG` and works even when the slog sink is off. The JSON record schema is unchanged (ts/event/actor/method/path/outcome/status/duration_ms plus event-specific fields).
- **Kubernetes deployment examples**: hardened DaemonSet manifests for standard and edge mode under `examples/kubernetes/` (read-only rootfs, dropped capabilities, non-root, node-socket mount, health probes).
- **Broadened test coverage**: a fuzz target for the wire `Envelope` parser (`FuzzEnvelope`), HTTP handler tests for the server (auth wrap, compose body limit, MCP method gating), drydock `HandleMessage` branch tests, and compose `validateRequest` injection-vector tests. A second cross-package unit backfill (edge wire contract, docker client/compose/events, config, metrics, server, generic, pool, drydock adapter) lifts overall coverage from ~46% to ~70%.
- **Configurable exec TTY**: the edge `exec_start` frame now honors a `tty` field, letting the controller request a non-TTY exec; it defaults to `true` when the field is absent, preserving the previous always-TTY behavior.
- **Codecov coverage ratchet**: CI uploads `coverage.out` to Codecov on every run, and `codecov.yml` enforces a no-regression project gate (`target: auto`, 1% wobble) plus an 80% patch gate on new/changed lines. The fuzz/soak/integration tiers are excluded from the accounting, so the number is a floor.

### Security

- **Pre-auth request body cap**: signed (Ed25519) requests are buffered through `http.MaxBytesReader` at 1 MB before signature verification — previously a 64 MB `io.LimitReader` with no read timeout, which let a slow-drip sender hold a large per-connection buffer before authenticating. Over-limit bodies now get 413. The exec (10 MB) and MCP (1 MB) limits are unchanged.
- **Registry-credential and proxy-parameter validation**: compose `registryAuth.server` must be a valid `https` URI before `docker login` runs (blocks redirecting a shared registry credential to an attacker-controlled host); container-log `since`/`until` and the other log query params are built with `url.Values` rather than string concatenation (blocks `&follow=1`-style injection that could pin an indefinitely blocking stream); container IDs/names are validated against the Docker charset before they are interpolated into Docker API paths.
- **Private signing-key permission check**: `PRIVATE_KEY_FILE` is rejected when it is group/world-accessible (looser than 0600), matching the existing `authorized_keys` check.
- **Outbound TLS posture**: the edge controller dial pins `MinVersion: TLS 1.2` (matching the inbound server), warns when `TLS_SKIP_VERIFY=true`, and refuses to send an unauthenticated hello when Ed25519 signing fails and no token fallback is configured (previously a silent downgrade).
- **Docker daemon error bodies are no longer forwarded to clients**: non-2xx Docker responses surface a generic `docker error (status N)` to API/MCP callers while the raw body is logged server-side; the MCP log demuxer also caps per-frame allocation at 256 KiB.
- **Nonce-cache behavior documented**: `SECURITY.md` now describes the replay window and fail-open-when-full semantics of the nonce LRU and why it is not reachable by an unauthenticated caller.

### Fixed

- **Exec terminal resize was permanently broken in edge mode**: post-startup resizes were sent to Docker using the controller's exec ID instead of the Docker-assigned exec instance ID, so every resize after the initial one 404'd. The session now records the Docker exec ID and uses it for resizes.
- **Exec resize no longer blocks the read pump**: `HandleResize` previously ran the Docker resize round-trip (up to ~450 ms of retries) inline on the WebSocket read pump, stalling pings and every other session. Resizes are now enqueued onto the same single per-session input drainer as keystrokes, preserving order and keeping the read pump free.
- **Exec sessions no longer leak across reconnects**: every live exec session is torn down when a controller connection ends, and the per-session goroutines recover from panics instead of taking down the agent process.
- **Goroutine lifecycle on shutdown**: the container-poll loop, the SIGHUP reload goroutine, and the rate-limiter / nonce-cache cleanup tickers now stop on shutdown, and the standard-mode audit log file is flushed and closed.
- **Container refresh no longer issues a Docker inspect per container every poll**: the periodic inventory refresh caches the built container and re-inspects only when the list entry's state/status/image changes (initial inventory still inspects every container).
- Removed two dead struct fields (`SSEClient.done`, `Collector.prevTime`); constrained the MCP route to `POST`; switched several `io.EOF` string/`==` comparisons to `errors.Is`; pooled the 32 KiB proxy stream buffer; and dropped an unnecessary `[]byte`→`string` copy on the request-body path.
- **Edge client aligned with the Drydock controller wire contract**: an Ed25519 hello-signing failure is now fatal instead of silently falling back to a token hash and reconnecting forever against a controller that only accepts Ed25519; a 404 on the WebSocket upgrade fails fast (naming `DD_EXPERIMENTAL_PORTWING` as the likely cause) rather than retrying indefinitely; the `welcome` frame is parsed so the agent honors the controller's `pollInterval` and warns on a `serverCompatLevel` mismatch; and inbound `error` frames are surfaced in the read pump instead of being dropped to the adapter default.

### Changed

- **Security-model docs truthed up**: removed fabricated `CVE-2026-*` identifiers and a fake "Arcane Docker Manager" advisory from `docs/security-model.md` and the docs-site `security-model.mdx`, replacing them with described vulnerability classes; corrected the Compose-guard control reference to "Control 8 — Compose input guards"; fixed a recurring "misonfigure" typo; and synced stale version strings. The marketing comparison now lists edge mode as early-access (Drydock 1.5+) instead of planned.
- **Tooling**: enabled the `unused` linter in `.golangci.yml`, pinned GoReleaser to an exact release (`v2.16.0`) instead of the floating `~> v2`, and converted `interface{}` to `any` across the codebase.
- **Edge mode now requires `PRIVATE_KEY_FILE`.** The Drydock controller is Ed25519-only and rejects token-only agents, so the agent fails fast at startup when running in edge mode without a signing key instead of looping on a rejected hello. **Breaking for token-only edge deployments:** provision a `PRIVATE_KEY_FILE` (and enroll its public key) before upgrading. Standard mode is unaffected.

## [0.4.0] - 2026-06-22

### Added

- **Tier-3 monthly deep fuzz**: `quality-fuzz-monthly.yml` gives each of the five fuzz targets a 1-hour budget on the first of the month (dispatchable to longer budgets before a release), completing the smoke → nightly → monthly fuzz tiering. Crash corpora retain for 180 days.
- **Weekly soak test**: `quality-soak-weekly.yml` runs the agent (generic adapter) against a mock Docker upstream under a sustained loadgen mix — inventory/version/proxy reads plus SSE subscriber connect/hold/disconnect churn — and fails if its resident set grows past a configurable budget (64 MiB default) over a multi-hour soak. New harness under `benchmarks/cmd/{mockdocker,loadgen}` driven by `scripts/soak.sh`. Catches the long-lived-agent leak profile the unit/integration/fuzz tiers don't.
- **Monthly benchmark tracking**: Go benchmarks on the per-request hot paths (auth middleware, Argon2id verify — cold derivation vs. warm SHA-256 cache, client-IP extraction, rate limiter) and the parse paths (PHC, image-ref, Drydock labels, trusted-proxy CIDRs, MCP dispatch). `quality-bench-monthly.yml` reruns them with `-benchmem -count=5` on the first of each month and retains the results for 90 days so a ns/op or allocs/op regression shows up month over month. Completes the test-posture parity with sockguard.
- **Edge tunnel test harness**: a dedicated unit-test harness for the edge WebSocket tunnel — an in-memory controller/agent WebSocket pair plus a consumer-side `dockerAPI` seam so the exec sessions and request fan-out run against a scripted fake Docker daemon with no live socket. Covers hello signing, request dispatch and concurrency rejection, the exec-session lifecycle (start/input/resize/output/end), ordered input replay, and send-path eviction. Lifts `internal/edge` exec/dispatch coverage from effectively zero to ~54%.

### Fixed

- **Edge exec input ordering**: `exec_input` that arrived immediately after `exec_start` could be dropped, because the session was only registered after the Docker `CreateExec`/`StartExec` round-trip completed. The session is now registered synchronously up front and early input is buffered in arrival order by a single per-session writer goroutine, then replayed once the exec connection is live — keystrokes typed before the shell comes up are no longer lost or reordered.
- **Edge outbound backpressure**: every sender (exec output, request/stream responses, metrics, pings) previously wrote the WebSocket directly under one mutex with no write deadline, so a single slow or wedged controller could head-of-line-block every session, stall the read pump, and hang the agent indefinitely. Outbound frames now funnel through a single `sendPump` goroutine fronting a bounded queue with a per-frame write deadline; a controller that can't keep up is evicted and reconnected rather than dropping frames (which would hang a request or corrupt a stream).
- **Grype container image scan**: the scheduled image scan failed on a stale cached rootfs. The scan now rebuilds the Wolfi rootfs without the BuildKit layer cache so weekly/manual runs pick up current packages, and `.grype.yaml` carries a scoped ignore for `GO-2026-4610` only where it is embedded in `/usr/bin/docker-compose` (a Windows Docker CLI plugin search-path advisory that does not apply to the Linux image). Also cleared the gosec `G115` finding in the mock Docker benchmark with a constant-safe log-frame length.

### Changed

- **Standardized dependency/CVE scanning on Grype + govulncheck; Snyk stays off Portwing.** Snyk's GitHub SCM integration scans the full Go *module requirement graph* (`go mod graph`) instead of the compiled build graph, so it flags advisories in modules that transitive deps merely *require* but the binary never links in (nothing in `go list -deps ./...`, nothing reachable per govulncheck, clean under Grype). That's a methodology gap, not staleness, so it's being decommissioned org-wide. Portwing never wired Snyk into the repo (no `.snyk` policy, no workflow step, no README badge), so there was nothing to strip out on the repo side. govulncheck (Go call-graph reachability) and Grype (the built image's binary build-info, plus `go.mod`/`go.sum` and the npm lockfiles) already cover dependencies accurately. The existing weekly scan is consolidated into `security-grype.yml`, which now also runs on pull requests (path-filtered to source/deps/Dockerfile/the workflow itself), keeps the weekly cron and manual dispatch, guards the heavy container build off PRs (govulncheck plus the dependency scan give fast PR coverage), gives each scanner a distinct code-scanning `category` so the Grype image and dependency SARIF no longer clobber each other in the Security tab, and runs gosec in report-only mode (`-no-fail`) so its heuristic findings still feed the Security tab without gating the build (CodeQL, Grype, and govulncheck handle the gating).
- **Pinned the Go toolchain to `go1.26.4` and made it the single source of truth for CI.** `go.mod` now carries a `toolchain go1.26.4` directive (it previously declared only `go 1.26.0`), so every build — local, CI, and release — runs on a stdlib past the reachable `crypto/x509` / `net/url` advisories (GO-2026-4599/4600/4601, fixed in 1.26.1) instead of whatever 1.26.x a runner happened to install. Every workflow's `setup-go` step switched from the floating `go-version: "1.26"` to `go-version-file: go.mod`, so the pin now governs the build, the govulncheck/Grype scans, and the release in lockstep — bump the toolchain in one place to move them all. `govulncheck ./...` is clean on 1.26.4.
- **Refreshed pinned base-image digests** via Dependabot: `golang:1.26.4-alpine` (builder) and `cgr.dev/chainguard/wolfi-base:latest` (runtime rootfs) moved to current digests, rebuilding the Wolfi packages with the latest upstream security fixes. Tag pins are unchanged — digests only.

## [0.3.0] - 2026-06-15

### Added

- **Startup banner**: the agent prints a centered truecolor half-block render of the logo plus a one-line `version · mode · adapter` summary at startup. Color is emitted only to a TTY (or under `FORCE_COLOR`) and suppressed under `NO_COLOR`, with ANSI escapes stripped so piped and log output stay clean.

### Changed

- **Renamed to Portwing** (formerly Lookout): the project name, Go module path (`github.com/codeswhat/portwing`), binary, Docker image, and every `lookout`-prefixed identifier are now `portwing`. **Breaking for anyone running a pre-release build:** the auth header `X-Lookout-Token` is now `X-Portwing-Token` (and the Ed25519 request headers `X-Lookout-Key-ID` / `-Timestamp` / `-Nonce` / `-Signature` are now `X-Portwing-*`), and the Prometheus metrics are renamed from `lookout_*` to `portwing_*` — update any clients, scrapers, dashboards, and alert rules accordingly. There is no backward-compatible alias.
- **Release pipeline**: pin GoReleaser to the `~> v2` major line (was `latest`) in both the release workflow and the CI config-check job, so neither can silently jump to a future GoReleaser v3 and to clear the action's "using 'latest' as default version" advisory.
- **Docker release build**: migrate from the deprecated GoReleaser `dockers` + `docker_manifests` blocks to a single `dockers_v2` entry that builds all three platforms (linux/amd64, linux/arm64, linux/arm/v7) in one buildx run and pushes a single multi-arch OCI index per tag. The two per-arch release Dockerfiles are unified into one `Dockerfile.release` that selects Wolfi (amd64/arm64) or Alpine (armv7) by `TARGETARCH`. The published image now also carries a Syft image SBOM attestation, and `:latest` is no longer moved by prereleases.

### Fixed

- **Edge reconnect backoff**: a long-lived edge session that drops now reconnects from `RECONNECT_DELAY` instead of inheriting stale exponential backoff from earlier connect failures (SPEC §13.1).
- **Edge read deadline**: the WebSocket read deadline is now held at a steady-state `max(2·HEARTBEAT_INTERVAL, 60s)` and re-armed on every received message, so a controller that goes silent (stops answering pings) is detected and triggers a reconnect instead of blocking forever (SPEC §13.2).
- **Flaky fuzz smoke / gating CI**: the Go fuzzing harness intermittently failed with a spurious `context deadline exceeded` (no crash, no slow input — verified handlers stay sub-10ms on adversarial inputs). On many-core machines Go fuzzing's default one-worker-per-core saturates every core and starves the coordinator goroutine until a worker misses its sync deadline. Both the pre-push hook and the gating CI fuzz job now cap fuzz worker count to `max(1, min(4, cores-1))` so the coordinator always keeps a core, which *prevents* the starvation; CI additionally retries the residual known `-fuzztime` boundary race once as a backstop (never retrying a real crash).

## [0.2.0] - 2026-06-12

### Added

- **Ed25519 per-request authentication**: signed requests via `X-Portwing-Key-ID` / `X-Portwing-Timestamp` / `X-Portwing-Nonce` / `X-Portwing-Signature` headers, verified against an `authorized_keys` file (`AUTHORIZED_KEYS`). Replay protection via nonce LRU (`NONCE_LRU_SIZE`) and timestamp window (`MAX_CLOCK_SKEW_SECONDS`), SIGHUP hot-reload of the key file, `portwing keygen` CLI subcommand, `X-Portwing-Reason` diagnostic header on 401s, and signed hello for edge mode (`PRIVATE_KEY_FILE`).
- **Key enrollment**: optional single-use `ENROLLMENT_TOKEN` (`POST /api/portwing/enroll`) for bootstrapping the first Ed25519 key — burned on first use, rate-limited, and audit-logged.
- **Repository infrastructure**: hardened CI (SHA-pinned actions, harden-runner, zizmor, actionlint), five Go fuzz targets (60s in CI, 5m nightly), integration suite against a real Docker daemon, weekly vulnerability scans (govulncheck/grype/gosec), monthly mutation testing, OpenSSF Scorecard, CodeQL, Dependabot, and a release-cut → release pipeline with CHANGELOG validation and post-publish verification.
- **Community and policy docs**: CONTRIBUTING, CODE_OF_CONDUCT, SECURITY (threat model + private advisory reporting), RELEASING, AGENTS, issue templates, CODEOWNERS.
- **Deployment examples**: hardened Docker Compose files for standard, edge, and sockguard-layered deployments (`examples/`), all `read_only` + `cap_drop: [ALL]` + `no-new-privileges` with secrets-based tokens.
- **Git hooks**: lefthook pipeline (lint, race tests, govulncheck, fuzz smoke, goreleaser dry-run, workflow checks) and an emoji-conventional-commit message validator.
- **Supply-chain pipeline**: cosign keyless signing of release archives (`checksums.txt.bundle`) and container images — both the per-arch images and the multi-arch manifest lists — CycloneDX SBOM generation via syft, and SLSA build provenance attestation via `actions/attest-build-provenance` (activated on public repositories).
- **Prometheus metrics**: `/metrics` and `/_portwing/metrics` endpoints exposing `portwing_build_info`, container count, and host resource metrics (CPU, memory, disk, network).
- **Argon2id token hashing**: `TOKEN_HASH` environment variable accepts an Argon2id PHC string (OWASP-recommended parameters: m=19456 KiB, t=2, p=1). `TOKEN_HASH_FILE` for Docker secrets support. A SHA-256 success cache keeps per-request cost flat after first verification. `portwing hash-token` CLI subcommand generates PHC strings.
- **Bearer auth**: `Authorization: Bearer <token>` header supported in addition to the `X-Portwing-Token` scheme.
- **`TRUSTED_PROXIES`**: configurable CIDR list for trusted reverse proxies; `X-Forwarded-For` is only honored from trusted sources for rate-limiting.
- **Audit logging**: structured JSON audit log (`AUDIT_LOG` env var) recording auth events (success, failure, rate-limit) with IP, user-agent, method, and path.
- **MCP server**: read-only Model Context Protocol server at `/_portwing/mcp` (Streamable HTTP transport, protocol revision 2025-11-25). Tools: `list_containers`, `inspect_container`, `container_logs`, `host_metrics`, `container_stats`.
- **Generic REST adapter**: headless REST+SSE management API (`internal/generic`) for standalone mode without a Drydock platform connection.
- **Drydock route additions**: `GET /api/log/entries`, watcher poll/get/container endpoints, trigger exec/batch endpoints.
- **`dd:watcher-snapshot` SSE event**: full watcher + container inventory payload emitted immediately after `dd:ack` on SSE connect and after every poll cycle, so Drydock can prune stale containers.
- **OpenAPI 3.1 spec**: `api/openapi.yaml` documenting all endpoints, request/response schemas, and security schemes.
- **Security model doc**: `docs/security-model.md` describing the defense-in-depth posture.
- **Watchtower migration guide**: `docs/migrating-from-watchtower.md` for teams migrating from Watchtower.

### Fixed

- **Streaming through auth middleware**: response body flushing and WebSocket hijack now work correctly when wrapped by the auth middleware chain (`statusRecorder` forwards `Flush` and `Hijack`).
- **Version injection**: GoReleaser ldflags now correctly set `protocol.AgentVersion` at build time.

## [0.1.0] - 2025-06-01

### Added

- Initial release of Portwing remote Docker agent.
- **Transparent Docker API proxy**: forwards Docker API requests from the Drydock platform to the local Docker daemon in standard mode.
- **Edge mode**: WebSocket tunnel to Drydock platform (`DRYDOCK_URL` + `TOKEN`) for environments where inbound connections are not possible.
- **Drydock adapter**: full Drydock protocol compatibility — container sync, component sync, watcher/trigger stubs, SSE broadcasting.
- **SSE event stream**: real-time container state events at `/api/events`.
- **Standard mode HTTP server**: `/_portwing/health`, `/_portwing/info`, `/api/containers`, `/api/containers/{id}/logs`, `/api/events`.
- **Token authentication**: `TOKEN` environment variable with timing-safe comparison.
- **Rate limiting**: 10 failed auth attempts per IP per minute.
- **Multi-arch Docker image**: `linux/amd64` and `linux/arm64` via GoReleaser + Wolfi (Chainguard) base image.
- **Static binary**: CGO_ENABLED=0, stripped, no external runtime dependencies.
