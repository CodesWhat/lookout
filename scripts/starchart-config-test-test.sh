#!/usr/bin/env bash
set -euo pipefail

# Self-test for scripts/starchart-config-test.sh. Each case breaks the real
# workflow in one specific way and asserts the contract rejects it with the
# message that names that specific breakage. A contract nobody has watched
# fail is a contract nobody knows works.
#
# Three rounds of review turned scripts/starchart-config-test.sh from "match
# a few key lines" into a thicket of exact-indent regexes, awk state
# machines, and allowlists, and this file grew a mutation for every bypass
# each round closed. A fourth round replaced all of that with a golden
# template: the exact expected contents of starchart.yml, embedded in the
# contract script, compared to the real file line by line. Under that model,
# almost all of the earlier mutations exercise the exact same mechanism --
# "an extra or changed line fails structurally, whatever it says" -- and
# produce the literal same message, because the contract no longer looks at
# what a line SAYS, only whether it matches. Where several old mutations now
# collapse to one indistinguishable assertion, this file keeps one
# representative case and says so in a comment, rather than asserting the
# same string ten times over.
#
# A fifth round found the line-by-line comparison itself had two blind
# spots: a NUL byte is silently dropped by `read`, and a missing final
# newline has no line to differ on. The contract's actual gate is now a byte
# comparison (`cmp -s` against the template rendered with the real file's
# own slot values); the line walk only explains a cmp failure afterward, so
# this file also asserts the exact fail-closed messages for the two byte-
# level cases a line comparison alone would have missed, and for
# renovate.json being absent or unusable.
#
# A sixth round closed the gap the fifth round's own line-based comparison
# still had one level down: `read` only splits on \n, but YAML also treats
# U+0085 (NEL), U+2028, and U+2029 as line breaks, so a NEL or line/paragraph
# separator hidden inside the pin's trailing comment used to read as one
# line here while a YAML parser reads it as two. The contract now asserts
# the whole file is printable ASCII plus LF as an explicit, file-wide check,
# which happens to run early enough that it also now catches the NUL-byte
# mutation below directly, by name, rather than by the byte-gate's generic
# fallback. The renovate.json cross-check also moved off an untested
# STARCHART_RENOVATE_JSON override and onto the workflow argument's own
# directory, and onto a single exact jq array-equality check in place of a
# two-step extract-then-compare, so the fixture tree below now mirrors the
# real repo's layout (workflow nested under .github/workflows/, renovate.json
# at the root next to it) instead of a flat directory the old override
# happened to resolve against.
#
# assert_rejected now matches the FULL "FAIL: ..." line by exact string
# equality, not a substring anywhere in the output. A substring match would
# have let a message drift (an interpolated value silently going missing, a
# line number silently wrong) past this file unnoticed, as long as some
# fixed fragment of the original wording still happened to appear somewhere.
# Exact-line matching means every assertion below is transcribed verbatim
# from what the contract actually printed for that exact mutation, not
# reconstructed from memory of what it was supposed to say.
#
# The three slots -- @PIN@, @PIN_COMMENT@, @BRANCH@ -- are the only positions
# the template lets vary, and they keep their own specific messages: a
# malformed pin is still "must be a full 40-hex commit SHA...", a bad branch
# is still "must not be main/master/HEAD/empty" or "must match this repo's
# dev/vX.Y convention" or the renovate.json cross-check, not a generic
# deviation. Deleting the branch: line entirely, though, is not a slot
# mutation: with no line left at that position to extract a branch value
# from, it is purely a structural deviation, caught by the byte gate on its
# own terms.
#
# The contract cross-checks branch: against renovate.json's baseBranchPatterns,
# so this fixture tree carries its own copy to stay hermetic -- no reading the
# real repo's renovate.json from a temp directory that isn't it.

test_root="$(mktemp -d "${TMPDIR:-/tmp}/portwing-starchart.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

mkdir -p "${test_root}/scripts" "${test_root}/.github/workflows"
cp scripts/starchart-config-test.sh "${test_root}/scripts/"
cp renovate.json "${test_root}/renovate.json"
fixture="${test_root}/.github/workflows/starchart.yml"

reset_fixture() {
	cp .github/workflows/starchart.yml "${fixture}"
}

run_contract() {
	local workflow_arg="${1:-.github/workflows/starchart.yml}"
	(cd "${test_root}" && bash scripts/starchart-config-test.sh "${workflow_arg}" 2>&1)
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

# expected_message is the contract's fail() text, verbatim, WITHOUT the
# "FAIL: " prefix -- assert_rejected adds that itself and requires the
# resulting line to match one line of output exactly, not merely appear as
# a substring somewhere in it (see the header comment above for why). The
# byte gate stops comparing at the first structural difference it finds
# (see the contract's own comments), so a single mutation still produces
# exactly one FAIL: line; expected_count stays here, defaulting to 1, as a
# tripwire against a future regression making one mutation trip two checks.
# workflow_arg defaults to the fixture's own path and only needs overriding
# for the one case that isn't a fixture mutation at all: a workflow argument
# that doesn't resolve to a file.
assert_rejected() {
	local expected_message="$1"
	local failure_message="$2"
	local expected_count="${3:-1}"
	local workflow_arg="${4:-.github/workflows/starchart.yml}"
	local output
	local status
	local actual_count

	set +e
	output="$(run_contract "${workflow_arg}")"
	status=$?
	set -e

	actual_count="$(grep -c '^FAIL: ' <<<"${output}" || true)"

	if [ "${status}" -eq 0 ] || ! grep -Fxq "FAIL: ${expected_message}" <<<"${output}" || [ "${actual_count}" -ne "${expected_count}" ]; then
		echo "FAIL: ${failure_message}" >&2
		echo "--- actual output (expected exact match on 'FAIL: ${expected_message}', ${expected_count} FAIL: line(s), got ${actual_count}) ---" >&2
		echo "${output}" >&2
		exit 1
	fi
}

# Insert TEXT (may contain embedded newlines) immediately after the first
# line that equals PATTERN exactly. Plain bash, no awk -v embedded-newline
# quirks and no BSD-vs-GNU sed 'a\' syntax differences to paper over.
insert_after_line() {
	local pattern="$1"
	local text="$2"
	local tmp="${fixture}.tmp"

	: >"${tmp}"
	while IFS= read -r line || [ -n "${line}" ]; do
		printf '%s\n' "${line}" >>"${tmp}"
		if [ "${line}" = "${pattern}" ]; then
			printf '%s\n' "${text}" >>"${tmp}"
		fi
	done <"${fixture}"
	mv "${tmp}" "${fixture}"
}

# The real workflow must pass its own contract. This is also this file's
# "byte-identical copy must pass" case: reset_fixture is a byte-for-byte
# copy of the real file, so this already exercises that path directly,
# without a second, redundant copy-and-run.
reset_fixture
assert_passes "the real starchart.yml must pass its own contract"

# A workflow argument that doesn't resolve to a file at all -- the
# contract's very first check, before the CRLF check, the printable-ASCII
# check, or any of the template machinery even loads. No mutation of the
# fixture applies here; run_contract's own argument is what varies.
assert_rejected \
	"workflow not found: .github/workflows/does-not-exist.yml" \
	"contract must reject a workflow argument that does not resolve to a file" \
	1 \
	.github/workflows/does-not-exist.yml

# --- line endings ------------------------------------------------------------

reset_fixture
awk '{ printf "%s\r\n", $0 }' "${fixture}" >"${fixture}.crlf" && mv "${fixture}.crlf" "${fixture}"
assert_rejected \
	"workflow file must not contain CRLF line endings (found 79 carriage return byte(s)); the template comparison assumes bare LF" \
	"contract must reject a CRLF-terminated file, and for that specific reason"

# --- the structural comparison: exact text, exact position, exact count -----

# A single trailing space on an otherwise-untouched line is still a mismatch;
# the comparison is byte-for-byte, not whitespace-tolerant.
reset_fixture
sed -i.bak 's/^permissions: {}$/permissions: {} /' "${fixture}"
assert_rejected \
	"starchart.yml deviates from the pinned shape at line 70: expected 'permissions: {}', got 'permissions: {} '" \
	"contract must reject a single trailing space on an otherwise-correct line"

# A changed comment line fails just as hard as a changed key: nothing in the
# template is exempt from the comparison, including prose that was never a
# security boundary before this round.
reset_fixture
sed -i.bak 's/^name: Star Chart$/name: Star Charts/' "${fixture}"
assert_rejected \
	"starchart.yml deviates from the pinned shape at line 1: expected 'name: Star Chart', got 'name: Star Charts'" \
	"contract must reject a changed comment/title line"

# A changed trigger, a changed tag pattern, and a deleted tag entry are all
# just changed or shifted lines now -- one representative of each shape
# (content change, and content change from a deletion shifting the next line
# into view) is enough; the mechanism is identical either way.
reset_fixture
sed -i.bak 's/^      - "v\*"$/      - "release-*"/' "${fixture}"
assert_rejected \
	'starchart.yml deviates from the pinned shape at line 67: expected '"'"'      - "v*"'"'"', got '"'"'      - "release-*"'"'"'' \
	"contract must reject a changed tags: entry"

reset_fixture
sed -i.bak '/^      - "v\*"$/d' "${fixture}"
assert_rejected \
	'starchart.yml deviates from the pinned shape at line 67: expected '"'"'      - "v*"'"'"', got '"'"'  workflow_dispatch:'"'"'' \
	"contract must reject a deleted tags: entry, caught by the next real line shifting into its place"

# An inserted line -- an extra trigger key, a second job, a decoy hiding in a
# block scalar, continue-on-error, an if: always() in any spelling, a
# secrets: in any spelling -- is the same mechanism from here on: whatever
# gets inserted is either a mismatch against the template line it displaces,
# or (inserted at the very end) a bare line-count mismatch. One of each shape
# stands in for what used to be a dozen near-identical assertions.
reset_fixture
insert_after_line "  workflow_dispatch:" '  schedule:'
assert_rejected \
	"starchart.yml deviates from the pinned shape at line 69: expected '', got '  schedule:'" \
	"contract must reject an extra schedule: trigger inserted into the on: block"

reset_fixture
sed -i.bak 's/^    permissions:$/    continue-on-error: true\n    permissions:/' "${fixture}"
assert_rejected \
	"starchart.yml deviates from the pinned shape at line 74: expected '    permissions:', got '    continue-on-error: true'" \
	"contract must reject continue-on-error inserted before the job's permissions: block"

reset_fixture
sed -i.bak 's/^    permissions:$/    secrets: inherit\n    permissions:/' "${fixture}"
assert_rejected \
	"starchart.yml deviates from the pinned shape at line 74: expected '    permissions:', got '    secrets: inherit'" \
	"contract must reject secrets: inherit inserted before the job's permissions: block"

reset_fixture
cat >>"${fixture}" <<'YAML'
  extra:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
YAML
assert_rejected \
	"starchart.yml deviates from the pinned shape: expected 79 lines, got 83" \
	"contract must reject a second job appended after the sole job -- caught purely on line count, since nothing in the template read past line 79 before"

reset_fixture
printf '\n' >>"${fixture}"
assert_rejected \
	"starchart.yml deviates from the pinned shape: expected 79 lines, got 80" \
	"contract must reject a single extra trailing blank line, on line count alone"

# The last line deleted outright: the mirror image of the two count-mismatch
# cases above, exercising the actual-file-SHORTER-than-the-template
# direction specifically (those two only exercised actual-longer).
reset_fixture
sed -i.bak '$d' "${fixture}"
assert_rejected \
	"starchart.yml deviates from the pinned shape: expected 79 lines, got 78" \
	"contract must reject the file's last line deleted outright, on line count alone"

# --- the class round 4 found: a quoted key, an anchor, an explicit key, a --
# --- spaced colon -- all now caught the same way, because none of them can -
# --- appear without changing SOME line's text, and the template doesn't ----
# --- care what changed, only that something did ----------------------------

reset_fixture
sed -i.bak "s/^    permissions:\$/    'secrets': inherit\n    permissions:/" "${fixture}"
assert_rejected \
	"starchart.yml deviates from the pinned shape at line 74: expected '    permissions:', got '    'secrets': inherit'" \
	"contract must reject a single-quoted 'secrets': key, which no tolerant regex ever needed to recognize by name"

reset_fixture
insert_after_line "name: Star Chart" 'run-name: &unconditional always()'
insert_after_line "  starchart:" '    if: *unconditional'
assert_rejected \
	"starchart.yml deviates from the pinned shape at line 2: expected '', got 'run-name: &unconditional always()'" \
	"contract must reject a YAML anchor/alias pair (if: *unconditional resolving an always() anchor on run-name:), caught at the anchor's own line before the alias is even reached"

reset_fixture
insert_after_line "  starchart:" \
	'    ? secrets
    : inherit'
assert_rejected \
	"starchart.yml deviates from the pinned shape at line 74: expected '    permissions:', got '    ? secrets'" \
	"contract must reject YAML's explicit-key mapping syntax (? secrets / : inherit), which no key: line regex could ever see"

reset_fixture
insert_after_line "  starchart:" '    concurrency : starchart'
assert_rejected \
	"starchart.yml deviates from the pinned shape at line 74: expected '    permissions:', got '    concurrency : starchart'" \
	"contract must reject a spaced colon (concurrency :), which no key-with-no-space-before-the-colon allowlist could ever recognize as a key"

# --- the class round 5 found: bytes a line-at-a-time text comparison can't -
# --- represent at all --------------------------------------------------------

# A NUL byte embedded in an otherwise-untouched line: bash's `read` drops it
# silently, so the line-based extraction and diagnostics both see the same
# text as the template. A sixth round's file-wide printable-ASCII check now
# catches this one directly, by name and byte count, before the byte gate
# even runs -- a NUL byte is as much "outside space-tilde-plus-LF" as a NEL
# is. Only a missing trailing newline (below) still has no byte for that
# check to see and falls through to the byte gate's own fallback message.
reset_fixture
python3 - "${fixture}" <<'PY'
import sys
p = sys.argv[1]
data = open(p, "rb").read()
data = data.replace(b"permissions: {}", b"permissions: {}\x00", 1)
open(p, "wb").write(data)
PY
assert_rejected \
	"workflow file must be printable ASCII plus LF (found 1 byte(s) outside that range); YAML's own line breaks (NEL, U+2028, U+2029) are otherwise invisible to a comparison built on bash's line splitting" \
	"contract must reject a NUL byte embedded in an otherwise-correct line, via the file-wide printable-ASCII check"

# A missing final newline: every line's TEXT still matches, since a line's
# text never includes the newline that ends it, and there is no missing BYTE
# for the printable-ASCII check to see either -- the byte gate's own
# fallback message is still the only thing that catches this one.
reset_fixture
python3 - "${fixture}" <<'PY'
import sys
p = sys.argv[1]
data = open(p, "rb").read()
assert data.endswith(b"\n")
open(p, "wb").write(data[:-1])
PY
assert_rejected \
	"starchart.yml deviates from the pinned shape: files differ after line 79, but every line read identically as text (a NUL byte or a missing trailing newline reads the same through a line-based comparison)" \
	"contract must reject a missing final newline, via the byte gate's fallback message, since neither the printable-ASCII check nor the line walk can see a missing byte"

# --- the class round 6 found: YAML line breaks a line-based comparison -----
# --- cannot see, because bash's `read` only ever splits on \n -------------
#
# All three mutations below are caught by the same file-wide printable-ASCII
# check as the NUL-byte case above, before extraction or the byte gate ever
# run. A pin-comment-specific ASCII check briefly existed alongside this one
# but was dead code -- the file-wide check always ran first and always
# caught the same bytes first, since nothing in this file's own template can
# carry one of these bytes without the file-wide check seeing it -- and a
# seventh round removed it rather than keep an unreachable branch around.

# NEL (U+0085, 2 UTF-8 bytes) spliced into the pin's trailing comment: a YAML
# parser reads this as a line break inside a scalar, so `secrets: inherit`
# after it would parse as a sibling job key that no line this file inspects
# would ever show as its own line.
reset_fixture
python3 - "${fixture}" <<'PY'
import sys
p = sys.argv[1]
data = open(p, "rb").read()
old = b"  # main, 2026-08-21\n"
assert old in data
nel = "\u0085".encode("utf-8")
new = old.rstrip(b"\n") + nel + b"    secrets: inherit\n"
data = data.replace(old, new, 1)
open(p, "wb").write(data)
PY
assert_rejected \
	"workflow file must be printable ASCII plus LF (found 2 byte(s) outside that range); YAML's own line breaks (NEL, U+2028, U+2029) are otherwise invisible to a comparison built on bash's line splitting" \
	"contract must reject a NEL hidden inside the pin's trailing comment, the exact bypass a sixth round reproduced against the pre-fix contract"

# U+2028 (LINE SEPARATOR, 3 UTF-8 bytes), placed outside any slot to prove
# the check is file-wide rather than pin-comment-specific.
reset_fixture
python3 - "${fixture}" <<'PY'
import sys
p = sys.argv[1]
data = open(p, "rb").read()
old = b"permissions: {}\n"
assert old in data
ls = "\u2028".encode("utf-8")
new = b"permissions: {}" + ls + b"\n"
data = data.replace(old, new, 1)
open(p, "wb").write(data)
PY
assert_rejected \
	"workflow file must be printable ASCII plus LF (found 3 byte(s) outside that range); YAML's own line breaks (NEL, U+2028, U+2029) are otherwise invisible to a comparison built on bash's line splitting" \
	"contract must reject U+2028 anywhere in the file, not just inside the pin comment"

# A tab inside the pin's trailing comment: not a YAML line break, but still
# outside printable ASCII, and "no tabs" is explicit in what the pin comment
# is allowed to hold.
reset_fixture
python3 - "${fixture}" <<'PY'
import sys
p = sys.argv[1]
data = open(p, "rb").read()
old = b"  # main, 2026-08-21\n"
assert old in data
data = data.replace(old, b"  # main,\t2026-08-21\n", 1)
open(p, "wb").write(data)
PY
assert_rejected \
	"workflow file must be printable ASCII plus LF (found 1 byte(s) outside that range); YAML's own line breaks (NEL, U+2028, U+2029) are otherwise invisible to a comparison built on bash's line splitting" \
	"contract must reject a tab inside the pin's trailing comment"

# --- the three slots: pin and pin comment -----------------------------------

reset_fixture
sed -i.bak \
	's|starchart-refresh\.yml@[0-9a-f]\{40\}|starchart-refresh.yml@main|' \
	"${fixture}"
assert_rejected \
	"the starchart-refresh.yml pin must be a full 40-hex commit SHA followed by a non-empty comment:     uses: CodesWhat/.github/.github/workflows/starchart-refresh.yml@main  # main, 2026-08-21" \
	"contract must reject the reusable workflow pinned to a branch"

reset_fixture
sed -i.bak \
	's|\(starchart-refresh\.yml@[0-9a-f]\{40\}\)  # main, 2026-08-21|\1|' \
	"${fixture}"
assert_rejected \
	"the starchart-refresh.yml pin must be a full 40-hex commit SHA followed by a non-empty comment:     uses: CodesWhat/.github/.github/workflows/starchart-refresh.yml@11004e42d7d19e86eb3b7777c467ec9522b784e1" \
	"contract must reject a SHA pin with no trailing comment"

reset_fixture
sed -i.bak \
	's|\(starchart-refresh\.yml@[0-9a-f]\{40\}\)  # main, 2026-08-21|\1  #|' \
	"${fixture}"
assert_rejected \
	"the starchart-refresh.yml pin must be a full 40-hex commit SHA followed by a non-empty comment:     uses: CodesWhat/.github/.github/workflows/starchart-refresh.yml@11004e42d7d19e86eb3b7777c467ec9522b784e1  #" \
	"contract must reject a SHA pin with an empty trailing comment"

# uses: line deleted outright: the following with: line shifts into its
# place, so this is caught structurally, at the uses: line's own position --
# extraction never found a uses: line to validate, so the slot check stays
# silent and the byte gate's own diagnostics explain it instead.
reset_fixture
sed -i.bak '/^    uses:/d' "${fixture}"
assert_rejected \
	"starchart.yml deviates from the pinned shape at line 76: expected '    uses: CodesWhat/.github/.github/workflows/starchart-refresh.yml@@PIN@  # @PIN_COMMENT@', got '    with:'" \
	"contract must reject a starchart job with no uses: line"

# --- the third slot: branch -------------------------------------------------

reset_fixture
sed -i.bak 's/^      branch: dev\/v0\.9$/      branch: main/' "${fixture}"
assert_rejected \
	"branch must not be main/master/HEAD/empty; the reusable workflow refuses these too, but this fails before a run is even needed: got 'main'" \
	"contract must reject branch: main, which the reusable workflow also refuses at run time"

reset_fixture
sed -i.bak 's/^      branch: dev\/v0\.9$/      branch: some-feature-branch/' "${fixture}"
assert_rejected \
	"branch must match this repo's dev/vX.Y convention: got 'some-feature-branch'" \
	"contract must reject a branch that isn't this repo's dev/vX.Y"

reset_fixture
sed -i.bak 's/^      branch: dev\/v0\.9$/      branch: dev\/v9.9/' "${fixture}"
assert_rejected \
	"branch: (dev/v9.9) must exactly match .github/workflows/../../renovate.json's baseBranchPatterns as a single-entry array; both roll together at a release cut" \
	"contract must reject a branch: that is shaped like dev/vX.Y but disagrees with renovate.json"

# Deleted outright: no longer a slot mutation. Extraction never finds a
# branch: line to validate, so the slot check stays silent -- the accent:
# line shifts up into the branch: line's position, and the byte gate's own
# diagnostics catch that shift the same way they catch any other deleted
# line.
reset_fixture
sed -i.bak '/^      branch: dev\/v0\.9$/d' "${fixture}"
assert_rejected \
	'starchart.yml deviates from the pinned shape at line 78: expected '"'"'      branch: @BRANCH@'"'"', got '"'"'      accent: "#7230d2"'"'"'' \
	"contract must reject a with: block with no branch: line, caught by the accent: line shifting into its place"

# accent:'s value has no placeholder -- it's pinned outright, the same as
# every other non-slot line -- so changing it is an ordinary structural
# deviation at its own line, not a slot check.
reset_fixture
sed -i.bak 's/^      accent: "#7230d2"$/      accent: "#000000"/' "${fixture}"
assert_rejected \
	'starchart.yml deviates from the pinned shape at line 79: expected '"'"'      accent: "#7230d2"'"'"', got '"'"'      accent: "#000000"'"'"'' \
	"contract must reject a changed accent: value"

# --- renovate.json itself: absent, or unusable ------------------------------
#
# The branch cross-check only runs once branch_value has already cleared the
# main/master/HEAD/empty and dev/vX.Y checks, so these mutations leave
# branch: at its correct value and only touch renovate.json. renovate_file
# is now derived from the workflow argument's own directory rather than the
# STARCHART_RENOVATE_JSON override this file used to rely on, which is why
# the messages below name it as ".github/workflows/../../renovate.json"
# rather than a bare "renovate.json" -- that's the literal, unnormalized
# path the contract builds from this fixture's own nested layout, the same
# one the real repo's default argument builds from its own root.
#
# The `jq -es --arg b ... 'length == 1 and (.[0] | type == "object") and
# (.[0].baseBranchPatterns == [$b])'` array-equality check a sixth round put
# in place of the old extract-then-compare folds malformed JSON, zero
# entries, and two entries into the exact same fail-closed message as an
# outright value mismatch: all four are just "the array isn't exactly
# [branch_value]" to a single boolean check. That collapse already existed
# for the first three under the old two-step version; the sixth round's
# version stops being able to tell them apart from a wrong VALUE too, which
# is fine -- the fix is the same either way (make renovate.json's
# baseBranchPatterns be [branch_value]) -- but it means these four cases now
# share their message with the branch/renovate.json mismatch case above, not
# just with each other.
#
# A seventh round found the sixth round's `jq -e` (no slurp) took its exit
# status from only the LAST value it printed: two JSON documents
# concatenated in one file -- {"baseBranchPatterns":["wrong"]} followed by
# {"baseBranchPatterns":["dev/v0.9"]} -- makes jq print false then true and
# exit 0, so the cross-check passed on a file whose FIRST document was
# wrong. `-s` slurps the whole input into a single array first, so there is
# exactly one output value and nothing left for jq to pick a "last" one
# from; `length == 1` on that array is what actually enforces "exactly one
# JSON document," which `-e` alone was never checking.

reset_fixture
rm -f "${test_root}/renovate.json"
assert_rejected \
	"expected .github/workflows/../../renovate.json to exist to cross-check the branch value" \
	"contract must reject a missing renovate.json"
cp renovate.json "${test_root}/renovate.json"

reset_fixture
echo '{not valid json' >"${test_root}/renovate.json"
assert_rejected \
	"branch: (dev/v0.9) must exactly match .github/workflows/../../renovate.json's baseBranchPatterns as a single-entry array; both roll together at a release cut" \
	"contract must reject malformed JSON in renovate.json, via the same exact-array-equality failure as any other renovate.json mismatch"
cp renovate.json "${test_root}/renovate.json"

reset_fixture
python3 - "${test_root}/renovate.json" <<'PY'
import json, sys
p = sys.argv[1]
d = json.load(open(p))
d["baseBranchPatterns"] = []
json.dump(d, open(p, "w"))
PY
assert_rejected \
	"branch: (dev/v0.9) must exactly match .github/workflows/../../renovate.json's baseBranchPatterns as a single-entry array; both roll together at a release cut" \
	"contract must reject renovate.json with zero baseBranchPatterns entries"
cp renovate.json "${test_root}/renovate.json"

reset_fixture
python3 - "${test_root}/renovate.json" <<'PY'
import json, sys
p = sys.argv[1]
d = json.load(open(p))
d["baseBranchPatterns"] = ["dev/v0.9", "dev/v1.0"]
json.dump(d, open(p, "w"))
PY
assert_rejected \
	"branch: (dev/v0.9) must exactly match .github/workflows/../../renovate.json's baseBranchPatterns as a single-entry array; both roll together at a release cut" \
	"contract must reject renovate.json with two baseBranchPatterns entries"
cp renovate.json "${test_root}/renovate.json"

# Two JSON documents concatenated in one file, the first wrong and the
# second correct: the exact bypass a seventh round reproduced against the
# pre-fix `jq -e` (no slurp), which took its exit status from the last of
# the two printed values and let the wrong first document through unseen.
reset_fixture
{
	echo '{"baseBranchPatterns":["wrong"]}'
	echo '{"baseBranchPatterns":["dev/v0.9"]}'
} >"${test_root}/renovate.json"
assert_rejected \
	"branch: (dev/v0.9) must exactly match .github/workflows/../../renovate.json's baseBranchPatterns as a single-entry array; both roll together at a release cut" \
	"contract must reject a concatenated multi-document renovate.json, which a bare jq -e (no slurp) took its exit status from only the last document and let a wrong first document slip through unseen"
cp renovate.json "${test_root}/renovate.json"

echo "starchart contract self-tests passed."
