import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import process from "node:process";
import { describe, it } from "node:test";

import {
  observeOwnedProcess,
  terminateOwnedProcessGroup,
  type OwnedProcess,
  type OwnedProcessOutcome,
  type ProcessSignal,
} from "../scripts/owned-process.mjs";

describe("test-owned process cleanup", () => {
  it(
    "removes a detached parent and its inherited child",
    { timeout: 5000 },
    async (context) => {
      const parent = spawn(
        process.execPath,
        [
          "-e",
          [
            'const { spawn } = require("node:child_process");',
            'process.on("SIGTERM", () => {});',
            'const child = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)"], { stdio: "ignore" });',
            'process.stdout.write(`${String(child.pid)}\\n`);',
            "setInterval(() => {}, 1000);",
          ].join("\n"),
        ],
        {
          detached: true,
          stdio: ["ignore", "pipe", "ignore"],
        },
      );
      const ownedProcess = observeOwnedProcess(parent);
      context.after(async () => {
        const pid = ownedProcess.pid;
        if (pid === undefined || ownedProcess.isFinished()) {
          return;
        }
        try {
          process.kill(-pid, "SIGKILL");
        } catch (error) {
          if ((error as NodeJS.ErrnoException).code !== "ESRCH") {
            throw error;
          }
        }
        await ownedProcess.outcome;
      });

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

      const outcome = await terminateOwnedProcessGroup(ownedProcess, {
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
      assert.equal(processExists(childPid), false);
    },
  );

  it("escalates only after the graceful timeout", async () => {
    const controlled = controlledOwnedProcess(4321);
    const calls: Array<{
      readonly pid: number;
      readonly signal: NodeJS.Signals | number;
    }> = [];
    const signalProcess: ProcessSignal = (pid, signal) => {
      calls.push({ pid, signal });
      if (pid === -4321 && signal === "SIGKILL") {
        controlled.finish({
          kind: "exit",
          code: null,
          signal: "SIGKILL",
        });
      }
      if (pid === -4321 && signal === 0) {
        throw errno("ESRCH");
      }
      return true;
    };

    await terminateOwnedProcessGroup(controlled.ownedProcess, {
      gracefulTimeoutMs: 1,
      exitTimeoutMs: 100,
      groupTimeoutMs: 100,
      pollIntervalMs: 1,
      signalProcess,
    });

    assert.deepEqual(calls, [
      { pid: 4321, signal: "SIGTERM" },
      { pid: 4321, signal: 0 },
      { pid: -4321, signal: "SIGKILL" },
      { pid: -4321, signal: 0 },
    ]);
  });

  it("does not signal after the explicit outcome latch has settled", async () => {
    const controlled = controlledOwnedProcess(4321);
    controlled.finish({ kind: "exit", code: null, signal: "SIGKILL" });
    const calls: Array<{
      readonly pid: number;
      readonly signal: NodeJS.Signals | number;
    }> = [];

    await assert.rejects(
      terminateOwnedProcessGroup(controlled.ownedProcess, {
        signalProcess(pid, signal) {
          calls.push({ pid, signal });
          return true;
        },
      }),
      /finished before cleanup/,
    );
    assert.deepEqual(calls, []);
  });

  it("surfaces unexpected cleanup signal failures", async () => {
    const controlled = controlledOwnedProcess(4321);
    const signalProcess: ProcessSignal = () => {
      throw errno("EPERM", "permission denied");
    };

    await assert.rejects(
      terminateOwnedProcessGroup(controlled.ownedProcess, { signalProcess }),
      /permission denied/,
    );
  });
});

function controlledOwnedProcess(pid: number): {
  readonly ownedProcess: OwnedProcess;
  finish(outcome: OwnedProcessOutcome): void;
} {
  let finished = false;
  let resolveOutcome: ((outcome: OwnedProcessOutcome) => void) | undefined;
  const outcome = new Promise<OwnedProcessOutcome>((resolve) => {
    resolveOutcome = resolve;
  });
  return {
    ownedProcess: {
      pid,
      outcome,
      isFinished: () => finished,
      ref() {},
    },
    finish(value) {
      if (finished) {
        return;
      }
      finished = true;
      resolveOutcome?.(value);
    },
  };
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

function errno(code: string, message = code): NodeJS.ErrnoException {
  const error = new Error(message) as NodeJS.ErrnoException;
  error.code = code;
  return error;
}
