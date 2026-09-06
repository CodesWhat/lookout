#!/usr/bin/env bash
# Contract checks for the ClusterFuzzLite tier: the two workflows, the build
# integration, and the corpus-branch wiring that ties them together.
#
# The fuzz-target inventory itself is NOT checked here. That lives in
# scripts/fuzz-tier-config-test.sh, which owns "every tier runs the same ten
# targets" across lefthook, ci-verify, nightly, monthly and .clusterfuzzlite.
# This file owns the shape of the ClusterFuzzLite lanes: permissions, pins,
# budgets, sanitizer, trigger, schedule, and where the corpus goes.
set -euo pipefail

pr_workflow="${1:-.github/workflows/quality-fuzz-cflite-pr.yml}"
batch_workflow="${2:-.github/workflows/quality-fuzz-cflite-batch.yml}"
prune_workflow="${3:-.github/workflows/quality-fuzz-cflite-prune.yml}"
dockerfile="${4:-.clusterfuzzlite/Dockerfile}"
build_script="${5:-.clusterfuzzlite/build.sh}"
# Read only for their cron lines, to derive the schedules these lanes must stay
# off. Nothing else about them is this file's business.
nightly_workflow="${6:-.github/workflows/quality-fuzz-nightly.yml}"
monthly_workflow="${7:-.github/workflows/quality-fuzz-monthly.yml}"

corpus_branch="clusterfuzzlite-corpus"
corpus_concurrency_group="quality-fuzz-cflite-corpus"
batch_cron='30 10 * * 6'
prune_cron='30 10 * * 3'

failures=0

fail() {
	echo "FAIL: $1" >&2
	failures=$((failures + 1))
}

for required in "${pr_workflow}" "${batch_workflow}" "${prune_workflow}" "${dockerfile}" \
	"${build_script}" "${nightly_workflow}" "${monthly_workflow}"; do
	if [ ! -f "${required}" ]; then
		fail "file not found: ${required}"
		echo "${failures} ClusterFuzzLite contract check(s) failed" >&2
		exit 1
	fi
done

# Prints the body of a top-level `jobs:` entry, from its own two-space key to
# the next one. Every per-job assertion reads from this rather than from the
# whole file, so a correct block belonging to the OTHER job cannot satisfy a
# check about this one.
job_block() {
	local workflow="$1"
	local job="$2"

	awk -v job="  ${job}:" '
		$0 == job { in_job = 1; next }
		in_job && /^  [^[:space:]]/ { in_job = 0 }
		in_job { print }
	' "${workflow}"
}

# Prints a job's `permissions:` block, scoped the same way.
job_permissions() {
	job_block "$1" "$2" |
		awk '
			$0 == "    permissions:" { in_perms = 1; next }
			in_perms && /^    [^[:space:]]/ { in_perms = 0 }
			in_perms && !/^[[:space:]]*#/ { print }
		' |
		grep -v '^[[:space:]]*$' || true
}

# Prints the `with:` block of the step whose `id:` is given, scoped to that
# step. Budgets and storage settings have to be on the step that actually runs
# the fuzzers, not merely somewhere in the file.
step_with() {
	local workflow="$1"
	local job="$2"
	local step_id="$3"

	job_block "${workflow}" "${job}" |
		awk -v want="        id: ${step_id}" '
			$0 == want { in_step = 1; next }
			in_step && /^      - / { in_step = 0 }
			in_step { print }
		'
}

assert_permissions() {
	local workflow="$1"
	local job="$2"
	local expected="$3"
	local actual

	actual="$(job_permissions "${workflow}" "${job}")"
	if [ -z "${actual}" ]; then
		fail "${workflow}: job '${job}' must declare its own permissions block"
		return
	fi
	# Exact match, not "contains". A grant this job does not need is as much a
	# violation as a missing one, and only equality catches the added line.
	if [ "${actual}" != "${expected}" ]; then
		fail "${workflow}: job '${job}' permissions must be exactly [$(tr '\n' ';' <<<"${expected}" | tr -s ' ')], found [$(tr '\n' ';' <<<"${actual}" | tr -s ' ')]"
	fi
}

assert_with() {
	local workflow="$1"
	local job="$2"
	local step_id="$3"
	local line="$4"
	local block

	block="$(step_with "${workflow}" "${job}" "${step_id}")"
	if [ -z "${block}" ]; then
		fail "${workflow}: job '${job}' must define a step with id: ${step_id}"
		return
	fi
	grep -Fq "          ${line}" <<<"${block}" ||
		fail "${workflow}: step '${step_id}' in job '${job}' must set ${line}"
}

# --- every `uses:` pinned to a full commit SHA with a version comment --------
#
# The repo-wide convention, asserted per workflow rather than trusting a
# repo-wide sweep, because these two files are the ones that would tempt someone
# into `@v1`: that is the only form the upstream docs show.
for workflow in "${pr_workflow}" "${batch_workflow}" "${prune_workflow}"; do
	while IFS= read -r line; do
		[ -n "${line}" ] || continue
		grep -Eq '^[[:space:]]*uses: [^@]+@[0-9a-f]{40}[[:space:]]+# .+$' <<<"${line}" ||
			fail "${workflow}: unpinned or uncommented action reference: ${line#"${line%%[![:space:]]*}"}"
	done < <(grep -E '^[[:space:]]*uses:' "${workflow}")

	uses_count="$(grep -Ec '^[[:space:]]*uses:' "${workflow}" || true)"
	[ "${uses_count}" -gt 0 ] ||
		fail "${workflow}: expected at least one action reference"
done

# --- both ClusterFuzzLite actions present in every fuzzing job ---------------

for workflow in "${pr_workflow}" "${batch_workflow}" "${prune_workflow}"; do
	grep -Fq "uses: google/clusterfuzzlite/actions/build_fuzzers@" "${workflow}" ||
		fail "${workflow}: must build fuzzers with google/clusterfuzzlite/actions/build_fuzzers"
	grep -Fq "uses: google/clusterfuzzlite/actions/run_fuzzers@" "${workflow}" ||
		fail "${workflow}: must run fuzzers with google/clusterfuzzlite/actions/run_fuzzers"
done

# --- harden-runner, consistent with every other workflow in the repo --------

for workflow in "${pr_workflow}" "${batch_workflow}" "${prune_workflow}"; do
	grep -Fq "uses: step-security/harden-runner@" "${workflow}" ||
		fail "${workflow}: must harden the runner before pulling anything"

	harden_count="$(grep -Fc "uses: step-security/harden-runner@" "${workflow}" || true)"
	block_count="$(grep -Ec '^[[:space:]]*egress-policy: block$' "${workflow}" || true)"
	if [ "${harden_count}" -ne "${block_count}" ]; then
		fail "${workflow}: every harden-runner step must set egress-policy: block (${harden_count} steps, ${block_count} blocks)"
	fi

	# gcr.io and storage.googleapis.com serve the OSS-Fuzz images; the Go proxy
	# and checksum database serve both the modules and the go1.27.0 toolchain
	# that base-builder-go's Go 1.25.0 downloads on first use. Any of the four
	# missing is a silent network failure inside a container, so name them.
	for endpoint in gcr.io:443 storage.googleapis.com:443 proxy.golang.org:443 sum.golang.org:443 github.com:443; do
		occurrences="$(grep -Fc "            ${endpoint}" "${workflow}" || true)"
		if [ "${occurrences}" -ne "${harden_count}" ]; then
			fail "${workflow}: every harden-runner allow-list must include ${endpoint} (${occurrences} of ${harden_count})"
		fi
	done

	# The artifact hosts @actions/artifact needs. storage-repo does not remove
	# the GitHub Actions filestore: get_filestore wraps a GithubActionsFilestore
	# in the GitFilestore, and upload_crashes delegates back to it, so a crash
	# reproducer leaves as a workflow artifact on every lane here. Blocked, the
	# job still reports the crash and loses the input that reproduces it.
	for endpoint in '*.actions.githubusercontent.com:443' 'results-receiver.actions.githubusercontent.com:443' '*.blob.core.windows.net:443'; do
		occurrences="$(grep -Fc "            ${endpoint}" "${workflow}" || true)"
		if [ "${occurrences}" -ne "${harden_count}" ]; then
			fail "${workflow}: every harden-runner allow-list must include ${endpoint} so a crash reproducer can be uploaded (${occurrences} of ${harden_count})"
		fi
	done
done

# --- PR lane: trigger, permissions, budget ----------------------------------

# pull_request_target would hand a fork's code a writable token against this
# repo, and running a fork's code is exactly this lane's job. Matched as a
# trigger key, not as a substring: the header comment names it too.
if grep -Eq '^  pull_request_target:' "${pr_workflow}"; then
	fail "${pr_workflow}: must use pull_request, never pull_request_target"
fi
grep -Eq '^  pull_request:$' "${pr_workflow}" ||
	fail "${pr_workflow}: must trigger on pull_request"

grep -Eq '^permissions:$' "${pr_workflow}" ||
	fail "${pr_workflow}: must declare a top-level permissions block"

assert_permissions "${pr_workflow}" code-change-fuzz \
	$'      contents: read\n      actions: read'

assert_with "${pr_workflow}" code-change-fuzz run "fuzz-seconds: 300"
assert_with "${pr_workflow}" code-change-fuzz run "mode: code-change"
assert_with "${pr_workflow}" code-change-fuzz run "sanitizer: address"
assert_with "${pr_workflow}" code-change-fuzz build "sanitizer: address"
# Without this, build_fuzzers drops targets it cannot prove the diff reaches,
# using coverage reports this repo does not generate.
assert_with "${pr_workflow}" code-change-fuzz build "keep-unaffected-fuzz-targets: true"

# --- scheduled lanes: schedules, budgets, write scope -----------------------

# The schedules these lanes must stay off, read out of the two Go-native
# scheduled fuzz workflows rather than written down here. A literal went stale
# the same day #264 moved the monthly lane from 08:30 to 02:30, which is the
# exact drift this guard exists to catch, so it can't be one.
# Per file, not pooled: if only one of the two stops yielding a cron, the guard
# quietly protects half of what it claims to, and every case still passes.
taken_crons=()
for taken_workflow in "${nightly_workflow}" "${monthly_workflow}"; do
	taken_found=0
	while IFS= read -r taken_cron; do
		[ -n "${taken_cron}" ] || continue
		taken_crons+=("${taken_cron}")
		taken_found=1
	done < <(awk -F"'" '/^[[:space:]]*- cron: /{print $2}' "${taken_workflow}")

	if [ "${taken_found}" -eq 0 ]; then
		fail "no cron found in ${taken_workflow}; the collision guard would silently stop covering that lane"
	fi
done

assert_single_schedule() {
	local workflow="$1"
	local expected_cron="$2"
	local cron_count
	local taken

	grep -Fq -- "- cron: '${expected_cron}'" "${workflow}" ||
		fail "${workflow}: must be scheduled at '${expected_cron}'"

	# Exactly one, so a second schedule cannot quietly put this lane back
	# alongside the one it is supposed to stay away from.
	cron_count="$(grep -Ec '^[[:space:]]*- cron:' "${workflow}" || true)"
	if [ "${cron_count}" -ne 1 ]; then
		fail "${workflow}: expected exactly 1 schedule, found ${cron_count}"
	fi

	for taken in ${taken_crons[@]+"${taken_crons[@]}"}; do
		if grep -Fq -- "- cron: '${taken}'" "${workflow}"; then
			fail "${workflow}: schedule '${taken}' collides with an existing deep-fuzz lane"
		fi
	done
}

assert_single_schedule "${batch_workflow}" "${batch_cron}"
assert_single_schedule "${prune_workflow}" "${prune_cron}"

if [ "${batch_cron}" = "${prune_cron}" ]; then
	fail "batch and prune must not share a schedule"
fi

assert_permissions "${batch_workflow}" batch-fuzz \
	$'      contents: write\n      actions: read'
assert_permissions "${prune_workflow}" prune-corpus \
	$'      contents: write\n      actions: read'

assert_with "${batch_workflow}" batch-fuzz run "fuzz-seconds: 600"
assert_with "${batch_workflow}" batch-fuzz run "mode: batch"
assert_with "${batch_workflow}" batch-fuzz run "sanitizer: address"
assert_with "${prune_workflow}" prune-corpus run "fuzz-seconds: 600"
assert_with "${prune_workflow}" prune-corpus run "mode: prune"
assert_with "${prune_workflow}" prune-corpus run "sanitizer: address"

# The old hour-long budget sustained near-100% CPU past the runner's measured
# 14-19 minute shutdown ceiling and killed the job twice. A regression back to
# it must fail loudly rather than pass because the new value merely coexists
# with the old one somewhere in the file.
if grep -Fq "fuzz-seconds: 3600" "${batch_workflow}"; then
	fail "${batch_workflow}: fuzz-seconds: 3600 exceeds the measured runner ceiling; batch must fuzz 600s total"
fi

# --- batch and prune must never run together --------------------------------
#
# They are separate workflows so quality-lane-notify.yml can tell them apart: it
# keys its tracking issue on the workflow name, and a green prune closing the
# issue a crashing batch opened is a false all-clear. The price of separate
# files is that the concurrency group is no longer shared by construction, so it
# is asserted here instead. Concurrency groups are repository-scoped strings; a
# group derived from the workflow name would differ per file and put both
# writers back on the branch at once.

assert_shared_concurrency() {
	local workflow="$1"
	local job="$2"
	local block

	block="$(
		awk '
			/^concurrency:$/ { in_block = 1; next }
			in_block && /^[^[:space:]]/ { in_block = 0 }
			in_block && !/^[[:space:]]*#/ && !/^[[:space:]]*$/ { print }
		' "${workflow}"
	)"

	if [ -z "${block}" ]; then
		fail "${workflow}: must declare a top-level concurrency block"
		return
	fi

	grep -Fqx "  group: ${corpus_concurrency_group}" <<<"${block}" ||
		fail "${workflow}: concurrency group must be the literal '${corpus_concurrency_group}', shared with the other corpus writer"
	grep -Eq '^  cancel-in-progress: false$' <<<"${block}" ||
		fail "${workflow}: a cancelled corpus write leaves the branch half-updated, so cancel-in-progress must be false"

	# A job-level group overrides the workflow-level one while the block above
	# still reads correct.
	if job_block "${workflow}" "${job}" | grep -Eq '^    concurrency:$'; then
		fail "${workflow}: job '${job}' must not override the shared concurrency group"
	fi
}

assert_shared_concurrency "${batch_workflow}" batch-fuzz
assert_shared_concurrency "${prune_workflow}" prune-corpus

# The PR lane must NOT join that group: it never writes the branch, and putting
# it there would make every pull request queue behind an hour of batch fuzzing.
if grep -Fq "group: ${corpus_concurrency_group}" "${pr_workflow}"; then
	fail "${pr_workflow}: the read-only lane must not share the corpus writers' concurrency group"
fi

# --- corpus storage: this repo, that branch, no new secret ------------------

for workflow in "${pr_workflow}" "${batch_workflow}" "${prune_workflow}"; do
	# One run_fuzzers step per mode, counted at the `with:` indent so the
	# workflow_dispatch input named `mode` cannot inflate it.
	run_steps="$(grep -Ec '^          mode: ' "${workflow}" || true)"

	# shellcheck disable=SC2016 # Asserting the literal text of the file under test.
	storage_repos="$(grep -Fc 'storage-repo: https://x-access-token:${{ github.token }}@github.com/${{ github.repository }}.git' "${workflow}" || true)"
	if [ "${storage_repos}" -ne "${run_steps}" ]; then
		fail "${workflow}: every run_fuzzers step must store the corpus in THIS repo with github.token (${storage_repos} of ${run_steps})"
	fi

	branches="$(grep -Fc "storage-repo-branch: ${corpus_branch}" "${workflow}" || true)"
	if [ "${branches}" -ne "${run_steps}" ]; then
		fail "${workflow}: every run_fuzzers step must use storage-repo-branch: ${corpus_branch} (${branches} of ${run_steps})"
	fi

	# Left at its default this is `gh-pages`, and a coverage run would create a
	# GitHub Pages branch in this repository as a side effect.
	coverage_branches="$(grep -Fc "storage-repo-branch-coverage: ${corpus_branch}" "${workflow}" || true)"
	if [ "${coverage_branches}" -ne "${run_steps}" ]; then
		fail "${workflow}: every run_fuzzers step must pin storage-repo-branch-coverage to ${corpus_branch} (${coverage_branches} of ${run_steps})"
	fi

	# No PAT, no repository secret, no second repo. That is the design.
	if grep -Fq 'secrets.PERSONAL_ACCESS_TOKEN' "${workflow}"; then
		fail "${workflow}: corpus persistence must not require a personal access token"
	fi
done

# --- build integration ------------------------------------------------------

grep -Eq '^FROM gcr\.io/oss-fuzz-base/base-builder-go:v1@sha256:[0-9a-f]{64}$' "${dockerfile}" ||
	fail "${dockerfile}: the builder image must be pinned by digest, like every other Dockerfile here"

# shellcheck disable=SC2016 # Asserting the literal text of the file under test.
grep -Fq 'COPY ./.clusterfuzzlite/build.sh $SRC/' "${dockerfile}" ||
	fail "${dockerfile}: must copy build.sh to \$SRC where ClusterFuzzLite looks for it"

# compile_native_go_fuzzer exits 0 and only prints a message when it cannot find
# `func <name>(f *testing.F)`. Without a check on the binary, a renamed target
# turns into a silently shorter fuzzer list instead of a failed build.
# shellcheck disable=SC2016 # Asserting the literal text of the file under test.
grep -Fq 'if [ ! -x "${OUT}/${fuzzer}" ]; then' "${build_script}" ||
	fail "${build_script}: must fail the build when compile_native_go_fuzzer produces no binary"

grep -Fq 'compile_native_go_fuzzer' "${build_script}" ||
	fail "${build_script}: must build the native Go testing.F targets with compile_native_go_fuzzer"

# go-118-fuzz-build's generated entry point imports the shim package, so it has
# to resolve against our go.mod or every target fails to compile.
# shellcheck disable=SC2016 # Asserting the literal text of the file under test.
grep -Fq 'go get "${shim_module}/testing@${shim_version}"' "${build_script}" ||
	fail "${build_script}: must add the go-118-fuzz-build testing shim at the generator's own version"

if [ "${failures}" -ne 0 ]; then
	echo "${failures} ClusterFuzzLite contract check(s) failed" >&2
	exit 1
fi

echo "ClusterFuzzLite contract checks passed."
