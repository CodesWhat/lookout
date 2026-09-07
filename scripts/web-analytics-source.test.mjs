import assert from "node:assert/strict";
import fs from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import test from "node:test";

const ROOT = path.resolve(import.meta.dirname, "..");

function read(relativePath) {
  return fs.readFileSync(path.join(ROOT, relativePath), "utf8");
}

function sourceFiles(root) {
  return fs
    .readdirSync(root, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile() && /\.(?:ts|tsx|js|mjs)$/.test(entry.name))
    .map((entry) => read(path.relative(ROOT, path.join(entry.parentPath, entry.name))));
}

function assertPostHogPackagesCurrent(lock) {
  const postHogPackages = Object.entries(lock.packages).filter(
    ([name]) => name === "node_modules/posthog-js" || name.endsWith("/node_modules/posthog-js"),
  );
  assert.ok(postHogPackages.length > 0);
  for (const [name, packageData] of postHogPackages) {
    assert.equal(packageData.version, "1.427.0", name);
  }
}

test("both web roots use the shared PostHog client and no Vercel analytics", () => {
  const rootPackage = JSON.parse(read("package.json"));
  const lockText = read("package-lock.json");
  const lock = JSON.parse(lockText);
  const sources = sourceFiles(path.join(ROOT, "website", "src"))
    .concat(sourceFiles(path.join(ROOT, "docs", "src")))
    .join("\n");

  assert.ok(rootPackage.workspaces.includes("analytics"));
  assertPostHogPackagesCurrent(lock);
  assert.doesNotMatch(lockText, /"@vercel\/analytics"/);
  assert.doesNotMatch(sources, /@vercel\/analytics|SpeedInsights|\.identify\s*\(/);
  const analyticsClient = read("analytics/src/client.ts");
  assert.match(analyticsClient, /posthog-js\/dist\/module\.slim"/);
  assert.match(analyticsClient, /createWebVitalsReporter/);
  assert.doesNotMatch(analyticsClient, /webVitalsBuffer\.begin|captureWebVital/);
  assert.doesNotMatch(analyticsClient, /WebVitalsAutocapture|extension-bundles/);

  const require = createRequire(import.meta.url);
  const extensionBundle = require.resolve("posthog-js/dist/extension-bundles.js", {
    paths: [path.join(ROOT, "analytics")],
  });
  const extensionBundleBytes = fs.statSync(extensionBundle).size;
  assert.equal(extensionBundleBytes, 155_315);
  assert.notEqual(extensionBundleBytes, 148_886);
  assert.ok(extensionBundleBytes > 850_000 - 784_278);

  for (const app of ["website", "docs"]) {
    assert.match(read(`${app}/src/instrumentation-client.ts`), /initializeAnalytics/);
    assert.match(read(`${app}/src/app/layout.tsx`), /<AnalyticsRuntime\s*\/>/);
    const runtime = read(`${app}/src/components/analytics-runtime.tsx`);
    assert.match(runtime, /capturePageview\(pathname\)/);
    assert.match(runtime, /useReportWebVitals\(reportWebVital\)/);
    assert.match(runtime, /useCallback<Parameters<typeof useReportWebVitals>\[0\]>/);
    assert.match(runtime, /createWebVitalsReporter\(pathname\)/);
    assert.match(runtime, /webVitalsReporter\.current\?\.\(name, value\)/);
    assert.match(runtime, /\},\s*\[\],\s*\)/);
    assert.doesNotMatch(runtime, /pathnameRef|\.current = pathname/);
    assert.match(runtime, /useEffect\(\(\) => \{\s*capturePageview\(pathname\)/);
  }

  assert.doesNotMatch(read("analytics/src/contract.ts"), /metric_name|metric_value/);

  const lockWithNestedStalePostHog = { packages: { ...lock.packages } };
  lockWithNestedStalePostHog.packages["node_modules/analytics/node_modules/posthog-js"] = {
    version: "1.417.0",
  };
  assert.throws(() => assertPostHogPackagesCurrent(lockWithNestedStalePostHog));
});

test("the finite CTA source map covers actual tracked component calls", () => {
  const contract = read("analytics/src/contract.ts");
  const trackedSources = [
    "website/src/components/cta-buttons.tsx",
    "website/src/components/footer.tsx",
    "website/src/components/get-started.tsx",
    "website/src/components/site-header.tsx",
    "docs/src/components/footer.tsx",
    "docs/src/components/site-header.tsx",
  ].map(read);

  for (const id of [
    "install_quick",
    "install_secure",
    "install_native",
    "docs_root",
    "docs_getting_started",
    "docs_installation",
    "github_org",
    "github_repository",
    "community_discord",
  ]) {
    assert.match(contract, new RegExp(`\\b${id}\\b`));
    assert.ok(
      trackedSources.some((source) => source.includes(`"${id}"`)),
      `${id} is unused`,
    );
  }

  assert.doesNotMatch(trackedSources.join("\n"), /docs_security/);
  assert.doesNotMatch(contract, /docs_security/);
});

test("the cookieless envelope keeps the fields PostHog's server hash requires", () => {
  const contract = read("analytics/src/contract.ts");

  // PostHog's cookieless server-hash ingestion step reads $raw_user_agent and
  // $host straight off event.properties and drops the event — with a
  // cookieless_missing_user_agent / cookieless_missing_host ingestion warning
  // and zero rows ingested — if either is absent (PostHog/posthog
  // nodejs/src/ingestion/common/cookieless/cookieless-manager.ts,
  // getProperties()/doBatchInner(), commit e87e55a). posthog-js attaches both
  // by default; before_send must allowlist them through, not silently strip
  // them. Regression guard: if these keys ever disappear from the allowlist
  // (or the comment explaining why they're there), every cookieless event on
  // every CodesWhat public site drops with no PostHog-side error beyond the
  // ingestion warning.
  assert.match(contract, /\$raw_user_agent/u);
  assert.match(contract, /\$host/u);
  assert.match(contract, /cookieless_missing_user_agent|cookieless server-hash/u);
});
