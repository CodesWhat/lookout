#!/usr/bin/env bash
#
# Functional test for scripts/ci/fuzz-history-compute.sh, run the same way
# quality-fuzz-nightly.yml's `history` job does: the compute script prints
# each record and quality-history-append.sh pushes it to a real (throwaway)
# git remote, over exactly the loop the workflow's `run:` block uses.
#
# The property under test: a malformed numeric field in one leg's downloaded
# record (corpus_total or new_interesting is not a plain non-negative
# integer — a "bad" leg, corrupted or hand-crafted) must never stop the
# remaining legs from being appended. Before this test existed, that field
# went straight into `$(( ))` unguarded; under `set -euo pipefail`, a
# non-numeric value there is an arithmetic syntax error that aborts the whole
# merge, silently dropping every record after the bad one — including the
# union row, which runs last.

set -euo pipefail
export LC_ALL=C

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compute="${repository_root}/scripts/ci/fuzz-history-compute.sh"
append="${repository_root}/scripts/ci/quality-history-append.sh"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/portwing-fuzz-history-merge.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

bare="${test_root}/qh.git"
git init --quiet --bare "${bare}"

failures=0
fail() {
	echo "FAIL: $1" >&2
	failures=$((failures + 1))
}

branch_file() {
	git -C "${bare}" cat-file blob "quality-history:$1" 2>/dev/null || true
}

# One record dir per fuzz-history-<Target>-<run-id> artifact, the shape
# actions/download-artifact leaves behind under `records/`.
records_dir="${test_root}/records"
mkdir -p "${records_dir}"

write_target_record() {
	local target="$1" corpus_total="$2" new_interesting="$3"
	local dir="${records_dir}/fuzz-history-${target}"
	mkdir -p "${dir}"
	jq -cn \
		--arg target "${target}" \
		--arg package "./internal/example/" \
		--argjson corpus_total "${corpus_total}" \
		--argjson new_interesting "${new_interesting}" \
		'{
			scope: "target", target: $target, package: $package,
			kind: "pass", coverage_status: "ok", corpus_coverage_pct: 50,
			corpus_seed: 1, corpus_cached: 1, corpus_total: $corpus_total,
			new_interesting: $new_interesting
		}' >"${dir}/fuzz-score-${target}.json"
	cat >"${dir}/fuzz-cover-${target}.out" <<EOF
mode: set
pkg/${target}.go:1.1,2.1 5 1
EOF
}

# Two well-formed legs plus one whose corpus_total is the string "bad" — the
# exact shape a corrupted or hand-crafted artifact would take: valid JSON,
# wrong type for one field.
write_target_record "FuzzOne" 10 2
write_target_record "FuzzTwo" 20 3

mkdir -p "${records_dir}/fuzz-history-FuzzBad"
jq -cn '{
	scope: "target", target: "FuzzBad", package: "./internal/example/",
	kind: "pass", coverage_status: "ok", corpus_coverage_pct: 50,
	corpus_seed: 1, corpus_cached: 1, corpus_total: "bad",
	new_interesting: 4
}' >"${records_dir}/fuzz-history-FuzzBad/fuzz-score-FuzzBad.json"
cat >"${records_dir}/fuzz-history-FuzzBad/fuzz-cover-FuzzBad.out" <<'EOF'
mode: set
pkg/FuzzBad.go:1.1,2.1 5 1
EOF

# --- the compute script itself must not abort on the bad record ------------

compute_output="$(bash "${compute}" "${records_dir}")"
compute_status=$?
[ "${compute_status}" -eq 0 ] ||
	fail "fuzz-history-compute.sh must exit 0 even with a malformed corpus_total in one record (got ${compute_status})"

printed_lines="$(printf '%s\n' "${compute_output}" | grep -c . || true)"
# 3 target records (FuzzOne, FuzzTwo, FuzzBad) + 1 union record = 4 lines.
[ "${printed_lines}" = "4" ] ||
	fail "expected 4 printed records (3 targets + 1 union), got ${printed_lines}: ${compute_output}"

union_line="$(printf '%s\n' "${compute_output}" | jq -c 'select(.scope == "union")')"
[ -n "${union_line}" ] ||
	fail "the malformed record must not have prevented the union line from being printed"
# FuzzOne (10) + FuzzTwo (20) + FuzzBad ("bad" -> sanitized to 0) = 30.
[ "$(jq -r '.corpus_total' <<<"${union_line}")" = "30" ] ||
	fail "the union's corpus_total must sum the well-formed legs and treat the malformed one as 0, got $(jq -r '.corpus_total' <<<"${union_line}")"

# --- run it exactly the way the workflow's history job does ----------------
#
# while IFS= read -r numbers; do
#   scripts/ci/quality-history-append.sh fuzz-nightly "${numbers}"
# done < <(bash scripts/ci/fuzz-history-compute.sh records)

run_count=0
while IFS= read -r numbers; do
	env \
		QUALITY_HISTORY_REMOTE="${bare}" \
		QUALITY_HISTORY_EVENT="schedule" \
		QUALITY_HISTORY_PUSH_ATTEMPTS="4" \
		QUALITY_HISTORY_AUTHOR_NAME="test" \
		QUALITY_HISTORY_AUTHOR_EMAIL="test@example.invalid" \
		GITHUB_RUN_ID="200" \
		GITHUB_RUN_ATTEMPT="1" \
		GITHUB_RUN_NUMBER="1" \
		GITHUB_WORKFLOW="Quality: Test" \
		GITHUB_SHA="0000000000000000000000000000000000000000" \
		GITHUB_REF_NAME="dev/v0.9" \
		bash "${append}" fuzz-nightly "${numbers}" >/dev/null 2>&1
	run_count=$((run_count + 1))
done < <(bash "${compute}" "${records_dir}")

[ "${run_count}" = "4" ] ||
	fail "the workflow's own read loop must see 4 lines from the compute script (N=3 targets + 1 union), saw ${run_count}"

# --- N+1 rows must actually have landed on the branch -----------------------
#
# N = 3 target records; +1 is the union row. The malformed FuzzBad record
# must have been recorded too (as its own row, sanitized), not dropped.
appended_lines="$(branch_file fuzz-nightly.jsonl | grep -c . || true)"
[ "${appended_lines}" = "4" ] ||
	fail "expected N+1 = 4 rows on the fuzz-nightly.jsonl branch (3 targets + 1 union), found ${appended_lines}"

targets_on_branch="$(branch_file fuzz-nightly.jsonl | jq -r 'select(.scope == "target") | .target' | sort)"
expected_targets="$(printf 'FuzzBad\nFuzzOne\nFuzzTwo\n')"
[ "${targets_on_branch}" = "${expected_targets}" ] ||
	fail "expected FuzzBad, FuzzOne and FuzzTwo all recorded despite FuzzBad's malformed field; got: ${targets_on_branch}"

# FuzzBad's own record is stored exactly as its leg reported it — the
# sanitizing guard protects the union's aggregate arithmetic, not the
# per-target record, so a downloaded record with a malformed field still
# lands on the branch carrying that field verbatim rather than being
# silently rewritten.
bad_record="$(branch_file fuzz-nightly.jsonl | jq -c 'select(.target == "FuzzBad")')"
[ "$(jq -r '.corpus_total' <<<"${bad_record}")" = "bad" ] ||
	fail "FuzzBad's own record must be appended with its corpus_total verbatim (\"bad\"), got $(jq -r '.corpus_total' <<<"${bad_record}")"

union_on_branch="$(branch_file fuzz-nightly.jsonl | jq -c 'select(.scope == "union")')"
[ -n "${union_on_branch}" ] ||
	fail "the union row must have reached the branch despite the malformed leg"
[ "$(jq -r '.corpus_total' <<<"${union_on_branch}")" = "30" ] ||
	fail "the appended union row's corpus_total must be 30 (10 + 20 + 0), got $(jq -r '.corpus_total' <<<"${union_on_branch}")"
[ "$(jq -r '.profiles_merged' <<<"${union_on_branch}")" = "3" ] ||
	fail "all three legs scored ok with a coverprofile, so profiles_merged must be 3, got $(jq -r '.profiles_merged' <<<"${union_on_branch}")"
# Each leg's coverprofile has one distinct, fully-covered block (5 stmts,
# count 1) under a different file name, so the union carries all three: 15
# covered of 15 total.
[ "$(jq -r '.union_stmts_covered' <<<"${union_on_branch}")" = "15" ] ||
	fail "the appended union row's union_stmts_covered must be 15 (3 legs x 5 covered stmts), got $(jq -r '.union_stmts_covered' <<<"${union_on_branch}")"
[ "$(jq -r '.union_stmts_total' <<<"${union_on_branch}")" = "15" ] ||
	fail "the appended union row's union_stmts_total must be 15 (3 legs x 5 stmts), got $(jq -r '.union_stmts_total' <<<"${union_on_branch}")"

if [ "${failures}" -ne 0 ]; then
	echo "${failures} fuzz-history merge check(s) failed" >&2
	exit 1
fi

echo "fuzz-history-compute.sh checks passed."
