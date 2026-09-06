#!/usr/bin/env bash
set -euo pipefail

# Contract for .github/workflows/main-is-released.yml (PW-3.6).
#
# The workflow's tag assertion alone is satisfiable by a main that is tagged
# AND has drifted from the dev branch it was promoted from: run 33753625450
# succeeded on the tag check alone. The `tree-parity` job is the second half,
# and it asserts two different things on two different schedules:
#
#   * an ANCESTRY check (origin/main has no commit missing from the active
#     dev branch) that runs on every trigger, because it holds at all times
#     under this repo's promote-by-merge flow, dev being ahead is normal;
#   * a TREE-equality check (the house invariant, "after every promotion,
#     `git diff --quiet origin/main origin/dev/<branch>` must hold") that
#     only holds right after a promotion, so it is opt-in behind the
#     `require_tree_parity` workflow_dispatch input rather than scheduled --
#     scheduled, it would be red most of the month and teach everyone to
#     ignore it.
#
# Every assertion below defends one way this job could quietly stop proving
# either half: the job disappearing, the dev-branch discovery collapsing to a
# hard-coded name that goes stale at the next dev cut, either comparison step
# running but not naming which two refs it compared, the tree check escaping
# its input guard and running unconditionally again, a widened permission
# grant, or workflow_dispatch being dropped so the job can't be exercised on
# demand.

workflow="${1:-.github/workflows/main-is-released.yml}"

failures=0

fail() {
	echo "FAIL: $1" >&2
	failures=$((failures + 1))
}

if [ ! -f "${workflow}" ]; then
	fail "workflow not found: ${workflow}"
	exit 1
fi

# A job's own block, from its two-space key to the next one.
job_block() {
	awk -v job="  $2:" '
        $0 == job { in_job = 1; next }
        in_job && /^  [^[:space:]]/ { in_job = 0 }
        in_job { print }
    ' "$1"
}

# --- triggers ------------------------------------------------------------
#
# The `on:` block only, scoped from its own key to the next top-level key, so
# a workflow_dispatch that moved somewhere else in the file can't satisfy a
# check that is actually about what fires this workflow.

on_block="$(
	awk '
        /^on:/ { in_block = 1; next }
        in_block && /^[^[:space:]]/ { in_block = 0 }
        in_block { print }
    ' "${workflow}"
)"

[ -n "${on_block}" ] || fail "expected a top-level on: block"

grep -Eq '^  workflow_dispatch:$' <<<"${on_block}" ||
	fail "workflow must be manually dispatchable (on.workflow_dispatch), so tree-parity can be exercised on demand"

# The require_tree_parity input, scoped to workflow_dispatch specifically so
# an input of the same name under a different trigger cannot satisfy a check
# that is about what a human dispatching this workflow can opt into.
dispatch_block="$(
	awk '
	    $0 == "  workflow_dispatch:" { in_dispatch = 1; next }
	    in_dispatch && /^  [^[:space:]]/ { in_dispatch = 0 }
	    in_dispatch { print }
	' <<<"${on_block}"
)"

[ -n "${dispatch_block}" ] || fail "expected an on.workflow_dispatch block"

grep -Eq '^      require_tree_parity:$' <<<"${dispatch_block}" ||
	fail "workflow_dispatch must declare a require_tree_parity input"

input_block="$(
	awk '
	    $0 == "      require_tree_parity:" { in_input = 1; next }
	    in_input && /^      [^[:space:]]/ { in_input = 0 }
	    in_input { print }
	' <<<"${dispatch_block}"
)"

grep -Eq '^        type: boolean$' <<<"${input_block}" ||
	fail "require_tree_parity input must be type: boolean"
grep -Eq '^        default: false$' <<<"${input_block}" ||
	fail "require_tree_parity input must default to false, so the tree-equality check is opt-in, not scheduled"

# --- the tree-parity job --------------------------------------------------

parity_job="$(job_block "${workflow}" "tree-parity")"
[ -n "${parity_job}" ] || fail "expected a top-level 'tree-parity' job"

# Exact block match rather than "contains contents: read": a widened grant is
# as much a violation as a missing one, and this job needs nothing but the
# checkout it reads both refs from.
parity_permissions="$(
	awk '
        $0 == "    permissions:" { in_perms = 1; next }
        in_perms && /^    [^[:space:]]/ { in_perms = 0 }
        in_perms { print }
    ' <<<"${parity_job}" | grep -v '^[[:space:]]*$'
)"
if [ "${parity_permissions}" != "      contents: read" ]; then
	fail "tree-parity job's permissions must be exactly contents: read (found: $(tr '\n' ';' <<<"${parity_permissions}"))"
fi

# Checkout has to see every branch, not just the one the schedule/dispatch
# checks out, or the dev-branch discovery step has nothing to search.
grep -Fq "fetch-depth: 0" <<<"${parity_job}" ||
	fail "tree-parity job's checkout must use fetch-depth: 0 so origin/dev/* is present locally"

# The dev branch is discovered, not hard-coded, so the job survives the next
# dev branch cut without an edit.
grep -Fq "git branch -r" <<<"${parity_job}" ||
	fail "tree-parity job must discover the active dev branch rather than hard-coding its name"
grep -Eq "grep -E '\\^origin/dev/'" <<<"${parity_job}" ||
	fail "tree-parity job must search for origin/dev/* branches"

# Exactly one dev branch is required; zero or more than one must fail loudly
# naming what was found, not silently pick one.
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
grep -Fq '"${count}" -ne 1' <<<"${parity_job}" ||
	fail "tree-parity job must fail when the origin/dev/* branch count is not exactly 1"
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
grep -Fq 'found ${count}' <<<"${parity_job}" ||
	fail "tree-parity job must name what it found when the dev-branch count is wrong"

# --- the ancestry step: runs on every trigger, unconditionally ------------
#
# This is the check that has to hold at all times, not just right after a
# promotion, so unlike the tree-equality step below it must NOT carry an
# if: guard: an if: here would silently turn the one assertion this job
# makes on every run into an opt-in one too.
ancestry_step="$(
	awk '
	    /^      - name: Assert origin\/main commits are all on origin\/dev\/\*$/ { in_step = 1; next }
	    in_step && /^      - name: / { in_step = 0 }
	    in_step { print }
	' <<<"${parity_job}"
)"

if [ -z "${ancestry_step}" ]; then
	fail "expected an 'Assert origin/main commits are all on origin/dev/*' step"
else
	grep -Fq "git rev-list origin/main" <<<"${ancestry_step}" ||
		fail "the ancestry step must run git rev-list against origin/main"
	grep -Fq "git log --oneline origin/main" <<<"${ancestry_step}" ||
		fail "the ancestry step must print the offending commits with git log --oneline when ancestry breaks"
	grep -Fq "exit 1" <<<"${ancestry_step}" ||
		fail "the ancestry step must fail the job when origin/main carries a commit the dev branch lacks"
	if grep -Eq '^ {8}if:' <<<"${ancestry_step}"; then
		fail "the ancestry step must not carry an if: guard; it has to run on every trigger, not just on dispatch"
	fi
fi

# --- the comparison step: names both refs, fails on drift -----------------
#
# Asserted as its own step block, not file-wide: a step whose name mentions
# both refs somewhere else in the job (e.g. the discovery step) is not the
# same guarantee as the comparison step itself saying what it compared.
diff_step="$(
	awk '
        /^      - name: Assert origin\/main and origin\/dev\/\* share a tree$/ { in_step = 1; next }
        in_step && /^      - name: / { in_step = 0 }
        in_step { print }
    ' <<<"${parity_job}"
)"

if [ -z "${diff_step}" ]; then
	fail "expected an 'Assert origin/main and origin/dev/* share a tree' step naming both refs it compares"
else
	grep -Fq "git diff --quiet origin/main" <<<"${diff_step}" ||
		fail "the comparison step must run git diff --quiet against origin/main"
	# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
	grep -Fq 'git diff --stat origin/main "${DEV_REF}"' <<<"${diff_step}" ||
		fail "the comparison step must print git diff --stat between the two refs when they differ"
	grep -Fq "exit 1" <<<"${diff_step}" ||
		fail "the comparison step must fail the job when the trees differ"
	# The tree-equality claim only ever holds right after a promotion, so
	# unlike the ancestry step above it MUST carry this exact guard, or a
	# scheduled run goes red for most of the month and teaches everyone to
	# ignore this workflow.
	grep -Fq "if: github.event_name == 'workflow_dispatch' && inputs.require_tree_parity" <<<"${diff_step}" ||
		fail "the tree-equality step must be gated by exactly: if: github.event_name == 'workflow_dispatch' && inputs.require_tree_parity"
fi

# --- action pins -----------------------------------------------------------
#
# A tag or branch ref is mutable, so a supply-chain compromise of either
# action reaches a job with read access to this repo's contents.

while IFS= read -r line; do
	[ -n "${line}" ] || continue
	trimmed="${line#"${line%%[![:space:]]*}"}"
	grep -Eq '^uses: [A-Za-z0-9._/-]+@[0-9a-f]{40}[[:space:]]+# v[0-9]' <<<"${trimmed}" ||
		fail "action reference must be pinned to a 40-hex commit SHA with a version comment: ${trimmed}"
done <<<"$(grep -E '^[[:space:]]*uses:' <<<"${parity_job}" || true)"

grep -Eq '^[[:space:]]*uses: step-security/harden-runner@[0-9a-f]{40}' <<<"${parity_job}" ||
	fail "tree-parity job must run harden-runner"
grep -Eq '^[[:space:]]*egress-policy: block$' <<<"${parity_job}" ||
	fail "harden-runner must run with egress-policy: block, not audit"
grep -Fq "persist-credentials: false" <<<"${parity_job}" ||
	fail "checkout must not persist credentials"

if [ "${failures}" -ne 0 ]; then
	echo "${failures} main-is-released contract check(s) failed" >&2
	exit 1
fi

echo "main-is-released contract checks passed."
