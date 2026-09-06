# Merge N `go test -covermode=set -coverprofile=...` profiles into one
# statement-coverage percentage.
#
# Called two ways from this repository:
#
#   * scripts/ci/fuzz-score.sh, with exactly one profile, to score a single
#     fuzz target's corpus replay;
#   * quality-fuzz-nightly.yml's `history` job, with every target's profile
#     from the run, to compute the union row's `union_coverage_pct` across
#     the whole matrix.
#
# Every non-header line is `block numstmts count`, where `block` is
# `file:startline.col,endline.col`. The same block can appear in more than
# one input file only when two fuzz targets share a package (this repo has
# one such pair: FuzzParsePHC and FuzzParseTrustedProxies, both in
# ./internal/server/), and a block covered by either target's replay is
# covered — so the merge keys on the block spec and keeps the max count seen
# for it, never a sum and never last-write-wins. numstmts for a given block
# is identical everywhere it appears (the compiler emits the same block
# table regardless of which binary is running it), so the first sighting is
# kept rather than re-summed.
#
# Usage: awk -f fuzz-coverprofile-union.awk profile1.out [profile2.out ...]
# Output: one line, "<pct> <covered-statements> <total-statements>"

$1 == "mode:" {
	next
}

{
	block = $1
	stmts = $2 + 0
	count = $3 + 0
	if (!(block in seen)) {
		seen[block] = 1
		stmts_by_block[block] = stmts
		count_by_block[block] = count
	} else if (count > count_by_block[block]) {
		count_by_block[block] = count
	}
}

END {
	total = 0
	covered = 0
	for (block in seen) {
		total += stmts_by_block[block]
		if (count_by_block[block] > 0) {
			covered += stmts_by_block[block]
		}
		# Test-only introspection: the percentage on stdout below is >0-vs-0
		# per block, so it cannot tell a max-merge from a sum-merge when a
		# block is covered by every input (3 and 4 both read as "covered").
		# scripts/fuzz-score-script-test.sh sets this to read the actual
		# merged count back off stderr and prove it is the max, not a sum.
		# Never touches stdout, so normal callers (fuzz-score.sh, the nightly
		# history job) are unaffected either way.
		if (ENVIRON["FUZZ_COVER_UNION_DEBUG"] == "1") {
			printf "%s %d %d\n", block, stmts_by_block[block], count_by_block[block] > "/dev/stderr"
		}
	}
	pct = 0
	if (total > 0) {
		pct = covered / total * 100
	}
	printf "%.2f %d %d\n", pct, covered, total
}
