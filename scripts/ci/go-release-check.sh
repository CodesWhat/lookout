#!/usr/bin/env bash
set -euo pipefail

module_directory="${MODULE_DIRECTORY:-.}"
if [ "${module_directory}" != "." ]; then
	echo "Portwing release checks require MODULE_DIRECTORY=." >&2
	exit 2
fi

go run github.com/goreleaser/goreleaser/v2@v2.17.1 check
bash scripts/package-release-config-self-test-test.sh
bash scripts/package-release-config-self-test-cleanup-test.sh
bash scripts/package-release-config-test-test.sh
bash scripts/package-release-config-test.sh
bash scripts/install-config-permissions-test.sh
bash scripts/standard-mode-bind-config-test.sh
bash scripts/setup-go-egress-config-test-test.sh
bash scripts/setup-go-egress-config-test.sh
bash scripts/pre-push-config-test-test.sh
bash scripts/pre-push-config-test.sh
bash scripts/fuzz-tier-config-test-test.sh
bash scripts/fuzz-tier-config-test.sh
bash scripts/fuzz-run-script-test.sh
bash scripts/fuzz-score-script-test.sh
bash scripts/fuzz-replay-script-test.sh
bash scripts/fuzz-history-merge-script-test.sh
bash scripts/cflite-config-test-test.sh
bash scripts/cflite-config-test.sh
bash scripts/codeql-trigger-config-test-test.sh
bash scripts/codeql-trigger-config-test.sh
bash scripts/quality-lane-notify-config-test-test.sh
bash scripts/quality-lane-notify-config-test.sh
bash scripts/security-grype-config-test-test.sh
bash scripts/security-grype-config-test.sh
bash scripts/quality-integration-engines-config-test-test.sh
bash scripts/quality-integration-engines-config-test.sh
bash scripts/quality-history-config-test-test.sh
bash scripts/quality-history-config-test.sh
bash scripts/main-is-released-config-test-test.sh
bash scripts/main-is-released-config-test.sh
bash scripts/starchart-config-test-test.sh
bash scripts/starchart-config-test.sh
bash scripts/drydock-compat-script-test.sh
bash scripts/benchstat-gate-script-test.sh
bash scripts/benchstat-walk-baselines-test.sh
bash scripts/mutation-gate-script-test.sh
bash scripts/mutation-ratchet-script-test.sh
bash scripts/quality-history-script-test.sh
bash scripts/quality-history-record-test.sh
bash scripts/mutation-survivors-record-test.sh
bash scripts/mutation-survivors-diff-test.sh
