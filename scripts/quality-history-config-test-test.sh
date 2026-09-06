#!/usr/bin/env bash
#
# Self-test for scripts/quality-history-config-test.sh.
#
# A contract test that cannot fail is worse than no contract test: it reports
# green forever and everyone stops looking. Each case below breaks one
# property in a copy of the real files and asserts the contract rejects it by
# name, so the checks are known to be load-bearing rather than merely present.

set -euo pipefail
export LC_ALL=C

test_root="$(mktemp -d "${TMPDIR:-/tmp}/portwing-quality-history-contract.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

tab="$(printf '\t')"
contract="scripts/quality-history-config-test.sh"
soak="${test_root}/soak.yml"
mutation="${test_root}/mutation.yml"
fuzz="${test_root}/fuzz.yml"
append="${test_root}/append.sh"

reset_fixtures() {
	cp .github/workflows/quality-soak-weekly.yml "${soak}"
	cp .github/workflows/quality-mutation-monthly.yml "${mutation}"
	cp .github/workflows/quality-fuzz-nightly.yml "${fuzz}"
	cp scripts/ci/quality-history-append.sh "${append}"
	chmod +x "${append}"
}

assert_passes() {
	if ! bash "${contract}" "${soak}" "${mutation}" "${fuzz}" "${append}" >/dev/null 2>&1; then
		echo "FAIL: $1" >&2
		bash "${contract}" "${soak}" "${mutation}" "${fuzz}" "${append}" >&2 || true
		exit 1
	fi
}

assert_rejected() {
	local expected="$1"
	local message="$2"
	local output
	local status

	set +e
	output="$(bash "${contract}" "${soak}" "${mutation}" "${fuzz}" "${append}" 2>&1)"
	status=$?
	set -e

	if [ "${status}" -eq 0 ] || ! grep -Fq "${expected}" <<<"${output}"; then
		echo "FAIL: ${message}" >&2
		echo "--- actual output ---" >&2
		echo "${output}" >&2
		exit 1
	fi
}

# The real files must pass their own contract.
reset_fixtures
assert_passes "the real workflows and append script must pass their own contract"

# --- the recording job itself ------------------------------------------------

reset_fixtures
sed -i.bak '/^    needs: soak$/d' "${soak}"
assert_rejected \
	"the history job must run after 'soak'" \
	"contract must reject a history job that no longer waits for the lane it records"

reset_fixtures
sed -i.bak '/^      - name: Append the run to the quality history$/,$d' "${soak}"
assert_rejected \
	"must call the shared script with lane 'soak'" \
	"contract must reject a soak lane that stopped appending"

# The event gate is dropped, so a pull_request or push run would record.
reset_fixtures
sed -i.bak \
	"s/^    if: always() && (github.event_name == 'schedule' || github.event_name == 'workflow_dispatch')\$/    if: always()/" \
	"${soak}"
assert_rejected \
	"the history job condition must be exactly" \
	"contract must reject a history job with no event gate"

# The gate is inverted into a conjunction, which no run can satisfy.
reset_fixtures
sed -i.bak \
	"s/github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'/github.event_name == 'schedule' \&\& github.event_name == 'workflow_dispatch'/" \
	"${mutation}"
assert_rejected \
	"the history job condition must be exactly" \
	"contract must reject a gate whose || became &&"

# The job writes into the wrong lane's series.
reset_fixtures
sed -i.bak 's#quality-history-append.sh soak #quality-history-append.sh bench #' "${soak}"
assert_rejected \
	"must call the shared script with lane 'soak'" \
	"contract must reject a soak job recording into another lane's file"

# The credential is no longer handed to the job, so every push would fail
# silently behind the appender's own warning.
reset_fixtures
sed -i.bak '/^          QUALITY_HISTORY_CREDENTIAL:/d' "${mutation}"
assert_rejected \
	"must pass the credential through QUALITY_HISTORY_CREDENTIAL" \
	"contract must reject a history job with no credential"

# The event is left to the ambient GITHUB_EVENT_NAME, which the script now
# refuses when empty.
reset_fixtures
sed -i.bak '/^          QUALITY_HISTORY_EVENT:/d' "${soak}"
assert_rejected \
	"must pass the event through QUALITY_HISTORY_EVENT" \
	"contract must reject a history job that does not pass the event explicitly"

# --- the reason the job is separate at all -----------------------------------
#
# The whole justification for a second job is that the one holding the
# credential executes nothing. A toolchain appearing in it undoes that
# silently, so the contract has to see it.

reset_fixtures
sed -i.bak 's|^      - name: Download the soak output$|      - name: Setup Go\n        uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e\n&|' "${soak}"
assert_rejected \
	"the history job must not run 'actions/setup-go'" \
	"contract must reject a toolchain in the job that holds the credential"

reset_fixtures
sed -i.bak 's|^      - name: Download the per-package records$|      - name: Sneak a build in\n        run: go build ./...\n&|' "${mutation}"
assert_rejected \
	"the history job must not run 'go build'" \
	"contract must reject product code running in the job that holds the credential"

# --- write scoping -----------------------------------------------------------

# contents: write promoted to the workflow level, where it covers every job.
reset_fixtures
sed -i.bak 's/^permissions:$/permissions:\n  contents: write/' "${soak}"
assert_rejected \
	"contents: write must appear exactly once in the file" \
	"contract must reject a workflow-level contents: write"

# The recording job loses its scoped write and falls back to the workflow
# default, which cannot push.
reset_fixtures
sed -i.bak 's/^      contents: write$/      contents: read/' "${mutation}"
assert_rejected \
	"history job's permissions must be exactly contents: write" \
	"contract must reject a history job that cannot write"

# The recording job's grant is widened beyond what it needs.
reset_fixtures
sed -i.bak 's/^      contents: write$/&\n      id-token: write/' "${soak}"
assert_rejected \
	"history job's permissions must be exactly contents: write" \
	"contract must reject a widened grant on the history job"

# The measuring job takes a permissions block of its own.
reset_fixtures
sed -i.bak 's/^  soak:$/&\n    permissions:\n      contents: write/' "${soak}"
assert_rejected \
	"the 'soak' job must not declare permissions of its own" \
	"contract must reject a write scope back on the four-hour soak job"

reset_fixtures
sed -i.bak 's/^  gremlins:$/&\n    permissions:\n      contents: read/' "${mutation}"
assert_rejected \
	"the 'gremlins' job must not declare permissions of its own" \
	"contract must reject a permissions block on the Gremlins matrix"

reset_fixtures
sed -i.bak 's/^  gate-canary:$/&\n    permissions:\n      contents: read/' "${mutation}"
assert_rejected \
	"the 'gate-canary' job must not declare permissions of its own" \
	"contract must reject a permissions block on a job that never records"

# The workflow default stops being read-only.
reset_fixtures
sed -i.bak 's/^  contents: read$/  packages: read/' "${soak}"
assert_rejected \
	"workflow-level permissions must stay contents: read" \
	"contract must reject a workflow default that is no longer contents: read"

# --- the mutation matrix's half of the handover ------------------------------

reset_fixtures
sed -i.bak '/^      - name: Record this package for the quality history$/d' "${mutation}"
assert_rejected \
	"the matrix leg must write its record for the history job" \
	"contract must reject a matrix that stopped producing records"

reset_fixtures
sed -i.bak '/^      - name: Upload the quality history record$/d' "${mutation}"
assert_rejected \
	"the matrix leg must upload its record for the history job" \
	"contract must reject a record the history job can never download"

reset_fixtures
sed -i.bak 's|^          PACKAGE_NAME: ${{ matrix.name }}$|&\n          QUALITY_HISTORY_CREDENTIAL: ${{ secrets.GITHUB_TOKEN }}|' "${mutation}"
assert_rejected \
	"the matrix leg must never see the write credential" \
	"contract must reject a credential handed back to the Gremlins matrix"

# The gated-mode test, weakened two ways. `jq -e .` accepts any JSON value; the
# per-value type test is the subtler trap, because jq reflects only the last
# value in the file and two concatenated documents walk straight through it.
reset_fixtures
sed -i.bak "s@^          elif jq -e -s .*@          elif jq -e . mutation-report.json >/dev/null 2>\&1; then@" "${mutation}"
assert_rejected \
	"the gated-mode test must require exactly one JSON object" \
	"contract must reject a gated-mode test that accepts any JSON value"

reset_fixtures
sed -i.bak "s@^          elif jq -e -s .*@          elif jq -e 'type == \"object\"' mutation-report.json >/dev/null 2>\&1; then@" "${mutation}"
assert_rejected \
	"the gated-mode test must require exactly one JSON object" \
	"contract must reject a per-value type test that a multi-document report passes"

# Each recording step carries its own gate. Asserting the gate against the job
# block counts it present when any one step has it, so both are stripped
# separately here.
strip_gate_from_step() {
	awk -v want="      - name: $1" '
        $0 == want { in_step = 1 }
        in_step && /^        if: always\(\)/ { in_step = 0; next }
        { print }
    ' "${mutation}" >"${mutation}.stripped" && mv "${mutation}.stripped" "${mutation}"
}

reset_fixtures
strip_gate_from_step "Upload the quality history record"
assert_rejected \
	"the 'Upload the quality history record' step must carry the same schedule/dispatch gate" \
	"contract must reject a gate stripped from the upload step alone"

reset_fixtures
strip_gate_from_step "Record this package for the quality history"
assert_rejected \
	"the 'Record this package for the quality history' step must carry the same schedule/dispatch gate" \
	"contract must reject a gate stripped from the record step alone"

# --- the appender's own locks ------------------------------------------------

reset_fixtures
sed -i.bak 's/^schedule | workflow_dispatch) ;;$/schedule | workflow_dispatch | "") ;;/' "${append}"
assert_rejected \
	"must not treat an empty event as permission to record" \
	"contract must reject an appender whose event gate fails open"

reset_fixtures
sed -i.bak '/^trap soft_exit EXIT$/d' "${append}"
assert_rejected \
	"must convert its own failures into a warning" \
	"contract must reject an appender that can fail a quality lane"

reset_fixtures
sed -i.bak "/^${tab}set +e\$/d" "${append}"
assert_rejected \
	"exit trap must disable errexit before cleaning up" \
	"contract must reject an exit trap that can itself fail"

reset_fixtures
# shellcheck disable=SC2016 # A sed program over the append script's literal text.
sed -i.bak 's/grep -Fxqf "${record_tmp}"/grep -Fq "nothing-like-it"/' "${append}"
assert_rejected \
	"must not append a record the branch already carries" \
	"contract must reject an appender that can write a duplicate row"

reset_fixtures
# shellcheck disable=SC2016 # A sed program over the append script's literal text.
sed -i.bak 's/^while \[ "${attempt}" -le "${attempts}" \]; do$/for attempt in $(seq 1 "${attempts}"); do/' "${append}"
assert_rejected \
	"retry loop must not depend on seq" \
	"contract must reject a retry loop that silently runs zero times"

reset_fixtures
chmod -x "${append}"
assert_rejected \
	"append script must be executable" \
	"contract must reject a non-executable append script"

echo "Quality history contract self-tests passed."
