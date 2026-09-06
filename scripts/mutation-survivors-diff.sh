#!/usr/bin/env bash
#
# PW-2.5. Compare the two most recent `mutation-survivors` quality-history
# records and report, per (package, source), which mutant identities were
# killed, which are new, and how many persisted -- or, when either run did
# not actually measure that package/source, say so instead of guessing.
#
# Usage: mutation-survivors-diff.sh [--package NAME] [--source gated|advisory]
#
#   --package NAME       only compare this package
#   --source gated|advisory   only compare this source
#
# Reads scripts/quality-history.sh mutation-survivors --last 2 --json, which
# is a depth=1 clone of the quality-history orphan branch -- no artifact
# download, no Gremlins re-run. MUTATION_SURVIVORS_RECORDS overrides that
# read with a file of JSONL records, so tests can feed a fixture without a
# clone.
#
# The `else` branch below is the point of this script, not a fallback. A run
# whose advisory legs all hit the runner's CPU-shutdown ceiling still
# produces a "missing" mutation-survivors entry (see
# scripts/ci/mutation-survivors-record.sh), and a naive set diff against that
# would report every surviving mutant from the other run as both a fresh
# kill and a fresh survivor. Anything not "measured" on both sides is
# reported as incomparable instead, with its own mutant count folded into
# `unknown` rather than into `killed` or `new`.

set -euo pipefail
export LC_ALL=C

die() {
	echo "mutation-survivors-diff: $*" >&2
	exit 1
}

usage() {
	echo "Usage: $0 [--package NAME] [--source gated|advisory]" >&2
}

package_filter=""
source_filter=""

while [ $# -gt 0 ]; do
	case "$1" in
	--package)
		[ $# -ge 2 ] || die "--package needs a name"
		package_filter="$2"
		shift 2
		;;
	--package=*)
		package_filter="${1#--package=}"
		shift
		;;
	--source)
		[ $# -ge 2 ] || die "--source needs gated or advisory"
		source_filter="$2"
		shift 2
		;;
	--source=*)
		source_filter="${1#--source=}"
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
		usage
		die "unexpected argument: $1"
		;;
	esac
done

case "${source_filter}" in
'' | gated | advisory) ;;
*) die "--source must be gated or advisory, got '${source_filter}'" ;;
esac

command -v jq >/dev/null 2>&1 || die "jq is required"

if [ -n "${MUTATION_SURVIVORS_RECORDS:-}" ]; then
	[ -f "${MUTATION_SURVIVORS_RECORDS}" ] ||
		die "MUTATION_SURVIVORS_RECORDS not found: ${MUTATION_SURVIVORS_RECORDS}"
	records="$(cat "${MUTATION_SURVIVORS_RECORDS}")"
else
	records="$(scripts/quality-history.sh mutation-survivors --last 2 --json)"
fi

[ -n "${records}" ] || die "no mutation-survivors records were read"

count="$(jq -s 'length' <<<"${records}")"
[ "${count}" -ge 2 ] ||
	die "need 2 mutation-survivors records to diff, found ${count}"

jq -s \
	--arg package "${package_filter}" \
	--arg source "${source_filter}" \
	'
    (map(.packages
         | map(select($package == "" or .name == $package))
         | map(select($source == "" or .source == $source))
         | map({key: (.name + "/" + .source), value: {state, ids: (.mutants | map(.f + ":" + .m + ":" + .a + ":" + (.o | tostring)))}})
         | from_entries)) as [$a, $b]
    | [($a | keys[]), ($b | keys[])] | flatten | unique
    | map(. as $k | {package: $k}
        + (if ($a[$k].state == "measured" and $b[$k].state == "measured")
           then {killed: ($a[$k].ids - $b[$k].ids), new: ($b[$k].ids - $a[$k].ids),
                 persisted: (($a[$k].ids) - (($a[$k].ids) - ($b[$k].ids)) | length)}
           else {comparable: false, was: $a[$k].state, now: $b[$k].state,
                 unknown: ((if $a[$k].state == "measured" then $a[$k].ids
                            elif $b[$k].state == "measured" then $b[$k].ids
                            else [] end) | length)} end))
    ' <<<"${records}"
