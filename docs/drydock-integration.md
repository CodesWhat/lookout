# Drydock Integration Reference

> Verified against Drydock `dev/v1.6` (`app/agent/AgentClient.ts`, `app/agent/api/`, and `app/agent/EdgeAgentAdapter.ts`).

## Architecture

Drydock supports two agent connectivity patterns. Portwing implements both.

```mermaid
flowchart TB
    subgraph std ["Standard mode — inbound HTTP (implemented)"]
        direction LR
        SC["Drydock controller<br/>AgentClient.ts (polls /api/*)"]
        SL["Portwing<br/>HTTP server /api/*"]
        SC -- "HTTP / SSE · X-Dd-Agent-Secret" --> SL
    end

    subgraph edge ["Edge mode — outbound WebSocket (Drydock v1.6.0-rc.11+)"]
        direction LR
        EL["Portwing<br/>edge/client (outbound)"]
        EC["Drydock controller<br/>WebSocket srv /api/portwing/ws<br/>(v1.6.0-rc.11+ — Ed25519)"]
        EL -- "WSS · hello → welcome" --> EC
    end

    subgraph legacy ["Legacy SSE agent — original Drydock Node.js"]
        direction LR
        LC["Drydock controller<br/>AgentClient.ts"]
        LL["original Node.js agent<br/>express /api/* · dd:ack on connect"]
        LC -- "HTTP / SSE" --> LL
    end
```

Portwing Standard Mode replaces the Legacy SSE Agent. Edge Mode has Portwing dial outbound to the Drydock controller's `/api/portwing/ws` endpoint using the `portwing/1.0` protocol; the controller endpoint shipped in Drydock 1.5 and requires an Ed25519-signed hello.

---

## Standard Mode: Drydock v1.6.0-rc.11+ Handshake Sequence

Source: `app/agent/AgentClient.ts:506–579`

```mermaid
sequenceDiagram
    participant D as Drydock controller (AgentClient)
    participant L as Portwing (standard mode)
    D->>L: GET /api/events · X-Dd-Agent-Secret
    L-->>D: text/event-stream
    L->>D: data: dd:ack (immediate on connect)
    Note over D: handleAckEvent() → handshake()
    D->>L: GET /api/containers
    L-->>D: 200 [Container, ...]
    D->>L: GET /api/watchers
    L-->>D: 200 [ComponentDescriptor, ...]
    D->>L: GET /api/triggers
    L-->>D: 200 [ComponentDescriptor, ...]
    Note over D,L: connection established, SSE stays open
    L->>D: data: dd:container-added (on change)
    L->>D: data: dd:container-updated
    L->>D: data: dd:container-removed
```

Source citation:

- `AgentClient.ts:506` — `handshake()` deduplication guard
- `AgentClient.ts:519` — `GET /api/containers`
- `AgentClient.ts:547` — `GET /api/watchers`
- `AgentClient.ts:559` — `GET /api/triggers`
- `AgentClient.ts:717` — `startSse()` → `GET /api/events`
- `AgentClient.ts:654` — `parseSseLine()` parses `data: {...}\n\n`

---

## Edge Mode: Portwing WebSocket Handshake

Source: `internal/edge/client.go`, `internal/protocol/messages.go`

```mermaid
sequenceDiagram
    participant L as Portwing (edge client)
    participant D as Drydock controller
    L->>D: WSS /api/portwing/ws
    L->>D: hello {version, protocol, agentId, agentName, pubKeyId,<br/>timestamp, nonce, signature, dockerVersion,<br/>capabilities, drydockCompat, watcherTypes}
    D->>L: welcome {pollInterval: 300, capabilities: [...]}
    L->>D: dd:component_sync {watchers: [...], triggers: []}
    L->>D: dd:container_sync {containers: [...]}
    L->>D: metrics {...}
```

The edge-mode hello is Ed25519-signed (`pubKeyId`/`timestamp`/`nonce`/`signature`); the controller endpoint is Ed25519-only and rejects token-hash hellos. The full `hello` payload (exact field set) is in [SPEC.md §3.2](../SPEC.md#32-hello-message).

> **Drydock version:** Drydock `v1.6.0-rc.11+` is required for Portwing v0.9.0's complete controller-owned watcher/update behavior in Standard and Edge modes. Earlier controllers can remain `portwing/1.0` wire-compatible without implementing the additive transport/ownership/event markers.
>
> **Wire versus feature compatibility:** the additive v0.9.0 markers do not bump `DrydockCompat` from 1.4.0. A successful connection alone does not prove the controller provides the v0.9.0 watcher/update feature path.

---

## Endpoints Portwing Serves (Standard Mode)

All authenticated `/api/*` endpoints accept an `Authorization: Bearer`,
`X-Dd-Agent-Secret`, or `X-Portwing-Token` header, or a valid Ed25519 request
signature in the five `X-Portwing-*` headers. `POST /api/portwing/enroll` is
the bootstrap exception when `ENROLLMENT_TOKEN` is configured: it is
rate-limited but authenticates with the one-use enrollment token in its request
body. The `/health` endpoint is unauthenticated.

| Drydock Call | Portwing Endpoint | Method | Notes |
|---|---|---|---|
| `AgentClient.startSse()` L717 | `GET /api/events` | GET | SSE stream; `dd:ack` on connect |
| `AgentClient._doHandshake()` L519 | `GET /api/containers` | GET | `[]Container` JSON |
| `AgentClient._doHandshake()` L547 | `GET /api/watchers` | GET | `[]ComponentDescriptor` |
| `AgentClient._doHandshake()` L559 | `GET /api/triggers` | GET | `[]ComponentDescriptor` (empty) |
| `AgentClient.getWatcher()` L1552 | `GET /api/watchers/{type}/{name}` | GET | Single watcher descriptor |
| `AgentClient.getContainerLogs()` L1523 | `GET /api/containers/{id}/logs` | GET | Plain text or `{logs:"..."}` |
| `AgentClient.deleteContainer()` L1539 | `DELETE /api/containers/{id}` | DELETE | 204 on success |
| `AgentClient.watch()` L1567 | `POST /api/watchers/{type}/{name}` | POST | 501 (legacy route; controller runs the watcher through the Docker proxy) |
| `AgentClient.watchContainer()` L1586 | `POST /api/watchers/{type}/{name}/container/{id}` | POST | 501 (same controller-owned path) |
| `AgentClient.runRemoteTrigger()` L1421 | `POST /api/triggers/{type}/{name}` | POST | 501 (Portwing advertises no remote trigger) |
| `AgentClient.runRemoteTriggerBatch()` L1469 | `POST /api/triggers/{type}/{name}/batch` | POST | 501 (same compatibility behavior) |
| `AgentClient.getLogEntries()` L1503 | `GET /api/log/entries` | GET | `[]` (no in-memory buffer) |
| Drydock probes | `GET /health` | GET | `{"status":"ok"}` (no auth) |

---

## SSE Event Shapes

Drydock parses SSE frames as `data: <json>\n\n`. Each frame is a JSON object
with `type` and `data` fields (`AgentClient.ts:659`).

### `dd:ack` (sent on every new SSE connection)

Drydock reads: `version`, `os`, `arch`, `cpus`, `memoryGb`, `uptimeSeconds`, `lastSeen`,
`logLevel`, `pollInterval` (`AgentClient.ts:755–782`)

Portwing sends:

```json
{
  "type": "dd:ack",
  "data": {
    "version": "0.9.0",
    "os": "linux",
    "arch": "amd64",
    "cpus": 4,
    "memoryGb": 15.6,
    "uptimeSeconds": 123,
    "lastSeen": "2026-06-11T12:00:00Z",
    "logLevel": "info",
    "pollInterval": "5m0s",
    "containers": {"total": 3, "running": 2, "stopped": 1},
    "images": 2
  }
}
```

Note: `memoryGb` is read from `/proc/meminfo` (no cgo) and rounded to one decimal GiB; non-Linux hosts report 0, which Drydock accepts. `pollInterval` is the agent's `DD_POLL_INTERVAL` as a Go duration string (Drydock's own agent sends a cron expression here — the field is informational, displayed as-is). Portwing 0.5.x and earlier sent `memoryGb: 0` and omitted `logLevel`/`pollInterval`.

`pollInterval` takes several display-only forms: a Go duration string (`"5m0s"`, Standard Mode `dd:ack`), a cron expression (Drydock's own legacy Node.js agent), and a bare integer string (`"300"`) on Drydock's REST `AgentInfo` surface. Treat those values as opaque display text. Edge Mode's `welcome` frame instead sends `pollInterval` as a JSON number (e.g. `300`). Portwing validates that value as an interval in seconds and uses it to control polling.

### `dd:container-added` / `dd:container-updated`

```json
{"type": "dd:container-added", "data": {<Container object>}}
```

### `dd:container-removed`

Drydock reads only `.data.id` (`AgentClient.ts:782`). Portwing also sends `name`
(harmless extra field).

```json
{"type": "dd:container-removed", "data": {"id": "abc123", "name": "web"}}
```

---

## Container Object Shape

Drydock's `AgentClient._doHandshake()` passes containers from `GET /api/containers`
directly to `storeContainer.insertContainer()` / `storeContainer.updateContainer()`.
Drydock expects these fields:

| Field | Type | Notes |
|---|---|---|
| `id` | string | Docker container ID |
| `name` | string | Container name (no leading `/`) |
| `displayName` | string | From `dd.display.name` label or falls back to `name` |
| `displayIcon` | string? | From `dd.display.icon` label |
| `status` | string | `running`, `stopped`, `paused`, `restarting`, `dead`, `created` |
| `watcher` | string | Watcher name (Portwing: `"docker"` or from `dd.watch` label) |
| `agent` | string? | Agent name (set by Drydock controller, stripped in agent responses) |
| `image.id` | string | Image SHA |
| `image.registry` | object | `{name:"unknown", url:"docker.io"}`; Portwing knows the registry URL, while the controller assigns its configured registry component |
| `image.name` | string | Image name |
| `image.tag` | object | `{value:"latest", semver:false}` |
| `image.digest` | object | `{watch:false, value:"sha256:…"}` |
| `image.architecture` | string | Docker image architecture, with the agent architecture as fallback |
| `image.os` | string | Docker image OS, with the agent OS as fallback |
| `updateAvailable` | bool | Always `false` in Portwing's raw inventory; Drydock enriches its controller-side watcher result |
| `updateKind` | object | Always `{"kind":"unknown"}` |
| `labels` | object? | All Docker labels |
| `details` | object? | Runtime details; ports and volumes use Drydock's string-array wire representation |

---

## Watcher Component Descriptor Shape

`GET /api/watchers` returns an array. `GET /api/watchers/{type}/{name}` returns one.

Drydock reads: `type`, `name`, `configuration`, `metadata` (`AgentClient.ts:489–503`).

Portwing returns:

```json
[{
  "type": "docker",
  "name": "docker",
  "configuration": {
    "description": "Watches Docker containers for updates via Docker Engine API",
    "capabilities": ["container-sync", "labels"],
    "transport": "docker-api",
    "execution": "controller",
    "events": "portwing"
  }
}]
```

`id` and `agent` fields (present in Drydock's own `mapComponentToItem`) are not
required by the AgentClient — it never reads them from the remote agent response.

### Watcher and Update Execution Model

The watcher configuration makes ownership explicit:

- `transport: "docker-api"` tells a compatible Drydock controller to run its
  native Docker watcher and update trigger through Portwing's authenticated,
  transparent Docker API proxy.
- `execution: "controller"` keeps registry checks and update orchestration in
  Drydock instead of delegating them to Portwing's legacy watcher/trigger POST
  endpoints.
- `events: "portwing"` makes Portwing the source of Docker lifecycle events, so
  Drydock does not open a duplicate Docker event stream through the proxy.

In Standard Mode those controller-owned Docker API calls use Portwing's inbound
HTTP proxy. In Edge Mode Drydock sends the same calls over the correlated
`request`/`response` transport on the outbound WebSocket. Portwing therefore
participates in updates as the authenticated Docker transport and event source,
even though Drydock owns watcher and trigger execution.

Portwing still returns no trigger components (`GET /api/triggers` and edge
`triggerTypes` remain empty), while `dd:component_sync` contains the Docker
watcher descriptor. The `dd:trigger` protocol vocabulary is retained for wire
compatibility, but it is not a remote trigger advertisement; the watcher/trigger
POST endpoints continue to return 501 for older clients.

The complete execution model requires Drydock `v1.6.0-rc.11` or later. Older
controllers can remain wire-compatible because the descriptor fields and Edge
ordering are additive, but they do not provide full v0.9.0 feature
compatibility.

---

## Environment Variable Mapping

### Drydock Controller Side (configures the connection to Portwing)

| Drydock env var / config | Description |
|---|---|
| Agent `host` | Portwing hostname/IP (set in Drydock agent component config, no env var) |
| Agent `port` | Portwing port (default 3000) |
| Agent `secret` | Shared secret → sent as `X-Dd-Agent-Secret` |
| `DD_AGENT_ALLOW_INSECURE_SECRET=true` | Allow secret over plain HTTP |
| Agent `cafile` | CA cert for TLS verification |
| Agent `certfile` / `keyfile` | mTLS certificate pair |

Source: `app/agent/components/Agent.ts:4–11`, `AgentClient.ts:247–258`

### Portwing Side (agent binary)

| Portwing env var | Purpose | Drydock counterpart |
|---|---|---|
| `TOKEN` | Shared secret (preferred) | Drydock agent `secret` |
| `DD_AGENT_SECRET` | Shared secret (Drydock agent secret) | Drydock agent `secret` |
| `TOKEN_FILE` | Path to file containing token | Drydock `DD_AGENT_SECRET_FILE` |
| `DD_AGENT_SECRET_FILE` | Path to file containing token | Drydock `DD_AGENT_SECRET_FILE` |
| `TOKEN_HASH` | Argon2id PHC hash of token (standard mode only) | n/a |
| `PORT` | HTTP listen port (default `3000`) | Drydock agent `port` |
| `BIND_ADDRESS` | Standard default `0.0.0.0`; Edge operations default `127.0.0.1` | n/a |
| `TLS_CERT` / `TLS_KEY` | TLS certificate/key | Drydock agent `certfile`/`keyfile` |
| `CA_CERT` | CA cert for edge mode TLS | Drydock agent `cafile` |
| `TLS_SKIP_VERIFY` | Skip TLS verification in edge mode | n/a |
| `DRYDOCK_URL` | Edge mode controller URL (agent dials out to `/api/portwing/ws`) | n/a |
| `AGENT_ID` | UUID for this agent | Drydock registers via `hello.agentId` |
| `AGENT_NAME` [^agent-name] | Display name (default: hostname) | Drydock agent component `name` |
| `DD_POLL_INTERVAL` [^poll-interval] | Container refresh interval in s (default 300) | n/a |
| `DOCKER_SOCKET` | Docker socket path | n/a |
| `LOG_LEVEL` | `debug`/`info`/`warn`/`error` | n/a |
| `TRUSTED_PROXIES` | CIDR list for X-Forwarded-For | n/a |

[^agent-name]: In Edge Mode, the (sanitized) `hello.agentName` sent by Portwing is now honored by Drydock as the agent's display name, as of Drydock dev/v1.6. Sanitization lowercases the name, replaces runs of non-`[a-z0-9-]` characters with a single `-`, trims leading/trailing `-`, and truncates to 63 characters; an empty result falls back to `portwing-edge-<agentId>`.
[^poll-interval]: In Edge Mode, a usable positive `pollInterval` from the current controller's `welcome` overrides the adapter interval. Without a usable override, Portwing uses the adapter interval, falling back to `DD_POLL_INTERVAL` when the adapter supplies none. An absent, zero, negative, malformed or out-of-range welcome interval keeps that fallback and does not close the connection. The previous connection's override is cleared on reconnect.

---

## Compatibility Matrix

> For the N-way version matrix (portwing version × Drydock version × sockguard preset × wire compat constant), see [`COMPATIBILITY.md`](../COMPATIBILITY.md) at the repo root — that file is the canonical source for "does version X of one tool work with version Y of another." The table below is feature-level detail: which individual endpoints/messages are implemented and verified against a specific Drydock build.

| Feature | Status | Evidence |
|---|---|---|
| `GET /api/events` SSE stream | COMPATIBLE | Portwing: `sse.go:48`; Drydock: `AgentClient.ts:717` |
| `dd:ack` event on connect | COMPATIBLE | Portwing: `sse.go:74`; Drydock: `AgentClient.ts:1299` |
| `dd:ack` fields (version, os, arch, cpus, memoryGb, uptimeSeconds, lastSeen, logLevel, pollInterval) | COMPATIBLE | Drydock `AgentClient.ts:755–782` reads exactly these fields |
| `dd:ack memoryGb=0` (non-Linux hosts) | COMPATIBLE | Drydock treats 0 as valid (no assertion on non-zero) |
| `dd:container-added` SSE | COMPATIBLE | Portwing: `sse.go:174`; Drydock: `AgentClient.ts:1303` |
| `dd:container-updated` SSE | COMPATIBLE | Portwing: `sse.go:187`; Drydock: `AgentClient.ts:1303` |
| `dd:container-removed` SSE (`{id, name}`) | COMPATIBLE | Drydock only reads `.id` (`AgentClient.ts:782`); extra `name` is harmless |
| `GET /api/containers` returns `[]Container` | COMPATIBLE | Portwing: `routes.go:27`; Drydock: `AgentClient.ts:519` |
| Container shape (id, name, displayName, status, watcher, image, labels, details) | COMPATIBLE | Portwing `model.go`; Drydock `model/container.js` |
| `GET /api/watchers` returns `[]ComponentDescriptor` | COMPATIBLE | Descriptor selects `transport=docker-api`, `execution=controller`, and `events=portwing` |
| `GET /api/watchers/{type}/{name}` (single watcher) | COMPATIBLE | Fixed in this PR: `routes.go:handleWatcherGet` |
| `GET /api/triggers` returns `[]` | COMPATIBLE | Portwing advertises no remote trigger; compatible Drydock uses its controller-owned update trigger through the Docker proxy |
| `GET /api/containers/{id}/logs` plain text | COMPATIBLE | Drydock `logs.ts:134–145` accepts both plain text and `{logs:"..."}` |
| `DELETE /api/containers/{id}` returns 204 | COMPATIBLE | Portwing: `routes.go:92`; Drydock: `AgentClient.ts:1543` |
| `POST /api/watchers/{type}/{name}` | COMPATIBLE (501) | Legacy route; Drydock runs its watcher through the transparent Docker proxy |
| `POST /api/watchers/{type}/{name}/container/{id}` | COMPATIBLE (501) | Same controller-owned execution path |
| `POST /api/triggers/{type}/{name}` | COMPATIBLE (501) | No Portwing remote trigger is advertised; Drydock applies updates through the proxy |
| `POST /api/triggers/{type}/{name}/batch` | COMPATIBLE (501) | Same compatibility behavior |
| `GET /api/log/entries` returns `[]` | COMPATIBLE | Fixed in this PR: `routes.go:handleLogEntries`; Drydock `AgentClient.ts:1503` |
| Authentication via `X-Dd-Agent-Secret` | COMPATIBLE | Portwing: `server/http.go` auth middleware; Drydock: `api/index.ts:73` |
| `/health` unauthenticated | COMPATIBLE | Portwing: `routes.go:handleSimpleHealth`; Drydock: `api/index.ts:152` |
| `dd:watcher-snapshot` SSE event | COMPATIBLE | Emitted after every poll cycle and on SSE connect (after `dd:ack`); see below |
| `dd:update-applied` / `dd:update-failed` SSE | N-A | Drydock owns these operation events; its Docker update calls still traverse Portwing and Portwing emits the resulting lifecycle events |
| `dd:update-operation-changed` SSE | N-A | Controller-owned update state; Portwing remains the Docker transport and lifecycle-event source |
| `dd:batch-update-completed` SSE | N-A | Controller-owned batch state; update calls still traverse Portwing |
| `dd:security-alert` / `dd:security-scan-cycle-complete` SSE | N-A | Portwing does not perform security scanning |
| Edge Mode WebSocket (`/api/portwing/ws`) | STABLE (`portwing/1.0`) | Ed25519-only; correlated `request`/`response` messages carry controller-owned watcher/update Docker API calls. Portwing 0.8 adds continuous log streaming; Drydock owns the cross-repo `quality-portwing-fleet-soak.yml` gate. |

---

## Gaps Requiring Drydock-side Changes

None. GAP-1 below was originally identified as requiring a Drydock-side
tolerance change; it has since been resolved Portwing-side.

### GAP-1 (RESOLVED): `dd:watcher-snapshot` SSE event not emitted by Portwing

**Resolution:** Portwing now emits `dd:watcher-snapshot` from `SSEBroadcaster`
after every container poll cycle (`Adapter.OnContainerRefresh`) and sends the
current snapshot to each newly connected SSE client immediately after
`dd:ack`. No Drydock-side change is needed.

**What Drydock expects:** When a watcher completes a full poll cycle, the original
Drydock Node.js agent emits a `dd:watcher-snapshot` SSE event:

```json
{
  "type": "dd:watcher-snapshot",
  "data": {
    "watcher": {"type": "docker", "name": "docker", "configuration": {...}},
    "containers": [<Container>, ...]
  }
}
```

Drydock's `AgentClient.ts:1310` handles this event to prune stale containers
(containers present in the previous inventory but absent from the snapshot are
removed). Additionally, `app/agent/api/event.ts:334` replays the last snapshot
per watcher to newly connected SSE clients, so a reconnecting controller never
misses the authoritative container list.

**What Portwing does instead:** Portwing emits individual `dd:container-added`,
`dd:container-updated`, and `dd:container-removed` events. The controller
receives these incremental events and maintains its own store. Pruning after
reconnects depends on the handshake `GET /api/containers` call (which Drydock
already does).

**Impact:** Low for most deployments. If a container is removed while the SSE
connection is dropped and the controller reconnects, the handshake call to
`GET /api/containers` will prune it (`AgentClient.ts:535`). The only edge case
is a zero-container handshake (preserved as ambiguous by Drydock, `AgentClient.ts:537`)
which relies on `dd:watcher-snapshot` to later prune stale entries.

**How it was fixed:** `SSEBroadcaster.BroadcastWatcherSnapshot` marshals the
watcher descriptor plus the full cached inventory (`ContainerManager.GetContainers`,
already rebuilt by the poll cycle that triggered the broadcast) — no extra
cycle tracking was needed. The snapshot is also written to each new SSE client
right after the `dd:ack` event so a reconnecting controller gets the
authoritative list without waiting up to one poll interval.

---

## Pre-built Docker Compose for Standard Mode

```yaml
services:
  portwing:
    image: ghcr.io/codeswhat/portwing:latest
    environment:
      TOKEN: "${DD_AGENT_SECRET}"       # same secret Drydock agent config uses
      PORT: "3000"
      AGENT_NAME: "my-server"
    # The image runs as non-root UID 65532; grant the Docker socket's group:
    #   export DOCKER_SOCK_GID=$(stat -c '%g' /var/run/docker.sock)
    group_add:
      - "${DOCKER_SOCK_GID:?set to the GID of /var/run/docker.sock}"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    ports:
      - "3000:3000"
    restart: unless-stopped
```

In Drydock agent component configuration:

```yaml
agent:
  my-server:
    host: http://my-server-ip
    port: 3000
    secret: "${DD_AGENT_SECRET}"
```
