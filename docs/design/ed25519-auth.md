# Ed25519 Per-Client Key Authentication — Design

**Status:** Implemented (Phase 1)  
**Author:** Wave 2 design team  
**Branch:** `feat/ed25519-design`  
**Date:** 2026-06-11  
**Related:** `docs/security-model.md`, `internal/server/middleware.go`

---

## 1. Goals and Non-Goals

### Goals

- Replace the shared `TOKEN`/`TOKEN_HASH` secret with per-client Ed25519 keypairs for both standard (HTTP) mode and edge (WebSocket) mode.
- Support multiple registered keys simultaneously (multi-platform, multi-operator).
- Provide replay protection: a captured valid request cannot be reused.
- Add zero new heavy dependencies — `crypto/ed25519`, `crypto/sha256`, and `encoding/hex` are all Go standard library.
- Keep `TOKEN`/`TOKEN_HASH` working as a fallback/legacy path during migration.
- Preserve the existing `tokenVerifier` interface so the middleware stack does not need structural change.
- Enable future mTLS layering: the key registry proposed here is compatible with using the same public key as a TLS client certificate CN.

### Non-Goals

- Full Noise XX / mutual TLS protocol at the transport layer (that is the next phase, noted in §5).
- Key-exchange negotiation at the TLS level in this phase.
- User-level (multi-tenant) RBAC on specific Docker API paths — auth remains binary.
- OCSP or X.509 certificate issuance.

---

## 2. Enrollment: Getting a Client's Public Key Registered

Three enrollment models are considered. A recommendation follows the threat analysis.

### 2.1 Model A — First-Claim TOFU (Trust On First Use)

> **Not implemented.** Model A is described here for design completeness only. It is not implemented in Portwing. The env vars referenced below (`TOFU_ENROLLMENT`, `ENROLLMENT_TIMEOUT`, `PORTWING_DEV_TOFU`) are hypothetical and have no effect on the running agent. See the recommendation at the end of this section.

The agent exposes a one-time enrollment endpoint. The first caller presents an Ed25519 public key; the agent accepts and persists it. Subsequent calls to the enrollment endpoint are rejected.

**Flow (hypothetical):**

1. Operator starts Portwing with `TOFU_ENROLLMENT=1` (hypothetical) and `ENROLLMENT_TIMEOUT=300` (hypothetical, seconds).
2. Within the timeout window, the caller `POST /api/portwing/enroll` with `{"public_key": "<base64url ed25519 pubkey>"}`.
3. Agent writes the key to the authorized-keys file and disables the enrollment endpoint.
4. All subsequent requests must carry an Ed25519 signature.

**Threat analysis:**

| Threat | Severity | Mitigation |
|--------|----------|------------|
| Race: attacker beats legitimate caller | High | Enrollment timeout + network-level firewall; TOFU window should be seconds, not hours |
| Operator forgets to enroll; window expires | Low | Restart with `TOFU_ENROLLMENT=1` again |
| Agent on public IP during enrollment | High | Operator must firewall the agent during enrollment; no protection if skipped |
| TOFU accepted over plaintext HTTP | Critical | TLS must be enforced when TOFU is enabled; agent should refuse enrollment on non-TLS |

**Verdict:** Viable for homelab deployments where the operator controls network access during enrollment. Not suitable for unattended deployments or public-IP agents without strict operational controls.

### 2.2 Model B — Operator-Provisioned AUTHORIZED_KEYS File

The operator writes an `authorized_keys`-style file listing trusted public keys before the agent starts. No enrollment API is needed.

**Flow:**

1. Operator generates an Ed25519 keypair on the platform side: `openssl genpkey -algorithm ed25519`.
2. Operator writes the public key (plus optional comment and permissions) into the authorized-keys file on the agent host.
3. Agent loads the file at startup (and on `SIGHUP`).
4. The platform signs every request with the private key.

**Threat analysis:**

| Threat | Severity | Mitigation |
|--------|----------|------------|
| File permissions too broad or path swapped | Medium | Agent requires a regular file, rejects world-readable and group/world-writable Unix modes, and validates the opened descriptor |
| Operator adds wrong/attacker key | Medium | Operational; no software mitigation beyond clear documentation |
| No enrollment API = no attack surface | — | Positive: there is nothing to race against |
| Key rotation requires file edit + SIGHUP | Low | Acceptable for operations-managed agents |

**Verdict:** Highest security baseline. Zero attack surface during enrollment. Recommended for production and edge-mode agents.

### 2.3 Model C — Enrollment Token (Bootstrap Token TOFU)

The agent is pre-configured with a short-lived `ENROLLMENT_TOKEN` (a random secret). A caller presents the token once and in exchange registers their public key. The token is burned after first use.

**Flow:**

1. Operator generates a random enrollment token and sets `ENROLLMENT_TOKEN=<hex>` on the agent.
2. Platform calls `POST /api/portwing/enroll` with a single JSON value no larger than 64 KiB: `{"enrollment_token": "<hex>", "public_key": "<standard-base64>"}`.
3. Agent verifies the token (constant-time), appends the public key to the authorized-keys file, clears the token from memory, and acknowledges.
4. The agent refuses further enrollment calls until restarted with a new token.

**Threat analysis:**

| Threat | Severity | Mitigation |
|--------|----------|------------|
| Token captured in transit | High | TLS required; refuse enrollment over plain HTTP |
| Token leaked via env var dump | Medium | Support `ENROLLMENT_TOKEN_FILE`; treat like `TOKEN_FILE` |
| Brute-force enrollment token | Low | Token should be ≥128 bits random; rate-limit `/enroll` endpoint |
| Platform loses private key post-enrollment | Medium | Must re-enroll; provide revocation path (see §4) |

**Verdict:** Good UX for automated deployments (CI, Kubernetes). Requires TLS at enrollment time.

### Recommendation

**Implement Model B (authorized_keys file) as the primary path**, with Model C (enrollment token) as an optional convenience for automated/cloud deployments.

Model A (bare TOFU) is explicitly *not recommended* for production use and is **not implemented**. It is described here for completeness only; the hypothetical `PORTWING_DEV_TOFU=1` gate does not exist in the codebase.

---

## 3. Request Authentication

### 3.1 Standard Mode (HTTP API)

Every version 2 request from an authenticated client carries five headers
that together constitute a detached Ed25519 signature.

#### Signed content

The client signs the following canonical byte string (UTF-8, no trailing newline):

```text
<METHOD>\n
<REQUEST-TARGET>\n
<SHA-256 of request body, hex-encoded, or the full 64-char SHA-256 of the empty string "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" for empty body>\n
<Unix timestamp, seconds, decimal>\n
<Nonce, 32 hex bytes>
```

Example for `POST /api/portwing/containers` with a JSON body:

```text
POST
/api/portwing/containers
9f86d081884c7d659a2feaa0c55ad015...
1749600000
a1b2c3d4e5f6...
```

`REQUEST-TARGET` is the escaped path plus the exact raw query string in
origin-form. The signed content deliberately does not include the `Host`
header — the agent is typically behind a reverse proxy and the Host value seen
by the agent may differ from the value the client intended.

#### Headers

| Header | Format | Example |
|--------|--------|---------|
| `X-Portwing-Key-ID` | hex-encoded first 8 bytes of SHA-256(public_key) | `4a3f2b1c9d8e7f6a` |
| `X-Portwing-Timestamp` | Unix epoch, seconds, decimal string | `1749600000` |
| `X-Portwing-Nonce` | 32 random hex bytes (128-bit) | `a1b2c3d4e5f6...` |
| `X-Portwing-Signature` | base64url (no padding) Ed25519 signature over canonical string | `MEQCIBe...` |
| `X-Portwing-Signature-Version` | canonical format version | `2` |

The existing `Authorization: Bearer` and `X-Portwing-Token` headers continue to work as the legacy path.

#### Verification steps (agent side)

1. If `X-Portwing-Signature` is absent, fall through to legacy token verification.
2. Look up the key by `X-Portwing-Key-ID`. If not found, return 401.
3. Parse `X-Portwing-Timestamp`. If `|now - timestamp| > 60s`, return 401 with `X-Portwing-Reason: timestamp-skew`.
4. Check the nonce LRU (see §3.3). If seen, return 401 with `X-Portwing-Reason: replay`.
5. Reconstruct the canonical byte string from the request (method, escaped path plus exact raw query, SHA-256 of body, timestamp, nonce).
6. Verify the Ed25519 signature against the registered public key. `crypto/ed25519.Verify` returns a bool — no constant-time concern here since the signature algorithm itself is not timing-sensitive to verify.
7. Record the nonce in the LRU. Advance the rate limiter success counter.

#### Wire example

```http
POST /api/portwing/containers/myapp/start HTTP/1.1
Host: agent.example.internal:3000
Content-Type: application/json
Content-Length: 2
X-Portwing-Key-ID: 4a3f2b1c9d8e7f6a
X-Portwing-Timestamp: 1749600000
X-Portwing-Nonce: a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6
X-Portwing-Signature: d2hhdCBhIGRheSB0byBiZSBhbGl2ZQ
X-Portwing-Signature-Version: 2

{}
```

### 3.2 Edge Mode (WebSocket)

The edge client (runs inside Docker, initiates an outbound WebSocket to the controller/platform) authenticates in two phases.

#### Phase 1: Signed Hello

The `HelloMessage` gains an optional `pubKeyId`, `timestamp`, `nonce`, and `signature` field. The client signs the same canonical string as for HTTP, using the WebSocket upgrade URL path and an empty body hash.

```json
{
  "type": "hello",
  "data": {
    "version": "0.3.0",
    "protocol": "portwing/1.0",
    "agentId": "...",
    "agentName": "prod-worker-01",
    "tokenHash": "",
    "pubKeyId": "4a3f2b1c9d8e7f6a",
    "timestamp": 1749600000,
    "nonce": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
    "signature": "d2hhdCBhIGRheSB0byBiZSBhbGl2ZQ",
    "dockerVersion": "27.5.1",
    "hostname": "prod-worker-01",
    "capabilities": ["compose", "exec", "metrics", "events"]
  }
}
```

The controller verifies the hello signature using its own copy of the agent's registered public key before sending `welcome`. If verification fails the controller sends an error envelope and closes the socket.

`tokenHash` is sent as empty string when Ed25519 auth is used so the controller can distinguish the auth mode without extra fields.

#### Phase 2: No per-message MAC

Per-message signing on a WebSocket incurs significant overhead for streaming paths (exec, logs). The connection is already TLS-protected. Replay of individual messages is bounded by the TLS session.

The authentication guarantee after phase 1: the peer that opened the TLS+WebSocket connection and passed hello verification controls the private key for the registered `agentId`. Subsequent messages within that connection are implicitly authorized by that proof.

If a future threat model requires per-message integrity (e.g., TLS termination at a load balancer the operator does not trust), a per-message HMAC keyed on a session key derived during hello can be added as a separate design increment.

### 3.3 Replay Protection

#### Timestamp window

Requests with `|server_time - X-Portwing-Timestamp| > 60 seconds` are rejected. This eliminates all replays captured more than 60 seconds after the fact.

#### Nonce LRU

An in-memory nonce LRU (not persistent) with a capacity of 10,000 entries covers replays within the 60-second window. After 60 seconds a nonce entry is safe to evict because the timestamp check will reject any replayed request with that nonce.

Implementation: a `sync.Mutex`-protected `map[string]time.Time` with a background cleaner, analogous to the existing `RateLimiter`. Size bounded to 10,000 entries (same cap as `RateLimiter.maxIPs`).

#### Clock skew handling

Agent and caller clocks may differ. The 60-second window accommodates NTP drift. Operators with >30s expected drift should set `MAX_CLOCK_SKEW_SECONDS` (default 60). Agents operating in environments without NTP should document this and operators may widen the window, accepting a corresponding replay window expansion.

The agent logs a warning when a valid request arrives with timestamp skew >30s: `"clock skew warning", "skew_seconds", N, "key_id", K`.

---

## 4. Key Rotation and Revocation

### Authorized-Keys File Format

The file format mirrors OpenSSH's `authorized_keys` for familiarity. Each line is one of:

```text
# comment
<algorithm> <base64-public-key> [comment]
```

Only one algorithm is recognized in this design: `ed25519`.

Examples:

```text
# Drydock platform - provisioned 2026-06-11
ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB3... drydock-prod

# Spare key for manual operator access
ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFk... operator-emergency

# Disabled key (prefix with # to revoke)
# ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAb... old-platform
```

The agent derives `Key-ID` as `hex(SHA-256(raw_32_byte_pubkey)[:8])` for each active entry.

### SIGHUP Reload

The agent sends `SIGHUP` to reload the authorized-keys file without restarting. Hot reload walks the file, rebuilds the in-memory key map, and logs each added/removed key ID. The nonce LRU is preserved across reload.

### Rotation

1. Generate a new keypair on the platform.
2. Append the new public key to the authorized-keys file on the agent.
3. Send `SIGHUP` to the agent. Both old and new keys are now active.
4. Update the platform to sign with the new private key.
5. After confirming traffic is working, remove the old public key from the file and send another `SIGHUP`.

This gives zero-downtime rotation with no coordination window.

### Revocation

Comment out or remove the key line from the authorized-keys file and send `SIGHUP`. The key is immediately removed from the in-memory map. Any request signed with the revoked key is rejected with 401 on the next agent restart or SIGHUP.

Revocation takes effect within seconds (the time for SIGHUP + file parse). There is no distributed revocation — this is appropriate for an agent model where the authorized-keys file is the single source of truth.

### Multiple Keys and Metadata

The comment field in the authorized-keys line is the primary human-readable identifier. The agent logs key additions/removals using the comment field. Operators should treat comments as required documentation:

```text
ed25519 <pubkey> platform:drydock:prod:2026-06-11
```

---

## 5. Wire Format, Config Surface, and Migration

### 5.1 Configuration (Environment Variables)

New variables (additive — all existing variables remain):

| Variable | Default | Description |
|----------|---------|-------------|
| `AUTHORIZED_KEYS` | `""` | Path to the authorized-keys file |
| `AUTHORIZED_KEYS_FILE` | alias | Alias for `AUTHORIZED_KEYS` (consistency with `TOKEN_FILE`) |
| `MAX_CLOCK_SKEW_SECONDS` | `60` | Replay protection timestamp window |
| `NONCE_LRU_SIZE` | `10000` | In-memory nonce cache capacity |
| `ENROLLMENT_TOKEN` | `""` | Bootstrap token for Model C enrollment (consumed on first use) |
| `ENROLLMENT_TOKEN_FILE` | `""` | File-based enrollment token |

The `AUTHORIZED_KEYS` variable is intentionally separate from the existing `TOKEN`/`TOKEN_HASH`/`TOKEN_FILE` variables. Both systems operate simultaneously during migration.

At the time this design was written, edge mode in `config.go` validated that `DRYDOCK_URL` required a raw `TOKEN`; the plan below extended that so validation passes if either `TOKEN` *or* `AUTHORIZED_KEYS` is set. (As of v0.6.0, edge mode instead requires `PRIVATE_KEY_FILE` and is Ed25519-only — see the note in §5.2.)

### 5.2 Migration Path

> **Historical note (as of v0.6.0):** The phases below are the original
> design-time plan and describe the state *at that time*, not v0.6.0 behavior.
> Edge mode is now Ed25519-only — `PRIVATE_KEY_FILE` is required and the
> controller rejects token-hash hellos, so the token-based Phase 0/1 handshake
> described here (including an edge hello that sends `tokenHash`) is superseded.
> Phases 2 and 3 were never executed, and `TOKEN`-based auth for standard/HTTP
> mode remains fully supported and is not deprecated. See `SECURITY.md` and
> `docs/security-model.md` for current behavior.

The migration is designed to be zero-downtime and backward-compatible:

**Phase 0 (original state):** `TOKEN` or `TOKEN_HASH` required. No Ed25519 support.

**Phase 1 (this PR series):** `AUTHORIZED_KEYS` added. Both `TOKEN` and `AUTHORIZED_KEYS` may be set simultaneously. Middleware checks for Ed25519 headers first; if absent, falls through to token check. Edge hello sends both `tokenHash` and signature; controller accepts either.

**Phase 2 (deprecation notice):** In release notes: "TOKEN-based auth is deprecated; migrate to AUTHORIZED_KEYS before v0.4.0."

**Phase 3 (removal):** Remove `rawTokenVerifier` and `argon2Verifier` paths. `AUTHORIZED_KEYS` required.

Operators who cannot migrate immediately keep Phase 1 behavior indefinitely until Phase 3 is released.

### 5.3 Exact JSON for Edge Hello (with Ed25519 auth)

The `HelloMessage` struct gains new fields. Fields are `omitempty` so existing controllers that do not understand Ed25519 fields are unaffected.

```go
type HelloMessage struct {
    // existing fields unchanged
    Version       string   `json:"version"`
    Protocol      string   `json:"protocol"`
    AgentID       string   `json:"agentId"`
    AgentName     string   `json:"agentName"`
    TokenHash     string   `json:"tokenHash,omitempty"`
    DockerVersion string   `json:"dockerVersion"`
    Hostname      string   `json:"hostname"`
    Capabilities  []string `json:"capabilities"`
    DrydockCompat string   `json:"drydockCompat,omitempty"`
    WatcherTypes  []string `json:"watcherTypes,omitempty"`
    TriggerTypes  []string `json:"triggerTypes,omitempty"`

    // new Ed25519 fields
    PubKeyID  string `json:"pubKeyId,omitempty"`
    Timestamp int64  `json:"timestamp,omitempty"`
    Nonce     string `json:"nonce,omitempty"`
    Signature string `json:"signature,omitempty"`
}
```

Wire example (edge hello with Ed25519, no token):

```json
{
  "type": "hello",
  "data": {
    "version": "0.3.0",
    "protocol": "portwing/1.0",
    "agentId": "3e4a5b6c-7d8e-9f0a-b1c2-d3e4f5a6b7c8",
    "agentName": "prod-worker-01",
    "pubKeyId": "4a3f2b1c9d8e7f6a",
    "timestamp": 1749600000,
    "nonce": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
    "signature": "d2hhdCBhIGRheSB0byBiZSBhbGl2ZQ",
    "dockerVersion": "27.5.1",
    "hostname": "prod-worker-01",
    "capabilities": ["compose", "exec", "metrics", "events"]
  }
}
```

### 5.4 Composing with mTLS

The public key in the authorized-keys file is the same key material that can back a self-signed X.509 certificate for mTLS. The forward-compatible path:

1. Agent generates Ed25519 keypair (this design).
2. Agent self-signs or CSR-signs an X.509 cert using the same keypair for mTLS.
3. Portwing accepts TLS client certificates and verifies the public key in the cert against the authorized-keys file — same verification path, different transport binding.

This means the authorized-keys file remains the single source of truth regardless of whether auth is done via HTTP headers or TLS client certs.

---

## 6. Current comparison

This design is implemented as of Portwing v0.8.1. The canonical cross-product
review now lives in the published [competitive landscape](https://portwing.codeswhat.com/docs/competitive-landscape);
this table retains only the authentication-specific comparison.

| Property | Portwing v0.8.1 | Komodo v2 Periphery | Arcane v2.9 Agent |
|----------|-----------------|---------------------|-------------------|
| Credential | Ed25519 key | Per-server public/private key pair | Environment token plus optional X.509 client certificate |
| Enrollment | Operator-provisioned `authorized_keys` file or one-time enrollment token | Onboarding key; Periphery generates its key pair | Agent token; optional automatic certificate enrollment |
| Client private key | Operator-held; loaded from a permission-checked file | Generated and retained by Periphery | Generated and retained by the Agent when mTLS is enabled |
| Authentication boundary | Detached signature on each standard-mode request; signed edge hello | Cryptographic handshake on the Core/Periphery channel | Token-authenticated channel; optional mTLS handshake |
| Application replay defense | Timestamp window plus atomic nonce LRU for signed HTTP | No per-request scheme documented; relies on the authenticated channel | No per-request scheme documented; relies on TLS/channel authentication |
| Rotation | Overlap keys, update file, SIGHUP, then remove old key | Automatic Periphery key rotation | Automatic certificate renewal; regenerate the environment token to revoke access |
| Multi-client identity | Multiple independent authorized keys | One expected Periphery identity per server | One agent identity per environment |
| mTLS | Future-compatible design, not currently implemented | Not documented | Native optional or required mode with automated CA/certificate lifecycle |

These are different security and operational tradeoffs. Portwing provides
explicit application-layer proof for each HTTP request, including replay
checks that remain visible to the agent after TLS termination. Komodo and
Arcane provide stronger automated credential lifecycle today. A correctly
implemented authenticated TLS or WebSocket channel already supplies channel
confidentiality, integrity, and replay ordering; this document does not claim
that application signatures make those channel designs categorically weaker.

Primary sources:

- Komodo [server onboarding and key lifecycle](https://komo.do/docs/setup/connect-servers)
  and [v2.0 architecture changes](https://komo.do/docs/releases/v2.0.0)
- Arcane [remote environments](https://getarcane.app/docs/features/environments)
  and [Edge Agent mTLS](https://getarcane.app/docs/security/edge-mtls)

---

## 7. Implementation Plan

Each item is scoped to be a reviewable, mergeable PR. Files that may conflict with other Wave 2 teams are flagged.

### PR 1 — `internal/auth` package: key registry and verifier

**Files:**

- `internal/auth/keys.go` — `AuthorizedKey` struct, `KeyRegistry` (loads file, SIGHUP reload, `LookupByID(keyID string) (*AuthorizedKey, bool)`)
- `internal/auth/keys_test.go` — parse valid/invalid files, SIGHUP reload, multiple keys
- `internal/auth/nonce.go` — `NonceLRU` (map + Mutex + background cleanup, same pattern as `RateLimiter`)
- `internal/auth/nonce_test.go` — LRU eviction, concurrent access, replay detection

No changes to existing files. Zero deps beyond stdlib.

**Test strategy:** table-driven unit tests covering authorized_keys parsing edge cases (blank lines, comments, malformed lines, duplicate key IDs), LRU capacity boundary, concurrent nonce insertion.

### PR 2 — `internal/auth/verify.go`: Ed25519 request verifier

**Files:**

- `internal/auth/verify.go` — request verifier plus `CanonicalMessage(method, target, bodyHash, timestamp, nonce)`; signature version 2 passes the complete origin-form request target and rejects unversioned queries.
- `internal/auth/verify_test.go` — happy path, bad signature, expired timestamp, replayed nonce, missing headers

**Conflict flag:** `internal/server/middleware.go` — the `tokenVerifier` interface is defined here. This PR reads but does not modify that file. If another team adds fields to `tokenVerifier`, coordinate.

### PR 3 — `internal/server/middleware.go`: wire Ed25519 verifier into auth middleware

**Files (modified):**

- `internal/server/middleware.go` — `AuthMiddleware` gains an optional `*auth.Ed25519Verifier` parameter (or wraps both verifiers in a `compositeVerifier`). Ed25519 check runs first; if `X-Portwing-Signature` is absent, falls through to existing `tokenVerifier` path.

**Conflict flag:** HIGH — this file is the most likely to be touched by other teams (rate limiter changes, new middleware, etc.). Coordinate merge order.

**Test strategy:** integration-style tests in `internal/server/middleware_test.go` covering: token-only request (existing), Ed25519 request (new), Ed25519 request with bad timestamp (rejected), Ed25519 request with valid token in parallel (accepted on token path).

### PR 4 — `internal/config/config.go`: new config fields

**Files (modified):**

- `internal/config/config.go` — add `AuthorizedKeysFile string`, `MaxClockSkewSeconds int`, `NonceLRUSize int`, `EnrollmentToken string`; update `IsEdgeMode()` to accept either `Token` or `AuthorizedKeysFile`; load `ENROLLMENT_TOKEN_FILE`

**Conflict flag:** MEDIUM — `config.go` is likely touched by any team adding a new env var. Check for conflicts before merging.

### PR 5 — `internal/edge/client.go`: signed hello in edge mode

**Files (modified):**

- `internal/edge/client.go` — `sendHello` reads private key path from config, loads the Ed25519 private key, signs the canonical hello string, populates new `HelloMessage` fields. Gracefully degrades to `tokenHash` if no private key configured.
- `internal/protocol/messages.go` — add `PubKeyID`, `Timestamp`, `Nonce`, `Signature` fields to `HelloMessage`

**Conflict flag:** `internal/protocol/messages.go` — any team adding new message types touches this file. Coordinate.

**Test strategy:** mock the WebSocket connection, verify that hello payload contains correct `pubKeyId`/`signature` when private key is configured; verify fallback to `tokenHash` when not configured.

### PR 6 — `internal/auth/enroll.go`: Model C enrollment endpoint (optional)

**Files (new):**

- `internal/auth/enroll.go` — `EnrollmentHandler(cfg *config.Config, registry *KeyRegistry) http.Handler`; validates `ENROLLMENT_TOKEN`, appends key to authorized-keys file, burns token

**Files (modified):**

- `internal/server/http.go` — register `POST /api/portwing/enroll` if `cfg.EnrollmentToken != ""`

**Conflict flag:** `internal/server/http.go` — HIGH conflict risk with other Wave 2 teams adding routes. Coordinate.

### PR 7 — `docs/security-model.md`: document Ed25519 as Control 11

Add Control 11 entry covering key registry, per-request signature verification, and replay protection. Reference this design doc.

### PR 8 — CLI: `portwing keygen` subcommand

**Files (new):**

- `cmd/keygen/keygen.go` — generate Ed25519 keypair, write private key to stdout in PEM (`PRIVATE KEY` block), write public key in authorized-keys line format

**Files (modified):**

- `cmd/main.go` (or equivalent entrypoint) — register `keygen` subcommand

### Total scope estimate

| PR | New files | Modified files | Est. complexity |
|----|-----------|----------------|-----------------|
| 1 | 4 | 0 | Medium |
| 2 | 2 | 0 | Medium |
| 3 | 0 | 1 (HIGH conflict) | Low |
| 4 | 0 | 1 (MEDIUM conflict) | Low |
| 5 | 0 | 2 (one MEDIUM conflict) | Medium |
| 6 | 1 | 1 (HIGH conflict) | Low |
| 7 | 0 | 1 | Low |
| 8 | 1 | 1 | Low |

PRs 1 and 2 can be built and reviewed independently with no codebase conflicts. PRs 3, 4, 5 touch shared files and should be sequenced after other Wave 2 teams' PRs are merged, or merged in a coordinated batch.

---

## Appendix A — Canonical Message Construction (pseudocode)

```go
func CanonicalMessage(method, target string, body []byte, timestampUnix int64, nonce string) []byte {
    var bodyHash string
    if len(body) == 0 {
        bodyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    } else {
        h := sha256.Sum256(body)
        bodyHash = hex.EncodeToString(h[:])
    }
    return []byte(fmt.Sprintf("%s\n%s\n%s\n%d\n%s",
        method, target, bodyHash, timestampUnix, nonce))
}
```

## Appendix B — Authorized-Keys File Grammar (EBNF)

```text
file       = { line "\n" }
line       = comment | key-line | blank
comment    = "#" { any-char-except-newline }
blank      = { " " | "\t" }
key-line   = "ed25519" SP base64-key [ SP comment-text ]
SP         = " "
base64-key = { base64-char }
comment-text = { any-char-except-newline }
```

Lines that do not match `key-line` (other than blank/comment) are logged as warnings and skipped; a single malformed line does not abort the load.

## Appendix C — Nonce Format

Nonce is 128 bits of random data, hex-encoded (32 hex characters). The agent does not validate the format beyond length; any 32-character hex string is accepted. Clients should use `crypto/rand` to generate nonces.

```go
nonce := make([]byte, 16)
_, _ = rand.Read(nonce) // io.ReadFull; ignoring error is not appropriate here — use proper error check
nonceHex := hex.EncodeToString(nonce)
```
