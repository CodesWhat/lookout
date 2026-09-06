# Contributing to Portwing

Thanks for your interest in contributing! Whether it is a bug fix, new feature, documentation improvement, or something else — all contributions are welcome. Please read [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) before participating.

Questions, ideas, or help? Use [GitHub Discussions](https://github.com/CodesWhat/portwing/discussions) or the [CodesWhat Discord](https://discord.gg/mWHCPJRzSx). GitHub Issues are for bugs and concrete feature requests — see [SECURITY.md](SECURITY.md) instead for reporting a vulnerability.

## Getting started

1. **Fork** the repository and clone your fork.
2. **Install Go 1.26+**:

   ```bash
   go version  # should be 1.26+
   ```

3. **Download dependencies**:

   ```bash
   go mod download
   ```

4. **Create a branch** from the active integration branch (currently
   `dev/v0.9`). Portwing uses an integration-branch flow: bug fixes and
   features branch from and merge back into the current `dev/*` branch,
   which is promoted to `main` immediately before each release cut —
   `main` itself only ever advances by those promotions.

## Development loop

```bash
go build ./...                  # Build
go test -race ./...             # All tests with race detector
gofmt -l .                      # List unformatted files (must be empty)
golangci-lint run               # Lint
go vet ./...                    # Vet
```

### Running integration tests

Integration tests require a running Docker daemon:

```bash
go test -tags=integration -race -timeout=10m ./internal/integration/
```

### Drydock compatibility

A compatibility verification script is included:

```bash
# Verify protocol compatibility with the Drydock platform
./scripts/drydock-compat-check.sh
```

### Fuzz testing (Tier 0 smoke)

Each fuzz target runs for 5 seconds locally:

```bash
go test -run=^$ -fuzz=^FuzzParsePHC$           -fuzztime=5s ./internal/server/
go test -run=^$ -fuzz=^FuzzParseTrustedProxies$ -fuzztime=5s ./internal/server/
go test -run=^$ -fuzz=^FuzzParseImageRef$       -fuzztime=5s ./internal/adapter/
go test -run=^$ -fuzz=^FuzzParseLabels$         -fuzztime=5s ./internal/adapter/drydock/
go test -run=^$ -fuzz=^FuzzMCPHandler$          -fuzztime=5s ./internal/mcp/
go test -run=^$ -fuzz=^FuzzEnvelope$            -fuzztime=5s ./internal/protocol/
go test -run=^$ -fuzz=^FuzzVerifyRequest$       -fuzztime=5s ./internal/auth/
go test -run=^$ -fuzz=^FuzzDecodeContainerLogStream$ -fuzztime=5s ./internal/docker/
go test -run=^$ -fuzz=^FuzzComposeRequestValidate$   -fuzztime=5s ./internal/docker/
go test -run=^$ -fuzz=^FuzzParseKeyLine$             -fuzztime=5s ./internal/auth/
```

Every target ships a committed seed corpus under
`<pkg>/testdata/fuzz/<Target>/`, and that is also where the engine writes a
crashing input. Commit a crasher along with its fix. The coverage-expanding
inputs the engine finds live in `$GOCACHE/fuzz/` instead; the nightly and
monthly lanes carry only that directory between runs through a shared
`actions/cache` key, so a deep run resumes rather than restarting from the
`f.Add()` seeds. The committed seed corpus stays authoritative on every run —
it is never cached, so git decides what's in it, not a stale cache entry — and
a crasher found mid-run is recovered from the uploaded failure artifact if it
isn't committed before the cache is pruned.

`FuzzParseImageRef` and `FuzzParseLabels` are the exception: both are
saturated (millions of executions, zero new interesting inputs), so the
nightly and monthly lanes run them as a seed-replay only — the committed and
cached corpus replayed through `go test -run`, no `-fuzz` minutes, no growth
in `$GOCACHE/fuzz/` for either — while still catching a regression in what's
already committed. The command above still runs a real 5-second `-fuzz` smoke
for them locally; only the CI lanes' cadence differs.

### Benchmarks

```bash
go test -run='^$' -bench=. -benchmem -count=5 -timeout=20m ./...
```

Methodology, hardware/Go version baseline, and what the monthly benchmark
gate does and doesn't measure: [`BENCHMARKS.md`](BENCHMARKS.md).

## Code style

- **Formatter**: `gofmt` (enforced by CI)
- **Linter**: [golangci-lint](https://golangci-lint.run/)
- Follow [Effective Go](https://go.dev/doc/effective_go) conventions
- Line length: no hard limit; use judgment
- **Zero new dependencies**: stdlib + `golang.org/x/crypto` + `github.com/google/uuid` + `github.com/gorilla/websocket`. PRs adding deps require a strong justification.

## Knip

The `website`, `docs`, and `analytics` npm workspaces (plus the root
`scripts/` workspace) are checked for dead files, unused exports, and
unlisted/unused dependencies by [knip](https://knip.dev), configured in
`knip.json`. Run it with `npm run knip`; it also runs as part of
`npm run check:web` (lefthook pre-push and CI's node-ci "Web Contract" job).

`knip.json` is plain JSON, so the reasons behind each entry live here instead
of inline comments:

- `.` workspace `ignoreDependencies: ["posthog-js"]` — root's
  `scripts/web-analytics-source.test.mjs` resolves
  `posthog-js/dist/extension-bundles.js` via `require.resolve(..., { paths:
  [...] })` pointed at the `analytics` workspace's `node_modules`, not root's.
  `posthog-js` is a real dependency of `analytics`, not of the root
  workspace, so knip correctly sees no root `package.json` entry for it; the
  ignore documents that the reference is deliberate cross-workspace
  resolution, not a phantom import.
- `website` workspace `entry: ["scripts/gen-bird-png.mjs"]` — this script is
  run manually, not imported by the Next.js app and not referenced from any
  `npm` script, so knip's Next.js plugin doesn't see it as reachable without
  being told it's an entry point. `security-headers.mjs` doesn't need the
  same treatment: it's reached via the `postbuild` script, and its test file
  is picked up by knip's test-file detection. The root `.` workspace has no
  entry glob for the same reason — every non-test script under root
  `scripts/` is already referenced from a `package.json` script, and the
  `*.test.mjs` files are covered by test-file detection.
- `website` workspace `ignoreIssues: { "src/components/ui/**": ["exports",
  "types"] }` — these are shadcn/ui-generated primitives; the unused variant
  exports (e.g. `badgeVariants`, `CardHeader`) are part of the generator's
  standard public API, kept for future consumers rather than trimmed to
  current usage. Scoped to the `exports`/`types` issue types rather than a
  file-level `ignore`, so a genuinely dead new file in that directory is
  still reported as unused.
- `website` workspace `ignoreBinaries: ["magick"]` — `gen-bird-png.mjs`
  shells out to ImageMagick's `magick` CLI, which isn't an npm dependency.

## Commit convention

We use **Conventional Commits** — no emoji:

```text
<type>(<scope>): <description>
```

Scope is optional. A `!` before the colon marks a breaking change (`feat(api)!: drop v1 tokens`). A `BREAKING CHANGE:` footer is valid Conventional Commit syntax, but release versioning currently reads only the subject-line `!` for major bumps.

| Type | Use |
|------|-----|
| `feat` | New feature |
| `fix` | Bug fix |
| `docs` | Documentation |
| `style` | Formatting only |
| `refactor` | Refactor (no feature/fix) |
| `perf` | Performance improvement |
| `test` | Tests |
| `build` | Build system / dependencies (e.g. `build(deps): bump gorilla/websocket`) |
| `ci` | CI / deployment config (e.g. `ci(deploy): add release-cut concurrency guard`) |
| `chore` | Everything else / tooling / config (e.g. `chore(config): tune lefthook timeouts`) |
| `revert` | Reverting a previous commit |

Example: `feat(auth): add Ed25519 enrollment`

Multi-change commits: lead type on first line, bulleted sub-changes in body. Reference Linear issues in footer as `Fixes: LIN-XXX`.

## Pull request guidelines

- Target the active integration branch (currently `dev/v0.9`) for all PRs,
  bug fixes and features alike. Never target `main` directly; it advances
  only through release promotions.
- Keep PRs focused — one feature or fix per PR.
- Include tests for non-trivial changes.
- Update `CHANGELOG.md` under `[Unreleased]` for user-visible changes.
- All CI checks must pass before merge.

## Security

Please report security vulnerabilities privately via [GitHub Security Advisories](https://github.com/CodesWhat/portwing/security/advisories/new) — **do NOT open a public issue**. See [SECURITY.md](SECURITY.md) for the full policy.
