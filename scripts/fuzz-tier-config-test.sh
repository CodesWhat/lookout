#!/usr/bin/env bash
set -euo pipefail

failures=0

fail() {
	echo "FAIL: $1" >&2
	failures=$((failures + 1))
}

escape_ere() {
	printf '%s\n' "$1" | sed 's/[][(){}.^$*+?|\\]/\\&/g'
}

fuzzers=(
	"FuzzParsePHC|./internal/server/"
	"FuzzParseTrustedProxies|./internal/server/"
	"FuzzParseImageRef|./internal/adapter/"
	"FuzzParseLabels|./internal/adapter/drydock/"
	"FuzzMCPHandler|./internal/mcp/"
	"FuzzEnvelope|./internal/protocol/"
	"FuzzVerifyRequest|./internal/auth/"
	"FuzzDecodeContainerLogStream|./internal/docker/"
	"FuzzComposeRequestValidate|./internal/docker/"
	"FuzzParseKeyLine|./internal/auth/"
)

# --- seed-replay tier (PW-7.37) ----------------------------------------------
#
# FuzzParseImageRef and FuzzParseLabels are saturated: 5.5M and 6.8M
# executions logged with zero new interesting inputs, and their path
# derivation was independently verified correct, so this is real saturation,
# not a bug. Fuzzing them further spends nightly/monthly minutes finding
# nothing while targets that still produce get the same budget. This is the
# ONE list that decides tier membership; every check below that cares about
# the split works outward from it, the same way `fuzzers` above is the one
# list every other check works outward from.
replay_tier_fuzzers=(
	"FuzzParseImageRef"
	"FuzzParseLabels"
)

is_replay_tier() {
	local name="$1" rt
	for rt in "${replay_tier_fuzzers[@]}"; do
		[ "${rt}" = "${name}" ] && return 0
	done
	return 1
}

# The exact fromJSON() array literal quality-fuzz-nightly.yml's step-level
# if: conditions gate on. Built from replay_tier_fuzzers, in the same order,
# so a target added to or removed from that list without a matching edit to
# the workflow's gating expression is caught below rather than silently
# leaving the workflow's actual gate out of sync with this file's inventory.
replay_tier_json="$(printf '"%s",' "${replay_tier_fuzzers[@]}")"
replay_tier_json="[${replay_tier_json%,}]"

# --- the array above has to be the whole truth ------------------------------
#
# Every other check in this file works outward from `fuzzers`, so a target
# added to the tree and listed nowhere is invisible to all of them: it runs in
# no tier, and nothing says so. Compare the array against what the source
# actually declares, in both directions.
declared_fuzzers="$(
	for source_dir in internal cmd; do
		[ -d "${source_dir}" ] || continue
		grep -rHoE '^func Fuzz[A-Za-z0-9_]*\(' --include='*_test.go' "${source_dir}" || true
	done | awk -F: '{
		file = $1
		name = $2
		sub(/^func /, "", name)
		sub(/\($/, "", name)
		sub(/\/[^\/]*$/, "", file)
		printf "%s|./%s/\n", name, file
	}' | sort
)"

listed_fuzzers="$(printf '%s\n' "${fuzzers[@]}" | sort)"

if [ "${declared_fuzzers}" != "${listed_fuzzers}" ]; then
	fail "the Fuzz* targets declared under internal/ and cmd/ do not match this file's inventory"
	echo "--- declared in the tree, missing from the inventory ---" >&2
	comm -23 <(printf '%s\n' "${declared_fuzzers}") <(printf '%s\n' "${listed_fuzzers}") >&2
	echo "--- in the inventory, not declared in the tree ---" >&2
	comm -13 <(printf '%s\n' "${declared_fuzzers}") <(printf '%s\n' "${listed_fuzzers}") >&2
fi

lefthook_fuzz_entries="$(
	awk '
		/^[[:space:]]*for entry in \\[[:space:]]*$/ { in_entries = 1 }
		in_entries { print }
		in_entries && /;[[:space:]]*do[[:space:]]*$/ { exit }
	' lefthook.yml
)"

caller_fuzz_inventory="$(
	awk '
		$0 == "      fuzzers-json: >-" {
			getline
			print
		}
	' .github/workflows/ci-verify.yml
)"

# PW-2.1. A seed corpus regression is invisible from the outside: deleting
# testdata/fuzz/<Target>/ still passes `go test`, and dropping the cache steps
# still produces a green fuzz run that quietly starts from f.Add() every night.
# Both get asserted here.
corpus_max_bytes=4096

for spec in "${fuzzers[@]}"; do
	fuzzer="${spec%%|*}"
	pkg="${spec#*|}"
	fuzzer_regex="$(escape_ere "${fuzzer}")"
	pkg_regex="$(escape_ere "${pkg}")"
	# The Go-engine tiers spell the package as a `go test` argument
	# (./internal/server/); .clusterfuzzlite/build.sh spells it as a path under
	# the module (internal/server). Derive one from the other so the two can
	# never drift independently.
	cflite_pkg="${pkg#./}"
	cflite_pkg="${cflite_pkg%/}"
	cflite_entry="^build_fuzzer[[:space:]]+$(escape_ere "${cflite_pkg}")[[:space:]]+${fuzzer_regex}\$"
	workflow_mapping="^[[:space:]]*-[[:space:]]*\\{[[:space:]]*name:[[:space:]]*${fuzzer_regex}[[:space:]]*,[[:space:]]*pkg:[[:space:]]*${pkg_regex}[[:space:]]*\\}[[:space:]]*$"
	caller_mapping="{\"name\":\"${fuzzer}\",\"pkg\":\"${pkg}\"}"
	lefthook_entry="^[[:space:]]*\"${fuzzer_regex}[[:space:]]+${pkg_regex}\"([[:space:]]+\\\\|;[[:space:]]*do)[[:space:]]*$"

	grep -Fq "${caller_mapping}" <<<"${caller_fuzz_inventory}" ||
		fail "ci-verify.yml must run ${fuzzer} in ${pkg}"
	grep -Eq "${lefthook_entry}" <<<"${lefthook_fuzz_entries}" ||
		fail "lefthook.yml must run ${fuzzer} in ${pkg}"
	# Nightly's matrix still carries every target, replay tier included — the
	# split happens per-step inside that job (see the "must gate a Replay
	# step" checks below), not by dropping a target from the matrix.
	grep -Eq "${workflow_mapping}" .github/workflows/quality-fuzz-nightly.yml ||
		fail "quality-fuzz-nightly.yml must run ${fuzzer} in ${pkg}"
	# The monthly matrix is where the tiers actually diverge: a replay-tier
	# target must NOT get a monthly leg at all (that is the whole savings
	# this tier exists for), and every other target still must.
	if is_replay_tier "${fuzzer}"; then
		grep -Eq "${workflow_mapping}" .github/workflows/quality-fuzz-monthly.yml &&
			fail "quality-fuzz-monthly.yml must not run replay-tier target ${fuzzer} in its matrix — it is seed-replay-only in the nightly and gets no monthly leg"
	else
		grep -Eq "${workflow_mapping}" .github/workflows/quality-fuzz-monthly.yml ||
			fail "quality-fuzz-monthly.yml must run ${fuzzer} in ${pkg}"
	fi
	# ClusterFuzzLite is unaffected by the Go-engine tiers' seed-replay split:
	# every target, replay tier included, still builds for libFuzzer.
	grep -Eq "${cflite_entry}" .clusterfuzzlite/build.sh ||
		fail ".clusterfuzzlite/build.sh must build ${fuzzer} from ${cflite_pkg}"

	corpus_dir="${pkg#./}"
	corpus_dir="${corpus_dir%/}/testdata/fuzz/${fuzzer}"
	corpus_count=0
	if [ -d "${corpus_dir}" ]; then
		corpus_count="$(find "${corpus_dir}" -type f | wc -l | tr -d ' ')"
	fi
	if [ "${corpus_count}" -eq 0 ]; then
		fail "${fuzzer} must ship a non-empty committed seed corpus at ${corpus_dir}/"
	else
		while IFS= read -r corpus_file; do
			[ "$(head -n 1 "${corpus_file}")" = "go test fuzz v1" ] ||
				fail "${corpus_file} is missing the 'go test fuzz v1' header, so the engine ignores it"
			corpus_size="$(wc -c <"${corpus_file}" | tr -d ' ')"
			[ "${corpus_size}" -le "${corpus_max_bytes}" ] ||
				fail "${corpus_file} is ${corpus_size} bytes; seed entries are capped at ${corpus_max_bytes}"
		done < <(find "${corpus_dir}" -type f)
	fi
done

# ClusterFuzzLite is the one tier that does not read the inventory from a
# workflow file, so a target added to the four Go-engine tiers and forgotten
# here would just never be built for libFuzzer. Count as well as match: the
# per-fuzzer loop above cannot see an ELEVENTH build_fuzzer line for a target
# nobody else runs.
cflite_entry_count="$(grep -Ec '^build_fuzzer[[:space:]]' .clusterfuzzlite/build.sh || true)"
if [ "${cflite_entry_count}" -ne "${#fuzzers[@]}" ]; then
	fail ".clusterfuzzlite/build.sh must build exactly ${#fuzzers[@]} fuzzers, found ${cflite_entry_count}"
fi

# Corpus persistence across the two scheduled lanes. Everything below is a
# mechanical line the workflows have to agree on; the reasoning for each choice
# lives in the workflow comments, not here.
cache_action_sha="55cc8345863c7cc4c66a329aec7e433d2d1c52a9"
upload_artifact_sha="043fb46d1a93c77aae656e7c1c64a875d1fc6a0a"
download_artifact_sha="3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c"
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
cache_key='          key: fuzz-corpus-v1-${{ runner.os }}-${{ matrix.fuzzer.name }}-${{ github.run_id }}-${{ github.run_attempt }}'
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
cache_restore_prefix='            fuzz-corpus-v1-${{ runner.os }}-${{ matrix.fuzzer.name }}-'
cache_save_guard="        if: always() && steps.corpus.outputs.generated != ''"

# Crash classification (PW-5.10): scripts/ci/fuzz-run.sh is the single owner
# of the retry/classify loop for all four callers — lefthook.yml,
# scripts/ci/go-fuzz.sh, quality-fuzz-nightly.yml and quality-fuzz-monthly.yml
# — instead of each one reimplementing it. Go prints a different message once
# a crasher already lives in the committed seed corpus (re-tested before
# anything new runs) than it does on first discovery, so the shared script
# has to grep both or every run after the first silently falls through to
# kind=error.
fuzz_run_script="scripts/ci/fuzz-run.sh"
crash_pattern_first_discovery="Failing input written to testdata"
crash_pattern_seed_regression="failure while testing seed corpus entry"

# Every check below runs against comment-stripped content, not the raw file:
# a caller could otherwise satisfy "must invoke fuzz-run.sh" or "must not
# reimplement the classifier" by leaving a comment behind — e.g. commenting
# out the real call while keeping a comment that mentions the script path, or
# discussing a crash phrase in prose. Both the bash `run:` blocks and the
# YAML around them use `#` for a full-line comment, so a first pass drops
# those. A second pass truncates a trailing comment too (e.g.
# `FUZZ_RETRIES=2 true # bash scripts/ci/fuzz-run.sh ...`, which would
# otherwise satisfy both the anchored FUZZ_RETRIES check and the invocation
# check while never actually running fuzz-run.sh) — safe here because none
# of the real invocation lines contain a literal '#'.
strip_comments() {
	grep -Ev '^[[:space:]]*#' "$1" | sed -E 's/[[:space:]]+#.*$//'
}

fuzz_run_script_code="$(strip_comments "${fuzz_run_script}")"

# Anchored to the classifier's own `if grep -q ...` line, not just "appears
# somewhere in the file": both phrases are echoed again in the script's own
# human-facing messages, so a bare `grep -Fq` would still pass after the
# classifier condition itself stopped matching.
# shellcheck disable=SC2016 # Asserting the literal text of the script.
crash_classifier_first_discovery='	if grep -q "Failing input written to testdata" "${attempt_log}"; then'
# shellcheck disable=SC2016 # Asserting the literal text of the script.
crash_classifier_seed_regression='	if grep -q "failure while testing seed corpus entry" "${attempt_log}"; then'

grep -Fxq "${crash_classifier_first_discovery}" <<<"${fuzz_run_script_code}" ||
	fail "${fuzz_run_script} must classify '${crash_pattern_first_discovery}' as kind=crash in the classifier condition itself"
grep -Fxq "${crash_classifier_seed_regression}" <<<"${fuzz_run_script_code}" ||
	fail "${fuzz_run_script} must classify '${crash_pattern_seed_regression}' as kind=crash in the classifier condition itself"

# None of the four callers may reimplement the classifier inline — that is
# exactly the duplication PW-5.10 removed, and a caller that grows its own
# copy of either phrase can silently drift from the shared one. Matched as
# "grep" and the phrase on the same non-comment line, in any quoting style
# (double quotes, single quotes, or none) — not the bare phrase text: the
# nightly/monthly step-summary steps legitimately echo the same phrases into
# their human-facing output, and that is reporting, not reclassification, and
# it never puts the word "grep" on that line.
#
# Each caller must instead invoke the shared script. The invocation is
# anchored to a non-comment line that starts (after leading whitespace) with
# the FUZZ_RETRIES= env-assignment prefix every caller leads its call with,
# and separately to a non-comment `bash .../fuzz-run.sh` line — not just "the
# path or FUZZ_RETRIES=2 appear somewhere in the file", which a stale comment
# can satisfy on its own once a caller's real call is commented out.
for caller in lefthook.yml scripts/ci/go-fuzz.sh .github/workflows/quality-fuzz-nightly.yml .github/workflows/quality-fuzz-monthly.yml; do
	caller_code="$(strip_comments "${caller}")"

	grep -Eq "grep.*${crash_pattern_first_discovery}" <<<"${caller_code}" &&
		fail "${caller} must not reimplement the '${crash_pattern_first_discovery}' classifier inline; it must call ${fuzz_run_script}"
	grep -Eq "grep.*${crash_pattern_seed_regression}" <<<"${caller_code}" &&
		fail "${caller} must not reimplement the '${crash_pattern_seed_regression}' classifier inline; it must call ${fuzz_run_script}"
	grep -Eq 'bash[[:space:]]+.*fuzz-run\.sh' <<<"${caller_code}" ||
		fail "${caller} must invoke ${fuzz_run_script} rather than its own retry/classify loop"

	# The monthly leg is the one exception to FUZZ_RETRIES=2: a retry re-runs
	# the FULL -fuzztime on the "context deadline exceeded" boundary flake,
	# so two retries there is two full fuzztimes in one job — 10m + 10m +
	# ~2m setup = ~22m, past that job's own timeout-minutes and back inside
	# the 14-19m runner-shutdown window the six-leg split exists to stay out
	# of. Every other caller keeps the shared default of 2.
	case "${caller}" in
	.github/workflows/quality-fuzz-monthly.yml) want_retries=1 ;;
	*) want_retries=2 ;;
	esac
	grep -Eq "^[[:space:]]*FUZZ_RETRIES=${want_retries}([[:space:]]|\\\\$|\"|$)" <<<"${caller_code}" ||
		fail "${caller} must pass FUZZ_RETRIES=${want_retries} explicitly to ${fuzz_run_script}, anchored at the call itself"
done

step_block() {
	awk -v want="      - name: $2" -v occurrence="${3:-1}" '
		$0 == want {
			seen++
			if (seen == occurrence) { inside = 1; print; next }
		}
		inside && /^      - name:/ { exit }
		inside { print }
	' "$1"
}

for workflow in .github/workflows/quality-fuzz-nightly.yml .github/workflows/quality-fuzz-monthly.yml; do
	# A wrong `go list` cross-check derivation would still look fine to every
	# other check in this file: `mkdir -p` creates whatever path was computed,
	# actions/cache saves and restores it without complaint, and the corpus
	# persists silently against a path `go` itself never reads from. This step
	# has to compare its own derived path against the toolchain's own answer and
	# fail loudly on a mismatch, or a drift like that costs another investigation
	# instead of a red step.
	#
	# The monthly workflow carries TWO "Resolve corpus paths" steps (the
	# fuzzing leg and the merge-corpus job that restores/merges/saves the
	# same cache path independently), and both derive the identical path
	# from the identical inputs, so both need the same guard or a drift in
	# just the merge job's copy would persist the merged corpus under a
	# path go itself never reads from, silently, forever. step_block's
	# occurrence param walks every "Resolve corpus paths" step in the file
	# rather than only the first.
	corpus_step_count="$(grep -c '^      - name: Resolve corpus paths$' "${workflow}")"
	if [ "${corpus_step_count}" -eq 0 ]; then
		fail "${workflow} must have a 'Resolve corpus paths' step"
	fi
	occurrence=1
	while [ "${occurrence}" -le "${corpus_step_count}" ]; do
		corpus_step_block="$(step_block "${workflow}" "Resolve corpus paths" "${occurrence}")"
		grep -Fq "go list -f '{{.ImportPath}}'" <<<"${corpus_step_block}" ||
			fail "${workflow}: 'Resolve corpus paths' step #${occurrence} must cross-check its derived import path against go list -f '{{.ImportPath}}'"
		grep -Fq 'corpus path derivation is wrong' <<<"${corpus_step_block}" ||
			fail "${workflow}: 'Resolve corpus paths' step #${occurrence} must fail loudly with a 'corpus path derivation is wrong' error on a go list mismatch"
		grep -Fq 'corpus path cross-check skipped' <<<"${corpus_step_block}" ||
			fail "${workflow}: 'Resolve corpus paths' step #${occurrence} must warn 'corpus path cross-check skipped' when go list produces no import path"
		occurrence=$((occurrence + 1))
	done

	grep -Fq "uses: actions/cache/restore@${cache_action_sha}" "${workflow}" ||
		fail "${workflow} must restore the fuzz corpus from actions/cache pinned to ${cache_action_sha}"
	grep -Fq "uses: actions/cache/save@${cache_action_sha}" "${workflow}" ||
		fail "${workflow} must save the fuzz corpus to actions/cache pinned to ${cache_action_sha}"

	# Restore and save have to name the same key. If they drift, every run
	# writes an entry the next run cannot find and the lane is back to zero.
	# The nightly's single job restores once and saves once (2 uses); the
	# monthly split the restore across two jobs — the leg restores its own
	# copy and merge-corpus restores again immediately before folding the
	# legs in and saving (PW-7.34 part C's restore-before-save fix) — plus
	# the one save, for 3.
	case "${workflow}" in
	.github/workflows/quality-fuzz-monthly.yml) want_key_uses=3 ;;
	*) want_key_uses=2 ;;
	esac
	key_uses="$(grep -Fxc "${cache_key}" "${workflow}" || true)"
	[ "${key_uses}" -eq "${want_key_uses}" ] ||
		fail "${workflow} must use one run-scoped corpus cache key on restore and save (want ${want_key_uses}, found ${key_uses})"

	# No workflow name in the prefix, on purpose: a cache entry is deleted
	# after 7 days without an access, so the monthly lane can only ever hit a
	# key the nightly keeps warm.
	grep -Fxq "${cache_restore_prefix}" "${workflow}" ||
		fail "${workflow} must share the nightly/monthly corpus restore-keys prefix"

	# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
	corpus_path='            ${{ steps.corpus.outputs.generated }}'
	path_uses="$(grep -Fxc "${corpus_path}" "${workflow}" || true)"
	[ "${path_uses}" -eq "${want_key_uses}" ] ||
		fail "${workflow} must cache ${corpus_path# *} on restore and save (want ${want_key_uses}, found ${path_uses})"

	# Regression guard: the git-tracked seed corpus must never be cached
	# alongside the generated corpus. actions/cache restore extracts over the
	# checkout without cleaning the destination, so a stale cache entry would
	# silently outrank HEAD for that directory.
	# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
	seed_in_cache="$(grep -Fxc '            ${{ steps.corpus.outputs.seed }}' "${workflow}" || true)"
	[ "${seed_in_cache}" -eq 0 ] ||
		fail "${workflow} must not cache the git-tracked seed corpus; the cache restore extracts over the checkout and would outrank HEAD"

	# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
	grep -Fq 'generated="$(go env GOCACHE)/fuzz/${module}/${rel}/${FUZZER}"' "${workflow}" ||
		fail "${workflow} must resolve the generated corpus to \$GOCACHE/fuzz/<import path>/<Target>"
	# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
	grep -Fq 'seed="${rel}/testdata/fuzz/${FUZZER}"' "${workflow}" ||
		fail "${workflow} must resolve the seed corpus to <pkg>/testdata/fuzz/<Target>"

	# The monthly's save guard has its own exact-text assertion in the
	# chunking section below (it also requires the merge step to have
	# succeeded, which the nightly's single-job shape has no equivalent
	# step for), so this generic guard is scoped to the nightly only.
	if [ "${workflow}" = ".github/workflows/quality-fuzz-nightly.yml" ]; then
		grep -Fxq "${cache_save_guard}" "${workflow}" ||
			fail "${workflow} must save the corpus on failure too — a crasher is the finding worth keeping"
	fi

	# harden-runner fails these transfers silently: actions/cache warns and the
	# job stays green while persisting nothing.
	for endpoint in '*.actions.githubusercontent.com:443' '*.blob.core.windows.net:443' 'results-receiver.actions.githubusercontent.com:443'; do
		grep -Fxq "            ${endpoint}" "${workflow}" ||
			fail "${workflow} must allow ${endpoint} or the corpus transfer is blocked without failing"
	done

	# Corpus persistence must not have cost the lane its least-privilege token.
	# Checked against the corpus-touching job specifically (named "deep-fuzz"
	# in the nightly workflow, "monthly-fuzz" in the monthly one — the first
	# job under `jobs:` either way), not a whole-file count: quality-fuzz-
	# nightly.yml's own `history` job legitimately carries a job-level
	# `permissions: contents: write` of its own (Spec 2), the same way
	# quality-mutation-monthly.yml's history job does, and that is a
	# different job holding a different, deliberate escalation.
	corpus_job_name="$(awk '
		/^jobs:$/ { in_jobs = 1; next }
		in_jobs && /^  [A-Za-z0-9_-]+:$/ {
			name = $1
			sub(/:$/, "", name)
			print name
			exit
		}
	' "${workflow}")"
	corpus_job_block="$(awk -v job="  ${corpus_job_name}:" '
		$0 == job { in_job = 1; next }
		in_job && /^  [^[:space:]]/ { in_job = 0 }
		in_job { print }
	' "${workflow}")"
	if grep -Eq '^[[:space:]]*permissions:' <<<"${corpus_job_block}"; then
		fail "${workflow}: the '${corpus_job_name}' job must not declare permissions of its own"
	fi

	# Anchored at column 0: this counts the workflow-level declaration only,
	# same as perms_block's own anchor just below.
	perms_declarations="$(grep -Ec '^permissions:$' "${workflow}" || true)"
	[ "${perms_declarations}" -eq 1 ] ||
		fail "${workflow} must declare workflow-level permissions exactly once (found ${perms_declarations})"
	perms_block="$(awk '/^permissions:$/ { inside = 1; next } inside && /^[^[:space:]]/ { exit } inside && NF { print }' "${workflow}")"
	[ "${perms_block}" = "  contents: read" ] ||
		fail "${workflow} permissions must stay exactly 'contents: read'"

	# Nightly and monthly restore from and save to the same per-fuzzer cache
	# prefix, so a manual workflow_dispatch of one can race the other's
	# scheduled run for the same fuzzer's cache entry outside the cron
	# spacing that keeps the two routine schedules apart. Both workflows'
	# corpus-touching jobs have to share one job-level concurrency group,
	# keyed on the fuzzer name the cache key itself uses (runner.os isn't a
	# valid context in a concurrency expression), with cancel-in-progress:
	# false so the second writer queues instead of either run being cancelled
	# mid-save.
	# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
	corpus_concurrency_group='      group: quality-fuzz-corpus-${{ matrix.fuzzer.name }}'
	grep -Fxq "${corpus_concurrency_group}" "${workflow}" ||
		fail "${workflow} corpus-touching job must share the concurrency group quality-fuzz-corpus-\${{ matrix.fuzzer.name }} with the other fuzz-corpus workflow"
	grep -Fxq "      cancel-in-progress: false" "${workflow}" ||
		fail "${workflow} corpus-touching job's concurrency group must set cancel-in-progress: false so a racing writer queues instead of being cancelled"

	# The if: guard has to sit ON the upload-artifact step itself, not just
	# appear somewhere in the file — a guard attached to the wrong step would
	# match a bare `grep -Fxq` just as well and silently stop protecting the
	# corpus artifact.
	upload_step_block="$(awk '
		/^      - name: Upload fuzz corpus on failure or cancel$/ { inside = 1; print; next }
		inside && /^      - name:/ { exit }
		inside { print }
	' "${workflow}")"
	[ -n "${upload_step_block}" ] ||
		fail "${workflow} must have an 'Upload fuzz corpus on failure or cancel' step"
	if [ "${workflow}" = ".github/workflows/quality-fuzz-nightly.yml" ]; then
		# The nightly workflow also gates on kind/reason, not just
		# failure()/cancelled(): a repro is only ever expected for a real
		# crash or a stale-cache replay (scripts/ci/fuzz-replay.sh's own
		# reason=stale-cache) — a build or corpus-copy error never produces
		# new corpus content, so uploading for those would just be a
		# guaranteed empty artifact. steps.replay only exists on this
		# workflow's replay tier, so the monthly workflow below (no id:
		# replay step) keeps the plain failure()/cancelled() guard instead.
		grep -Fxq "        if: |" <<<"${upload_step_block}" ||
			fail "${workflow} must guard the 'Upload fuzz corpus on failure or cancel' step with a multi-line if: expression"
		grep -Fxq "          (failure() || cancelled()) &&" <<<"${upload_step_block}" ||
			fail "${workflow} must guard the 'Upload fuzz corpus on failure or cancel' step with failure() || cancelled()"
		grep -Fxq "          ((steps.fuzz.outputs.kind || steps.replay.outputs.kind) == 'crash' ||" <<<"${upload_step_block}" ||
			fail "${workflow} must only upload the corpus artifact when kind is crash (steps.fuzz or steps.replay)"
		grep -Fxq "           (steps.fuzz.outputs.reason || steps.replay.outputs.reason) == 'stale-cache')" <<<"${upload_step_block}" ||
			fail "${workflow} must also upload the corpus artifact when reason is stale-cache (steps.fuzz or steps.replay)"
		grep -Fxq "          if-no-files-found: warn" <<<"${upload_step_block}" ||
			fail "${workflow} must set if-no-files-found: warn on the 'Upload fuzz corpus on failure or cancel' step now that it only runs when a repro was expected"
	else
		grep -Fxq "        if: failure() || cancelled()" <<<"${upload_step_block}" ||
			fail "${workflow} must guard the 'Upload fuzz corpus on failure or cancel' step with if: failure() || cancelled()"
	fi
	grep -Fq "uses: actions/upload-artifact@${upload_artifact_sha}" <<<"${upload_step_block}" ||
		fail "${workflow} must upload the corpus artifact with actions/upload-artifact pinned to ${upload_artifact_sha} in the same step"
done

# --- Monthly fuzz chunking (PW-7.34 part B) ----------------------------------
#
# Since ~2026-09-01, hosted ubuntu-24.04 runners kill any job that saturates
# CPU for more than ~14-19 minutes with "The runner has received a shutdown
# signal" (exit 143) — run 33514192612 (2026-09-01) died at 19m into what was
# a single 60m-per-fuzzer job. quality-fuzz-monthly.yml now splits each
# fuzzer's hour into 6 parallel 10-minute legs instead of one long job, with a
# separate per-fuzzer job that merges the six legs' corpora and is the only
# thing that saves the cache. Every property below is one a leg-count-and-
# artifact-plumbing regression would otherwise silently reintroduce the
# runner-shutdown failure mode this design exists to avoid.

# A named job's own block, from its two-space key to the next one. Local to
# this section: quality-history-config-test.sh has its own copy scoped to a
# different set of workflows.
job_block_by_name() {
	awk -v job="  $2:" '
        $0 == job { in_job = 1; next }
        in_job && /^  [^[:space:]]/ { in_job = 0 }
        in_job { print }
    ' "$1"
}

monthly_workflow=".github/workflows/quality-fuzz-monthly.yml"
monthly_leg_job="$(job_block_by_name "${monthly_workflow}" "monthly-fuzz")"
monthly_merge_job="$(job_block_by_name "${monthly_workflow}" "merge-corpus")"

[ -n "${monthly_leg_job}" ] ||
	fail "${monthly_workflow} must have a top-level 'monthly-fuzz' job"
[ -n "${monthly_merge_job}" ] ||
	fail "${monthly_workflow} must have a top-level 'merge-corpus' job that merges the legs' corpora"

# Count as well as match: the per-fuzzer loop above already proves every
# non-replay-tier target is present and every replay-tier target is absent
# from each job's matrix individually, but it cannot see an EXTRA entry
# nobody's inventory accounts for. Both jobs must carry exactly the
# non-replay-tier count — not the full ten, and not ten minus one.
want_monthly_entries=$((${#fuzzers[@]} - ${#replay_tier_fuzzers[@]}))
for job_label in "monthly-fuzz:${monthly_leg_job}" "merge-corpus:${monthly_merge_job}"; do
	job_name="${job_label%%:*}"
	job_body="${job_label#*:}"
	entry_count="$(grep -Ec '^          - \{ name: Fuzz' <<<"${job_body}" || true)"
	[ "${entry_count}" -eq "${want_monthly_entries}" ] ||
		fail "${monthly_workflow} '${job_name}' job must matrix exactly ${want_monthly_entries} fuzzers (the ${#fuzzers[@]}-target inventory minus the ${#replay_tier_fuzzers[@]}-target replay tier), found ${entry_count}"
done

# The per-leg fuzztime default must already be inside the 12-minute cap, not
# just accepted by the budget step's runtime check — a default above the cap
# would fail every scheduled run, not just a misconfigured manual dispatch.
grep -Fq 'default: "10m"' "${monthly_workflow}" ||
	fail "${monthly_workflow} must default the per-leg fuzztime input to 10m"

# The budget step's own hard cap: a fuzztime above 12 minutes (720s) must be
# refused, loudly, rather than silently clamped or accepted. Anchored to the
# comparison and the exit itself, not just "720 appears somewhere in the
# file", so a cap that stops being enforced at runtime is caught even if the
# comment describing it survives.
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
grep -Fq 'if [ "${budget_s}" -gt 720 ]; then' "${monthly_workflow}" ||
	fail "${monthly_workflow} budget step must refuse a per-leg fuzztime over 720s (12m)"
budget_cap_block="$(awk '
    /if \[ "\$\{budget_s\}" -gt 720 \]; then/ { inside = 1 }
    inside { print }
    inside && /^          fi$/ { exit }
' "${monthly_workflow}")"
grep -Fq 'exit 1' <<<"${budget_cap_block}" ||
	fail "${monthly_workflow} budget step must exit non-zero when the per-leg fuzztime exceeds the 12m cap, not just warn"

# The budget step's else branch — a fuzztime that does not match any of the
# ^[0-9]+[hms]$ arms above (1h30m, 0.5h, 12m0s, 10m30s, 0.2h, ...) — must
# refuse too, not silently default to budget_s=600 and pass the fuzztime
# through to `go test -fuzz` unchecked. go test -fuzz ignores -timeout
# while fuzzing, so an unrecognized value sailing past the 12-minute cap
# check was the bypass: any Go-valid duration the three arms above don't
# match hit the old else, got budget_s=600 (which is <= 720 and so passes
# the cap check below), and was still written to the fuzz step verbatim.
grep -Fq 'budget_s=600' "${monthly_workflow}" &&
	fail "${monthly_workflow} budget step must not silently default an unparsed fuzztime to budget_s=600 and pass it through; it must refuse with exit 1"
budget_else_block="$(awk '
    /^          else$/ { inside = 1 }
    inside { print }
    inside && /^          fi$/ { exit }
' "${monthly_workflow}")"
[ -n "${budget_else_block}" ] ||
	fail "${monthly_workflow} budget step must have an else branch for an unparsed fuzztime"
grep -Fq 'exit 1' <<<"${budget_else_block}" ||
	fail "${monthly_workflow} budget step's else branch (an unparsed fuzztime) must exit 1, not silently fall back to a default"

# The chunk dimension: exactly 6 legs per fuzzer, matching the 60 CPU-minute
# budget split into 6×10-minute pieces.
grep -Fxq "        chunk: [1, 2, 3, 4, 5, 6]" <<<"${monthly_leg_job}" ||
	fail "${monthly_workflow} 'monthly-fuzz' job must matrix chunk: [1, 2, 3, 4, 5, 6]"

# 15-minute job ceiling: one 12m fuzztime attempt (FUZZ_RETRIES=1, checked
# above) plus ~2m of setup/checkout/cache overhead is ~14m, so 15m is a
# backstop above the legitimate maximum, not a number this job is expected
# to run into — and still comfortably under the ~14-19m runner-shutdown
# window.
grep -Fxq "    timeout-minutes: 15" <<<"${monthly_leg_job}" ||
	fail "${monthly_workflow} 'monthly-fuzz' leg job must set timeout-minutes: 15"

# Every leg uploads its corpus chunk for merge-corpus to fold back together —
# pinned to the same actions/upload-artifact SHA the crash-artifact upload
# above already asserts, with 1-day retention (this artifact is read once by
# merge-corpus in the same run and never again, unlike the 180-day crash
# artifact).
leg_chunk_upload="$(awk '
    $0 == "      - name: Upload fuzz corpus chunk" { inside = 1; print; next }
    inside && /^      - name:/ { exit }
    inside { print }
' <<<"${monthly_leg_job}")"
[ -n "${leg_chunk_upload}" ] ||
	fail "${monthly_workflow} 'monthly-fuzz' job must have an 'Upload fuzz corpus chunk' step"
grep -Fq "uses: actions/upload-artifact@${upload_artifact_sha}" <<<"${leg_chunk_upload}" ||
	fail "${monthly_workflow} 'Upload fuzz corpus chunk' step must use actions/upload-artifact pinned to ${upload_artifact_sha}"
grep -Fxq "          retention-days: 1" <<<"${leg_chunk_upload}" ||
	fail "${monthly_workflow} 'Upload fuzz corpus chunk' step must set retention-days: 1"
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
grep -Fq 'name: fuzz-corpus-chunk-${{ matrix.fuzzer.name }}-${{ matrix.chunk }}-${{ github.run_id }}' <<<"${leg_chunk_upload}" ||
	fail "${monthly_workflow} chunk artifact name must be fuzz-corpus-chunk-<fuzzer>-<chunk>-<run_id>"

# merge-corpus must download exactly those chunks and be the one job that
# saves the cache — the leg job must NOT also save it, or six legs of the
# same fuzzer would race the same cache key the corpus-writer concurrency
# group exists to prevent.
grep -Fq "uses: actions/download-artifact@${download_artifact_sha}" <<<"${monthly_merge_job}" ||
	fail "${monthly_workflow} 'merge-corpus' job must download the chunk artifacts with actions/download-artifact pinned to ${download_artifact_sha}"
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
grep -Fq 'pattern: fuzz-corpus-chunk-${{ matrix.fuzzer.name }}-*-${{ github.run_id }}' <<<"${monthly_merge_job}" ||
	fail "${monthly_workflow} 'merge-corpus' job must download fuzz-corpus-chunk-<fuzzer>-*-<run_id>"
grep -Fq "uses: actions/cache/save@${cache_action_sha}" <<<"${monthly_merge_job}" ||
	fail "${monthly_workflow} 'merge-corpus' job must be the one that saves the corpus cache"
grep -Fq "uses: actions/cache/save@${cache_action_sha}" <<<"${monthly_leg_job}" &&
	fail "${monthly_workflow} 'monthly-fuzz' leg job must not itself save the corpus cache; only 'merge-corpus' may, or six legs of one fuzzer would race the same cache key"

# Neither the leg job nor the merge job may declare permissions of its own —
# the same "the corpus-touching job must not carry a credential" property
# quality-history-config-test.sh guards for the recording jobs, extended here
# to both halves of the now-split monthly lane. Nothing in this workflow
# should ever need more than the workflow-level contents: read.
for job_name_check in "monthly-fuzz" "merge-corpus"; do
	job_block_check="$(job_block_by_name "${monthly_workflow}" "${job_name_check}")"
	if grep -Eq '^[[:space:]]*permissions:' <<<"${job_block_check}"; then
		fail "${monthly_workflow}: the '${job_name_check}' job must not declare permissions of its own"
	fi
done

# merge-corpus must run on !cancelled(), not always(): always() would also
# run it — and its Save fuzz corpus step — on a CANCELLED run, writing a
# near-empty corpus under a fresh key that restore-keys would then hand to
# every future run as the newest entry for this fuzzer.
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
grep -Fxq '    if: ${{ !cancelled() }}' <<<"${monthly_merge_job}" ||
	fail "${monthly_workflow} 'merge-corpus' job must set if: \${{ !cancelled() }} exactly, not always()"

# merge-corpus must restore the cache before it merges and saves — the
# pre-chunking shape did restore -> fuzz -> save in one job, which made a
# lost update against the nightly's own writer impossible; splitting the
# fuzzing out to the six legs above moved the read to them (no concurrency
# group) and left only the write here, so restoring again immediately
# before the merge closes that gap.
grep -Fq "uses: actions/cache/restore@${cache_action_sha}" <<<"${monthly_merge_job}" ||
	fail "${monthly_workflow} 'merge-corpus' job must restore the fuzz corpus (actions/cache/restore pinned to ${cache_action_sha}) before merging and saving"

# The save guard's exact text: !cancelled(), a non-empty generated path, and
# the merge step's own success — a cache-restore failure, a jq failure, or
# any other error inside Merge chunk corpus must not save whatever partial
# `generated` dir it left behind.
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
monthly_save_guard="        if: \${{ !cancelled() && steps.corpus.outputs.generated != '' && steps.merge.outcome == 'success' }}"
grep -Fxq "${monthly_save_guard}" <<<"${monthly_merge_job}" ||
	fail "${monthly_workflow} 'merge-corpus' job's Save fuzz corpus step must guard on !cancelled() && steps.corpus.outputs.generated != '' && steps.merge.outcome == 'success'"

# The merge step must escalate kind to infra when fewer than all six legs
# reported — otherwise a leg torn down by the runner (never running its own
# always() upload) is indistinguishable from a leg that ran clean, and a red
# run summarizes as kind=pass, which is what the notification lane keys on.
monthly_merge_step="$(awk '
    $0 == "      - name: Merge chunk corpus and aggregate leg stats" { inside = 1; print; next }
    inside && /^      - name:/ { exit }
    inside { print }
' <<<"${monthly_merge_job}")"
[ -n "${monthly_merge_step}" ] ||
	fail "${monthly_workflow} must have a 'Merge chunk corpus and aggregate leg stats' step"
grep -Fq 'expected_legs' <<<"${monthly_merge_step}" ||
	fail "${monthly_workflow} 'Merge chunk corpus and aggregate leg stats' step must compute expected_legs"
grep -Fq 'kind="infra"' <<<"${monthly_merge_step}" ||
	fail "${monthly_workflow} 'Merge chunk corpus and aggregate leg stats' step must escalate kind=\"infra\" when fewer than expected_legs legs reported"

# --- Seed-replay tier gating (PW-7.37) ---------------------------------------
#
# The nightly matrix carries all ten targets (the per-fuzzer loop above
# already proved that), but only the non-replay-tier eight spend -fuzz
# minutes: the two replay_tier_fuzzers targets run through a "Replay" step
# instead of the "Fuzz" step, gated by the exact fromJSON() array this file
# derives from replay_tier_fuzzers itself — so a tier list that drifts
# between this file's inventory and the workflow's own gate is caught here,
# rather than by two green runs quietly measuring different sets of targets.
nightly_workflow_for_replay=".github/workflows/quality-fuzz-nightly.yml"
nightly_gate_negated="        if: \${{ !contains(fromJSON('${replay_tier_json}'), matrix.fuzzer.name) }}"
nightly_gate_positive="        if: \${{ contains(fromJSON('${replay_tier_json}'), matrix.fuzzer.name) }}"

resolve_budget_block="$(step_block "${nightly_workflow_for_replay}" "Resolve fuzz budget")"
[ -n "${resolve_budget_block}" ] ||
	fail "${nightly_workflow_for_replay} must have a 'Resolve fuzz budget' step"
grep -Fxq "${nightly_gate_negated}" <<<"${resolve_budget_block}" ||
	fail "${nightly_workflow_for_replay} 'Resolve fuzz budget' step must skip the replay tier (${replay_tier_json}) — it has no -fuzz budget to resolve"

# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
fuzz_step_block="$(step_block "${nightly_workflow_for_replay}" 'Fuzz ${{ matrix.fuzzer.name }}')"
[ -n "${fuzz_step_block}" ] ||
	fail "${nightly_workflow_for_replay} must have a 'Fuzz \${{ matrix.fuzzer.name }}' step"
grep -Fxq "${nightly_gate_negated}" <<<"${fuzz_step_block}" ||
	fail "${nightly_workflow_for_replay} 'Fuzz \${{ matrix.fuzzer.name }}' step must skip the replay tier (${replay_tier_json}) — those targets run through 'Replay' instead"

# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
replay_step_block="$(step_block "${nightly_workflow_for_replay}" 'Replay ${{ matrix.fuzzer.name }}')"
[ -n "${replay_step_block}" ] ||
	fail "${nightly_workflow_for_replay} must have a 'Replay \${{ matrix.fuzzer.name }}' step for the seed-replay tier"
grep -Fxq "${nightly_gate_positive}" <<<"${replay_step_block}" ||
	fail "${nightly_workflow_for_replay} 'Replay \${{ matrix.fuzzer.name }}' step must run ONLY for the replay tier (${replay_tier_json})"
grep -Eq 'bash[[:space:]]+scripts/ci/fuzz-replay\.sh' <<<"${replay_step_block}" ||
	fail "${nightly_workflow_for_replay} 'Replay \${{ matrix.fuzzer.name }}' step must invoke scripts/ci/fuzz-replay.sh"

# The Score/Summarize steps must read whichever of the two mutually-exclusive
# steps actually ran, or a replay-tier target's kind/reason is silently empty
# every night once the Fuzz step stops running for it.
grep -Fq 'steps.fuzz.outputs.kind || steps.replay.outputs.kind' "${nightly_workflow_for_replay}" ||
	fail "${nightly_workflow_for_replay} must read steps.fuzz.outputs.kind || steps.replay.outputs.kind, not steps.fuzz alone"

fail_count_before_replay_script_check="${failures}"
if [ ! -f scripts/ci/fuzz-replay.sh ]; then
	fail "scripts/ci/fuzz-replay.sh must exist; it is what the 'Replay' step above invokes"
fi
if [ "${failures}" -eq "${fail_count_before_replay_script_check}" ]; then
	replay_script_code="$(strip_comments scripts/ci/fuzz-replay.sh)"
	# The replay script must classify a genuine regression as kind=crash,
	# reason=seed-regression — the same reason value the -fuzz path's own
	# seed-corpus-regression case uses (scripts/ci/fuzz-run.sh above) — so
	# the Summarize step's existing "a previously-fixed crash has regressed"
	# branch covers both without a third reason value to keep in sync.
	grep -Fq 'kind=crash' <<<"${replay_script_code}" ||
		fail "scripts/ci/fuzz-replay.sh must emit kind=crash on a seed-replay regression"
	grep -Fq 'reason=seed-regression' <<<"${replay_script_code}" ||
		fail "scripts/ci/fuzz-replay.sh must emit reason=seed-regression on a seed-replay regression"
	grep -Fq 'kind=replay' <<<"${replay_script_code}" ||
		fail "scripts/ci/fuzz-replay.sh must emit kind=replay on a clean pass, not kind=pass — that value stays reserved for a run that spent -fuzz minutes"

	# Three more exit paths, each kind=error rather than kind=crash, since
	# none of them is a finding the committed corpus is responsible for: a
	# package that no longer builds, a cached GOCACHE entry whose shape no
	# longer matches the target's current signature, and a failure copying
	# that cache into testdata in the first place. Collapsing any of these
	# into reason=seed-regression would send someone hunting a corpus
	# regression that isn't there.
	grep -Fq 'reason=build' <<<"${replay_script_code}" ||
		fail "scripts/ci/fuzz-replay.sh must emit reason=build when the package fails to compile, distinct from a corpus regression"
	grep -Fq 'reason=stale-cache' <<<"${replay_script_code}" ||
		fail "scripts/ci/fuzz-replay.sh must emit reason=stale-cache when a cached corpus entry no longer matches the target's signature"
	grep -Fq 'reason=corpus-copy' <<<"${replay_script_code}" ||
		fail "scripts/ci/fuzz-replay.sh must emit reason=corpus-copy when copying the generated corpus into testdata fails, rather than swallowing the error into a green kind=replay"
fi

# --- Spec 2: corpus coverage score step (nightly only) ----------------------
#
# The score-then-cleanup pair, and the quality-history record they feed,
# belong to the nightly lane only: it is the one that runs on a schedule and
# has a `history` job. The monthly workflow is untouched by this spec.
nightly_workflow=".github/workflows/quality-fuzz-nightly.yml"

step_line() {
	grep -n "^      - name: $2\$" "$1" | head -n 1 | cut -d: -f1 || true
}

save_corpus_line="$(step_line "${nightly_workflow}" "Save fuzz corpus")"
upload_failure_line="$(step_line "${nightly_workflow}" "Upload fuzz corpus on failure or cancel")"
score_step_line="$(step_line "${nightly_workflow}" "Score corpus coverage")"
cleanup_step_line="$(step_line "${nightly_workflow}" "Verify cached corpus copies were cleaned up")"

[ -n "${save_corpus_line}" ] ||
	fail "${nightly_workflow} must have a 'Save fuzz corpus' step"
[ -n "${upload_failure_line}" ] ||
	fail "${nightly_workflow} must have an 'Upload fuzz corpus on failure or cancel' step"
[ -n "${score_step_line}" ] ||
	fail "${nightly_workflow} must have a 'Score corpus coverage' step"
[ -n "${cleanup_step_line}" ] ||
	fail "${nightly_workflow} must have a 'Verify cached corpus copies were cleaned up' step"

# The crash-artifact upload has to run BEFORE the score step, not just
# somewhere in the file: the score step is the one that copies cached-*
# entries into the seed dir, and that copy step's own `testdata/fuzz/` tree is
# exactly what the failure-upload step's path glob captures. Scoring first
# would let a cached-* copy ride along into the crash artifact a human later
# downloads and commits as a minimized repro.
if [ -n "${upload_failure_line}" ] && [ -n "${score_step_line}" ]; then
	[ "${upload_failure_line}" -lt "${score_step_line}" ] ||
		fail "${nightly_workflow}: 'Upload fuzz corpus on failure or cancel' (line ${upload_failure_line}) must come before 'Score corpus coverage' (line ${score_step_line}) — a cached-* copy must never be able to enter the crash artifact"
fi

# Every one of the 10 matrix fuzzers is scored by construction: the score
# step, like every other step in this job, is one step shared across the
# whole matrix rather than duplicated per fuzzer — the per-fuzzer inventory
# loop above already proves all 10 run through this job, so what remains to
# prove is that the shared step itself exists, runs unconditionally, and
# sits after the cache save it must never race.
if [ -n "${save_corpus_line}" ] && [ -n "${score_step_line}" ]; then
	[ "${score_step_line}" -gt "${save_corpus_line}" ] ||
		fail "${nightly_workflow}: 'Score corpus coverage' (line ${score_step_line}) must come after 'Save fuzz corpus' (line ${save_corpus_line}) — a cached-* copy must never be able to enter the actions/cache entry that step just wrote"
fi

score_step_block="$(step_block "${nightly_workflow}" "Score corpus coverage")"
if [ -n "${score_step_block}" ]; then
	grep -Fxq "        if: always()" <<<"${score_step_block}" ||
		fail "${nightly_workflow}: the 'Score corpus coverage' step must run if: always()"
	grep -Eq 'bash[[:space:]]+scripts/ci/fuzz-score\.sh' <<<"${score_step_block}" ||
		fail "${nightly_workflow}: the 'Score corpus coverage' step must invoke scripts/ci/fuzz-score.sh"
fi

cleanup_step_block="$(step_block "${nightly_workflow}" "Verify cached corpus copies were cleaned up")"
if [ -n "${cleanup_step_block}" ]; then
	grep -Fxq "        if: always()" <<<"${cleanup_step_block}" ||
		fail "${nightly_workflow}: the 'Verify cached corpus copies were cleaned up' step must run if: always()"
	# fuzz-score.sh already deletes exactly the cached-<basename> paths it
	# copied, from its own manifest, on every exit path. A blanket
	# `rm -f "${SEED}/cached-"*` here would also delete a cached-* file
	# fuzz-score.sh deliberately left alone (a pre-existing untracked one,
	# or every copy when a tracked cached-* file made it refuse to copy at
	# all) — this step must only verify, never delete.
	# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
	grep -Fq 'rm -f "${SEED}/cached-"*' <<<"${cleanup_step_block}" &&
		fail "${nightly_workflow}: the 'Verify cached corpus copies were cleaned up' step must not rm -f \"\${SEED}/cached-\"*; it must only verify fuzz-score.sh's own cleanup"
	grep -Fiq 'manifest' <<<"${cleanup_step_block}" ||
		fail "${nightly_workflow}: the 'Verify cached corpus copies were cleaned up' step must reference fuzz-score.sh's manifest of copied paths"
fi

if [ -n "${score_step_line}" ] && [ -n "${cleanup_step_line}" ]; then
	[ "${cleanup_step_line}" -gt "${score_step_line}" ] ||
		fail "${nightly_workflow}: 'Verify cached corpus copies were cleaned up' must come after 'Score corpus coverage'"
fi

# Monthly/nightly cron overlap. Both lanes restore from and save to the same
# corpus cache prefix, and restore-keys picks whichever entry is newest, so if
# the monthly 360m job is still running when the nightly starts, the lane that
# saves second silently discards the other's coverage. The monthly cron hour
# has to leave at least 6 hours (360m + margin) before the nightly cron hour.
cron_hour() {
	awk -F"'" '/^[[:space:]]*- cron: /{print $2; exit}' "$1" | awk '{print $2}'
}
nightly_cron_hour="$(cron_hour .github/workflows/quality-fuzz-nightly.yml)"
monthly_cron_hour="$(cron_hour .github/workflows/quality-fuzz-monthly.yml)"
[ -n "${nightly_cron_hour}" ] ||
	fail "quality-fuzz-nightly.yml must declare a schedule.cron"
[ -n "${monthly_cron_hour}" ] ||
	fail "quality-fuzz-monthly.yml must declare a schedule.cron"
if [ -n "${nightly_cron_hour}" ] && [ -n "${monthly_cron_hour}" ]; then
	[ "$((10#${monthly_cron_hour} + 6))" -le "$((10#${nightly_cron_hour}))" ] ||
		fail "quality-fuzz-monthly.yml cron hour (${monthly_cron_hour}) + 6 must be <= quality-fuzz-nightly.yml cron hour (${nightly_cron_hour}), or the 360m monthly job can still be running when the nightly starts and restore-keys races the corpus cache"
fi

if [ "$failures" -ne 0 ]; then
	echo "${failures} fuzz tier contract check(s) failed" >&2
	exit 1
fi

echo "Fuzz tier contract checks passed."
