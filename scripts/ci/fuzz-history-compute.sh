#!/usr/bin/env bash
#
# Compute quality-fuzz-nightly.yml's per-run quality-history records from the
# downloaded per-target fuzz-score-*.json artifacts (Spec 2 — Fuzz score):
# one JSON object per line for each target's own record (scope: target),
# followed by one more line for the run's merged statement coverage
# (scope: union). Prints only — the caller (the `history` job's own
# `run:` block) is what actually calls
# scripts/ci/quality-history-append.sh, one call per printed line, so that
# script stays the one and only thing in this repository that pushes to the
# quality-history branch.
#
# Split out of that inline shell so the arithmetic on untrusted per-record
# fields (corpus_total, new_interesting — each downloaded from another job's
# artifact) is exercised by a real script test
# (scripts/fuzz-history-merge-script-test.sh) instead of only ever running
# for real once a night.
#
# Usage: fuzz-history-compute.sh <records-dir>
#
#   <records-dir>  directory holding one subdirectory per downloaded
#                  fuzz-history-*-<run-id> artifact, each containing a
#                  fuzz-score-<Target>.json and (when coverage_status="ok")
#                  a fuzz-cover-<Target>.out
#
# Optional env:
#   TARGETS_EXPECTED  the fixed matrix size (deep-fuzz's strategy.job-total,
#                      carried out as its `targets_expected` job output).
#                      max()'d against the number of records actually
#                      downloaded, so a leg that died before its own upload
#                      step ran (cancelled, runner lost, checkout failed) is
#                      still counted as missing rather than simply invisible.
#
# A malformed corpus_total or new_interesting in one leg's record — anything
# that is not a plain non-negative integer — must never stop the remaining
# records from being printed: every record is validated and sanitized on its
# own, and the loop always runs to completion.
set -euo pipefail
export LC_ALL=C
shopt -s nullglob

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
union_awk="${script_dir}/fuzz-coverprofile-union.awk"

records_dir="${1:-}"
if [ -z "${records_dir}" ]; then
	echo "usage: fuzz-history-compute.sh <records-dir>" >&2
	exit 1
fi

records=("${records_dir}"/*/fuzz-score-*.json)

if [ "${#records[@]}" -eq 0 ]; then
	echo "::warning::No per-target fuzz records were downloaded; nothing to append." >&2
	exit 0
fi

records_downloaded="${#records[@]}"
# The matrix size, carried out of deep-fuzz as strategy.job-total. A leg that
# died before its upload step ran — cancelled while pending on the shared
# per-fuzzer corpus concurrency group, runner lost, checkout failed —
# contributes no record at all, so a count of downloaded records can never
# notice that it is missing. max(), so an absent or stale value can only ever
# fail safe.
targets_total="${records_downloaded}"
case "${TARGETS_EXPECTED:-}" in
'' | *[!0-9]*) ;;
*)
	if [ "${TARGETS_EXPECTED}" -gt "${targets_total}" ]; then
		targets_total="${TARGETS_EXPECTED}"
	fi
	;;
esac

# A digits-only guard for a value that is about to be handed to `$(( ))`. A
# non-numeric or missing value degrades to 0 rather than reaching arithmetic
# expansion as a syntax error, which — under set -euo pipefail — would abort
# this whole loop and every record after the bad one, target and union lines
# alike, instead of just the one field.
sanitize_int() {
	local v="$1"
	case "${v}" in
	'' | *[!0-9]*) echo 0 ;;
	*) echo "${v}" ;;
	esac
}

profile_args=()
packages_file="$(mktemp)"
trap 'rm -f "${packages_file}"' EXIT
corpus_total_all=0
new_interesting_total=0

# Every record is untrusted input from another job: read as data through jq
# and passed as a single argument, the same discipline
# quality-mutation-monthly.yml's history job uses for its own per-package
# records.
for record in "${records[@]}"; do
	if ! numbers="$(jq -ec 'select(type == "object")' "${record}" 2>/dev/null)"; then
		echo "::warning::${record} is not a JSON object; skipping it." >&2
		continue
	fi
	printf '%s\n' "${numbers}"

	status="$(jq -r '.coverage_status // empty' "${record}")"
	target_name="$(jq -r '.target // empty' "${record}")"
	pkg="$(jq -r '.package // empty' "${record}")"
	profile="$(dirname "${record}")/fuzz-cover-${target_name}.out"
	if [ "${status}" = "ok" ] && [ -s "${profile}" ]; then
		profile_args+=("${profile}")
		[ -n "${pkg}" ] && printf '%s\n' "${pkg}" >>"${packages_file}"
	fi

	# jq itself only defaults a missing/null field (`// 0`); a present but
	# non-numeric field (a string like "bad", an object, an array) passes
	# `// 0` through unchanged, so the type is checked explicitly too and
	# anything that fails it becomes the literal string "bad" for
	# sanitize_int's digits-only case guard to catch.
	corpus_total="$(jq -r '(.corpus_total // 0) | if type == "number" then floor else "bad" end' "${record}" 2>/dev/null || echo bad)"
	corpus_total="$(sanitize_int "${corpus_total}")"
	corpus_total_all=$((corpus_total_all + corpus_total))

	new_interesting="$(jq -r '(.new_interesting // 0) | if type == "number" then floor else "bad" end' "${record}" 2>/dev/null || echo bad)"
	new_interesting="$(sanitize_int "${new_interesting}")"
	new_interesting_total=$((new_interesting_total + new_interesting))
done

profiles_merged="${#profile_args[@]}"
packages=0
if [ -s "${packages_file}" ]; then
	packages="$(sort -u "${packages_file}" | wc -l | tr -d ' ')"
fi

union_coverage_pct="null"
union_stmts_covered="null"
union_stmts_total="null"
if [ "${profiles_merged}" -gt 0 ]; then
	union_line="$(awk -f "${union_awk}" "${profile_args[@]}")"
	read -r union_coverage_pct union_stmts_covered union_stmts_total <<<"${union_line}" || true
fi

# True whenever a target's coverage could not be folded into the union — its
# record never arrived, or it scored anything other than "ok" — so a reader
# of the series can tell a lower union number apart from a run that simply
# measured fewer targets. When TARGETS_EXPECTED is absent (every leg failed,
# so no job output was published), targets_total falls back to the downloaded
# record count and partial behaves as it did before this field existed.
partial="false"
if [ "${profiles_merged}" -lt "${targets_total}" ]; then
	partial="true"
fi

jq -cn \
	--arg target "ALL" \
	--argjson union_coverage_pct "${union_coverage_pct}" \
	--argjson union_stmts_covered "${union_stmts_covered}" \
	--argjson union_stmts_total "${union_stmts_total}" \
	--argjson packages "${packages}" \
	--argjson profiles_merged "${profiles_merged}" \
	--argjson targets_total "${targets_total}" \
	--argjson records_downloaded "${records_downloaded}" \
	--argjson partial "${partial}" \
	--argjson corpus_total "${corpus_total_all}" \
	--argjson new_interesting_total "${new_interesting_total}" \
	'{
		scope: "union",
		target: $target,
		union_coverage_pct: $union_coverage_pct,
		union_stmts_covered: $union_stmts_covered,
		union_stmts_total: $union_stmts_total,
		packages: $packages,
		profiles_merged: $profiles_merged,
		targets_total: $targets_total,
		records_downloaded: $records_downloaded,
		partial: $partial,
		corpus_total: $corpus_total,
		new_interesting_total: $new_interesting_total
	}'
