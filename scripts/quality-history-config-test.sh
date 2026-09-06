#!/usr/bin/env bash
#
# Contract for the quality-history recording jobs (PW-5.5).
#
# The jobs this guards run once a week in one lane, once a month in another,
# and nightly in a third, cannot fail their caller by design, and write to a
# branch nobody checks out. Every property below is one that would otherwise
# be discovered weeks later, or never:
#
#   * the recording job still exists, in all three lanes, calling the shared
#     script;
#   * it fires only on schedule/workflow_dispatch, so a branch's numbers never
#     land in the trunk's series;
#   * `contents: write` is the recording job's alone. It is the only write
#     scope in any lane, and the measuring jobs run code — Gremlins literally
#     executes mutated source, the soak job runs the agent for four hours,
#     the fuzz job runs `go test` against a corpus restored from cache — so a
#     write-scoped credential in one of those is a real escalation and not a
#     style point;
#   * the recording job builds and runs nothing. A job that checks out, calls
#     jq and pushes has no way to reach a credential through code it executed;
#     the moment it grows a `setup-go` or a `go test`, that stops being true.
#
# Usage: quality-history-config-test.sh [soak.yml] [mutation.yml] [fuzz.yml] [append.sh]

set -euo pipefail
export LC_ALL=C

soak_workflow="${1:-.github/workflows/quality-soak-weekly.yml}"
mutation_workflow="${2:-.github/workflows/quality-mutation-monthly.yml}"
fuzz_workflow="${3:-.github/workflows/quality-fuzz-nightly.yml}"
append_script="${4:-scripts/ci/quality-history-append.sh}"

failures=0

fail() {
	echo "FAIL: $1" >&2
	failures=$((failures + 1))
}

for path in "${soak_workflow}" "${mutation_workflow}" "${fuzz_workflow}" "${append_script}"; do
	if [ ! -f "${path}" ]; then
		fail "file not found: ${path}"
		exit 1
	fi
done

# A real tab, for assertions about this repo's tab-indented shell. `\t` is not
# portable in a POSIX ERE and reads as a literal `t` under GNU grep, which
# makes an assertion pass unconditionally rather than fail loudly.
tab="$(printf '\t')"

# Comment lines are not code. Every "must not contain" assertion below reads
# through this, because the jobs explain themselves at length and a job whose
# comment names soak.sh is not a job that runs soak.sh.
strip_comments() {
	grep -v '^[[:space:]]*#' || true
}

# A job's own block, from its two-space key to the next one. Every scoped
# assertion below reads from this rather than from the whole file, so a
# `contents: write` that migrated to a different job can't satisfy a check
# that is about the recording job.
job_block() {
	awk -v job="  $2:" '
        $0 == job { in_job = 1; next }
        in_job && /^  [^[:space:]]/ { in_job = 0 }
        in_job { print }
    ' "$1"
}

# A named step's own block inside a job block, from its `- name:` line to the
# next step. Asserting a per-step property against the whole job block counts
# it satisfied anywhere in the job, so a gate deleted from one step still reads
# as present because a sibling step carries it.
step_block() {
	awk -v want="      - name: $1" '
        $0 == want { in_step = 1; next }
        in_step && /^      - / { in_step = 0 }
        in_step { print }
    '
}

# The gate is asserted as one exact string rather than as three substring
# greps: a substring check cannot tell `||` from `&&`, and the difference
# between "schedule or dispatch" and "schedule and dispatch" is the
# difference between a series that records every run and one that records
# none.
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
gate="always() && (github.event_name == 'schedule' || github.event_name == 'workflow_dispatch')"

# A ':'-prefixed line is bash's no-op command. A bare substring grep cannot
# tell "scripts/ci/foo" (a real invocation) from ": scripts/ci/foo" (the same
# text, disabled) apart, so every "the job must call/run X" assertion below
# routes through this instead of a bare `grep -Fq`.
assert_invoked() {
	local needle="$1" label="$2" block="$3"
	if ! grep -F "${needle}" <<<"${block}" | grep -Eqv '^[[:space:]]*:([[:space:]]|$)'; then
		fail "${label}"
	fi
}

# Same no-op exclusion, but returning the first surviving line number rather
# than pass/fail, for the "record runs before append" ordering check.
first_invoked_line() {
	local needle="$1" block="$2"
	grep -n -F "${needle}" <<<"${block}" |
		grep -Ev '^[0-9]+:[[:space:]]*:([[:space:]]|$)' |
		head -1 | cut -d: -f1 || true
}

check_lane() {
	local workflow="$1"
	local measuring_job="$2"
	local lane="$3"
	local label="$4"

	local block
	block="$(job_block "${workflow}" "history")"
	if [ -z "${block}" ]; then
		fail "${label}: expected a top-level 'history' job"
		return
	fi

	# Accepts both `needs: X` and `needs: [X, Y]`: the mutation lane's
	# history job needs both gremlins and mutation-advisory (PW-2.5), while
	# soak and fuzz still declare a single measuring job. A missing
	# `needs:` line is folded into the same failure as a `needs:` that
	# names the wrong job, rather than a separate message: either way the
	# job does not run after the measuring job.
	local needs_line needs_value found job
	needs_line="$(grep -E '^    needs: ' <<<"${block}" | head -1 || true)"
	found=0
	if [ -n "${needs_line}" ]; then
		needs_value="${needs_line#    needs: }"
		needs_value="${needs_value#\[}"
		needs_value="${needs_value%\]}"
		IFS=',' read -ra needs_jobs <<<"${needs_value}"
		for job in "${needs_jobs[@]}"; do
			job="$(printf '%s' "${job}" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
			if [ "${job}" = "${measuring_job}" ]; then
				found=1
				break
			fi
		done
	fi
	[ "${found}" -eq 1 ] ||
		fail "${label}: the history job must run after '${measuring_job}'"

	grep -Fq "    if: ${gate}" <<<"${block}" ||
		fail "${label}: the history job condition must be exactly: ${gate}"

	assert_invoked "scripts/ci/quality-history-append.sh ${lane} " \
		"${label}: the history job must call the shared script with lane '${lane}'" "${block}"

	# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
	grep -Fq 'QUALITY_HISTORY_CREDENTIAL: ${{ secrets.GITHUB_TOKEN }}' <<<"${block}" ||
		fail "${label}: the history job must pass the credential through QUALITY_HISTORY_CREDENTIAL"

	# The event is passed explicitly rather than left to the ambient
	# GITHUB_EVENT_NAME, because the script now refuses an empty event
	# instead of treating it as "probably fine".
	# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
	grep -Fq 'QUALITY_HISTORY_EVENT: ${{ github.event_name }}' <<<"${block}" ||
		fail "${label}: the history job must pass the event through QUALITY_HISTORY_EVENT"

	# The property that makes the split worth having: the job holding the
	# credential executes nothing it could have been made to execute.
	local code
	code="$(strip_comments <<<"${block}")"
	local forbidden
	# Invocations, not job names: `needs: gremlins` is a dependency edge, not
	# something this job executes.
	for forbidden in "actions/setup-go" "gremlins unleash" "scripts/soak.sh" \
		"go test" "go install" "go build" "go run"; do
		if grep -Fq "${forbidden}" <<<"${code}"; then
			fail "${label}: the history job must not run '${forbidden}'; it holds the write credential"
		fi
	done

	local job_permissions
	job_permissions="$(
		awk '
            $0 == "    permissions:" { in_perms = 1; next }
            in_perms && /^    [^[:space:]]/ { in_perms = 0 }
            in_perms { print }
        ' <<<"${block}" | grep -v '^[[:space:]]*$'
	)"
	if [ "${job_permissions}" != "      contents: write" ]; then
		fail "${label}: the history job's permissions must be exactly contents: write (found: $(tr '\n' ';' <<<"${job_permissions}"))"
	fi

	# The measuring job must not have grown a write scope of its own, and
	# neither may anything else in the file.
	#
	# Read through a command substitution rather than a pipe. `grep -q` exits
	# on its first match and closes the pipe, the awk upstream dies of SIGPIPE
	# with status 141, and under `pipefail` the pipeline reports 141 -- so the
	# `if` is false and the violation this test exists to catch goes unreported.
	# It is a race on how much awk had already written, which is why it looked
	# green on one machine and not another. Nothing here needs to stream.
	if grep -q '^    permissions:' <<<"$(job_block "${workflow}" "${measuring_job}")"; then
		fail "${label}: the '${measuring_job}' job must not declare permissions of its own"
	fi

	local write_count
	write_count="$(grep -c '^[[:space:]]*contents: write$' "${workflow}" || true)"
	if [ "${write_count}" -ne 1 ]; then
		fail "${label}: contents: write must appear exactly once in the file, found ${write_count}"
	fi

	grep -Eq '^  contents: read$' "${workflow}" ||
		fail "${label}: the workflow-level permissions must stay contents: read"
}

check_lane "${soak_workflow}" "soak" "soak" "soak"
check_lane "${mutation_workflow}" "gremlins" "mutation" "mutation"
check_lane "${fuzz_workflow}" "deep-fuzz" "fuzz-nightly" "fuzz"

# The mutation matrix hands its numbers over as an artifact, so the record has
# to actually be produced and uploaded or the history job downloads nothing and
# the whole lane records silence.
mutation_gremlins="$(job_block "${mutation_workflow}" "gremlins")"
grep -Fq "      - name: Record this package for the quality history" <<<"${mutation_gremlins}" ||
	fail "mutation: the matrix leg must write its record for the history job"
grep -Fq "      - name: Upload the quality history record" <<<"${mutation_gremlins}" ||
	fail "mutation: the matrix leg must upload its record for the history job"
for step in "Record this package for the quality history" \
	"Upload the quality history record"; do
	step_body="$(step_block "${step}" <<<"${mutation_gremlins}")"
	if [ -z "${step_body}" ]; then
		fail "mutation: expected a '${step}' step in the gremlins job"
		continue
	fi
	grep -Fq "        if: ${gate}" <<<"${step_body}" ||
		fail "mutation: the '${step}' step must carry the same schedule/dispatch gate"
done
if grep -Fq "QUALITY_HISTORY_CREDENTIAL" <<<"$(strip_comments <<<"${mutation_gremlins}")"; then
	fail "mutation: the matrix leg must never see the write credential"
fi

# The gated-mode test has to slurp. A bare `jq -e .` exits 0 for an array, a
# string or a number, and a per-value `jq -e 'type == "object"'` reports only
# the last value in the file, so a report that is two concatenated documents
# passes it. Behaviour is covered by scripts/quality-history-record-test.sh;
# this asserts the literal form, because that is a one-flag edit away from
# coming back and the behavioural test would then be testing the wrong thing.
for weak in "elif jq -e . mutation-report.json" \
	"elif jq -e 'type == \"object\"' mutation-report.json"; do
	if grep -Fq "${weak}" <<<"${mutation_gremlins}"; then
		fail "mutation: the gated-mode test must require exactly one JSON object, not '${weak}'"
	fi
done
# shellcheck disable=SC2016 # asserting the workflow's literal jq program text
grep -Fq "elif jq -e -s 'length == 1 and (.[0] | type == \"object\")' mutation-report.json" <<<"${mutation_gremlins}" ||
	fail "mutation: the gated-mode test must be jq -e -s 'length == 1 and (.[0] | type == \"object\")'"

# PW-2.5. The mutation-survivors record needs both the gating legs' and the
# advisory legs' artifacts, and it has to run before the append that
# consumes it or the history job would push nothing for this lane.
mutation_history="$(job_block "${mutation_workflow}" "history")"
grep -Fq "    needs: [gremlins, mutation-advisory]" <<<"${mutation_history}" ||
	fail "mutation: the history job must need both gremlins and mutation-advisory"
# shellcheck disable=SC2016 # asserting the workflow's literal ${{ }} syntax
grep -Fq 'pattern: mutation-history-*-${{ github.run_id }}' <<<"${mutation_history}" ||
	fail "mutation: the history job must download the per-package records"
# shellcheck disable=SC2016 # asserting the workflow's literal ${{ }} syntax
grep -Fq 'pattern: mutation-advisory-*-${{ github.run_id }}' <<<"${mutation_history}" ||
	fail "mutation: the history job must download the advisory legs' records"
assert_invoked "scripts/ci/mutation-survivors-record.sh" \
	"mutation: the history job must run the survivor identity record script" "${mutation_history}"
assert_invoked "scripts/ci/quality-history-append.sh mutation-survivors" \
	"mutation: the history job must append the mutation-survivors record" "${mutation_history}"

# PW-2.5 finding 3. A mutation-survivors record can reach the spec's 1 MiB
# ceiling, well past Linux's 128 KiB single-argument limit, so this lane's
# append must read the record from a file rather than pass it inline like
# the other four lanes do.
grep -Fq "QUALITY_HISTORY_RECORD_FILE=survivors.json" <<<"${mutation_history}" ||
	fail "mutation: the mutation-survivors append must read the record via QUALITY_HISTORY_RECORD_FILE, not inline argv"
if grep -Fq 'quality-history-append.sh mutation-survivors "' <<<"${mutation_history}"; then
	fail "mutation: the mutation-survivors append must not pass the record as an inline argument"
fi

# PW-2.5 finding 7. Unbounded growth on a monthly cron is a slow leak this
# test is the only thing that would ever catch: nothing downstream re-checks
# the cap once it ships.
grep -Fq "QUALITY_HISTORY_RETAIN=12" <<<"${mutation_history}" ||
	fail "mutation: the mutation-survivors append must cap history with QUALITY_HISTORY_RETAIN=12"

record_line="$(first_invoked_line "scripts/ci/mutation-survivors-record.sh" "${mutation_history}")"
append_line="$(first_invoked_line "scripts/ci/quality-history-append.sh mutation-survivors" "${mutation_history}")"
if [ -n "${record_line}" ] && [ -n "${append_line}" ] && [ "${record_line}" -ge "${append_line}" ]; then
	fail "mutation: the survivor record script must run before the mutation-survivors append"
fi

# The advisory and canary jobs measure things and record nothing. Naming them
# keeps the "only the recording job can write" property from decaying into
# "some job in this file can write".
for job in mutation-advisory gate-canary; do
	if grep -q '^    permissions:' <<<"$(job_block "${mutation_workflow}" "${job}")"; then
		fail "mutation: the '${job}' job must not declare permissions of its own"
	fi
done

# --- the appender's own second lock ------------------------------------------
#
# The workflow `if:` is the first gate and a copy-paste away from being wrong.
# The script refuses a non-scheduled event on its own, which is what a lane
# added later still hits even if its author forgets the condition. The empty
# case is refused too: allowing it made the gate fail open exactly when a
# caller forgot to pass the event.

grep -Eq '^schedule \| workflow_dispatch\) ;;$' "${append_script}" ||
	fail "the append script must refuse any event that is not schedule or workflow_dispatch"

if grep -Eq '^schedule \| workflow_dispatch \| ""\) ;;$' "${append_script}"; then
	fail "the append script must not treat an empty event as permission to record"
fi

grep -Fq 'trap soft_exit EXIT' "${append_script}" ||
	fail "the append script must convert its own failures into a warning, never a nonzero exit"

# The trap runs under errexit, so a cleanup failure inside it would abort
# before `exit 0` and hand the caller the nonzero status the trap exists to
# swallow.
#
# Matched as a fixed whole line with a real tab rather than as `\t` in an ERE.
# POSIX ERE has no `\t`: GNU grep reads it as a literal `t`, so the pattern
# silently becomes `^tset +e$` and the assertion passes on every input. It went
# green on macOS only because this machine's `grep` is ugrep, which does accept
# `\t`. Nothing here needs a regex, so nothing here uses one.
grep -Fxq "${tab}set +e" "${append_script}" ||
	fail "the append script's exit trap must disable errexit before cleaning up"

# A duplicate row is indistinguishable from a real second measurement once it
# is in the series, and a push that lands but loses its response produces one.
# Read via `-f` from the record file rather than as a `-Fxq` argv pattern, so
# a record up to the spec's 1 MiB ceiling never has to fit in a single argv
# string (PW-2.5 finding 3) -- see QUALITY_HISTORY_RECORD_FILE above.
# shellcheck disable=SC2016 # Asserting the literal text of the append script.
grep -Fq 'grep -Fxqf "${record_tmp}"' "${append_script}" ||
	fail "the append script must not append a record the branch already carries"

# seq is not guaranteed present; an empty expansion would skip the retry loop
# entirely and exit 0 having recorded nothing.
if grep -Fq 'seq 1' <<<"$(strip_comments <"${append_script}")"; then
	fail "the append script's retry loop must not depend on seq"
fi

[ -x "${append_script}" ] ||
	fail "the append script must be executable"

if [ "${failures}" -ne 0 ]; then
	echo "${failures} quality-history contract check(s) failed" >&2
	exit 1
fi

echo "Quality history contract checks passed."
