#!/usr/bin/env bash
set -euo pipefail

# Exercises scripts/ci/fuzz-replay.sh directly, against a throwaway Go module
# built in mktemp -d rather than this repo's own fuzz targets: a package that
# fails to compile (kind=error reason=build) and a corpus copy into an
# unwritable directory (kind=error reason=corpus-copy) both need fixtures
# this repo's own, always-compiling fuzz packages can't provide, and a
# throwaway module keeps every fixture -- a real crash included -- out of
# this repo's own testdata/ and git status. No `-fuzz` budget is ever spent:
# every case here is a `go test -run` replay, the same <=5s smoke lefthook.yml
# and scripts/fuzz-score-script-test.sh already rely on.

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
replay="${repository_root}/scripts/ci/fuzz-replay.sh"

module="$(mktemp -d)"
# The corpus-copy fixture makes its seed dir unwritable; restore write
# permission before the trap tries to remove it, or cleanup itself fails.
trap 'chmod -R u+rwx "${module}" 2>/dev/null || true; rm -rf "${module}"' EXIT

failures=0
fail() {
	echo "FAIL: $1" >&2
	failures=$((failures + 1))
}

cat >"${module}/go.mod" <<'EOF'
module fuzzreplaytest

go 1.23
EOF

# A target with one committed seed ("hello") and a fuzz function that fails
# only on the literal input "boom" -- the panic is seed-content-driven, not a
# real bug, so a second seed can turn case A's clean pass into case B's
# regression without touching the Go source between them.
mkdir -p "${module}/pkgtest/testdata/fuzz/FuzzEcho"
cat >"${module}/pkgtest/echo_test.go" <<'EOF'
package pkgtest

import "testing"

func FuzzEcho(f *testing.F) {
	f.Add("hello")
	f.Fuzz(func(t *testing.T, s string) {
		if s == "boom" {
			t.Fatalf("boom: %q", s)
		}
	})
}
EOF
cat >"${module}/pkgtest/testdata/fuzz/FuzzEcho/seed1" <<'EOF'
go test fuzz v1
string("hello")
EOF

# A package that never compiles, for the build-error case. Its own Fuzz
# target name is irrelevant -- `go test -run '^$' -count=1 <pkg>` fails on
# the package as a whole before any -run pattern is even considered.
mkdir -p "${module}/pkgbroken"
cat >"${module}/pkgbroken/broken_test.go" <<'EOF'
package pkgbroken

func thisDoesNotCompile( {
EOF

# A GOCACHE-shaped generated corpus, for the two cases that need one: the
# crash case (to prove its cached copy is what lands in fuzz-replay-failure/)
# and the corpus-copy case (to prove a copy attempt is what fails).
mkdir -p "${module}/generated/FuzzEcho"
cat >"${module}/generated/FuzzEcho/gen1" <<'EOF'
go test fuzz v1
string("cached-ok")
EOF

no_cached_survive() {
	local dir="$1" label="$2"
	local hits
	hits="$(find "${dir}" -maxdepth 1 -type f -name 'cached-*' 2>/dev/null || true)"
	[ -z "${hits}" ] || fail "${label}: cached-* file(s) survived under ${dir}: ${hits}"
}

# --- (A) a passing target -> kind=replay, exit 0 -----------------------------
seed_dir="${module}/pkgtest/testdata/fuzz/FuzzEcho"
out_a="$(mktemp)"
status=0
(cd "${module}" && FUZZ_OUTPUT_FILE="${out_a}" bash "${replay}" "./pkgtest" "FuzzEcho" "" "${seed_dir}") || status=$?
[ "${status}" -eq 0 ] || fail "case A (pass): expected exit 0, got ${status}"
grep -Fxq "kind=replay" "${out_a}" || fail "case A (pass): expected kind=replay, got: $(cat "${out_a}")"
grep -Fxq "status=0" "${out_a}" || fail "case A (pass): expected status=0, got: $(cat "${out_a}")"
no_cached_survive "${seed_dir}" "case A"
rm -f "${out_a}"

# --- (B) a seed that panics -> kind=crash reason=seed-regression, exit 1 ----
cat >"${seed_dir}/seed-boom" <<'EOF'
go test fuzz v1
string("boom")
EOF

out_b="$(mktemp)"
status=0
(cd "${module}" && FUZZ_OUTPUT_FILE="${out_b}" bash "${replay}" "./pkgtest" "FuzzEcho" "${module}/generated/FuzzEcho" "${seed_dir}") || status=$?
[ "${status}" -eq 1 ] || fail "case B (crash): expected exit 1, got ${status}"
grep -Fxq "kind=crash" "${out_b}" || fail "case B (crash): expected kind=crash, got: $(cat "${out_b}")"
grep -Fxq "reason=seed-regression" "${out_b}" || fail "case B (crash): expected reason=seed-regression, got: $(cat "${out_b}")"
no_cached_survive "${seed_dir}" "case B"

# The reproduction artifact: this run's cached-<basename> copy of
# generated/FuzzEcho/gen1 must have been saved before the EXIT trap removed
# it from testdata, so a genuine regression is still downloadable afterward.
repro_dir="${module}/fuzz-replay-failure/FuzzEcho"
[ -d "${repro_dir}" ] || fail "case B (crash): expected reproduction dir ${repro_dir} to exist"
repro_count="$(find "${repro_dir}" -maxdepth 1 -type f 2>/dev/null | wc -l | tr -d ' ')"
[ "${repro_count}" -ge 1 ] || fail "case B (crash): expected at least one cached-corpus copy preserved under ${repro_dir}, found ${repro_count}"
rm -f "${out_b}"
rm -f "${seed_dir}/seed-boom"

# --- (C) a compile error -> kind=error reason=build, exit 2 -----------------
out_c="$(mktemp)"
status=0
(cd "${module}" && FUZZ_OUTPUT_FILE="${out_c}" bash "${replay}" "./pkgbroken" "FuzzNope" "" "${module}/pkgbroken/testdata/fuzz/FuzzNope") || status=$?
[ "${status}" -eq 2 ] || fail "case C (build): expected exit 2, got ${status}"
grep -Fxq "kind=error" "${out_c}" || fail "case C (build): expected kind=error, got: $(cat "${out_c}")"
grep -Fxq "reason=build" "${out_c}" || fail "case C (build): expected reason=build, got: $(cat "${out_c}")"
rm -f "${out_c}"

# --- (D) a copy into an unwritable dir -> kind=error reason=corpus-copy, exit 2
ro_seed_dir="${module}/ro-seed/FuzzEcho"
mkdir -p "${ro_seed_dir}"
chmod 0555 "${ro_seed_dir}"

out_d="$(mktemp)"
status=0
(cd "${module}" && FUZZ_OUTPUT_FILE="${out_d}" bash "${replay}" "./pkgtest" "FuzzEcho" "${module}/generated/FuzzEcho" "${ro_seed_dir}") || status=$?
chmod 0755 "${ro_seed_dir}"
[ "${status}" -eq 2 ] || fail "case D (corpus-copy): expected exit 2, got ${status}"
grep -Fxq "kind=error" "${out_d}" || fail "case D (corpus-copy): expected kind=error, got: $(cat "${out_d}")"
grep -Fxq "reason=corpus-copy" "${out_d}" || fail "case D (corpus-copy): expected reason=corpus-copy, got: $(cat "${out_d}")"
no_cached_survive "${ro_seed_dir}" "case D"
rm -f "${out_d}"

# --- (E) a generated cached-* entry whose shape doesn't match the target's
# signature -> kind=error reason=stale-cache, exit 2, and the stale entry is
# gone from the generated dir afterward (PW review finding A: the old script
# left it there for "Save fuzz corpus" to re-save under the next key).
mkdir -p "${module}/generated-stale/FuzzEcho"
cat >"${module}/generated-stale/FuzzEcho/badtype" <<'EOF'
go test fuzz v1
int(1)
EOF

out_e="$(mktemp)"
status=0
(cd "${module}" && FUZZ_OUTPUT_FILE="${out_e}" bash "${replay}" "./pkgtest" "FuzzEcho" "${module}/generated-stale/FuzzEcho" "${seed_dir}") || status=$?
[ "${status}" -eq 2 ] || fail "case E (stale-cache): expected exit 2, got ${status}"
grep -Fxq "kind=error" "${out_e}" || fail "case E (stale-cache): expected kind=error, got: $(cat "${out_e}")"
grep -Fxq "reason=stale-cache" "${out_e}" || fail "case E (stale-cache): expected reason=stale-cache, got: $(cat "${out_e}")"
stale_left="$(find "${module}/generated-stale/FuzzEcho" -maxdepth 1 -type f 2>/dev/null | wc -l | tr -d ' ')"
[ "${stale_left}" -eq 0 ] || fail "case E (stale-cache): expected the stale generated dir to be emptied, ${stale_left} file(s) remain"
no_cached_survive "${seed_dir}" "case E"
rm -f "${out_e}"

# --- (F) a committed seed with the wrong shape and an empty generated cache
# -> kind=error reason=seed-decode, NOT stale-cache, exit 2 (PW review
# finding B: Go prints the same decode error for a committed seed as for a
# cached-* copy, and the old script misreported both as stale-cache, which
# never self-heals a seed that needs a source fix).
cat >"${seed_dir}/seed-badtype" <<'EOF'
go test fuzz v1
int(1)
EOF

out_f="$(mktemp)"
status=0
(cd "${module}" && FUZZ_OUTPUT_FILE="${out_f}" bash "${replay}" "./pkgtest" "FuzzEcho" "" "${seed_dir}") || status=$?
[ "${status}" -eq 2 ] || fail "case F (seed-decode): expected exit 2, got ${status}"
grep -Fxq "kind=error" "${out_f}" || fail "case F (seed-decode): expected kind=error, got: $(cat "${out_f}")"
grep -Fxq "reason=seed-decode" "${out_f}" || fail "case F (seed-decode): expected reason=seed-decode, got: $(cat "${out_f}")"
if grep -Fxq "reason=stale-cache" "${out_f}"; then
	fail "case F (seed-decode): must not report reason=stale-cache for a committed seed"
fi
rm -f "${seed_dir}/seed-badtype" "${out_f}"

# --- (G) a repro save failure during a stale-cache classification:
# fuzz-replay-failure/<target> is blocked by a plain file where the
# directory needs to go, and the generated corpus has an entry whose shape
# no longer matches the target -> kind=error reason=stale-cache, exit 2,
# repro_saved=false, a ::warning:: naming the path and error (PW review
# finding C: the old script's `mkdir -p ... || true` / `cp ... || true`
# swallowed a save failure silently, so a cached-only regression could lose
# its reproducer with nothing in the log to explain why), AND (PW review
# finding E/P2) the stale entry under the generated dir survives instead of
# being deleted -- deleting it here would destroy the only remaining copy
# of the entry the reproducer save just failed to preserve elsewhere.
mkdir -p "${module}/generated-stale-g/FuzzEcho"
cat >"${module}/generated-stale-g/FuzzEcho/badtype" <<'EOF'
go test fuzz v1
int(1)
EOF
rm -rf "${module}/fuzz-replay-failure"
: >"${module}/fuzz-replay-failure"

out_g="$(mktemp)"
err_g="$(mktemp)"
status=0
(cd "${module}" && GITHUB_ACTIONS=true FUZZ_OUTPUT_FILE="${out_g}" bash "${replay}" "./pkgtest" "FuzzEcho" "${module}/generated-stale-g/FuzzEcho" "${seed_dir}") >/dev/null 2>"${err_g}" || status=$?
[ "${status}" -eq 2 ] || fail "case G (repro-save): expected exit 2 (stale-cache), got ${status}"
grep -Fxq "kind=error" "${out_g}" || fail "case G (repro-save): expected kind=error, got: $(cat "${out_g}")"
grep -Fxq "reason=stale-cache" "${out_g}" || fail "case G (repro-save): expected reason=stale-cache, got: $(cat "${out_g}")"
grep -Fxq "repro_saved=false" "${out_g}" || fail "case G (repro-save): expected repro_saved=false, got: $(cat "${out_g}")"
grep -Fq '::warning::' "${err_g}" || fail "case G (repro-save): expected a ::warning:: line on stderr, got: $(cat "${err_g}")"
stale_left_g="$(find "${module}/generated-stale-g/FuzzEcho" -maxdepth 1 -type f 2>/dev/null | wc -l | tr -d ' ')"
[ "${stale_left_g}" -eq 1 ] || fail "case G (repro-save): expected the stale generated entry to survive a failed repro save, found ${stale_left_g} file(s)"
no_cached_survive "${seed_dir}" "case G"
rm -f "${out_g}" "${err_g}"
rm -rf "${module}/fuzz-replay-failure"

# --- (H) a cached corpus entry that is an empty file -> kind=error
# reason=stale-cache, exit 2 (PW review finding D/P1: Go's
# testing/fuzz.go readCorpusData wraps unmarshalCorpusFile's error as
# "unmarshal: %v" before ReadCorpus prepends the quoted path, so an empty
# file reads "<path>": unmarshal: cannot unmarshal empty string -- the old
# decode_pattern anchored "cannot unmarshal" directly against the quoted
# path with no "unmarshal: " infix, so this case fell all the way through
# to a false kind=crash reason=seed-regression instead of being recognized
# as a stale cache entry).
mkdir -p "${module}/generated-empty/FuzzEcho"
: >"${module}/generated-empty/FuzzEcho/empty"

out_h="$(mktemp)"
status=0
(cd "${module}" && FUZZ_OUTPUT_FILE="${out_h}" bash "${replay}" "./pkgtest" "FuzzEcho" "${module}/generated-empty/FuzzEcho" "${seed_dir}") || status=$?
[ "${status}" -eq 2 ] || fail "case H (empty-cached): expected exit 2, got ${status}"
grep -Fxq "kind=error" "${out_h}" || fail "case H (empty-cached): expected kind=error, got: $(cat "${out_h}")"
grep -Fxq "reason=stale-cache" "${out_h}" || fail "case H (empty-cached): expected reason=stale-cache, got: $(cat "${out_h}")"
if grep -Fxq "reason=seed-regression" "${out_h}"; then
	fail "case H (empty-cached): must not misclassify a wrapped empty-corpus decode error as seed-regression"
fi
no_cached_survive "${seed_dir}" "case H"
rm -f "${out_h}"

# --- (I) a committed seed that is an empty file -> kind=error
# reason=seed-decode, NOT stale-cache, exit 2 (same wrapped-message gap as
# case H, but on a git-tracked seed rather than a cached-* copy -- must
# still be told apart from stale-cache the way case F already checks for
# the unwrapped decode message).
: >"${seed_dir}/seed-empty"

out_i="$(mktemp)"
status=0
(cd "${module}" && FUZZ_OUTPUT_FILE="${out_i}" bash "${replay}" "./pkgtest" "FuzzEcho" "" "${seed_dir}") || status=$?
[ "${status}" -eq 2 ] || fail "case I (empty-seed): expected exit 2, got ${status}"
grep -Fxq "kind=error" "${out_i}" || fail "case I (empty-seed): expected kind=error, got: $(cat "${out_i}")"
grep -Fxq "reason=seed-decode" "${out_i}" || fail "case I (empty-seed): expected reason=seed-decode, got: $(cat "${out_i}")"
if grep -Fxq "reason=stale-cache" "${out_i}"; then
	fail "case I (empty-seed): must not report reason=stale-cache for a committed seed"
fi
rm -f "${seed_dir}/seed-empty" "${out_i}"

# --- (J) a generated cached-* entry whose corpus header is an unknown
# encoding version ("go test fuzz v9") -> kind=error reason=stale-cache,
# exit 2 (Codex review: internal/fuzz/fuzz.go's readCorpusData wraps EVERY
# unmarshalCorpusFile error the same way, not just "cannot unmarshal ...";
# an unknown version is one of the wrapped messages the old decode_pattern
# let fall all the way through to a false kind=crash reason=seed-regression
# -- confirmed against git show e9cdc02:scripts/ci/fuzz-replay.sh).
mkdir -p "${module}/generated-v9/FuzzEcho"
cat >"${module}/generated-v9/FuzzEcho/futureversion" <<'EOF'
go test fuzz v9
string("x")
EOF

out_j="$(mktemp)"
status=0
(cd "${module}" && FUZZ_OUTPUT_FILE="${out_j}" bash "${replay}" "./pkgtest" "FuzzEcho" "${module}/generated-v9/FuzzEcho" "${seed_dir}") || status=$?
[ "${status}" -eq 2 ] || fail "case J (unknown-version-cached): expected exit 2, got ${status}"
grep -Fxq "kind=error" "${out_j}" || fail "case J (unknown-version-cached): expected kind=error, got: $(cat "${out_j}")"
grep -Fxq "reason=stale-cache" "${out_j}" || fail "case J (unknown-version-cached): expected reason=stale-cache, got: $(cat "${out_j}")"
if grep -Fxq "reason=seed-regression" "${out_j}"; then
	fail "case J (unknown-version-cached): must not misclassify a wrapped unknown-version decode error as seed-regression"
fi
no_cached_survive "${seed_dir}" "case J"
rm -f "${out_j}"

# --- (K) a committed seed whose corpus header is an unknown encoding
# version ("go test fuzz v9") -> kind=error reason=seed-decode, NOT
# stale-cache, exit 2 (same wrapped-message gap as case J, but on a
# git-tracked seed -- confirmed against e9cdc02, which also misreports this
# as reason=seed-regression).
cat >"${seed_dir}/seed-v9" <<'EOF'
go test fuzz v9
string("x")
EOF

out_k="$(mktemp)"
status=0
(cd "${module}" && FUZZ_OUTPUT_FILE="${out_k}" bash "${replay}" "./pkgtest" "FuzzEcho" "" "${seed_dir}") || status=$?
[ "${status}" -eq 2 ] || fail "case K (unknown-version-seed): expected exit 2, got ${status}"
grep -Fxq "kind=error" "${out_k}" || fail "case K (unknown-version-seed): expected kind=error, got: $(cat "${out_k}")"
grep -Fxq "reason=seed-decode" "${out_k}" || fail "case K (unknown-version-seed): expected reason=seed-decode, got: $(cat "${out_k}")"
if grep -Fxq "reason=stale-cache" "${out_k}"; then
	fail "case K (unknown-version-seed): must not report reason=stale-cache for a committed seed"
fi
rm -f "${seed_dir}/seed-v9" "${out_k}"

# --- (L) a generated cached-* entry whose filename contains a literal
# double quote -> kind=error reason=stale-cache, exit 2 (Codex review: Go's
# %q formatting of the failing path escapes an embedded quote as \", and
# the old path capture backtracked past that escaped quote -- reading
# "cached-bad\"name" as basename "name" and misclassifying this run's own
# cached copy as a committed seed, reason=seed-decode, on e9cdc02).
mkdir -p "${module}/generated-quote/FuzzEcho"
: >"${module}/generated-quote/FuzzEcho/bad\"name"

out_l="$(mktemp)"
status=0
(cd "${module}" && FUZZ_OUTPUT_FILE="${out_l}" bash "${replay}" "./pkgtest" "FuzzEcho" "${module}/generated-quote/FuzzEcho" "${seed_dir}") || status=$?
[ "${status}" -eq 2 ] || fail "case L (quoted-cached-filename): expected exit 2, got ${status}"
grep -Fxq "kind=error" "${out_l}" || fail "case L (quoted-cached-filename): expected kind=error, got: $(cat "${out_l}")"
grep -Fxq "reason=stale-cache" "${out_l}" || fail "case L (quoted-cached-filename): expected reason=stale-cache, got: $(cat "${out_l}")"
if grep -Fxq "reason=seed-decode" "${out_l}"; then
	fail "case L (quoted-cached-filename): must not misclassify its own cached copy as a committed seed"
fi
no_cached_survive "${seed_dir}" "case L"
rm -f "${out_l}"

# --- no cached-* survives outside fuzz-replay-failure/, in any case --------
#
# fuzz-replay-failure/ is the one place a cached-<basename> copy is meant to
# land (case B's assertion above already covers it); everywhere else -- every
# testdata dir this run touched -- must be clean.
lingering="$(find "${module}" -type f -name 'cached-*' ! -path '*/fuzz-replay-failure/*' 2>/dev/null || true)"
[ -z "${lingering}" ] || fail "cached-* file(s) survived outside fuzz-replay-failure/ under ${module}: ${lingering}"

if [ "${failures}" -ne 0 ]; then
	echo "${failures} fuzz-replay check(s) failed" >&2
	exit 1
fi

echo "fuzz-replay self-tests passed."
