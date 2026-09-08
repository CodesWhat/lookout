<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="portwing-dark.png" />
  <source media="(prefers-color-scheme: light)" srcset="portwing.png" />
  <img src="portwing.png" alt="Portwing" width="200" height="200">
</picture>

<h1>Portwing</h1>

**Security-first remote Docker agent — control your containers from anywhere, safely.**

</div>

<p align="center">
  <a href="https://github.com/CodesWhat/portwing/releases"><img src="https://img.shields.io/github/v/release/CodesWhat/portwing?include_prereleases&label=release" alt="Release"></a>
  <a href="https://github.com/orgs/CodesWhat/packages/container/package/portwing"><img src="https://img.shields.io/badge/platforms-amd64%20%7C%20arm64%20%7C%20arm%2Fv7-informational?logo=linux&logoColor=white" alt="Multi-arch"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-C9A227" alt="License AGPL-3.0"></a>
  <br>
  <a href="https://github.com/CodesWhat/portwing/actions/workflows/ci-verify.yml"><img src="https://github.com/CodesWhat/portwing/actions/workflows/ci-verify.yml/badge.svg?branch=main" alt="CI"></a>
  <a href="https://securityscorecards.dev/viewer/?uri=github.com/CodesWhat/portwing"><img src="https://img.shields.io/ossf-scorecard/github.com/CodesWhat/portwing?label=openssf+scorecard&style=flat" alt="OpenSSF Scorecard"></a>
  <a href="https://www.bestpractices.dev/projects/14029"><img src="https://www.bestpractices.dev/projects/14029/badge" alt="OpenSSF Best Practices"></a>
  <a href="https://qlty.sh/gh/CodesWhat/projects/portwing"><img src="https://qlty.sh/badges/0c146428-4c10-46d3-8b6e-0622a7b07720/maintainability.svg" alt="Maintainability"></a>
  <a href="https://codecov.io/gh/CodesWhat/portwing"><img src="https://codecov.io/gh/CodesWhat/portwing/graph/badge.svg" alt="Coverage"></a>
  <a href="https://github.com/CodesWhat/portwing/actions/workflows/quality-fuzz-nightly.yml"><img src="https://github.com/CodesWhat/portwing/actions/workflows/quality-fuzz-nightly.yml/badge.svg?branch=main" alt="Nightly fuzz"></a>
  <br>
  <a href="https://github.com/CodesWhat/portwing/releases"><img src="https://img.shields.io/github/downloads/CodesWhat/portwing/total?logo=github&logoColor=white&label=downloads" alt="Release downloads"></a>
  <a href="https://github.com/orgs/CodesWhat/packages/container/package/portwing"><img src="https://img.shields.io/badge/GHCR-portwing-2ea44f?logo=github&logoColor=white" alt="GHCR image"></a>
  <a href="https://github.com/sponsors/CodesWhat"><img src="https://img.shields.io/badge/Sponsor-ea4aaa?logo=githubsponsors&logoColor=white" alt="Sponsor"></a>
</p>

<hr>

> [!WARNING]
> **Pre-1.0 software — APIs may still change.** Portwing is pre-`v1.0.0` (currently `v0.9.17`). The compatibility guarantees that already apply are published in [STABILITY.md](STABILITY.md); other surfaces may still change between minor releases. Pin to an exact version and review the [CHANGELOG](CHANGELOG.md) before upgrading.

<h2 align="center">Contents</h2>

- [Documentation](#documentation)
- [Quick Start](#quick-start)
- [Native Packages](#native-packages)
- [Recent Updates](#recent-updates)
- [Features](#features)
- [Authentication](#authentication)
- [Connection Modes](#connection-modes)
- [Standalone Mode](#standalone-generic-mode)
- [Configuration](#configuration)
- [API Reference](#api-reference)
- [Stability Policy](#stability-policy)
- [Competitive Landscape](#competitive-landscape)
- [Token Security](#token-security)
- [Verify a Release](#verify-a-release)
- [Security](#security)
- [Audit Logging](#audit-logging)
- [Star History](#star-history)
- [Built With](#built-with)
- [Community & Support](#community--support)

<hr>

> [!NOTE]
> **v0.9.17 is the current release.** This patch hardens authentication and resource limits and fixes Docker streaming, container inventory, Compose operations and Edge connection handling. Interrupted transfers report failure, long-lived stats and image pushes use streaming transport, and slow inventory refreshes or a full Drydock worker pool no longer stall Edge keepalives and control messages. It also fixes environment-only Compose updates, canceled stack-lock waits, OpenRC configuration export and invalid duration settings. Wire compatibility remains `portwing/1.0` and `DrydockCompat` 1.4.0; full watcher/update feature compatibility requires Drydock `v1.6.0-rc.11+`. See [CHANGELOG.md](CHANGELOG.md) for the full itemized history.

```mermaid
flowchart LR
    subgraph server ["Your server"]
        DD["Drydock<br/>(controller + UI)"]
    end

    subgraph hostA ["Remote host A"]
        direction LR
        LA["Portwing<br/>(agent)"]
        SGA["sockguard<br/>(socket filter)"]
        DA["Docker Engine"]
        LA -- "filtered socket" --> SGA --> DA
    end

    subgraph hostB ["Remote host B"]
        direction LR
        LB["Portwing<br/>(agent)"]
        SGB["sockguard<br/>(socket filter)"]
        DB["Docker Engine"]
        LB -- "filtered socket" --> SGB --> DB
    end

    DD -- "HTTPS + SSE · X-Dd-Agent-Secret" --> LA
    DD -- "HTTPS + SSE · X-Dd-Agent-Secret" --> LB
```

> The Drydock controller connects **inbound** to each standard-mode Portwing agent over HTTP/HTTPS (it initiates; Portwing serves). Each agent reaches the Docker Engine only through a sockguard socket filter. In production-supported **edge mode**, the agent instead dials Drydock over the stable `portwing/1.0` WebSocket tunnel, so no inbound control port needs publishing. Full v0.9 watcher/update integration requires Drydock `v1.6.0-rc.11+`. Keep the separate unauthenticated operations listener private — see [Connection Modes](#connection-modes).

<h2 align="center" id="documentation">Documentation</h2>

| Resource | Link |
| --- | --- |
| Website | [portwing.codeswhat.com](https://portwing.codeswhat.com) |
| Docs | [portwing.codeswhat.com/docs](https://portwing.codeswhat.com/docs) |
| Competitive Landscape | [portwing.codeswhat.com/docs/competitive-landscape](https://portwing.codeswhat.com/docs/competitive-landscape) |
| Security Model | [`docs/security-model.md`](docs/security-model.md) |
| Ed25519 Auth Design | [`docs/design/ed25519-auth.md`](docs/design/ed25519-auth.md) |
| Watchtower Migration | [`docs/migrating-from-watchtower.md`](docs/migrating-from-watchtower.md) |
| Drydock Integration | [`docs/drydock-integration.md`](docs/drydock-integration.md) |
| Benchmarks | [`BENCHMARKS.md`](BENCHMARKS.md) |
| OpenAPI Spec | [`api/openapi.yaml`](api/openapi.yaml) |
| Changelog | [`CHANGELOG.md`](CHANGELOG.md) |
| Contributing | [`CONTRIBUTING.md`](CONTRIBUTING.md) |
| Code of Conduct | [Contributor Covenant 2.1](CODE_OF_CONDUCT.md) |
| Governance | [`GOVERNANCE.md`](GOVERNANCE.md) |
| Security Assurance | [`SECURITY-ASSURANCE.md`](SECURITY-ASSURANCE.md) |
| Security Policy | [`SECURITY.md`](SECURITY.md) |
| Releasing | [`RELEASING.md`](RELEASING.md) |
| Examples | [`examples/`](examples/) |
| Issues | [GitHub Issues](https://github.com/CodesWhat/portwing/issues) |
| Discussions | [GitHub Discussions](https://github.com/CodesWhat/portwing/discussions) |

<hr>

<h2 align="center" id="quick-start">Quick Start</h2>

### Recommended deployment (hardened)

The strongest posture combines three controls: **sockguard** (socket-level request filtering so Portwing never touches the raw Docker socket directly), **Ed25519 authentication** (signed requests or the required signed edge hello, with replay protection and no shared secret), and a **hardened container runtime** (`read_only`, `cap_drop: ALL`, `no-new-privileges`, secrets-mounted credentials). The plaintext examples publish port 3000 only on host loopback. For remote access, either configure Portwing TLS before widening that bind, or keep the plaintext listener private behind a TLS-terminating reverse proxy. Use production-supported [edge mode](#connection-modes) when the host must dial out. Use Drydock `v1.6.0-rc.11+` for the complete v0.9 watcher/update contract.

**Step 1 — generate a token and pull the example:**

```bash
openssl rand -hex 32 > portwing_token.txt
sudo chown 65532:65532 portwing_token.txt && sudo chmod 0400 portwing_token.txt
export DOCKER_SOCK_GID=$(stat -c '%g' /var/run/docker.sock)
# Download the hardened compose file and its sockguard policy
curl -fsSLO https://raw.githubusercontent.com/CodesWhat/portwing/main/examples/docker-compose.with-sockguard.yml
curl -fsSLO https://raw.githubusercontent.com/CodesWhat/portwing/main/examples/sockguard.yaml
```

**Step 2 — start the hardened stack:**

```bash
docker compose -f docker-compose.with-sockguard.yml up -d
```

<h2 align="center" id="native-packages">Native Packages</h2>

Stable releases also ship a Homebrew cask plus signed/checksummed `deb` and
`rpm` packages for `amd64`, `arm64`, and `armv7`.

```bash
# macOS
brew install --cask codeswhat/tap/portwing

# Debian/Ubuntu (after downloading the matching release asset)
sudo apt install ./portwing_0.9.17_linux_amd64.deb

# Fedora/RHEL (after downloading the matching release asset)
sudo rpm --install ./portwing_0.9.17_linux_amd64.rpm
```

Packages install the command and, on Linux, a hardened `portwing.service`; they
do not start it before authentication is configured. See the
[native installation guide](https://portwing.codeswhat.com/docs/installation)
for artifact verification, configuration, upgrade, uninstall, and service-user
expectations.

This runs sockguard and Portwing as separate containers sharing a filtered socket volume. Neither container has the raw Docker socket mounted directly; sockguard enforces an allowlist of Docker API operations at the socket level. The full compose file (`examples/docker-compose.with-sockguard.yml`):

```yaml
# Portwing + sockguard — two-layer defense.
#
# Sockguard sits between Portwing and the host's Docker socket and writes a
# filtered unix socket into a shared named volume. Portwing talks to that
# filtered socket instead of mounting /var/run/docker.sock directly, so even
# a fully compromised agent is constrained to the explicit API allowlist in
# sockguard.yaml.
#
# Generate a token first and make it readable by the container user:
#   openssl rand -hex 32 > portwing_token.txt
#   sudo chown 65532:65532 portwing_token.txt && sudo chmod 0400 portwing_token.txt
#
# Both images run as UID 65532. Sockguard needs the numeric group ID of the
# host Docker socket to open it:
#   export DOCKER_SOCK_GID=$(stat -c '%g' /var/run/docker.sock)
# Portwing itself needs no group_add here — it talks only to sockguard's
# filtered socket, which sockguard creates 0600 under the same UID.
# This plaintext example publishes only on host loopback. For remote access,
# configure Portwing TLS before changing this bind, or keep the plaintext
# listener private behind a TLS-terminating reverse proxy.

services:
  sockguard:
    image: ghcr.io/codeswhat/sockguard:latest
    restart: unless-stopped
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    group_add:
      - "${DOCKER_SOCK_GID:?set to the GID of /var/run/docker.sock (see header)}"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ./sockguard.yaml:/etc/sockguard/sockguard.yaml:ro
      - sockguard-socket:/var/run/sockguard
    environment:
      - SOCKGUARD_LISTEN_SOCKET=/var/run/sockguard/sockguard.sock

  portwing:
    image: ghcr.io/codeswhat/portwing:0.9.17
    restart: unless-stopped
    depends_on:
      - sockguard
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - /tmp
    user: "65532:65532"  # image default; explicit so it survives image overrides
    ports:
      - "127.0.0.1:3000:3000"
    volumes:
      - sockguard-socket:/var/run/sockguard:ro
      - portwing-stacks:/data/stacks
    environment:
      - DOCKER_SOCKET=/var/run/sockguard/sockguard.sock
      - TOKEN_FILE=/run/secrets/portwing_token
    secrets:
      - portwing_token

secrets:
  portwing_token:
    file: ./portwing_token.txt

volumes:
  sockguard-socket:
  portwing-stacks:
```

By design, the `sockguard.yaml` preset above (a copy of sockguard's `portwing.yaml`) denies all exec — `POST /containers/*/exec` matches no allow rule and hits the catch-all deny, returning a 403 with a `not allowed by portwing preset` reason. If Drydock's edge exec feature is in play, use [`examples/docker-compose.edge-with-exec.yml`](examples/docker-compose.edge-with-exec.yml) instead, which pairs edge mode with sockguard's `portwing-with-exec.yaml` preset (`examples/sockguard-with-exec.yaml`) so exec is actually allowed through the proxy; under that preset, policy denials carry specific reasons instead, like `exec denied: privileged exec is not allowed`. Either way, the denial reason now reaches Portwing's exec error and is forwarded in the `exec_end` frame's reason to the controller (drydock-side display of it is still pending). Note sockguard only includes the detailed reason in its `reason` field when `deny_verbosity: verbose` is set in the preset; under the default `minimal` verbosity the surfaced message is just the generic `request denied by sockguard policy`.

**Upgrade to Ed25519 key auth (zero shared secrets):** generate a keypair with `portwing keygen`, mount the `authorized_keys` file, and set `AUTHORIZED_KEYS=/etc/portwing/authorized_keys` — see [Authentication](#authentication). Use `PRIVATE_KEY_FILE` for signed edge-mode hellos.

<details>
<summary>Edge mode variant (outbound WebSocket — stable portwing/1.0)</summary>

> **Production supported.** Edge mode uses the stable `portwing/1.0` protocol and is covered by Drydock's cross-repo `quality-portwing-fleet-soak.yml` workflow: real Portwing processes under multi-agent reconnect, exec, backpressure, and continuous-log load. Portwing's separate `quality-soak-weekly.yml` covers the Standard/generic HTTP path and SSE churn under an RSS-growth budget. Use Drydock `v1.6.0-rc.11+` for full v0.9 watcher/update feature compatibility; older controllers may remain wire-compatible without that behavior.

For hosts behind NAT or a firewall, [`examples/docker-compose.edge.yml`](examples/docker-compose.edge.yml) has Portwing dial out to your Drydock controller's edge endpoint (`DRYDOCK_URL` + `/api/portwing/ws`); no port is published on the remote host.

Edge mode is Ed25519-only — generate a keypair first and register the public key with Drydock (`POST /api/v1/portwing/keys`):

```bash
portwing keygen -comment "edge-host-01" > portwing_ed25519.pem
sudo chown 65532:65532 portwing_ed25519.pem && sudo chmod 0400 portwing_ed25519.pem
```

```yaml
services:
  portwing:
    image: ghcr.io/codeswhat/portwing:0.9.17
    restart: unless-stopped
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - /tmp
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - portwing-stacks:/data/stacks
    environment:
      - DRYDOCK_URL=https://drydock.example.com
      - PRIVATE_KEY_FILE=/run/secrets/portwing_key
      - AGENT_NAME=edge-host-01
    secrets:
      - portwing_key

secrets:
  portwing_key:
    file: ./portwing_ed25519.pem

volumes:
  portwing-stacks:
```

</details>

<details>
<summary>Quick start (evaluation only — not for production)</summary>

> **This is for trying Portwing out locally.** Environment-variable tokens are visible in `docker inspect` and process listings. Do not use in production — use the hardened deployment above instead.

```bash
docker run -d \
  --name portwing \
  --group-add $(stat -c '%g' /var/run/docker.sock) \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -p 127.0.0.1:3000:3000 \
  -e TOKEN=$(openssl rand -hex 24) \
  ghcr.io/codeswhat/portwing:0.9.17
```

Portwing now fails closed: Standard mode refuses to start without `TOKEN`,
`TOKEN_HASH`, or `AUTHORIZED_KEYS`. For local-only development, an explicitly
unauthenticated loopback listener requires `ALLOW_UNAUTHENTICATED=true` and
`BIND_ADDRESS=127.0.0.1` (or `::1`). A non-loopback unauthenticated listener
additionally requires `ALLOW_UNAUTHENTICATED_REMOTE=true`; never use that
second override on a shared or production network.

The image runs as the non-root `portwing` user (UID 65532); `--group-add` grants it the Docker socket's group so it can reach the daemon.

</details>

<details>
<summary>Binary install (install.sh)</summary>

```bash
curl -fsSL https://raw.githubusercontent.com/codeswhat/portwing/main/scripts/install.sh | bash
```

The generated standard-mode config binds to `127.0.0.1`. Keep that listener
private behind a TLS-terminating reverse proxy, or configure Portwing TLS before
changing the bind for remote access.

</details>

<hr>

<h2 align="center" id="recent-updates">Recent Updates</h2>

<details>
<summary><strong>Early release highlights (v0.1.0 – v0.3.0)</strong></summary>

For v0.4.0 and later — including v0.9.17, the current release — see [CHANGELOG.md](CHANGELOG.md) for the full itemized history.

- **v0.3.0** — startup banner, Lookout→Portwing rename completed, GoReleaser `dockers_v2` migration, and two edge-mode bug fixes (reconnect backoff reset, steady-state read deadline).
- **v0.2.0** — the security & observability release. Ed25519 per-request authentication with signed requests via `X-Portwing-Key-ID` / `X-Portwing-Timestamp` / `X-Portwing-Nonce` / `X-Portwing-Signature` headers, verified against an `authorized_keys` file. Replay protection via nonce LRU and timestamp window, SIGHUP hot-reload of the key file, `portwing keygen` CLI subcommand, and `X-Portwing-Reason` diagnostic header on 401s. Signed edge-mode hello via `PRIVATE_KEY_FILE`. Also shipped in v0.2.0:
  - **Key enrollment** — optional single-use `ENROLLMENT_TOKEN` (`POST /api/portwing/enroll`) for bootstrapping the first Ed25519 key — burned on first use, rate-limited, and audit-logged.
  - **Argon2id token hashing** — `TOKEN_HASH` / `TOKEN_HASH_FILE` with OWASP-recommended parameters; SHA-256 success cache keeps per-request cost flat.
  - **MCP server** — read-only Model Context Protocol endpoint at `/_portwing/mcp` (Streamable HTTP, protocol 2025-11-25) for AI assistants (Claude, Cursor, Windsurf). Tools: `list_containers`, `inspect_container`, `container_logs`, `host_metrics`, `container_stats`.
  - **Prometheus metrics** — `/metrics` and `/_portwing/metrics` exposing `portwing_build_info`, container count, and host resource metrics. (The per-request HTTP series — `portwing_http_requests_total`, the `portwing_http_request_duration_seconds` histogram, `portwing_http_requests_in_flight`, `portwing_auth_failures_total`, and `portwing_rate_limited_total` — were added later, in v0.5.0.)
  - **Structured audit logging** — `AUDIT_LOG` env var records auth events, Compose operations, and exec sessions as JSON lines.
  - **Generic REST adapter** — headless REST + SSE management API for standalone mode without a Drydock platform connection (`ADAPTER=generic`).
- **v0.1.0** — initial release: transparent Docker API proxy, Edge mode WebSocket tunnel, Drydock adapter, SSE event stream, token auth, rate limiting, multi-arch image.
- **Cross-cutting: CI & supply-chain hardening** (v0.2.0 onward, deepened through v0.5.0) — SHA-pinned actions, five Go fuzz targets (60s CI / 5m nightly), integration suite against a real Docker daemon, weekly vulnerability scans (govulncheck/grype/gosec), monthly mutation testing, OpenSSF Scorecard, CodeQL, cosign keyless signing, per-archive CycloneDX SBOMs, an image SBOM attestation, and SLSA Build L2 provenance on every release.

See [CHANGELOG.md](CHANGELOG.md) for the full itemized history.

</details>

<hr>

<h2 align="center" id="features">Features</h2>

| Feature | Description |
|---|---|
| **Connection Modes** | Standard mode lets Drydock connect inbound over HTTP/SSE. Production-supported edge mode lets the agent dial outbound over the stable `portwing/1.0` WebSocket tunnel for NAT/firewalled hosts; full v0.9 watcher/update support requires Drydock `v1.6.0-rc.11+`. |
| **Transparent Docker API Proxy** | All Docker Engine API paths forwarded to the local daemon — streaming endpoints, exec session hijacking, and long-lived connections included. |
| **Ed25519 Per-Client Authentication** | Per-request signatures with per-client keys, replay protection via nonce LRU and timestamp window, `authorized_keys`-style rotation via SIGHUP, zero shared secrets. |
| **Argon2id Token Hashing** | Hash your token at rest with OWASP-recommended Argon2id parameters; `TOKEN_HASH_FILE` for Docker secrets support; SHA-256 success cache keeps per-request overhead flat. |
| **MCP Server** | AI assistants connect to `/_portwing/mcp` (Streamable HTTP, protocol 2025-11-25). Read-only tools: `list_containers`, `inspect_container`, `container_logs`, `host_metrics`, `container_stats`. Env variable values are never transmitted. |
| **Container Inventory** | Full container metadata with `dd.*` label parsing and SSE broadcasting. Portwing marks watcher execution as controller-owned so compatible Drydock runs native watcher/update calls through the Standard or Edge Docker proxy. |
| **Prometheus Metrics** | Host and per-container CPU/memory/network in cAdvisor-compatible format at `/_portwing/metrics`. Zero external dependencies. |
| **Audit Logging** | Structured JSON of every API call, auth event, exec session, and Compose operation. Recent records are retained in memory by default; file/stdout/stderr persistence is opt-in. |
| **Host Metrics** | CPU, memory, disk, network, and uptime collection. |
| **Interactive Exec** | Terminal sessions via WebSocket or HTTP hijack with 100 concurrent session cap. |
| **Docker Compose** | Full lifecycle management with security hardening — path traversal protection, env var denylist, service name injection prevention. |
| **SSE Compatibility** | Drop-in replacement for existing Drydock agents, including `dd:watcher-snapshot` full inventory on connect. |
| **Signed Supply Chain** | Cosign keyless signatures, per-archive CycloneDX SBOMs, an image SBOM attestation, and SLSA Build L2 provenance on every release. Verifiable without managing signing keys. |
| **Two-Layer Defense** | Pair with [sockguard](https://github.com/codeswhat/sockguard) so the agent never touches the raw Docker socket directly. |
| **Minimal Footprint** | Static Go binary, ~47 MB Wolfi (Chainguard) container image. CGO disabled, stripped, no external runtime dependencies. |
| **Standalone Mode** | `ADAPTER=generic` provides a clean REST + SSE API on `/api/v1/*` backed by the local Docker daemon — no Drydock account required. |

<hr>

<h2 align="center" id="authentication">Authentication</h2>

<details>
<summary><strong>Token Authentication (quickstart)</strong></summary>

Set `TOKEN` to a random secret. All requests must supply it via
`Authorization: Bearer`, `X-Portwing-Token`, or `X-Dd-Agent-Secret`.

```bash
TOKEN=$(openssl rand -hex 32)
docker run -d --name portwing \
  --group-add $(stat -c '%g' /var/run/docker.sock) \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e TOKEN="$TOKEN" \
  -p 127.0.0.1:3000:3000 \
  ghcr.io/codeswhat/portwing:0.9.17
```

</details>

<details>
<summary><strong>Ed25519 Per-Client Key Authentication (recommended)</strong></summary>

Ed25519 keypairs give per-client identity with per-request signatures and
replay protection. No shared secrets.

**Generate a keypair:**

```bash
# Writes the private key (PEM PKCS#8) and the authorized_keys line to stdout.
portwing keygen -comment "my-platform:prod"
```

**Copy the `authorized_keys` line to the agent host:**

```text
# /etc/portwing/authorized_keys  (root:65532, mode 0640 — readable by the
# container's non-root user, not world-readable)
ed25519 AAAA... my-platform:prod
```

**Start the agent with Ed25519 auth:**

```bash
docker run -d --name portwing \
  --group-add $(stat -c '%g' /var/run/docker.sock) \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /etc/portwing/authorized_keys:/etc/portwing/authorized_keys:ro \
  -e AUTHORIZED_KEYS=/etc/portwing/authorized_keys \
  -p 127.0.0.1:3000:3000 \
  ghcr.io/codeswhat/portwing:0.9.17
```

**Key rotation (zero-downtime):**

1. Generate a new keypair: `portwing keygen -comment "my-platform:prod:2026-07"`
2. Append the new public key line to the authorized_keys file on the agent host.
3. Send `SIGHUP` to reload: `kill -HUP $(pidof portwing)` or
   `docker kill --signal HUP portwing`. Both old and new keys are now active.
4. Update the platform to use the new private key.
5. Remove the old key from the file and send another `SIGHUP`.

Token auth (`TOKEN`/`TOKEN_HASH`) continues to work alongside Ed25519 — both
can be set simultaneously during migration. The middleware checks for
`X-Portwing-Signature` first; if absent, it falls back to the token check.
HTTP signature version 2 also requires
`X-Portwing-Signature-Version: 2` and signs the complete request target,
including the escaped path and exact raw query string. Legacy unversioned
signatures are accepted only on query-free requests.

</details>

<hr>

<h2 align="center" id="connection-modes">Connection Modes</h2>

<details>
<summary><strong>Standard Mode and Edge Mode</strong></summary>

### Standard Mode — implemented

Portwing runs an HTTP(S) server; the **Drydock controller connects inbound** and pulls from it. This is the integration that works today.

- Set when `DRYDOCK_URL` is not configured
- Drydock authenticates with the `X-Dd-Agent-Secret` shared secret (optional mTLS)
- Handshake on `GET /api/containers` · `/api/watchers` · `/api/triggers`, then a long-lived **SSE** stream on `GET /api/events`
- Transparent Docker API proxy on all paths; agent endpoints under `/_portwing/*`
- Optional TLS with modern cipher suites (TLS 1.2+)

### Edge Mode — production supported

Portwing initiates an outbound WebSocket to the controller's edge endpoint (`DRYDOCK_URL` + `/api/portwing/ws`) for hosts with no inbound control port. The stable `portwing/1.0` path is covered by Drydock's cross-repo `quality-portwing-fleet-soak.yml` workflow, including real Portwing processes, concurrent exec, continuous logs, reconnect storms, and controller backpressure. Portwing's own `quality-soak-weekly.yml` is a separate Standard/generic HTTP and SSE RSS-growth soak. Drydock `v1.6.0-rc.11+` provides full v0.9 watcher/update feature compatibility; older compatible controllers can establish the wire connection but lack that coordinated execution model. The endpoint is **Ed25519-only**: set `PRIVATE_KEY_FILE` and register the public key with Drydock.

- Set when `DRYDOCK_URL` is configured along with `PRIVATE_KEY_FILE` — mandatory, not optional; Drydock rejects token-only agents, so `TOKEN` or `AUTHORIZED_KEYS` alone are not sufficient
- Targets hosts behind NAT, firewalls, and dynamic IPs
- Auto-reconnect with exponential backoff + jitter; signed hello via `PRIVATE_KEY_FILE`
- Sends watcher component ownership before raw inventory; controller-owned
  Docker calls use correlated WebSocket `request`/`response` messages

Edge mode's controller trust is **TLS-only and one-directional**: the signed
hello lets the controller verify Portwing, but nothing verifies the
controller's identity in return, so a plaintext `http://`/`ws://`
`DRYDOCK_URL` would let an on-path attacker complete the handshake and drive
dockerd. Portwing fails closed on a plaintext `DRYDOCK_URL` unless you set
`ALLOW_INSECURE_EDGE_URL=true`, which is for trusted local testing only —
always use `https://`/`wss://` against a real controller.

```text
DRYDOCK_URL set + PRIVATE_KEY_FILE set  →  Edge Mode (outbound WebSocket)
DRYDOCK_URL unset                       →  Standard Mode (inbound HTTP server)
```

`DRYDOCK_URL` set without `PRIVATE_KEY_FILE` is neither of the above — it's a
fatal startup error, since the edge endpoint is Ed25519-only and rejects
token-only agents.

</details>

<hr>

<h2 align="center" id="standalone-generic-mode">Standalone (Generic) Mode</h2>

<details>
<summary><strong>Run without a Drydock platform connection</strong></summary>

Run Portwing without any external controller by setting `ADAPTER=generic`.
You get a clean REST + SSE API on `/api/v1/*` backed directly by the local
Docker daemon — no Drydock account required.

```bash
docker run -d \
  --name portwing \
  --group-add $(stat -c '%g' /var/run/docker.sock) \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e ADAPTER=generic \
  -e TOKEN=my-secret \
  -p 127.0.0.1:3000:3000 \
  ghcr.io/codeswhat/portwing:0.9.17
```

### Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /api/v1/version` | Agent version, protocol info |
| `GET /api/v1/containers` | Cached container inventory |
| `GET /api/v1/containers/{id}/logs` | Container logs (`tail`, `since`, `until`, `follow`) |
| `GET /api/v1/events` | SSE stream of Docker lifecycle events |

### curl examples

```bash
TOKEN=my-secret

# Agent version
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:3000/api/v1/version | jq .

# Container inventory
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:3000/api/v1/containers | jq .

# Last 50 log lines from a container
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3000/api/v1/containers/my-container/logs?tail=50"

# Stream container logs live
curl -sN -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3000/api/v1/containers/my-container/logs?follow=1"

# Stream Docker lifecycle events (SSE)
curl -sN -H "Authorization: Bearer $TOKEN" \
  http://localhost:3000/api/v1/events
```

Each SSE event is a JSON object:

```json
{
  "ts": "2026-06-11T10:00:00Z",
  "type": "container",
  "action": "start",
  "containerId": "abc123def456",
  "name": "my-container",
  "image": "nginx:latest",
  "labels": { "app": "web" }
}
```

A comment heartbeat line (`: heartbeat`) is written every 30 seconds to keep
the connection alive through proxies.

</details>

<hr>

<h2 align="center" id="configuration">Configuration</h2>

<details>
<summary><strong>Environment variable reference</strong></summary>

### Connection

| Variable | Default | Description |
|----------|---------|-------------|
| `DRYDOCK_URL` | -- | WebSocket URL for Edge mode (`wss://...`). A plaintext `http://`/`ws://` scheme is rejected unless `ALLOW_INSECURE_EDGE_URL=true` — see below. |
| `TOKEN` | -- | Authentication token (plaintext) |
| `TOKEN_FILE` | -- | Path to file containing token |
| `TOKEN_HASH` | -- | Argon2id hash of token (generate with `portwing hash-token`) |
| `TOKEN_HASH_FILE` | -- | Path to file containing Argon2id hash |
| `AUTHORIZED_KEYS` | -- | Path to Ed25519 authorized_keys file (per-client asymmetric auth) |
| `AUTHORIZED_KEYS_FILE` | -- | Alias for `AUTHORIZED_KEYS` |
| `MAX_CLOCK_SKEW_SECONDS` | `60` | Maximum allowed clock skew for Ed25519 request timestamps |
| `NONCE_LRU_SIZE` | `10000` | In-memory nonce cache capacity for replay protection |
| `ENROLLMENT_TOKEN` | -- | One-shot bootstrap token for Model C key enrollment |
| `ENROLLMENT_TOKEN_FILE` | -- | File containing enrollment token |
| `PRIVATE_KEY_FILE` | -- | Ed25519 private key (PEM PKCS#8) for signing edge-mode hello |
| `CA_CERT` | -- | Custom CA certificate for Edge mode |
| `TLS_SKIP_VERIFY` | `false` | Skip TLS verification (testing only) |
| `PORT` | `3000` | HTTP server port |
| `BIND_ADDRESS` | Standard: `0.0.0.0`; Edge: `127.0.0.1` | HTTP/operations listener bind address. Edge mode refuses a non-loopback address unless `ALLOW_UNAUTHENTICATED_REMOTE=true` — the operations listener carries no authentication, so set it only on an isolated monitoring network. |
| `TLS_CERT` | -- | Server TLS certificate (Standard mode) |
| `TLS_KEY` | -- | Server TLS key (Standard mode) |
| `TRUSTED_PROXIES` | -- | Comma-separated CIDRs of reverse proxies whose `X-Forwarded-For` is trusted; unset means forwarding headers are ignored |
| `ALLOW_UNAUTHENTICATED` | `false` | Explicit local-development opt-in when no credential is configured; otherwise startup fails closed |
| `ALLOW_UNAUTHENTICATED_REMOTE` | `false` | Additional dangerous opt-in required for unauthenticated non-loopback binds |
| `ALLOW_INSECURE_EDGE_URL` | `false` | Dangerous opt-in to connect Edge mode to a plaintext `http://`/`ws://` `DRYDOCK_URL`; the controller's identity is otherwise verified by TLS alone, so this is for trusted local testing only |

`TLS_CERT` and `TLS_KEY` must be configured together. Setting only one is a
startup error.

### Docker

| Variable | Default | Description |
|----------|---------|-------------|
| `DOCKER_SOCKET` | Auto-detect | Docker socket path |
| `STACKS_DIR` | `/data/stacks` | Compose stack file directory |

Portwing talks to the Docker daemon over the Unix socket only — there is no `DOCKER_HOST` TCP transport.

### Agent Identity

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENT_ID` | UUID v4 | Unique agent identifier |
| `AGENT_NAME` | Hostname | Human-readable name |

### Operational

| Variable | Default | Description |
|----------|---------|-------------|
| `HEARTBEAT_INTERVAL` | `30` | Ping interval (seconds) |
| `WELCOME_TIMEOUT` | `30` | Seconds to await the Drydock welcome message in edge mode |
| `REQUEST_TIMEOUT` | `30` | Docker API request timeout (seconds) |
| `RECONNECT_DELAY` | `1` | Initial reconnect delay (seconds) |
| `MAX_RECONNECT_DELAY` | `60` | Max reconnect delay (seconds) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `SKIP_DF_COLLECTION` | -- | Disable disk metrics |
| `MAX_STREAM_SESSIONS` | `100` | Standard mode: max concurrent streaming proxy responses (SPEC 7.3); non-positive disables the bound. Edge mode's equivalent limit is fixed, not configurable. |
| `MAX_EXEC_SESSIONS` | `100` | Standard mode: max concurrent hijacked exec/attach sessions (SPEC 7.3); non-positive disables the bound. Edge mode's equivalent limit is fixed, not configurable. |
| `AUDIT_LOG` | -- | Optional persistent audit sink: `stdout`, `stderr`, or a file path; unset disables only the sink (the in-memory ring is controlled by `AUDIT_BUFFER_SIZE`) |
| `AUDIT_BUFFER_SIZE` | `256` | In-memory audit records retained for `GET /_portwing/audit`; `0` disables. Independent of `AUDIT_LOG`. |

### Adapter

| Variable | Default | Description |
|----------|---------|-------------|
| `ADAPTER` | `drydock` | Adapter to use: `drydock` (Drydock-compatible) or `generic` (standalone REST/SSE) |

### Drydock Compatibility

| Variable | Default | Description |
|----------|---------|-------------|
| `DD_AGENT_SECRET` | -- | Drydock agent secret token |
| `DD_AGENT_SECRET_FILE` | -- | Drydock agent secret token file |
| `DD_POLL_INTERVAL` | `300` | Container inventory refresh (seconds) |

</details>

<hr>

<h2 align="center" id="api-reference">API Reference</h2>

<details>
<summary><strong>Health, agent, MCP, Drydock-compatible, and proxy endpoints</strong></summary>

### Health Endpoints

| Endpoint | Method | Auth | Description |
|----------|--------|------|-------------|
| `/health` | GET | No | Process liveness; never probes dependencies |
| `/ready` | GET | No | Docker readiness, plus controller readiness in edge mode |
| `/_portwing/health` | GET | No | Compatibility alias for `/ready` |

Every response includes status, live/ready booleans, mode, version, uptime,
Docker state, and edge-controller state. `/ready` returns HTTP 503 when a
required dependency is unreachable. The image healthcheck uses `/health`;
Kubernetes examples use `/health` for liveness and `/ready` for readiness.

### Agent Endpoints

| Endpoint | Method | Auth | Description |
|----------|--------|------|-------------|
| `/_portwing/info` | GET | Yes | Agent version, mode, capabilities |
| `/_portwing/compose` | POST | Yes | Docker Compose operations |
| `/_portwing/metrics` | GET | Yes | Prometheus metrics (agent-scoped) |
| `/metrics` | GET | Yes | Prometheus metrics (Drydock agent secret) |
| `/_portwing/audit` | GET | Yes | Recent audit records (JSON, newest-first; `?limit=N`) |
| `/_portwing/audit/export` | GET | Standard: yes; edge: no | Cursor-based NDJSON export (oldest-first) |
| `/_portwing/mcp` | POST | Yes | MCP server (JSON-RPC 2.0, protocol 2025-11-25) |

In Edge Mode, `/_portwing/audit/export` is served without inbound
authentication on the same limited operations listener as `/metrics`,
`/health`, and `/ready`. Audit records can contain client addresses, request
paths, container identifiers, and operation names. Never publish this listener
to a host port or untrusted network; restrict it to the local/private
observability network and allow only the exporter that consumes it.

### MCP — AI Assistant Integration

Portwing exposes a read-only [Model Context Protocol](https://modelcontextprotocol.io/) endpoint
at `POST /_portwing/mcp`. AI assistants (Claude, Cursor, Windsurf, or any MCP client) can query
live container state through this endpoint using their standard tool-call flow.

**Protocol:** MCP 2025-11-25 — Streamable HTTP, stateless single-request mode, `Content-Type: application/json`.

**Available tools:**

| Tool | Description |
|------|-------------|
| `list_containers` | All containers — id, names, image, state, status, labels |
| `inspect_container(id)` | State, image, env-var count (values never exposed via this MCP tool), mounts, networks, restart policy |
| `container_logs(id, tail)` | Last N lines of stdout/stderr (max 500) |
| `host_metrics` | CPU, memory, disk, network, uptime snapshot (Linux only, see below) |
| `container_stats(id)` | One-shot CPU/memory/network stats for a container |

**Platform support:** `host_metrics` reads everything except the CPU core count and disk from
`/proc`, so it works on the Linux container, `.deb` and `.rpm` builds and on no other platform. On
a native macOS install (including the Homebrew cask) the tool returns an MCP error naming the
missing procfs rather than a snapshot of zeros, and `/metrics` reports
`portwing_host_metrics_supported 0` with the host resource series omitted. Every other MCP tool
is unaffected, since they read from the Docker API rather than the host.

Disk usage is measured separately with `statfs` against the Docker daemon's actual data root
(resolved from `/info`, not assumed to be `/var/lib/docker`), so it can fail independently of the
`/proc`-backed fields even on Linux — for example when the data root is unreadable. That failure
is reported in-band rather than as a zero: the response carries `diskMetricsAvailable: false` and
a `diskError` string, and `/metrics` reports `portwing_host_disk_metrics_available 0` with
`portwing_host_disk_total_bytes`/`portwing_host_disk_used_bytes` omitted, independent of
`portwing_host_metrics_supported`.

**Credential hygiene:** `inspect_container` returns only the *count* of environment variables —
values are never transmitted through this MCP tool, preventing accidental secret leakage. This
scoping applies only to `inspect_container`: the sibling `GET /api/containers` REST endpoint (and
the container inventory synced over the edge WebSocket) returns full container env var values by
design — see [Security Model](docs/security-model.md) for details.

#### Add to Claude Desktop (claude_desktop_config.json)

```json
{
  "mcpServers": {
    "portwing": {
      "command": "curl",
      "args": ["-s", "-X", "POST",
               "-H", "Content-Type: application/json",
               "-H", "Authorization: Bearer YOUR_PORTWING_TOKEN",
               "http://your-host:3000/_portwing/mcp"],
      "type": "http",
      "url": "http://your-host:3000/_portwing/mcp",
      "headers": {
        "Authorization": "Bearer YOUR_PORTWING_TOKEN"
      }
    }
  }
}
```

#### Add via claude mcp add (CLI)

```bash
claude mcp add --transport http \
  --header "Authorization: Bearer YOUR_PORTWING_TOKEN" \
  portwing http://your-host:3000/_portwing/mcp
```

#### .mcp.json (project-level, Cursor / Windsurf / any client)

```json
{
  "mcpServers": {
    "portwing": {
      "type": "http",
      "url": "http://your-host:3000/_portwing/mcp",
      "headers": {
        "Authorization": "Bearer YOUR_PORTWING_TOKEN"
      }
    }
  }
}
```

Replace `YOUR_PORTWING_TOKEN` with the value you set in `TOKEN` / `TOKEN_FILE` / `TOKEN_HASH`.

### Drydock-Compatible Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/events` | GET | SSE event stream (`dd:ack`, container events) |
| `/api/containers` | GET | Container inventory |
| `/api/containers/:id/logs` | GET | Container logs |
| `/api/containers/:id` | DELETE | Remove container |
| `/api/watchers` | GET | Watcher components |
| `/api/triggers` | GET | Trigger components |

### Docker API Proxy

All other paths (`/*`) are transparently proxied to the Docker Engine API, including streaming endpoints and exec session hijacking.

### Metrics

Portwing exposes Prometheus metrics at `/_portwing/metrics` (and the alias
`/metrics`). Standard mode requires bearer auth; edge mode exposes `/metrics`
and `/_portwing/audit/export` without authentication on its limited private
operations listener. In addition to
build/host/per-container and HTTP series, the endpoint reports edge-controller
connection/reconnect/backpressure state plus audit ring, sink, and export
health.

Prometheus scrape config:

```yaml
scrape_configs:
  - job_name: portwing
    scheme: https          # or http if TLS not configured
    static_configs:
      - targets: ["your-host:3000"]
    authorization:
      type: Bearer
      credentials: YOUR_PORTWING_TOKEN
    tls_config:
      # ca_file: /etc/prometheus/portwing-ca.crt  # if using custom CA
      insecure_skip_verify: false
```

</details>

<hr>

<h2 align="center" id="stability-policy">Stability Policy</h2>

Portwing `v0.9.x` publishes the compatibility contract that becomes binding at
`v1.0.0`. It defines semantic-versioning guarantees for the HTTP/OpenAPI
surface, environment variables, MCP tools and schemas, and the
`portwing/1.0`/`DrydockCompat` wire protocol, together with the deprecation and
security-exception process.

Read [STABILITY.md](STABILITY.md) for the policy and
[COMPATIBILITY.md](COMPATIBILITY.md) for the exact Portwing, Drydock, and
sockguard versions tested together.

<hr>

<h2 align="center" id="competitive-landscape">Competitive Landscape</h2>

The published [competitive landscape](https://portwing.codeswhat.com/docs/competitive-landscape) compares
Portwing with Portainer Agent, Komodo Periphery, Arcane Agent, Hawser, Docker's
native remote-access options, and adjacent agents. It records primary sources,
feature ownership across Portwing/Drydock/Sockguard, pre-v1 gates, candidate
work, and explicit non-goals. Unknown competitor behavior is recorded as “not
documented,” not guessed as absent.

<hr>

<h2 align="center" id="token-security">Token Security</h2>

<details>
<summary><strong>Plaintext, file-based, and hash-at-rest token options</strong></summary>

### Plaintext token (testing only)

> **Warning:** Environment variables are visible in `docker inspect` and
> process listings. For production, use `TOKEN_FILE` or `TOKEN_HASH_FILE`
> with a mounted secret.

```bash
# Generate a strong token
TOKEN=$(openssl rand -hex 32)
docker run -e TOKEN="$TOKEN" ... ghcr.io/codeswhat/portwing:0.9.17
```

### File-based token (production)

```bash
TOKEN=$(openssl rand -hex 32)
printf '%s' "$TOKEN" > /run/secrets/portwing-token
chown 65532:65532 /run/secrets/portwing-token && chmod 0400 /run/secrets/portwing-token
docker run -e TOKEN_FILE=/run/secrets/portwing-token \
  -v /run/secrets/portwing-token:/run/secrets/portwing-token:ro \
  ... ghcr.io/codeswhat/portwing:0.9.17
```

### Hash-at-rest with TOKEN_HASH

Store only an Argon2id hash so the plaintext token never appears in env dumps
or config files:

```bash
# Generate the hash (token is read from stdin, never argv)
HASH=$(printf '%s' "$TOKEN" | portwing hash-token)
# $argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>

# Use the hash instead of the plaintext
docker run -e TOKEN_HASH="$HASH" ... ghcr.io/codeswhat/portwing:0.9.17
```

Or write the hash to a file and use `TOKEN_HASH_FILE`:

```bash
printf '%s' "$TOKEN" | portwing hash-token > /run/secrets/portwing-token-hash
docker run -e TOKEN_HASH_FILE=/run/secrets/portwing-token-hash ...
```

</details>

<hr>

<h2 align="center" id="verify-a-release">Verify a Release</h2>

<details>
<summary><strong>Cosign verification for checksums and container images</strong></summary>

Portwing releases are signed with [Sigstore cosign](https://github.com/sigstore/cosign)
via GitHub Actions keyless signing. Checksums and container images can be
verified without managing signing keys.

### Verify the checksums file

```bash
VERSION=0.9.17

cosign verify-blob \
  --certificate-identity "https://github.com/CodesWhat/portwing/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  --bundle "checksums.txt.bundle" \
  "checksums.txt"
```

### Verify the container image

```bash
VERSION=0.9.17

cosign verify \
  --certificate-identity "https://github.com/CodesWhat/portwing/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "ghcr.io/codeswhat/portwing:${VERSION}"
```

### SBOM

Each binary archive has a matching CycloneDX release asset, for example
`portwing_0.9.17_linux_amd64.tar.gz.cyclonedx.json`. The SBOM has no standalone
cosign bundle; verify the signed `checksums.txt`, then verify the SBOM's digest
against that manifest. Public releases also give every checksummed asset its
own GitHub build-provenance attestation. The container image carries a separate
OCI SBOM attestation.

</details>

<hr>

<h2 align="center" id="security">Security</h2>

<details>
<summary><strong>Security model summary</strong></summary>

- **Authentication**: Fail-closed startup; raw tokens stored as fixed-size SHA-256 digests and compared timing-safely; hash-at-rest via `TOKEN_HASH` (Argon2id); Ed25519 per-client keypairs with versioned full-request-target signatures and replay protection
- **Rate Limiting**: 10 failed auth attempts per IP per minute, plus a two-request cap on concurrent cold Argon2id derivations
- **TLS**: TLS 1.2+ with modern AEAD cipher suites
- **Compose Security**: Root-confined, symlink-resistant file writes; path traversal protection; env var denylist; service name injection prevention
- **Resource Limits**: WebSocket (16 MB), response body (100 MB), exec sessions (100 concurrent)

See [docs/security-model.md](docs/security-model.md) for the full citable spec and CVE mapping.

</details>

<hr>

<h2 align="center" id="audit-logging">Audit Logging</h2>

<details>
<summary><strong>Structured JSON audit trail for every security-relevant action</strong></summary>

Portwing ships structured JSON audit logging for every security-relevant action — a feature that commercial container management platforms lock behind paid tiers.

### Enable

```bash
# Write to a file (opened append-only, mode 0600)
docker run -e AUDIT_LOG=/var/log/portwing-audit.log ...

# Or to stdout/stderr (useful with log aggregators)
docker run -e AUDIT_LOG=stdout ...
```

The persistent file/stdout/stderr sink is disabled by default (`AUDIT_LOG`
unset). Independently, `AUDIT_BUFFER_SIZE` defaults to 256 and keeps recent
records in an in-memory ring; set it to `0` to disable retention. The ring feeds
`GET /_portwing/audit` and cursor-based NDJSON export at
`GET /_portwing/audit/export`. Standard mode authenticates both paths. Edge
mode exposes only the NDJSON export, without authentication, on its private
operations listener; never publish that listener to an untrusted network. A
409 response detects
overwritten history or a restarted cursor generation instead of silently
dropping records. The sink and export schemas are stable from v0.8.0.

### Events

| `event` | Triggered when |
|---------|---------------|
| `api_request` | Any authenticated API call completes |
| `auth_failure` | An invalid token is presented |
| `rate_limited` | An IP is blocked by the rate limiter |
| `compose_op` | A Docker Compose operation runs |
| `exec_start` | An interactive exec tunnel opens |
| `enrollment` | An Ed25519 key is enrolled via `/api/portwing/enroll` |

### Sample JSON lines

```json
{"time":"2026-01-15T10:23:45.123456789Z","level":"INFO","msg":"","event":"api_request","actor":"203.0.113.42","method":"POST","path":"/_portwing/compose","outcome":"allowed","status":200,"duration_ms":3.14}
```

Compose operations include additional fields:

```json
{"time":"2026-01-15T10:23:45.200Z","level":"INFO","msg":"","event":"compose_op","actor":"203.0.113.42","operation":"up","stack":"nginx-stack","outcome":"allowed"}
```

Exec tunnel events:

```json
{"time":"2026-01-15T10:24:01.500Z","level":"INFO","msg":"","event":"exec_start","actor":"203.0.113.42","container":"abc123def456","exec_id":"e7f8a9b1","outcome":"allowed"}
```

</details>

<hr>

<a id="star-history"></a>

Growth over time:

<div align="center">
  <a href="https://github.com/CodesWhat/portwing/stargazers">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="docs/assets/star-history-dark.svg" />
      <img alt="Star history for CodesWhat/portwing" src="docs/assets/star-history.svg" width="900" />
    </picture>
  </a>
</div>

<sub>Generated from GitHub's own stargazer timestamps and committed to the repo, so rendering it costs no third-party request. Refreshed at each release rather than on a schedule, so it never changes underneath a tag that already points at it; each chart ships with the following release. See `.github/workflows/starchart.yml`.</sub>

---

<div align="center">

[![SemVer](https://img.shields.io/badge/semver-2.0.0-blue)](https://semver.org/)
[![Conventional Commits](https://img.shields.io/badge/commits-conventional-fe5196?logo=conventionalcommits&logoColor=fff)](https://www.conventionalcommits.org/)
[![Keep a Changelog](https://img.shields.io/badge/changelog-Keep%20a%20Changelog-E05735)](https://keepachangelog.com/)

### Built With

[![Go 1.27](https://img.shields.io/badge/Go_1.27-00ADD8?logo=go&logoColor=fff)](https://go.dev/)
[![gorilla/websocket](https://img.shields.io/badge/gorilla%2Fwebsocket-00ADD8?logo=go&logoColor=fff)](https://github.com/gorilla/websocket)
[![google/uuid](https://img.shields.io/badge/google%2Fuuid-00ADD8?logo=go&logoColor=fff)](https://github.com/google/uuid)
[![golang.org/x/crypto](https://img.shields.io/badge/x%2Fcrypto-00ADD8?logo=go&logoColor=fff)](https://pkg.go.dev/golang.org/x/crypto)
[![Sigstore](https://img.shields.io/badge/Sigstore-FFC107?logo=sigstore&logoColor=000)](https://www.sigstore.dev/)
[![Wolfi](https://img.shields.io/badge/Wolfi-4A4A55?logo=chainguard&logoColor=fff)](https://edu.chainguard.dev/open-source/wolfi/overview/)
[![Docker](https://img.shields.io/badge/Docker-2496ED?logo=docker&logoColor=fff)](https://www.docker.com/)
[![GoReleaser](https://img.shields.io/badge/GoReleaser-00ADD8?logo=go&logoColor=fff)](https://goreleaser.com/)

### Community & Support

Non-security bugs and concrete feature requests go to [GitHub Issues](https://github.com/CodesWhat/portwing/issues); open-ended questions, ideas, and design discussion go to [GitHub Discussions](https://github.com/CodesWhat/portwing/discussions); real-time chat and early support are on the [CodesWhat Discord](https://discord.gg/mWHCPJRzSx). **Vulnerabilities must not be filed as public issues** — see [SECURITY.md](SECURITY.md) for private disclosure.

Issues, ideas, and pull requests are welcome — start with [CONTRIBUTING.md](CONTRIBUTING.md), and use [SECURITY.md](SECURITY.md) for private vulnerability disclosure.

Every release image is cosign-signed via GitHub Actions OIDC. Before running a Portwing image in production, verify it with the canonical invocation in the [Verify a Release](#verify-a-release) section above.

**[AGPL-3.0 License](LICENSE)**

Built by <a href="https://codeswhat.com">CodesWhat</a>

[![Ko-fi](https://img.shields.io/badge/Ko--fi-Support-ff5e5b?logo=kofi&logoColor=white)](https://ko-fi.com/codeswhat)
[![Buy Me a Coffee](https://img.shields.io/badge/Buy%20Me%20a%20Coffee-ffdd00?logo=buymeacoffee&logoColor=black)](https://buymeacoffee.com/codeswhat)
[![Sponsor](https://img.shields.io/badge/Sponsor-ea4aaa?logo=githubsponsors&logoColor=white)](https://github.com/sponsors/CodesWhat)

<a href="#portwing">Back to top</a>

</div>
