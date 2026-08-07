import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  withTimeout,
  type TimeoutHandle,
  type TimeoutTimers,
} from "../scripts/timing.mjs";

describe("script timeout", () => {
  it("unrefs and clears the losing timer when the operation settles", async () => {
    let unrefed = false;
    let cleared: TimeoutHandle | undefined;
    const handle: TimeoutHandle = {
      unref(): void {
        unrefed = true;
      },
    };
    const timers: TimeoutTimers = {
      set() {
        return handle;
      },
      clear(timer) {
        cleared = timer;
      },
    };

    assert.equal(
      await withTimeout(Promise.resolve("ready"), 10_000, "too slow", timers),
      "ready",
    );
    assert.equal(unrefed, true);
    assert.equal(cleared, handle);
  });

  it("clears the timer after a timeout rejection", async () => {
    let fire: (() => void) | undefined;
    let cleared = false;
    const timers: TimeoutTimers = {
      set(_milliseconds, callback) {
        fire = callback;
        return {};
      },
      clear() {
        cleared = true;
      },
    };
    const pending = new Promise<never>(() => undefined);
    const result = withTimeout(pending, 100, "deadline reached", timers);

    assert.notEqual(fire, undefined);
    fire?.();
    await assert.rejects(result, /deadline reached/);
    assert.equal(cleared, true);
  });
});
