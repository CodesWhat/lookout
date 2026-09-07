# Portwing Roadmap

> Portwing is pre-`v1.0.0` software (currently `v0.9.16`). This roadmap describes
> direction and priorities — not commitments. Items and ordering may change
> between releases. For the authoritative record of what has shipped, see the
> [CHANGELOG](CHANGELOG.md).

This direction covers at least the next twelve months, through August 2027.
Items without a release assignment remain demand-driven rather than promised.

## Shipped — edge mode, hardening, and quality gates

The security-hardening, release/supply-chain, test-coverage, and edge-tunnel
work previously tracked here as "in progress" landed across v0.5.0 through
v0.9.0 — see [CHANGELOG.md](CHANGELOG.md) for the itemized history. In
particular:

- **End-to-end edge mode.** Drydock 1.5 first shipped the matching
  `/api/portwing/ws` controller endpoint (Ed25519-only); Drydock 1.6 enables
  the stable `portwing/1.0` endpoint by default. Portwing can dial out and
  manage NAT'd / firewalled hosts with no inbound control port published.
- **Edge tunnel robustness.** Ordered exec I/O, a single-writer outbound
  backpressure path with per-frame write deadlines and slow-consumer eviction,
  and a dedicated wire-contract test harness (`internal/edge/wire_contract_test.go`).
- **Three-tier fuzzing, soak testing, and benchmark tracking** in CI, at
  parity with sockguard's quality posture — `quality-fuzz-nightly.yml` /
  `quality-fuzz-monthly.yml`, `quality-soak-weekly.yml`
  (`benchmarks/cmd/{mockdocker,loadgen}` + `scripts/soak.sh`), and
  `quality-bench-monthly.yml`.
- **Reproducible base images.** `Dockerfile` and `Dockerfile.release` pin
  every base image by digest, with Dependabot tracking the `docker` ecosystem.
- **Controller-owned watcher/update execution.** Portwing marks the Docker
  watcher as `transport=docker-api`, `execution=controller`, and
  `events=portwing`; Drydock runs its native watcher and update trigger through
  Portwing over Standard HTTP or Edge correlated requests. The complete v0.9
  feature path requires Drydock `v1.6.0-rc.11+` while the stable wire contract
  remains `DrydockCompat` 1.4.0.
- **Shared Edge audit export.** The cursor-based NDJSON exporter is available
  on Edge Mode's private operations listener, with deployment examples that
  isolate its intentionally unauthenticated trust boundary.

## Toward `v1.0`

The path to `v1.0.0` is gated on concrete, verifiable items rather than a
calendar date:

- **Completed 2026-08-29 (re-verified 2026-09-02): Competitive review gate.**
  The market audit is published at
  [portwing.codeswhat.com/docs/competitive-landscape](https://portwing.codeswhat.com/docs/competitive-landscape). Comparison claims must
  stay tied to primary sources. The audit found no missing container lifecycle,
  streaming, Compose, observability, or authentication primitive that requires
  new Portwing code for v1.0. A final re-verify against primary sources at the
  v1.0 cut is the only work remaining.
- **Completed 2026-08-29: Tagged acceptance matrix.** Published Portwing's
  then-current release, Drydock v1.6.0, and Sockguard v2.0.0 artifacts passed the full
  Standard/Edge matrix in the
  [hosted conformance run](https://github.com/CodesWhat/sockguard/actions/runs/33283283870).
  A real Drydock controller exercised enrollment, overlapping-key rotation,
  revocation, two SIGHUP reloads, and clock-skew rejection and recovery. The
  legacy-floor row passed alongside the current Standard and Edge rows.
- **Completed for v0.8 — Edge-mode graduation.** Drydock 1.6 enables
  `/api/portwing/ws` by default, with `DD_EXPERIMENTAL_PORTWING=false` retained
  as an emergency disable. Drydock's `quality-portwing-fleet-soak.yml` runs the
  real cross-repo multi-agent soak: reconnect storms, sustained exec sessions,
  continuous logs, and controller-side backpressure with machine-checked RSS
  and heap budgets. Portwing's own weekly soak separately covers the
  Standard/generic HTTP path and SSE churn under an RSS-growth budget.
- **Completed for v0.8 — Published wire/API stability policy.** Semantic-versioning guarantees for
  the HTTP API surface, the environment-variable surface, and the
  MCP tool surface and `DrydockCompat` wire contract are defined in
  [STABILITY.md](STABILITY.md).
- **Completed for v0.8 — Package-manager distribution.** A Homebrew tap and
  signed/checksummed `deb`/`rpm` packages
  built through the existing GoReleaser pipeline, so installation doesn't
  require a container image or pulling the raw binary off GitHub Releases.
- **Completed for v0.8 — Continuous edge logs.** The `dd:container_log_*`
  namespace now supports correlated stdout/stderr chunks, explicit end/error
  frames, viewer cancellation, bounded agent/controller queues, and a legacy
  one-shot fallback for mixed-version fleets.
- **Completed for v0.8 — Operational ergonomics.** Mode-aware liveness and
  readiness, shared standard/edge metrics, cursor-based NDJSON audit export,
  and ready-to-run Compose/Kubernetes observability examples.
- **Completed for v0.9 — Controller-owned update integration.** Additive
  watcher transport/ownership/event markers, component-before-inventory Edge
  ordering, no remote-trigger advertisement, and documented Standard/Edge
  proxy paths are coordinated with Drydock `v1.6.0-rc.11+`.
- **Completed for v0.9 — Edge audit export hardening.** Standard and Edge
  modes share one exporter implementation; public docs and Kubernetes examples
  explicitly isolate Edge Mode's unauthenticated operations listener.
- **Docs stay current-only until v1.0.** `docs/content/docs/` is a single flat
  tree tracking the latest release, with no per-version archive. That's
  deliberate through `v0.9.x`. The v1.0.0 release-cut PR is the trigger to
  decide whether the semver guarantees in [STABILITY.md](STABILITY.md) need a
  versioned docs archive — e.g. drydock's `content/docs/{current,v1.x}` split
  plus `archive-provenance.json` and its release-docs-archive tests — before
  v1.0 ships.
- **SLSA Build L2 is the ceiling for now.** Provenance attestation (see
  [RELEASING.md](RELEASING.md)) stays at Build L2 because GoReleaser, cosign,
  and attest all run inline inside `release.yml`. Build L3 needs an isolated,
  org-shared reusable release workflow in `CodesWhat/.github`; none exists yet
  for any CodesWhat repo. The trigger is that workflow landing — portwing's
  release job moves onto it then, not before.

Post-v1 candidates are intentionally demand-driven: controller-managed
Portwing upgrade/rollback waves, optional client-certificate authentication,
polling/intermittent edge transport, controller-assisted two-key rotation, and
a BuildKit-aware Sockguard profile. None should expand the base socket policy
or add an unaudited self-update path merely for feature-table parity.

## Non-goals

- **Container orchestration** — Portwing controls a single host's Docker daemon.
  It is not a scheduler and not a Swarm / Kubernetes replacement.
- **A bundled UI** — Portwing is an agent; the control plane (e.g. Drydock) owns
  the user-facing interface.
- **Host administration** — arbitrary host shells, host-file browsing, volume
  backup, and secrets-vault behavior do not belong in the agent.
- **Fleet product features** — RBAC, GitOps, schedules, alert routing,
  vulnerability workflows, and agent rollout policy belong in Drydock.

---

*Detailed internal planning is tracked separately and intentionally not
published here.*
