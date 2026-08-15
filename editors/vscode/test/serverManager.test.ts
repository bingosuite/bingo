import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  resolveServerConfiguration,
  type BingoServerConfiguration,
} from "../src/configuration.js";
import type {
  HealthProbe,
  HealthProbeResult,
} from "../src/health.js";
import {
  ServerManager,
  ServerManagerError,
  type ServerManagerDependencies,
} from "../src/serverManager.js";
import type {
  ServerProcessOutcome,
  SpawnServerRequest,
} from "../src/serverProcess.js";

const compatible: HealthProbeResult = {
  kind: "compatible",
  health: {
    instanceId: "winner",
    dapAddress: "127.0.0.1:4711",
    dapSessionEventVersion: 1,
  },
};
const absent: HealthProbeResult = { kind: "absent" };

function configuration(
  overrides: Partial<BingoServerConfiguration> = {},
): BingoServerConfiguration {
  return {
    mode: "auto",
    managementEndpoint: { host: "127.0.0.1", port: 6060 },
    dapEndpoint: { host: "127.0.0.1", port: 4711 },
    readyTimeoutMs: 5000,
    idleTimeoutMs: 30000,
    ...overrides,
  };
}

interface Harness {
  readonly manager: ServerManager;
  readonly requests: SpawnServerRequest[];
  readonly logs: string[];
  readonly delays: number[];
  readonly now: () => number;
  readonly spawnedAt: () => number;
  readonly setProbe: (probe: HealthProbe) => void;
  readonly outcome: (outcome: ServerProcessOutcome) => void;
  readonly stoppedObserving: () => boolean;
}

interface HarnessOptions {
  readonly probeElapsedMs?: (
    timeoutMs: number,
    callIndex: number,
  ) => number;
}

function harness(
  initialProbe: HealthProbe,
  options: HarnessOptions = {},
): Harness {
  let clock = 0;
  let probeCalls = 0;
  let probe = initialProbe;
  let reportOutcome: ((outcome: ServerProcessOutcome) => void) | undefined;
  let observationStopped = false;
  let spawnTime = -1;
  const requests: SpawnServerRequest[] = [];
  const logs: string[] = [];
  const delays: number[] = [];
  const dependencies: ServerManagerDependencies = {
    probe: async (...args) => {
      const result = await probe(...args);
      const elapsed = options.probeElapsedMs?.(args[2], probeCalls) ?? 0;
      probeCalls += 1;
      clock += Math.min(args[2], Math.max(0, elapsed));
      return result;
    },
    resolveBinary: () => Promise.resolve("/extension/bin/bingo"),
    spawnServer: (request, onOutcome) => {
      requests.push(request);
      spawnTime = clock;
      reportOutcome = onOutcome;
      return {
        stopObserving(): void {
          observationStopped = true;
        },
      };
    },
    delay: (milliseconds, signal) => {
      if (signal.aborted) {
        const error = new Error("aborted");
        error.name = "AbortError";
        return Promise.reject(error);
      }
      delays.push(milliseconds);
      clock += milliseconds;
      return Promise.resolve();
    },
    now: () => clock,
    runtime: { platform: "darwin", arch: "arm64" },
    logPathFor: () => Promise.resolve("/logs/bingo-6060.log"),
    log: (message) => {
      logs.push(message);
    },
  };
  return {
    manager: new ServerManager(dependencies),
    requests,
    logs,
    delays,
    now: () => clock,
    spawnedAt: () => spawnTime,
    setProbe(next) {
      probe = next;
    },
    outcome(value) {
      assert.notEqual(reportOutcome, undefined);
      reportOutcome?.(value);
    },
    stoppedObserving() {
      return observationStopped;
    },
  };
}

function sequence(...results: HealthProbeResult[]): HealthProbe {
  let index = 0;
  return () => {
    const result = results[Math.min(index, results.length - 1)];
    index += 1;
    assert.notEqual(result, undefined);
    return Promise.resolve(result as HealthProbeResult);
  };
}

describe("server manager", () => {
  it("reuses a session-discovery-compatible server without spawning", async () => {
    const test = harness(sequence(compatible));
    assert.deepEqual(
      await test.manager.ensureServer(configuration()),
      { host: "127.0.0.1", port: 4711 },
    );
    assert.equal(test.requests.length, 0);
  });

  it("spawns an absent server with exact arguments and waits for readiness", async () => {
    const test = harness(sequence(absent, compatible));
    await test.manager.ensureServer(configuration());

    assert.deepEqual(test.requests, [
      {
        binaryPath: "/extension/bin/bingo",
        args: [
          "-addr",
          "127.0.0.1:6060",
          "-dap-addr",
          "127.0.0.1:4711",
          "-idle-timeout",
          "30000ms",
        ],
        logPath: "/logs/bingo-6060.log",
      },
    ]);
    assert.equal(test.stoppedObserving(), true);
  });

  it("coalesces simultaneous ensures for one endpoint", async () => {
    let release: ((result: HealthProbeResult) => void) | undefined;
    let probes = 0;
    const pending = new Promise<HealthProbeResult>((resolve) => {
      release = resolve;
    });
    const test = harness(() => {
      probes += 1;
      return Promise.resolve(pending);
    });

    const first = test.manager.ensureServer(configuration());
    const second = test.manager.ensureServer(configuration());
    assert.equal(probes, 1);
    release?.(compatible);
    await Promise.all([first, second]);
    assert.equal(probes, 1);
  });

  it("clears a failed ensure so a retry can succeed", async () => {
    const test = harness(
      sequence(
        { kind: "incompatible", reason: "wrong service" },
        compatible,
      ),
    );
    await assert.rejects(
      test.manager.ensureServer(configuration()),
      hasCode("endpointOccupied"),
    );
    await test.manager.ensureServer(configuration());
  });

  it("rejects normalized identical auto endpoints without probing or spawning", () => {
    let probes = 0;
    const test = harness(() => {
      probes += 1;
      return Promise.resolve(compatible);
    });
    const config = resolveServerConfiguration({
      managementHost: " 127.0.0.1 ",
      managementPort: 6060,
      dapHost: "127.0.0.1",
      dapPort: 6060,
    });
    assert.throws(
      () => test.manager.ensureServer(config),
      (error: unknown) =>
        error instanceof ServerManagerError &&
        error.code === "invalidConfiguration" &&
        /requires distinct management and DAP endpoints/.test(error.message),
    );
    assert.equal(probes, 0);
    assert.equal(test.requests.length, 0);
  });

  it("keeps literal auto-host validation ahead of endpoint equality", () => {
    let probes = 0;
    const test = harness(() => {
      probes += 1;
      return Promise.resolve(compatible);
    });
    assert.throws(
      () => test.manager.ensureServer(
        configuration({
          managementEndpoint: { host: "localhost", port: 6060 },
          dapEndpoint: { host: "localhost", port: 6060 },
        }),
      ),
      (error: unknown) =>
        error instanceof ServerManagerError &&
        error.code === "invalidConfiguration" &&
        /requires managementHost and dapHost to be 127\.0\.0\.1/.test(
          error.message,
        ),
    );
    assert.equal(probes, 0);
    assert.equal(test.requests.length, 0);
  });

  it("connect-only accepts identical custom endpoints without managed work", async () => {
    const test = harness(() =>
      Promise.reject(new Error("probe must not run")),
    );
    const config = configuration({
      mode: "connectOnly",
      managementEndpoint: { host: "remote.example", port: 16060 },
      dapEndpoint: { host: "remote.example", port: 16060 },
    });

    assert.deepEqual(await test.manager.ensureServer(config), config.dapEndpoint);
    assert.equal(test.requests.length, 0);
  });

  it("rejects unsupported local auto-start platforms directly", () => {
    const manager = new ServerManager({
      probe: () => Promise.reject(new Error("probe must not run")),
      resolveBinary: () => Promise.reject(new Error("binary must not run")),
      spawnServer: () => {
        throw new Error("spawn must not run");
      },
      delay: () => Promise.reject(new Error("delay must not run")),
      now: () => 0,
      runtime: { platform: "win32", arch: "x64" },
      logPathFor: () => Promise.reject(new Error("log must not run")),
      log() {},
    });

    assert.throws(
      () => manager.ensureServer(configuration()),
      hasCode("unsupportedAutoStart"),
    );
  });

  it("surfaces a missing bundled binary before spawning", async () => {
    const manager = new ServerManager({
      probe: sequence(absent),
      resolveBinary: () => Promise.reject(new Error("missing binary")),
      spawnServer: () => {
        throw new Error("spawn must not run");
      },
      delay: () => Promise.resolve(),
      now: () => 0,
      runtime: { platform: "darwin", arch: "arm64" },
      logPathFor: () => Promise.resolve("/logs/server.log"),
      log() {},
    });

    await assert.rejects(
      manager.ensureServer(configuration()),
      (error: unknown) =>
        error instanceof ServerManagerError &&
        error.code === "binaryUnavailable" &&
        /missing binary/.test(error.message),
    );
  });

  it("accepts a healthy winner after this child loses the listener race", async () => {
    const test = harness(sequence(absent, compatible));
    const ensured = test.manager.ensureServer(configuration());
    await waitForSpawn(test.requests);
    test.outcome({ kind: "exit", code: 1, signal: null });
    await ensured;
  });

  it("reports an early child failure if no winner becomes healthy", async () => {
    const test = harness(sequence(absent));
    const ensured = test.manager.ensureServer(
      configuration({ readyTimeoutMs: 200 }),
    );
    await waitForSpawn(test.requests);
    test.outcome({
      kind: "error",
      error: new Error("bind failed"),
    });
    await assert.rejects(
      ensured,
      (error: unknown) =>
        error instanceof ServerManagerError &&
        error.code === "spawnFailed" &&
        /bind failed/.test(error.message) &&
        /bingo-6060\.log/.test(error.message),
    );
  });

  it("reports a bounded readiness timeout while a child remains alive", async () => {
    const test = harness(sequence(absent));
    await assert.rejects(
      test.manager.ensureServer(
        configuration({ readyTimeoutMs: 200 }),
      ),
      hasCode("readinessTimedOut"),
    );
  });

  it("uses the full minimum readiness window for fast absent probes", async () => {
    const timeouts: number[] = [];
    const test = harness(
      (_management, _dap, timeoutMs) => {
        timeouts.push(timeoutMs);
        return Promise.resolve(absent);
      },
      { probeElapsedMs: () => 5 },
    );

    await assert.rejects(
      test.manager.ensureServer(configuration({ readyTimeoutMs: 100 })),
      hasCode("readinessTimedOut"),
    );
    assert.deepEqual(timeouts, [100, 100, 50, 35, 25]);
    assert.deepEqual(test.delays, [45, 10, 5, 20]);
    assert.equal(test.now() - test.spawnedAt(), 100);
    assert.equal(timeouts.every((timeout) => timeout >= 25), true);
    assert.equal(test.delays.every((delay) => delay > 0), true);
    assert.ok(timeouts.length <= 5);
    assert.equal(test.requests.length, 1);
  });

  it("accepts a server that becomes healthy during the final window", async () => {
    const timeouts: number[] = [];
    const test = harness(
      (_management, _dap, timeoutMs) => {
        timeouts.push(timeoutMs);
        return Promise.resolve(
          timeouts.length === 4 ? compatible : absent,
        );
      },
      { probeElapsedMs: () => 5 },
    );

    await test.manager.ensureServer(
      configuration({ readyTimeoutMs: 100 }),
    );
    assert.deepEqual(timeouts, [100, 100, 50, 35]);
    assert.deepEqual(test.delays, [45, 10]);
    assert.equal(test.now() - test.spawnedAt(), 70);
  });

  it("passes the remaining readiness deadline to each health probe", async () => {
    const timeouts: number[] = [];
    const test = harness(
      (_management, _dap, timeoutMs) => {
        timeouts.push(timeoutMs);
        return Promise.resolve(absent);
      },
      { probeElapsedMs: () => 5 },
    );
    await assert.rejects(
      test.manager.ensureServer(configuration({ readyTimeoutMs: 250 })),
      hasCode("readinessTimedOut"),
    );
    assert.deepEqual(timeouts, [250, 250, 145, 50, 35, 25]);
    assert.deepEqual(test.delays, [100, 90, 10, 5, 20]);
    assert.equal(test.now() - test.spawnedAt(), 250);
  });

  it("keeps a slow probe within the single readiness deadline", async () => {
    const timeouts: number[] = [];
    const test = harness(
      (_management, _dap, timeoutMs) => {
        timeouts.push(timeoutMs);
        return Promise.resolve(absent);
      },
      {
        probeElapsedMs: (timeoutMs, callIndex) =>
          callIndex === 0 ? 5 : timeoutMs,
      },
    );

    await assert.rejects(
      test.manager.ensureServer(configuration({ readyTimeoutMs: 100 })),
      hasCode("readinessTimedOut"),
    );
    assert.deepEqual(timeouts, [100, 100]);
    assert.deepEqual(test.delays, []);
    assert.equal(test.now() - test.spawnedAt(), 100);
  });

  it("cancels promptly during the final-window delay", async () => {
    let clock = 0;
    let delayCalls = 0;
    let markFinalDelayStarted: (() => void) | undefined;
    const finalDelayStarted = new Promise<void>((resolve) => {
      markFinalDelayStarted = resolve;
    });
    const delays: number[] = [];
    const manager = new ServerManager({
      probe: sequence(absent),
      resolveBinary: () => Promise.resolve("/extension/bin/bingo"),
      spawnServer: () => ({ stopObserving() {} }),
      delay: (milliseconds, signal) => {
        delays.push(milliseconds);
        delayCalls += 1;
        if (delayCalls === 1) {
          clock += milliseconds;
          return Promise.resolve();
        }
        return new Promise((_resolve, reject) => {
          const rejectCancelled = (): void => {
            const error = new Error("aborted");
            error.name = "AbortError";
            reject(error);
          };
          signal.addEventListener("abort", rejectCancelled, { once: true });
          markFinalDelayStarted?.();
        });
      },
      now: () => clock,
      runtime: { platform: "darwin", arch: "arm64" },
      logPathFor: () => Promise.resolve("/logs/bingo-6060.log"),
      log: () => {},
    });

    const ensured = manager.ensureServer(
      configuration({ readyTimeoutMs: 100 }),
    );
    await finalDelayStarted;
    manager.dispose();

    await assert.rejects(ensured, hasCode("cancelled"));
    assert.deepEqual(delays, [50, 10]);
  });

  it("treats an older server as an occupied endpoint without spawning", async () => {
    const test = harness(
      sequence({
        kind: "incompatible",
        reason: "DAP session event version is undefined, expected 1",
      }),
    );
    await assert.rejects(
      test.manager.ensureServer(configuration()),
      (error: unknown) =>
        error instanceof ServerManagerError &&
        error.code === "endpointOccupied" &&
        /DAP session event version is undefined, expected 1/.test(error.message),
    );
    assert.equal(test.requests.length, 0);
  });

  it("fails safely on a non-refused transport error without spawning", async () => {
    const test = harness(
      sequence({
        kind: "transportError",
        error: new Error("request timed out"),
      }),
    );
    await assert.rejects(
      test.manager.ensureServer(configuration()),
      hasCode("healthProbeFailed"),
    );
    assert.equal(test.requests.length, 0);
  });

  it("dispose cancels readiness and never exposes a kill operation", async () => {
    let markDelayStarted: (() => void) | undefined;
    const delayStarted = new Promise<void>((resolve) => {
      markDelayStarted = resolve;
    });
    const custom = new ServerManager({
      probe: sequence(absent),
      resolveBinary: () => Promise.resolve("/extension/bin/bingo"),
      spawnServer: () => ({ stopObserving() {} }),
      delay: (_milliseconds, signal) =>
        new Promise((_resolve, reject) => {
          markDelayStarted?.();
          const rejectCancelled = (): void => {
            const error = new Error("aborted");
            error.name = "AbortError";
            reject(error);
          };
          if (signal.aborted) {
            rejectCancelled();
            return;
          }
          signal.addEventListener("abort", rejectCancelled, { once: true });
        }),
      now: () => 0,
      runtime: { platform: "darwin", arch: "arm64" },
      logPathFor: () => Promise.resolve("/logs/server.log"),
      log() {},
    });
    const ensured = custom.ensureServer(configuration());
    await delayStarted;
    custom.dispose();
    await assert.rejects(ensured, hasCode("cancelled"));
  });

  for (const phase of ["binary", "log"] as const) {
    it(`dispose during ${phase} resolution prevents spawn`, async () => {
      const gate = deferred<string>();
      const entered = deferred<void>();
      let spawns = 0;
      const manager = new ServerManager({
        probe: sequence(absent),
        resolveBinary: () => {
          if (phase === "binary") {
            entered.resolve();
            return gate.promise;
          }
          return Promise.resolve("/extension/bin/bingo");
        },
        spawnServer: () => {
          spawns += 1;
          return { stopObserving() {} };
        },
        delay: () => Promise.resolve(),
        now: () => 0,
        runtime: { platform: "darwin", arch: "arm64" },
        logPathFor: () => {
          if (phase === "log") {
            entered.resolve();
            return gate.promise;
          }
          return Promise.resolve("/logs/server.log");
        },
        log() {},
      });

      const ensured = manager.ensureServer(configuration());
      await entered.promise;
      manager.dispose();
      gate.resolve(
        phase === "binary"
          ? "/extension/bin/bingo"
          : "/logs/server.log",
      );

      await assert.rejects(ensured, hasCode("cancelled"));
      assert.equal(spawns, 0);
    });
  }

  it("classifies abort during the initial probe as cancellation", async () => {
    const entered = deferred<void>();
    let spawns = 0;
    const manager = new ServerManager({
      probe: (_management, _dap, _timeout, signal) =>
        new Promise((_resolve, reject) => {
          entered.resolve();
          const rejectAbort = (): void => {
            const error = new Error("operation cancelled");
            error.name = "AbortError";
            reject(error);
          };
          if (signal.aborted) {
            rejectAbort();
            return;
          }
          signal.addEventListener("abort", rejectAbort, { once: true });
        }),
      resolveBinary: () => Promise.resolve("/extension/bin/bingo"),
      spawnServer: () => {
        spawns += 1;
        return { stopObserving() {} };
      },
      delay: () => Promise.resolve(),
      now: () => 0,
      runtime: { platform: "darwin", arch: "arm64" },
      logPathFor: () => Promise.resolve("/logs/server.log"),
      log() {},
    });

    const ensured = manager.ensureServer(configuration());
    await entered.promise;
    manager.dispose();

    await assert.rejects(ensured, hasCode("cancelled"));
    assert.equal(spawns, 0);
  });
});

function hasCode(code: ServerManagerError["code"]) {
  return (error: unknown): boolean =>
    error instanceof ServerManagerError && error.code === code;
}

async function waitForSpawn(requests: SpawnServerRequest[]): Promise<void> {
  while (requests.length === 0) {
    await Promise.resolve();
  }
}

function deferred<T>(): {
  readonly promise: Promise<T>;
  readonly resolve: (value: T) => void;
} {
  let resolvePromise: ((value: T) => void) | undefined;
  const promise = new Promise<T>((resolve) => {
    resolvePromise = resolve;
  });
  return {
    promise,
    resolve(value) {
      assert.notEqual(resolvePromise, undefined);
      resolvePromise?.(value);
    },
  };
}
