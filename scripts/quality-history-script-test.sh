#!/usr/bin/env bash
#
# Functional test for scripts/ci/quality-history-append.sh and
# scripts/quality-history.sh, run against a throwaway bare repository.
#
# The mechanism these two scripts implement has exactly one live rehearsal a
# week (the soak lane) and one a month (mutation), and it is deliberately
# incapable of failing its caller, so a broken appender reports as a warning
# buried in a scheduled run's log. That is the same shape of silence PW-6.1
# found in the mutation gate. This test is the standing check instead: it
# drives the real scripts over a real git remote and asserts the three paths
# that matter — bootstrap, ordinary append, and a push rejected by a
# concurrent writer — plus the refusals that keep junk out of the series.
#
# The concurrency case is the one worth the setup. Sixteen mutation matrix
# legs push to one branch, so a rejected push is routine rather than
# exceptional, and the failure mode is silent data loss: a retry that force-
# pushed, or that replayed a stale fetch, would drop the record that won the
# race and nobody would ever see the gap. The competing commit is staged in
# the bare repo before the push and a pre-receive hook swaps the branch onto
# it once, which reproduces "someone else landed between your fetch and your
# push" deterministically.

set -euo pipefail
export LC_ALL=C

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
append="${repo_root}/scripts/ci/quality-history-append.sh"
query="${repo_root}/scripts/quality-history.sh"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/portwing-quality-history.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

bare="${test_root}/qh.git"
failures=0

fail() {
	echo "FAIL: $1" >&2
	failures=$((failures + 1))
}

# The appender never exits nonzero by design, so every assertion has to read
# the remote's state (or the annotation on stderr), never the exit code alone.
run_append() {
	local lane="$1"
	local numbers="$2"
	shift 2
	env \
		QUALITY_HISTORY_REMOTE="${bare}" \
		QUALITY_HISTORY_EVENT="schedule" \
		QUALITY_HISTORY_PUSH_ATTEMPTS="${QUALITY_HISTORY_PUSH_ATTEMPTS:-4}" \
		QUALITY_HISTORY_AUTHOR_NAME="test" \
		QUALITY_HISTORY_AUTHOR_EMAIL="test@example.invalid" \
		GITHUB_RUN_ID="${GITHUB_RUN_ID_OVERRIDE:-100}" \
		GITHUB_RUN_ATTEMPT="1" \
		GITHUB_RUN_NUMBER="7" \
		GITHUB_WORKFLOW="Quality: Test" \
		GITHUB_SHA="0000000000000000000000000000000000000000" \
		GITHUB_REF_NAME="dev/v0.9" \
		"$@" \
		bash "${append}" "${lane}" "${numbers}" 2>&1
}

branch_file() {
	git -C "${bare}" cat-file blob "quality-history:$1" 2>/dev/null || true
}

git init --quiet --bare "${bare}"

# --- bootstrap: the branch does not exist yet -------------------------------

output="$(run_append soak '{"rss_growth_bytes":1024,"outcome":"success"}')" || true

if ! git -C "${bare}" rev-parse --verify --quiet refs/heads/quality-history >/dev/null; then
	fail "bootstrap must create the quality-history branch (output: ${output})"
fi

soak_lines="$(branch_file soak.jsonl | grep -c . || true)"
[ "${soak_lines}" = "1" ] ||
	fail "bootstrap must write exactly one soak record, found ${soak_lines}"

[ -n "$(branch_file README.md)" ] ||
	fail "bootstrap must write a README.md explaining the branch"

# The branch has to be an orphan: sharing history with a trunk branch is what
# would drag these commits into a release tree.
parents="$(git -C "${bare}" rev-list --max-parents=0 --count refs/heads/quality-history)"
[ "${parents}" = "1" ] ||
	fail "the history branch must start from a root commit, found ${parents} roots"

# --- the envelope the appender adds ------------------------------------------

record="$(branch_file soak.jsonl | tail -n1)"
for key in lane timestamp workflow event run_id run_attempt sha ref rss_growth_bytes; do
	if ! jq -e --arg k "${key}" 'has($k)' >/dev/null 2>&1 <<<"${record}"; then
		fail "record must carry '${key}' (record: ${record})"
	fi
done
[ "$(jq -r '.lane' <<<"${record}")" = "soak" ] || fail "record lane must be the lane argument"
[ "$(jq -r '.run_id' <<<"${record}")" = "100" ] || fail "record must carry the run id"
[ "$(jq -r '.run_attempt' <<<"${record}")" = "1" ] || fail "record must carry the run attempt"
jq -e '.timestamp | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")' \
	>/dev/null <<<"${record}" || fail "timestamp must be UTC RFC3339 (got $(jq -r '.timestamp' <<<"${record}"))"

# The envelope wins a key collision: a lane must not be able to overwrite the
# provenance fields with numbers of its own.
GITHUB_RUN_ID_OVERRIDE=101 run_append soak '{"run_id":"spoofed","rss_growth_bytes":2048}' >/dev/null || true
[ "$(branch_file soak.jsonl | tail -n1 | jq -r '.run_id')" = "101" ] ||
	fail "the envelope must win a key collision with the lane's own numbers"

# --- ordinary append onto an existing branch ---------------------------------

run_append mutation '{"package":"./internal/auth","efficacy":79.17}' >/dev/null || true

[ "$(branch_file mutation.jsonl | grep -c . || true)" = "1" ] ||
	fail "a second lane must get its own JSONL file"
[ "$(branch_file soak.jsonl | grep -c . || true)" = "2" ] ||
	fail "appending one lane must not disturb another lane's file"

# --- push rejected by a concurrent writer ------------------------------------
#
# Stage a competing commit on top of the current tip, then reject exactly one
# push and swap the branch onto it. A correct appender refetches and replays;
# an appender that force-pushed or reused its first fetch would drop the
# competitor's line.

staging="${test_root}/competitor"
git clone --quiet --branch quality-history "${bare}" "${staging}"
git -C "${staging}" config user.name "competitor"
git -C "${staging}" config user.email "competitor@example.invalid"
echo '{"lane":"soak","competing":true}' >>"${staging}/soak.jsonl"
git -C "${staging}" add soak.jsonl
git -C "${staging}" commit --quiet -m "competing writer"
git -C "${staging}" push --quiet origin HEAD:refs/competitors/staged

cat >"${bare}/hooks/pre-receive" <<'HOOK'
#!/bin/sh
# Reject the first push only, having moved the branch to the staged competing
# commit.
#
# Two things about the quarantine a pre-receive hook runs inside. It refuses
# ref updates outright ("ref updates forbidden inside quarantine
# environment"), so GIT_QUARANTINE_PATH has to go before update-ref will
# move the branch. And any object the hook wrote would be discarded along
# with the rejected push, which is why the competing commit is staged into
# the repository beforehand and only pointed at from here.
marker="$(git rev-parse --git-dir)/rejected-once"
if [ -f "${marker}" ]; then
    exit 0
fi
: >"${marker}"
unset GIT_QUARANTINE_PATH
git update-ref refs/heads/quality-history refs/competitors/staged
echo "simulated concurrent writer" >&2
exit 1
HOOK
chmod +x "${bare}/hooks/pre-receive"

before="$(branch_file soak.jsonl | grep -c . || true)"
output="$(run_append soak '{"rss_growth_bytes":4096,"outcome":"success"}')" || true
after="$(branch_file soak.jsonl | grep -c . || true)"

grep -Fq "did not land, retrying" <<<"${output}" ||
	fail "a rejected push must announce the retry (output: ${output})"
[ "${after}" = "$((before + 2))" ] ||
	fail "after a rejected push the file must hold both the competitor's line and ours (${before} -> ${after})"
grep -Fq '"competing":true' <<<"$(branch_file soak.jsonl)" ||
	fail "the retry must replay onto the competitor's commit, not overwrite it"
[ "$(branch_file soak.jsonl | tail -n1 | jq -r '.rss_growth_bytes')" = "4096" ] ||
	fail "the retried record must be the last line"

rm -f "${bare}/hooks/pre-receive"

# --- refusals ----------------------------------------------------------------

lines_before="$(branch_file soak.jsonl | grep -c . || true)"

# A pull_request or push run must never write to the series.
output="$(env QUALITY_HISTORY_EVENT="pull_request" \
	QUALITY_HISTORY_REMOTE="${bare}" \
	bash "${append}" soak '{"rss_growth_bytes":1}' 2>&1)" || true
grep -Fq "not schedule or workflow_dispatch" <<<"${output}" ||
	fail "a pull_request run must be refused by name (output: ${output})"
[ "$(branch_file soak.jsonl | grep -c . || true)" = "${lines_before}" ] ||
	fail "a pull_request run must not append"

# Junk arguments warn and change nothing.
for bad in 'not json' '[1,2,3]' '"a string"'; do
	output="$(run_append soak "${bad}")" || true
	grep -Fq "::warning::" <<<"${output}" ||
		fail "a non-object numbers argument must warn (input: ${bad})"
done
output="$(run_append not-a-lane '{"x":1}')" || true
grep -Fq "unknown lane" <<<"${output}" || fail "an unknown lane must be refused"
[ "$(branch_file soak.jsonl | grep -c . || true)" = "${lines_before}" ] ||
	fail "a refused append must leave the series untouched"

# An unset event is refused rather than treated as "probably fine". Allowing
# it made the gate fail open exactly where it mattered: a caller that forgot
# to pass the event recorded unconditionally.
output="$(env -u QUALITY_HISTORY_EVENT -u GITHUB_EVENT_NAME \
	QUALITY_HISTORY_REMOTE="${bare}" \
	bash "${append}" soak '{"rss_growth_bytes":1}' 2>&1)" || true
grep -Fq "not schedule or workflow_dispatch" <<<"${output}" ||
	fail "an unset event must be refused (output: ${output})"
[ "$(branch_file soak.jsonl | grep -c . || true)" = "${lines_before}" ] ||
	fail "an unset event must not append"

# A push can land server-side and lose its response, and the retry would then
# write the same run twice. Two invocations that produce a byte-identical
# record must leave one row, not two. QUALITY_HISTORY_TIMESTAMP exists so this
# is reproducible; everything else about the record is already fixed by the
# run id, attempt and lane.
duplicate_numbers='{"rss_growth_bytes":777,"outcome":"success"}'
run_append_fixed() {
	env \
		QUALITY_HISTORY_REMOTE="${bare}" \
		QUALITY_HISTORY_EVENT="schedule" \
		QUALITY_HISTORY_TIMESTAMP="2026-09-03T00:00:00Z" \
		QUALITY_HISTORY_AUTHOR_NAME="test" \
		QUALITY_HISTORY_AUTHOR_EMAIL="test@example.invalid" \
		GITHUB_RUN_ID="900" \
		GITHUB_RUN_ATTEMPT="1" \
		GITHUB_RUN_NUMBER="9" \
		GITHUB_WORKFLOW="Quality: Test" \
		GITHUB_SHA="1111111111111111111111111111111111111111" \
		GITHUB_REF_NAME="dev/v0.9" \
		bash "${append}" soak "${duplicate_numbers}" 2>&1
}

run_append_fixed >/dev/null || true
after_first="$(branch_file soak.jsonl | grep -c . || true)"
output="$(run_append_fixed)" || true
after_second="$(branch_file soak.jsonl | grep -c . || true)"

grep -Fq "already on quality-history" <<<"${output}" ||
	fail "a repeated identical record must be recognised, not re-appended (output: ${output})"
[ "${after_second}" = "${after_first}" ] ||
	fail "a repeated identical record must not grow the file (${after_first} -> ${after_second})"
[ "$(branch_file soak.jsonl | grep -c '"rss_growth_bytes":777' || true)" = "1" ] ||
	fail "the duplicated run must appear exactly once in the series"

lines_before="$(branch_file soak.jsonl | grep -c . || true)"

# An unreachable remote warns and exits 0. This is the property that keeps a
# history outage from turning a green quality lane red.
set +e
env QUALITY_HISTORY_REMOTE="${test_root}/does-not-exist.git" \
	QUALITY_HISTORY_EVENT="schedule" \
	QUALITY_HISTORY_PUSH_ATTEMPTS=2 \
	bash "${append}" soak '{"rss_growth_bytes":1}' >"${test_root}/unreachable.log" 2>&1
status=$?
set -e
[ "${status}" -eq 0 ] ||
	fail "an unreachable remote must still exit 0, got ${status}"
grep -Fq "::warning::" "${test_root}/unreachable.log" ||
	fail "an unreachable remote must emit a warning annotation"

# --- the query helper reads what the appender wrote --------------------------

table="$(env QUALITY_HISTORY_REMOTE="${bare}" bash "${query}" soak --last 2)"
grep -Fq "timestamp" <<<"$(head -n1 <<<"${table}")" ||
	fail "the query helper must print a header row (output: ${table})"
grep -Fq "4096" <<<"${table}" ||
	fail "the query helper must show the most recent soak numbers (output: ${table})"
[ "$(grep -c . <<<"${table}")" = "3" ] ||
	fail "--last 2 must print a header and two rows, got $(grep -c . <<<"${table}") lines"

json="$(env QUALITY_HISTORY_REMOTE="${bare}" bash "${query}" soak --last 1 --json)"
jq -e '.lane == "soak"' >/dev/null <<<"${json}" ||
	fail "--json must print raw JSONL records (output: ${json})"

set +e
env QUALITY_HISTORY_REMOTE="${bare}" bash "${query}" bench >"${test_root}/bench.log" 2>&1
status=$?
set -e
[ "${status}" -ne 0 ] || fail "querying a lane with no records must fail loudly"
grep -Fq "has not recorded" "${test_root}/bench.log" ||
	fail "querying an unrecorded lane must say so (output: $(cat "${test_root}/bench.log"))"

# --- QUALITY_HISTORY_RETAIN and QUALITY_HISTORY_RETAIN_BYTES (PW-2.5) -------
#
# A separate bare remote, so the count assertions below are exact rather than
# relative to whatever the soak/mutation lanes above left behind.

bare2="${test_root}/qh-retain.git"
git init --quiet --bare "${bare2}"

branch_file2() {
	git -C "${bare2}" cat-file blob "quality-history:$1" 2>/dev/null || true
}

run_append2() {
	local lane="$1" numbers="$2" run_id="$3"
	shift 3
	env \
		QUALITY_HISTORY_REMOTE="${bare2}" \
		QUALITY_HISTORY_EVENT="schedule" \
		QUALITY_HISTORY_AUTHOR_NAME="test" \
		QUALITY_HISTORY_AUTHOR_EMAIL="test@example.invalid" \
		GITHUB_RUN_ID="${run_id}" \
		GITHUB_RUN_ATTEMPT="1" \
		GITHUB_RUN_NUMBER="${run_id}" \
		GITHUB_WORKFLOW="Quality: Test" \
		GITHUB_SHA="0000000000000000000000000000000000000000" \
		GITHUB_REF_NAME="dev/v0.9" \
		"$@" \
		bash "${append}" "${lane}" "${numbers}" 2>&1
}

# QUALITY_HISTORY_RETAIN=3 over five appends: the newest 3 lines, in order.
for marker in 1 2 3 4 5; do
	run_append2 mutation-survivors "{\"run_marker\":${marker}}" "1${marker}0" \
		QUALITY_HISTORY_RETAIN=3 >/dev/null || true
done
retained="$(branch_file2 mutation-survivors.jsonl)"
[ "$(grep -c . <<<"${retained}" || true)" = "3" ] ||
	fail "QUALITY_HISTORY_RETAIN=3 over five appends must leave exactly 3 lines"
markers="$(jq -r '.run_marker' <<<"${retained}" | paste -sd, -)"
[ "${markers}" = "3,4,5" ] ||
	fail "QUALITY_HISTORY_RETAIN must keep the newest records in order, got '${markers}'"

# RETAIN=0 is refused: a warning, and the append still happens unpruned.
output="$(run_append2 mutation-survivors '{"run_marker":6}' 160 QUALITY_HISTORY_RETAIN=0)"
grep -Fq "QUALITY_HISTORY_RETAIN must be at least 1" <<<"${output}" ||
	fail "RETAIN=0 must be refused with a warning (output: ${output})"
[ "$(grep -c . <<<"$(branch_file2 mutation-survivors.jsonl)" || true)" = "4" ] ||
	fail "a refused RETAIN must not block the append, only the pruning"

# RETAIN_BYTES=0 is refused the same way RETAIN=0 is: a warning, no byte
# ceiling enforced, and the append (and any RETAIN pruning) still happens.
output="$(run_append2 mutation-survivors '{"run_marker":6.5}' 165 \
	QUALITY_HISTORY_RETAIN=3 QUALITY_HISTORY_RETAIN_BYTES=0)"
grep -Fq "QUALITY_HISTORY_RETAIN_BYTES must be at least 1" <<<"${output}" ||
	fail "RETAIN_BYTES=0 must be refused with a warning (output: ${output})"
[ "$(grep -c . <<<"$(branch_file2 mutation-survivors.jsonl)" || true)" = "3" ] ||
	fail "a refused RETAIN_BYTES must still let RETAIN prune normally"

# Unset leaves the file unpruned: one more append grows it by exactly one.
before_unset="$(grep -c . <<<"$(branch_file2 mutation-survivors.jsonl)" || true)"
run_append2 mutation-survivors '{"run_marker":7}' 170 >/dev/null || true
after_unset="$(grep -c . <<<"$(branch_file2 mutation-survivors.jsonl)" || true)"
[ "${after_unset}" = "$((before_unset + 1))" ] ||
	fail "an unset RETAIN must not prune (${before_unset} -> ${after_unset})"

# A duplicate record under a cap is still a no-op: the duplicate guard runs
# before the append and the prune, so a repeat changes nothing.
dup_numbers='{"run_marker":8}'
run_append2 mutation-survivors "${dup_numbers}" 180 \
	QUALITY_HISTORY_RETAIN=3 QUALITY_HISTORY_TIMESTAMP="2026-09-04T00:00:00Z" >/dev/null || true
count_after_first="$(grep -c . <<<"$(branch_file2 mutation-survivors.jsonl)" || true)"
output="$(run_append2 mutation-survivors "${dup_numbers}" 180 \
	QUALITY_HISTORY_RETAIN=3 QUALITY_HISTORY_TIMESTAMP="2026-09-04T00:00:00Z")"
count_after_second="$(grep -c . <<<"$(branch_file2 mutation-survivors.jsonl)" || true)"
grep -Fq "already on" <<<"${output}" ||
	fail "a duplicate record under a cap must still be recognised (output: ${output})"
[ "${count_after_first}" = "${count_after_second}" ] ||
	fail "a duplicate record under a cap must not grow the file (${count_after_first} -> ${count_after_second})"

# --- QUALITY_HISTORY_RETAIN_BYTES: oldest-first, never the newest -----------

bare3="${test_root}/qh-bytes.git"
git init --quiet --bare "${bare3}"

branch_file3() {
	git -C "${bare3}" cat-file blob "quality-history:$1" 2>/dev/null || true
}

run_append3() {
	local lane="$1" numbers="$2" run_id="$3"
	shift 3
	env \
		QUALITY_HISTORY_REMOTE="${bare3}" \
		QUALITY_HISTORY_EVENT="schedule" \
		QUALITY_HISTORY_AUTHOR_NAME="test" \
		QUALITY_HISTORY_AUTHOR_EMAIL="test@example.invalid" \
		GITHUB_RUN_ID="${run_id}" \
		GITHUB_RUN_ATTEMPT="1" \
		GITHUB_RUN_NUMBER="${run_id}" \
		GITHUB_WORKFLOW="Quality: Test" \
		GITHUB_SHA="0000000000000000000000000000000000000000" \
		GITHUB_REF_NAME="dev/v0.9" \
		"$@" \
		bash "${append}" "${lane}" "${numbers}" 2>&1
}

pad="$(printf '%*s' 60 '')"
pad="${pad// /x}"

for marker in 1 2 3 4 5; do
	run_append3 mutation-survivors "{\"pad\":\"${pad}\",\"run_marker\":${marker}}" "2${marker}0" \
		QUALITY_HISTORY_RETAIN=1000 QUALITY_HISTORY_RETAIN_BYTES=500 >/dev/null || true
done
retained="$(branch_file3 mutation-survivors.jsonl)"
grep -Fq '"run_marker":5' <<<"${retained}" ||
	fail "the byte ceiling must never drop the newest record"
if grep -Fq '"run_marker":1' <<<"${retained}"; then
	fail "the byte ceiling must drop the oldest record first"
fi
markers="$(jq -r '.run_marker' <<<"${retained}" | paste -sd, -)"
case "${markers}" in
5 | 4,5 | 3,4,5 | 2,3,4,5 | 1,2,3,4,5) ;;
*) fail "the byte ceiling must keep a contiguous run of the newest records, got '${markers}'" ;;
esac
lines="$(grep -c . <<<"${retained}" || true)"
if [ "${lines}" -gt 1 ]; then
	size="$(printf '%s' "${retained}" | wc -c | tr -d ' ')"
	[ "${size}" -le 500 ] ||
		fail "once more than the newest record remains, the file must fit the byte ceiling"
fi

# A record that alone exceeds the ceiling is still kept, with a warning.
bigpad="$(printf '%*s' 2000 '')"
bigpad="${bigpad// /x}"
output="$(run_append3 mutation-survivors "{\"pad\":\"${bigpad}\",\"run_marker\":99}" 299 \
	QUALITY_HISTORY_RETAIN=1000 QUALITY_HISTORY_RETAIN_BYTES=500)"
grep -Fq "::warning::" <<<"${output}" ||
	fail "a newest record that alone exceeds the ceiling must still warn (output: ${output})"
grep -Fq "exceeds the 500-byte ceiling" <<<"${output}" ||
	fail "the ceiling-exceeded warning must name the byte ceiling (output: ${output})"
retained2="$(branch_file3 mutation-survivors.jsonl)"
[ "$(grep -c . <<<"${retained2}" || true)" = "1" ] ||
	fail "an oversized newest record must still drop everything older"
grep -Fq '"run_marker":99' <<<"${retained2}" ||
	fail "an oversized newest record must still be kept, not dropped"

# --- QUALITY_HISTORY_RECORD_FILE: a record too large for a single argv ------
#
# Linux caps a single argv/environ string (MAX_ARG_STRLEN) at 128 KiB; the
# spec allows a mutation-survivors record up to 1 MiB. This fixture is well
# past the 128 KiB line, so passing it as $2 the old way would fail before
# the script even started, at the shell's own execve() of the child.

bare4="${test_root}/qh-file.git"
git init --quiet --bare "${bare4}"

branch_file4() {
	git -C "${bare4}" cat-file blob "quality-history:$1" 2>/dev/null || true
}

# `head -c | tr` rather than bash's `${var// /x}`: replacing 300,000
# characters one at a time in a bash parameter expansion is quadratic and
# takes minutes; this is one read and one translate. The 300,000-byte pad
# is written straight to a file and read into jq with `--rawfile`, never
# passed as a `--arg` value: a single argv string is capped at 128 KiB on
# Linux (MAX_ARG_STRLEN), so building this fixture via `--arg` would fail
# to even construct the fixture on the platform this test exists to cover.
pad_file="${test_root}/big-pad.txt"
head -c 300000 /dev/zero | tr '\0' 'x' >"${pad_file}"
big_value="$(cat "${pad_file}")"
record_file="${test_root}/big-record.json"
jq -cn --rawfile pad "${pad_file}" '{pad: $pad, outcome:"success"}' >"${record_file}"
[ "$(wc -c <"${record_file}")" -gt 131072 ] ||
	fail "the big-record fixture must itself exceed 128 KiB to be a meaningful test"

output="$(
	env \
		QUALITY_HISTORY_REMOTE="${bare4}" \
		QUALITY_HISTORY_EVENT="schedule" \
		QUALITY_HISTORY_AUTHOR_NAME="test" \
		QUALITY_HISTORY_AUTHOR_EMAIL="test@example.invalid" \
		GITHUB_RUN_ID="400" \
		GITHUB_RUN_ATTEMPT="1" \
		GITHUB_RUN_NUMBER="400" \
		GITHUB_WORKFLOW="Quality: Test" \
		GITHUB_SHA="0000000000000000000000000000000000000000" \
		GITHUB_REF_NAME="dev/v0.9" \
		QUALITY_HISTORY_RECORD_FILE="${record_file}" \
		bash "${append}" mutation-survivors 2>&1
)"
grep -Fq "recorded mutation-survivors" <<<"${output}" ||
	fail "a record read via QUALITY_HISTORY_RECORD_FILE must still append (output: ${output})"
big_recorded="$(branch_file4 mutation-survivors.jsonl)"
[ "$(grep -c . <<<"${big_recorded}" || true)" = "1" ] ||
	fail "a large record via QUALITY_HISTORY_RECORD_FILE must land as exactly one line"
[ "$(jq -r '.pad' <<<"${big_recorded}")" = "${big_value}" ] ||
	fail "a large record via QUALITY_HISTORY_RECORD_FILE must round-trip its content exactly"

# Passing both the inline argument and the env var is refused, not silently
# preferring one over the other.
output="$(
	env \
		QUALITY_HISTORY_REMOTE="${bare4}" \
		QUALITY_HISTORY_EVENT="schedule" \
		QUALITY_HISTORY_RECORD_FILE="${record_file}" \
		bash "${append}" mutation-survivors '{"x":1}' 2>&1
)"
grep -Fq "not both" <<<"${output}" ||
	fail "passing both an inline argument and QUALITY_HISTORY_RECORD_FILE must be refused (output: ${output})"

# --- QUALITY_HISTORY_RECORD_FILE: exactly one JSON document, no more, no less
#
# `jq -e 'type == "object"'` only reflects the LAST top-level value read, so
# a file holding two concatenated objects passed the old check even though
# `--slurpfile ... $numbers_arr[0]` would then silently drop the second
# document. Both a two-document file and an empty file must be refused
# loudly instead.

two_docs_file="${test_root}/two-docs-record.json"
printf '{"outcome":"success"}\n{"outcome":"success"}\n' >"${two_docs_file}"
output="$(
	env \
		QUALITY_HISTORY_REMOTE="${bare4}" \
		QUALITY_HISTORY_EVENT="schedule" \
		QUALITY_HISTORY_RECORD_FILE="${two_docs_file}" \
		bash "${append}" mutation-survivors 2>&1
)" || true
grep -Fq "exactly one JSON object" <<<"${output}" ||
	fail "a record file holding two JSON documents must be refused, not silently truncated (output: ${output})"
[ "$(grep -c . <<<"$(branch_file4 mutation-survivors.jsonl)" || true)" = "1" ] ||
	fail "a rejected two-document record file must not append a second line"

empty_docs_file="${test_root}/empty-record.json"
: >"${empty_docs_file}"
output="$(
	env \
		QUALITY_HISTORY_REMOTE="${bare4}" \
		QUALITY_HISTORY_EVENT="schedule" \
		QUALITY_HISTORY_RECORD_FILE="${empty_docs_file}" \
		bash "${append}" mutation-survivors 2>&1
)" || true
grep -Fq "exactly one JSON object" <<<"${output}" ||
	fail "an empty record file must be refused (output: ${output})"
[ "$(grep -c . <<<"$(branch_file4 mutation-survivors.jsonl)" || true)" = "1" ] ||
	fail "a rejected empty record file must not append a line"

if [ "${failures}" -ne 0 ]; then
	echo "${failures} quality-history script check(s) failed" >&2
	exit 1
fi

echo "Quality history script checks passed."
