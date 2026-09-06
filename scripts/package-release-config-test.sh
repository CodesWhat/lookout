#!/usr/bin/env bash
set -euo pipefail

failures=0

require_text() {
	local file="$1"
	local text="$2"
	local description="$3"

	if ! grep -Fq -- "$text" "$file"; then
		echo "FAIL: ${description} (${file} must contain: ${text})" >&2
		failures=$((failures + 1))
	fi
}

# grep -Fq matches anywhere in the file, comments included, which is a false
# pass waiting to happen for a pinned release value: a stale
# `# was GRYPE_VERSION: "0.118.0"` line satisfies it while the active
# assignment says something else, and so does a second assignment that YAML
# would actually be the one to use. Match active lines only and require exactly
# one, so a stale comment and a duplicate key both fail here.
active_lines() {
	grep -vE '^[[:space:]]*#' "$1"
}

require_exactly_one_active() {
	local file="$1"
	local pattern="$2"
	local text="$3"
	local description="$4"
	local total
	local matching

	total="$(active_lines "$file" | grep -cE -- "$pattern" || true)"
	matching="$(active_lines "$file" | grep -cF -- "$text" || true)"
	if [ "${total}" -ne 1 ] || [ "${matching}" -ne 1 ]; then
		echo "FAIL: ${description} (${file} must contain exactly one active: ${text})" >&2
		failures=$((failures + 1))
	fi
}

require_file() {
	local file="$1"
	local description="$2"

	if [ ! -f "$file" ]; then
		echo "FAIL: ${description} (${file} is missing)" >&2
		failures=$((failures + 1))
	fi
}

require_first_line() {
	local file="$1"
	local expected="$2"
	local description="$3"
	local actual

	actual="$(sed -n '1p' "$file")"
	if [ "$actual" != "$expected" ]; then
		echo "FAIL: ${description} (${file} first line must be: ${expected})" >&2
		failures=$((failures + 1))
	fi
}

require_last_line() {
	local file="$1"
	local expected="$2"
	local description="$3"
	local actual

	actual="$(tail -n 1 "$file")"
	if [ "$actual" != "$expected" ]; then
		echo "FAIL: ${description} (${file} last line must be: ${expected}, got: ${actual})" >&2
		failures=$((failures + 1))
	fi
}

require_sha256() {
	local file="$1"
	local expected="$2"
	local description="$3"
	local actual

	actual="$(shasum -a 256 "$file" | awk '{print $1}')"
	if [ "$actual" != "$expected" ]; then
		echo "FAIL: ${description} (${file} SHA-256 must be ${expected}, got ${actual})" >&2
		failures=$((failures + 1))
	fi
}

reject_text() {
	local file="$1"
	local text="$2"
	local description="$3"

	if grep -Fq -- "$text" "$file"; then
		echo "FAIL: ${description} (${file} must not contain: ${text})" >&2
		failures=$((failures + 1))
	fi
}

# Case-insensitive variant, for rejecting hostnames. Hostnames resolve the same
# in any case, so a case-sensitive reject is trivially defeated by an embed
# written as WARPCHART.DEV, which makes the identical third-party request.
reject_text_ci() {
	local file="$1"
	local text="$2"
	local description="$3"

	if grep -Fiq -- "$text" "$file"; then
		echo "FAIL: ${description} (${file} must not contain, in any case: ${text})" >&2
		failures=$((failures + 1))
	fi
}

validate_current_release_example_values() {
	local file="$1"
	local matches="$2"
	local description="$3"
	local stale_matches

	if [ -z "${matches}" ]; then
		echo "FAIL: ${description} (${file} has no recognized active release example)" >&2
		failures=$((failures + 1))
		return
	fi

	stale_matches="$(printf '%s\n' "${matches}" |
		grep -Eo '[0-9]+\.[0-9]+\.[0-9]+' |
		grep -Fvx -- "${release_version}" || true)"
	if [ -n "${stale_matches}" ]; then
		echo "FAIL: active release examples must use ${release_version} (${file} contains:)" >&2
		echo "${stale_matches}" >&2
		failures=$((failures + 1))
	fi
}

require_current_release_examples() {
	local file="$1"
	local pattern="$2"
	local description="$3"
	local matches

	matches="$(grep -Eo -- "${pattern}" "${file}" || true)"
	validate_current_release_example_values "${file}" "${matches}" "${description}"
}

workflow_job_block() {
	local file="$1"
	local job="$2"

	awk -v job="${job}" '
		$0 == "  " job ":" { in_job = 1 }
		in_job && $0 != "  " job ":" && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { exit }
		in_job { print }
	' "${file}"
}

require_block_text() {
	local block="$1"
	local text="$2"
	local description="$3"

	if ! grep -Fq -- "$text" <<<"${block}"; then
		echo "FAIL: ${description} (workflow job must contain: ${text})" >&2
		failures=$((failures + 1))
	fi
}

require_exactly_one_active ".goreleaser.yml" \
	'^[[:space:]]*mod_timestamp:' \
	'mod_timestamp: "{{ .CommitTimestamp }}"' \
	"release binaries must be byte-reproducible (mod_timestamp pinned to the commit)"

# The binary's mtime being pinned doesn't make the archive reproducible on its
# own: GoReleaser auto-includes LICENSE*/README*/CHANGELOG* with the source
# file's own filesystem mtime whenever `files:` is unset. An explicit `files:`
# entry per glob, each carrying its own commit-pinned mtime, is what actually
# closes that gap — so assert each entry individually (require_exactly_one_active
# so a commented-out or duplicated entry still fails) and assert the mtime
# override count separately, since the same "{{ .CommitDate }}" line repeats
# once per entry and require_exactly_one_active can't express "exactly three".
require_exactly_one_active ".goreleaser.yml" \
	'^[[:space:]]*- src: LICENSE\*$' \
	'- src: LICENSE*' \
	"release archives must include LICENSE* with a pinned mtime"
require_exactly_one_active ".goreleaser.yml" \
	'^[[:space:]]*- src: README\*$' \
	'- src: README*' \
	"release archives must include README* with a pinned mtime"
require_exactly_one_active ".goreleaser.yml" \
	'^[[:space:]]*- src: CHANGELOG\*$' \
	'- src: CHANGELOG*' \
	"release archives must include CHANGELOG* with a pinned mtime"
archive_mtime_overrides="$(active_lines ".goreleaser.yml" | grep -cF 'mtime: "{{ .CommitDate }}"' || true)"
if [ "${archive_mtime_overrides}" -ne 3 ]; then
	echo "FAIL: release archives must pin all three LICENSE*/README*/CHANGELOG* entries' mtime to the commit date (.goreleaser.yml has ${archive_mtime_overrides} active mtime: \"{{ .CommitDate }}\" line(s), want 3)" >&2
	failures=$((failures + 1))
fi
require_text ".goreleaser.yml" "nfpms:" "GoReleaser must define native Linux packages"
require_text ".goreleaser.yml" "formats: [deb, rpm]" "GoReleaser must build deb and rpm packages"
require_text ".goreleaser.yml" "src: scripts/portwing.service" "native packages must include the systemd unit"
require_text ".goreleaser.yml" "dst: /usr/lib/systemd/system/portwing.service" "the systemd unit must use the portable package path"
require_text ".goreleaser.yml" "homebrew_casks:" "GoReleaser must publish a Homebrew cask"
require_text ".goreleaser.yml" "skip_upload: auto" "prereleases must not update the stable Homebrew channel"
require_text ".goreleaser.yml" 'token: "{{ .Env.HOMEBREW_TAP_TOKEN }}"' "Homebrew publishing must use the dedicated tap token"
public_site="https://portwing.codeswhat.com"
protected_site="https://getportwing-codeswhat.vercel.app"

toolchain_version="$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2 }' go.mod)"
if ! grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' <<<"${toolchain_version}"; then
	echo "FAIL: go.mod must pin an exact Go toolchain patch version" >&2
	failures=$((failures + 1))
	toolchain_version="unresolved"
fi

builder_ref=""
for dockerfile in Dockerfile Dockerfile.armv7 Dockerfile.dev; do
	current_ref="$(grep -E '^FROM golang:' "${dockerfile}" | sed -n '1p' || true)"
	if ! grep -Eq "^FROM golang:${toolchain_version//./\\.}-alpine@sha256:[0-9a-f]{64}([[:space:]]+AS[[:space:]]+builder)?$" <<<"${current_ref}"; then
		echo "FAIL: ${dockerfile} must use the exact go.mod toolchain in a digest-pinned Alpine builder" >&2
		failures=$((failures + 1))
	fi
	if [ -z "${builder_ref}" ]; then
		builder_ref="${current_ref%% AS builder}"
	elif [ "${current_ref%% AS builder}" != "${builder_ref}" ]; then
		echo "FAIL: all from-source Dockerfiles must use the same Go builder reference" >&2
		failures=$((failures + 1))
	fi
done

# release_version, release_date, and previous_version come from CHANGELOG.md
# rather than being hand-set here. A hand-set constant can only ever agree
# with the docs it was written to expect, not with the version actually
# being released — that's exactly how this contract sat pinned at v0.9.2
# through two release cuts and stayed green while the docs it exists to
# police went stale. CHANGELOG.md's newest dated "## [vX.Y.Z] - YYYY-MM-DD"
# heading is the same value release-cut.yml already requires to exist
# before it will push a tag ("Validate CHANGELOG entry for release tag"),
# so sourcing it from there too leaves no separate constant to forget.
changelog_heading_regex='^## \[v[0-9]+\.[0-9]+\.[0-9]+\] - [0-9]{4}-[0-9]{2}-[0-9]{2}$'
release_heading="$(grep -E "${changelog_heading_regex}" CHANGELOG.md | sed -n '1p' || true)"
previous_heading="$(grep -E "${changelog_heading_regex}" CHANGELOG.md | sed -n '2p' || true)"
release_version="$(printf '%s\n' "${release_heading}" | sed -E 's/^## \[v([0-9.]+)\].*/\1/')"
release_date="$(printf '%s\n' "${release_heading}" | sed -E 's/.*- ([0-9-]+)$/\1/')"
previous_version="$(printf '%s\n' "${previous_heading}" | sed -E 's/^## \[v([0-9.]+)\].*/\1/')"

if ! echo "${release_version}" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' ||
	! echo "${release_date}" | grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2}$' ||
	! echo "${previous_version}" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
	echo "FAIL: could not derive release_version, release_date, and previous_version from CHANGELOG.md's two newest dated '## [vX.Y.Z] - YYYY-MM-DD' headings" >&2
	failures=$((failures + 1))
	release_version="unresolved"
	release_date="unresolved"
	previous_version="unresolved"
fi

# A prep PR can rename an already-released heading instead of inserting a
# fresh one above it (portwing#283: v0.9.12's heading was edited in place to
# read v0.9.13, so every v0.9.12 entry now reads as if it shipped in
# v0.9.13). A missing release_version/previous_version pair above cannot
# catch that, because the rename still leaves exactly two dated headings in
# place. Check every locally known "vX.Y.Z" tag instead: each one names a
# release that already happened, so CHANGELOG.md must still carry that
# heading. This only sees tags the checkout actually has, which is why it
# runs locally on pre-push as well as in CI. The Release Contract job in
# ci-verify.yml and release-cut.yml check out with fetch-depth: 0 so the tags
# come along, but the org's reusable GoReleaser gate runs this script from a
# depth-1 clone that carries no tags at all. A shallow checkout therefore
# says so and skips this one check, loudly, instead of passing vacuously or
# failing every PR; a full checkout with no release tags is a hard failure,
# because that shape can only be a broken clone. Releases
# through v0.6.0 headed the section without a "v" prefix ("## [0.6.0]");
# accept either form so this doesn't flag that older, still-correct
# convention. The match is anchored to the start of the line and requires a
# trailing "YYYY-MM-DD" date, so a heading-shaped substring inside prose or a
# code fence (e.g. "see the old \`## [v0.6.0] -\` format") cannot stand in for
# a real heading.
if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	release_tags="$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true)"
	if [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" = "true" ] && [ -z "${release_tags}" ]; then
		echo "SKIP: shallow checkout carries no release tags, so the tag-heading check did not run here; it runs in Release Contract, release-cut, and pre-push" >&2
	elif [ -z "${release_tags}" ]; then
		echo "FAIL: no release tags found in a full checkout" >&2
		failures=$((failures + 1))
	else
		missing_tag_headings=""
		for release_tag in ${release_tags}; do
			unprefixed_tag="${release_tag#v}"
			release_tag_regex="${release_tag//./\\.}"
			unprefixed_tag_regex="${unprefixed_tag//./\\.}"
			if ! grep -Eq "^## \[${release_tag_regex}\] - [0-9]{4}-[0-9]{2}-[0-9]{2}\$" CHANGELOG.md &&
				! grep -Eq "^## \[${unprefixed_tag_regex}\] - [0-9]{4}-[0-9]{2}-[0-9]{2}\$" CHANGELOG.md; then
				missing_tag_headings="${missing_tag_headings} ${release_tag}"
			fi
		done
		if [ -n "${missing_tag_headings}" ]; then
			echo "FAIL: CHANGELOG.md has no '## [vX.Y.Z] - YYYY-MM-DD' heading for tag(s):${missing_tag_headings} (a prep PR likely renamed an existing heading instead of inserting a new one; restore the missing section(s) with their original entries)" >&2
			failures=$((failures + 1))
		fi
	fi
fi

require_text ".goreleaser.yml" "${public_site}/" "published package metadata must use the public website"
require_text "README.md" "${public_site}/docs/installation" "the repository landing page must link the public package guide"
require_text "docs/src/lib/site-config.ts" 'domain: "portwing.codeswhat.com"' "documentation metadata must use the public website"
require_text "website/src/lib/site-config.ts" 'domain: "portwing.codeswhat.com"' "website metadata must use the public website"
require_text "website/public/llms.txt" "Website: ${public_site}" "agent discovery metadata must use the public website"
require_text "README.md" "currently \`v${release_version}\`" "the repository landing page must identify the current release"
release_version_regex="${release_version//./\\.}"
stale_readme_examples="$(grep -Eo '(VERSION=|portwing_)[0-9]+\.[0-9]+\.[0-9]+' README.md |
	grep -Ev "^(VERSION=|portwing_)${release_version_regex}$" || true)"
if [ -n "${stale_readme_examples}" ]; then
	echo "FAIL: README release commands and asset names must use ${release_version}:" >&2
	echo "${stale_readme_examples}" >&2
	failures=$((failures + 1))
fi
require_text "website/src/lib/site-config.ts" "version: \"${release_version}\"" "website metadata must identify the current release"
require_text "website/src/components/get-started.tsx" "portwing_${release_version}_linux_amd64.deb" "website package examples must use the current release"
require_text "docs/content/docs/installation.mdx" "VERSION=${release_version}" "installation examples must use the current release"
require_text "docs/content/docs/verification.mdx" "TAG=${release_version}" "verification examples must use the current release"
require_text "website/public/llms.txt" "Portwing v${release_version} is" "agent discovery metadata must identify the current release"
require_text "ROADMAP.md" "currently \`v${release_version}\`" "the roadmap must identify the current release"
require_text "COMPATIBILITY.md" "v${release_version} (latest release) / \`main\`" "the compatibility matrix must identify the current release"
require_text "api/openapi.yaml" "  version: ${release_version}" "the OpenAPI contract must identify the current release"
require_text "examples/observability/docker-compose.yml" "ghcr.io/codeswhat/portwing:${release_version}" "the observability example must pin the current release"
openapi_agent_version_examples="$(awk '
	/^        (version|agentVersion):[[:space:]]*$/ { in_agent_version = 1; next }
	in_agent_version && /^[[:space:]]+example: "[0-9]+\.[0-9]+\.[0-9]+"$/ {
		print
		in_agent_version = 0
		next
	}
	in_agent_version && /^        [A-Za-z][A-Za-z0-9]*:/ { in_agent_version = 0 }
' api/openapi.yaml)"
validate_current_release_example_values "api/openapi.yaml" \
	"${openapi_agent_version_examples}" \
	"OpenAPI agent-version examples must identify the current release"
require_current_release_examples "api/openapi.yaml" \
	'"type":"dd:ack".*"version":"[0-9]+\.[0-9]+\.[0-9]+"' \
	"OpenAPI Drydock handshake examples must identify the current release"
require_current_release_examples "docs/content/docs/api-reference.mdx" \
	'"(agentVersion|version)"[[:space:]]*:[[:space:]]*"[0-9]+\.[0-9]+\.[0-9]+"' \
	"API reference response examples must identify the current release"
require_current_release_examples "docs/content/docs/standalone-mode.mdx" \
	'"(agentVersion|version)"[[:space:]]*:[[:space:]]*"[0-9]+\.[0-9]+\.[0-9]+"' \
	"standalone-mode response examples must identify the current release"
require_current_release_examples "docs/content/docs/observability.mdx" \
	'"version"[[:space:]]*:[[:space:]]*"[0-9]+\.[0-9]+\.[0-9]+"' \
	"observability response examples must identify the current release"
require_current_release_examples "docs/content/docs/observability.mdx" \
	'portwing_build_info\{version="[0-9]+\.[0-9]+\.[0-9]+"\}' \
	"observability build metadata examples must identify the current release"
require_current_release_examples "docs/content/docs/security-model.mdx" \
	'^VERSION=[0-9]+\.[0-9]+\.[0-9]+$' \
	"security verification examples must identify the current release"

# The checks above assert the new version is present on each surface they name.
# This asserts the previous one is gone everywhere else, which is what actually
# catches a half-finished bump: an rpm example sitting next to a checked deb
# example, a sample JSON payload, an attestation command in a doc. Enumerating
# surfaces only ever finds the surfaces someone remembered to enumerate.
# A "since v<previous>" callout in README.md is the one deliberate exception:
# a docs-only patch's release note (README's [!NOTE] block, v0.9.13) names the
# prior release on purpose to say nothing binary changed since it, which is
# not a forgotten bump. The exemption is scoped to that one file and that
# exact phrase, so the same stale literal sitting in any other matched file -
# an rpm example, a doc's attestation command - still fails; a broader
# file-wide exemption would let a stale version hide behind an unrelated
# "since v<previous>" line anywhere in the repo.
stale_previous_version_matches="$(git grep -n -F -- "$previous_version" -- \
	'*.md' '*.mdx' '*.ts' '*.tsx' '*.yaml' '*.yml' '*.txt' \
	':(exclude)CHANGELOG.md' \
	':(exclude)scripts/package-release-config-test.sh' || true)"
if [ -n "${stale_previous_version_matches}" ]; then
	stale_previous_version_matches="$(awk -v exempt="since v${previous_version}" '
		index($0, "README.md:") == 1 && index($0, exempt) { next }
		{ print }
	' <<<"${stale_previous_version_matches}")"
fi
if [ -n "${stale_previous_version_matches}" ]; then
	echo "FAIL: stale references to v${previous_version} remain outside the changelog" >&2
	echo "${stale_previous_version_matches}" >&2
	failures=$((failures + 1))
fi

if git grep -F -- "$protected_site" -- . \
	':(exclude)CHANGELOG.md' \
	':(exclude)security_best_practices_report.md' \
	':(exclude)scripts/package-release-config-test.sh'; then
	echo "FAIL: active public surfaces must not link to Vercel's protected team alias" >&2
	failures=$((failures + 1))
fi

# shellcheck disable=SC2016 # GitHub evaluates these literals in release.yml.
require_text ".github/workflows/release.yml" 'HOMEBREW_TAP_TOKEN: ${{ secrets.HOMEBREW_TAP_TOKEN }}' "the release must pass the tap token to GoReleaser"
require_text ".github/workflows/release.yml" "Verify deb package install" "the release must install-test the deb package"
require_text ".github/workflows/release.yml" "Verify rpm package install" "the release must install-test the rpm package"
require_text ".github/workflows/release.yml" "Verify Homebrew cask install" "the release must install-test the published Homebrew cask"
require_text ".github/workflows/release.yml" "ubuntu:24.04@sha256:" "the deb smoke image must be digest-pinned"
require_text ".github/workflows/release.yml" "fedora:42@sha256:" "the rpm smoke image must be digest-pinned"
# shellcheck disable=SC2016 # The workflow expands GITHUB_REF_NAME.
require_text ".github/workflows/release.yml" 'release.yml@refs/tags/${GITHUB_REF_NAME}' "release verification must bind the signer identity to the exact release tag"
reject_text ".github/workflows/release.yml" "certificate-identity-regexp" "release verification must not accept an unanchored signer identity"

expected_scan_action_ref="anchore/scan-action@27805bf3b4e84b4a5c980df22ed233c00390a439"
scan_action_count="$(awk -v expected="${expected_scan_action_ref}" \
	'$1 == "uses:" && $2 == expected && $3 == "#" && $4 == "v7.4.2" { count++ } END { print count + 0 }' \
	.github/workflows/security-grype.yml)"
# Counting only the lines that match would still pass with a fourth scanner lane
# pinned to an older scan-action sitting beside the three correct ones, which is
# the shape a partial bump actually takes. Count every scan-action use as well,
# so an added lane has to be at the reviewed pin or fail here.
#
# Three lanes since the published-release re-scan landed: grype-image (built
# from the branch), grype-deps (lockfiles), and grype-published-release (the
# GHCR manifest users actually pull). The count is deliberately exact rather
# than a floor — a lane silently dropped is as much a regression as one added
# off-pin, and this is the only place that would notice.
scan_action_total="$(awk \
	'$1 == "uses:" && $2 ~ /^anchore\/scan-action@/ { count++ } END { print count + 0 }' \
	.github/workflows/security-grype.yml)"
if [ "${scan_action_count}" -ne 3 ] || [ "${scan_action_total}" -ne 3 ]; then
	echo "FAIL: security-grype.yml must use anchore/scan-action v7.4.2 at the reviewed pin in all three scanner lanes" >&2
	failures=$((failures + 1))
fi
require_exactly_one_active ".github/workflows/release.yml" \
	'^[[:space:]]*GRYPE_VERSION:' \
	'GRYPE_VERSION: "0.118.0"' \
	"release Grype version must match anchore/scan-action@v7.4.2's Grype v0.118.0"
require_exactly_one_active ".github/workflows/release.yml" \
	'--certificate-identity[= ]"?https://github\.com/anchore/grype/' \
	'--certificate-identity "https://github.com/anchore/grype/.github/workflows/release.yaml@refs/heads/main"' \
	"release Grype checksum verification must use the exact Anchore Grype release workflow identity"

# Publishing holds contents, packages, identity, and attestation write access.
# Keep all provenance checks in a separate read-only job, then make the
# privileged job depend on it. Checking these strings file-wide would let the
# publish job run independently while an unrelated job carried the right text.
release_job="$(workflow_job_block ".github/workflows/release.yml" "release")"
require_block_text "${release_job}" "environment: Production" \
	"the privileged release job must use the Production environment"

release_prerequisite_job="$({
	awk '
		$0 == "jobs:" { in_jobs = 1; next }
		in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
			if (job != "" && checks_main && checks_ci && found == "") found = job
			job = $1
			sub(/:$/, "", job)
			checks_main = 0
			checks_ci = 0
			next
		}
		in_jobs && /git merge-base --is-ancestor/ && /origin\/main/ { checks_main = 1 }
		in_jobs && /uses: \.\/\.github\/actions\/verify-ci-success/ { checks_ci = 1 }
		END {
			if (job != "" && checks_main && checks_ci && found == "") found = job
			print found
		}
	' .github/workflows/release.yml
} || true)"

if [ -z "${release_prerequisite_job}" ]; then
	echo "FAIL: release.yml must have a read-only prerequisite job that proves the tag commit is on origin/main and passed ci-verify.yml" >&2
	failures=$((failures + 1))
else
	release_prerequisite_block="$(workflow_job_block ".github/workflows/release.yml" "${release_prerequisite_job}")"
	require_block_text "${release_prerequisite_block}" "actions: read" \
		"the release prerequisite must have only the read access needed to inspect CI"
	require_block_text "${release_prerequisite_block}" "contents: read" \
		"the release prerequisite must have only the read access needed to inspect the tag and main"
	if grep -Eq '^[[:space:]]+[A-Za-z0-9_-]+:[[:space:]]*write[[:space:]]*$' <<<"${release_prerequisite_block}"; then
		echo "FAIL: the release prerequisite must be unprivileged (its job permissions must not grant write access)" >&2
		failures=$((failures + 1))
	fi
	if ! grep -Eq 'target_sha="\$\(git rev-parse[[:space:]]+"?\$\{GITHUB_REF(_NAME)?\}\^\{commit\}"?\)"' <<<"${release_prerequisite_block}"; then
		# shellcheck disable=SC2016 # The diagnostic names the literal workflow expression.
		echo 'FAIL: the release prerequisite must resolve the pushed tag to a commit with git rev-parse "${GITHUB_REF}^{commit}"' >&2
		failures=$((failures + 1))
	fi
	if ! grep -Eq 'git merge-base --is-ancestor[[:space:]]+"?\$\{target_sha\}"?[[:space:]]+origin/main' <<<"${release_prerequisite_block}"; then
		echo "FAIL: the release prerequisite must prove the resolved tag commit is an ancestor of origin/main" >&2
		failures=$((failures + 1))
	fi
	# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
	require_block_text "${release_prerequisite_block}" 'echo "sha=${target_sha}" >> "$GITHUB_OUTPUT"' \
		"the release prerequisite must expose the resolved tag commit to the CI check"
	require_block_text "${release_prerequisite_block}" "workflow-file: ci-verify.yml" \
		"the release prerequisite must query ci-verify.yml"
	if ! grep -Eq 'target-sha:[[:space:]]+\$\{\{[[:space:]]*steps\.target\.outputs\.sha[[:space:]]*\}\}' <<<"${release_prerequisite_block}"; then
		echo "FAIL: the release prerequisite must query CI for the exact commit resolved from the pushed tag" >&2
		failures=$((failures + 1))
	fi

	release_needs="$(awk '
		$1 == "needs:" {
			field_count = NF
			$1 = ""
			gsub(/[\[\],]/, " ")
			print
			if (field_count == 1) collecting = 1
			next
		}
		collecting && $1 == "-" { print $2; next }
		collecting { exit }
	' <<<"${release_job}" | tr ' ' '\n' | sed '/^$/d')"
	if ! grep -Fxq -- "${release_prerequisite_job}" <<<"${release_needs}"; then
		echo "FAIL: the privileged release job must depend on the read-only release prerequisite" >&2
		failures=$((failures + 1))
	fi
fi

release_cut_main_line="$(grep -nE 'git merge-base --is-ancestor[[:space:]]+"?\$\{TARGET_SHA\}"?[[:space:]]+origin/main' \
	.github/workflows/release-cut.yml | sed -n '1s/:.*//p' || true)"
release_cut_tag_line="$(grep -nF -- '- name: Create and push release tag' \
	.github/workflows/release-cut.yml | sed -n '1s/:.*//p' || true)"
if [ -z "${release_cut_main_line}" ] || [ -z "${release_cut_tag_line}" ] ||
	[ "${release_cut_main_line:-0}" -ge "${release_cut_tag_line:-0}" ]; then
	echo "FAIL: release-cut.yml must prove TARGET_SHA is on origin/main before creating the release tag" >&2
	failures=$((failures + 1))
fi

# The published image is the only artifact users actually pull, and until this
# job existed nothing scanned it: security-grype.yml's container scan builds its
# own image from the root Dockerfile and never sees the real manifest. Each
# assertion below guards a specific way this gate could be quietly weakened
# back into decoration.
require_text ".github/workflows/release.yml" "Run Grype against the published image" \
	"the release must scan the actual published image, not a locally rebuilt approximation"
require_text ".github/workflows/release.yml" "registry:ghcr.io/codeswhat/portwing@" \
	"the published-image scan must target an immutable digest, not a mutable tag"
# The gate map below is only meaningful if an unrecognized value is rejected. A
# `case` that fell through to no `--fail-on` would silently un-gate any leg
# whose gate got typo'd.
require_text ".github/workflows/release.yml" "Unknown gate" \
	"the published-image scan must fail on an unrecognized gate value, not default to report-only"

# Assert against the grype command ITSELF, not the file. A file-wide grep for
# "--fail-on high" is satisfied by the `case` branch that builds the array, and
# a grep for the platform flag is satisfied by a comment — so both passed while
# the invocation had been stripped of the flags entirely. Deleting
# ${fail_on[@]+"${fail_on[@]}"} from the command left amd64 and arm64 scanning
# with no gate at all and this test still exiting 0 (measured, not assumed).
#
# Extract the invocation by following the line continuations, then require each
# flag inside it. An empty extraction fails every assertion below, so renaming
# or restructuring the command cannot silently un-assert it.
grype_invocation="$(awk '
	/grype "registry:/ { collecting = 1 }
	collecting { print; if ($0 !~ /\\[[:space:]]*$/) exit }
' .github/workflows/release.yml)"

require_in_grype_command() {
	local text="$1"
	local description="$2"

	if ! printf '%s\n' "${grype_invocation}" | grep -Fq -- "$text"; then
		echo "FAIL: ${description} (the grype invocation in .github/workflows/release.yml must contain: ${text})" >&2
		failures=$((failures + 1))
	fi
}

require_in_grype_command "registry:ghcr.io/codeswhat/portwing@" \
	"the published-image scan must target an immutable digest, not a mutable tag"
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
require_in_grype_command '--platform "${PLATFORM}"' \
	"the published-image scan must select the matrix platform, not the runner's native arch; without this the matrix stays intact while every leg scans the runner's own arch"
require_in_grype_command "--config .grype.yaml" \
	"the published-image scan must use the repo's reviewed suppression policy, not grype defaults"
# shellcheck disable=SC2016 # Asserting the literal text of the workflow.
require_in_grype_command '${fail_on[@]+"${fail_on[@]}"}' \
	"the per-platform gate must actually reach the grype command; the gate map is decoration if the flags never get passed"

# Grype can report either the Go vulnerability ID or the GitHub advisory ID
# for the same finding as its databases and alias selection change. Both IDs
# must remain paired and scoped to the exact third-party Compose binary; an
# unscoped alias suppresses a real future import into Portwing itself, while a
# missing alias makes the release gate depend on which identifier Grype emits.
require_scoped_compose_advisory_alias() {
	local advisory_id="$1"
	local block
	local count

	count="$(grep -Fc -- "  - vulnerability: ${advisory_id}" .grype.yaml || true)"
	block="$(awk -v advisory_id="${advisory_id}" '
		$1 == "-" && $2 == "vulnerability:" {
			if (capture) exit
			capture = ($3 == advisory_id)
		}
		capture { print }
	' .grype.yaml)"

	if [ "${count}" -ne 1 ] ||
		! grep -Fq 'name: github.com/docker/docker' <<<"${block}" ||
		! grep -Fq 'version: v28.5.2+incompatible' <<<"${block}" ||
		! grep -Fq 'type: go-module' <<<"${block}" ||
		! grep -Fq 'location: "**/usr/bin/docker-compose"' <<<"${block}"; then
		echo "FAIL: .grype.yaml must contain exactly one ${advisory_id} ignore scoped to github.com/docker/docker v28.5.2+incompatible at **/usr/bin/docker-compose" >&2
		failures=$((failures + 1))
	fi
}

require_scoped_compose_advisory_alias "GHSA-pxq6-2prw-chj9"
require_scoped_compose_advisory_alias "GO-2026-4883"
require_scoped_compose_advisory_alias "GHSA-x744-4wpc-v9h2"
require_scoped_compose_advisory_alias "GO-2026-4887"

# The Compose grpc suppression is a reachability argument about one third-party
# binary, not a claim about Portwing. Portwing's own module graph carries no
# google.golang.org/grpc at all, so an ignore that lost its package, version or
# location scope would quietly cover a real future grpc import into the agent
# itself. Version-scoped for the same reason the entries above are: the next
# grpc the Wolfi Compose package embeds has to come back through review.
require_scoped_compose_grpc_advisory() {
	local advisory_id="$1"
	local block
	local scrubbed_block
	local count

	count="$(grep -Fc -- "  - vulnerability: ${advisory_id}" .grype.yaml || true)"
	block="$(awk -v advisory_id="${advisory_id}" '
		$1 == "-" && $2 == "vulnerability:" {
			if (capture) exit
			capture = ($3 == advisory_id)
		}
		capture { print }
	' .grype.yaml)"
	# A field commented out (e.g. "#      location: ...") is absent as far
	# as YAML and grype are concerned, but a plain substring grep over the
	# raw block still sees the text and passes. Strip full comment lines and
	# trailing comments before matching, then anchor each field to a key at
	# the start of a line so a comment can no longer stand in for scope.
	scrubbed_block="$(sed -e 's/[[:space:]]#.*$//' -e '/^[[:space:]]*#/d' <<<"${block}")"

	if [ "${count}" -ne 1 ] ||
		! grep -Eq '^[[:space:]]+name: google\.golang\.org/grpc[[:space:]]*$' <<<"${scrubbed_block}" ||
		! grep -Eq '^[[:space:]]+version: v1\.83\.0[[:space:]]*$' <<<"${scrubbed_block}" ||
		! grep -Eq '^[[:space:]]+type: go-module[[:space:]]*$' <<<"${scrubbed_block}" ||
		! grep -Eq '^[[:space:]]+location: "\*\*/usr/bin/docker-compose"[[:space:]]*$' <<<"${scrubbed_block}"; then
		echo "FAIL: .grype.yaml must contain exactly one ${advisory_id} ignore scoped to google.golang.org/grpc v1.83.0 at **/usr/bin/docker-compose" >&2
		failures=$((failures + 1))
	fi
}

require_scoped_compose_grpc_advisory "GHSA-vp52-pcj8-j9qc"
require_text "scripts/verify-scanner-exclusions.sh" "github.com/docker/docker/daemon/pkg/plugin" \
	"the Compose advisory exclusion must stay guarded against linking Docker Engine's daemon plugin package"
require_text "scripts/verify-scanner-exclusions.sh" "github.com/docker/docker/pkg/authorization" \
	"the Compose advisory exclusion must stay guarded against linking Docker Engine's authorization package"
# The per-platform gate is the actual security posture, so assert the whole map
# rather than the substring "--fail-on high" — that string survives intact even
# if every leg is flipped to report-only.
#
# Wolfi has no armv7, so that leg is built from Alpine and carries a different
# (worse) package set than amd64/arm64; it is report-only on purpose, and
# RELEASING.md records why and when it flips. It is also the leg most likely to
# be dropped to make the gate quiet, which is why it is asserted by name here.
# Changing any of these three values is a security decision that has to update
# this list, RELEASING.md, and the matrix comment together.
expected_grype_gates="linux/amd64=high
linux/arm64=high
linux/arm/v7=none"
actual_grype_gates="$(awk '
	$0 == "  grype-published-image:" { injob = 1; next }
	injob && /^  [^ ]/ { injob = 0 }
	injob && $1 == "-" && $2 == "platform:" { platform = $3; next }
	injob && $1 == "gate:" { print platform "=" $2 }
' .github/workflows/release.yml)"
if [ "${actual_grype_gates}" != "${expected_grype_gates}" ]; then
	echo "FAIL: the published-image scan's per-platform gates changed (expected '${expected_grype_gates//$'\n'/, }', got '${actual_grype_gates//$'\n'/, }')" >&2
	failures=$((failures + 1))
fi

# The star-history chart is a committed artifact, not a third-party embed.
# Both retired services are rejected by name because both were, at some
# point, the prescribed answer: star-history.com until it stopped rendering
# for our repos, and warpchart.dev for three days after that. A future sweep
# working from a stale registry read would happily reinstate either one, and
# the failure mode is quiet — a live route serves a plausible card at HTTP
# 200 whether or not it has data, so nothing goes visibly red. A committed
# SVG fails loudly instead: stale is readable, missing is a broken image.
# Rejecting the hostnames also keeps visitor IPs off a third party, which is
# what the cookieless analytics posture requires.
reject_text_ci "README.md" "star-history.com" \
	"the README must not embed a third-party star-history chart; the chart is committed at docs/assets/star-history.svg"
reject_text_ci "README.md" "warpchart.dev" \
	"the README must not embed a third-party star-history chart; warpchart.dev was the prescribed replacement for three days and is retired org-wide"
# The refresh has to actually be reachable, which is a separate question from the
# chart being correct, and it is the half that fails silently. GitHub suppresses
# workflow runs for events emitted by GITHUB_TOKEN, so a `release:` trigger on
# starchart.yml fires and starts nothing: release.yml publishes via GoReleaser
# with exactly that credential. The workflow reads as correctly wired and never
# runs, and the only symptom is a chart that quietly stops updating, which is
# what choosing a committed artifact over a live embed was meant to prevent.
#
# The v* tag push is the trigger that works, because release-cut.yml pushes the
# tag with RELEASE_PAT specifically so downstream workflows fire; release.yml
# has always depended on that. Asserting the working trigger is present AND the
# broken one is absent, because each is satisfiable while the other is wrong.
if ! awk '/^on:/{c=1;next} c&&/^[^[:space:]#]/{exit} c' .github/workflows/starchart.yml |
	grep -Eq '^[[:space:]]*tags:[[:space:]]*$'; then
	echo "FAIL: starchart.yml must be triggered by the v* tag push (release-cut.yml pushes the tag with RELEASE_PAT so downstream workflows fire; a release: trigger or a GITHUB_TOKEN dispatch would create no run at all)" >&2
	failures=$((failures + 1))
fi
if awk '/^on:/{c=1;next} c&&/^[^[:space:]#]/{exit} c' .github/workflows/starchart.yml |
	grep -Eq '^[[:space:]]*release:[[:space:]]*$'; then
	echo "FAIL: starchart.yml must not use a release: trigger (GitHub suppresses runs for events emitted by GITHUB_TOKEN, and release.yml publishes with GITHUB_TOKEN, so it would read as wired and never run)" >&2
	failures=$((failures + 1))
fi

require_file "docs/assets/star-history.svg" \
	"the committed star-history chart must exist; the README references it by relative path and a missing file renders as a broken image"
require_file "docs/assets/star-history-dark.svg" \
	"the dark-theme chart must exist; the README's <picture> names it and a missing srcset target renders as a broken image for every dark-mode reader"

# Rejecting the two retired hosts is only half the contract: it stays satisfied
# if the chart is dropped from the README entirely, or repointed at some third
# host nobody has thought to name yet. Extract the chart section and assert
# inside it, the same way the grype invocation above is extracted — a file-wide
# grep for the img tag would be satisfied by any other occurrence in the README,
# and naming hosts one at a time only ever catches the ones already known.
star_section="$(awk '
	/<a id="star-history"><\/a>/ { collecting = 1 }
	collecting { print; if ($0 == "---") exit }
' README.md)"
if [ -z "${star_section}" ]; then
	echo 'FAIL: the README must keep the star-history section (no <a id="star-history"></a> anchor found in README.md)' >&2
	failures=$((failures + 1))
fi
# The whitespace before src is load-bearing: "src=" as a bare substring is also
# inside data-src, and <img data-src="docs/assets/star-history.svg"> renders
# nothing while satisfying the naive pattern.
if ! printf '%s\n' "${star_section}" | grep -Eq '<img([[:space:]][^>]*)?[[:space:]]src="docs/assets/star-history\.svg"'; then
	echo "FAIL: the star-history section must render the committed SVG (it needs an <img> whose src attribute is docs/assets/star-history.svg)" >&2
	failures=$((failures + 1))
fi
# The theme pair has to be selected by <picture>, never by a media query inside
# a single self-theming SVG. A media query in an <img>-embedded SVG resolves
# against the reader's OS preference, not GitHub's own theme toggle, so one
# file shows a white card to anyone reading GitHub in dark mode on a light OS.
# <picture> follows the toggle because GitHub sets color-scheme on the page.
# That failure is invisible to whoever ships it, since it only appears in the
# theme combination they are not using.
#
# Asserted as ONE <source> tag inside the <picture>, carrying BOTH attributes,
# rather than as three independent checks for a <picture>, a dark media query and
# a dark srcset. Three independent checks are all satisfied by markup that
# renders the light chart to everyone: the media query on one element and the
# srcset on another, or a correct-looking <source> sitting outside the <picture>
# where the browser ignores it. Each part being present somewhere in the section
# is not the same as the parts being wired together, which is the same defect
# this file has now been bitten by three times in different places.
#
# The section is flattened to a single line first so the extraction survives the
# block being reformatted across lines, and the two attribute orders are matched
# explicitly because ERE has no lookahead to do it order-independently.
picture_block="$(printf '%s\n' "${star_section}" | tr '\n' ' ' |
	sed -n 's/.*<picture>\(.*\)<\/picture>.*/\1/p')"
if [ -z "${picture_block}" ]; then
	echo "FAIL: the star-history section must wrap the chart in a <picture> element (a <source> outside a <picture> is inert, and every reader silently gets the light chart)" >&2
	failures=$((failures + 1))
fi
dark_source_pattern='<source[^>]*[[:space:]]media="\(prefers-color-scheme:[[:space:]]*dark\)"[^>]*[[:space:]]srcset="docs/assets/star-history-dark\.svg"'
dark_source_pattern_swapped='<source[^>]*[[:space:]]srcset="docs/assets/star-history-dark\.svg"[^>]*[[:space:]]media="\(prefers-color-scheme:[[:space:]]*dark\)"'
if ! printf '%s\n' "${picture_block}" |
	grep -Eq "${dark_source_pattern}|${dark_source_pattern_swapped}"; then
	echo 'FAIL: the star-history section needs a single <source> inside its <picture> carrying both media="(prefers-color-scheme: dark)" and srcset="docs/assets/star-history-dark.svg" (split across two elements, or placed outside the <picture>, the browser ignores it and every reader gets the light chart)' >&2
	failures=$((failures + 1))
fi
# Catches the next replacement service without having to know its name. This is
# a regression guard, not an adversary control: it reads the markup literally,
# so a deliberately entity-encoded host (warpchart&#46;dev) would slip past it.
# Worth knowing, not worth an HTML decoder in shell.
#
# Deliberately not restricted to <img src>, and deliberately not a list of
# attribute names either. Any attribute pointing at an external host is a
# third-party request from this section — data-src is fetched by lazy-loaders,
# <object data=> and <use href=> load too — and enumerating the attributes
# worth checking is the same mistake as enumerating the hosts worth rejecting:
# it only ever covers the ones already thought of. A reject is the one place
# where matching too much is the safe direction, so this matches any attribute
# whose value is an external URL. The quote handling covers the three legal
# forms (double, single, bare) and the optional scheme covers protocol-relative
# //host references. Prose URLs in this section are unaffected: they have no
# preceding `=`.
#
# One carve-out, and it is a distinction rather than an exception: the shape
# wraps the chart in a link to the stargazers page, so an <a href> to an
# external host is expected here. A link is navigation the reader chooses, not
# a resource the browser fetches on render, so it leaks no visitor IP and is
# not what this check is about. Neutralising href only on <a> open tags keeps
# every other element in scope, so <use href>, <object data> and data-src are
# still caught. Exempting the stargazers URL by name instead would have been an
# allow-list of one, which is the enumeration mistake described above.
#
# The url(...) alternation is not decoration: an external reference inside a CSS
# value is not preceded by `=`, so style="background:url(//host/x.png)" slipped
# the attribute pattern entirely. Found by mutation, not by reading it back —
# the comment here previously claimed that case was covered when it was not.
external_src_pattern="(=|url\()[[:space:]]*[\"']?(https?:)?//"
scrubbed_section="$(printf '%s\n' "${star_section}" |
	sed -E 's@<a[[:space:]]([^>]*[[:space:]])?href="[^"]*"@<a \1href="#"@g')"
if printf '%s\n' "${scrubbed_section}" | grep -Eiq "${external_src_pattern}"; then
	echo "FAIL: the star-history section must not load an image from a third party (the chart is committed at docs/assets/star-history.svg)" >&2
	failures=$((failures + 1))
fi

# And existence is not content: the refresh workflow commits these files back at
# every release cut, so a generator that fails halfway can leave a truncated or
# wrong-repo SVG in place. The title carries the repo slug, so requiring it
# proves the file is both a closed SVG document and this repo's chart. Matched
# as a prefix because the title also carries the live star count, which changes
# on every refresh; pinning the whole string would fail on the next real one.
#
# Both files get the same three checks. Asserting only the light one would let a
# half-finished generator run ship a broken dark chart, and dark is the half
# nobody reviewing on a light screen would ever see.
for chart in docs/assets/star-history.svg docs/assets/star-history-dark.svg; do
	require_text "$chart" "<title>Star history for CodesWhat/portwing" \
		"the committed chart must be this repository's chart, not a placeholder or another repo's"
	require_text "$chart" "<path" \
		"the committed chart must actually plot the series; a titled but empty <svg> renders as blank space, not a broken image"
	require_last_line "$chart" "</svg>" \
		"the committed chart must be a complete SVG document; a truncated generator run renders as a broken image"
done

# The one failure every check above passes: a renderer that writes the light
# chart to both paths. Each file is then individually valid, titled, plotted and
# closed, the <picture> resolves, nothing 404s, and dark-mode readers get a white
# card anyway. That is the exact defect this whole theme pair exists to prevent,
# so it is worth asserting directly rather than inferring from the parts.
if cmp -s docs/assets/star-history.svg docs/assets/star-history-dark.svg; then
	echo "FAIL: the light and dark charts are byte-identical, so the <picture> selects the same image for both themes (the dark variant should be drawn on the dark canvas with the derived accent)" >&2
	failures=$((failures + 1))
fi

require_file "docs/content/docs/installation.mdx" "the documentation site must include native installation guidance"
require_file "NOTICE" "project identity and copyright must live outside the standard license text"
require_first_line "LICENSE" "                    GNU AFFERO GENERAL PUBLIC LICENSE" "LICENSE must begin with the canonical AGPL-3.0 text"
require_sha256 "LICENSE" "8486a10c4393cee1c25392769ddd3b2d6c242d6ec7928e1414efff7dfb2f07ef" "LICENSE must match GitHub's canonical AGPL-3.0 template byte for byte"
reject_text "LICENSE" "Portwing - Lightweight Remote Docker Agent" "LICENSE must not carry a project-specific preamble"
reject_text "LICENSE" "Copyright (C) 2026 CodesWhat" "LICENSE must not carry a project-specific copyright preamble"
if [ -f "NOTICE" ]; then
	require_text "NOTICE" "Portwing - Lightweight Remote Docker Agent" "NOTICE must preserve the project identity"
	require_text "NOTICE" "Copyright (C) 2026 CodesWhat" "NOTICE must preserve the project copyright"
fi
if [ -f "docs/content/docs/installation.mdx" ]; then
	require_text "docs/content/docs/installation.mdx" "brew install --cask codeswhat/tap/portwing" "Homebrew installation must be documented"
	require_text "docs/content/docs/installation.mdx" "apt install" "deb installation must be documented"
	require_text "docs/content/docs/installation.mdx" "rpm --install" "rpm installation must be documented"
	require_text "docs/content/docs/installation.mdx" "Upgrade" "package upgrades must be documented"
	require_text "docs/content/docs/installation.mdx" "Uninstall" "package removal must be documented"
	require_text "docs/content/docs/installation.mdx" "portwing.service" "service expectations must be documented"
	require_text "docs/content/docs/installation.mdx" "checksums.txt" "artifact verification must be linked to package installation"
fi

require_text "README.md" "brew install --cask codeswhat/tap/portwing" "the repository landing page must advertise Homebrew installation"
require_text "README.md" "/docs/installation" "the repository landing page must link the full package guide"
# shellcheck disable=SC2016 # The documented shell command expands VERSION.
require_text "README.md" 'release.yml@refs/tags/v${VERSION}' "public verification instructions must bind signatures to the selected tag"
require_text "RELEASING.md" "HOMEBREW_TAP_TOKEN" "maintainer release docs must name the tap publishing credential"
require_text "RELEASING.md" "verify-native-packages" "maintainer release docs must describe the native package gate"
require_text "website/src/components/get-started.tsx" "codeswhat/tap/portwing" "the website must advertise the Homebrew cask"
require_text "website/src/components/get-started.tsx" "apt install ./portwing_" "the website must advertise the deb package"

require_text "scripts/ci/go-release-check.sh" "bash scripts/package-release-config-test.sh" "CI release adapter must enforce the package release contract"
require_text "scripts/ci/go-release-check.sh" "bash scripts/install-config-permissions-test.sh" "CI release adapter must enforce installer config permissions"
require_text "scripts/ci/go-release-check.sh" "bash scripts/standard-mode-bind-config-test.sh" "CI release adapter must enforce safe standard-mode publication"

required_ci_contexts=(
	"Go CI / Build & Test"
	"Go CI / Lint"
	"Go CI / Govulncheck"
	"Go CI / Workflow Security"
	"Go CI / Commit Message"
	"Go CI / GoReleaser Config"
	"Go CI / Qlty Check"
	"Security: Secrets"
	"Dependency Review"
	"CodeQL Analysis"
	"Security: Gosec SAST"
	"Security: Grype Dependency Scan (Go + npm)"
)

for context in "${required_ci_contexts[@]}"; do
	require_text "scripts/apply-branch-protection.sh" "\"context\": \"${context}\"" "branch protection must require the reusable context ${context}"
done

retired_ci_contexts=(
	"🏗️ Build & Test"
	"🧹 Lint"
	"🔍 Govulncheck"
	"🔒 Workflow Security"
	"💬 Commit Message"
	"📦 GoReleaser Config"
)

for context in "${retired_ci_contexts[@]}"; do
	reject_text ".github/workflows/ci-verify.yml" "name: \"${context}\"" "workflow must not report the retired CI context ${context}"
	reject_text "scripts/apply-branch-protection.sh" "\"context\": \"${context}\"" "branch protection must not require the retired CI context ${context}"
done

# The X1 canary promoted; branch protection now requires only the reusable
# "Go CI / ..." contexts, so the plain-name bridge jobs that mirrored them
# were removed from the workflow, and the branch-protection IaC now targets
# the reusable contexts directly. Reject reintroduction of the retired
# plain-name contexts in both places.
retired_x1_bridge_contexts=(
	"Build & Test"
	"Lint"
	"Govulncheck"
	"Workflow Security"
	"Commit Message"
	"GoReleaser Config"
)

for context in "${retired_x1_bridge_contexts[@]}"; do
	reject_text ".github/workflows/ci-verify.yml" "name: \"${context}\"" "workflow must not reintroduce the retired X1 bridge context ${context}"
	reject_text "scripts/apply-branch-protection.sh" "\"context\": \"${context}\"" "branch protection must not reintroduce the retired X1 bridge context ${context}"
done

# The emoji-prefixed job names retired when CI converged on the house
# no-emoji standard. Renaming a job renames its check-run context, so
# reintroducing one of these silently stops producing a context the ruleset
# still requires, and every PR hangs waiting for a status that never arrives.
retired_emoji_ci_contexts=(
	"🔑 Security: Secrets"
	"📦 Dependency Review"
	"🔍 CodeQL Analysis"
	"🔐 Security: Gosec SAST"
	"📦 Security: Grype Dependency Scan (Go + npm)"
)

for context in "${retired_emoji_ci_contexts[@]}"; do
	reject_text ".github/workflows/ci-verify.yml" "name: \"${context}\"" "workflow must not report the retired emoji CI context ${context}"
	reject_text ".github/workflows/security-grype.yml" "name: \"${context}\"" "workflow must not report the retired emoji CI context ${context}"
	reject_text "scripts/apply-branch-protection.sh" "\"context\": \"${context}\"" "branch protection must not require the retired emoji CI context ${context}"
done

# govulncheck has exactly one gate: the required "Go CI / Govulncheck"
# context, which runs it at v1.7.0 via scripts/ci/go-govulncheck.sh. A second
# copy used to live in security-grype.yml pinned to v1.2.0 - an older tool on
# a non-required check, gating the same property twice. Reject its return.
reject_text ".github/workflows/security-grype.yml" "golang.org/x/vuln/cmd/govulncheck" \
	"security-grype.yml must not reintroduce a duplicate govulncheck gate (Go CI / Govulncheck is the required one)"

# The source-level half of the scanner-exclusion check must stay on a job that
# actually runs on pull requests. grype-image also calls the script, but with
# path arguments and under `if: github.event_name != 'pull_request'`, so it
# always skips on PRs. When the duplicate govulncheck job was retired this
# check moved to grype-deps; if it ever leaves, PRs silently stop verifying
# that the scoped Grype suppressions are not actually imported.
require_text ".github/workflows/security-grype.yml" "./scripts/verify-scanner-exclusions.sh" \
	"security-grype.yml must run the source-level scanner-exclusion check"
grype_deps_exclusion="$(
	perl -0777 -ne 'print "ok" if /^  grype-deps:.*?verify-scanner-exclusions\.sh/ms' \
		.github/workflows/security-grype.yml
)"
if [ -z "$grype_deps_exclusion" ]; then
	echo "FAIL: the scanner-exclusion check must live in the grype-deps job, which runs on pull requests" >&2
	failures=$((failures + 1))
fi

# gosec runs with -no-fail on purpose (it has no severity cutoff and would
# otherwise fail on LOW-severity heuristics like G104). That makes the explicit
# severity-gate step the only thing standing between the required
# "Security: Gosec SAST" context and a permanent green. Delete the step while
# leaving -no-fail and the check silently becomes report-only, which is worse
# than not requiring it at all, because the ruleset still claims it gates.
require_text ".github/workflows/security-grype.yml" "args: -no-fail -fmt sarif -out gosec-results.sarif ./..." \
	"gosec must keep -no-fail; the severity-gate step decides the outcome, not gosec's own exit code"
require_text ".github/workflows/security-grype.yml" "Gate on gosec HIGH/MEDIUM findings" \
	"security-grype.yml must keep the explicit gosec severity gate; -no-fail alone makes the required check a no-op"
require_text ".github/workflows/security-grype.yml" 'select(.level == "error")' \
	"the gosec gate must filter SARIF results on level==\"error\" (gosec's MEDIUM/HIGH), not merely check the file exists"

# The house standard is no emoji anywhere in CI, not just in the job names
# that happen to be required contexts. Enforced over the whole workflow
# directory so run-name blocks and step output cannot drift back. perl rather
# than `grep -P`, which BSD grep does not reliably provide.
emoji_in_workflows="$(
	perl -CSD -ne 'print "$ARGV:$.\n" if /\p{Extended_Pictographic}/' .github/workflows/*.yml
)"
if [ -n "$emoji_in_workflows" ]; then
	echo "FAIL: CI workflows must not contain emoji (house standard). Offending lines:" >&2
	echo "$emoji_in_workflows" >&2
	failures=$((failures + 1))
fi

# The two security-grype.yml jobs above are required contexts, so that
# workflow must fire on every PR. A `paths:` filter under its pull_request
# trigger produces no check run at all on a PR it does not match, which wedges
# that PR forever. Gate expensive steps inside the job instead.
grype_pr_paths="$(
	perl -0777 -ne 'print "path-filtered" if /^  pull_request:.*?^    paths:/ms' \
		.github/workflows/security-grype.yml
)"
if [ -n "$grype_pr_paths" ]; then
	echo "FAIL: security-grype.yml must not path-filter its pull_request trigger; its jobs are required contexts" >&2
	failures=$((failures + 1))
fi

# A .gitleaksignore entry pinned to a working-tree fingerprint (file:rule:line)
# addresses a line number in the current tree. The PR Secrets check restores
# the ignore policy from the base ref before scanning, so any later commit
# that shifts that line number makes the pin stop matching and the same
# finding fails again — which is exactly the failure this check exists to
# rule out permanently, not repin around. A commit-pinned entry (SHA:file:...)
# doesn't have that problem: the SHA addresses an immutable blob, so it never
# drifts. Require every active line to be commit-pinned.
gitleaksignore_bad_entries="$(grep -vE '^[[:space:]]*(#|$)' .gitleaksignore |
	grep -vE '^[0-9a-f]{40}:' || true)"
if [ -n "${gitleaksignore_bad_entries}" ]; then
	echo "FAIL: .gitleaksignore entries must be commit-pinned (a 40-hex commit SHA followed by ':'), not a bare file:rule:line working-tree fingerprint; a line-pinned entry breaks the base-ref policy restore on promotion PRs once the pinned line shifts. Fix the flagged content instead of repinning. Offending line(s):" >&2
	echo "${gitleaksignore_bad_entries}" >&2
	failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
	echo "${failures} package release contract check(s) failed" >&2
	exit 1
fi

echo "Package release contract checks passed."
