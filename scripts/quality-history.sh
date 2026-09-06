#!/usr/bin/env bash
#
# Read the quality-lane trend series that scripts/ci/quality-history-append.sh
# writes to the `quality-history` orphan branch.
#
# Usage: scripts/quality-history.sh <lane> [--last N] [--json]
#
#   <lane>     soak | mutation | mutation-survivors | fuzz-nightly | bench
#   --last N   show the N most recent records (default 20, 0 for all)
#   --json     print the raw JSONL instead of a table
#
# The branch is fetched, not checked out: nothing about the working tree
# changes, and the series never lands in a released tree. A shallow fetch is
# enough because each record is a whole line in the tip's file — the commits in
# between carry no information the tip doesn't already have.
#
# Environment overrides mirror the appender's:
#   QUALITY_HISTORY_REMOTE   remote to read from (default origin)
#   QUALITY_HISTORY_BRANCH   branch to read (default quality-history)

set -euo pipefail
export LC_ALL=C

die() {
	echo "quality-history: $*" >&2
	exit 1
}

usage() {
	echo "Usage: $0 <lane> [--last N] [--json]" >&2
	echo "  lane: soak | mutation | mutation-survivors | fuzz-nightly | bench" >&2
}

lane=""
last=20
as_json=no

while [ $# -gt 0 ]; do
	case "$1" in
	--last)
		[ $# -ge 2 ] || die "--last needs a count"
		last="$2"
		shift 2
		;;
	--last=*)
		last="${1#--last=}"
		shift
		;;
	--json)
		as_json=yes
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		usage
		die "unknown option: $1"
		;;
	*)
		[ -z "${lane}" ] || die "only one lane at a time (got '${lane}' and '$1')"
		lane="$1"
		shift
		;;
	esac
done

[ -n "${lane}" ] || {
	usage
	die "no lane given"
}

case "${lane}" in
soak | mutation | mutation-survivors | fuzz-nightly | bench) ;;
*) die "unknown lane '${lane}'; expected one of soak, mutation, mutation-survivors, fuzz-nightly, bench" ;;
esac

case "${last}" in
'' | *[!0-9]*) die "--last must be a non-negative integer, got '${last}'" ;;
esac

command -v jq >/dev/null 2>&1 || die "jq is required"

branch="${QUALITY_HISTORY_BRANCH:-quality-history}"
remote="${QUALITY_HISTORY_REMOTE:-}"
if [ -z "${remote}" ]; then
	git rev-parse --git-dir >/dev/null 2>&1 || die "not inside a git repository"
	remote="$(git remote get-url origin 2>/dev/null)" ||
		die "no QUALITY_HISTORY_REMOTE set and this repository has no 'origin' remote"
fi

# Read through a throwaway clone rather than fetching into the caller's
# repository: a shallow fetch of an unrelated orphan branch would leave a
# .git/shallow entry and a stray ref behind in a working clone, and this is a
# read-only query.
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/quality-history-read.XXXXXX")"
trap 'rm -rf "${work_dir}"' EXIT

git init --quiet "${work_dir}/repo"
git -C "${work_dir}/repo" remote add origin "${remote}"
if ! git -C "${work_dir}/repo" fetch --quiet --depth=1 origin \
	"+refs/heads/${branch}:refs/heads/${branch}" 2>/dev/null; then
	die "could not fetch ${branch} from ${remote}; no lane has recorded a run yet, or the remote is unreachable"
fi

if ! records="$(git -C "${work_dir}/repo" cat-file blob "${branch}:${lane}.jsonl" 2>/dev/null)"; then
	die "${branch} has no ${lane}.jsonl yet; that lane has not recorded a scheduled run"
fi

[ -n "${records}" ] || die "${lane}.jsonl is empty"

if [ "${last}" -gt 0 ]; then
	records="$(printf '%s\n' "${records}" | tail -n "${last}")"
fi

if [ "${as_json}" = "yes" ]; then
	printf '%s\n' "${records}"
	exit 0
fi

# Columns are the union of every key present in the selected records, with the
# envelope fields pulled to the front so the table reads chronologically
# left-to-right. Deriving them rather than hard-coding a list means a lane that
# starts recording a new number shows it here without a second edit, and a lane
# that stops recording one leaves a visibly empty column instead of silently
# shifting every value one place left.
#
# A missing or null value prints as "-" rather than as an empty field, because
# `column -t` collapses runs of the separator: an empty cell would shift every
# value after it one column to the left and quietly mislabel the row.
printf '%s\n' "${records}" |
	jq -rs '
        . as $rows
        | ([$rows[] | keys_unsorted[]] | unique) as $all
        | (["timestamp", "run_id", "run_attempt", "sha"]
           | map(select(. as $k | $all | index($k)))) as $lead
        | ($lead + ($all - $lead - ["lane", "workflow", "event", "ref", "run_number"])) as $cols
        | ($cols | @tsv),
          ($rows[]
           | [$cols[] as $c
              | (.[$c]
                 | if . == null or . == "" then "-"
                   elif $c == "sha" then tostring[0:8]
                   elif (type == "array" or type == "object") then ("[" + (length | tostring) + "]")
                   else tostring end)]
           | @tsv)
    ' |
	if command -v column >/dev/null 2>&1; then
		column -t -s "$(printf '\t')"
	else
		cat
	fi
