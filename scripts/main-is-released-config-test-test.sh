#!/usr/bin/env bash
set -euo pipefail

# Self-test for scripts/main-is-released-config-test.sh. Each case breaks the
# real workflow in one specific way and asserts the contract rejects it with
# the message that names that specific breakage. A contract nobody has
# watched fail is a contract nobody knows works.

test_root="$(mktemp -d "${TMPDIR:-/tmp}/portwing-main-is-released.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

mkdir -p "${test_root}/scripts"
cp scripts/main-is-released-config-test.sh "${test_root}/scripts/"
fixture="${test_root}/workflow.yml"

reset_fixture() {
	cp .github/workflows/main-is-released.yml "${fixture}"
}

run_contract() {
	(cd "${test_root}" && bash scripts/main-is-released-config-test.sh workflow.yml 2>&1)
}

assert_passes() {
	local failure_message="$1"
	if ! run_contract >/dev/null 2>&1; then
		echo "FAIL: ${failure_message}" >&2
		echo "--- actual output ---" >&2
		run_contract >&2 || true
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

# Removes lines from (and including) start_line up to (and excluding)
# next_line, so a step block can be dropped without hand-counting how many
# lines it spans.
remove_block() {
	local start_line="$1"
	local next_line="$2"

	awk -v start_line="${start_line}" -v next_line="${next_line}" '
		$0 == start_line {
			skipping = 1
			next
		}
		$0 == next_line {
			skipping = 0
		}
		!skipping { print }
	' "${fixture}" >"${fixture}.tmp"
	mv "${fixture}.tmp" "${fixture}"
}

# The real workflow must pass its own contract.
reset_fixture
assert_passes "the real main-is-released.yml must pass its own contract"

# --- the require_tree_parity input --------------------------------------

# Input dropped from boolean to unspecified type: an untyped input is a
# string in the inputs context, so `inputs.require_tree_parity` would never
# be the boolean the tree-equality step's if: guard needs.
reset_fixture
sed -i.bak '/^        type: boolean$/d' "${fixture}"
assert_rejected \
	"require_tree_parity input must be type: boolean" \
	"contract must reject a require_tree_parity input missing type: boolean"

# Input missing its default: false. Without it a dispatch that forgets the
# input could inherit a default the workflow author didn't choose, which is
# exactly the "opt-in" property this input exists to guarantee.
reset_fixture
sed -i.bak '/^        default: false$/d' "${fixture}"
assert_rejected \
	"require_tree_parity input must default to false" \
	"contract must reject a require_tree_parity input with no default: false"

# --- the ancestry step ----------------------------------------------------

# The whole step gone: the one assertion that is supposed to run on every
# trigger has vanished, and nothing else in the file makes that claim.
reset_fixture
remove_block \
	"      - name: Assert origin/main commits are all on origin/dev/*" \
	"      - name: Assert origin/main and origin/dev/* share a tree"
assert_rejected \
	"expected an 'Assert origin/main commits are all on origin/dev/*' step" \
	"contract must reject a workflow missing the ancestry step"

# The step gains an if: guard: this is the regression the coordinator's
# review caught directly — the ancestry check silently narrowed to dispatch
# only, same shape as the tree check's own guard.
reset_fixture
sed -i.bak \
	"s|^      - name: Assert origin/main commits are all on origin/dev/\\*\$|&\\n        if: github.event_name == 'workflow_dispatch'|" \
	"${fixture}"
assert_rejected \
	"the ancestry step must not carry an if: guard" \
	"contract must reject an ancestry step that has grown its own if: guard"

# --- the tree-equality step ------------------------------------------------

# The if: guard dropped: without it a scheduled run enforces exact tree
# equality every night, which is red most of the month between promotions.
reset_fixture
sed -i.bak \
	"/if: github.event_name == 'workflow_dispatch' && inputs.require_tree_parity/d" \
	"${fixture}"
assert_rejected \
	"the tree-equality step must be gated by exactly" \
	"contract must reject a tree-equality step with no require_tree_parity guard"

# The guard present but loosened to run on every workflow_dispatch, not just
# an explicit opt-in: this reintroduces false reds for any ordinary dispatch
# of the workflow (e.g. a manual re-run) that doesn't set the input.
reset_fixture
sed -i.bak \
	"s|if: github.event_name == 'workflow_dispatch' && inputs.require_tree_parity|if: github.event_name == 'workflow_dispatch'|" \
	"${fixture}"
assert_rejected \
	"the tree-equality step must be gated by exactly" \
	"contract must reject a tree-equality guard that drops the require_tree_parity input"

echo "main-is-released contract self-tests passed."
