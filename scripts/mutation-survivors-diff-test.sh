#!/usr/bin/env bash
#
# Self-test for scripts/mutation-survivors-diff.sh (PW-2.5).
#
# Feeds two-line JSONL fixtures through MUTATION_SURVIVORS_RECORDS rather
# than a real quality-history clone, so this covers the jq program's own
# logic -- the `else` branch is the point, not the ordinary case: a package
# that went from "measured" to "missing" (the live 98-mutant advisory
# outage) must not read as 98 fresh kills, and a whole-file insertion that
# shifts every mutant's window must not read as 0 persisted survivors
# mislabelled as killed-and-new when nothing was actually fixed.

set -euo pipefail
export LC_ALL=C

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
diff_script="${repo_root}/scripts/mutation-survivors-diff.sh"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/portwing-mutation-survivors-diff.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

failures=0
fail() {
	echo "FAIL: $1" >&2
	failures=$((failures + 1))
}

mutant() {
	# f m s a o l c
	jq -cn --arg f "$1" --arg m "$2" --arg s "$3" --arg a "$4" --argjson o "$5" --argjson l "$6" --argjson c "$7" \
		'{f:$f, m:$m, s:$s, a:$a, o:$o, l:$l, c:$c}'
}

# --- measured -> missing: the live 98-mutant advisory outage -----------------
#
# Run A measured "server/advisory" with 2 survivors. Run B's advisory legs
# all hit the runner's CPU-shutdown ceiling, so the record script recorded
# "missing", not "zero-mutants" and not a re-measurement. A naive set diff
# would read run B's absent ids as 2 kills; this must instead say
# comparable:false and fold the 2 into "unknown".

fixture1="${test_root}/fixture1.jsonl"
{
	jq -cn --argjson mutants "[$(mutant f.go M1 L aaaaaaaaaaaa 0 10 1), $(mutant f.go M1 L bbbbbbbbbbbb 0 20 1)]" \
		--argjson w "[$(mutant w.go M3 L eeeeeeeeeeee 0 1 1)]" \
		'{schema:1, packages:[
            {name:"server", package:"./internal/server", source:"advisory", state:"measured", counts:{lived:2,not_covered:0}, mutants:$mutants},
            {name:"worker", package:"./internal/worker", source:"gated", state:"measured", counts:{lived:1,not_covered:0}, mutants:$w}
        ]}'
	jq -cn --argjson w "[$(mutant w.go M3 L eeeeeeeeeeee 0 1 1)]" \
		'{schema:1, packages:[
            {name:"server", package:"./internal/server", source:"advisory", state:"missing", counts:null, mutants:[]},
            {name:"worker", package:"./internal/worker", source:"gated", state:"measured", counts:{lived:1,not_covered:0}, mutants:$w}
        ]}'
} >"${fixture1}"

output="$(MUTATION_SURVIVORS_RECORDS="${fixture1}" bash "${diff_script}")"
row="$(jq -c '.[] | select(.package == "server/advisory")' <<<"${output}")"
[ -n "${row}" ] || fail "measured->missing must produce a server/advisory row (output: ${output})"
[ "$(jq -r '.comparable' <<<"${row}")" = "false" ] ||
	fail "measured->missing must be incomparable, not diffed as a set (row: ${row})"
[ "$(jq -r '.unknown' <<<"${row}")" = "2" ] ||
	fail "measured->missing must fold the prior run's 2 survivors into 'unknown', not 'killed' (row: ${row})"
[ "$(jq 'has("killed")' <<<"${row}")" = "false" ] ||
	fail "an incomparable row must not carry a 'killed' key at all (row: ${row})"
[ "$(jq -r '.was' <<<"${row}")" = "measured" ] && [ "$(jq -r '.now' <<<"${row}")" = "missing" ] ||
	fail "an incomparable row must say what each side actually was (row: ${row})"

# --- missing -> measured: the other direction of the same guard -------------
#
# A prior run's leg died first (recorded "missing"); this run measured it.
# This must be incomparable too, folding the CURRENT run's ids into
# "unknown" rather than reporting them all as "new".

fixture1b="${test_root}/fixture1b.jsonl"
{
	jq -cn '{schema:1, packages:[{name:"server", package:"./internal/server", source:"advisory", state:"missing", counts:null, mutants:[]}]}'
	jq -cn --argjson mutants "[$(mutant f.go M1 L aaaaaaaaaaaa 0 10 1), $(mutant f.go M1 L bbbbbbbbbbbb 0 20 1), $(mutant f.go M1 L cccccccccccc 0 30 1)]" \
		'{schema:1, packages:[{name:"server", package:"./internal/server", source:"advisory", state:"measured", counts:{lived:3,not_covered:0}, mutants:$mutants}]}'
} >"${fixture1b}"

output="$(MUTATION_SURVIVORS_RECORDS="${fixture1b}" bash "${diff_script}")"
row="$(jq -c '.[] | select(.package == "server/advisory")' <<<"${output}")"
[ "$(jq -r '.comparable' <<<"${row}")" = "false" ] ||
	fail "missing->measured must be incomparable too, not diffed as a set (row: ${row})"
[ "$(jq -r '.was' <<<"${row}")" = "missing" ] && [ "$(jq -r '.now' <<<"${row}")" = "measured" ] ||
	fail "an incomparable row must say what each side actually was, in either direction (row: ${row})"
[ "$(jq -r '.unknown' <<<"${row}")" = "3" ] ||
	fail "missing->measured must fold the current run's 3 mutants into 'unknown', not 'new' (row: ${row})"
[ "$(jq 'has("new")' <<<"${row}")" = "false" ] ||
	fail "an incomparable row must not carry a 'new' key at all (row: ${row})"

# --- measured -> measured, one mutant dropped: exactly one kill -------------

fixture2="${test_root}/fixture2.jsonl"
{
	jq -cn --argjson mutants "[$(mutant g.go M1 L aaaaaaaaaaaa 0 5 1), $(mutant g.go M1 L bbbbbbbbbbbb 0 9 1)]" \
		'{schema:1, packages:[{name:"edge", package:"./internal/edge", source:"gated", state:"measured", counts:{lived:2,not_covered:0}, mutants:$mutants}]}'
	jq -cn --argjson mutants "[$(mutant g.go M1 L aaaaaaaaaaaa 0 5 1)]" \
		'{schema:1, packages:[{name:"edge", package:"./internal/edge", source:"gated", state:"measured", counts:{lived:1,not_covered:0}, mutants:$mutants}]}'
} >"${fixture2}"

output="$(MUTATION_SURVIVORS_RECORDS="${fixture2}" bash "${diff_script}")"
row="$(jq -c '.[] | select(.package == "edge/gated")' <<<"${output}")"
[ "$(jq -r '.comparable // "unset"' <<<"${row}")" = "unset" ] ||
	fail "measured->measured must not carry a 'comparable' key (row: ${row})"
[ "$(jq '.killed | length' <<<"${row}")" = "1" ] ||
	fail "dropping one of two mutants must report exactly one kill (row: ${row})"
[ "$(jq -r '.killed[0]' <<<"${row}")" = "g.go:M1:bbbbbbbbbbbb:0" ] ||
	fail "the killed id must be the one that dropped out (row: ${row})"
[ "$(jq '.new | length' <<<"${row}")" = "0" ] ||
	fail "dropping a mutant must not report a new one (row: ${row})"
[ "$(jq -r '.persisted' <<<"${row}")" = "1" ] ||
	fail "the surviving mutant must be counted persisted (row: ${row})"

# --- a whole-file insertion above every mutant: zero kills, zero new --------
#
# Every mutant's line number shifts down by the same amount a real editor
# insertion would produce (the two sides deliberately use DIFFERENT `l`
# values), and the 5-line window re-identifies each one at its new line
# (this is the documented open risk: a real code change here would read the
# same way). What this asserts is narrower: an insertion that does not
# touch any mutant's own window content must not manufacture spurious kills
# or news out of thin air when the identity (f, m, a, o) is unchanged,
# regardless of where the line moved to.

fixture3="${test_root}/fixture3.jsonl"
{
	jq -cn --argjson mutants "[$(mutant h.go M2 U cccccccccccc 0 30 1), $(mutant h.go M2 U dddddddddddd 1 31 4)]" \
		'{schema:1, packages:[{name:"metrics", package:"./internal/metrics", source:"gated", state:"measured", counts:{lived:0,not_covered:2}, mutants:$mutants}]}'
	jq -cn --argjson mutants "[$(mutant h.go M2 U cccccccccccc 0 35 1), $(mutant h.go M2 U dddddddddddd 1 36 4)]" \
		'{schema:1, packages:[{name:"metrics", package:"./internal/metrics", source:"gated", state:"measured", counts:{lived:0,not_covered:2}, mutants:$mutants}]}'
} >"${fixture3}"

output="$(MUTATION_SURVIVORS_RECORDS="${fixture3}" bash "${diff_script}")"
row="$(jq -c '.[] | select(.package == "metrics/gated")' <<<"${output}")"
[ "$(jq '.killed | length' <<<"${row}")" = "0" ] || fail "an unchanged run must report zero kills (row: ${row})"
[ "$(jq '.new | length' <<<"${row}")" = "0" ] || fail "an unchanged run must report zero new mutants (row: ${row})"
[ "$(jq -r '.persisted' <<<"${row}")" = "2" ] || fail "an unchanged run must persist both mutants (row: ${row})"

# --- filters -------------------------------------------------------------
#
# fixture1 carries two packages (server/advisory and worker/gated), so an
# unfiltered diff has two rows and --package/--source actually have
# something to filter out; against a single-package fixture these flags
# would pass even if they were silently no-ops.

output="$(MUTATION_SURVIVORS_RECORDS="${fixture1}" bash "${diff_script}")"
[ "$(jq 'length' <<<"${output}")" = "2" ] ||
	fail "the filter fixture must itself carry two rows unfiltered, got $(jq 'length' <<<"${output}")"

output="$(MUTATION_SURVIVORS_RECORDS="${fixture1}" bash "${diff_script}" --package server)"
[ "$(jq 'length' <<<"${output}")" = "1" ] || fail "--package must restrict the rows to that package"
[ "$(jq -r '.[0].package' <<<"${output}")" = "server/advisory" ] || fail "--package must keep the requested package's row"

output="$(MUTATION_SURVIVORS_RECORDS="${fixture1}" bash "${diff_script}" --package server --source gated)"
[ "$(jq 'length' <<<"${output}")" = "0" ] ||
	fail "--source gated must drop a package that only has an advisory row"

output="$(MUTATION_SURVIVORS_RECORDS="${fixture1}" bash "${diff_script}" --source gated)"
[ "$(jq 'length' <<<"${output}")" = "1" ] || fail "--source gated must keep only the gated row"
[ "$(jq -r '.[0].package' <<<"${output}")" = "worker/gated" ] || fail "--source gated must keep worker/gated specifically"

# --- refusals ----------------------------------------------------------------

set +e
bash "${diff_script}" --source bogus >"${test_root}/bad-source.log" 2>&1
status=$?
set -e
[ "${status}" -ne 0 ] || fail "--source must reject a value other than gated or advisory"
grep -Fq "must be gated or advisory" "${test_root}/bad-source.log" ||
	fail "--source must name the two valid values in its refusal"

single="${test_root}/single.jsonl"
jq -cn '{schema:1, packages:[]}' >"${single}"
set +e
MUTATION_SURVIVORS_RECORDS="${single}" bash "${diff_script}" >"${test_root}/single.log" 2>&1
status=$?
set -e
[ "${status}" -ne 0 ] || fail "one record must not be enough to diff"
grep -Fq "need 2" "${test_root}/single.log" ||
	fail "a too-short history must say it needs 2 records (output: $(cat "${test_root}/single.log"))"

if [ "${failures}" -ne 0 ]; then
	echo "${failures} mutation survivors diff check(s) failed" >&2
	exit 1
fi

echo "Mutation survivors diff checks passed."
