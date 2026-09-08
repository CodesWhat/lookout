# Portwing Security Model

This document is the citable specification for Portwing's security controls.
Each control is named and described precisely so downstream tools, auditors,
and compliance checks can reference it by name.

The controls below are numbered to serve as a stable citation surface
(Control *N*) and describe what the agent enforces in its own code. The
documentation site's security model page presents an expanded, independently
numbered set that additionally covers the Sockguard socket filter, the hardened
container runtime (read-only rootfs, dropped capabilities, `no-new-privileges`),
structured audit logging with external immutable export, and signed/verifiable releases — the two numbering
schemes are kept independent so these code-level control numbers don't shift.

## Controls

### 1. Proxy-First Architecture

Every byte that would reach the Docker socket passes through Portwing's HTTP
server, which applies authentication before forwarding. There is no path by
which an unauthenticated caller can reach the Docker daemon directly. The
catch-all mux entry that implements the Docker API proxy is registered last
and is wrapped with the same auth middleware as every named route:

```go
mux.Handle("/", auth(s.handleDockerProxy))
```

This means a missing or misconfigured auth middleware would cause a build-time
or startup-time failure rather than a silent security gap.

Standard mode also fails closed at construction time when no raw token,
Argon2id token hash, or authorized-keys registry is configured. Local
unauthenticated development requires `ALLOW_UNAUTHENTICATED=true`; a
non-loopback bind additionally requires the independent
`ALLOW_UNAUTHENTICATED_REMOTE=true` acknowledgement.

### 2. Timing-Safe Token Comparison

Raw token validation stores only a fixed-size SHA-256 digest in the verifier.
Each presented value is hashed and compared with
`crypto/subtle.ConstantTimeCompare` from the Go standard library. The
comparison therefore has fixed length regardless of the submitted token
length and the long-lived verifier does not retain the raw configured token.

Applies to all accepted credential headers: `Authorization: Bearer`,
`X-Portwing-Token`, and `X-Dd-Agent-Secret`.

### 3. Argon2id Token-at-Rest (TOKEN_HASH)

Portwing supports storing the token as an Argon2id hash rather than in
plaintext. Three mechanisms are available (evaluated in order):

| Mechanism | Environment variable | Description |
|-----------|----------------------|-------------|
| Plaintext | `TOKEN` / `DD_AGENT_SECRET` | Token value directly; read from env |
| Plaintext file | `TOKEN_FILE` / `DD_AGENT_SECRET_FILE` | Path to a file containing the token |
| Hash | `TOKEN_HASH` / `TOKEN_HASH_FILE` | Argon2id PHC string (see below) |

The `portwing hash-token` CLI subcommand generates a suitable hash using
OWASP-recommended parameters (m=19456 KiB, t=2, p=1). The token is read from
stdin so it never appears in shell history or process listings:

```bash
printf '%s' "$TOKEN" | portwing hash-token
# $argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
```

Set the output as `TOKEN_HASH` (or write it to a file referenced by
`TOKEN_HASH_FILE`). At runtime Portwing compares the incoming token against
the stored hash using Argon2id verification, still wrapped with
`crypto/subtle` for the final byte comparison to prevent timing leakage.

This means the plaintext token is never stored on disk; a compromised
environment variable dump or configuration file reveals only the hash.
Cold Argon2id work is bounded to two concurrent derivations across the agent.
Additional cold requests receive HTTP 429 before allocating Argon2 memory.
After the first successful verification, the verifier caches only a fixed-size
SHA-256 digest and uses the timing-safe fast path.

### 4. Compose Path-Traversal Guard

All file paths in `ComposeRequest` are first checked lexically to remain within
`STACKS_DIR`. Uploaded Compose files and `.env.drydock` are then written
through Go's directory-scoped `os.Root` API. Every directory component and the
destination are checked without following symlinks, and the rooted open keeps
the operation confined even if a path is swapped concurrently.

```go
if !strings.HasPrefix(resolved, absBase+string(filepath.Separator)) && resolved != absBase {
    return "", fmt.Errorf("path %q escapes stacks directory", path)
}
```

A caller supplying `../../etc/passwd`, a symlinked directory, or a symlinked
destination receives a validation error; uploaded data is never written
outside the stacks directory.

### 5. Compose Env Denylist

Environment variables passed in `ComposeRequest.EnvVars` are validated on two
axes before being written to `.env.drydock`:

1. **Key format:** Must match `^[a-zA-Z_][a-zA-Z0-9_]*$`.
2. **Denylist:** The following keys are rejected unconditionally:
   `LD_PRELOAD`, `LD_LIBRARY_PATH`, `PATH`, `DOCKER_HOST`, `DOCKER_CONFIG`,
   `DOCKER_CERT_PATH`, `DOCKER_TLS_VERIFY`, `DOCKER_CONTEXT`, `HOME`,
   `SHELL`, `BASH_ENV`, `ENV`, `CDPATH`, `IFS`.

The denylist covers dynamic linker hijacking (`LD_*`), shell init-file
injection (`BASH_ENV`, `ENV`), and Docker socket redirection (`DOCKER_HOST`,
`DOCKER_CONTEXT`).

### 6. Compose Flag-Injection Filter

Service names in `ComposeRequest.Services` are validated to not start with
`-`. Docker Compose passes service names as positional arguments; a name like
`--privileged` would be interpreted as a flag by the subprocess shell,
potentially escalating privileges or altering compose behaviour.

```go
if strings.HasPrefix(svc, "-") {
    return fmt.Errorf("invalid service name: %q", svc)
}
```

### 7. Rate Limiting

Failed authentication attempts are tracked per remote IP in a sliding
one-minute window:

- **Threshold:** 10 failures within 60 seconds triggers a block.
- **Response:** HTTP 429 (`Too Many Requests`) until the window expires.
- **Connection reuse:** auth admission rejections before body reads close the
  HTTP/1 connection and expire its read deadline. This is the permanent
  trade-off: a legitimate client reconnects after rejection, while a client
  that declares a body and never sends it cannot hold the handler in
  `net/http`'s post-response drain. Keep-alive is not preserved on this path.
- **Memory cap:** At most 10,000 IP entries are tracked simultaneously;
  new entries beyond the cap are silently dropped to prevent memory
  exhaustion (fail-open for tracking, not for auth).
- **Cleanup:** A background goroutine prunes expired entries every 5 minutes.
- **Expensive-verification capacity:** no more than two authentication
  attempts per IP and two cold Argon2id derivations agent-wide may be in
  flight; excess work receives HTTP 429 before credential verification.
- **Enrollment abuse accounting:** malformed and oversized enrollment bodies
  use a separate per-IP failure window, so they cannot evade throttling while
  remaining independent of credential-failure accounting.
- **Enrollment admission:** request bodies have a 10-second read deadline, with
  at most two active enrollment handlers per client and 32 across the agent.
  Excess requests receive HTTP 429 before their bodies are read.

### 8. TLS 1.2+ AEAD-Only

`TLS_CERT` and `TLS_KEY` must either both be configured or both be empty;
partial TLS configuration is a startup error. When configured, Portwing
presents TLS with:

- **Minimum version:** TLS 1.2 (`tls.VersionTLS12`)
- **Cipher suites (TLS 1.2):** ECDHE-ECDSA-AES256-GCM-SHA384,
  ECDHE-RSA-AES256-GCM-SHA384, ECDHE-ECDSA-AES128-GCM-SHA256,
  ECDHE-RSA-AES128-GCM-SHA256, ECDHE-ECDSA-CHACHA20-POLY1305,
  ECDHE-RSA-CHACHA20-POLY1305. All are AEAD modes; CBC and RC4 suites are
  not in the list.
- **Curves:** X25519, P-256.

TLS 1.3 cipher suites are controlled by Go's TLS stack, which selects only
AEAD suites by design.

### 9. Resource Caps

| Resource | Limit | Purpose |
|----------|-------|---------|
| WebSocket read | 16 MB | Prevent memory exhaustion from large WebSocket frames |
| HTTP response body read | 100 MB | Bound buffered Docker API responses |
| Legacy edge log response | 100 MB | Bound a buffered `dd:container_log_response` for older controllers |
| Edge log-stream frame | 256 KiB | Skip an oversized Docker log frame before it enters the controller queue |
| Concurrent edge log streams | 128 | Bound Docker readers and per-stream goroutines independently of message handlers |
| Edge outbound queue | 256 frames | Evict and reconnect a stalled controller instead of growing memory without limit |
| Legacy edge follow-log window | ~7s | Bound a non-streaming `follow=true` request so it can't hold a message-handler slot indefinitely |
| Exec request body | 10 MB | Limit exec payload size |
| Ed25519 signed-request body | 1 MB | Bound the request body buffered for signature verification |
| Enrollment request body | 64 KiB | Bound unauthenticated JSON parsing before enrollment authentication |
| Enrollment body read | 10 seconds | Stop unauthenticated slow-body handlers |
| Concurrent enrollment handlers | 32 agent-wide, 2 per client | Bound unauthenticated handlers before body parsing |
| Cold Argon2id derivations | 2 | Bound memory-hard authentication work across the agent |
| Concurrent exec sessions | 100 | Prevent unbounded goroutine growth |
| Concurrent stream sessions | 100 | Prevent unbounded goroutine growth |
| Rate-limiter IP table | 10,000 entries | Prevent rate-limiter map exhaustion |

### 10. Panic Recovery

The outermost HTTP handler is wrapped with `RecoveryMiddleware`, which uses
`defer/recover` to catch any panics in downstream handlers. On recovery it
logs the full stack trace at ERROR level and returns HTTP 500. This prevents
a single malformed request from crashing the agent process and taking the
Docker proxy offline.

### 11. Ed25519 Per-Client Key Authentication

When `AUTHORIZED_KEYS` is configured, Portwing authenticates requests using
per-client Ed25519 keypairs instead of (or alongside) the shared token. Full
design rationale is in `docs/design/ed25519-auth.md`.

**Key registry:** Operator writes an `authorized_keys`-style file listing
trusted 32-byte Ed25519 public keys (one per line, `ed25519 <base64> [comment]`
format). The agent loads this file at startup and on `SIGHUP`. Hot reload adds
or removes keys from the in-memory map without restarting.

**Per-request signature:** Signature version 2 requests carry five
headers (`X-Portwing-Key-ID`, `X-Portwing-Timestamp`, `X-Portwing-Nonce`,
`X-Portwing-Signature`, and `X-Portwing-Signature-Version: 2`). The agent
verifies the Ed25519 signature over
`METHOD\nREQUEST-TARGET\nbody-sha256-hex\ntimestamp\nnonce`, where the request
target is the escaped path plus the exact raw query string. Unversioned legacy
signatures are accepted only for query-free requests. No new dependencies.

**Replay protection:** Two complementary mechanisms:

1. **Timestamp window:** Requests with `|now - timestamp| > MAX_CLOCK_SKEW_SECONDS`
   (default 60 s) are rejected with `X-Portwing-Reason: timestamp-skew`.
2. **Nonce LRU:** An in-memory nonce cache (capacity `NONCE_LRU_SIZE`, default
   10,000 entries) tracks nonces within the timestamp window. Repeated nonces
   return `X-Portwing-Reason: replay`. A full cache with no expired entries
   returns HTTP 401 with `X-Portwing-Reason: nonce-capacity`; it never accepts
   a request without recording its nonce. The LRU is preserved across SIGHUP reloads.

**Verification order in middleware:** If `X-Portwing-Signature` is present, Ed25519
verification runs; if absent, the request falls through to the existing token
verifier. Both auth methods can coexist during migration.

**File security:** Authorized-keys and private-key paths must resolve to regular
files. On Unix, the agent rejects world-readable or group/world-writable
credential files while permitting group-readable mode `0640`. Validation uses
the already-open file descriptor, eliminating the stat-then-open race while
remaining compatible with symlinked secret mounts.

**Edge-mode signed hello:** Edge (WebSocket) mode requires `PRIVATE_KEY_FILE` —
configuration load fails fast when `DRYDOCK_URL` is set without it, because the
Drydock `/api/portwing/ws` endpoint is Ed25519-only and rejects token-hash
hellos. The agent signs the WebSocket hello with its Ed25519 private key,
embedding `pubKeyId`/`timestamp`/`nonce`/`signature` fields, which the
controller verifies before sending `welcome`. A `tokenHash` hello path still
exists in the code for non-edge use, but the config gate makes it unreachable
for a running edge agent.

**Model C enrollment (optional):** When `ENROLLMENT_TOKEN` is set alongside
`AUTHORIZED_KEYS`, the agent exposes `POST /api/portwing/enroll` (outside the
auth middleware, rate-limited). The configured enrollment token is retained
and compared only as a fixed-size SHA-256 digest. Bodies are capped at 64 KiB
and must contain a single JSON value. A caller presents the token and a public key;
the agent appends the key to the authorized_keys file, reloads the registry,
and burns the token (refusing further enrollment until restart).

See `docs/design/ed25519-auth.md` for full threat analysis, key rotation
procedures, and migration path from token auth.

### 12. Non-Root Runtime Identity

The published image runs as the dedicated `portwing` user (UID 65532) — the
final stage sets `USER 65532:65532`, and `/data/stacks` plus the user's home
directory are pre-owned by that UID so read-only-rootfs deployments work
without a writable layer. `DOCKER_CONFIG` points at the `/tmp` tmpfs so
`docker login` during Compose deploys works under `read_only: true`.

Because the agent must open the host's Docker socket (`root:docker 0660` on
typical hosts), deployments grant the socket's group explicitly: `group_add`
in Compose, `--group-add $(stat -c '%g' /var/run/docker.sock)` with
`docker run`, or `supplementalGroups` in Kubernetes. Mounted credential files
must be readable by UID 65532 (`chown 65532:65532` + `chmod 0400` on the host
file). `user: "0:0"` restores the old root-in-container behavior where an
environment requires it.

Root-vs-non-root is not the load-bearing boundary for a Docker-socket agent —
any process that reaches the socket can pivot to the host through the daemon —
but the non-root default removes root-owned files on shared volumes, blocks
accidental privileged access through other mounts, and satisfies
`runAsNonRoot` admission policies without overrides.

---

## CVE Mapping

The table below maps classes of publicly known vulnerabilities to the Portwing
controls that structurally prevent them. "Structurally prevents" means the
architecture makes exploitation impossible regardless of token or config
values, not merely that a patch has been applied.

Only CVEs verified against public advisories at the time of writing are cited.
CVE identifiers that could not be independently verified have been omitted.

| CVE / Advisory | Vulnerability Class | Portwing Control | Architectural Reason |
|----------------|---------------------|-----------------|----------------------|
| **CVE-2024-41110** ([GHSA-v23v-6jw2-98fq](https://github.com/moby/moby/security/advisories/GHSA-v23v-6jw2-98fq)) | Docker AuthZ plugin bypass via zero-length `Content-Length` body. A crafted request with `Content-Length: 0` causes the daemon to forward the request to the AuthZ plugin without a body; plugins that default-deny on missing body are bypassed. CVSS 9.9. | Control 1 — Proxy-First Architecture | Portwing does not use Docker's AuthZ plugin mechanism at all. Authentication is implemented at the Portwing HTTP layer (controls 1 + 2), before the request is ever forwarded to the Docker socket. There is no AuthZ plugin to bypass. |
| **Docker AuthZ plugin bypass via oversized body** | Class of bypass where oversized (>1 MB) request bodies are silently dropped before the AuthZ plugin sees them while the daemon still executes the original request — an incomplete fix for the zero-length-body class above. | Control 1 — Proxy-First Architecture | Same reason as CVE-2024-41110: Portwing authenticates at the HTTP layer, not via Docker AuthZ plugins. The body-stripping behaviour in Docker's plugin dispatch path is irrelevant because Portwing's auth decision is made and enforced before the request reaches that path. |
| **Token-attaching proxy forwards before authenticating** | Class where a proxy/agent middleware attaches its auth token and forwards a request to a remote Docker agent *before* verifying the original caller is authenticated, letting unauthenticated callers reach remote agents. | Control 1 — Proxy-First Architecture | Portwing's auth middleware is applied to every route including the catch-all proxy *before* any forwarding occurs (see `registerRoutes` in `internal/server/http.go`). The middleware cannot be bypassed by ordering — auth wraps the handler, not the other way around. |
| **Privileged API paths missing from a per-path authorization map** | Class where management endpoints (e.g. Docker plugin install/enable under `/plugins/*`) are absent from a proxy's authorization handler map, letting unauthorized users reach them. | Control 1 — Proxy-First Architecture | Portwing's catch-all handler applies auth uniformly to *all* paths, including `/plugins/*`. There is no per-path allow-list to misconfigure; omitting a path from the allow-list is not possible because the default is to authenticate everything. |
| **Security settings not enforced on an alternate API surface** | Class where endpoint security settings (privileged mode, host PID, device mapping, …) are enforced on one API path but not on an alternate surface such as Swarm service create/update. | Control 1 — Proxy-First Architecture | Portwing does not implement its own container security policy; it relies on the Docker daemon's own access controls and the operator's token-based access model. All Docker API paths are proxied with the same auth enforcement — there are no Swarm-specific code paths with weakened controls. |
| **Partial-enforcement bind-mount bypass** | Class where a "disable bind mounts for non-administrators" control checks only `HostConfig.Binds` and not `HostConfig.Mounts`, so the restriction is bypassable via the unchecked field. | Control 1 — Proxy-First Architecture | Portwing does not implement container creation policies; it proxies Docker API requests to authenticated callers. Access control is binary (token present and valid = full Docker API access). Portwing is therefore not a policy enforcement layer and does not reproduce this class of partial-enforcement flaw. |
| **GHSA-7vx4-hf96-mqq6** (Dockge console injection, High, published 2025-03-31 — [GitHub Advisory](https://github.com/louislam/dockge/security/advisories)) | Terminal/console output injection in Dockge's web UI, likely via unsanitised container log or exec output rendered in the browser. | Not applicable (different attack surface) | Portwing does not have a web UI or a browser-rendered console. Log and exec output are streamed as raw bytes to authenticated API callers. Browser-side rendering is the responsibility of Drydock, not Portwing. |
| **CVE-2025-64419 / CVE-2025-66209–66213** ([Coolify, Aikido](https://www.aikido.dev/blog/ai-pentesting-coolify-cves)) | Coolify: command injection via docker-compose.yaml content and other inputs allowing root RCE on the host. CVSS up to 10.0. | Controls 4, 5, 6 — Compose Guards | Portwing validates all compose inputs before execution: path traversal protection (control 4), env var key/denylist validation (control 5), service name flag-injection filter (control 6). Compose file content written via `files` map is stored as-is and executed by the Docker Compose binary — operators should treat this with the same trust level as direct compose file access. |

### Notes on Cited CVEs

- **Coolify CVEs beyond the batch above:** The Coolify disclosure referenced
  multiple additional CVEs; only the representative root-RCE class is cited here
  as it is the directly analogous attack surface.

### Scope Caveat

Portwing is a privileged agent: an authenticated caller has full Docker API
access. The controls above prevent *unauthenticated or improperly authorized*
access and certain classes of *injection* attacks against the Compose path.
They do not limit what an authenticated caller can do with the Docker API.
Operators should treat the Portwing token as a root-equivalent credential.

### Container Env Vars Are Not Redacted on `/api/containers`

`GET /api/containers` and the container inventory synced to Drydock over the
edge WebSocket (`dd:container_sync` / `dd:container_added` / `_updated`)
include each container's environment variables as plaintext key/value pairs
(`RuntimeDetails.Env` in `internal/adapter/containers.go`). This is a
deliberate design decision — Portwing does not redact or filter env values on
this surface; that responsibility belongs to Drydock (or another downstream
consumer) if redaction before display or storage is required. This is
distinct from the MCP `inspect_container` tool, which reports only an env-var
*count* and never the values (see the [README](../README.md#mcp--ai-assistant-integration)).
Any authenticated caller of `/api/containers`, and any system that receives
the edge sync, sees full env var values — this is most relevant to
standalone or no-Sockguard deployments where the caller/consumer trust
boundary is wider than "operator only."
