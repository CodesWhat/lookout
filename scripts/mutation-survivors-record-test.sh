#!/usr/bin/env bash
#
# Self-test for scripts/ci/mutation-survivors-record.sh (PW-2.5).
#
# Built from fixtures rather than a real Gremlins report: the identity rule
# (a 5-line window hash plus an ordinal among same-window mutants) has to be
# exercised at its actual collision boundaries -- two mutants on one line,
# two lines with byte-identical content but different surroundings, a path
# that tries to escape the package directory -- and a real report gives no
# control over any of those.

set -euo pipefail
export LC_ALL=C

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
record="${repo_root}/scripts/ci/mutation-survivors-record.sh"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/portwing-mutation-survivors-record.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

failures=0
fail() {
	echo "FAIL: $1" >&2
	failures=$((failures + 1))
}

records_dir="${test_root}/records"
advisory_dir="${test_root}/advisory"
src_root="${test_root}/src"
mkdir -p "${records_dir}" "${advisory_dir}" "${src_root}"

# --- alpha: gated, exercises the identity rule itself -----------------------
#
# Line 6 carries two CONDITIONALS_BOUNDARY mutants (the (file,type,line)
# collision that a bare identity can't resolve; `o` has to). Line 15 carries
# a third CONDITIONALS_BOUNDARY mutant on different, unrelated source, so its
# window -- and therefore its `a` -- must differ from line 6's pair. Lines 4
# and 13 are byte-identical after trimming ("x := 1") but sit in different
# functions, so their windows -- and their `a` -- must differ too.
mkdir -p "${src_root}/internal/alpha"
cat >"${src_root}/internal/alpha/a.go" <<'EOF'
package alpha

func Foo() {
    x := 1
    y := 2
    if x > y {
        return x
    }
    return y
}

func Bar() {
    x := 1
    y := 9
    if x < y {
        return y
    }
    return x
}
EOF

mkdir -p "${records_dir}/alpha"
jq -n '{name:"alpha", package:"./internal/alpha", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/alpha/quality-history-record.json"
jq -n '{
  name: "alpha", package: "./internal/alpha",
  survivors: [
    {file:"a.go", line:6, column:8, mutator:"CONDITIONALS_BOUNDARY"},
    {file:"a.go", line:6, column:12, mutator:"CONDITIONALS_BOUNDARY"},
    {file:"a.go", line:15, column:8, mutator:"CONDITIONALS_BOUNDARY"}
  ],
  uncovered: [
    {file:"a.go", line:4, column:5, mutator:"INCREMENT_DECREMENT"},
    {file:"a.go", line:13, column:5, mutator:"INCREMENT_DECREMENT"}
  ]
}' >"${records_dir}/alpha/mutation-survivors.json"

# --- gamma: gated, a file_name that escapes the package directory -----------
mkdir -p "${records_dir}/gamma"
jq -n '{name:"gamma", package:"./internal/gamma", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/gamma/quality-history-record.json"
jq -n '{
  name: "gamma", package: "./internal/gamma",
  survivors: [{file:"../../etc/passwd", line:1, column:1, mutator:"CONDITIONALS_BOUNDARY"}],
  uncovered: []
}' >"${records_dir}/gamma/mutation-survivors.json"

# --- epsilon: gated mode with no mutation-survivors.json ---------------------
mkdir -p "${records_dir}/epsilon"
jq -n '{name:"epsilon", package:"./internal/epsilon", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/epsilon/quality-history-record.json"
# Deliberately no mutation-survivors.json: the leg reported "gated" but its
# survivors upload never landed.

# --- kappa: gated, a mutant near the top of a short file --------------------
mkdir -p "${src_root}/internal/kappa"
printf 'package kappa\nfunc K() {}\n' >"${src_root}/internal/kappa/short.go"
mkdir -p "${records_dir}/kappa"
jq -n '{name:"kappa", package:"./internal/kappa", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/kappa/quality-history-record.json"
jq -n '{
  name: "kappa", package: "./internal/kappa",
  survivors: [{file:"short.go", line:2, column:1, mutator:"REMOVE_SELF_ASSIGNMENTS"}],
  uncovered: []
}' >"${records_dir}/kappa/mutation-survivors.json"

# --- zeta: advisory, an ordinary measured report -----------------------------
mkdir -p "${src_root}/internal/zeta"
cat >"${src_root}/internal/zeta/z.go" <<'EOF'
package zeta

func Z() bool {
    a := 1
    b := 2
    return a == b
}
EOF
mkdir -p "${advisory_dir}/mutation-advisory-misc"
jq -n '{
  mutants_killed: 0, mutants_lived: 1,
  files: [{
    file_name: "z.go",
    mutations: [
      {type:"INVERT_LOGICAL", status:"LIVED", line:6, column:12},
      {type:"INVERT_LOGICAL", status:"NOT COVERED", line:4, column:5}
    ]
  }]
}' >"${advisory_dir}/mutation-advisory-misc/mutation-advisory-zeta.json"

# --- eta: advisory, txt only, the real zero-mutants phrase -----------------
#
# The gating leg's own zero-mutants check greps mutation-report.txt for this
# exact literal ("Assess the gate" step in quality-mutation-monthly.yml), so
# the advisory .txt-only case has to key off the same phrase rather than
# treating any bare .txt as a clean zero-mutants measurement.
printf 'gremlins is hunting for mutants\nNo results to report\n' \
	>"${advisory_dir}/mutation-advisory-misc/mutation-advisory-eta.txt"

# --- omicron: advisory, txt only, WITHOUT the zero-mutants phrase ----------
#
# A leg that died before Gremlins produced a report also leaves only a .txt
# behind (or none at all); it must read as "missing", not "zero-mutants".
printf 'gremlins is hunting for mutants\nsignal: killed\n' \
	>"${advisory_dir}/mutation-advisory-misc/mutation-advisory-omicron.txt"

# --- delta: advisory, an absolute file_name --------------------------------
#
# mutants_killed/mutants_lived have to be real numbers here, same as every
# other fixture meant to reach build_from_raw: since round 4's safe_count
# fix, an absent count now reads as unparseable on its own (safe_count
# treats a missing key the same as null -> "BAD"), which would make this
# assertion pass for the wrong reason -- the count check, not the absolute-
# path rejection this fixture exists to exercise.
jq -n '{mutants_killed: 0, mutants_lived: 1, files: [{file_name: "/etc/passwd", mutations: [{type:"INVERT_LOGICAL", status:"LIVED", line:1, column:1}]}]}' \
	>"${advisory_dir}/mutation-advisory-misc/mutation-advisory-delta.json"

# theta gets neither a records-dir entry nor an advisory-dir entry: both its
# gated and advisory rows must come back "missing".

# --- iota: gated, a command-injection attempt in the 'line' field -----------
#
# A downloaded artifact sets "line" to a JSON string carrying a shell
# command substitution instead of a number. l feeds `$(( ))` in
# window_hash; if it ever reached that unvalidated, this would run `touch`
# in the job that holds contents: write.
mkdir -p "${records_dir}/iota"
jq -n '{name:"iota", package:"./internal/iota", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/iota/quality-history-record.json"
pwned_marker="${test_root}/pwned"
poison_line="x[\$(touch ${pwned_marker})]"
jq -n --arg poison "${poison_line}" '{
  name: "iota", package: "./internal/iota",
  survivors: [{file:"a.go", line: $poison, column:1, mutator:"CONDITIONALS_BOUNDARY"}],
  uncovered: []
}' >"${records_dir}/iota/mutation-survivors.json"

# --- mu: gated, a malformed .survivors shape (a string, not an array) -------
mkdir -p "${records_dir}/mu"
jq -n '{name:"mu", package:"./internal/mu", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/mu/quality-history-record.json"
jq -n '{name:"mu", package:"./internal/mu", survivors:"bad", uncovered:[]}' \
	>"${records_dir}/mu/mutation-survivors.json"

# --- nu: gated, every mutant TIMED OUT (killed+lived at zero) ---------------
mkdir -p "${records_dir}/nu"
# Real Gremlins shape: MutantsTotal is killed+lived+notViable and excludes
# TIMED OUT entirely, so an all-timed-out package reports mutants_total 0,
# not >0. timed_out is the only place the real count lives (parsed by the
# workflow from the text report and carried into this JSON).
jq -n '{name:"nu", package:"./internal/nu", mode:"gated", outcome:"success", mutants_total:0, killed:0, lived:0, timed_out:5}' \
	>"${records_dir}/nu/quality-history-record.json"
jq -n '{name:"nu", package:"./internal/nu", survivors:[], uncovered:[]}' \
	>"${records_dir}/nu/mutation-survivors.json"

# --- xi: advisory, every mutant TIMED OUT ------------------------------------
#
# Real shape: four mutations, every one status "TIMED OUT" (not an empty
# files array with a total headline number -- the advisory JSON carries the
# full per-mutant status list, and that is where the real TIMED OUT count
# has to come from).
jq -n '{
  mutants_total: 0, mutants_killed: 0, mutants_lived: 0,
  files: [{
    file_name: "x.go",
    mutations: [
      {type:"CONDITIONALS_BOUNDARY", status:"TIMED OUT", line:1, column:1},
      {type:"CONDITIONALS_BOUNDARY", status:"TIMED OUT", line:2, column:1},
      {type:"CONDITIONALS_BOUNDARY", status:"TIMED OUT", line:3, column:1},
      {type:"CONDITIONALS_BOUNDARY", status:"TIMED OUT", line:4, column:1}
    ]
  }]
}' >"${advisory_dir}/mutation-advisory-misc/mutation-advisory-xi.json"

# --- sigma: gated, .survivors is an array of non-objects (not a string) -----
#
# shape_ok's earlier bug only checked the top-level value was an array, not
# that its elements were objects, so [42] passed the check and then blew up
# under `set -e` the moment the extraction tried to read .file/.mutator off
# a bare number. This must demote to unparseable, the same as mu's
# non-array shape, not abort the whole script.
mkdir -p "${records_dir}/sigma"
jq -n '{name:"sigma", package:"./internal/sigma", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/sigma/quality-history-record.json"
jq -n '{name:"sigma", package:"./internal/sigma", survivors:[42], uncovered:[]}' \
	>"${records_dir}/sigma/mutation-survivors.json"

# --- tau: advisory, .mutations is an array of non-objects (not a string) ----
#
# Same bug, advisory side: a files[].mutations entry of [42] must demote to
# unparseable rather than crashing the extraction that reads .status/.line
# off it.
jq -n '{
  mutants_total: 1, mutants_killed: 0, mutants_lived: 0,
  files: [{file_name: "t.go", mutations: [42]}]
}' >"${advisory_dir}/mutation-advisory-misc/mutation-advisory-tau.json"

# --- upsilon: gated, .survivors is JSON false, not an array or absent -------
#
# `.survivors // []` folds `false` into the "no entries" case, which the
# round-1 shape check did too, silently reporting a malformed record as a
# clean zero-mutants measurement. `false` is a wrong shape, not an empty
# list, and must demote to unparseable.
mkdir -p "${records_dir}/upsilon"
jq -n '{name:"upsilon", package:"./internal/upsilon", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/upsilon/quality-history-record.json"
jq -n '{name:"upsilon", package:"./internal/upsilon", survivors:false, uncovered:[]}' \
	>"${records_dir}/upsilon/mutation-survivors.json"

# --- phi: gated, .survivors is JSON null, not an array or absent ------------
#
# The workflow's own producer never writes .survivors as null -- it builds
# the field with a jq array comprehension, [] at minimum for zero mutants --
# so null is as wrong a shape as false or a string, not an "empty" one.
# is_arr_ok must require an array outright, or a corrupted artifact with
# survivors:null gets folded to zero survivors and recorded measured with
# complete:true instead of unparseable.
mkdir -p "${records_dir}/phi"
jq -n '{name:"phi", package:"./internal/phi", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/phi/quality-history-record.json"
jq -n '{name:"phi", package:"./internal/phi", survivors:null, uncovered:[]}' \
	>"${records_dir}/phi/mutation-survivors.json"

# --- chi: gated, killed is the JSON string "08", not a number ---------------
#
# The workflow always writes killed as a bare JSON number (`.mutants_killed
# // null`); a quoted numeral like "08" is a wrong type, not a legitimate
# value that merely looks unusual, so safe_count rejects it outright (type
# != "number") rather than coercing it. Before safe_count existed, is_digits
# accepted "08" on its own (it's all digits), and bash `$((killed + lived))`
# would then try to parse it as an octal literal and abort the whole script
# under set -e, since 8 and 9 aren't valid octal digits; `10#` in the
# arithmetic stays as defense in depth against that class of bug even though
# safe_count now closes off the path that used to reach it with this exact
# fixture.
mkdir -p "${src_root}/internal/chi"
cat >"${src_root}/internal/chi/c.go" <<'EOF'
package chi

func C() int {
    return 1
}
EOF
mkdir -p "${records_dir}/chi"
jq -n '{name:"chi", package:"./internal/chi", mode:"gated", outcome:"success", timed_out:5, killed:"08", lived:0}' \
	>"${records_dir}/chi/quality-history-record.json"
jq -n '{name:"chi", package:"./internal/chi", survivors:[{file:"c.go", line:3, column:1, mutator:"CONDITIONALS_BOUNDARY"}], uncovered:[]}' \
	>"${records_dir}/chi/mutation-survivors.json"

# --- psi: gated, timed_out is the non-numeric JSON string "x" ---------------
#
# is_digits rejects "x" outright: gated_all_timed_out must demote this to
# unparseable (its 2-exit-status case), not fall through and read the
# malformed record as an ordinary measurement the way returning a plain
# false would.
mkdir -p "${records_dir}/psi"
jq -n '{name:"psi", package:"./internal/psi", mode:"gated", outcome:"success", timed_out:"x", killed:0, lived:0}' \
	>"${records_dir}/psi/quality-history-record.json"
jq -n '{name:"psi", package:"./internal/psi", survivors:[], uncovered:[]}' \
	>"${records_dir}/psi/mutation-survivors.json"

# --- beta: gated, killed is JSON false -------------------------------------
#
# `// 0` in the old extraction folded `false` into a passing "0" before
# is_digits ever saw it, so a corrupt record with killed:false read as a
# real all-timed-out zero instead of the malformed value it is. safe_count
# must reject it outright (rc 2 -> unparseable), not silently substitute 0.
mkdir -p "${records_dir}/beta"
jq -n '{name:"beta", package:"./internal/beta", mode:"gated", outcome:"success", timed_out:5, killed:false, lived:0}' \
	>"${records_dir}/beta/quality-history-record.json"
jq -n '{name:"beta", package:"./internal/beta", survivors:[], uncovered:[]}' \
	>"${records_dir}/beta/mutation-survivors.json"

# --- lambda: gated, timed_out is entirely absent from the record ------------
#
# The workflow's writer step always emits timed_out (as a number or an
# explicit null via `// null`), so an entry missing the key outright is
# exactly as untrustworthy as one where it's present and malformed; jq reads
# a missing key the same as null, and safe_count treats null as "BAD" too.
mkdir -p "${records_dir}/lambda"
jq -n '{name:"lambda", package:"./internal/lambda", mode:"gated", outcome:"success", killed:0, lived:0}' \
	>"${records_dir}/lambda/quality-history-record.json"
jq -n '{name:"lambda", package:"./internal/lambda", survivors:[], uncovered:[]}' \
	>"${records_dir}/lambda/mutation-survivors.json"

# --- omega: gated, killed is a 20-digit number ------------------------------
#
# is_digits alone (digits-only, no length bound) would accept this and hand
# it to `$(( ))`, which silently wraps on integer overflow on bash 3.2
# rather than erroring (`$((10#18446744073709551616))` evaluates to 0
# there), turning a corrupt count into a wrong-but-plausible one instead of
# a loud rejection. The 15-digit bound on is_digits must reject it outright.
mkdir -p "${records_dir}/omega"
jq -n '{name:"omega", package:"./internal/omega", mode:"gated", outcome:"success", timed_out:5, killed:18446744073709551616, lived:0}' \
	>"${records_dir}/omega/quality-history-record.json"
jq -n '{name:"omega", package:"./internal/omega", survivors:[], uncovered:[]}' \
	>"${records_dir}/omega/mutation-survivors.json"

# --- pi: gated, a pinned anchor digest ---------------------------------------
#
# A fixed, fully-in-bounds 5-line window whose hash is computed independently
# below with the same algorithm (trim, \x1f-join, sha256, first 12 hex) and
# compared byte-for-byte, so a future edit to window_hash's plumbing (finding
# 5's awk rewrite included) can't silently change the anchor bytes.
mkdir -p "${src_root}/internal/pi"
cat >"${src_root}/internal/pi/p.go" <<'EOF'
package pi

func P() int {
    return 1
}
EOF
mkdir -p "${records_dir}/pi"
jq -n '{name:"pi", package:"./internal/pi", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/pi/quality-history-record.json"
jq -n '{name:"pi", package:"./internal/pi", survivors:[{file:"p.go", line:3, column:1, mutator:"CONDITIONALS_BOUNDARY"}], uncovered:[]}' \
	>"${records_dir}/pi/mutation-survivors.json"

# --- rho: gated, mutants fed out of (line,column) order; and an advisory ----
#     mutant on the same package that collides on (f,m,a) with the gated
#     ones, to prove the two sources number ordinals independently.
mkdir -p "${src_root}/internal/rho"
cat >"${src_root}/internal/rho/r.go" <<'EOF'
package rho

func R() int {
    a := 1
    b := 2
    if a > b {
        return a
    }
    return b
}
EOF
mkdir -p "${records_dir}/rho"
jq -n '{name:"rho", package:"./internal/rho", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/rho/quality-history-record.json"
jq -n '{
  name: "rho", package: "./internal/rho",
  survivors: [
    {file:"r.go", line:6, column:20, mutator:"CONDITIONALS_BOUNDARY"},
    {file:"r.go", line:6, column:5, mutator:"CONDITIONALS_BOUNDARY"},
    {file:"r.go", line:6, column:12, mutator:"CONDITIONALS_BOUNDARY"}
  ],
  uncovered: []
}' >"${records_dir}/rho/mutation-survivors.json"
jq -n '{
  mutants_killed: 0, mutants_lived: 1,
  files: [{
    file_name: "r.go",
    mutations: [{type:"CONDITIONALS_BOUNDARY", status:"LIVED", line:6, column:99}]
  }]
}' >"${advisory_dir}/mutation-advisory-misc/mutation-advisory-rho.json"

# --- digamma: gated, a file_name carrying the "*" glob metacharacter ------
#
# ".*/d.go" used to be the fixture that proved real_path_ok independently
# rejected something MUTANT_OK_FILTER's jq predicate accepted -- but that
# rejection turned out to be an accident of real_path_ok's unquoted `for
# seg in $1` glob-expanding ".*" against the current working directory's
# always-present "." and ".." entries, not an actual path check. Both
# layers are fixed now: MUTANT_OK_FILTER rejects any "*", "?", "[" or
# backslash byte in `f` outright (deterministic, cwd-independent), and
# real_path_ok runs its own split under `set -f` so it can no longer
# glob-expand anything. This fixture's rejection now comes from the jq
# layer, on the "*" byte, before real_path_ok ever runs; "koppa" below is
# the same check on a different metacharacter ("?"), to prove the jq
# predicate isn't keyed to "*" specifically.
mkdir -p "${records_dir}/digamma"
jq -n '{name:"digamma", package:"./internal/digamma", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/digamma/quality-history-record.json"
jq -n '{name:"digamma", package:"./internal/digamma", survivors:[{file:".*/d.go", line:3, column:1, mutator:"CONDITIONALS_BOUNDARY"}], uncovered:[]}' \
	>"${records_dir}/digamma/mutation-survivors.json"

# --- koppa: gated, a file_name carrying the "?" glob metacharacter --------
mkdir -p "${records_dir}/koppa"
jq -n '{name:"koppa", package:"./internal/koppa", mode:"gated", outcome:"success", timed_out:0, killed:0, lived:0}' \
	>"${records_dir}/koppa/quality-history-record.json"
jq -n '{name:"koppa", package:"./internal/koppa", survivors:[{file:"a?b/e.go", line:3, column:1, mutator:"CONDITIONALS_BOUNDARY"}], uncovered:[]}' \
	>"${records_dir}/koppa/mutation-survivors.json"

expected_list="${test_root}/packages.txt"
cat >"${expected_list}" <<'EOF'
alpha|./internal/alpha
gamma|./internal/gamma
epsilon|./internal/epsilon
kappa|./internal/kappa
zeta|./internal/zeta
eta|./internal/eta
omicron|./internal/omicron
delta|./internal/delta
theta|./internal/theta
iota|./internal/iota
mu|./internal/mu
nu|./internal/nu
xi|./internal/xi
sigma|./internal/sigma
tau|./internal/tau
upsilon|./internal/upsilon
phi|./internal/phi
chi|./internal/chi
psi|./internal/psi
beta|./internal/beta
lambda|./internal/lambda
omega|./internal/omega
pi|./internal/pi
rho|./internal/rho
digamma|./internal/digamma
koppa|./internal/koppa
EOF

output="$(bash "${record}" "${records_dir}" "${advisory_dir}" "${src_root}" "${expected_list}")"

jq -e 'type == "object"' >/dev/null <<<"${output}" ||
	fail "the record script must print exactly one JSON object (output: ${output})"
[ "$(jq -r '.schema' <<<"${output}")" = "1" ] || fail "schema must be 1"
[ "$(jq -r '.anchor' <<<"${output}")" = "trimline5-sha256-12" ] || fail "anchor must name the identity scheme"

# --- alpha/gated: the identity rule ------------------------------------------

alpha="$(jq -c '.packages[] | select(.name == "alpha" and .source == "gated")' <<<"${output}")"
[ -n "${alpha}" ] || fail "alpha/gated entry is missing"
[ "$(jq -r '.state' <<<"${alpha}")" = "measured" ] || fail "alpha/gated must be measured"
[ "$(jq -r '.counts.lived' <<<"${alpha}")" = "3" ] || fail "alpha/gated must count 3 lived"
[ "$(jq -r '.counts.not_covered' <<<"${alpha}")" = "2" ] || fail "alpha/gated must count 2 not_covered"
[ "$(jq '.mutants | length' <<<"${alpha}")" = "5" ] || fail "alpha/gated must carry 5 mutant identities"

line6="$(jq -c '[.mutants[] | select(.l == 6)]' <<<"${alpha}")"
[ "$(jq 'length' <<<"${line6}")" = "2" ] || fail "line 6 must carry both CONDITIONALS_BOUNDARY mutants"
a1="$(jq -r '.[0].a' <<<"${line6}")"
a2="$(jq -r '.[1].a' <<<"${line6}")"
[ "${a1}" = "${a2}" ] || fail "two mutants on the same line must share the same window hash"
o1="$(jq -r '.[0].o' <<<"${line6}")"
o2="$(jq -r '.[1].o' <<<"${line6}")"
[ "${o1}" != "${o2}" ] || fail "two mutants sharing (file,type,a) must get distinct ordinals"
[ "$(printf '%s\n%s\n' "${o1}" "${o2}" | sort -n | tr '\n' ',')" = "0,1," ] ||
	fail "the ordinals for a two-mutant collision must be 0 and 1"

line15_a="$(jq -r '.mutants[] | select(.l == 15) | .a' <<<"${alpha}")"
[ -n "${line15_a}" ] || fail "line 15's mutant is missing"
[ "${line15_a}" != "${a1}" ] ||
	fail "a same-type mutant on a different, unrelated line must not collide on 'a'"

line4_a="$(jq -r '.mutants[] | select(.l == 4) | .a' <<<"${alpha}")"
line13_a="$(jq -r '.mutants[] | select(.l == 13) | .a' <<<"${alpha}")"
[ -n "${line4_a}" ] && [ -n "${line13_a}" ] ||
	fail "the line-4/line-13 identical-text mutants are missing"
[ "${line4_a}" != "${line13_a}" ] ||
	fail "identical trimmed line text on two different lines must not collide on 'a' (window differs)"

# --- gamma/gated: a file_name that escapes the package directory ------------

gamma="$(jq -c '.packages[] | select(.name == "gamma" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${gamma}")" = "unparseable" ] ||
	fail "a '../../etc/passwd' file_name must make the gated entry unparseable"
[ "$(jq -r '.counts' <<<"${gamma}")" = "null" ] || fail "an unparseable entry must have null counts"
[ "$(jq '.mutants | length' <<<"${gamma}")" = "0" ] || fail "an unparseable entry must carry no mutants"

# --- delta/advisory: an absolute file_name -----------------------------------

delta="$(jq -c '.packages[] | select(.name == "delta" and .source == "advisory")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${delta}")" = "unparseable" ] ||
	fail "an absolute file_name ('/etc/passwd') must make the advisory entry unparseable"

# --- epsilon/gated: gated mode, no mutation-survivors.json ------------------

epsilon="$(jq -c '.packages[] | select(.name == "epsilon" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${epsilon}")" = "unmeasured" ] ||
	fail "a gated mode with no mutation-survivors.json must be unmeasured, not $(jq -r '.state' <<<"${epsilon}")"
[ "$(jq -r '.state' <<<"${epsilon}")" != "zero-mutants" ] ||
	fail "a missing survivors file must never read as zero-mutants"

# --- theta/advisory: no advisory data anywhere for this package -------------

theta_advisory="$(jq -c '.packages[] | select(.name == "theta" and .source == "advisory")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${theta_advisory}")" = "missing" ] ||
	fail "a package absent from every advisory group must be missing, not $(jq -r '.state' <<<"${theta_advisory}")"
theta_gated="$(jq -c '.packages[] | select(.name == "theta" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${theta_gated}")" = "missing" ] ||
	fail "a package with no gated record at all must be missing"

# --- zeta/advisory: an ordinary measured advisory report --------------------

zeta="$(jq -c '.packages[] | select(.name == "zeta" and .source == "advisory")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${zeta}")" = "measured" ] || fail "zeta/advisory must be measured"
[ "$(jq -r '.counts.lived' <<<"${zeta}")" = "1" ] || fail "zeta/advisory must count 1 lived"
[ "$(jq -r '.counts.not_covered' <<<"${zeta}")" = "1" ] || fail "zeta/advisory must count 1 not_covered"

# --- eta/advisory: txt only, an explicit zero-mutant leg --------------------

eta="$(jq -c '.packages[] | select(.name == "eta" and .source == "advisory")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${eta}")" = "zero-mutants" ] ||
	fail "a txt-only advisory leg (no json) must be zero-mutants, not $(jq -r '.state' <<<"${eta}")"
[ "$(jq -r '.counts.lived' <<<"${eta}")" = "0" ] && [ "$(jq -r '.counts.not_covered' <<<"${eta}")" = "0" ] ||
	fail "a zero-mutants entry must carry zeroed counts"

# --- kappa/gated: window padding near the top of a short file ---------------

kappa="$(jq -c '.packages[] | select(.name == "kappa" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${kappa}")" = "measured" ] ||
	fail "a mutant near the top of a short file must still be measured"
kappa_a="$(jq -r '.mutants[0].a' <<<"${kappa}")"
printf '%s' "${kappa_a}" | grep -Eq '^[0-9a-f]{12}$' ||
	fail "a mutant whose window runs off both ends of a short file must still hash cleanly, got '${kappa_a}'"

# --- omicron/advisory: txt only, without the zero-mutants phrase ------------

omicron="$(jq -c '.packages[] | select(.name == "omicron" and .source == "advisory")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${omicron}")" = "missing" ] ||
	fail "a .txt-only advisory leg without the zero-mutants phrase must be missing, not $(jq -r '.state' <<<"${omicron}")"

# --- iota/gated: a command-injection attempt in 'line' -----------------------

iota="$(jq -c '.packages[] | select(.name == "iota" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${iota}")" = "unparseable" ] ||
	fail "a non-numeric 'line' field must make the gated entry unparseable, not $(jq -r '.state' <<<"${iota}")"
[ ! -e "${pwned_marker}" ] ||
	fail "a malicious 'line' field must never reach shell arithmetic; found ${pwned_marker}"

# --- mu/gated: a malformed .survivors shape ----------------------------------

mu="$(jq -c '.packages[] | select(.name == "mu" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${mu}")" = "unparseable" ] ||
	fail "a non-array .survivors must make that entry unparseable, not $(jq -r '.state' <<<"${mu}")"
# A malformed shape in one package must demote only that package's entry,
# never abort the whole run: alpha (processed earlier) and rho (processed
# later) both still measured proves the script kept going past mu.
[ "$(jq -r '.packages | length' <<<"${output}")" = "52" ] ||
	fail "a malformed package must not drop other packages from the run (got $(jq -r '.packages | length' <<<"${output}") entries)"

# --- nu/gated, xi/advisory: every mutant TIMED OUT ---------------------------

nu="$(jq -c '.packages[] | select(.name == "nu" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${nu}")" = "unmeasured" ] ||
	fail "timed_out>0 with killed+lived at zero must read as unmeasured, not $(jq -r '.state' <<<"${nu}")"
[ "$(jq -r '.counts' <<<"${nu}")" = "null" ] || fail "an unmeasured entry must have null counts"

xi="$(jq -c '.packages[] | select(.name == "xi" and .source == "advisory")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${xi}")" = "unmeasured" ] ||
	fail "an all-timed-out advisory report must read as unmeasured, not $(jq -r '.state' <<<"${xi}")"

# --- sigma/gated, tau/advisory: an array of non-objects, e.g. [42] ----------

sigma="$(jq -c '.packages[] | select(.name == "sigma" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${sigma}")" = "unparseable" ] ||
	fail "a .survivors array of non-objects ([42]) must read as unparseable, not $(jq -r '.state' <<<"${sigma}")"

tau="$(jq -c '.packages[] | select(.name == "tau" and .source == "advisory")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${tau}")" = "unparseable" ] ||
	fail "a .mutations array of non-objects ([42]) must read as unparseable, not $(jq -r '.state' <<<"${tau}")"

# --- upsilon/gated: .survivors is JSON false, not an array or absent --------

upsilon="$(jq -c '.packages[] | select(.name == "upsilon" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${upsilon}")" = "unparseable" ] ||
	fail "a .survivors of false must read as unparseable, not $(jq -r '.state' <<<"${upsilon}")"

# --- phi/gated: .survivors is JSON null, not an array or absent -------------

phi="$(jq -c '.packages[] | select(.name == "phi" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${phi}")" = "unparseable" ] ||
	fail "a .survivors of null must read as unparseable, not $(jq -r '.state' <<<"${phi}")"

# --- chi/gated: killed is the JSON string "08" -------------------------------

chi="$(jq -c '.packages[] | select(.name == "chi" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${chi}")" = "unparseable" ] ||
	fail "killed as the JSON string \"08\" must read as unparseable, not $(jq -r '.state' <<<"${chi}")"

# --- psi/gated: timed_out is the non-numeric JSON string "x" ----------------

psi="$(jq -c '.packages[] | select(.name == "psi" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${psi}")" = "unparseable" ] ||
	fail "a non-numeric timed_out must read as unparseable, not $(jq -r '.state' <<<"${psi}")"

# --- beta/gated: killed is JSON false ----------------------------------------

beta="$(jq -c '.packages[] | select(.name == "beta" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${beta}")" = "unparseable" ] ||
	fail "a killed of false must read as unparseable, not $(jq -r '.state' <<<"${beta}")"

# --- lambda/gated: timed_out is absent from the record -----------------------

lambda="$(jq -c '.packages[] | select(.name == "lambda" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${lambda}")" = "unparseable" ] ||
	fail "a record missing timed_out entirely must read as unparseable, not $(jq -r '.state' <<<"${lambda}")"

# --- omega/gated: killed is a 20-digit number --------------------------------

omega="$(jq -c '.packages[] | select(.name == "omega" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${omega}")" = "unparseable" ] ||
	fail "a 20-digit killed must read as unparseable, not $(jq -r '.state' <<<"${omega}")"

# --- pi/gated: a pinned anchor digest ----------------------------------------

pi_window="package pi"$'\x1f'""$'\x1f'"func P() int {"$'\x1f'"return 1"$'\x1f'"}"$'\x1f'
if command -v sha256sum >/dev/null 2>&1; then
	pi_expected_a="$(printf '%s' "${pi_window}" | sha256sum | awk '{print $1}' | cut -c1-12)"
else
	pi_expected_a="$(printf '%s' "${pi_window}" | shasum -a 256 | awk '{print $1}' | cut -c1-12)"
fi
pi="$(jq -c '.packages[] | select(.name == "pi" and .source == "gated")' <<<"${output}")"
pi_a="$(jq -r '.mutants[0].a' <<<"${pi}")"
[ "${pi_a}" = "${pi_expected_a}" ] ||
	fail "pinned anchor digest mismatch: got '${pi_a}', want '${pi_expected_a}'"

# --- rho: ordinals out of input order, and independent per-source numbering -

rho_gated="$(jq -c '.packages[] | select(.name == "rho" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${rho_gated}")" = "measured" ] || fail "rho/gated must be measured"
rho_c5_o="$(jq -r '.mutants[] | select(.c == 5) | .o' <<<"${rho_gated}")"
rho_c12_o="$(jq -r '.mutants[] | select(.c == 12) | .o' <<<"${rho_gated}")"
rho_c20_o="$(jq -r '.mutants[] | select(.c == 20) | .o' <<<"${rho_gated}")"
[ "${rho_c5_o}" = "0" ] ||
	fail "ordinal assignment must follow (line,column) order even fed out of order: column 5 wanted o=0, got '${rho_c5_o}'"
[ "${rho_c12_o}" = "1" ] ||
	fail "ordinal assignment must follow (line,column) order even fed out of order: column 12 wanted o=1, got '${rho_c12_o}'"
[ "${rho_c20_o}" = "2" ] ||
	fail "ordinal assignment must follow (line,column) order even fed out of order: column 20 wanted o=2, got '${rho_c20_o}'"

rho_advisory="$(jq -c '.packages[] | select(.name == "rho" and .source == "advisory")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${rho_advisory}")" = "measured" ] || fail "rho/advisory must be measured"
rho_adv_a="$(jq -r '.mutants[0].a' <<<"${rho_advisory}")"
rho_gated_c5_a="$(jq -r '.mutants[] | select(.c == 5) | .a' <<<"${rho_gated}")"
[ "${rho_adv_a}" = "${rho_gated_c5_a}" ] ||
	fail "the cross-source fixture must actually collide on 'a' to be a meaningful test"
[ "$(jq -r '.mutants[0].o' <<<"${rho_advisory}")" = "0" ] ||
	fail "advisory ordinals must be numbered independently of a gated entry sharing the same (f,m,a)"

# --- digamma/gated, koppa/gated: MUTANT_OK_FILTER rejects glob metacharacters

digamma="$(jq -c '.packages[] | select(.name == "digamma" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${digamma}")" = "unparseable" ] ||
	fail "a file_name carrying a '*' must be unparseable, not $(jq -r '.state' <<<"${digamma}")"

koppa="$(jq -c '.packages[] | select(.name == "koppa" and .source == "gated")' <<<"${output}")"
[ "$(jq -r '.state' <<<"${koppa}")" = "unparseable" ] ||
	fail "a file_name carrying a '?' must be unparseable, not $(jq -r '.state' <<<"${koppa}")"

# --- the run as a whole ------------------------------------------------------

[ "$(jq -r '.complete' <<<"${output}")" = "false" ] ||
	fail "a run with unmeasured/unparseable/missing entries must not be complete"

if [ "${failures}" -ne 0 ]; then
	echo "${failures} mutation survivors record check(s) failed" >&2
	exit 1
fi

echo "Mutation survivors record checks passed."
