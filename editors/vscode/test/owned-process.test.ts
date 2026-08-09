import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import process from "node:process";
import { describe, it, type TestContext } from "node:test";

import {
  observeOwnedProcess,
  terminateOwnedProcessGroup,
  type OwnedProcess,
  type OwnedProcessOutcome,
  type ProcessSignal,
} from "../scripts/owned-process.mjs";

describe("test-owned process cleanup", () => {
  it(
    "removes an uncooperative detached parent and its inherited child",
    { timeout: 5000 },
    async (context) => {
      const fixture = await spawnParentWithChild(context, [
        'process.on("SIGTERM", () => {});',
        "setInterval(() => {}, 1000);",
      ]);

      const outcome = await terminateOwnedProcessGroup(fixture.ownedProcess, {
        gracefulTimeoutMs: 25,
        exitTimeoutMs: 2000,
        groupTimeoutMs: 2000,
        pollIntervalMs: 5,
      });

      assert.deepEqual(outcome, {
        kind: "exit",
        code: null,
        signal: "SIGKILL",
      });
      assert.equal(processExists(fixture.childPid), false);
    },
  );

  it(
    "removes an inherited child after the detached parent exits first",
    { timeout: 5000 },
    async (context) => {
      const fixture = await spawnParentWithChild(context, ["process.exit(23);"]);
      const parentOutcome = await fixture.ownedProcess.outcome;
      assert.deepEqual(parentOutcome, {
        kind: "exit",
        code: 23,
        signal: null,
      });
      assert.equal(processExists(fixture.childPid), true);

      const cleanupOutcome = await terminateOwnedProcessGroup(
        fixture.ownedProcess,
        {
          gracefulTimeoutMs: 10,
          exitTimeoutMs: 2000,
          groupTimeoutMs: 2000,
          pollIntervalMs: 5,
        },
      );

      assert.deepEqual(cleanupOutcome, parentOutcome);
      assert.equal(processExists(fixture.childPid), false);
    },
  );

  it(
    "does not signal after a detached process exits without descendants",
    { timeout: 5000 },
    async () => {
      const calls: Array<{
        readonly pid: number;
        readonly signal: NodeJS.Signals | number;
      }> = [];
      const signalProcess: ProcessSignal = (pid, signal) => {
        calls.push({ pid, signal });
        return process.kill(pid, signal);
      };
      const child = spawn(process.execPath, ["-e", "process.exit(19)"], {
        detached: true,
        stdio: "ignore",
      });
      const ownedProcess = observeOwnedProcess(child, signalProcess);
      const outcome = await ownedProcess.outcome;
      assert.deepEqual(outcome, {
        kind: "exit",
        code: 19,
        signal: null,
      });
      calls.length = 0;

      const cleanupOutcome = await terminateOwnedProcessGroup(ownedProcess, {
        gracefulTimeoutMs: 1,
        exitTimeoutMs: 100,
        groupTimeoutMs: 100,
        pollIntervalMs: 1,
      });

      assert.deepEqual(cleanupOutcome, outcome);
      assert.deepEqual(calls, []);
    },
  );

  it("escalates only after the graceful timeout", async () => {
    const controlled = controlledOwnedProcess(4321);

    await terminateOwnedProcessGroup(controlled.ownedProcess, {
      gracefulTimeoutMs: 1,
      exitTimeoutMs: 100,
      groupTimeoutMs: 100,
      pollIntervalMs: 1,
    });

    assert.deepEqual(controlled.signals, [
      { pid: 4321, signal: "SIGTERM" },
      { pid: -4321, signal: "SIGKILL" },
    ]);
  });

  it("surfaces unexpected cleanup signal failures", async () => {
    const controlled = controlledOwnedProcess(
      4321,
      new Error("permission denied"),
    );

    await assert.rejects(
      terminateOwnedProcessGroup(controlled.ownedProcess),
      /permission denied/,
    );
  });
});

async function spawnParentWithChild(
  context: TestContext,
  afterSpawn: readonly string[],
): Promise<{
  readonly childPid: number;
  readonly ownedProcess: OwnedProcess;
}> {
  const parent = spawn(
    process.execPath,
    [
      "-e",
      [
        'const { spawn } = require("node:child_process");',
        'const child = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)"], { stdio: "ignore" });',
        'process.stdout.write(`${String(child.pid)}\\n`);',
        ...afterSpawn,
      ].join("\n"),
    ],
    {
      detached: true,
      stdio: ["ignore", "pipe", "ignore"],
    },
  );
  const ownedProcess = observeOwnedProcess(parent);
  const stdout = parent.stdout;
  if (stdout === null) {
    throw new Error("detached test parent has no stdout");
  }
  const chunk = await new Promise<Buffer>((resolve, reject) => {
    stdout.once("data", (data: Buffer) => {
      resolve(data);
    });
    stdout.once("error", reject);
  });
  const childPid = Number(String(chunk).trim());
  assert.ok(Number.isSafeInteger(childPid) && childPid > 0);
  context.after(async () => {
    await stopFixture(parent, ownedProcess, childPid);
  });
  return { childPid, ownedProcess };
}

async function stopFixture(
  parent: ChildProcess,
  ownedProcess: OwnedProcess,
  childPid: number,
): Promise<void> {
  if (!ownedProcess.isFinished() && parent.pid !== undefined) {
    sendIfPresent(parent.pid, "SIGKILL");
  }
  sendIfPresent(childPid, "SIGKILL");
  if (!ownedProcess.isFinished()) {
    await Promise.race([
      ownedProcess.outcome,
      new Promise((resolve) => {
        setTimeout(resolve, 1000);
      }),
    ]);
  }
}

function controlledOwnedProcess(
  pid: number,
  leaderSignalError?: Error,
): {
  readonly ownedProcess: OwnedProcess;
  readonly signals: Array<{
    readonly pid: number;
    readonly signal: NodeJS.Signals;
  }>;
} {
  let finished = false;
  let groupAlive = true;
  let resolveOutcome: ((outcome: OwnedProcessOutcome) => void) | undefined;
  const outcome = new Promise<OwnedProcessOutcome>((resolve) => {
    resolveOutcome = resolve;
  });
  const signals: Array<{
    readonly pid: number;
    readonly signal: NodeJS.Signals;
  }> = [];
  return {
    ownedProcess: {
      pid,
      outcome,
      hasProcessGroup: () => groupAlive,
      isFinished: () => finished,
      ref() {},
      signalGroup(signal) {
        signals.push({ pid: -pid, signal });
        groupAlive = false;
        finished = true;
        resolveOutcome?.({
          kind: "exit",
          code: null,
          signal: "SIGKILL",
        });
        return true;
      },
      signalLeader(signal) {
        if (leaderSignalError !== undefined) {
          throw leaderSignalError;
        }
        signals.push({ pid, signal });
        return true;
      },
    },
    signals,
  };
}

function sendIfPresent(pid: number, signal: NodeJS.Signals): void {
  try {
    process.kill(pid, signal);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ESRCH") {
      throw error;
    }
  }
}

function processExists(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ESRCH") {
      return false;
    }
    throw error;
  }
}
