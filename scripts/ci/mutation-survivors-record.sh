#!/usr/bin/env bash
#
# PW-2.5. Build the `mutation-survivors` quality-history record for one
# mutation-testing run: a stable identity for every LIVED/NOT COVERED mutant
# in the gating (`mutation-report.json`, via the leg's own
# `mutation-survivors.json`) and advisory (`mutation-advisory-<name>.json`)
# reports, so scripts/mutation-survivors-diff.sh can tell "killed" from "we
# never measured this run" across runs.
#
# Usage:
#   mutation-survivors-record.sh <records-dir> <advisory-dir> <src-root> <expected-list>
#
#   <records-dir>    Downloaded `mutation-history-*-<run_id>` artifacts, one
#                     subdirectory per gating leg, each holding
#                     quality-history-record.json and (when mode is "gated")
#                     mutation-survivors.json.
#   <advisory-dir>    Downloaded `mutation-advisory-*-<run_id>` artifacts, one
#                     subdirectory per advisory group, each holding
#                     mutation-advisory-<name>.{json,txt} per package.
#   <src-root>        Root of a fresh source checkout at the run's own SHA,
#                      used only to read the 5-line window each identity is
#                      hashed from. Nothing here executes Go.
#   <expected-list>   A file of `name|package` lines, one per gating-matrix
#                      package (the workflow's MUTATION_PACKAGES env block).
#                      Every line gets a gated entry and an advisory entry,
#                      always, so a leg that never uploaded still gets a
#                      "missing" row instead of silently vanishing from the
#                      series.
#
# One JSON object is written to stdout: {schema, anchor, complete, packages}.
# The run envelope (lane, timestamp, workflow, run_id, sha, ...) is added by
# scripts/ci/quality-history-append.sh, the same way every other lane's
# record does, so it is not duplicated here.
#
# Identity: id = name:f:m:a:o, where `a` is the first 12 hex characters of a
# sha256 over the whitespace-trimmed source lines l-2..l+2 (5 lines, each
# followed by \x1f, missing lines padded with an empty string), and `o` is
# the 0-based ordinal among mutants sharing (f, m, a) within the same source,
# ordered by (line, column). See .planning/pw-2.5-survivor-payload-spec.md
# for the measurement behind this choice: bare file+type+line collides 66 of
# 335 times on this repo's real churn, and the 5-line window plus ordinal
# resolves all but 3 genuine code changes.
#
# Every {f,m,s,l,c} field below is sourced from a downloaded artifact: a
# report the gremlins/mutation-advisory jobs produced by running mutated Go
# source. That is treated as untrusted input throughout this script. `l` and
# `c` never reach `$(( ))`, and no field reaches a path or a jq argument,
# until it has passed validation: `f` is a relative path with no leading "/",
# no ".." segment, and no control characters; `m` matches `^[A-Z_]+$`; `s` is
# exactly "L" or "U"; `l` and `c` are non-negative JSON numbers (checked
# twice: once in jq while the value is still JSON-typed, once more in bash
# with a `case` glob immediately before the arithmetic in window_hash, so a
# gap in either layer alone still can't reach `$(( ))`). Any mutant that
# fails demotes the whole package/source entry to "unparseable" rather than
# being dropped or aborting the run: a report that can't be trusted for one
# field can't be trusted for the rest of it either.

set -euo pipefail
export LC_ALL=C
shopt -s nullglob

die() {
	echo "mutation-survivors-record: $*" >&2
	exit 1
}

records_dir="${1:-}"
advisory_dir="${2:-}"
src_root="${3:-}"
expected_list="${4:-}"

[ -n "${records_dir}" ] && [ -n "${advisory_dir}" ] && [ -n "${src_root}" ] && [ -n "${expected_list}" ] || {
	echo "Usage: $0 <records-dir> <advisory-dir> <src-root> <expected-list>" >&2
	exit 1
}
[ -f "${expected_list}" ] || die "expected-list not found: ${expected_list}"
command -v jq >/dev/null 2>&1 || die "jq is required"

sha256_hex() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum | awk '{print $1}'
	else
		shasum -a 256 | awk '{print $1}'
	fi
}

# A non-negative bash-integer literal: no sign, no decimal point, digits
# only, and no more than 15 of them. The last gate before a value drives
# `$(( ))` arithmetic. The length bound matters on its own: bash's integer
# arithmetic silently wraps rather than erroring on overflow (on bash 3.2,
# `$((10#18446744073709551616))` evaluates to 0), so an oversized digit
# string would sail through the earlier digit-only check and then produce a
# wrong-but-plausible number instead of the loud rejection a corrupt count
# deserves. 15 digits covers any real mutant count with room to spare.
is_digits() {
	case "$1" in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "${#1}" -le 15 ]
}

# Rejects a leading "/" (an absolute path escaping src-root) and any ".",
# ".." or empty path segment (escaping the package directory, or a
# double-slash/trailing-slash malformation). Field splitting on "/" is
# deliberate here rather than a regex: POSIX ERE has no reliable word
# boundary for a bare ".." segment without pulling in GNU-only extensions.
#
# `for seg in $1` is unquoted (required for the field split to happen at
# all), which also puts every resulting word through pathname expansion --
# a real bug this function used to have: a raw `f` field such as ".*/x.go"
# glob-expanded ".*" against whatever the current working directory
# happened to contain, and every directory always has "." and ".." entries
# for that pattern to match, so the rejection this function appeared to
# produce for that input was an accident of `shopt`/`GLOBIGNORE` state, not
# an actual path check. The subshell below runs the split under `set -f`
# (noglob) so `for seg in $1` performs field splitting ONLY, no expansion,
# and is scoped to the subshell so it never leaks noglob into the caller.
# MUTANT_OK_FILTER at :131 is the layer that actually has to reject glob
# metacharacters (`*?[\`) deterministically; this function only ever needs
# to be a pure, CWD-independent "/", ".", "..", empty-segment check.
real_path_ok() {
	case "$1" in
	/*) return 1 ;;
	esac
	(
		set -f
		IFS='/'
		local seg
		for seg in $1; do
			case "${seg}" in
			'' | . | ..) exit 1 ;;
			esac
		done
		exit 0
	)
}

# Applied to a JSON array of raw {f,m,s,l,c} objects (still on stdin, still
# JSON-typed): annotates each with a computed `ok` boolean and streams them
# back out one per line. Shared between the gated and advisory extractions
# so the field-validation rule behind finding 1 lives in exactly one place,
# and runs before any field is ever pulled out of JSON into a bash string.
#
# This is the layer that has to reject a glob metacharacter (`*`, `?`,
# `[`, backslash) deterministically: jq's own `split("/")` never expands
# anything, so a segment equal to "." or ".." is rejected here by plain
# string equality, cwd-independent and shell-independent. real_path_ok
# below does the same "/", ".", "..", empty-segment check again, in bash,
# as defense in depth once a field is pulled out of JSON into a shell
# string -- it used to ALSO be where the glob-metacharacter question got
# answered, by accident, because its unquoted `for seg in $1` glob-expanded
# a metacharacter segment against the current working directory. That
# accident is fixed there (a noglob subshell), which means it can no
# longer answer the question at all, so it has to be answered here
# instead, on the string, before either check ever runs.
MUTANT_OK_FILTER='
    map(. + {ok: (
        (.f? != null) and ((.f | type) == "string")
        and ((.f | explode | any(. < 32 or . == 127)) | not)
        and ((.f | explode | any(. == 42 or . == 63 or . == 91 or . == 92)) | not)
        and ((.f | startswith("/")) | not)
        and ((.f | split("/") | any(. == ".." or . == "." or . == "")) | not)
        and (.f != "")
        and (.m? != null) and ((.m | type) == "string") and (.m | test("^[A-Z_]+$"))
        and (.s == "L" or .s == "U")
        and (.l? != null) and ((.l | type) == "number") and (.l == (.l | floor)) and (.l >= 1)
        and (.c? != null) and ((.c | type) == "number") and (.c == (.c | floor)) and (.c >= 0)
    )})
    | .[]
'

# The 5-line window around $2 in file $1, whitespace-trimmed per line, each
# followed by \x1f, hashed and truncated to 12 hex characters. A line before
# 1 or past EOF (including a missing file entirely) contributes an empty
# field rather than failing: the window is padding-tolerant by design so a
# mutant near the top of a short file still gets an identity. One awk pass
# per mutant collects the whole window (and stops reading past it) instead
# of five separate `sed` scans, each restarting at line 1.
window_hash() {
	local file="$1" line="$2"
	local start=$((line - 2))
	local end=$((line + 2))
	local lead=0
	if [ "${start}" -lt 1 ]; then
		lead=$((1 - start))
	fi

	local -a existing=()
	if [ -f "${file}" ]; then
		local raw ln
		raw="$(awk -v s="${start}" -v e="${end}" 'NR>=s && NR<=e {print} NR>e {exit}' "${file}" 2>/dev/null || true)"
		if [ -n "${raw}" ]; then
			while IFS= read -r ln || [ -n "${ln}" ]; do
				existing+=("${ln}")
			done <<<"${raw}"
		fi
	fi

	local window="" i slot trimmed
	for ((i = 0; i < 5; i++)); do
		trimmed=""
		if [ "${i}" -ge "${lead}" ]; then
			slot=$((i - lead))
			if [ "${slot}" -lt "${#existing[@]}" ]; then
				trimmed="$(printf '%s' "${existing[${slot}]}" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
			fi
		fi
		window="${window}${trimmed}"$'\x1f'
	done
	printf '%s' "${window}" | sha256_hex | cut -c1-12
}

# {name, package, source, state, counts, mutants} as one compact JSON line.
build_entry() {
	local name="$1" package="$2" source="$3" state="$4" counts="$5" mutants="$6"
	jq -cn \
		--arg name "${name}" --arg package "${package}" --arg source "${source}" \
		--arg state "${state}" --argjson counts "${counts}" --argjson mutants "${mutants}" \
		'{name: $name, package: $package, source: $source, state: $state, counts: $counts, mutants: $mutants}'
}

# Turns a stream of raw {f,m,s,l,c,ok} objects (one per line on stdin, `ok`
# already computed by MUTANT_OK_FILTER) into a finished "measured" entry:
# re-validates `l`/`c`/`m`/`s` in bash as a second gate, validates every real
# path, hashes every window, assigns ordinals within (f, m, a), and rolls up
# the LIVED / NOT COVERED counts. Any mutant that fails either validation
# layer demotes the whole entry to "unparseable" rather than dropping just
# that one mutant or aborting the run: a rejected field means the report
# cannot be trusted to describe this package's real files at all.
build_from_raw() {
	local name="$1" package="$2" source="$3" raw="$4"
	local pkg_rel="${package#./}"
	local tmp
	tmp="$(mktemp)"

	local bad=0
	local line ok f m s l c real a
	while IFS= read -r line; do
		[ -n "${line}" ] || continue

		ok="$(jq -r '.ok' <<<"${line}" 2>/dev/null || echo false)"
		if [ "${ok}" != "true" ]; then
			bad=1
			break
		fi

		f="$(jq -r '.f' <<<"${line}")"
		m="$(jq -r '.m' <<<"${line}")"
		s="$(jq -r '.s' <<<"${line}")"
		l="$(jq -r '.l' <<<"${line}")"
		c="$(jq -r '.c' <<<"${line}")"

		# Defense in depth: `ok` above already confirmed these came from
		# JSON numbers/safe strings, but `l` and `c` are about to drive
		# shell arithmetic in window_hash, so they are re-checked here
		# with a `case` glob rather than trusted on jq's say-so alone. A
		# downloaded artifact that can influence a `$(( ))` expression is
		# a command-injection primitive in the job that holds
		# `contents: write`; this is the last gate before that
		# expression runs.
		if ! is_digits "${l}" || ! is_digits "${c}"; then
			bad=1
			break
		fi
		case "${m}" in
		'' | *[!A-Z_]*)
			bad=1
			;;
		esac
		[ "${bad}" -eq 0 ] || break
		case "${s}" in
		L | U) ;;
		*)
			bad=1
			;;
		esac
		[ "${bad}" -eq 0 ] || break

		# Validated on the raw file_name, not the concatenated real path: a
		# leading "/" in $f (Gremlins' own field) still passes an
		# `${pkg_rel}/${f}` concatenation, since prepending a prefix to an
		# absolute-looking string never anchors it at "/" and instead just
		# produces a real-looking "pkg//etc/passwd" that a naive check on
		# the joined string would wave through.
		if ! real_path_ok "${f}"; then
			bad=1
			break
		fi
		real="${pkg_rel}/${f}"
		if ! real_path_ok "${real}"; then
			bad=1
			break
		fi

		a="$(window_hash "${src_root}/${real}" "${l}")"
		jq -cn \
			--arg f "${f}" --arg m "${m}" --arg s "${s}" --arg a "${a}" \
			--argjson l "${l}" --argjson c "${c}" \
			'{f: $f, m: $m, s: $s, a: $a, l: $l, c: $c}' >>"${tmp}"
	done <<<"${raw}"

	if [ "${bad}" -eq 1 ]; then
		rm -f "${tmp}"
		build_entry "${name}" "${package}" "${source}" "unparseable" null '[]'
		return
	fi

	local mutants
	mutants="$(jq -cs '
        group_by([.f, .m, .a])
        | map(sort_by([.l, .c]) | to_entries | map(.value + {o: .key}))
        | add // []
        | sort_by([.f, .l, .c])
        | map({f, m, s, a, o, l, c})
    ' "${tmp}")"
	rm -f "${tmp}"

	local lived not_covered counts
	lived="$(jq '[.[] | select(.s == "L")] | length' <<<"${mutants}")"
	not_covered="$(jq '[.[] | select(.s == "U")] | length' <<<"${mutants}")"
	counts="$(jq -cn --argjson l "${lived}" --argjson u "${not_covered}" '{lived: $l, not_covered: $u}')"

	build_entry "${name}" "${package}" "${source}" "measured" "${counts}" "${mutants}"
}

# Exactly one top-level JSON object, the same guard the gremlins matrix leg
# applies to mutation-report.json before trusting it (see the workflow's own
# comment at the "Record this package for the quality history" step): a
# truncated or concatenated report must not be read as a clean measurement.
one_object() {
	jq -e -s 'length == 1 and (.[0] | type == "object")' "$1" >/dev/null 2>&1
}

# True when every mutant that got no KILLED/LIVED verdict did so because it
# TIMED OUT, mirroring the guard mutation-gate.sh and the workflow's own
# advisory headline table apply. Gremlins scores efficacy as
# killed/(killed+lived), so an all-timed-out run reports 0.00% the same way
# a genuinely all-LIVED run would, and the two must not be conflated as
# "measured".
#
# Gremlins' own mutants_total is killed+lived+notViable (report.go:
# `MutantsTotal: r.lived + r.killed + r.notViable`) and excludes TIMED OUT
# and NOT COVERED entirely, so an all-timed-out package reports
# mutants_total == 0, not >0: gating on "total > 0" can never fire for the
# exact case it exists to catch. There is no mutants_timed_out field in
# Gremlins' JSON output either. So the gated and advisory cases each read
# the real TIMED OUT count from wherever it actually lives: the gated
# leg's own quality-history-record.json carries `timed_out`, parsed by the
# workflow from the text report's "Timed out: N" line for this reason; the
# advisory leg has the full per-mutant status list right there, so its
# count is the number of `.mutations[].status == "TIMED OUT"` entries,
# read directly rather than through a proxy field.
#
# Three-way exit status, not a boolean: 0 means "yes, all timed out", 1
# means "no, this looks like a real measurement", and 2 means "the fields
# this depends on are malformed" -- a caller that only checked for 0/nonzero
# would read 2 the same as 1 and fall through to treating a corrupted
# artifact as a trustworthy measurement. `10#` on every value forces base-10
# arithmetic: a JSON string field such as `"killed": "08"` passes
# `is_digits` (it is all digits) but `$((killed + lived))` alone would try
# to parse "08" as octal and abort the whole script under `set -e`, since 8
# and 9 are not valid octal digits.
#
# The extraction below never falls back with `// 0`: the workflow always
# writes `timed_out`/`killed`/`lived` (gated) and `mutants_killed`/
# `mutants_lived` (advisory) as either a number or an explicit JSON null
# (see the writer step's `// null`), so there is no legitimate "absent"
# case to paper over. `// 0` folded `false` and `null` alike into a
# passing "0" before `is_digits` ever saw them, so a corrupt record with
# `"killed": false` read as a real zero instead of the malformed value it
# is. `safe_count` requires an actual non-negative integer and emits the
# literal string "BAD" for anything else (`null`, `false`, a string, a
# float, a negative number, or an array/object); `is_digits` then rejects
# "BAD" the same way it rejects any other non-digit string.
SAFE_COUNT_FILTER='def safe_count: if (type == "number") and (. == floor) and (. >= 0) then tostring else "BAD" end;'
gated_all_timed_out() {
	local file="$1"
	local vals timed_out killed lived
	vals="$(jq -r "${SAFE_COUNT_FILTER}"'
        [(.timed_out | safe_count), (.killed | safe_count), (.lived | safe_count)] | @tsv
    ' "${file}" 2>/dev/null || true)"
	IFS=$'\t' read -r timed_out killed lived <<<"${vals}"
	if ! is_digits "${timed_out}" || ! is_digits "${killed}" || ! is_digits "${lived}"; then
		return 2
	fi
	if [ "$((10#${timed_out}))" -gt 0 ] && [ "$((10#${killed} + 10#${lived}))" -eq 0 ]; then
		return 0
	fi
	return 1
}

advisory_all_timed_out() {
	local file="$1"
	local vals timed_out killed lived
	vals="$(jq -r "${SAFE_COUNT_FILTER}"'
        [ ([ (.files // [])[] | (.mutations // [])[] | select(.status == "TIMED OUT") ] | length | tostring),
          (.mutants_killed | safe_count), (.mutants_lived | safe_count) ] | @tsv
    ' "${file}" 2>/dev/null || true)"
	IFS=$'\t' read -r timed_out killed lived <<<"${vals}"
	if ! is_digits "${timed_out}" || ! is_digits "${killed}" || ! is_digits "${lived}"; then
		return 2
	fi
	if [ "$((10#${timed_out}))" -gt 0 ] && [ "$((10#${killed} + 10#${lived}))" -eq 0 ]; then
		return 0
	fi
	return 1
}

gated_entry() {
	local name="$1" package="$2"
	local d rf n rec_dir="" rec_file=""

	for d in "${records_dir}"/*/; do
		[ -d "${d}" ] || continue
		rf="${d}quality-history-record.json"
		[ -f "${rf}" ] || continue
		n="$(jq -r '.name // empty' "${rf}" 2>/dev/null || true)"
		if [ "${n}" = "${name}" ]; then
			rec_dir="${d}"
			rec_file="${rf}"
			break
		fi
	done

	if [ -z "${rec_file}" ]; then
		build_entry "${name}" "${package}" "gated" "missing" null '[]'
		return
	fi

	local mode
	mode="$(jq -r '.mode // empty' "${rec_file}" 2>/dev/null || true)"
	case "${mode}" in
	gated)
		local survivors_file="${rec_dir}mutation-survivors.json"
		if [ ! -f "${survivors_file}" ]; then
			build_entry "${name}" "${package}" "gated" "unmeasured" null '[]'
			return
		fi
		if ! one_object "${survivors_file}"; then
			build_entry "${name}" "${package}" "gated" "unparseable" null '[]'
			return
		fi

		# .survivors/.uncovered must actually be arrays of objects before
		# anything tries to index an element as {file,line,column,mutator}:
		# a malformed shape (a string, or an array of non-objects like
		# [42]) must demote this entry to "unparseable", not raise a jq
		# type error that `set -e` would turn into a whole-script abort.
		# No real producer ever writes `.survivors`/`.uncovered` as null:
		# the workflow builds them with jq array comprehensions (`[]` at
		# minimum for zero mutants), so `is_arr_ok` requires an array of
		# objects outright. `null` and absent are wrong shapes here too,
		# same as `false`, a string, or a number: `// []` (which would
		# fold both into "no entries") is deliberately not used.
		local shape_ok
		shape_ok="$(jq -r '
            def is_arr_ok: (type == "array") and all(type == "object");
            (.survivors | is_arr_ok) and (.uncovered | is_arr_ok)
        ' "${survivors_file}" 2>/dev/null || echo false)"
		if [ "${shape_ok}" != "true" ]; then
			build_entry "${name}" "${package}" "gated" "unparseable" null '[]'
			return
		fi

		local timed_out_rc=0
		gated_all_timed_out "${rec_file}" || timed_out_rc=$?
		case "${timed_out_rc}" in
		0)
			build_entry "${name}" "${package}" "gated" "unmeasured" null '[]'
			return
			;;
		2)
			build_entry "${name}" "${package}" "gated" "unparseable" null '[]'
			return
			;;
		esac

		local raw
		raw="$(jq -c '
            [ (.survivors // [])[] | {f: .file, m: .mutator, s: "L", l: .line, c: .column} ]
            + [ (.uncovered // [])[] | {f: .file, m: .mutator, s: "U", l: .line, c: .column} ]
        ' "${survivors_file}" | jq -c "${MUTANT_OK_FILTER}")"
		build_from_raw "${name}" "${package}" "gated" "${raw}"
		;;
	zero-mutants)
		build_entry "${name}" "${package}" "gated" "zero-mutants" '{"lived":0,"not_covered":0}' '[]'
		;;
	unmeasured)
		build_entry "${name}" "${package}" "gated" "unmeasured" null '[]'
		;;
	*)
		build_entry "${name}" "${package}" "gated" "unparseable" null '[]'
		;;
	esac
}

advisory_entry() {
	local name="$1" package="$2"
	local d any_group=0

	for d in "${advisory_dir}"/*/; do
		[ -d "${d}" ] || continue
		any_group=1
		break
	done

	if [ "${any_group}" -eq 0 ]; then
		build_entry "${name}" "${package}" "advisory" "missing" null '[]'
		return
	fi

	local json_file="" txt_file=""
	for d in "${advisory_dir}"/*/; do
		[ -d "${d}" ] || continue
		if [ -f "${d}mutation-advisory-${name}.json" ]; then
			json_file="${d}mutation-advisory-${name}.json"
			break
		fi
	done
	if [ -z "${json_file}" ]; then
		for d in "${advisory_dir}"/*/; do
			[ -d "${d}" ] || continue
			if [ -f "${d}mutation-advisory-${name}.txt" ]; then
				txt_file="${d}mutation-advisory-${name}.txt"
				break
			fi
		done
	fi

	if [ -n "${json_file}" ]; then
		if ! one_object "${json_file}"; then
			build_entry "${name}" "${package}" "advisory" "unparseable" null '[]'
			return
		fi

		# Same shape guard as the gated path, for .files/.mutations: an
		# array element that isn't an object (e.g. [42]) must demote to
		# "unparseable" rather than raising a jq type error under `set -e`
		# when the extraction below tries to index it as {file_name,...}.
		# No real Gremlins report ever writes `.files` or a file's
		# `.mutations` as null (the workflow's own zero-mutant leg is
		# text-only, never a JSON report with a null array), so `null` and
		# absent are wrong shapes too, same as `false` or a string: `// []`
		# (which would fold either into "no entries", misreporting a
		# malformed report as a clean zero-mutants measurement) is
		# deliberately not used.
		local shape_ok
		shape_ok="$(jq -r '
            def is_arr_ok: (type == "array") and all(type == "object");
            (.files | is_arr_ok) and (.files | all(.mutations | is_arr_ok))
        ' "${json_file}" 2>/dev/null || echo false)"
		if [ "${shape_ok}" != "true" ]; then
			build_entry "${name}" "${package}" "advisory" "unparseable" null '[]'
			return
		fi

		local timed_out_rc=0
		advisory_all_timed_out "${json_file}" || timed_out_rc=$?
		case "${timed_out_rc}" in
		0)
			build_entry "${name}" "${package}" "advisory" "unmeasured" null '[]'
			return
			;;
		2)
			build_entry "${name}" "${package}" "advisory" "unparseable" null '[]'
			return
			;;
		esac

		local raw
		raw="$(jq -c '
            [ (.files // [])[] | .file_name as $f | (.mutations // [])[]
              | select(.status == "LIVED" or .status == "NOT COVERED")
              | {f: $f, m: .type, s: (if .status == "LIVED" then "L" else "U" end), l: .line, c: .column} ]
        ' "${json_file}" | jq -c "${MUTANT_OK_FILTER}")"
		build_from_raw "${name}" "${package}" "advisory" "${raw}"
		return
	fi

	# The .txt is the tee'd Gremlins stdout for a leg that never produced
	# JSON. A genuine zero-mutants run and a leg that died before Gremlins
	# ran both leave only a .txt behind (or none at all), and they must not
	# be conflated: the gating leg's own zero-mutants check greps
	# mutation-report.txt for this exact phrase (workflow's "Assess the
	# gate" step), so a died leg's .txt lacks it and is "missing", not a
	# silent "zero-mutants".
	if [ -n "${txt_file}" ]; then
		if grep -qF "No results to report" "${txt_file}" 2>/dev/null; then
			build_entry "${name}" "${package}" "advisory" "zero-mutants" '{"lived":0,"not_covered":0}' '[]'
		else
			build_entry "${name}" "${package}" "advisory" "missing" null '[]'
		fi
		return
	fi

	build_entry "${name}" "${package}" "advisory" "missing" null '[]'
}

entries_file="$(mktemp)"
trap 'rm -f "${entries_file}"' EXIT

name=""
package=""
while IFS='|' read -r name package || [ -n "${name}" ]; do
	[ -n "${name}" ] || continue
	gated_entry "${name}" "${package}" >>"${entries_file}"
	advisory_entry "${name}" "${package}" >>"${entries_file}"
done <"${expected_list}"

packages="$(jq -cs '.' "${entries_file}")"
complete="$(jq -c '[.[] | (.state == "measured" or .state == "zero-mutants")] | all' <<<"${packages}")"

jq -cn --argjson packages "${packages}" --argjson complete "${complete}" \
	'{schema: 1, anchor: "trimline5-sha256-12", complete: $complete, packages: $packages}'
