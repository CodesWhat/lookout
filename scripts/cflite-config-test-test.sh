#!/usr/bin/env bash
# Self-tests for scripts/cflite-config-test.sh.
#
# Each case mutates a copy of a real file in exactly one way and asserts the
# contract notices. A contract that only ever sees the passing case proves
# nothing about the failures it was written to catch.
set -euo pipefail

test_root="$(mktemp -d "${TMPDIR:-/tmp}/portwing-cflite.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

mkdir -p "${test_root}/scripts"
cp scripts/cflite-config-test.sh "${test_root}/scripts/"

pr_fixture="${test_root}/pr.yml"
batch_fixture="${test_root}/batch.yml"
prune_fixture="${test_root}/prune.yml"
dockerfile_fixture="${test_root}/Dockerfile"
build_fixture="${test_root}/build.sh"
nightly_fixture="${test_root}/nightly.yml"
monthly_fixture="${test_root}/monthly.yml"

reset_fixtures() {
	cp .github/workflows/quality-fuzz-cflite-pr.yml "${pr_fixture}"
	cp .github/workflows/quality-fuzz-cflite-batch.yml "${batch_fixture}"
	cp .github/workflows/quality-fuzz-cflite-prune.yml "${prune_fixture}"
	cp .clusterfuzzlite/Dockerfile "${dockerfile_fixture}"
	cp .clusterfuzzlite/build.sh "${build_fixture}"
	cp .github/workflows/quality-fuzz-nightly.yml "${nightly_fixture}"
	cp .github/workflows/quality-fuzz-monthly.yml "${monthly_fixture}"
}

# BSD and GNU sed disagree about `\n` in a replacement, and this suite has to
# behave identically under the pre-push hook on macOS and under CI on Linux.
# Multi-line inserts go through awk instead.
insert_before() {
	local file="$1"
	local anchor="$2"
	shift 2

	# awk's -v expands backslash escapes but cannot take a literal newline, so
	# the lines are joined with the two-character sequence and expanded there.
	local inserted=""
	local line
	for line in "$@"; do
		if [ -z "${inserted}" ]; then
			inserted="${line}"
		else
			inserted="${inserted}\\n${line}"
		fi
	done

	awk -v anchor="${anchor}" -v inserted="${inserted}" '
		!done && $0 == anchor { print inserted; done = 1 }
		{ print }
	' "${file}" >"${file}.tmp"
	mv "${file}.tmp" "${file}"
}

run_contract() {
	(cd "${test_root}" && bash scripts/cflite-config-test.sh \
		pr.yml batch.yml prune.yml Dockerfile build.sh \
		nightly.yml monthly.yml 2>&1)
}

assert_passes() {
	local failure_message="$1"
	local output
	local status

	set +e
	output="$(run_contract)"
	status=$?
	set -e

	if [ "${status}" -ne 0 ]; then
		echo "FAIL: ${failure_message}" >&2
		echo "--- actual output ---" >&2
		echo "${output}" >&2
		exit 1
	fi
}

assert_rejected() {
	local expected="$1"
	local failure_message="$2"
	local output
	local status

	set +e
	output="$(run_contract)"
	status=$?
	set -e

	if [ "${status}" -eq 0 ] || ! grep -Fq "${expected}" <<<"${output}"; then
		echo "FAIL: ${failure_message}" >&2
		echo "--- actual output ---" >&2
		echo "${output}" >&2
		exit 1
	fi
}

# The real files must pass their own contract.
reset_fixtures
assert_passes "the real ClusterFuzzLite configuration must pass its own contract"

# --- pinning -----------------------------------------------------------------

# The upstream docs only ever show `@v1`, so this is the mistake most likely to
# be made by someone copying from them.
reset_fixtures
sed -i.bak 's|uses: google/clusterfuzzlite/actions/run_fuzzers@[0-9a-f]*  # v1|uses: google/clusterfuzzlite/actions/run_fuzzers@v1|' "${pr_fixture}"
assert_rejected \
	"unpinned or uncommented action reference" \
	"contract must reject a floating @v1 action reference"

# A SHA with no version comment leaves nobody able to tell what it is.
reset_fixtures
sed -i.bak 's|@884713a6c30a92e5e8544c39945cd7cb630abcd1  # v1|@884713a6c30a92e5e8544c39945cd7cb630abcd1|' "${batch_fixture}"
assert_rejected \
	"unpinned or uncommented action reference" \
	"contract must reject a pinned SHA with no version comment"

# --- the PR lane's trigger and permissions -----------------------------------

# pull_request_target runs a fork's code with a writable token against this repo.
reset_fixtures
sed -i.bak 's|^  pull_request:$|  pull_request_target:|' "${pr_fixture}"
assert_rejected \
	"must use pull_request, never pull_request_target" \
	"contract must reject pull_request_target on the fork-facing lane"

# The code-change lane never writes; a write grant here is a widening, not a fix.
reset_fixtures
sed -i.bak 's|^      contents: read$|      contents: write|' "${pr_fixture}"
assert_rejected \
	"job 'code-change-fuzz' permissions must be exactly" \
	"contract must reject a code-change job that can write to the repo"

reset_fixtures
insert_before "${pr_fixture}" "      actions: read" "      id-token: write"
assert_rejected \
	"job 'code-change-fuzz' permissions must be exactly" \
	"contract must reject a code-change job that grants id-token: write"

# --- budgets, mode, sanitizer ------------------------------------------------

reset_fixtures
sed -i.bak 's|fuzz-seconds: 300|fuzz-seconds: 30|' "${pr_fixture}"
assert_rejected \
	"must set fuzz-seconds: 300" \
	"contract must reject a changed code-change budget"

reset_fixtures
sed -i.bak 's|fuzz-seconds: 600|fuzz-seconds: 300|' "${batch_fixture}"
assert_rejected \
	"must set fuzz-seconds: 600" \
	"contract must reject a batch budget that is no longer 600s"

# The old hour-long budget is not merely "some other value"; it is the exact
# value that was measured to exceed the runner's CPU ceiling and kill the job.
reset_fixtures
sed -i.bak 's|fuzz-seconds: 600|fuzz-seconds: 3600|' "${batch_fixture}"
assert_rejected \
	"exceeds the measured runner ceiling" \
	"contract must reject a batch budget reverted to the old 3600s"

# Swapping the two scheduled modes reads plausible and silently stops the corpus
# ever growing: prune only minimizes what batch found.
reset_fixtures
sed -i.bak 's|^          mode: batch$|          mode: prune|' "${batch_fixture}"
assert_rejected \
	"must set mode: batch" \
	"contract must reject a batch lane that runs the prune mode"

# Go builds only support the address sanitizer; anything else builds nothing.
reset_fixtures
sed -i.bak 's|^          sanitizer: address$|          sanitizer: undefined|' "${pr_fixture}"
assert_rejected \
	"must set sanitizer: address" \
	"contract must reject a sanitizer Go cannot build"

# Without this, build_fuzzers drops targets it cannot prove the diff reaches,
# from coverage reports this repo does not produce.
reset_fixtures
sed -i.bak '/keep-unaffected-fuzz-targets: true/d' "${pr_fixture}"
assert_rejected \
	"must set keep-unaffected-fuzz-targets: true" \
	"contract must reject a PR build that can silently drop every target"

# --- corpus persistence ------------------------------------------------------

reset_fixtures
sed -i.bak 's|storage-repo-branch: clusterfuzzlite-corpus|storage-repo-branch: main|' "${batch_fixture}"
assert_rejected \
	"must use storage-repo-branch: clusterfuzzlite-corpus" \
	"contract must reject a corpus written to a branch that is not the corpus branch"

# Left at its default this is gh-pages, and a coverage run would create a
# GitHub Pages branch in this repository as a side effect.
reset_fixtures
sed -i.bak '/storage-repo-branch-coverage: clusterfuzzlite-corpus/d' "${pr_fixture}"
assert_rejected \
	"must pin storage-repo-branch-coverage to clusterfuzzlite-corpus" \
	"contract must reject a coverage branch left at its gh-pages default"

# One lane keeping its storage repo while another loses it is the realistic
# version of this, and it would leave prune minimizing an empty local corpus.
reset_fixtures
sed -i.bak '/storage-repo: https:\/\/x-access-token/d' "${prune_fixture}"
assert_rejected \
	"must store the corpus in THIS repo with github.token" \
	"contract must reject a run step that lost its storage repo"

# The design constraint: no new secret.
reset_fixtures
sed -i.bak 's|https://x-access-token:${{ github.token }}@github.com/${{ github.repository }}.git|https://${{ secrets.PERSONAL_ACCESS_TOKEN }}@github.com/CodesWhat/portwing-corpus.git|' "${pr_fixture}"
assert_rejected \
	"must not require a personal access token" \
	"contract must reject a corpus store that needs a new secret"

# --- batch and prune must never run together ---------------------------------

reset_fixtures
sed -i.bak 's|^  cancel-in-progress: false$|  cancel-in-progress: true|' "${batch_fixture}"
assert_rejected \
	"cancel-in-progress must be false" \
	"contract must reject cancelling a run mid-corpus-write"

# The group derived per workflow instead of shared. Each file still reads
# correct on its own; only comparing both against the literal catches it.
reset_fixtures
sed -i.bak 's|^  group: quality-fuzz-cflite-corpus$|  group: quality-fuzz-cflite-corpus-${{ github.workflow }}|' "${prune_fixture}"
assert_rejected \
	"concurrency group must be the literal 'quality-fuzz-cflite-corpus'" \
	"contract must reject a per-workflow concurrency group for the corpus writers"

# A job-level group overrides the shared one and puts the two writers back in
# parallel, while the workflow-level block still reads correct.
reset_fixtures
insert_before "${batch_fixture}" "    runs-on: ubuntu-24.04" \
	"    concurrency:" "      group: batch-only"
assert_rejected \
	"must not override the shared concurrency group" \
	"contract must reject a job-level concurrency group"

# The read-only lane joining the writers' group would queue every pull request
# behind an hour of batch fuzzing.
reset_fixtures
sed -i.bak 's|^  group: quality-fuzz-cflite-pr-.*$|  group: quality-fuzz-cflite-corpus|' "${pr_fixture}"
assert_rejected \
	"must not share the corpus writers' concurrency group" \
	"contract must reject the PR lane joining the corpus concurrency group"

# --- schedule ----------------------------------------------------------------

reset_fixtures
sed -i.bak "s|- cron: '30 10 \* \* 6'|- cron: '30 9 * * *'|" "${batch_fixture}"
assert_rejected \
	"collides with an existing deep-fuzz lane" \
	"contract must reject a schedule that lands on the nightly deep-fuzz slot"

# Prune moved onto the batch day. The shared concurrency group still serializes
# them, so nothing corrupts; it just wastes most of a week of corpus growth.
reset_fixtures
sed -i.bak "s|- cron: '30 10 \* \* 3'|- cron: '30 10 * * 6'|" "${prune_fixture}"
assert_rejected \
	"must be scheduled at '30 10 * * 3'" \
	"contract must reject prune moved onto the batch day"

# A second schedule added alongside the first.
reset_fixtures
insert_before "${batch_fixture}" "  workflow_dispatch:" "    - cron: '0 3 * * 1'"
assert_rejected \
	"expected exactly 1 schedule, found 2" \
	"contract must reject a second schedule slipped into a lane"

# The monthly deep-fuzz slot, which is 02:30 on day 1 rather than the 08:30 it
# used to be. A hardcoded taken-list in the contract went stale the day that
# moved and let this through; the list is read out of the workflow now.
reset_fixtures
sed -i.bak "s|- cron: '30 10 \* \* 3'|- cron: '30 2 1 * *'|" "${prune_fixture}"
assert_rejected \
	"collides with an existing deep-fuzz lane" \
	"contract must reject a schedule that lands on the monthly deep-fuzz slot"

# The taken-list is only as good as the files it reads. One of the two going
# quiet must be an error, not a smaller list that still passes everything.
reset_fixtures
grep -v "^[[:space:]]*- cron: " "${monthly_fixture}" >"${monthly_fixture}.tmp"
mv "${monthly_fixture}.tmp" "${monthly_fixture}"
assert_rejected \
	"no cron found in monthly.yml" \
	"a monthly workflow missing its cron must not silently shrink the collision guard"

# --- egress ------------------------------------------------------------------

# A lane losing a host from its allow-list fails as an opaque timeout inside a
# container, which is why it is asserted here and not left to the run to find.
reset_fixtures
sed -i.bak '/            gcr.io:443/d' "${prune_fixture}"
assert_rejected \
	"allow-list must include gcr.io:443" \
	"contract must reject a harden-runner step dropping the registry host"

reset_fixtures
sed -i.bak 's|^          egress-policy: block$|          egress-policy: audit|' "${pr_fixture}"
assert_rejected \
	"must set egress-policy: block" \
	"contract must reject an egress policy that only audits"

# The artifact hosts are the easiest ones to think this lane does not need,
# because storage-repo makes the corpus go to a branch. The crash reproducer
# still leaves as a workflow artifact, on every lane, including the read-only
# PR one.
reset_fixtures
sed -i.bak '/            \*.blob.core.windows.net:443/d' "${pr_fixture}"
assert_rejected \
	"so a crash reproducer can be uploaded" \
	"contract must reject a lane that drops the artifact blob host"

reset_fixtures
sed -i.bak '/            results-receiver.actions.githubusercontent.com:443/d' "${batch_fixture}"
assert_rejected \
	"must include results-receiver.actions.githubusercontent.com:443" \
	"contract must reject a lane that drops the artifact results receiver"

# --- build integration -------------------------------------------------------

reset_fixtures
sed -i.bak 's|^FROM gcr.io/oss-fuzz-base/base-builder-go:v1@sha256:[0-9a-f]*$|FROM gcr.io/oss-fuzz-base/base-builder-go:v1|' "${dockerfile_fixture}"
assert_rejected \
	"builder image must be pinned by digest" \
	"contract must reject an unpinned builder image"

# compile_native_go_fuzzer exits 0 when it cannot find the function, so without
# this guard a renamed target becomes a shorter fuzzer list, not a failed build.
reset_fixtures
# shellcheck disable=SC2016 # Asserting the literal text of the file under test.
sed -i.bak 's|if \[ ! -x "${OUT}/${fuzzer}" \]; then|if false; then|' "${build_fixture}"
assert_rejected \
	"must fail the build when compile_native_go_fuzzer produces no binary" \
	"contract must reject a build script that cannot notice a missing binary"

reset_fixtures
# shellcheck disable=SC2016 # Asserting the literal text of the file under test.
sed -i.bak '/go get "${shim_module}\/testing@${shim_version}"/d' "${build_fixture}"
assert_rejected \
	"must add the go-118-fuzz-build testing shim" \
	"contract must reject a build script missing the go-118-fuzz-build shim"

echo "ClusterFuzzLite contract self-tests passed."
