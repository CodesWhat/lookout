#!/usr/bin/env bash
set -euo pipefail

fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT

# Rebuilt from scratch before every mutation, so one negative case can never
# leave a broken file behind and make the next one pass for the wrong reason.
seed_fixture() {
	rm -rf "${fixture:?}"/*
	mkdir -p "${fixture}/scripts/ci" "${fixture}/.github/workflows" "${fixture}/.clusterfuzzlite"
	cp scripts/fuzz-tier-config-test.sh "${fixture}/scripts/"
	cp scripts/ci/go-fuzz.sh scripts/ci/fuzz-run.sh scripts/ci/fuzz-replay.sh "${fixture}/scripts/ci/"
	cp lefthook.yml "${fixture}/"
	cp .github/workflows/ci-verify.yml "${fixture}/.github/workflows/"
	cp .github/workflows/quality-fuzz-nightly.yml "${fixture}/.github/workflows/"
	cp .github/workflows/quality-fuzz-monthly.yml "${fixture}/.github/workflows/"
	cp .clusterfuzzlite/build.sh "${fixture}/.clusterfuzzlite/"
	while IFS= read -r corpus_dir; do
		mkdir -p "${fixture}/${corpus_dir}"
		cp "${corpus_dir}"/* "${fixture}/${corpus_dir}/"
	done < <(find internal -type d -path '*/testdata/fuzz/*')
	# The files that declare the targets, so the inventory-vs-tree check has a
	# tree to read. Only the declaring files; nothing else compiles here.
	while IFS= read -r fuzz_file; do
		mkdir -p "${fixture}/$(dirname "${fuzz_file}")"
		cp "${fuzz_file}" "${fixture}/${fuzz_file}"
	done < <(grep -rlE '^func Fuzz[A-Za-z0-9_]*\(' --include='*_test.go' internal cmd || true)
}

expect_pass() {
	if ! (cd "${fixture}" && bash scripts/fuzz-tier-config-test.sh >/dev/null); then
		echo "FAIL: $1" >&2
		exit 1
	fi
}

expect_fail() {
	if (cd "${fixture}" && bash scripts/fuzz-tier-config-test.sh >/dev/null 2>&1); then
		echo "FAIL: $1" >&2
		exit 1
	fi
}

seed_fixture
expect_pass "valid fuzz tier fixture must pass"

seed_fixture
awk '
	/FuzzParseKeyLine \.\/internal\/auth\/"; do/ {
		print "        # \"FuzzParseKeyLine ./internal/auth/\""
		print "          \"FuzzUnrelated ./internal/auth/\"; do"
		next
	}
	{ print }
' "${fixture}/lefthook.yml" >"${fixture}/lefthook.yml.tmp"
mv "${fixture}/lefthook.yml.tmp" "${fixture}/lefthook.yml"
expect_fail "a comment outside the Lefthook entry list must not satisfy the contract"

seed_fixture
sed 's|"pkg":"./internal/auth/"|"pkg":"X/internal/auth/"|' \
	.github/workflows/ci-verify.yml >"${fixture}/.github/workflows/ci-verify.yml"
expect_fail "a malformed workflow mapping must not satisfy the contract"

# --- Seed corpus (PW-2.1) ---------------------------------------------------

seed_fixture
rm -rf "${fixture}/internal/protocol/testdata/fuzz/FuzzEnvelope"
expect_fail "a fuzzer with no committed seed corpus must not satisfy the contract"

seed_fixture
find "${fixture}/internal/protocol/testdata/fuzz/FuzzEnvelope" -type f -delete
expect_fail "an empty seed corpus directory must not satisfy the contract"

seed_fixture
printf 'string("nope")\n' >"${fixture}/internal/protocol/testdata/fuzz/FuzzEnvelope/headerless"
expect_fail "a corpus file without the 'go test fuzz v1' header must not satisfy the contract"

seed_fixture
{
	printf 'go test fuzz v1\nstring("'
	head -c 8192 /dev/zero | tr '\0' 'a'
	printf '")\n'
} >"${fixture}/internal/protocol/testdata/fuzz/FuzzEnvelope/oversized"
expect_fail "an oversized corpus file must not satisfy the contract"

# --- Corpus persistence (PW-2.1) --------------------------------------------

nightly="${fixture}/.github/workflows/quality-fuzz-nightly.yml"
monthly="${fixture}/.github/workflows/quality-fuzz-monthly.yml"

seed_fixture
sed 's|uses: actions/cache/restore@|uses: actions/cache/restore@v6 # |' "${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "an unpinned corpus cache restore must not satisfy the contract"

seed_fixture
# Only the second key line — the save step's — so the two genuinely disagree.
awk '
	/^          key: fuzz-corpus-v1-/ {
		seen++
		if (seen == 2) { print $0 "-save"; next }
	}
	{ print }
' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "restore and save disagreeing on the cache key must not satisfy the contract"

seed_fixture
sed 's|^            fuzz-corpus-v1-\${{ runner.os }}-\${{ matrix.fuzzer.name }}-$|            fuzz-corpus-v1-monthly-\${{ matrix.fuzzer.name }}-|' \
	"${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a monthly-private restore-keys prefix must not satisfy the contract"

seed_fixture
sed '/^            \*\.blob\.core\.windows\.net:443$/d' "${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "dropping a cache egress endpoint must not satisfy the contract"

seed_fixture
sed "s|^        if: always() && steps.corpus.outputs.generated != ''$|        if: success()|" \
	"${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "a corpus save that skips failed runs must not satisfy the contract"

seed_fixture
awk '
	$0 == "    timeout-minutes: 75" {
		print
		print "    permissions:"
		print "      actions: read"
		next
	}
	{ print }
' "${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "a job-level permissions grant must not satisfy the contract"

seed_fixture
sed '/^        if: failure() || cancelled()$/d' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "dropping the on-failure corpus artifact upload must not satisfy the contract"

seed_fixture
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
awk '
	$0 == "            ${{ steps.corpus.outputs.generated }}" {
		print
		print "            ${{ steps.corpus.outputs.seed }}"
		next
	}
	{ print }
' "${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "caching the git-tracked seed corpus must not satisfy the contract"

# --- Monthly fuzz chunking (PW-7.34 part B) ---------------------------------

seed_fixture
sed 's|        chunk: \[1, 2, 3, 4, 5, 6\]|        chunk: [1, 2, 3, 4, 5]|' \
	"${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a monthly chunk matrix with fewer than 6 legs must not satisfy the contract"

seed_fixture
sed 's|default: "10m"|default: "15m"|' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a per-leg fuzztime default other than 10m must not satisfy the contract"

seed_fixture
# Widen the cap itself from 12m (720s) to 2h (7200s) — the default fuzztime
# is still 10m, so only the enforcement of the cap is broken, not the default.
sed 's|-gt 720|-gt 7200|' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a per-leg fuzztime cap above 12 minutes must not satisfy the contract"

seed_fixture
# Drop the cap block's own `exit 1`, leaving the ::error annotation (and
# every other line) untouched — a bare "does the cap comparison appear
# somewhere" check would still pass this, only "does it actually exit
# non-zero" notices.
awk '
	/if \[ "\$\{budget_s\}" -gt 720 \]; then/ { inside = 1 }
	inside && /^            exit 1$/ { next }
	inside && /^          fi$/ { inside = 0 }
	{ print }
' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a per-leg fuzztime cap that does not exit non-zero must not satisfy the contract"

seed_fixture
# Drop the leg job's chunk-artifact upload entirely — merge-corpus would then
# have nothing to download for that leg. The step is the last one in the
# job, so deletion has to stop at the next job boundary (2-space indent, not
# just the next step), or it eats the rest of the file.
awk '
	$0 == "      - name: Upload fuzz corpus chunk" { skip = 1; next }
	skip && (/^      - name:/ || /^  [^[:space:]]/) { skip = 0 }
	skip { next }
	{ print }
' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a monthly leg that does not upload a corpus chunk artifact must not satisfy the contract"

seed_fixture
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
sed 's|name: fuzz-corpus-chunk-${{ matrix.fuzzer.name }}-${{ matrix.chunk }}-${{ github.run_id }}|name: fuzz-corpus-chunk-${{ matrix.fuzzer.name }}-${{ github.run_id }}|' \
	"${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a chunk artifact name missing the chunk index must not satisfy the contract"

seed_fixture
# A leg job that ALSO saves the corpus cache — the exact race the split into
# monthly-fuzz (restore only) and merge-corpus (the one saver) exists to
# prevent: six legs of the same fuzzer writing the same cache key.
awk '
	$0 == "      - name: Upload fuzz corpus chunk" {
		print
		print "        uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9  # v6.1.0"
		next
	}
	{ print }
' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a leg job that also saves the corpus cache must not satisfy the contract"

seed_fixture
# merge-corpus growing a permissions block of its own — the write-scope
# discipline quality-history-config-test.sh guards for the recording jobs,
# extended here to the job that now owns the cache save.
awk '
	$0 == "    needs: monthly-fuzz" {
		print
		print "    permissions:"
		print "      contents: write"
		next
	}
	{ print }
' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a merge-corpus job with permissions of its own must not satisfy the contract"

# --- Deep-reasoner findings on PR #296 (fuzztime validation bypass, retries,
#     restore-before-save, missing-leg escalation) ---------------------------

seed_fixture
# Revert the budget step's else branch from "refuse an unparsed fuzztime"
# back to the old silent default. Any Go-valid duration the three ^[0-9]+[hms]$
# arms don't match (1h30m, 0.5h, 12m0s, ...) would hit this branch, get
# budget_s=600 (which passes the -gt 720 cap check), and sail through to
# -fuzztime unchecked — go test -fuzz ignores -timeout while fuzzing, so
# nothing else bounds it.
awk '
	/^          else$/ { inside = 1; print; next }
	inside && /^          fi$/ { print "            budget_s=600"; print; inside = 0; next }
	inside { next }
	{ print }
' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a budget step else branch that silently defaults to budget_s=600 instead of exiting 1 must not satisfy the contract"

seed_fixture
# The monthly leg's FUZZ_RETRIES back to the shared default of 2 — a retry
# re-runs the FULL -fuzztime on the boundary flake, so two retries is two
# fuzztimes in one job (10m + 10m + ~2m setup = ~22m), past this job's own
# timeout-minutes and back inside the runner-shutdown window the six-leg
# split exists to stay out of.
sed 's|FUZZ_RETRIES=1 FUZZ_OUTPUT_FILE=|FUZZ_RETRIES=2 FUZZ_OUTPUT_FILE=|' \
	"${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a monthly leg passing FUZZ_RETRIES=2 instead of 1 must not satisfy the contract"

seed_fixture
# merge-corpus back to if: always() — would also run the job (and its Save
# fuzz corpus step) on a CANCELLED run, writing a near-empty corpus under a
# fresh key that restore-keys would then hand to the next run as the newest
# entry for this fuzzer.
sed 's|^    if: \${{ !cancelled() }}$|    if: always()|' \
	"${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
# shellcheck disable=SC2016 # Message text only, no expansion intended.
expect_fail 'a merge-corpus job using if: always() instead of if: ${{ !cancelled() }} must not satisfy the contract'

seed_fixture
# Delete merge-corpus's own "Restore fuzz corpus" step. Before the split,
# one job did restore -> fuzz -> save in sequence, so a lost update against
# the nightly's own writer was impossible; without this restore, the merge
# step's save can silently overwrite a nightly write that landed inside this
# job's own window with a union that never saw it.
awk '
	$0 == "      - name: Restore fuzz corpus" { skip = 1; next }
	skip && (/^      - name:/ || /^  [^[:space:]]/) { skip = 0 }
	skip { next }
	{ print }
' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a merge-corpus job with no restore step before merging and saving must not satisfy the contract"

seed_fixture
# Delete the merge step's missing-leg escalation block (kind="infra" on
# fewer than expected_legs legs reporting), leaving expected_legs=6 itself
# and the warning in place. kind stays seeded "pass" and only ratchets up
# from leg-status.json records that actually arrived — a leg torn down by
# the runner never runs its own always() upload and contributes nothing, so
# a red run (fewer than 6 legs reported) would summarize as kind=pass, the
# exact case this block exists to catch.
awk '
	/^          if \[ "\$\{legs_found\}" -lt "\$\{expected_legs\}" \] && \[ "\$\(rank "\$\{kind\}"\)" -lt "\$\(rank infra\)" \]; then$/ { skip = 1; next }
	skip && /^          fi$/ { skip = 0; next }
	skip { next }
	{ print }
' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a merge step with no missing-leg kind=infra escalation must not satisfy the contract"

# --- Crash classification lives in scripts/ci/fuzz-run.sh (PW-5.10) --------

fuzz_run="${fixture}/scripts/ci/fuzz-run.sh"
lefthook="${fixture}/lefthook.yml"
go_fuzz="${fixture}/scripts/ci/go-fuzz.sh"

seed_fixture
# The shared script losing the seed-corpus-regression phrase from its own
# classifier condition. first-discovery stays, so a classifier that only
# greps the old string would still pass this.
sed 's|failure while testing seed corpus entry|failure while testing seed corpus ENTRY|' \
	"${fuzz_run}" >"${fuzz_run}.tmp"
mv "${fuzz_run}.tmp" "${fuzz_run}"
expect_fail "scripts/ci/fuzz-run.sh losing the seed-corpus-regression classifier phrase must not satisfy the contract"

seed_fixture
# Same, for the first-discovery phrase.
sed 's|Failing input written to testdata|Failing input written to TESTDATA|' \
	"${fuzz_run}" >"${fuzz_run}.tmp"
mv "${fuzz_run}.tmp" "${fuzz_run}"
expect_fail "scripts/ci/fuzz-run.sh losing the first-discovery classifier phrase must not satisfy the contract"

seed_fixture
# A caller reimplementing the grep inline instead of calling the shared
# script — exactly the duplication PW-5.10 removed. lefthook.yml still calls
# fuzz-run.sh here too, so a bare "does this caller invoke the shared script"
# check would miss the reintroduced duplicate classifier entirely.
awk '
	/FUZZ_RETRIES=2 FUZZ_TIMEOUT=1m/ {
		print "          grep -q \"Failing input written to testdata\" /dev/null && true"
	}
	{ print }
' "${lefthook}" >"${lefthook}.tmp"
mv "${lefthook}.tmp" "${lefthook}"
expect_fail "a caller reimplementing the crash-phrase grep inline must not satisfy the contract"

seed_fixture
# A caller dropping the retry env — the explicit FUZZ_RETRIES=2 that makes
# the retry budget visible at the call site instead of relying silently on
# fuzz-run.sh's own default.
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
sed 's|FUZZ_RETRIES=2 FUZZ_OUTPUT_FILE="\$GITHUB_OUTPUT" FUZZ_LOG_FILE="fuzz-run-\${FUZZER}.log" bash scripts/ci/fuzz-run.sh|FUZZ_OUTPUT_FILE="$GITHUB_OUTPUT" FUZZ_LOG_FILE="fuzz-run-${FUZZER}.log" bash scripts/ci/fuzz-run.sh|' \
	"${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "a caller dropping the FUZZ_RETRIES retry env must not satisfy the contract"

seed_fixture
# go-fuzz.sh dropping the retry env too — a separate caller, separate line.
sed '/^FUZZ_RETRIES=2 \\$/d' "${go_fuzz}" >"${go_fuzz}.tmp"
mv "${go_fuzz}.tmp" "${go_fuzz}"
expect_fail "scripts/ci/go-fuzz.sh dropping the FUZZ_RETRIES retry env must not satisfy the contract"

seed_fixture
# A caller commenting out its real call while leaving a comment that
# mentions fuzz-run.sh's path — a bare "does the path appear in the file"
# check (or "does FUZZ_RETRIES=2 appear in the file") is satisfiable by the
# leftover comment alone once the actual invocation is gone.
awk '
	/FUZZ_RETRIES=2 FUZZ_TIMEOUT=1m/ {
		print "        # was: calls scripts/ci/fuzz-run.sh with FUZZ_RETRIES=2"
		print "        # " $0
		next
	}
	{ print }
' "${lefthook}" >"${lefthook}.tmp"
mv "${lefthook}.tmp" "${lefthook}"
expect_fail "a caller with its real call commented out, and a comment mentioning the script path in its place, must not satisfy the contract"

seed_fixture
# A caller reimplementing the crash-phrase grep inline with single quotes —
# the same duplication as above, but in a quoting style the old classifier
# check (anchored to a double-quoted grep only) would have missed.
inline_grep_line="grep -q 'failure while testing seed corpus entry' /dev/null && true"
awk -v ins="${inline_grep_line}" '
	/FUZZ_RETRIES=2 \\$/ { print; print ins; next }
	{ print }
' "${go_fuzz}" >"${go_fuzz}.tmp"
mv "${go_fuzz}.tmp" "${go_fuzz}"
expect_fail "a caller reimplementing the crash-phrase grep inline with single quotes must not satisfy the contract"

seed_fixture
# A caller keeping the anchored FUZZ_RETRIES=2 prefix and the
# bash .../fuzz-run.sh text on the same line, but as a trailing comment
# after a real command that never runs it — strip_comments only dropped
# full-line comments, so this line's non-comment prefix
# (`FUZZ_RETRIES=2 true`) satisfied the anchored FUZZ_RETRIES check and the
# whole line's text satisfied the invocation check, while `true` (not
# fuzz-run.sh) is what actually runs.
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
sed 's|FUZZ_RETRIES=2 FUZZ_TIMEOUT=1m FUZZ_PARALLEL="\$workers" bash scripts/ci/fuzz-run.sh "\$2" "\$1" 5s|FUZZ_RETRIES=2 true # bash scripts/ci/fuzz-run.sh "\$2" "\$1" 5s|' \
	"${lefthook}" >"${lefthook}.tmp"
mv "${lefthook}.tmp" "${lefthook}"
expect_fail "a caller with the real call reduced to a trailing comment after an unrelated command must not satisfy the contract"

# --- Monthly/nightly cron overlap (PW codex follow-up #2) -------------------

seed_fixture
# 5 + 6 = 11 > 9 (nightly), so the 360m monthly job can still be running when
# the nightly starts and the two lanes race the shared corpus cache prefix.
sed "s|30 2 1 \* \*|30 5 1 * *|" "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a monthly cron less than 6 hours ahead of the nightly cron must not satisfy the contract"

# --- Upload-artifact if: scoped to its own step (PW codex follow-up #3) -----

seed_fixture
# Move the if: guard off the upload-artifact step and onto a neighboring step
# with a different if:, so a bare "does this string exist anywhere" check
# would still pass while the artifact upload itself runs unconditionally.
# The Upload step's own guard is now a multi-line `if: |` block preceded by
# several comment lines (PW review finding C: kind/reason gating), so a
# single getline no longer removes it -- skip everything between the name
# line and the step's `uses:` line instead, however many lines that guard
# spans.
awk '
	/^      - name: Upload fuzz corpus on failure or cancel$/ { print; skip = 1; next }
	skip && /^        uses: actions\/upload-artifact/ { skip = 0 }
	skip { next }
	/^      - name: Save fuzz corpus$/ { print; print "        if: failure() || cancelled()"; next }
	{ print }
' "${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "an if: guard attached to a different step must not satisfy the contract"

# --- Failure-upload before scoring (codex follow-up #3) ---------------------

seed_fixture
# Duplicate the 'Score corpus coverage' step immediately ahead of 'Upload
# fuzz corpus on failure or cancel', leaving the original step untouched in
# its rightful place further down. step_line() takes the FIRST match, so this
# makes the contract see 'Score corpus coverage' as running before the
# failure-upload step even though every other check (step exists, runs
# if: always(), invokes fuzz-score.sh, comes after Save fuzz corpus) still
# passes — only the ordering check between these two specific steps notices.
score_block_file="${fixture}/score-block.txt"
awk '
	$0 == "      - name: Score corpus coverage" { inside = 1; print; next }
	inside && /^      - name:/ { exit }
	inside { print }
' "${nightly}" >"${score_block_file}"
# Read the captured block back with getline rather than passing it through
# -v: awk -v does not accept an embedded newline in the assigned value on
# every awk this repo has to run under.
awk -v blockfile="${score_block_file}" '
	$0 == "      - name: Upload fuzz corpus on failure or cancel" {
		while ((getline line < blockfile) > 0) {
			print line
		}
		close(blockfile)
	}
	{ print }
' "${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "'Score corpus coverage' running before 'Upload fuzz corpus on failure or cancel' must not satisfy the contract"

# --- Verify-cleanup step must verify, not delete (CodeRabbit review follow-up) --
#
# fuzz-score.sh already cleans up exactly what it copied, from its own
# manifest, on every exit path. The paired workflow step exists only to
# catch a leftover the script's own cleanup missed, so it must never itself
# delete a cached-* file — a blanket `rm -f "${SEED}/cached-"*` would also
# remove a file the scorer deliberately left alone (a pre-existing untracked
# one, or every copy when a tracked cached-* file made it refuse to copy at
# all) — and it must reference the manifest fuzz-score.sh keeps, not
# reinvent its own notion of what survived.

seed_fixture
# Reintroduce the blanket delete inside the verify step's run block. Every
# other property of the step (name, if: always(), ordering) is untouched, so
# only this check notices the regression back to deleting instead of
# verifying.
awk '
	/^          leftover=""$/ {
		print "          rm -f \"${SEED}/cached-\"*"
		print
		next
	}
	{ print }
' "${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "the verify-cleanup step deleting cached-* files with a blanket rm -f must not satisfy the contract"

seed_fixture
# Drop every mention of "manifest" from the verify step's run block, leaving
# its behavior (and every other step) otherwise identical, so only the
# manifest-reference check notices.
awk '
	/^      - name: Verify cached corpus copies were cleaned up$/ { inside = 1 }
	inside && /^      - name:/ && !/Verify cached corpus copies were cleaned up$/ { inside = 0 }
	inside && /manifest/ { next }
	{ print }
' "${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "the verify-cleanup step with no reference to fuzz-score.sh's manifest must not satisfy the contract"

# --- Resolve corpus paths: go-list cross-check (review follow-up) ----------
#
# A wrong path derivation looks identical to a right one to every other check
# in this file: mkdir -p creates whatever was computed, actions/cache saves
# and restores it without complaint, and the corpus persists silently against
# a path go itself never reads from. Only the go-list cross-check itself can
# catch that, so its absence has to fail the contract.

seed_fixture
# Strip the go-list cross-check out of the 'Resolve corpus paths' step,
# leaving the plain derivation (mkdir -p and all) exactly as it read before
# this contract existed.
awk '
	/^          listed="\$\(go list/ { skip = 1 }
	skip && /^          fi$/ { skip = 0; next }
	skip { next }
	{ print }
' "${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "a 'Resolve corpus paths' step with no go-list cross-check must not satisfy the contract"

seed_fixture
# Same strip, on the monthly workflow: the cross-check is not nightly-only,
# so dropping it from the monthly step must fail the contract too.
awk '
	/^          listed="\$\(go list/ { skip = 1 }
	skip && /^          fi$/ { skip = 0; next }
	skip { next }
	{ print }
' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a monthly 'Resolve corpus paths' step with no go-list cross-check must not satisfy the contract"

seed_fixture
# The previous fixture strips the cross-check from every "Resolve corpus
# paths" occurrence at once, so it can't tell a resolver that only walks the
# first (leg job) step from one that walks both. Strip it from ONLY the
# second occurrence (the merge-corpus job's step), leaving the leg job's copy
# untouched, so this fails only if the contract actually reaches the second
# occurrence.
awk '
	/^      - name: Resolve corpus paths$/ { resolver++ }
	resolver == 2 && /^          listed="\$\(go list/ { skip = 1 }
	skip && /^          fi$/ { skip = 0; next }
	skip { next }
	{ print }
' "${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "the merge-corpus resolver must retain its go-list cross-check"

# --- Corpus writer concurrency (review follow-up) ---------------------------

seed_fixture
# Nightly keeps the correct shared group; monthly's diverges, so the two
# workflows' corpus-touching jobs no longer serialise against each other.
sed 's|      group: quality-fuzz-corpus-\${{ matrix.fuzzer.name }}|      group: quality-fuzz-corpus-monthly-${{ matrix.fuzzer.name }}|' \
	"${monthly}" >"${monthly}.tmp"
mv "${monthly}.tmp" "${monthly}"
expect_fail "a corpus-touching job whose concurrency group doesn't match the other fuzz-corpus workflow's must not satisfy the contract"

seed_fixture
# 6-space indent is the job-level concurrency block; the workflow-level one
# above it is indented 2 spaces and must stay untouched by this mutation.
sed 's|^      cancel-in-progress: false$|      cancel-in-progress: true|' \
	"${nightly}" >"${nightly}.tmp"
mv "${nightly}.tmp" "${nightly}"
expect_fail "a corpus-touching job's concurrency group with cancel-in-progress: true must not satisfy the contract"

seed_fixture
# A target dropped from the ClusterFuzzLite build while the four Go-engine tiers
# still run it. This is the drift the tier is most likely to acquire, because
# build.sh is the only inventory that is not a workflow file.
grep -v '^build_fuzzer internal/auth FuzzParseKeyLine$' \
	.clusterfuzzlite/build.sh >"${fixture}/.clusterfuzzlite/build.sh"
expect_fail "a fuzzer missing from .clusterfuzzlite/build.sh must not satisfy the contract"

seed_fixture
# An eleventh target built for libFuzzer that no other tier runs. Every
# per-fuzzer check still passes; only the count notices.
{
	cat .clusterfuzzlite/build.sh
	echo 'build_fuzzer internal/auth FuzzUnrelated'
} >"${fixture}/.clusterfuzzlite/build.sh"
expect_fail "an extra, unaccounted-for ClusterFuzzLite target must not satisfy the contract"

seed_fixture
# The right target built from the wrong package.
sed 's|^build_fuzzer internal/server FuzzParsePHC$|build_fuzzer internal/servers FuzzParsePHC|' \
	.clusterfuzzlite/build.sh >"${fixture}/.clusterfuzzlite/build.sh"
expect_fail "a ClusterFuzzLite target built from the wrong package must not satisfy the contract"

seed_fixture
# An eleventh target declared in the tree and listed in no tier at all. Every
# per-fuzzer check still passes, because they all iterate the inventory.
printf '\nfunc FuzzUnlisted(f *testing.F) {}\n' >>"${fixture}/internal/auth/keys_fuzz_test.go"
expect_fail "a Fuzz target declared in the tree but listed in no tier must not satisfy the contract"

seed_fixture
# The other direction: an inventory entry whose target no longer exists. The
# workflow greps still find its name in every tier, so only this check notices.
rm -f "${fixture}/internal/auth/keys_fuzz_test.go"
expect_fail "an inventory entry with no declaration left in the tree must not satisfy the contract"

echo "Fuzz tier contract self-tests passed."
