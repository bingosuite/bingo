import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { decodeWebviewMessage } from "../src/messages.js";

describe("webview message codec", () => {
  it("accepts the bounded command surface", () => {
    assert.deepEqual(decodeWebviewMessage({ type: "ready" }), { type: "ready" });
    assert.deepEqual(
      decodeWebviewMessage({ type: "selectSession", id: "debug-1" }),
      { type: "selectSession", id: "debug-1" },
    );
    assert.deepEqual(
      decodeWebviewMessage({ type: "selectGoroutine", id: 42 }),
      { type: "selectGoroutine", id: 42 },
    );
    assert.deepEqual(
      decodeWebviewMessage({ type: "selectGoroutine", id: 0 }),
      { type: "selectGoroutine", id: 0 },
    );
    assert.deepEqual(
      decodeWebviewMessage({
        type: "rendered",
        generation: 3,
        revision: 42,
      }),
      { type: "rendered", generation: 3, revision: 42 },
    );
  });

  it("rejects unknown commands, extra fields, and invalid identifiers", () => {
    assert.throws(() => decodeWebviewMessage({ type: "continue" }), /unknown/);
    assert.throws(
      () => decodeWebviewMessage({ type: "refresh", command: "Continue" }),
      /unexpected fields/,
    );
    assert.throws(
      () => decodeWebviewMessage({ type: "selectGoroutine", id: -1 }),
      /safe integer/,
    );
    assert.throws(
      () => decodeWebviewMessage({ type: "rendered", revision: 1 }),
      /unexpected fields/,
    );
  });
});
