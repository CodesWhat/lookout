# Portwing Security Assurance Case

Last reviewed: 2026-08-11

This document maps Portwing's security requirements to its threat model,
implementation controls, and public verification evidence. The detailed control
specification remains [`docs/security-model.md`](docs/security-model.md).

## Security requirements and limits

Portwing is intended to:

- authenticate every protected Standard Mode request and every Edge Mode
  controller connection;
- prevent replay, timing, traversal, flag-injection, and resource-exhaustion
  attacks at its exposed boundaries;
- protect stored credentials and avoid leaking them through logs or responses;
- fail closed when authentication or critical configuration is absent or
  incomplete;
- provide an attributable audit trail for security-relevant operations; and
- publish signed artifacts, SBOMs, and provenance from a verified release
  workflow.

An authenticated Portwing caller has broad Docker API authority. The Docker
socket is effectively host-root access, so authentication alone is not
least-privilege authorization. Production deployments should place
[Sockguard](https://github.com/CodesWhat/sockguard) or another allowlisting
proxy between Portwing and the raw socket. Support and disclosure commitments
are defined in [`SECURITY.md`](SECURITY.md).

## Threat model and trust boundaries

Threat actors include unauthenticated network clients, a holder of a stolen or
revoked credential, a malicious or compromised controller, hostile Docker API
responses, malicious Compose inputs, and a contributor or dependency attempting
to alter release artifacts.

The principal trust boundaries are:

1. **Standard Mode client to HTTP server.** Authentication, replay prevention,
   rate limiting, input validation, and TLS protect the inbound boundary.
2. **Edge Mode agent to controller.** The agent initiates the connection and
   authenticates the controller relationship, while bounded queues and message
   validation constrain the tunnel.
3. **Portwing to Docker API.** Requests cross into a host-root-equivalent
   service. Portwing mediates authentication and resource bounds; Sockguard is
   the recommended endpoint-authorization boundary.
4. **Request to filesystem and subprocess.** Root-confined file operations,
   environment restrictions, and argument validation protect Compose handling.
5. **Source to release artifact.** CI, review, signing, provenance, SBOM, and
   post-publication verification protect the supply-chain boundary.

## Claims and evidence

### Fail-safe defaults and complete mediation

Standard Mode refuses to start without a configured credential unless local
unauthenticated operation is explicitly acknowledged. Non-loopback anonymous
operation needs an additional acknowledgement. The catch-all Docker proxy and
named routes share authentication middleware, so protected operations do not
depend on individual handlers remembering to opt in.

Evidence: Controls 1 and 11 in
[`docs/security-model.md`](docs/security-model.md),
[`internal/server/`](internal/server), [`internal/auth/`](internal/auth), and
their adjacent tests.

### Authentication, replay, and secret handling

Per-client Ed25519 signatures bind the method, exact request target, body hash,
timestamp, and nonce. A timestamp window and atomic nonce LRU reject replays.
Shared tokens support Argon2id hashes, fixed-length timing-safe comparison, and
mounted secret files with permission checks.

Evidence: [`docs/design/ed25519-auth.md`](docs/design/ed25519-auth.md), Controls
2, 3, and 11 in [`docs/security-model.md`](docs/security-model.md), and
[`internal/auth/`](internal/auth).

### Input validation and resource limits

Compose writes stay beneath the configured stack root, dangerous environment
variables and flag-like service names are rejected, untrusted paths and image
references are parsed before use, and network and streaming operations have
documented size, concurrency, and time bounds. Panic recovery keeps malformed
requests from terminating the agent.

Evidence: Controls 4 through 10 in
[`docs/security-model.md`](docs/security-model.md),
[`internal/adapter/`](internal/adapter), [`internal/docker/`](internal/docker),
and the fuzz and integration targets documented in [`AGENTS.md`](AGENTS.md).

### Least privilege and defense in depth

The release image runs as UID 65532. The hardened deployment uses a read-only
root filesystem, drops all Linux capabilities, prevents new privileges, mounts
secrets as files, and places Sockguard in front of the Docker socket. These
controls reduce the impact of a process-level compromise without claiming that
the raw Docker socket itself is safe.

Evidence: [`Dockerfile`](Dockerfile), [`SECURITY.md`](SECURITY.md),
[`examples/docker-compose.with-sockguard.yml`](examples/docker-compose.with-sockguard.yml),
and the public [security model](https://portwing.codeswhat.com/docs/security-model).

### Common weakness, test, and release controls

CI applies formatting, vetting, static security analysis, dependency and image
vulnerability scans, race-enabled tests, integration tests, fuzzing, and a 97%
coverage floor. A separate weekly lane rescans the currently published GHCR
release image (the latest non-prerelease tag, both `linux/amd64` and
`linux/arm64`) for CVEs disclosed since that release was cut, independent of
the scans PR and release-cut runs perform. The release workflow creates signed
images and archives, CycloneDX SBOMs, and provenance, then verifies the
published artifacts against the expected workflow identity.

Fuzzing runs in five tiers over the same ten targets, so a finding is reachable
from a laptop and from a scheduled lane alike. Four of them run Go's native
engine: 5 seconds per target on pre-push, 60 seconds on every push and pull
request, 5 minutes nightly, and an hour per target on the first of the month.
The fifth rebuilds the identical targets against libFuzzer and
AddressSanitizer through OSS-Fuzz's toolchain (ClusterFuzzLite). It adds two
things the other four do not have. It explores differently: a second mutation
engine and a sanitizer-instrumented build take a different path through the
same input space, and it adds ASan-class detection on top of the panics and
assertion failures the Go engine reports. The overlap with what Go's engine
finds is not measured, and no claim is made that either reaches inputs the
other cannot. It also keeps a corpus that cannot expire, committed to an orphan
branch of this repository rather than held in the Actions cache the two
scheduled Go-engine lanes share, where an entry is evicted after seven days
without a hit. On a codebase this close to pure Go, AddressSanitizer's reach is
limited to the runtime boundary.

Evidence: [`.github/workflows/ci-verify.yml`](.github/workflows/ci-verify.yml),
[`.github/workflows/security-grype.yml`](.github/workflows/security-grype.yml),
[`.github/workflows/quality-fuzz-cflite-pr.yml`](.github/workflows/quality-fuzz-cflite-pr.yml),
[`.github/workflows/quality-fuzz-cflite-batch.yml`](.github/workflows/quality-fuzz-cflite-batch.yml),
[`.github/workflows/quality-fuzz-cflite-prune.yml`](.github/workflows/quality-fuzz-cflite-prune.yml),
[`.github/workflows/release.yml`](.github/workflows/release.yml), the public
[coverage report](https://qlty.sh/gh/CodesWhat/projects/portwing), and
[`docs/content/docs/verification.mdx`](docs/content/docs/verification.mdx).

The scheduled lanes also record what they measured, not only whether they
passed. The weekly RSS-growth soak and the monthly mutation matrix each append
their headline numbers to `quality-history`, an orphan branch carrying one
append-only JSONL file per lane, so a leak or a mutation-score decline that
stays inside its threshold on every individual run is still visible as a trend
across them. Reading a lane's series is
[`scripts/quality-history.sh`](scripts/quality-history.sh); the branch is never
merged and is not part of any released tree.

## Residual risk

A valid Docker-capable credential can still produce host-level impact. A
misconfigured socket policy, overly broad network exposure, controller
compromise, or stolen secret remains dangerous. Edge Mode makes the controller
a high-trust party. Operators remain responsible for network segmentation,
credential rotation, host hardening, backups, and selecting a Sockguard policy
that permits only the Docker operations their deployment needs.
