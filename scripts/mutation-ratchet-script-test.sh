#!/usr/bin/env bash
set -euo pipefail

# Self-test for scripts/ci/mutation-ratchet.sh.
#
# Each case builds a throwaway records directory (one subdirectory per
# package, holding a quality-history-record.json the way the downloaded
# mutation-history-*-<run_id> artifacts do) and, where needed, a
# quality-history mutation.jsonl, then runs the real script against them.

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
ratchet="${repository_root}/scripts/ci/mutation-ratchet.sh"
gate="${repository_root}/scripts/ci/mutation-gate.sh"
fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT

failures=0

fail() {
	echo "FAIL: $1" >&2
	failures=$((failures + 1))
}

expect_status() {
	[ "$1" = "$2" ] || fail "$3: expected exit $1, got $2"
}

# Write one package's quality-history-record.json fixture.
#
# write_record <dir> <name> <package> <mode> <outcome> <timed_out> \
#   <efficacy_floor> <efficacy> <mcover_floor> <mcover> \
#   [mutants_total] [killed] [lived]
#
# The last three are optional and default to 0/null/null, matching a record
# with no timed-out mutants: mutants_total feeds the PW-7.36 tolerance ratio,
# killed/lived feed the efficacy-pool discount. Existing call sites that omit
# them are unaffected, since both only apply when timed_out > 0.
write_record() {
	local dir="$1" name="$2" package="$3" mode="$4" outcome="$5" timed_out="$6"
	local efficacy_floor="$7" efficacy="$8" mcover_floor="$9" mcover="${10}"
	local mutants_total="${11:-0}" killed="${12:-null}" lived="${13:-null}"
	mkdir -p "${dir}"
	jq -n \
		--arg name "${name}" --arg package "${package}" --arg mode "${mode}" --arg outcome "${outcome}" \
		--argjson timed_out "${timed_out}" \
		--argjson efficacy_floor "${efficacy_floor}" --argjson efficacy "${efficacy}" \
		--argjson mcover_floor "${mcover_floor}" --argjson mcover "${mcover}" \
		--argjson mutants_total "${mutants_total}" \
		--argjson killed "${killed}" --argjson lived "${lived}" \
		'{
            name: $name, package: $package, mode: $mode, outcome: $outcome, timed_out: $timed_out,
            efficacy_floor: $efficacy_floor, efficacy: $efficacy,
            mutator_coverage_floor: $mcover_floor, mutator_coverage: $mcover,
            mutants_total: $mutants_total, killed: $killed, lived: $lived
        }' >"${dir}/quality-history-record.json"
}

# Write one history.jsonl row for a package. run_id defaults to omitted, and
# timed_out/killed/lived default to 0/null/null (no timed-out mutants), so
# existing call sites are unaffected.
write_history_row() {
	local name="$1" efficacy="$2" mcover="$3" run_id="${4:-}"
	local timed_out="${5:-0}" killed="${6:-null}" lived="${7:-null}"
	jq -cn \
		--arg name "${name}" --argjson efficacy "${efficacy}" --argjson mcover "${mcover}" \
		--arg run_id "${run_id}" \
		--argjson timed_out "${timed_out}" --argjson killed "${killed}" --argjson lived "${lived}" \
		'{name: $name, mode: "gated", outcome: "success", efficacy: $efficacy, mutator_coverage: $mcover,
		  timed_out: $timed_out, killed: $killed, lived: $lived}
		 + (if $run_id == "" then {} else {run_id: $run_id} end)'
}

# --- (1) a measured value below the current floor produces no proposal ------
#
# Both metrics measure below their own floor. min(pool) is then below the
# floor too, and basis - buffer is lower still, so the "proposed > floor"
# guard rejects it outright.
case1="${fixture}/case1"
mkdir -p "${case1}/records/regressed"
write_record "${case1}/records/regressed" "regressed" "./internal/regressed" \
	"gated" "success" 0 90.00 85.00 90.00 88.00
: >"${case1}/history.jsonl"

set +e
bash "${ratchet}" "${case1}/records" "${case1}/history.jsonl" "${case1}/out.json"
status=$?
set -e
expect_status 0 "${status}" "a below-floor measurement still exits 0"
count="$(jq '.proposals | length' "${case1}/out.json")"
[ "${count}" -eq 0 ] || fail "a below-floor measurement must produce no proposal, got ${count}"

# --- (2) proposed < measured always, proposed > current_floor always --------
#
# A package with real headroom and some history spread, so it actually
# proposes on both metrics.
case2="${fixture}/case2"
mkdir -p "${case2}/records/edge"
write_record "${case2}/records/edge" "edge" "./internal/edge" \
	"gated" "success" 0 74.73 85.00 91.00 95.00
{
	write_history_row "edge" 81.90 92.00
	write_history_row "edge" 83.00 93.00
} >"${case2}/history.jsonl"

bash "${ratchet}" "${case2}/records" "${case2}/history.jsonl" "${case2}/out.json"
proposal_count="$(jq '.proposals | length' "${case2}/out.json")"
[ "${proposal_count}" -gt 0 ] || fail "case2 fixture should have produced at least one proposal to test"

# basis is min(pool), not the run's own measurement or the best history row.
# efficacy pool = [85.00 (measured), 81.90, 83.00] (the two history rows);
# min(pool) = 81.90.
basis="$(jq -r '.proposals[] | select(.metric == "efficacy") | .basis' "${case2}/out.json")"
[ "${basis}" = "81.90" ] || fail "case2 efficacy basis must equal min(pool) = 81.90, got ${basis}"

bad="$(jq -r '
    .proposals[]
    | select(.proposed <= .current_floor)
    | .metric
' "${case2}/out.json")"
[ -z "${bad}" ] || fail "proposed must always be > current_floor, violated for: ${bad}"

# measured is not carried in the proposal, but proposed is derived from
# basis (min of the pool, itself <= the measured value) minus a strictly
# positive buffer, so proposed < basis <= measured always. Assert the
# stronger, checkable form: proposed < basis.
bad="$(jq -r '
    .proposals[]
    | select(.proposed >= .basis)
    | .metric
' "${case2}/out.json")"
[ -z "${bad}" ] || fail "proposed must always be < basis (and thus < measured), violated for: ${bad}"

# --- (3) min(pool) drives it, not the current measurement -------------------
#
# Pool is exactly [91.11, 92.00] against a floor of 90.00: basis 91.11,
# spread 0.89, buffer max(1.0, 0.89) = 1.00, proposed 90.11, gain 0.11.
# Below the 2.00 minimum gain, so no proposal for efficacy.
case3="${fixture}/case3"
mkdir -p "${case3}/records/pool-driven"
write_record "${case3}/records/pool-driven" "pool-driven" "./internal/pool-driven" \
	"gated" "success" 0 90.00 92.00 0 0
write_history_row "pool-driven" 91.11 0 >"${case3}/history.jsonl"

bash "${ratchet}" "${case3}/records" "${case3}/history.jsonl" "${case3}/out.json"
efficacy_proposals="$(jq '[.proposals[] | select(.metric == "efficacy")] | length' "${case3}/out.json")"
[ "${efficacy_proposals}" -eq 0 ] ||
	fail "pool [91.11, 92.00] against floor 90.00 must not clear the 2.00 minimum gain"

# --- (4) mode/timed_out skips, each with a reason ----------------------------
#
# "to" times out 5 of a 5-mutant run (no mutants_total given, defaults to 0,
# so mutants_attempted = 0 + 5 = 5): a 100% timed-out ratio is over the 5%
# tolerance from every angle, so it still skips, now as
# "timed-out-over-tolerance" with the counts that produced the decision.
case4="${fixture}/case4"
write_record "${case4}/records/zm" "zm" "./internal/zm" "zero-mutants" "success" 0 0 0 0 0
write_record "${case4}/records/um" "um" "./internal/um" "unmeasured" "skipped" 0 0 0 0 0
write_record "${case4}/records/up" "up" "./internal/up" "unparseable" "failure" 0 0 0 0 0
write_record "${case4}/records/to" "to" "./internal/to" "gated" "success" 5 80.00 85.00 80.00 85.00
: >"${case4}/history.jsonl"

bash "${ratchet}" "${case4}/records" "${case4}/history.jsonl" "${case4}/out.json"
while read -r pkg_name expected_reason; do
	reason="$(jq -r --arg n "${pkg_name}" '.skipped[] | select(.name == $n) | .reason' "${case4}/out.json")"
	[ "${reason}" = "${expected_reason}" ] ||
		fail "${pkg_name}: expected skip reason '${expected_reason}', got '${reason}'"
done <<-EOF
	zm zero-mutants
	um unmeasured
	up unparseable
	to timed-out-over-tolerance
EOF
proposal_count="$(jq '.proposals | length' "${case4}/out.json")"
[ "${proposal_count}" -eq 0 ] || fail "every package in case4 should have been skipped, not proposed"

to_timed_out="$(jq -r '.skipped[] | select(.name == "to") | .timed_out' "${case4}/out.json")"
to_mutants="$(jq -r '.skipped[] | select(.name == "to") | .mutants' "${case4}/out.json")"
[ "${to_timed_out}" = "5" ] || fail "case4 'to' skip must record timed_out=5, got ${to_timed_out}"
[ "${to_mutants}" = "5" ] || fail "case4 'to' skip must record mutants=5 (0 mutants_total + 5 timed_out), got ${to_mutants}"

# --- (5) the proposed value re-fed to mutation-gate.sh against its own basis -
case5="${fixture}/case5"
mkdir -p "${case5}/records/roundtrip"
write_record "${case5}/records/roundtrip" "roundtrip" "./internal/roundtrip" \
	"gated" "success" 0 74.73 85.00 91.00 95.00
{
	write_history_row "roundtrip" 81.90 92.00
	write_history_row "roundtrip" 83.00 93.00
} >"${case5}/history.jsonl"

bash "${ratchet}" "${case5}/records" "${case5}/history.jsonl" "${case5}/out.json"
proposed="$(jq -r '.proposals[] | select(.metric == "efficacy") | .proposed' "${case5}/out.json")"
basis="$(jq -r '.proposals[] | select(.metric == "efficacy") | .basis' "${case5}/out.json")"
[ -n "${proposed}" ] || fail "case5 fixture should have produced an efficacy proposal to round-trip"

report="${case5}/basis-report.json"
jq -n --argjson efficacy "${basis}" \
	'{test_efficacy: $efficacy, mutations_coverage: 100, mutants_killed: 1, mutants_lived: 0}' >"${report}"

set +e
bash "${gate}" "${report}" "${proposed}" 0
gate_status=$?
set -e
expect_status 0 "${gate_status}" "a run measuring the pool's own basis (${basis}) must clear the proposed floor (${proposed})"

# --- (6) this run's own history row must not evict the oldest prior --------
#
# The ratchet job needs [gremlins, history], so by the time it checks out
# quality-history the history job has already appended this run's row for
# every package. Without the run_id filter, the newest slot in the 6-row
# window holds a duplicate of the current measurement instead of the oldest
# real prior (70.00), raising basis from 70.00 to 90.00 and turning a
# correctly-conservative "no proposal" into a proposal of 84.00.
case6="${fixture}/case6"
mkdir -p "${case6}/records/edge"
write_record "${case6}/records/edge" "edge" "./internal/edge" \
	"gated" "success" 0 74.73 96.00 91.00 99.00
{
	write_history_row "edge" 70.00 99.00 r1
	write_history_row "edge" 90.00 99.00 r2
	write_history_row "edge" 90.00 99.00 r3
	write_history_row "edge" 90.00 99.00 r4
	write_history_row "edge" 90.00 99.00 r5
	write_history_row "edge" 90.00 99.00 r6
	write_history_row "edge" 96.00 99.00 CURRENT
} >"${case6}/history.jsonl"

GITHUB_RUN_ID=CURRENT bash "${ratchet}" "${case6}/records" "${case6}/history.jsonl" "${case6}/out.json"
efficacy_proposals="$(jq '[.proposals[] | select(.metric == "efficacy")] | length' "${case6}/out.json")"
[ "${efficacy_proposals}" -eq 0 ] ||
	fail "this run's own history row must not evict the oldest prior (70.00) from the 6-row window"

# The same fixture's mutator_coverage side is an all-99 pool against a floor
# of 91.00: basis 99.00, spread 0, buffer max(1.0, 0) = 1.00, proposed 98.00,
# gain 7.00 -- clears the 2.00 minimum, so this metric does propose. Assert
# on it directly: dropping the metric loop's mutator_coverage iteration
# entirely (or its floor2/gain arithmetic) would silently make this pass by
# never emitting anything for the metric, so check both that exactly one
# proposal exists and that its value is the one the arithmetic above demands.
mcover_proposals="$(jq -c '[.proposals[] | select(.metric == "mutator_coverage")]' "${case6}/out.json")"
mcover_count="$(jq 'length' <<<"${mcover_proposals}")"
[ "${mcover_count}" -eq 1 ] ||
	fail "case6 must produce exactly one mutator_coverage proposal, got ${mcover_count}"
mcover_proposed="$(jq -r '.[0].proposed' <<<"${mcover_proposals}")"
[ "${mcover_proposed}" = "98.00" ] ||
	fail "case6 mutator_coverage proposed must be 98.00 (basis 99.00 - buffer 1.00), got ${mcover_proposed}"

# --- (7) samples counts distinct runs, not this run counted twice ----------
case7="${fixture}/case7"
mkdir -p "${case7}/records/dupe"
write_record "${case7}/records/dupe" "dupe" "./internal/dupe" \
	"gated" "success" 0 74.73 90.00 0 0
{
	write_history_row "dupe" 89.00 0 r1
	write_history_row "dupe" 90.00 0 CURRENT
} >"${case7}/history.jsonl"

GITHUB_RUN_ID=CURRENT bash "${ratchet}" "${case7}/records" "${case7}/history.jsonl" "${case7}/out.json"
samples="$(jq -r '.proposals[] | select(.metric == "efficacy") | .samples' "${case7}/out.json")"
[ "${samples}" = "2" ] ||
	fail "samples must count this run plus 1 real prior, not a duplicate of itself, got samples=${samples}"

# --- (8) history rows written before the run_id field existed still count --
#
# Guards against over-filtering: a row with no run_id must not be treated as
# "this run" just because GITHUB_RUN_ID is set.
case8="${fixture}/case8"
mkdir -p "${case8}/records/legacy"
write_record "${case8}/records/legacy" "legacy" "./internal/legacy" \
	"gated" "success" 0 74.73 96.00 0 0
{
	write_history_row "legacy" 95.00 0
	write_history_row "legacy" 95.00 0
} >"${case8}/history.jsonl"

GITHUB_RUN_ID=CURRENT bash "${ratchet}" "${case8}/records" "${case8}/history.jsonl" "${case8}/out.json"
samples="$(jq -r '.proposals[] | select(.metric == "efficacy") | .samples' "${case8}/out.json")"
[ "${samples}" = "3" ] ||
	fail "history rows without a run_id must survive the current-run filter, got samples=${samples}"

# --- (9) min(pool) as basis, not max(pool) -----------------------------------
#
# case3 above (pool [91.11, 92.00], floor 90.00) doesn't discriminate: both a
# min-based and a max-based basis reject it, so a regression from min(pool)
# to max(pool) would pass case3 silently. This fixture is chosen so the two
# bases disagree on whether to propose at all.
#
# Pool = [97.00 (measured), 93.00 (one history row)], floor 90.00.
#
#   min-based (what the script does):
#     basis  = min(97.00, 93.00)          = 93.00
#     spread = max - min = 97.00 - 93.00  =  4.00
#     buffer = max(1.0, 4.00)             =  4.00
#     proposed = floor2(93.00 - 4.00)     = 89.00
#     89.00 <= floor 90.00 -> guard rejects it -> no proposal.
#
#   max-based (what a regression would do, NOT asserted as correct):
#     basis  = max(97.00, 93.00)          = 97.00
#     buffer = max(1.0, 4.00)             =  4.00
#     proposed = floor2(97.00 - 4.00)     = 93.00
#     gain = 93.00 - 90.00 = 3.00 >= 2.00 and 93.00 > 90.00 -> would propose.
#
# The two bases disagree (no proposal vs. a proposal), so this fixture
# catches a min-to-max regression that case3 cannot.
case9="${fixture}/case9"
mkdir -p "${case9}/records/spread"
write_record "${case9}/records/spread" "spread" "./internal/spread" \
	"gated" "success" 0 90.00 97.00 0 0
write_history_row "spread" 93.00 0 >"${case9}/history.jsonl"

bash "${ratchet}" "${case9}/records" "${case9}/history.jsonl" "${case9}/out.json"
efficacy_proposals="$(jq '[.proposals[] | select(.metric == "efficacy")] | length' "${case9}/out.json")"
[ "${efficacy_proposals}" -eq 0 ] ||
	fail "pool [97.00, 93.00] against floor 90.00 must use min(pool) = 93.00 as basis (no proposal), not max(pool) = 97.00"

# --- (10) PW-7.36: exactly 5% timed-out is tolerated, not skipped -----------
#
# 5 timed out of 95 + 5 = 100 attempted: timed_out*100 (500) <=
# mutants_attempted*5 (500), so the <= boundary itself must not skip.
case10="${fixture}/case10"
mkdir -p "${case10}/records/exactly5"
write_record "${case10}/records/exactly5" "exactly5" "./internal/exactly5" \
	"gated" "success" 5 70.00 90.00 70.00 90.00 95
: >"${case10}/history.jsonl"

bash "${ratchet}" "${case10}/records" "${case10}/history.jsonl" "${case10}/out.json"
skip_reason="$(jq -r '.skipped[] | select(.name == "exactly5") | .reason' "${case10}/out.json")"
[ -z "${skip_reason}" ] || fail "exactly 5% timed-out must not skip, got reason '${skip_reason}'"
proposal_count="$(jq '[.proposals[] | select(.name == "exactly5")] | length' "${case10}/out.json")"
[ "${proposal_count}" -gt 0 ] || fail "exactly5 fixture should have produced at least one proposal to test tolerance"
recorded_timed_out="$(jq -r '.proposals[] | select(.name == "exactly5" and .metric == "efficacy") | .timed_out' "${case10}/out.json")"
recorded_mutants="$(jq -r '.proposals[] | select(.name == "exactly5" and .metric == "efficacy") | .mutants' "${case10}/out.json")"
[ "${recorded_timed_out}" = "5" ] || fail "exactly5 proposal must record timed_out=5, got ${recorded_timed_out}"
[ "${recorded_mutants}" = "100" ] || fail "exactly5 proposal must record mutants=100 (95 + 5), got ${recorded_mutants}"

# --- (11) PW-7.36: just over 5% timed-out still skips ------------------------
#
# 51 timed out of 949 + 51 = 1000 attempted is 5.1%, just over the tolerance:
# timed_out*100 (5100) > mutants_attempted*5 (5000).
case11="${fixture}/case11"
mkdir -p "${case11}/records/justover"
write_record "${case11}/records/justover" "justover" "./internal/justover" \
	"gated" "success" 51 70.00 90.00 70.00 90.00 949
: >"${case11}/history.jsonl"

bash "${ratchet}" "${case11}/records" "${case11}/history.jsonl" "${case11}/out.json"
skip_reason="$(jq -r '.skipped[] | select(.name == "justover") | .reason' "${case11}/out.json")"
[ "${skip_reason}" = "timed-out-over-tolerance" ] ||
	fail "5.1% timed-out must skip as timed-out-over-tolerance, got '${skip_reason}'"
recorded_timed_out="$(jq -r '.skipped[] | select(.name == "justover") | .timed_out' "${case11}/out.json")"
recorded_mutants="$(jq -r '.skipped[] | select(.name == "justover") | .mutants' "${case11}/out.json")"
[ "${recorded_timed_out}" = "51" ] || fail "justover skip must record timed_out=51, got ${recorded_timed_out}"
[ "${recorded_mutants}" = "1000" ] || fail "justover skip must record mutants=1000 (949 + 51), got ${recorded_mutants}"
proposal_count="$(jq '[.proposals[] | select(.name == "justover")] | length' "${case11}/out.json")"
[ "${proposal_count}" -eq 0 ] || fail "justover must not produce a proposal once skipped"

# --- (12) 0 timed-out records timed_out=0 and is unaffected ------------------
case12="${fixture}/case12"
mkdir -p "${case12}/records/notimeouts"
write_record "${case12}/records/notimeouts" "notimeouts" "./internal/notimeouts" \
	"gated" "success" 0 70.00 90.00 70.00 90.00
: >"${case12}/history.jsonl"

bash "${ratchet}" "${case12}/records" "${case12}/history.jsonl" "${case12}/out.json"
recorded_timed_out="$(jq -r '.proposals[] | select(.name == "notimeouts" and .metric == "efficacy") | .timed_out' "${case12}/out.json")"
recorded_mutants="$(jq -r '.proposals[] | select(.name == "notimeouts" and .metric == "efficacy") | .mutants' "${case12}/out.json")"
[ "${recorded_timed_out}" = "0" ] || fail "notimeouts proposal must record timed_out=0, got ${recorded_timed_out}"
[ "${recorded_mutants}" = "0" ] || fail "notimeouts proposal must record mutants=0 (no mutants_total given), got ${recorded_mutants}"

# --- (13) PW-7.36: the timed-out discount never raises the proposed value ---
#
# killed=90, lived=5, timed_out=5 (100 attempted, exactly the 5% boundary, so
# this package is tolerated, not skipped). Gremlins' own killed/(killed+lived)
# would report 90/95 = 94.7368...%, recorded here as the raw "efficacy" the
# package measured (94.74, matching Gremlins' own "%.2f" rounding) -- but the
# pool must use the discounted value instead: 100 * killed / (killed + lived +
# timed_out) = 100 * 90 / 100 = 90.00, never the raw 94.74.
#
# No history row, so the pool holds only this run's value and basis equals it
# directly: buffer = max(1.0, spread 0, seed 0) = 1.00.
#
#   with the discount (what the script must do):
#     basis = 90.00, proposed = floor2(90.00 - 1.00) = 89.00
#   without it (hand-computed here, NOT asserted as correct):
#     basis = 94.74, proposed = floor2(94.74 - 1.00) = 93.74
#
# 89.00 <= 93.74 is the invariant this fixture exists to check: the
# timed-out discount can only pull the proposed value down, never up.
case13="${fixture}/case13"
mkdir -p "${case13}/records/timedadjust"
write_record "${case13}/records/timedadjust" "timedadjust" "./internal/timedadjust" \
	"gated" "success" 5 70.00 94.74 70.00 90.00 95 90 5
: >"${case13}/history.jsonl"

bash "${ratchet}" "${case13}/records" "${case13}/history.jsonl" "${case13}/out.json"
efficacy_proposal="$(jq -c '.proposals[] | select(.name == "timedadjust" and .metric == "efficacy")' "${case13}/out.json")"
[ -n "${efficacy_proposal}" ] || fail "timedadjust fixture should have produced an efficacy proposal to check"

basis="$(jq -r '.basis' <<<"${efficacy_proposal}")"
proposed="$(jq -r '.proposed' <<<"${efficacy_proposal}")"
measured="$(jq -r '.measured' <<<"${efficacy_proposal}")"

[ "${basis}" = "90.00" ] || fail "timedadjust basis must be the discounted 90.00 (killed/(killed+lived+timed_out)), got ${basis}"
[ "${measured}" = "94.74" ] || fail "timedadjust measured must stay the raw Gremlins value 94.74, got ${measured}"

proposed_without_discount="$(awk 'BEGIN { printf "%.2f", int(((94.74 - 1.00) * 100) + 0.0000001) / 100 }')"
awk -v with="${proposed}" -v without="${proposed_without_discount}" 'BEGIN { exit !(with <= without) }' ||
	fail "timedadjust proposed (${proposed}) must never exceed what it would be without the discount (${proposed_without_discount})"
[ "${proposed}" = "89.00" ] || fail "timedadjust proposed must be 89.00 (basis 90.00 - buffer 1.00), got ${proposed}"

# --- (14) an undiscountable timed-out history row is dropped, not raw -------
#
# A history row that timed out mutants but carries no killed/lived cannot be
# recomputed as killed/(killed+lived+timed_out), so it cannot be discounted
# the way PW-7.36 requires. Falling back to its raw (undiscounted) efficacy
# would let it enter the pool anyway; the fix drops it from the pool
# entirely instead, so its presence in history.jsonl must change nothing:
# the proposal from a history file holding only that row must be identical
# to the proposal from an empty history file, and the row must be counted
# in discarded_rows.
case14a="${fixture}/case14a"
mkdir -p "${case14a}/records/discarded"
write_record "${case14a}/records/discarded" "discarded" "./internal/discarded" \
	"gated" "success" 0 70.00 90.00 0 0
write_history_row "discarded" 55.00 60.00 "" 5 >"${case14a}/history.jsonl"

case14b="${fixture}/case14b"
mkdir -p "${case14b}/records/discarded"
write_record "${case14b}/records/discarded" "discarded" "./internal/discarded" \
	"gated" "success" 0 70.00 90.00 0 0
: >"${case14b}/history.jsonl"

bash "${ratchet}" "${case14a}/records" "${case14a}/history.jsonl" "${case14a}/out.json"
bash "${ratchet}" "${case14b}/records" "${case14b}/history.jsonl" "${case14b}/out.json"

discarded_rows="$(jq -r '.proposals[] | select(.metric == "efficacy") | .discarded_rows' "${case14a}/out.json")"
[ "${discarded_rows}" = "1" ] || fail "case14a must count the undiscountable row in discarded_rows, got ${discarded_rows}"

basis_with="$(jq -r '.proposals[] | select(.metric == "efficacy") | .basis' "${case14a}/out.json")"
proposed_with="$(jq -r '.proposals[] | select(.metric == "efficacy") | .proposed' "${case14a}/out.json")"
basis_without="$(jq -r '.proposals[] | select(.metric == "efficacy") | .basis' "${case14b}/out.json")"
proposed_without="$(jq -r '.proposals[] | select(.metric == "efficacy") | .proposed' "${case14b}/out.json")"

[ "${basis_with}" = "90.00" ] || fail "case14a basis must ignore the discarded row and equal the measured 90.00, got ${basis_with}"
[ "${proposed_with}" = "89.00" ] || fail "case14a proposed must be 89.00 (basis 90.00 - buffer 1.00), got ${proposed_with}"
[ "${basis_with}" = "${basis_without}" ] ||
	fail "the discarded row must not change basis: with=${basis_with} without=${basis_without}"
[ "${proposed_with}" = "${proposed_without}" ] ||
	fail "the discarded row must not raise (or change) the proposal: with=${proposed_with} without=${proposed_without}"

if [ "${failures}" -ne 0 ]; then
	echo "${failures} mutation ratchet check(s) failed" >&2
	exit 1
fi

echo "mutation ratchet self-tests passed."
