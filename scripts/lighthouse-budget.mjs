import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import process from "node:process";
import { fileURLToPath, pathToFileURL } from "node:url";
import { killAll, launch } from "chrome-launcher";
import lighthouse from "lighthouse";

const SCRIPT_FILE = fileURLToPath(import.meta.url);
const ROOT = path.resolve(path.dirname(SCRIPT_FILE), "..");

export function median(values) {
  if (values.length === 0) throw new Error("median requires at least one value");
  const sorted = [...values].sort((a, b) => a - b);
  const middle = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 0 ? (sorted[middle - 1] + sorted[middle]) / 2 : sorted[middle];
}

export function verifyLighthouseRuns(config, runs) {
  if (runs.length === 0) throw new Error(`expected Lighthouse runs for ${config.site}`);
  const result = {
    performance: median(runs.map((run) => run.performance)),
    totalByteWeight: median(runs.map((run) => run.totalByteWeight)),
    scriptTransferBytes: median(runs.map((run) => run.scriptTransferBytes)),
  };
  if (result.performance < config.performanceMin) {
    throw new Error(
      `${config.site} performance budget exceeded: ${result.performance} < ${config.performanceMin}`,
    );
  }
  if (result.totalByteWeight > config.totalByteWeightMax) {
    throw new Error(
      `${config.site} total byte weight budget exceeded: ${result.totalByteWeight} > ${config.totalByteWeightMax}`,
    );
  }
  if (result.scriptTransferBytes > config.scriptTransferBytesMax) {
    throw new Error(
      `${config.site} script transfer budget exceeded: ${result.scriptTransferBytes} > ${config.scriptTransferBytesMax}`,
    );
  }
  return result;
}

function serveStatic(outputRoot, mountPath) {
  const rootPrefix = `${path.resolve(outputRoot)}${path.sep}`;
  const mountPrefix = mountPath === "/" ? "" : mountPath;
  const server = http.createServer((request, response) => {
    let pathname;
    try {
      pathname = decodeURIComponent(new URL(request.url ?? "/", "http://127.0.0.1").pathname);
    } catch {
      response.writeHead(400).end("bad request");
      return;
    }
    if (mountPrefix && pathname !== mountPrefix && !pathname.startsWith(`${mountPrefix}/`)) {
      response.writeHead(404).end("not found");
      return;
    }
    const outputPathname = mountPrefix ? pathname.slice(mountPrefix.length) || "/" : pathname;
    let file = path.resolve(outputRoot, `.${outputPathname}`);
    if (file !== path.resolve(outputRoot) && !file.startsWith(rootPrefix)) {
      response.writeHead(403).end("forbidden");
      return;
    }
    if (pathname.endsWith("/")) file = path.join(file, "index.html");
    if (!fs.existsSync(file) || !fs.statSync(file).isFile()) {
      response.writeHead(404).end("not found");
      return;
    }
    const contentTypes = {
      ".css": "text/css",
      ".html": "text/html",
      ".ico": "image/x-icon",
      ".js": "text/javascript",
      ".json": "application/json",
      ".png": "image/png",
      ".svg": "image/svg+xml",
      ".webmanifest": "application/manifest+json",
      ".woff2": "font/woff2",
    };
    response.writeHead(200, {
      "Content-Type": contentTypes[path.extname(file)] ?? "application/octet-stream",
      "Cache-Control": "no-store",
    });
    fs.createReadStream(file).pipe(response);
  });
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => resolve(server));
  });
}

function metrics(lhr) {
  if (lhr.runtimeError?.code) throw new Error(`Lighthouse runtime error: ${lhr.runtimeError.code}`);
  const performance = lhr.categories.performance?.score;
  const totalByteWeight = lhr.audits["total-byte-weight"]?.numericValue;
  const scripts = lhr.audits["resource-summary"]?.details?.items?.find(
    (item) => item.resourceType === "script",
  );
  if (
    typeof performance !== "number" ||
    typeof totalByteWeight !== "number" ||
    typeof scripts?.transferSize !== "number"
  ) {
    throw new Error("Lighthouse report is missing required performance metrics");
  }
  return { performance, totalByteWeight, scriptTransferBytes: scripts.transferSize };
}

export async function withLighthouseResources({ startServer, startChrome, run }) {
  const server = await startServer();
  let chrome;
  try {
    chrome = await startChrome();
    return await run({
      server,
      chrome,
      startChrome,
      setChrome: (nextChrome) => {
        chrome = nextChrome;
      },
    });
  } finally {
    try {
      if (chrome) await chrome.kill();
    } finally {
      await new Promise((resolve) => server.close(resolve));
    }
  }
}

const DEFAULT_CHROME_ATTEMPTS = 3;
const MAX_CHROME_ATTEMPTS = 10;

// The Chrome debugging port can go away mid-run (Chrome crash, runner OOM),
// which lighthouse and chrome-launcher surface as a plain connection error
// rather than anything naming Chrome. Budget failures (verifyLighthouseRuns,
// missing metrics) must never match here, or a real regression would get
// silently retried into a false pass. puppeteer-core wraps the underlying
// ECONNREFUSED in a TypeError and only exposes it on `error.cause`, so this
// walks the cause chain (depth-capped to stay cycle-safe).
export function isChromeConnectionError(error) {
  if (!error) return false;
  for (let current = error, depth = 0; current && depth < 5; current = current.cause, depth += 1) {
    const message = String(current?.message ?? current);
    if (
      current?.code === "ECONNREFUSED" ||
      current?.code === "ECONNRESET" ||
      /ECONNREFUSED|ECONNRESET|socket hang up/.test(message) ||
      /unable to connect to chrome/i.test(message) ||
      /failed to fetch browser websocket url/i.test(message) ||
      /chrome (has crashed|failed to launch|connection)/i.test(message) ||
      /(target|session) closed/i.test(message)
    ) {
      return true;
    }
  }
  return false;
}

// LIGHTHOUSE_CHROME_ATTEMPTS caps the total attempts (including the first) a
// single Lighthouse run gets before this exits fail-closed. It must be an
// integer between 1 and MAX_CHROME_ATTEMPTS; anything else is a configuration
// error, not a run failure, so it throws immediately rather than falling
// back silently. A plain /^\d+$/ check isn't enough on its own: a long
// digit string like "9".repeat(400) still matches the regex but parses to
// Infinity, which would never let the retry loop terminate.
export function resolveChromeAttempts(env = process.env) {
  const raw = env.LIGHTHOUSE_CHROME_ATTEMPTS;
  if (raw === undefined || raw === "") return DEFAULT_CHROME_ATTEMPTS;
  const parsed = Number(raw);
  if (
    !/^\d+$/.test(raw) ||
    !Number.isSafeInteger(parsed) ||
    parsed < 1 ||
    parsed > MAX_CHROME_ATTEMPTS
  ) {
    throw new Error(
      `LIGHTHOUSE_CHROME_ATTEMPTS must be an integer between 1 and ${MAX_CHROME_ATTEMPTS}, got ${JSON.stringify(raw)}`,
    );
  }
  return parsed;
}

async function killChromeQuietly(chrome) {
  try {
    await chrome?.kill();
  } catch (error) {
    // The connection already dropped; the process is likely already gone.
    // Swallowed deliberately, but worth a trace: if kill() is failing for a
    // reason other than "already gone," a leaked Chrome process is the
    // symptom and this is the only place that would say why.
    process.stderr.write(
      `lighthouse-budget: chrome.kill() failed, ignoring (${error?.message ?? error})\n`,
    );
  }
}

// A launch() rejection from chrome-launcher/puppeteer-core still leaves the
// spawned Chrome process registered internally (chrome-launcher adds it to
// its instance registry before awaiting readiness), so a failed attempt
// orphans a process unless something reaps it. killAll() does that reaping;
// it only touches instances this process's chrome-launcher actually spawned,
// so it is a no-op when nothing has launched yet (e.g. under test doubles).
export async function launchChromeWithRetries({
  launchChrome,
  maxAttempts = resolveChromeAttempts(),
  log = (message) => process.stdout.write(message),
}) {
  if (maxAttempts < 1) {
    throw new Error(
      `LIGHTHOUSE_CHROME_ATTEMPTS resolved to ${maxAttempts}; at least one attempt is required`,
    );
  }
  for (let attempt = 1; attempt <= maxAttempts; attempt += 1) {
    try {
      return await launchChrome();
    } catch (error) {
      if (!isChromeConnectionError(error)) throw error;
      killAll();
      if (attempt === maxAttempts) {
        throw new Error(
          `chrome failed to launch ${maxAttempts} time(s) in a row (${error.message}); giving up`,
          { cause: error },
        );
      }
      log(
        `chrome launch attempt ${attempt}/${maxAttempts} failed (${error.message}), relaunching\n`,
      );
    }
  }
}

// Core retry loop, kept free of real Chrome/lighthouse so it can be unit
// tested with injected doubles. `chrome` is the already-launched instance to
// start with; `startChrome` relaunches a fresh one when the current one
// drops; `runLighthouse` performs a single audit and is the thing that
// throws the injected ECONNREFUSED in tests.
export async function collectLighthouseRuns({
  config,
  url,
  outputDir,
  chrome,
  startChrome,
  setChrome,
  runLighthouse,
  maxAttempts = resolveChromeAttempts(),
  log = (message) => process.stdout.write(message),
}) {
  if (maxAttempts < 1) {
    throw new Error(
      `LIGHTHOUSE_CHROME_ATTEMPTS resolved to ${maxAttempts}; at least one attempt is required`,
    );
  }
  let current = chrome;
  const completedRuns = [];
  for (let index = 0; index < config.numberOfRuns; index += 1) {
    let lastChromeError;
    let attemptResult;
    let succeeded = false;
    for (let attempt = 1; attempt <= maxAttempts; attempt += 1) {
      try {
        const result = await runLighthouse(url, { port: current.port });
        if (!result) throw new Error(`Lighthouse returned no result for run ${index + 1}`);
        fs.writeFileSync(path.join(outputDir, `run-${index + 1}.json`), JSON.stringify(result.lhr));
        attemptResult = metrics(result.lhr);
        succeeded = true;
        break;
      } catch (error) {
        if (!isChromeConnectionError(error)) throw error;
        lastChromeError = error;
        await killChromeQuietly(current);
        if (attempt === maxAttempts) break;
        log(
          `run ${index + 1}/${config.numberOfRuns} attempt ${attempt}/${maxAttempts}: ` +
            `chrome connection lost (${error.message}), relaunching\n`,
        );
        try {
          current = await startChrome();
          setChrome(current);
        } catch (relaunchError) {
          killAll();
          throw new Error(
            `run ${index + 1}/${config.numberOfRuns} could not relaunch Chrome after attempt ` +
              `${attempt}/${maxAttempts} (${relaunchError.message})`,
            { cause: relaunchError },
          );
        }
      }
    }
    if (!succeeded) {
      throw new Error(
        `run ${index + 1}/${config.numberOfRuns} lost its Chrome connection ${maxAttempts} ` +
          `time(s) in a row (${lastChromeError?.message}); giving up`,
      );
    }
    completedRuns.push(attemptResult);
  }
  return completedRuns;
}

async function run(configPath) {
  const config = (await import(pathToFileURL(path.resolve(configPath)).href)).default;
  if (typeof config.outputRoot !== "string" || config.outputRoot.length === 0) {
    throw new Error(`Lighthouse output root is missing for ${config.site ?? configPath}`);
  }
  if (
    typeof config.mountPath !== "string" ||
    !config.mountPath.startsWith("/") ||
    (config.mountPath !== "/" && config.mountPath.endsWith("/"))
  ) {
    throw new Error(`Lighthouse mount path is invalid for ${config.site ?? configPath}`);
  }
  const outputRoot = path.resolve(ROOT, config.outputRoot);
  if (!fs.existsSync(outputRoot))
    throw new Error(`${config.outputRoot} is missing; run npm run build first`);
  const launchChrome = () =>
    launch({
      chromeFlags: ["--headless", "--no-sandbox", "--disable-gpu"],
      connectionPollInterval: 500,
      maxConnectionRetries: 20,
    });
  // Both the very first launch and every in-loop relaunch go through the
  // same retrying launcher: chrome-launcher's own connection budget
  // (maxConnectionRetries x connectionPollInterval, ~10s here) can still
  // reject on a slow or crash-looping runner, and that rejection must be
  // retried and accounted for exactly like a mid-run connection drop.
  const startChrome = () => launchChromeWithRetries({ launchChrome });
  const runs = await withLighthouseResources({
    startServer: () => serveStatic(outputRoot, config.mountPath),
    startChrome,
    run: async ({ server, chrome, startChrome: relaunchChrome, setChrome }) => {
      const address = server.address();
      if (!address || typeof address === "string") {
        throw new Error("static server has no TCP port");
      }
      const outputDir = path.join(ROOT, ".lighthouseci", config.site);
      fs.rmSync(outputDir, { recursive: true, force: true });
      fs.mkdirSync(outputDir, { recursive: true });
      const url = config.url.replace("{PORT}", String(address.port));
      return collectLighthouseRuns({
        config,
        url,
        outputDir,
        chrome,
        startChrome: relaunchChrome,
        setChrome,
        runLighthouse: (runUrl, options) =>
          lighthouse(runUrl, {
            ...options,
            output: "json",
            logLevel: "error",
            onlyCategories: ["performance"],
          }),
      });
    },
  });
  const verified = verifyLighthouseRuns(config, runs);
  process.stdout.write(
    `${config.site}: five-run median performance=${verified.performance.toFixed(2)}, total=${verified.totalByteWeight}, scripts=${verified.scriptTransferBytes}\n`,
  );
}

if (process.argv[1] && path.resolve(process.argv[1]) === SCRIPT_FILE) {
  const configPath = process.argv[2];
  if (!configPath) throw new Error("usage: node scripts/lighthouse-budget.mjs <config.cjs>");
  await run(configPath);
}
