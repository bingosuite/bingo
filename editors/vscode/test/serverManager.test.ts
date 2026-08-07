import assert from "node:assert/strict";
import { describe, it } from "node:test";

import type { BingoServerConfiguration } from "../src/configuration.js";
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
  readonly setProbe: (probe: HealthProbe) => void;
  readonly outcome: (outcome: ServerProcessOutcome) => void;
  readonly stoppedObserving: () => boolean;
}

function harness(initialProbe: HealthProbe): Harness {
  let clock = 0;
  let probe = initialProbe;
  let reportOutcome: ((outcome: ServerProcessOutcome) => void) | undefined;
  let observationStopped = false;
  const requests: SpawnServerRequest[] = [];
  const logs: string[] = [];
  const delays: number[] = [];
  const dependencies: ServerManagerDependencies = {
    probe: (...args) => probe(...args),
    resolveBinary: () => Promise.resolve("/extension/bin/bingo"),
    spawnServer: (request, onOutcome) => {
      requests.push(request);
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
  it("reuses a compatible server without spawning", async () => {
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

  it("rejects remote auto mode without probing or spawning", () => {
    let probes = 0;
    const test = harness(() => {
      probes += 1;
      return Promise.resolve(compatible);
    });
    assert.throws(
      () => test.manager.ensureServer(
        configuration({
          managementEndpoint: { host: "remote.example", port: 6060 },
        }),
      ),
      hasCode("invalidConfiguration"),
    );
    assert.equal(probes, 0);
    assert.equal(test.requests.length, 0);
  });

  it("connect-only bypasses platform, probing, binary checks, and spawning", async () => {
    const test = harness(() =>
      Promise.reject(new Error("probe must not run")),
    );
    const config = configuration({
      mode: "connectOnly",
      managementEndpoint: { host: "remote.example", port: 16060 },
      dapEndpoint: { host: "remote.example", port: 14711 },
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
    const test = harness((_management, _dap, timeoutMs) => {
      timeouts.push(timeoutMs);
      return Promise.resolve(absent);
    });

    await assert.rejects(
      test.manager.ensureServer(configuration({ readyTimeoutMs: 100 })),
      hasCode("readinessTimedOut"),
    );
    assert.deepEqual(timeouts, [100, 50, 1]);
    assert.deepEqual(test.delays, [50, 49, 1]);
    assert.equal(test.now(), 100);
    assert.equal(test.delays.every((delay) => delay > 0), true);
    assert.equal(test.requests.length, 1);
  });

  it("accepts a server that becomes healthy just before the minimum deadline", async () => {
    const timeouts: number[] = [];
    const test = harness((_management, _dap, timeoutMs) => {
      timeouts.push(timeoutMs);
      return Promise.resolve(
        timeouts.length === 3 ? compatible : absent,
      );
    });

    await test.manager.ensureServer(
      configuration({ readyTimeoutMs: 100 }),
    );
    assert.deepEqual(timeouts, [100, 50, 1]);
    assert.deepEqual(test.delays, [50, 49]);
    assert.equal(test.now(), 99);
  });

  it("passes the remaining readiness deadline to each health probe", async () => {
    const timeouts: number[] = [];
    const test = harness((_management, _dap, timeoutMs) => {
      timeouts.push(timeoutMs);
      return Promise.resolve(absent);
    });
    await assert.rejects(
      test.manager.ensureServer(configuration({ readyTimeoutMs: 250 })),
      hasCode("readinessTimedOut"),
    );
    assert.deepEqual(timeouts, [250, 150, 50, 1]);
    assert.deepEqual(test.delays, [100, 100, 49, 1]);
    assert.equal(test.now(), 250);
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
    assert.deepEqual(delays, [50, 49]);
  });

  it("fails safely on an incompatible endpoint without spawning", async () => {
    const test = harness(
      sequence({ kind: "incompatible", reason: "not bingo" }),
    );
    await assert.rejects(
      test.manager.ensureServer(configuration()),
      hasCode("endpointOccupied"),
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
