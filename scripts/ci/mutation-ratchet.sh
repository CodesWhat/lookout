#!/usr/bin/env bash
#
# Propose mutation floor ratchets from this run's per-package records and the
# quality-history series, without writing anything back to the repository.
#
# The `ratchet` job in quality-mutation-monthly.yml stays `contents: read`:
# raising a floor is a two-file lockstep edit (the matrix in that workflow and
# `expectedFloors` in scripts/mutation-config.test.mjs) plus a per-package
# prose comment, and that edit belongs in a reviewed PR, not a scheduled job
# holding write access. This script only ever proposes.
#
# Usage: mutation-ratchet.sh <records-dir> <history.jsonl> <out.json>
#
#   <records-dir>   Directory of downloaded mutation-history-*-<run_id>
#                    artifacts, one subdirectory per package, each holding a
#                    quality-history-record.json (always) and, for a package
#                    that measured ("gated" mode), a mutation-survivors.json.
#   <history.jsonl>  The quality-history branch's mutation.jsonl, or a path
#                    that does not exist / is empty on the branch's first run.
#   <out.json>       Where the proposal document is written.
#
# Per package, per metric (efficacy, mutator_coverage):
#
#   pool    = this run's measured value, plus up to the 6 most recent prior
#             mutation.jsonl rows for that package with mode == "gated" and
#             outcome == "success". "Prior" is enforced by run id, not by
#             position: the `history` job appends this run's own rows to the
#             branch before this job checks it out, so without that filter
#             the newest slot in the 6-row window holds a duplicate of the
#             current measurement and silently evicts the oldest real prior,
#             raising basis and shrinking the buffer.
#
#             PW-7.36: a row (this run's own, or a prior one carrying the
#             same killed/lived/timed_out counts) that timed out any mutants
#             does not enter the efficacy pool as Gremlins' own reported
#             value. It is recomputed as killed / (killed + lived +
#             timed_out), i.e. every timed-out mutant is added to the
#             sample and credited to neither side -- never counted as
#             killed -- instead of being dropped from the ratio the way
#             Gremlins' own killed/(killed+lived) drops it. That can only
#             pull the pool value down or leave it unchanged, so a package
#             that timed out mutants can never ratchet a floor higher than a
#             clean run would have. mutator_coverage is not adjusted:
#             Gremlins counts a timed-out mutant as covered (a test did run
#             against it), so it costs nothing there. A history row that
#             timed out mutants but is missing killed or lived cannot be
#             recomputed at all, so it cannot be trusted to have been
#             discounted -- it is dropped from the pool instead of falling
#             back to its raw (undiscounted) efficacy, and the drop is
#             counted in the proposal's discarded_rows.
#   basis   = min(pool). Never the best run in the pool -- a ratchet built
#             from the best run would relock the very slack PW-6.1 exists to
#             catch.
#   buffer  = max(1.0, max(pool) - min(pool), seed). `seed` covers two
#             packages (adapter-drydock, metrics) whose run-to-run spread is
#             documented in the workflow's own matrix comments but is not yet
#             reflected in quality-history, because recording started after
#             their floors were set; the seed floors the buffer until real
#             samples overtake it.
#   proposed = basis - buffer, floored (not rounded) to two decimals so a
#             proposal never sits a hundredth above what the pool actually
#             supports, and so it round-trips through mutation-gate.sh's own
#             hundredths comparison.
#
# A proposal is only emitted when proposed clears the current floor by at
# least 2.00 and is strictly above it -- never a lower or equal value. A
# package is skipped entirely (not proposed, not silently dropped) when its
# own run this cycle was not a clean gated measurement: mode != "gated",
# outcome != "success", its timed-out mutants exceed 5% of the mutants it
# attempted (reason "timed-out-over-tolerance", the counts that produced the
# decision recorded alongside it), or neither metric has a readable pool.
# Up to that 5% tolerance a timed-out mutant no longer skips the package on
# its own -- see the pool comment above for how it is kept from raising a
# floor instead.
set -euo pipefail
export LC_ALL=C

records_dir="${1:-}"
history_file="${2:-}"
out_file="${3:-}"

if [ -z "${records_dir}" ] || [ -z "${history_file}" ] || [ -z "${out_file}" ]; then
	echo "Usage: $0 <records-dir> <history.jsonl> <out.json>" >&2
	exit 2
fi

if [ ! -d "${records_dir}" ]; then
	echo "mutation-ratchet: records directory not found: ${records_dir}" >&2
	exit 2
fi

if ! command -v jq >/dev/null 2>&1; then
	echo "mutation-ratchet: jq is required" >&2
	exit 2
fi

min_gain_hundredths=200

# Seed buffers for the two packages called out above. "name:metric" -> floor.
# A colon, not a pipe: case patterns treat "|" as alternation, so
# "adapter-drydock|efficacy" would match either half rather than the pair.
seed_buffer() {
	case "$1:$2" in
	"adapter-drydock:efficacy") printf '2.0' ;;
	"metrics:efficacy") printf '2.5' ;;
	"metrics:mutator_coverage") printf '12.0' ;;
	*) printf '0' ;;
	esac
}

# Floor (never round) to hundredths and print with two decimals, so the
# result always compares <= the true value under mutation-gate.sh's own
# to_hundredths().
floor2() {
	awk -v x="$1" 'BEGIN { printf "%.2f", int((x * 100) + 0.0000001) / 100 }'
}

# Integer hundredths, for comparisons that must not trip on float dust
# (jq and awk both round-trip a decimal like 90.10 as 90.099999999999994).
to_hundredths() {
	awk -v x="$1" 'BEGIN { printf "%d", int((x * 100) + 0.0000001) }'
}

shopt -s nullglob
current_records=("${records_dir}"/*/quality-history-record.json)
survivor_records=("${records_dir}"/*/mutation-survivors.json)

if [ "${#current_records[@]}" -eq 0 ]; then
	echo "mutation-ratchet: no per-package records found under ${records_dir}" >&2
	exit 1
fi

current_json="$(jq -sc '[ .[] | select(type == "object") ]' "${current_records[@]}")"

if [ "${#survivor_records[@]}" -gt 0 ]; then
	survivors_json="$(jq -sc '[ .[] | select(type == "object") ]' "${survivor_records[@]}")"
else
	survivors_json="[]"
fi

if [ -s "${history_file}" ]; then
	history_json="$(jq -sc '[ .[] | select(type == "object") ]' "${history_file}")"
else
	history_json="[]"
fi

names="$(jq -r '[ .[].name ] | unique[]' <<<"${current_json}")"

proposals_file="$(mktemp)"
skipped_file="$(mktemp)"
trap 'rm -f "${proposals_file}" "${skipped_file}"' EXIT

while IFS= read -r name; do
	[ -n "${name}" ] || continue

	record="$(jq -c --arg name "${name}" '[ .[] | select(.name == $name) ][0]' <<<"${current_json}")"
	package="$(jq -r '.package' <<<"${record}")"
	mode="$(jq -r '.mode' <<<"${record}")"
	outcome="$(jq -r '.outcome' <<<"${record}")"
	timed_out="$(jq -r '.timed_out // 0' <<<"${record}")"
	killed="$(jq -r '.killed // empty' <<<"${record}")"
	lived="$(jq -r '.lived // empty' <<<"${record}")"

	if [ "${mode}" != "gated" ]; then
		jq -cn --arg name "${name}" --arg reason "${mode}" '{name: $name, reason: $reason}' >>"${skipped_file}"
		continue
	fi
	if [ "${outcome}" != "success" ]; then
		jq -cn --arg name "${name}" --arg reason "outcome-${outcome}" '{name: $name, reason: $reason}' >>"${skipped_file}"
		continue
	fi

	# PW-7.36. mutants_total is Gremlins' own killed+lived+notViable count,
	# which already excludes timed-out mutants (see the extraction comment in
	# quality-mutation-monthly.yml), so adding timed_out back in gives the
	# count of mutants actually attempted this run. Both sides of the
	# tolerance check are plain non-negative integers -- mutants_total from
	# Gremlins' JSON, timed_out parsed as digits from its text report -- so
	# timed_out*100 <= mutants_attempted*5 is exactly timed_out/mutants <=
	# 5% without ever forming the fraction, and with timed_out at 0 the
	# check always passes. Above the tolerance the package is still skipped,
	# same as before, but with the counts that produced the decision
	# attached instead of a bare reason string.
	mutants_total="$(jq -r '.mutants_total // 0' <<<"${record}")"
	mutants_attempted=$((mutants_total + timed_out))
	if [ "$((timed_out * 100))" -gt "$((mutants_attempted * 5))" ]; then
		jq -cn --arg name "${name}" --argjson timed_out "${timed_out}" --argjson mutants "${mutants_attempted}" \
			'{name: $name, reason: "timed-out-over-tolerance", timed_out: $timed_out, mutants: $mutants}' >>"${skipped_file}"
		continue
	fi

	pool_ok=0
	for metric_spec in "efficacy efficacy_floor efficacy" "mutator_coverage mutator_coverage_floor mcover"; do
		read -r metric floor_field workflow_field <<<"${metric_spec}"

		measured="$(jq -r --arg m "${metric}" '.[$m] // empty' <<<"${record}")"
		current_floor="$(jq -r --arg f "${floor_field}" '.[$f] // empty' <<<"${record}")"

		if [ -z "${measured}" ] || [ -z "${current_floor}" ]; then
			continue
		fi
		pool_ok=1

		# PW-7.36. A timed-out mutant is neither killed nor lived, so it is
		# never counted as killed: every timed-out mutant is added to the
		# denominator here and credited to neither side, which can only pull
		# this value down from Gremlins' own killed/(killed+lived) or leave
		# it unchanged (timed_out == 0), never raise it. Applies to this
		# run's own contribution and to any history row that carries the
		# same killed/lived/timed_out counts (quality-history-append.sh
		# appends the full record, not just the headline numbers). Only
		# efficacy is adjusted: Gremlins counts a timed-out mutant as
		# covered, so mutator_coverage owes it nothing.
		current_pool_value="${measured}"
		if [ "${metric}" = "efficacy" ] && [ "${timed_out}" -gt 0 ] && [ -n "${killed}" ] && [ -n "${lived}" ]; then
			current_pool_value="$(awk -v k="${killed}" -v l="${lived}" -v t="${timed_out}" \
				'BEGIN { printf "%.10f", (100.0 * k) / (k + l + t) }')"
		fi

		# A history row that timed out mutants but is missing killed or
		# lived cannot be recomputed as killed/(killed+lived+timed_out), so
		# it cannot be discounted the way PW-7.36 requires. Falling back to
		# its raw (undiscounted) efficacy would let exactly the kind of row
		# this discount exists for raise the pool minimum, so such rows are
		# dropped from the pool entirely instead, and counted so the drop is
		# visible on the proposal.
		#
		# Policy: an undiscountable row is dropped rather than admitted raw,
		# because a raw value can only be higher than the discounted truth
		# and could only ever push the minimum up, never down — dropping it
		# is the conservative branch, and the proposal reports it in
		# discarded_rows. As of this writing, quality-history:mutation.jsonl
		# carries 64 rows and none has this shape — every row either carries
		# the full killed/lived/timed_out/efficacy set or has timed_out == 0
		# — so this branch has never actually fired against real history.
		history_result="$(jq -c --arg name "${name}" --arg metric "${metric}" --arg run_id "${GITHUB_RUN_ID:-}" '
            [ .[]
              | select(.name == $name and .mode == "gated" and .outcome == "success")
              | select($run_id == "" or ((.run_id // "") | tostring) != $run_id)
            ]
            | .[-6:]
            | {
                values: [ .[]
                  | select(
                      $metric != "efficacy"
                      or ((.timed_out // 0) == 0)
                      or (.killed != null and .lived != null)
                    )
                  | (
                      if $metric == "efficacy" and ((.timed_out // 0) > 0)
                      then (100 * .killed) / (.killed + .lived + (.timed_out // 0))
                      else .[$metric]
                      end
                    )
                  | select(. != null)
                ],
                discarded: [ .[]
                  | select(
                      $metric == "efficacy"
                      and ((.timed_out // 0) > 0)
                      and (.killed == null or .lived == null)
                    )
                ] | length
              }
        ' <<<"${history_json}")"
		history_values="$(jq -r '.values[]' <<<"${history_result}")"
		discarded_rows="$(jq -r '.discarded' <<<"${history_result}")"

		pool="$(printf '%s\n%s\n' "${current_pool_value}" "${history_values}" | awk 'NF')"
		samples="$(printf '%s\n' "${pool}" | awk 'NF' | wc -l | tr -d ' ')"

		basis="$(printf '%s\n' "${pool}" | sort -g | head -n1)"
		max_val="$(printf '%s\n' "${pool}" | sort -g | tail -n1)"
		spread="$(awk -v a="${max_val}" -v b="${basis}" 'BEGIN { printf "%.10f", a - b }')"
		seed="$(seed_buffer "${name}" "${metric}")"
		buffer="$(awk -v s="${spread}" -v sd="${seed}" 'BEGIN {
            m = (s > 1.0) ? s : 1.0
            if (sd > m) m = sd
            printf "%.10f", m
        }')"

		proposed="$(floor2 "$(awk -v b="${basis}" -v buf="${buffer}" 'BEGIN { printf "%.10f", b - buf }')")"

		proposed_h="$(to_hundredths "${proposed}")"
		floor_h="$(to_hundredths "${current_floor}")"
		gain_h=$((proposed_h - floor_h))

		if [ "${gain_h}" -ge "${min_gain_hundredths}" ] && [ "${proposed_h}" -gt "${floor_h}" ]; then
			gain="$(awk -v g="${gain_h}" 'BEGIN { printf "%.2f", g / 100 }')"
			basis_fmt="$(floor2 "${basis}")"
			buffer_fmt="$(floor2 "${buffer}")"
			floor_fmt="$(floor2 "${current_floor}")"
			measured_fmt="$(floor2 "${measured}")"
			jq -cn \
				--arg name "${name}" --arg package "${package}" --arg metric "${metric}" \
				--argjson current_floor "${floor_fmt}" --argjson basis "${basis_fmt}" \
				--argjson measured "${measured_fmt}" \
				--argjson buffer "${buffer_fmt}" --argjson proposed "${proposed}" \
				--argjson gain "${gain}" --argjson samples "${samples}" \
				--argjson timed_out "${timed_out}" --argjson mutants "${mutants_attempted}" \
				--argjson discarded_rows "${discarded_rows}" \
				--arg workflow_field "${workflow_field}" --arg floor_hint "${floor_fmt}" \
				--arg test_map_key "${package}" \
				'{
                    name: $name, package: $package, metric: $metric,
                    current_floor: $current_floor, basis: $basis, measured: $measured, buffer: $buffer,
                    proposed: $proposed, gain: $gain, samples: $samples,
                    timed_out: $timed_out, mutants: $mutants, discarded_rows: $discarded_rows,
                    edit: {
                        workflow_line_hint: ($workflow_field + ": " + $floor_hint),
                        test_map_key: $test_map_key
                    }
                }' >>"${proposals_file}"
		fi
	done

	if [ "${pool_ok}" -eq 0 ]; then
		jq -cn --arg name "${name}" '{name: $name, reason: "no-measurement"}' >>"${skipped_file}"
	fi
done <<<"${names}"

proposals_json="$(jq -sc '.' "${proposals_file}")"
skipped_json="$(jq -sc '.' "${skipped_file}")"

survivors_by_package="$(jq -c '
    [ .[] | select(.efficacy != null) | {name, efficacy, survivors, uncovered} ]
    | sort_by(.efficacy)
' <<<"${survivors_json}")"

jq -cn \
	--arg run_id "${GITHUB_RUN_ID:-}" \
	--arg sha "${GITHUB_SHA:-}" \
	--arg generated_at "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
	--argjson proposals "${proposals_json}" \
	--argjson skipped "${skipped_json}" \
	--argjson survivors_by_package "${survivors_by_package}" \
	'{
        run_id: (if $run_id == "" then null else $run_id end),
        sha: (if $sha == "" then null else $sha end),
        generated_at: $generated_at,
        proposals: $proposals,
        skipped: $skipped,
        survivors_by_package: $survivors_by_package
    }' >"${out_file}"

echo "mutation-ratchet: wrote $(jq '.proposals | length' "${out_file}") proposal(s) and $(jq '.skipped | length' "${out_file}") skip(s) to ${out_file}"
