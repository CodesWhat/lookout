# quality-history

Machine-written trend data for Portwing's scheduled quality lanes. One
append-only JSONL file per lane, one record per lane run:

| File | Lane | Cadence |
|---|---|---|
| `soak.jsonl` | `quality-soak-weekly.yml` | weekly |
| `mutation.jsonl` | `quality-mutation-monthly.yml` | monthly, one record per matrix package |
| `mutation-survivors.jsonl` | `quality-mutation-monthly.yml` | monthly, one record per run, capped to the newest 12 |
| `fuzz-nightly.jsonl` | `quality-fuzz-nightly.yml` | nightly |
| `bench.jsonl` | `quality-bench-monthly.yml` | monthly |

This is an orphan branch. It shares no history with `main` or any `dev/*`
branch, it is never merged, and nothing here is part of a release. Records are
written by `scripts/ci/quality-history-append.sh` on the trunk branches; read
them with `scripts/quality-history.sh <lane> [--last N]`.

Every record carries the same envelope: `lane`, `timestamp` (UTC), `workflow`,
`event`, `run_id`, `run_attempt`, `run_number`, `sha`, `ref`. The remaining
keys are the lane's own headline numbers.

Only `schedule` and `workflow_dispatch` runs append. Rewriting history here is
safe: nothing depends on these commits, and the branch can be deleted and
allowed to bootstrap itself again.
