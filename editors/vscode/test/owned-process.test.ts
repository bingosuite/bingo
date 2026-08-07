import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  terminateOwnedProcessGroup,
  type ProcessSignal,
} from "../scripts/owned-process.mjs";

describe("test-owned process cleanup", () => {
  it("signals only the detached process group and waits for exit", async () => {
    const calls: Array<{ readonly pid: number; readonly signal: NodeJS.Signals }> = [];
    let finishExit: (() => void) | undefined;
    const exit = new Promise<void>((resolve) => {
      finishExit = resolve;
    });
    const signalProcess: ProcessSignal = (pid, signal) => {
      calls.push({ pid, signal });
      finishExit?.();
      return true;
    };

    await terminateOwnedProcessGroup(4321, exit, signalProcess);
    assert.deepEqual(calls, [{ pid: -4321, signal: "SIGKILL" }]);
  });

  it("tolerates an already-exited owned process group", async () => {
    const signalProcess: ProcessSignal = () => {
      const error = new Error("gone") as NodeJS.ErrnoException;
      error.code = "ESRCH";
      throw error;
    };

    await terminateOwnedProcessGroup(
      4321,
      Promise.resolve(),
      signalProcess,
    );
  });

  it("surfaces unexpected cleanup signal failures", async () => {
    const signalProcess: ProcessSignal = () => {
      const error = new Error("permission denied") as NodeJS.ErrnoException;
      error.code = "EPERM";
      throw error;
    };

    await assert.rejects(
      terminateOwnedProcessGroup(4321, Promise.resolve(), signalProcess),
      /permission denied/,
    );
  });
});
