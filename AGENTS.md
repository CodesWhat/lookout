# AGENTS.md

Guidance for coding agents working in this repository.

## What is Portwing?

Portwing is a security-first remote Docker agent written in Go. It exposes a transparent Docker API proxy over the local Docker socket, plus higher-level endpoints for container lifecycle, compose stack management, exec, and events. It runs in two modes: **standard** (inbound HTTP/S server on `PORT`, default 3000) and **edge** (outbound WebSocket tunnel to `DRYDOCK_URL` — no inbound ports). Two adapters: **drydock** (native `dd:*` protocol + SSE compatibility for [drydock](https://github.com/CodesWhat/drydock)) and **generic** (clean REST API defined in `api/openapi.yaml`).

## Repository structure

- `cmd/portwing/` — entrypoint; subcommands: serve (default), `keygen`, `hash-token`, `version`
- `internal/config/` — env-var configuration (no flags, no config files)
- `internal/server/` — standard-mode HTTP server, auth middleware, rate limiting, audit log, metrics
- `internal/auth/` — Ed25519 per-request auth: key registry (`authorized_keys` format), nonce LRU, request verification, keygen, enrollment
- `internal/edge/` — edge-mode WebSocket client (outbound tunnel)
- `internal/docker/` — Docker socket client (proxy, hijack, streaming)
- `internal/adapter/` — adapter interface + `drydock/` and shared types; `internal/generic/` — generic REST adapter
- `internal/mcp/` — Model Context Protocol server (JSON-RPC over HTTP)
- `internal/protocol/` — wire types shared between standard and edge modes; `version.go` holds `AgentVersion`
- `api/openapi.yaml` — generic adapter API contract
- `docs/` — security model, drydock integration notes
- `scripts/drydock-compat-check.sh` — 35-check live compatibility suite against a running agent

## Build, test, lint

```bash
go build ./cmd/portwing              # build
go test -race ./...                 # all tests (race detector is mandatory)
golangci-lint run                   # lint (config: .golangci.yml, v2 schema)
go test -tags integration ./internal/integration/   # needs a real dockerd

# Fuzzers (5s smoke, all 10; CI runs 60s, nightly 5m, monthly 6x10m legs = 1h):
go test -run='^$' -fuzz='^FuzzParsePHC$' -fuzztime=5s ./internal/server/
go test -run='^$' -fuzz='^FuzzParseTrustedProxies$' -fuzztime=5s ./internal/server/
go test -run='^$' -fuzz='^FuzzParseImageRef$' -fuzztime=5s ./internal/adapter/
go test -run='^$' -fuzz='^FuzzParseLabels$' -fuzztime=5s ./internal/adapter/drydock/
go test -run='^$' -fuzz='^FuzzMCPHandler$' -fuzztime=5s ./internal/mcp/
go test -run='^$' -fuzz='^FuzzEnvelope$' -fuzztime=5s ./internal/protocol/
go test -run='^$' -fuzz='^FuzzVerifyRequest$' -fuzztime=5s ./internal/auth/
go test -run='^$' -fuzz='^FuzzDecodeContainerLogStream$' -fuzztime=5s ./internal/docker/
go test -run='^$' -fuzz='^FuzzComposeRequestValidate$' -fuzztime=5s ./internal/docker/
go test -run='^$' -fuzz='^FuzzParseKeyLine$' -fuzztime=5s ./internal/auth/

# Live drydock-compat smoke (agent must be running):
./scripts/drydock-compat-check.sh http://localhost:3000 <token>
```

## Hard invariants — do not break these

1. **Dependency policy: three direct deps, period.** `gorilla/websocket`, `google/uuid`, `golang.org/x/crypto`. Everything else is stdlib. Do not add a dependency without explicit maintainer approval; all crypto is stdlib (`crypto/ed25519`, `crypto/subtle`, `x/crypto/argon2`).
2. **`statusRecorder` must forward `Flush`, `Hijack`, and `Unwrap`** (`internal/server/middleware.go`). SSE streaming and Docker exec/attach hijacking break silently otherwise. Regression test: `TestAuthMiddlewarePreservesStreamingInterfaces`.
3. **Rate limiting runs before credential verification** — failed-auth accounting must stay cheap and happen first, including on the enrollment path (`rateLimitOnly` records 401s).
4. **Nonce replay protection: `NonceLRU.Add` is the atomic authority.** Never gate acceptance on `Seen()` alone — that's a TOCTOU race. `Seen()` is only a cheap pre-verify reject.
5. **`AgentVersion` stays a `var`** (`internal/protocol/version.go`) — GoReleaser injects it with `-X`, which silently does nothing to a `const`.
6. **`dd:watcher-snapshot` containers must serialize as a JSON array, never `null`** — drydock's `handleWatcherSnapshotEvent` prunes from it; `nil` slices must be normalized to `[]adapter.Container{}`.
7. **macOS test sockets: unix socket paths are limited to 104 bytes on darwin.** Use `os.MkdirTemp("", "lk")` for socket dirs in tests, never `t.TempDir()` (its path is too long on macOS runners).
8. **Shell scripts target bash 3.2** (macOS system bash). Under `set -u`, empty-array expansion must use `${arr[@]+"${arr[@]}"}`; command substitutions feeding a pipeline that may legitimately fail need `|| true` under `set -e`.
9. **Auth failures return 401 with an `X-Portwing-Reason` header** (`timestamp-skew`, `replay`, `unknown-key`, `invalid-signature`) — the compat script and drydock both key off these.
10. **A `Fuzz*` target must be self-contained in its own `_test.go` file, and may only use the `testing` API the ClusterFuzzLite shim implements.** `compile_native_go_fuzzer` builds it with go-118-fuzz-build, which lifts that one file into a normal package build and rewrites `testing` to a drop-in shim: a helper in a sibling `_test.go` is undefined, the shim has no `testing.TB`, and its `F` has no `Fatalf`. A helper shared between a Fuzz seeding scope and a Test takes no testing type at all, it returns an error. Breaks only the ClusterFuzzLite tier, so `go test` and the four Go-native tiers stay green while it is broken. Details in `.clusterfuzzlite/build.sh`.

## Conventions

- **Commits:** plain Conventional Commits, no emoji — `<type>(scope): <description>` (see CONTRIBUTING.md). Enforced by lefthook via `scripts/validate-commit-msg.sh`.
- **Branches:** `main` is production; one active dev branch is the next release; feature branches merge into the dev branch promptly and are deleted after merge.
- **Tests:** table-driven with `httptest`; anything touching goroutines runs under `-race` in CI — write tests accordingly (no unsynchronized `httptest.ResponseRecorder` access from a handler goroutine; wrap with a mutex-guarded recorder).
- **Errors:** wrap with `fmt.Errorf("context: %w", err)`; structured logging via `log/slog` only.

## CI map

`ci-verify.yml` calls the organization Go and Node workflows by a full commit SHA; fixed commands live in `scripts/ci/`, while Portwing keeps its fuzz inventory, CodeQL category, and dependency review locally. It runs lint, test -race, the coverage floor, fuzz smoke, web builds, workflow security, and release-config checks on every applicable push/PR. `quality-fuzz-nightly.yml` (Tier 2, 5m per fuzzer, daily) and `quality-fuzz-monthly.yml` (Tier 3, 1h per fuzzer, monthly, split into 6 parallel 10-minute legs per fuzzer plus a per-fuzzer merge job — no single leg saturates CPU long enough to hit the hosted-runner shutdown signal that kills a sustained job past ~14-19m), which share one `actions/cache` key namespace for the generated `$GOCACHE/fuzz/` corpus (the committed seed corpus under `testdata/fuzz/` is never cached and stays authoritative on every run) so the monthly lane restores what the nightly saved instead of missing its own entry to the 7-day eviction — two of the ten targets, `FuzzParseImageRef` and `FuzzParseLabels`, are saturated (millions of executions, zero new interesting inputs) and run seed-replay only (`go test -run`, no `-fuzz` minutes) every night via `scripts/ci/fuzz-replay.sh`, with no leg in the monthly matrix at all; `scripts/fuzz-tier-config-test.sh`'s `replay_tier_fuzzers` pins that split · `quality-fuzz-monthly-rerun.yml` (re-runs the monthly fuzz only when its runner was torn down, never on a fuzz finding) · `quality-fuzz-cflite-pr.yml`, `quality-fuzz-cflite-batch.yml` and `quality-fuzz-cflite-prune.yml` (Tier 4, ClusterFuzzLite: the same ten targets rebuilt against libFuzzer and AddressSanitizer through OSS-Fuzz's base-builder-go, 300s on a PR that touches Go, an hour every Saturday, a corpus prune on Wednesday, with the corpus persisted on this repo's `clusterfuzzlite-corpus` orphan branch; build integration in `.clusterfuzzlite/`, shape gated by `scripts/cflite-config-test.sh`) · `quality-integration.yml` (real dockerd) · `quality-integration-engines.yml` (weekly, the same suite against a pinned Docker Engine matrix) · `quality-mutation-monthly.yml` (Gremlins) · `quality-bench-monthly.yml` (benchstat hot-path regression gate, monthly) · `quality-soak-weekly.yml` (RSS-growth soak, Sundays) · `quality-lane-notify.yml` (one `workflow_run` listener over every scheduled quality lane: opens or comments on a `quality-lane`-labelled tracking issue when a lane fails, closes it when the lane goes green) · `security-grype.yml` (Grype, gosec) · `security-scorecard.yml` (OpenSSF) · `security-zap-baseline.yml` (passive DAST baseline for the static sites, weekly) · `release-cut.yml` → `release.yml` (GoReleaser + cosign + provenance, plus a per-platform Grype scan of the actual published image; see RELEASING.md) · `main-is-released.yml` (daily cron watching that main's HEAD is a tagged release; deliberately not a required check) · `starchart.yml` (regenerates the committed star-history SVG on a `v*` tag push, never on a cron). `greptile.json` pins Greptile to `skipReview: AUTOMATIC` so it never reviews on its own; `greptile.yml` summons it as an on-demand second opinion when a PR gets the `second-opinion` label, alongside CodeRabbit's automatic review.

### Quality lane history

The scheduled quality lanes print their headline numbers to one run's log and
step summary and nothing else, so a regression that is slow enough to stay
inside its threshold every single week is invisible in every single run. The
`quality-soak-weekly.yml` and `quality-mutation-monthly.yml` lanes each end
with a separate `history` job that appends that run's numbers to
`quality-history`, an orphan branch in this repository holding one append-only
JSONL file per lane (`soak.jsonl`, `mutation.jsonl`, and
`fuzz-nightly.jsonl`/`bench.jsonl` when those lanes adopt it).

It is a separate job on purpose. `contents: write` has to live somewhere for
the push, and the measuring jobs run the product: the soak job drives the agent
under load for four hours, and the Gremlins matrix executes mutated source and
a third-party binary. A write-scoped credential in either would be reachable
through anything those steps could have rewritten, including the append script
itself. The `history` job checks the tree out fresh, downloads the run's
artifacts, and runs nothing but jq and git; the soak numbers travel as job
outputs plus the existing `soak-output.txt` artifact, and each mutation matrix
leg uploads a one-file `mutation-history-<package>-<run_id>` record for it to
read. Downloaded records are data: validated as JSON and passed as arguments,
never sourced.

Read a lane's series with `scripts/quality-history.sh <lane> [--last N]`, which
fetches the branch into a throwaway clone and prints a table; `--json` gives
the raw records. Records are written by `scripts/ci/quality-history-append.sh`,
which is deliberately incapable of failing its caller: an append that cannot
reach the remote warns and exits 0, because a trend surface must never be able
to turn a green quality lane red. Only `schedule` and `workflow_dispatch` runs
append, gated both in the workflow `if:` and in the script.

The branch is never merged and never released. Nothing on a trunk branch
changes when a lane runs, which is what keeps the house rule that committed
generated artifacts only move at a release cut.
`scripts/quality-history-config-test.sh` holds both lanes to the write
scoping, including that the recording job never grows a `setup-go` or a `go
build`; `scripts/quality-history-script-test.sh` drives the appender against a
real git remote (bootstrap, append, a push rejected by a concurrent writer, and
a repeated identical record that must not become a second row); and
`scripts/quality-history-record-test.sh` extracts the mutation lane's record
step out of the workflow and runs it against fixtures, because deciding whether
a run counts as measured is where every bug in this feature has been so far.
