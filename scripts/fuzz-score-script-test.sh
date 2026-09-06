#!/usr/bin/env bash
set -euo pipefail

# Exercises scripts/ci/fuzz-score.sh and scripts/ci/fuzz-coverprofile-union.awk
# directly (Spec 2 — Fuzz score). No mock `go`: fuzz-score.sh's own `go test
# -run` replay is fast enough to run for real against one of this repo's own
# fuzz targets (see lefthook.yml's 5s-per-target smoke for the same
# assumption), and a real replay is what actually proves the copy/cleanup
# trick leaves the seed corpus untouched.

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
fuzz_score="${repository_root}/scripts/ci/fuzz-score.sh"
union_awk="${repository_root}/scripts/ci/fuzz-coverprofile-union.awk"
fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT

failures=0
fail() {
	echo "FAIL: $1" >&2
	failures=$((failures + 1))
}

command -v jq >/dev/null 2>&1 || {
	echo "jq is required for this test" >&2
	exit 1
}

# field() uses jq -c (compact JSON), not jq -r: -r prints a JSON string and a
# bare number identically ("4" either way), so a field that regressed from
# the number 4 to the string "4" — or from a real value to the string "null"
# instead of JSON null — would read as unchanged. -c keeps the type in the
# output ("4" for the number, "\"4\"" for the string, "null" only for JSON
# null), so a type regression fails the assertion instead of slipping past it.
field() {
	jq -c --arg k "$2" '.[$k]' "$1"
}

# --- awk union: not last-write-wins, and covers-both is max, not sum --------
#
# Two things to prove, and one fixture pair each, because the union's output
# is a single covered/total percentage and a block only reads as "covered" at
# all once its merged count is >0 — so a block covered in both inputs cannot
# tell a max-merge apart from a sum-merge through the percentage alone; 3 and
# 4 both read as ">0". The count itself has to come off FUZZ_COVER_UNION_DEBUG
# instead.
#
# Fixture 1 (not last-write-wins): block A is covered only in profile 1,
# block B only in profile 2. A last-write-wins merge would lose whichever
# block the last-read profile did not cover; keeping the max per block keeps
# both, so the union reports full coverage of both blocks while either
# profile alone reports half. (A plain sum would also keep both here — this
# fixture does not distinguish sum from max; the doc comment used to claim it
# did, which is the "overclaiming" this rewrite fixes. Fixture 2 below is
# what actually proves max over sum.)

cat >"${fixture}/prof1.out" <<'EOF'
mode: set
pkg/file.go:1.1,2.1 5 1
pkg/file.go:3.1,4.1 3 0
EOF
cat >"${fixture}/prof2.out" <<'EOF'
mode: set
pkg/file.go:1.1,2.1 5 0
pkg/file.go:3.1,4.1 3 1
EOF

solo="$(awk -f "${union_awk}" "${fixture}/prof1.out")"
[ "${solo}" = "62.50 5 8" ] ||
	fail "awk union of profile1 alone: expected '62.50 5 8', got '${solo}'"

merged="$(awk -f "${union_awk}" "${fixture}/prof1.out" "${fixture}/prof2.out")"
[ "${merged}" = "100.00 8 8" ] ||
	fail "awk union of both profiles: expected '100.00 8 8' (both blocks covered by at least one profile), got '${merged}'"

# Fixture 2 (max, not sum): one block, covered 3 times in profile A and once
# in profile B. A sum-merge would carry 4 for the merged count; a max-merge
# carries 3. Read back off FUZZ_COVER_UNION_DEBUG=1's stderr line, since the
# stdout percentage alone cannot distinguish them (both are ">0").
cat >"${fixture}/prof-a.out" <<'EOF'
mode: set
pkg/file.go:5.1,6.1 5 3
EOF
cat >"${fixture}/prof-b.out" <<'EOF'
mode: set
pkg/file.go:5.1,6.1 5 1
EOF

merge_debug="$(FUZZ_COVER_UNION_DEBUG=1 awk -f "${union_awk}" "${fixture}/prof-a.out" "${fixture}/prof-b.out" 2>&1 1>/dev/null)"
case "${merge_debug}" in
*'pkg/file.go:5.1,6.1 5 3'*) ;;
*) fail "awk union must merge a block covered in both profiles to the max count (3), got debug line '${merge_debug}'" ;;
esac
case "${merge_debug}" in
*'pkg/file.go:5.1,6.1 5 4'*) fail "awk union summed the counts (found 4) instead of taking the max (3)" ;;
*) ;;
esac

# Fixture 3 (zero coverage): one block, 21 statements, never covered. Proves
# the covered/total pair reads 0/21 rather than empty or a stale value when
# nothing in the profile is covered at all.
cat >"${fixture}/prof-zero.out" <<'EOF'
mode: set
pkg/file.go:1.1,2.1 21 0
EOF
zero="$(awk -f "${union_awk}" "${fixture}/prof-zero.out")"
[ "${zero}" = "0.00 0 21" ] ||
	fail "awk union of a fully-uncovered profile: expected '0.00 0 21', got '${zero}'"

# --- fuzz-score.sh: unreadable profile -> failed, null, exit 0 ----------
#
# A package that does not exist makes `go test` fail before it ever writes a
# coverprofile. No FUZZ_SCORE_KIND is set, so this also proves "failed" is
# reachable independently of the "crash" short-circuit below.

out="${fixture}/failed.json"
status=0
bash "${fuzz_score}" "./this/package/does/not/exist/" "FuzzNope" "" "" "" "${out}" || status=$?
[ "${status}" -eq 0 ] ||
	fail "fuzz-score.sh must exit 0 even when the underlying go test fails (got ${status})"
[ -f "${out}" ] || fail "fuzz-score.sh must still write ${out} on a go test failure"
if [ -f "${out}" ]; then
	[ "$(field "${out}" coverage_status)" = '"failed"' ] ||
		fail "unreadable profile: expected coverage_status=\"failed\", got '$(field "${out}" coverage_status)'"
	[ "$(field "${out}" corpus_coverage_pct)" = "null" ] ||
		fail "unreadable profile: expected corpus_coverage_pct=null, got '$(field "${out}" corpus_coverage_pct)'"
	[ "$(field "${out}" corpus_total)" = "0" ] ||
		fail "unreadable profile: expected corpus_total=0 (a JSON number, not the string \"0\"), got '$(field "${out}" corpus_total)'"
fi

# --- fuzz-score.sh: FUZZ_SCORE_KIND=crash short-circuits the replay -----
#
# Same nonexistent package (which would fail if the replay actually ran),
# but with the run's own fuzz outcome already known to be a crash. The
# replay must be skipped rather than independently failing on it.

out="${fixture}/crash.json"
status=0
FUZZ_SCORE_KIND=crash bash "${fuzz_score}" "./this/package/does/not/exist/" "FuzzNope" "" "" "" "${out}" || status=$?
[ "${status}" -eq 0 ] || fail "fuzz-score.sh must exit 0 on the crash short-circuit (got ${status})"
if [ -f "${out}" ]; then
	[ "$(field "${out}" coverage_status)" = '"crash"' ] ||
		fail "FUZZ_SCORE_KIND=crash: expected coverage_status=\"crash\", got '$(field "${out}" coverage_status)'"
	[ "$(field "${out}" corpus_coverage_pct)" = "null" ] ||
		fail "FUZZ_SCORE_KIND=crash: expected corpus_coverage_pct=null, got '$(field "${out}" corpus_coverage_pct)'"
	[ "$(field "${out}" kind)" = '"crash"' ] ||
		fail 'FUZZ_SCORE_KIND=crash: expected the kind field to round-trip as "crash"'
fi

# --- fuzz log parsing: last `fuzz: elapsed:` line, or null not 0 --------

log_missing="${fixture}/no-elapsed.log"
echo "some unrelated go test output" >"${log_missing}"
out="${fixture}/no-elapsed.json"
bash "${fuzz_score}" "./this/package/does/not/exist/" "FuzzNope" "" "" "${log_missing}" "${out}"
for k in new_interesting corpus_engine_total execs execs_per_sec elapsed_s corpus_engine_delta; do
	v="$(field "${out}" "${k}")"
	[ "${v}" = "null" ] ||
		fail "a log with no 'fuzz: elapsed:' line must yield ${k}=null, not '${v}' (proves null, not a false 0)"
done

# The trailing line is Go's own FINAL stats line, printed by `defer
# c.logStats()` after the workers stop: its own rate is (count -
# countLastLog)/interval = 0 while execs/new interesting/total stay
# cumulative and correct. `tail -n 1` picks this line, so execs_per_sec has
# to come from elapsed_s and execs, never from the log's own "(N/sec)".
log_present="${fixture}/with-elapsed.log"
cat >"${log_present}" <<'EOF'
fuzz: elapsed: 30s, execs: 219217 (7307/sec), new interesting: 12 (total: 89)
fuzz: elapsed: 5m0s, execs: 6173290 (20577/sec), new interesting: 4 (total: 118)
fuzz: elapsed: 5m0s, execs: 6173290 (0/sec), new interesting: 4 (total: 118)
EOF
# A real generated/seed pair instead of empty strings, so corpus_total and
# corpus_engine_delta (fix 2B) have real numbers to reconcile against the
# engine's own total (118) rather than trivially 0.
generated_with_elapsed="${fixture}/with-elapsed-generated"
seed_with_elapsed="${fixture}/with-elapsed-seed"
mkdir -p "${generated_with_elapsed}" "${seed_with_elapsed}"
printf 'go test fuzz v1\nstring("engine-cached-1")\n' >"${generated_with_elapsed}/cached1"
printf 'go test fuzz v1\nstring("engine-cached-2")\n' >"${generated_with_elapsed}/cached2"
printf 'go test fuzz v1\nstring("seed-1")\n' >"${seed_with_elapsed}/seed1"

out="${fixture}/with-elapsed.json"
bash "${fuzz_score}" "./this/package/does/not/exist/" "FuzzNope" "${generated_with_elapsed}" "${seed_with_elapsed}" "${log_present}" "${out}"
[ "$(field "${out}" new_interesting)" = "4" ] || fail "must parse new_interesting from the LAST fuzz: elapsed: line (4), got '$(field "${out}" new_interesting)'"
[ "$(field "${out}" corpus_engine_total)" = "118" ] || fail "must parse corpus_engine_total (118), got '$(field "${out}" corpus_engine_total)'"
[ "$(field "${out}" execs)" = "6173290" ] || fail "must parse execs (6173290), got '$(field "${out}" execs)'"
# execs_per_sec must come off the last line's elapsed (5m0s = 300s) and
# execs, not off its own (0/sec) rate: 6173290/300 = 20577.63 -> int() 20577.
[ "$(field "${out}" execs_per_sec)" = "20577" ] || fail "must derive execs_per_sec from elapsed_s and execs, not the log's own trailing (0/sec) (expected 20577), got '$(field "${out}" execs_per_sec)'"
[ "$(field "${out}" elapsed_s)" = "300" ] || fail "must parse elapsed_s from the last 'elapsed: 5m0s' (300), got '$(field "${out}" elapsed_s)'"
[ "$(field "${out}" corpus_total)" = "3" ] || fail "expected corpus_total=3 (1 seed + 2 engine-cached), got '$(field "${out}" corpus_total)'"
[ "$(field "${out}" corpus_engine_delta)" = "115" ] || fail "expected corpus_engine_delta=115 (118 engine total - 3 corpus total), got '$(field "${out}" corpus_engine_delta)'"

# Hour-form duration (1h1m0s = 3660s), proving the h/m/s conversion isn't
# minute-only.
log_hour="${fixture}/with-elapsed-hour.log"
cat >"${log_hour}" <<'EOF'
fuzz: elapsed: 1h1m0s, execs: 7320 (0/sec), new interesting: 0 (total: 5)
EOF
out="${fixture}/with-elapsed-hour.json"
bash "${fuzz_score}" "./this/package/does/not/exist/" "FuzzNope" "" "" "${log_hour}" "${out}"
[ "$(field "${out}" elapsed_s)" = "3660" ] || fail "hour-form duration: expected elapsed_s=3660 (1h1m0s), got '$(field "${out}" elapsed_s)'"
[ "$(field "${out}" execs_per_sec)" = "2" ] || fail "hour-form duration: expected execs_per_sec=2 (7320/3660), got '$(field "${out}" execs_per_sec)'"

# --- copy/cleanup pair: zero cached-* survive, seed dir stays clean -----
#
# Real target, real package, real `go test -run` replay — the only way to
# actually prove the trick works: copy $GOCACHE-shaped files into the
# git-tracked seed dir as cached-<basename>, replay them, then remove every
# one again. Run from the repository root so the relative package path
# resolves; every artifact this leaves in the CWD (the coverprofile, the go
# test log) is cleaned up explicitly below, on top of already being
# *.out-gitignored.
seed_dir="${repository_root}/internal/server/testdata/fuzz/FuzzParsePHC"
generated_dir="${fixture}/generated"
mkdir -p "${generated_dir}"
# Fake GOCACHE-shaped entries, in FuzzParsePHC's own real corpus format
# ("go test fuzz v1" plus one string argument) — not arbitrary bytes. A
# malformed entry doesn't get skipped by `go test -run`; it fails the whole
# replay (verified: "unmarshal: must include version and at least one
# value", exit non-zero), which would make this section indistinguishable
# from the "unreadable profile" case above instead of proving the ok path.
# shellcheck disable=SC2016 # The $ signs are literal PHC-string content, not shell expansion.
printf 'go test fuzz v1\nstring("$argon2id$fuzz-score-fixture-1$$$$")\n' >"${generated_dir}/aaaa1111"
# shellcheck disable=SC2016 # The $ signs are literal PHC-string content, not shell expansion.
printf 'go test fuzz v1\nstring("$argon2id$fuzz-score-fixture-2$$$$")\n' >"${generated_dir}/bbbb2222"

out="${fixture}/copy-cleanup.json"
status=0
(
	cd "${repository_root}"
	bash "${fuzz_score}" "./internal/server/" "FuzzParsePHC" "${generated_dir}" "${seed_dir}" "" "${out}"
) || status=$?
# Cleanup of this test's own byproducts, regardless of pass/fail below.
rm -f "${repository_root}/fuzz-cover-FuzzParsePHC.out" "${repository_root}/fuzz-score-go-test.log"

[ "${status}" -eq 0 ] || fail "fuzz-score.sh must exit 0 on the real-target replay (got ${status})"

leftover="$(find "${seed_dir}" -maxdepth 1 -name 'cached-*' -type f 2>/dev/null || true)"
[ -z "${leftover}" ] ||
	fail "cached-* corpus copies survived cleanup: ${leftover}"

seed_git_status="$(git -C "${repository_root}" status --porcelain -- "internal/server/testdata/fuzz/FuzzParsePHC" 2>/dev/null || true)"
[ -z "${seed_git_status}" ] ||
	fail "the seed corpus directory must be git-clean after the copy/cleanup pair, got: ${seed_git_status}"

if [ -f "${out}" ]; then
	cached_count="$(field "${out}" corpus_cached)"
	[ "${cached_count}" = "2" ] ||
		fail "expected corpus_cached=2 (proves the copy actually happened before cleanup), got '${cached_count}'"
	seed_count="$(field "${out}" corpus_seed)"
	[ "${seed_count}" -gt 0 ] 2>/dev/null ||
		fail "expected corpus_seed > 0 from the committed seed corpus, got '${seed_count}'"
	[ "$(field "${out}" coverage_status)" = '"ok"' ] ||
		fail "a real target's replay should score ok, got '$(field "${out}" coverage_status)'"
	[ "$(field "${out}" reason)" = "null" ] ||
		fail "expected reason=null on the ok path, got '$(field "${out}" reason)'"
	pct="$(field "${out}" corpus_coverage_pct)"
	case "${pct}" in
	'' | null) fail "expected a numeric corpus_coverage_pct for a real target, got '${pct}'" ;;
	esac
	stmts_total="$(field "${out}" corpus_coverage_stmts_total)"
	case "${stmts_total}" in
	'' | null) fail "expected a numeric corpus_coverage_stmts_total for a real target, got '${stmts_total}'" ;;
	*) [ "${stmts_total}" -gt 0 ] 2>/dev/null || fail "expected corpus_coverage_stmts_total > 0, got '${stmts_total}'" ;;
	esac
else
	fail "fuzz-score.sh did not write ${out}"
fi

# --- tracked cached-* seed: refuse the copy, record failed + a reason ------
#
# A cached-<basename> file already tracked in git under the seed dir means
# either a previous run's cleanup failed to remove it before it got
# committed, or someone hand-added one. Either way this run must never copy
# the generated corpus over it. The only way to make a file "tracked" is a
# real `git add` + commit, so this runs against a throwaway repo rather than
# this repository's own tree.
tracked_repo="${fixture}/tracked-repo"
mkdir -p "${tracked_repo}/seed"
(
	cd "${tracked_repo}"
	git init --quiet
	git config user.email test@example.invalid
	git config user.name test
	printf 'go test fuzz v1\nstring("preexisting")\n' >seed/cached-preexisting
	git add seed/cached-preexisting
	git commit --quiet -m "seed a tracked cached-* file"
)

tracked_generated="${fixture}/tracked-generated"
mkdir -p "${tracked_generated}"
printf 'go test fuzz v1\nstring("new")\n' >"${tracked_generated}/newentry"

out="${fixture}/tracked-cached.json"
status=0
(
	cd "${tracked_repo}"
	bash "${fuzz_score}" "./nope/" "FuzzNope" "${tracked_generated}" "seed" "" "${out}"
) || status=$?
[ "${status}" -eq 0 ] ||
	fail "fuzz-score.sh must exit 0 when a tracked cached-* seed file is found (got ${status})"

if [ -f "${out}" ]; then
	[ "$(field "${out}" coverage_status)" = '"failed"' ] ||
		fail "a tracked cached-* seed file must record coverage_status=\"failed\", got '$(field "${out}" coverage_status)'"
	reason="$(field "${out}" reason)"
	[ "${reason}" != "null" ] ||
		fail "a tracked cached-* seed file must record a non-null reason"
	[ "$(field "${out}" corpus_cached)" = "0" ] ||
		fail "a tracked cached-* seed file must skip the copy entirely (expected corpus_cached=0), got '$(field "${out}" corpus_cached)'"
else
	fail "fuzz-score.sh did not write ${out}"
fi

[ ! -e "${tracked_repo}/seed/cached-newentry" ] ||
	fail "the generated corpus must not be copied into a seed dir that already has a tracked cached-* file"

tracked_status="$(git -C "${tracked_repo}" status --porcelain 2>/dev/null || true)"
[ -z "${tracked_status}" ] ||
	fail "the tracked cached-* seed file must be left untouched, got git status: ${tracked_status}"

# --- manifest cleanup: deletes exactly what the copy step created ----------
#
# An untracked cached-* file already sitting in the seed dir before this run
# starts (a survivor of some earlier interrupted run, never committed) is not
# something this run copied, so the manifest-driven cleanup must leave it
# alone. A `rm -f "${seed}/cached-"*` glob would have deleted it too; the
# manifest is what makes cleanup precise instead of blunt.
manifest_repo="${fixture}/manifest-repo"
mkdir -p "${manifest_repo}/seed"
(
	cd "${manifest_repo}"
	git init --quiet
	git config user.email test@example.invalid
	git config user.name test
	: >.gitkeep
	git add .gitkeep
	git commit --quiet -m "init"
)
printf 'go test fuzz v1\nstring("orphan")\n' >"${manifest_repo}/seed/cached-orphan"

manifest_generated="${fixture}/manifest-generated"
mkdir -p "${manifest_generated}"
printf 'go test fuzz v1\nstring("fresh")\n' >"${manifest_generated}/freshentry"

out="${fixture}/manifest.json"
status=0
(
	cd "${manifest_repo}"
	bash "${fuzz_score}" "./this/package/does/not/exist/" "FuzzNope" "${manifest_generated}" "seed" "" "${out}"
) || status=$?
[ "${status}" -eq 0 ] || fail "fuzz-score.sh must exit 0 on the manifest-cleanup fixture (got ${status})"

[ -f "${manifest_repo}/seed/cached-orphan" ] ||
	fail "cleanup must only remove files the copy step itself created; it deleted a pre-existing cached-* file it never copied"
[ ! -e "${manifest_repo}/seed/cached-freshentry" ] ||
	fail "cleanup left this run's own copy (cached-freshentry) behind"

if [ "${failures}" -ne 0 ]; then
	echo "${failures} fuzz-score.sh check(s) failed" >&2
	exit 1
fi

echo "fuzz-score.sh checks passed."
