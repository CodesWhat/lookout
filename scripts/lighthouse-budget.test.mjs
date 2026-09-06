import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  collectLighthouseRuns,
  isChromeConnectionError,
  launchChromeWithRetries,
  median,
  resolveChromeAttempts,
  verifyLighthouseRuns,
  withLighthouseResources,
} from "./lighthouse-budget.mjs";

function fixtureLhr({ performance, totalByteWeight, scriptTransferBytes }) {
  return {
    categories: { performance: { score: performance } },
    audits: {
      "total-byte-weight": { numericValue: totalByteWeight },
      "resource-summary": {
        details: { items: [{ resourceType: "script", transferSize: scriptTransferBytes }] },
      },
    },
  };
}

function fakeChrome(port) {
  return { port, kill: async () => {} };
}

test("median is stable for unsorted odd and even inputs", () => {
  assert.equal(median([5, 1, 3]), 3);
  assert.equal(median([4, 1, 3, 2]), 2.5);
  assert.throws(() => median([]), /at least one value/);
});

test("Lighthouse budgets use the median and fail closed", () => {
  const config = {
    site: "fixture",
    performanceMin: 0.9,
    totalByteWeightMax: 1_000,
    scriptTransferBytesMax: 500,
  };
  const good = [
    { performance: 0.91, totalByteWeight: 900, scriptTransferBytes: 400 },
    { performance: 0.93, totalByteWeight: 800, scriptTransferBytes: 300 },
    { performance: 0.89, totalByteWeight: 1_200, scriptTransferBytes: 600 },
  ];
  assert.deepEqual(verifyLighthouseRuns(config, good), {
    performance: 0.91,
    totalByteWeight: 900,
    scriptTransferBytes: 400,
  });
  assert.throws(
    () => verifyLighthouseRuns(config, good.slice(0, 1).concat(good.slice(2))),
    /total byte weight budget exceeded/,
  );
  assert.throws(() => verifyLighthouseRuns(config, []), /expected Lighthouse runs/);
});

test("Lighthouse resource cleanup closes the server across setup failures", async () => {
  let serverCloseCount = 0;
  const server = {
    close(callback) {
      serverCloseCount += 1;
      callback();
    },
  };

  await assert.rejects(
    withLighthouseResources({
      startServer: async () => server,
      startChrome: async () => {
        throw new Error("fixture Chrome launch failed");
      },
      run: async () => assert.fail("runs must not start after Chrome launch fails"),
    }),
    /fixture Chrome launch failed/,
  );
  assert.equal(serverCloseCount, 1);
});

test("Lighthouse resource cleanup closes the server when Chrome cleanup fails", async () => {
  let serverCloseCount = 0;
  const server = {
    close(callback) {
      serverCloseCount += 1;
      callback();
    },
  };

  await assert.rejects(
    withLighthouseResources({
      startServer: async () => server,
      startChrome: async () => ({
        kill: async () => {
          throw new Error("fixture Chrome cleanup failed");
        },
      }),
      run: async () => {
        throw new Error("fixture report setup failed");
      },
    }),
    /fixture Chrome cleanup failed/,
  );
  assert.equal(serverCloseCount, 1);
});

test("isChromeConnectionError matches Chrome/connection failures and not budget failures", () => {
  const econnrefused = Object.assign(new Error("connect ECONNREFUSED 127.0.0.1:41695"), {
    code: "ECONNREFUSED",
  });
  assert.ok(isChromeConnectionError(econnrefused));
  assert.ok(isChromeConnectionError(new Error("Unable to connect to Chrome")));
  assert.ok(isChromeConnectionError(new Error("Chrome has crashed")));
  assert.ok(
    isChromeConnectionError(Object.assign(new Error("connection reset"), { code: "ECONNRESET" })),
  );
  assert.ok(isChromeConnectionError(new Error("read ECONNRESET")));
  assert.ok(isChromeConnectionError(new Error("socket hang up")));
  assert.ok(
    !isChromeConnectionError(new Error("marketing performance budget exceeded: 0.5 < 0.66")),
  );
  assert.ok(!isChromeConnectionError(new Error("Lighthouse runtime error: NO_FCP")));
  assert.ok(!isChromeConnectionError(new Error("Lighthouse runtime error: TARGET_CRASHED")));
  assert.ok(
    !isChromeConnectionError(
      new Error("Lighthouse report is missing required performance metrics"),
    ),
  );
  assert.ok(!isChromeConnectionError(undefined));
});

test("isChromeConnectionError walks error.cause to find a wrapped connection failure", () => {
  // puppeteer-core 25.7.0's getWSEndpoint rewrites a dead debugging port into
  // a plain TypeError and only keeps the real ECONNREFUSED on `.cause`.
  const wrapped = Object.assign(
    new TypeError(
      "Failed to fetch browser webSocket URL from http://127.0.0.1:41695/json/version: fetch failed",
    ),
    {
      cause: Object.assign(new Error("connect ECONNREFUSED 127.0.0.1:41695"), {
        code: "ECONNREFUSED",
      }),
    },
  );
  assert.ok(isChromeConnectionError(wrapped));
  assert.ok(isChromeConnectionError(new Error("Protocol error (Page.navigate): Target closed")));
  assert.ok(
    isChromeConnectionError(new Error("Session closed. Most likely the page has been closed.")),
  );
});

test("isChromeConnectionError is cycle-safe and does not hang on a circular cause chain", () => {
  const a = new Error("a");
  const b = new Error("b");
  a.cause = b;
  b.cause = a;
  assert.ok(!isChromeConnectionError(a));
});

test("resolveChromeAttempts defaults to 3 and validates its env override", () => {
  assert.equal(resolveChromeAttempts({}), 3);
  assert.equal(resolveChromeAttempts({ LIGHTHOUSE_CHROME_ATTEMPTS: "" }), 3);
  assert.equal(resolveChromeAttempts({ LIGHTHOUSE_CHROME_ATTEMPTS: "5" }), 5);
  assert.equal(resolveChromeAttempts({ LIGHTHOUSE_CHROME_ATTEMPTS: "10" }), 10);
  assert.throws(
    () => resolveChromeAttempts({ LIGHTHOUSE_CHROME_ATTEMPTS: "0" }),
    /LIGHTHOUSE_CHROME_ATTEMPTS/,
  );
  assert.throws(
    () => resolveChromeAttempts({ LIGHTHOUSE_CHROME_ATTEMPTS: "11" }),
    /LIGHTHOUSE_CHROME_ATTEMPTS/,
  );
  assert.throws(
    () => resolveChromeAttempts({ LIGHTHOUSE_CHROME_ATTEMPTS: "9".repeat(400) }),
    /LIGHTHOUSE_CHROME_ATTEMPTS/,
  );
  assert.throws(
    () => resolveChromeAttempts({ LIGHTHOUSE_CHROME_ATTEMPTS: "-1" }),
    /LIGHTHOUSE_CHROME_ATTEMPTS/,
  );
  assert.throws(
    () => resolveChromeAttempts({ LIGHTHOUSE_CHROME_ATTEMPTS: "abc" }),
    /LIGHTHOUSE_CHROME_ATTEMPTS/,
  );
  assert.throws(
    () => resolveChromeAttempts({ LIGHTHOUSE_CHROME_ATTEMPTS: "1.5" }),
    /LIGHTHOUSE_CHROME_ATTEMPTS/,
  );
});

test("launchChromeWithRetries relaunches after a connection failure and returns the working handle", async () => {
  const logs = [];
  let calls = 0;
  const chrome = await launchChromeWithRetries({
    launchChrome: async () => {
      calls += 1;
      if (calls === 1) {
        const error = new Error("connect ECONNREFUSED 127.0.0.1:9999");
        error.code = "ECONNREFUSED";
        throw error;
      }
      return { port: 2, kill: async () => {} };
    },
    maxAttempts: 3,
    log: (message) => logs.push(message),
  });
  assert.equal(chrome.port, 2);
  assert.equal(calls, 2);
  assert.equal(logs.length, 1);
  assert.match(
    logs[0],
    /^chrome launch attempt 1\/3 failed \(connect ECONNREFUSED 127\.0\.0\.1:9999\), relaunching\n$/,
  );
});

test("launchChromeWithRetries gives up and names Chrome after every attempt fails", async () => {
  await assert.rejects(
    launchChromeWithRetries({
      launchChrome: async () => {
        const error = new Error("connect ECONNREFUSED 127.0.0.1:9999");
        error.code = "ECONNREFUSED";
        throw error;
      },
      maxAttempts: 3,
      log: () => {},
    }),
    /chrome failed to launch 3 time\(s\) in a row/,
  );
});

test("launchChromeWithRetries does not retry a non-connection launch failure", async () => {
  let calls = 0;
  await assert.rejects(
    launchChromeWithRetries({
      launchChrome: async () => {
        calls += 1;
        throw new Error("ChromeNotInstalledError");
      },
      maxAttempts: 3,
      log: () => assert.fail("must not log a retry for a non-connection error"),
    }),
    /ChromeNotInstalledError/,
  );
  assert.equal(calls, 1);
});

test("collectLighthouseRuns retries a run after Chrome drops the connection", async () => {
  const outputDir = fs.mkdtempSync(path.join(os.tmpdir(), "portwing-lighthouse-retry-"));
  try {
    const config = { site: "fixture", numberOfRuns: 1 };
    const logs = [];
    const killedPorts = [];
    let lighthouseCalls = 0;
    let startChromeCalls = 0;
    const runs = await collectLighthouseRuns({
      config,
      url: "http://127.0.0.1:0/",
      outputDir,
      chrome: { port: 1, kill: async () => killedPorts.push(1) },
      startChrome: async () => {
        startChromeCalls += 1;
        const port = startChromeCalls + 1;
        return { port, kill: async () => killedPorts.push(port) };
      },
      setChrome: () => {},
      runLighthouse: async () => {
        lighthouseCalls += 1;
        if (lighthouseCalls === 1) {
          const error = new Error("connect ECONNREFUSED 127.0.0.1:41695");
          error.code = "ECONNREFUSED";
          throw error;
        }
        return {
          lhr: fixtureLhr({ performance: 0.95, totalByteWeight: 900, scriptTransferBytes: 400 }),
        };
      },
      maxAttempts: 3,
      log: (message) => logs.push(message),
    });
    assert.deepEqual(runs, [{ performance: 0.95, totalByteWeight: 900, scriptTransferBytes: 400 }]);
    assert.equal(lighthouseCalls, 2);
    assert.equal(startChromeCalls, 1);
    assert.deepEqual(killedPorts, [1]);
    assert.equal(logs.length, 1);
    assert.match(
      logs[0],
      /^run 1\/1 attempt 1\/3: chrome connection lost \(connect ECONNREFUSED 127\.0\.0\.1:41695\), relaunching\n$/,
    );
  } finally {
    fs.rmSync(outputDir, { recursive: true, force: true });
  }
});

test("collectLighthouseRuns exits fail-closed and names Chrome when every attempt drops the connection", async () => {
  const outputDir = fs.mkdtempSync(path.join(os.tmpdir(), "portwing-lighthouse-exhausted-"));
  try {
    const config = { site: "fixture", numberOfRuns: 5 };
    const logs = [];
    let lighthouseCalls = 0;
    let startChromeCalls = 0;
    await assert.rejects(
      collectLighthouseRuns({
        config,
        url: "http://127.0.0.1:0/",
        outputDir,
        chrome: fakeChrome(1),
        startChrome: async () => {
          startChromeCalls += 1;
          return fakeChrome(startChromeCalls + 1);
        },
        setChrome: () => {},
        runLighthouse: async () => {
          lighthouseCalls += 1;
          const error = new Error("connect ECONNREFUSED 127.0.0.1:41695");
          error.code = "ECONNREFUSED";
          throw error;
        },
        maxAttempts: 3,
        log: (message) => logs.push(message),
      }),
      /run 1\/5 lost its Chrome connection 3 time\(s\) in a row/,
    );
    assert.equal(lighthouseCalls, 3);
    assert.equal(startChromeCalls, 2);
    assert.equal(logs.length, 2);
    assert.match(logs[0], /^run 1\/5 attempt 1\/3: chrome connection lost/);
    assert.match(logs[1], /^run 1\/5 attempt 2\/3: chrome connection lost/);
  } finally {
    fs.rmSync(outputDir, { recursive: true, force: true });
  }
});

test("collectLighthouseRuns does not retry a budget failure", async () => {
  const outputDir = fs.mkdtempSync(path.join(os.tmpdir(), "portwing-lighthouse-budget-"));
  try {
    const config = { site: "fixture", numberOfRuns: 1 };
    let lighthouseCalls = 0;
    await assert.rejects(
      collectLighthouseRuns({
        config,
        url: "http://127.0.0.1:0/",
        outputDir,
        chrome: fakeChrome(1),
        startChrome: async () => assert.fail("must not relaunch Chrome for a non-connection error"),
        setChrome: () => {},
        runLighthouse: async () => {
          lighthouseCalls += 1;
          throw new Error("Lighthouse runtime error: NO_FCP");
        },
        maxAttempts: 3,
      }),
      /Lighthouse runtime error: NO_FCP/,
    );
    assert.equal(lighthouseCalls, 1);
  } finally {
    fs.rmSync(outputDir, { recursive: true, force: true });
  }
});

test("collectLighthouseRuns refuses to run with zero attempts configured", async () => {
  const outputDir = fs.mkdtempSync(path.join(os.tmpdir(), "portwing-lighthouse-zero-"));
  try {
    await assert.rejects(
      collectLighthouseRuns({
        config: { site: "fixture", numberOfRuns: 1 },
        url: "http://127.0.0.1:0/",
        outputDir,
        chrome: fakeChrome(1),
        startChrome: async () =>
          assert.fail("must not launch Chrome with zero attempts configured"),
        setChrome: () => {},
        runLighthouse: async () =>
          assert.fail("must not run Lighthouse with zero attempts configured"),
        maxAttempts: 0,
      }),
      /at least one attempt/,
    );
  } finally {
    fs.rmSync(outputDir, { recursive: true, force: true });
  }
});

test("collectLighthouseRuns names Chrome and the run index when the relaunch itself fails", async () => {
  const outputDir = fs.mkdtempSync(path.join(os.tmpdir(), "portwing-lighthouse-relaunch-"));
  try {
    const config = { site: "fixture", numberOfRuns: 5 };
    await assert.rejects(
      collectLighthouseRuns({
        config,
        url: "http://127.0.0.1:0/",
        outputDir,
        chrome: fakeChrome(1),
        startChrome: async () => {
          const error = new Error("connect ECONNREFUSED 127.0.0.1:9999");
          error.code = "ECONNREFUSED";
          throw error;
        },
        setChrome: () => {},
        runLighthouse: async () => {
          const error = new Error("connect ECONNREFUSED 127.0.0.1:41695");
          error.code = "ECONNREFUSED";
          throw error;
        },
        maxAttempts: 3,
        log: () => {},
      }),
      /run 1\/5 could not relaunch Chrome after attempt 1\/3/,
    );
  } finally {
    fs.rmSync(outputDir, { recursive: true, force: true });
  }
});

test("collectLighthouseRuns records the relaunched Chrome handle via setChrome", async () => {
  const outputDir = fs.mkdtempSync(path.join(os.tmpdir(), "portwing-lighthouse-setchrome-"));
  try {
    const config = { site: "fixture", numberOfRuns: 1 };
    const recorded = [];
    let lighthouseCalls = 0;
    const runs = await collectLighthouseRuns({
      config,
      url: "http://127.0.0.1:0/",
      outputDir,
      chrome: fakeChrome(1),
      startChrome: async () => fakeChrome(7),
      setChrome: (chrome) => recorded.push(chrome),
      runLighthouse: async () => {
        lighthouseCalls += 1;
        if (lighthouseCalls === 1) {
          const error = new Error("connect ECONNREFUSED 127.0.0.1:41695");
          error.code = "ECONNREFUSED";
          throw error;
        }
        return {
          lhr: fixtureLhr({ performance: 0.95, totalByteWeight: 900, scriptTransferBytes: 400 }),
        };
      },
      maxAttempts: 3,
      log: () => {},
    });
    assert.equal(runs.length, 1);
    assert.equal(recorded.length, 1);
    assert.equal(recorded[0].port, 7);
  } finally {
    fs.rmSync(outputDir, { recursive: true, force: true });
  }
});

test("withLighthouseResources wires startChrome and setChrome through to run() and kills the current handle on exit", async () => {
  let killedTwelve = false;
  const server = {
    close(callback) {
      callback();
    },
  };
  await assert.rejects(
    withLighthouseResources({
      startServer: async () => server,
      startChrome: async () => ({ port: 11, kill: async () => {} }),
      run: async (args) => {
        assert.equal(typeof args.startChrome, "function");
        assert.equal(typeof args.setChrome, "function");
        args.setChrome({
          port: 12,
          kill: async () => {
            killedTwelve = true;
          },
        });
        throw new Error("fixture run failed after handoff");
      },
    }),
    /fixture run failed after handoff/,
  );
  assert.equal(killedTwelve, true);
});
