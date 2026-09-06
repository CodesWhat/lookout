#!/usr/bin/env bash
set -euo pipefail

fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT

release_version="$(
	grep -E '^## \[v[0-9]+\.[0-9]+\.[0-9]+\] - [0-9]{4}-[0-9]{2}-[0-9]{2}$' CHANGELOG.md |
		sed -n '1{s/^## \[v\([0-9.]*\)\].*/\1/p;}' || true
)"
if [ -z "${release_version}" ]; then
	echo "FAIL: could not derive release version from CHANGELOG.md" >&2
	exit 1
fi

previous_version="$(
	grep -E '^## \[v[0-9]+\.[0-9]+\.[0-9]+\] - [0-9]{4}-[0-9]{2}-[0-9]{2}$' CHANGELOG.md |
		sed -n '2{s/^## \[v\([0-9.]*\)\].*/\1/p;}' || true
)"
if [ -z "${previous_version}" ]; then
	echo "FAIL: could not derive previous version from CHANGELOG.md" >&2
	exit 1
fi

restore_release_workflows() {
	cp .github/workflows/release.yml "${fixture}/.github/workflows/"
	cp .github/workflows/release-cut.yml "${fixture}/.github/workflows/"
}

restore_grype_workflow() {
	cp .github/workflows/security-grype.yml "${fixture}/.github/workflows/"
}

git archive HEAD | tar -x -C "${fixture}"
cp scripts/package-release-config-test.sh "${fixture}/scripts/"
cp scripts/verify-scanner-exclusions.sh "${fixture}/scripts/"
cp .grype.yaml "${fixture}/"
cp api/openapi.yaml "${fixture}/api/"
cp docs/content/docs/api-reference.mdx "${fixture}/docs/content/docs/"
cp docs/content/docs/standalone-mode.mdx "${fixture}/docs/content/docs/"
cp docs/content/docs/observability.mdx "${fixture}/docs/content/docs/"
cp docs/content/docs/security-model.mdx "${fixture}/docs/content/docs/"
mkdir -p "${fixture}/scripts/ci"
cp scripts/ci/go-release-check.sh "${fixture}/scripts/ci/"
restore_release_workflows
restore_grype_workflow
git -C "${fixture}" init -q
git -C "${fixture}" add .
git -C "${fixture}" -c user.email=test@example.com -c user.name=test commit -q -m 'fixture commit'

expect_release_contract_failure() {
	local expected="$1"
	local description="$2"
	local output
	local status

	set +e
	output="$(cd "${fixture}" && bash scripts/package-release-config-test.sh 2>&1)"
	status=$?
	set -e
	if [ "${status}" -eq 0 ] || ! grep -Fq "${expected}" <<<"${output}"; then
		echo "FAIL: ${description}" >&2
		echo "${output}" >&2
		exit 1
	fi
}

# A full checkout with no release tags can only be a broken clone, so the
# guard must fail loudly there instead of passing vacuously. Exercise that
# shape before the fixture is tagged at all.
expect_release_contract_failure \
	"FAIL: no release tags found in a full checkout" \
	"the package release contract must reject a full checkout with no release tags at all"

# A depth-1 clone carries no tags either, and the org's reusable GoReleaser
# gate runs the script from one. That shape must pass with a visible SKIP
# line, not fail every PR and not pass silently.
shallow="$(mktemp -d)"
git clone -q --depth 1 "file://${fixture}" "${shallow}/repo" 2>/dev/null
set +e
shallow_output="$(cd "${shallow}/repo" && bash scripts/package-release-config-test.sh 2>&1)"
shallow_status=$?
set -e
rm -rf "${shallow}"
if [ "${shallow_status}" -ne 0 ] || ! grep -Fq "SKIP: shallow checkout carries no release tags" <<<"${shallow_output}"; then
	echo "FAIL: a shallow checkout must skip the tag-heading check with a SKIP line and pass" >&2
	echo "${shallow_output}" >&2
	exit 1
fi

git -C "${fixture}" tag "v${release_version}"

if ! (cd "${fixture}" && bash scripts/package-release-config-test.sh >/dev/null); then
	echo "FAIL: complete package release fixture must pass" >&2
	exit 1
fi

sed 's/GRYPE_VERSION: "0.118.0"/GRYPE_VERSION: "0.118.1"/' \
	"${fixture}/.github/workflows/release.yml" >"${fixture}/.github/workflows/release.yml.tmp"
mv "${fixture}/.github/workflows/release.yml.tmp" "${fixture}/.github/workflows/release.yml"
expect_release_contract_failure \
	"FAIL: release Grype version must match anchore/scan-action@v7.4.2's Grype v0.118.0" \
	"the package release contract must reject a Grype version that disagrees with scan-action v7.4.2"
restore_release_workflows

sed 's#https://github.com/anchore/grype/.github/workflows/release.yaml@refs/heads/main#https://github.com/anchore/grype/.github/workflows/release.yaml@refs/heads/release#' \
	"${fixture}/.github/workflows/release.yml" >"${fixture}/.github/workflows/release.yml.tmp"
mv "${fixture}/.github/workflows/release.yml.tmp" "${fixture}/.github/workflows/release.yml"
expect_release_contract_failure \
	"FAIL: release Grype checksum verification must use the exact Anchore Grype release workflow identity" \
	"the package release contract must reject a near-miss Grype signing identity"
restore_release_workflows

# The three fixtures below cover the ways a substring check passes while the
# value the tool actually reads is wrong: a stale comment carrying the expected
# text, a duplicate key that YAML resolves to the later value, and an extra
# scanner lane beside two correct ones.
awk '
	!changed && $1 == "GRYPE_VERSION:" {
		print "  # was GRYPE_VERSION: \"0.118.0\""
		print "  GRYPE_VERSION: \"0.110.0\""
		changed = 1
		next
	}
	{ print }
	END { if (!changed) exit 1 }
' "${fixture}/.github/workflows/release.yml" >"${fixture}/.github/workflows/release.yml.tmp"
mv "${fixture}/.github/workflows/release.yml.tmp" "${fixture}/.github/workflows/release.yml"
expect_release_contract_failure \
	"FAIL: release Grype version must match anchore/scan-action@v7.4.2's Grype v0.118.0" \
	"the package release contract must reject a stale comment standing in for the active Grype version"
restore_release_workflows

awk '
	!changed && $1 == "GRYPE_VERSION:" {
		print
		print "  GRYPE_VERSION: \"0.110.0\""
		changed = 1
		next
	}
	{ print }
	END { if (!changed) exit 1 }
' "${fixture}/.github/workflows/release.yml" >"${fixture}/.github/workflows/release.yml.tmp"
mv "${fixture}/.github/workflows/release.yml.tmp" "${fixture}/.github/workflows/release.yml"
expect_release_contract_failure \
	"FAIL: release Grype version must match anchore/scan-action@v7.4.2's Grype v0.118.0" \
	"the package release contract must reject a duplicate GRYPE_VERSION assignment"
restore_release_workflows

awk '
	!changed && $1 == "uses:" && $2 ~ /^anchore\/scan-action@/ {
		print
		indent = $0
		sub(/[^[:space:]].*$/, "", indent)
		print indent "uses: anchore/scan-action@0000000000000000000000000000000000000000  # v7.0.0"
		changed = 1
		next
	}
	{ print }
	END { if (!changed) exit 1 }
' "${fixture}/.github/workflows/security-grype.yml" >"${fixture}/.github/workflows/security-grype.yml.tmp"
mv "${fixture}/.github/workflows/security-grype.yml.tmp" "${fixture}/.github/workflows/security-grype.yml"
expect_release_contract_failure \
	"FAIL: security-grype.yml must use anchore/scan-action v7.4.2 at the reviewed pin in all three scanner lanes" \
	"the package release contract must reject an extra scan-action pinned outside the reviewed version"
restore_grype_workflow

sed '/bash scripts\/package-release-config-test.sh/d' \
	"${fixture}/scripts/ci/go-release-check.sh" >"${fixture}/scripts/ci/go-release-check.sh.tmp"
mv "${fixture}/scripts/ci/go-release-check.sh.tmp" "${fixture}/scripts/ci/go-release-check.sh"
set +e
adapter_output="$(cd "${fixture}" && bash scripts/package-release-config-test.sh 2>&1)"
adapter_status=$?
set -e
if [ "${adapter_status}" -eq 0 ] || ! grep -Fq \
	"FAIL: CI release adapter must enforce the package release contract" <<<"${adapter_output}"; then
	echo "FAIL: package release contract must reject a disconnected CI adapter" >&2
	exit 1
fi
cp scripts/ci/go-release-check.sh "${fixture}/scripts/ci/"

sed 's/workflow-file: ci-verify.yml/workflow-file: other.yml/' \
	"${fixture}/.github/workflows/release.yml" >"${fixture}/.github/workflows/release.yml.tmp"
mv "${fixture}/.github/workflows/release.yml.tmp" "${fixture}/.github/workflows/release.yml"
expect_release_contract_failure \
	"FAIL: the release prerequisite must query ci-verify.yml" \
	"the package release contract must reject a prerequisite that does not verify ci-verify.yml"
restore_release_workflows

awk '
	/target_sha=.*git rev-parse/ && !changed {
		sub(/\^\{commit\}/, "")
		changed = 1
	}
	{ print }
	END { if (!changed) exit 1 }
' "${fixture}/.github/workflows/release.yml" >"${fixture}/.github/workflows/release.yml.tmp"
mv "${fixture}/.github/workflows/release.yml.tmp" "${fixture}/.github/workflows/release.yml"
# shellcheck disable=SC2016 # Matching the validator's literal workflow diagnostic.
expect_release_contract_failure \
	'FAIL: the release prerequisite must resolve the pushed tag to a commit with git rev-parse "${GITHUB_REF}^{commit}"' \
	"the package release contract must reject a prerequisite that does not peel an annotated tag to its commit"
restore_release_workflows

sed '/git merge-base --is-ancestor.*origin\/main/s#origin/main#origin/not-main#' \
	"${fixture}/.github/workflows/release.yml" >"${fixture}/.github/workflows/release.yml.tmp"
mv "${fixture}/.github/workflows/release.yml.tmp" "${fixture}/.github/workflows/release.yml"
expect_release_contract_failure \
	"FAIL: release.yml must have a read-only prerequisite job that proves the tag commit is on origin/main and passed ci-verify.yml" \
	"the package release contract must reject a prerequisite that does not prove the tag commit is on origin/main"
restore_release_workflows

sed 's#target-sha: ${{ steps.target.outputs.sha }}#target-sha: ${{ github.sha }}#' \
	"${fixture}/.github/workflows/release.yml" >"${fixture}/.github/workflows/release.yml.tmp"
mv "${fixture}/.github/workflows/release.yml.tmp" "${fixture}/.github/workflows/release.yml"
expect_release_contract_failure \
	"FAIL: the release prerequisite must query CI for the exact commit resolved from the pushed tag" \
	"the package release contract must reject a CI query that bypasses the peeled tag output"
restore_release_workflows

sed 's/actions: read/actions: write/' \
	"${fixture}/.github/workflows/release.yml" >"${fixture}/.github/workflows/release.yml.tmp"
mv "${fixture}/.github/workflows/release.yml.tmp" "${fixture}/.github/workflows/release.yml"
expect_release_contract_failure \
	"FAIL: the release prerequisite must be unprivileged" \
	"the package release contract must reject write access in the release prerequisite"
restore_release_workflows

awk '
	$0 == "  release:" { in_release = 1 }
	in_release && /^    needs:/ && !removed { removed = 1; next }
	in_release && $0 != "  release:" && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { in_release = 0 }
	{ print }
	END { if (!removed) exit 1 }
' "${fixture}/.github/workflows/release.yml" >"${fixture}/.github/workflows/release.yml.tmp"
mv "${fixture}/.github/workflows/release.yml.tmp" "${fixture}/.github/workflows/release.yml"
expect_release_contract_failure \
	"FAIL: the privileged release job must depend on the read-only release prerequisite" \
	"the package release contract must reject a publish job disconnected from the prerequisite"
restore_release_workflows

sed 's/environment: Production/environment: Preview/' \
	"${fixture}/.github/workflows/release.yml" >"${fixture}/.github/workflows/release.yml.tmp"
mv "${fixture}/.github/workflows/release.yml.tmp" "${fixture}/.github/workflows/release.yml"
expect_release_contract_failure \
	"FAIL: the privileged release job must use the Production environment" \
	"the package release contract must reject a publish job outside the Production environment"
restore_release_workflows

sed '/git merge-base --is-ancestor.*origin\/main/s#origin/main#origin/not-main#' \
	"${fixture}/.github/workflows/release-cut.yml" >"${fixture}/.github/workflows/release-cut.yml.tmp"
mv "${fixture}/.github/workflows/release-cut.yml.tmp" "${fixture}/.github/workflows/release-cut.yml"
expect_release_contract_failure \
	"FAIL: release-cut.yml must prove TARGET_SHA is on origin/main before creating the release tag" \
	"the package release contract must reject a release cut that can tag a commit outside origin/main"
restore_release_workflows

for advisory_id in GHSA-pxq6-2prw-chj9 GO-2026-4883 GHSA-x744-4wpc-v9h2 GO-2026-4887; do
	awk -v advisory_id="${advisory_id}" '
		$1 == "-" && $2 == "vulnerability:" { skip = ($3 == advisory_id) }
		!skip { print }
	' "${fixture}/.grype.yaml" >"${fixture}/.grype.yaml.tmp"
	mv "${fixture}/.grype.yaml.tmp" "${fixture}/.grype.yaml"
	expect_release_contract_failure \
		"FAIL: .grype.yaml must contain exactly one ${advisory_id} ignore scoped to github.com/docker/docker v28.5.2+incompatible at **/usr/bin/docker-compose" \
		"the package release contract must reject removal of ${advisory_id} from the scoped Docker Compose advisory aliases"
	cp .grype.yaml "${fixture}/"
done

while IFS='|' read -r scope_field replacement; do
	awk -v scope_field="${scope_field}:" -v replacement="${replacement}" '
		$1 == "-" && $2 == "vulnerability:" { target = ($3 == "GO-2026-4887") }
		target && $1 == scope_field && !changed {
			print "      " scope_field " " replacement
			changed = 1
			next
		}
		{ print }
		END { if (!changed) exit 1 }
	' "${fixture}/.grype.yaml" >"${fixture}/.grype.yaml.tmp"
	mv "${fixture}/.grype.yaml.tmp" "${fixture}/.grype.yaml"
	expect_release_contract_failure \
		"FAIL: .grype.yaml must contain exactly one GO-2026-4887 ignore scoped to github.com/docker/docker v28.5.2+incompatible at **/usr/bin/docker-compose" \
		"the package release contract must reject changing the GO-2026-4887 ${scope_field} scope"
	cp .grype.yaml "${fixture}/"
done <<'EOF'
name|github.com/docker/other
version|v28.5.3+incompatible
type|unknown
location|"**/usr/bin/*"
EOF

awk '
	$1 == "-" && $2 == "vulnerability:" { skip = ($3 == "GHSA-vp52-pcj8-j9qc") }
	!skip { print }
' "${fixture}/.grype.yaml" >"${fixture}/.grype.yaml.tmp"
mv "${fixture}/.grype.yaml.tmp" "${fixture}/.grype.yaml"
expect_release_contract_failure \
	"FAIL: .grype.yaml must contain exactly one GHSA-vp52-pcj8-j9qc ignore scoped to google.golang.org/grpc v1.83.0 at **/usr/bin/docker-compose" \
	"the package release contract must reject removal of the scoped Compose grpc suppression"
cp .grype.yaml "${fixture}/"

while IFS='|' read -r scope_field replacement; do
	awk -v scope_field="${scope_field}:" -v replacement="${replacement}" '
		$1 == "-" && $2 == "vulnerability:" { target = ($3 == "GHSA-vp52-pcj8-j9qc") }
		target && $1 == scope_field && !changed {
			print "      " scope_field " " replacement
			changed = 1
			next
		}
		{ print }
		END { if (!changed) exit 1 }
	' "${fixture}/.grype.yaml" >"${fixture}/.grype.yaml.tmp"
	mv "${fixture}/.grype.yaml.tmp" "${fixture}/.grype.yaml"
	expect_release_contract_failure \
		"FAIL: .grype.yaml must contain exactly one GHSA-vp52-pcj8-j9qc ignore scoped to google.golang.org/grpc v1.83.0 at **/usr/bin/docker-compose" \
		"the package release contract must reject changing the GHSA-vp52-pcj8-j9qc ${scope_field} scope"
	cp .grype.yaml "${fixture}/"
done <<'EOF'
name|google.golang.org/other
version|v1.83.1
type|unknown
location|"**/usr/bin/*"
EOF

# A scope field commented out (rather than changed) removes it from grype's
# and YAML's view of the entry just as effectively as deleting it, so the
# guard has to reject that too, not just a substring match on the captured
# block.
while IFS= read -r scope_field; do
	awk -v scope_field="${scope_field}:" '
		$1 == "-" && $2 == "vulnerability:" { target = ($3 == "GHSA-vp52-pcj8-j9qc") }
		target && $1 == scope_field && !changed {
			print "#" $0
			changed = 1
			next
		}
		{ print }
		END { if (!changed) exit 1 }
	' "${fixture}/.grype.yaml" >"${fixture}/.grype.yaml.tmp"
	mv "${fixture}/.grype.yaml.tmp" "${fixture}/.grype.yaml"
	expect_release_contract_failure \
		"FAIL: .grype.yaml must contain exactly one GHSA-vp52-pcj8-j9qc ignore scoped to google.golang.org/grpc v1.83.0 at **/usr/bin/docker-compose" \
		"the package release contract must reject commenting out the GHSA-vp52-pcj8-j9qc ${scope_field} scope"
	cp .grype.yaml "${fixture}/"
done <<'EOF'
name
version
type
location
EOF

prefix_version="${release_version}0"
readme_prefix_backup="${fixture}/README.md.prefix-version-backup"
cp "${fixture}/README.md" "${readme_prefix_backup}"

awk -v from="VERSION=${release_version}" -v to="VERSION=${prefix_version}" '
	!changed && index($0, from) {
		sub(from, to)
		changed = 1
	}
	{ print }
	END { if (!changed) exit 1 }
' "${fixture}/README.md" >"${fixture}/README.md.tmp"
mv "${fixture}/README.md.tmp" "${fixture}/README.md"

set +e
validator_output="$(cd "${fixture}" && bash scripts/package-release-config-test.sh 2>&1)"
validator_status=$?
set -e
expected_diagnostic="FAIL: README release commands and asset names must use ${release_version}:"

if [ "${validator_status}" -eq 0 ]; then
	echo "FAIL: prefix version ${prefix_version} must not match current version ${release_version}" >&2
	exit 1
fi
if ! grep -Fq "${expected_diagnostic}" <<<"${validator_output}"; then
	echo "FAIL: prefix-version fixture failed without the expected stale README diagnostic" >&2
	echo "${validator_output}" >&2
	exit 1
fi
mv "${readme_prefix_backup}" "${fixture}/README.md"

expect_stale_release_example_failure() {
	local file="$1"
	local injected_line="$2"
	local backup="${fixture}/${file}.release-example-backup"

	cp "${fixture}/${file}" "${backup}"
	printf '%s\n' "${injected_line}" >>"${fixture}/${file}"
	expect_release_contract_failure \
		"FAIL: active release examples must use ${release_version}" \
		"the package release contract must reject a stale release example in ${file}"
	mv "${backup}" "${fixture}/${file}"
}

expect_stale_release_example_failure "api/openapi.yaml" $'        agentVersion:\n          example: "0.0.1"'
expect_stale_release_example_failure "api/openapi.yaml" '        data: {"type":"dd:ack","data":{"version":"0.0.1"}}'
expect_stale_release_example_failure "docs/content/docs/api-reference.mdx" '{"version":"0.0.1"}'
expect_stale_release_example_failure "docs/content/docs/standalone-mode.mdx" '{"agentVersion":"0.0.1"}'
expect_stale_release_example_failure "docs/content/docs/observability.mdx" 'portwing_build_info{version="0.0.1"} 1'
expect_stale_release_example_failure "docs/content/docs/security-model.mdx" 'VERSION=0.0.1'

gitleaksignore_backup="${fixture}/.gitleaksignore.backup"
cp "${fixture}/.gitleaksignore" "${gitleaksignore_backup}"
printf '%s\n' "website/src/components/cli-demo.tsx:private-key:508" >>"${fixture}/.gitleaksignore"
expect_release_contract_failure \
	"FAIL: .gitleaksignore entries must be commit-pinned" \
	"the package release contract must reject a line-pinned .gitleaksignore working-tree fingerprint"
mv "${gitleaksignore_backup}" "${fixture}/.gitleaksignore"

# The stale-previous-version exemption is scoped to README.md's "since
# v<previous>" release note only. A plain previous-version literal anywhere
# else - including another Markdown file - must still fail.
releasing_stale_backup="${fixture}/RELEASING.md.stale-version-backup"
cp "${fixture}/RELEASING.md" "${releasing_stale_backup}"
printf '\nStale reference to v%s.\n' "${previous_version}" >>"${fixture}/RELEASING.md"
expect_release_contract_failure \
	"FAIL: stale references to v${previous_version} remain outside the changelog" \
	"the package release contract must reject a plain stale previous-version literal in a non-README markdown file"
mv "${releasing_stale_backup}" "${fixture}/RELEASING.md"

# The exact "since v<previous>" phrase in README.md is the one exempted
# shape; it must not fail on its own.
readme_since_backup="${fixture}/README.md.since-phrase-backup"
cp "${fixture}/README.md" "${readme_since_backup}"
printf '\nNo changes since v%s.\n' "${previous_version}" >>"${fixture}/README.md"
if ! (cd "${fixture}" && bash scripts/package-release-config-test.sh >/dev/null); then
	echo "FAIL: a 'since v<previous>' phrase in README.md must not fail the stale-version check" >&2
	exit 1
fi
mv "${readme_since_backup}" "${fixture}/README.md"

# The same "since v<previous>" phrase outside README.md is not exempt; the
# exemption is scoped by file, not just by phrase.
releasing_since_backup="${fixture}/RELEASING.md.since-phrase-backup"
cp "${fixture}/RELEASING.md" "${releasing_since_backup}"
printf '\nNo changes since v%s.\n' "${previous_version}" >>"${fixture}/RELEASING.md"
expect_release_contract_failure \
	"FAIL: stale references to v${previous_version} remain outside the changelog" \
	"the package release contract must reject the 'since v<previous>' phrase outside README.md"
mv "${releasing_since_backup}" "${fixture}/RELEASING.md"

# The CHANGELOG-vs-tag guard only sees tags the checkout actually has, so
# exercising it needs a real tag in the fixture repo. It guards against
# portwing#283: a prep PR renamed the v0.9.12 heading to v0.9.13 instead of
# inserting a fresh section, leaving the already-tagged v0.9.12 release with
# no matching heading. "v${release_version}" stays tagged for the whole
# fixture lifetime and always has a matching heading, so adding a second tag
# with no heading is what isolates the failure below.
git -C "${fixture}" tag v0.0.1
expect_release_contract_failure \
	"FAIL: CHANGELOG.md has no '## [vX.Y.Z] - YYYY-MM-DD' heading for tag(s): v0.0.1" \
	"the package release contract must reject a git tag with no matching CHANGELOG.md heading"
git -C "${fixture}" tag -d v0.0.1 >/dev/null

# A heading-shaped substring that is not anchored at the start of the line -
# prose referencing the old heading text, or the same text sitting inside a
# code fence - must not satisfy the guard either.
changelog_decoy_backup="${fixture}/CHANGELOG.md.decoy-backup"
cp "${fixture}/CHANGELOG.md" "${changelog_decoy_backup}"
# shellcheck disable=SC2016 # The backticks are a literal Markdown code span, not command substitution.
printf '%s\n' 'not a real heading: `## [v0.0.2] - 2099-01-01`' >>"${fixture}/CHANGELOG.md"
git -C "${fixture}" tag v0.0.2
expect_release_contract_failure \
	"FAIL: CHANGELOG.md has no '## [vX.Y.Z] - YYYY-MM-DD' heading for tag(s): v0.0.2" \
	"the package release contract must reject a heading-shaped substring that is not anchored at the start of the line"
git -C "${fixture}" tag -d v0.0.2 >/dev/null
mv "${changelog_decoy_backup}" "${fixture}/CHANGELOG.md"

echo "Package release contract self-tests passed."
