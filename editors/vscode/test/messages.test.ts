import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { decodeAction, decodeWebviewMessage } from "../src/messages.js";

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
      decodeWebviewMessage({ type: "selectFrame", id: 7 }),
      { type: "selectFrame", id: 7 },
    );
    assert.deepEqual(
      decodeWebviewMessage({ type: "expandVariable", reference: 65_536 }),
      { type: "expandVariable", reference: 65_536 },
    );
    assert.deepEqual(decodeWebviewMessage({ type: "refreshInspection" }), {
      type: "refreshInspection",
    });
    assert.deepEqual(
      decodeWebviewMessage({
        type: "openSource",
        target: "frame",
        frameId: 1,
      }),
      {
        type: "openSource",
        target: "frame",
        frameId: 1,
      },
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
    assert.throws(
      () => decodeWebviewMessage({ type: "selectFrame", id: 0 }),
      /safe integer/,
    );
    assert.throws(
      () =>
        decodeWebviewMessage({
          type: "openSource",
          target: "command:evil",
          frameId: 0,
        }),
      /displayed metadata/,
    );
  });

  it("binds actions to a document, rendered revision, session and selection", () => {
    const message = {
      type: "action",
      generation: 3,
      revision: 42,
      debugSessionId: "session",
      goroutineId: 7,
      action: { type: "openSource", target: "created", frameId: 0 },
    };
    assert.equal(decodeAction(message).context.goroutineId, 7);
    for (const patch of [
      { generation: 0 }, { generation: NaN }, { revision: -1 },
      { goroutineId: 1.5 }, { debugSessionId: "x".repeat(257) },
      { action: { type: "ready" } },
      { action: { type: "rendered", revision: 42, generation: 3 } },
      { action: { type: "openSource", target: "created", frameId: 0, path: "/etc/passwd" } },
      { action: { type: "continue" } },
    ]) {
      assert.throws(() => decodeAction({ ...message, ...patch }));
    }
    assert.throws(() => decodeAction({ ...message, arbitrary: true }), /unexpected/);
    assert.throws(() => decodeAction(message.action));
  });

  it("enforces every action-context numeric boundary without coercion", () => {
    const message = {
      type: "action", generation: 1, revision: 0, debugSessionId: "",
      goroutineId: 0, action: { type: "refresh" },
    };
    assert.deepEqual(decodeAction(message).context, {
      generation: 1, revision: 0, debugSessionId: "", goroutineId: 0,
    });
    for (const field of ["generation", "revision", "goroutineId"]) {
      assert.doesNotThrow(() => decodeAction({
        ...message, [field]: Number.MAX_SAFE_INTEGER,
      }));
      for (const value of [
        -1, 0.5, NaN, Infinity, -Infinity, Number.MAX_SAFE_INTEGER + 1,
        "1", true, null, undefined, {}, [],
      ]) {
        assert.throws(() => decodeAction({ ...message, [field]: value }),
          `${field} must reject an invalid ${typeof value} value`);
      }
    }
    assert.throws(() => decodeAction({ ...message, generation: 0 }));
    assert.doesNotThrow(() => decodeAction({
      ...message, debugSessionId: "x".repeat(256),
    }));
    for (const debugSessionId of ["x".repeat(257), null, 42, {}, []]) {
      assert.throws(() => decodeAction({ ...message, debugSessionId }));
    }
  });

  it("only accepts metadata source selectors and never path or command payloads", () => {
    for (const target of ["created", "start", "current", "frame"]) {
      const message = { type: "openSource", target, frameId: 1 };
      assert.doesNotThrow(() => decodeWebviewMessage(message));
      for (const key of ["path", "uri", "command", "arguments", "__proto__"]) {
        assert.throws(() => decodeWebviewMessage({
          ...message, [key]: "command:workbench.action.terminal.new",
        }), /unexpected fields/);
      }
    }
    for (const target of [
      "", "Created", "../main.go", "/etc/passwd", "javascript:alert(1)",
      "file:///etc/passwd", "https://example.invalid/main.go", "<script>",
      null, 1, {}, [],
    ]) {
      assert.throws(() => decodeWebviewMessage({
        type: "openSource", target, frameId: 1,
      }));
    }
    assert.throws(() => decodeWebviewMessage({
      type: "openSource", target: "frame", frameId: 0,
    }));
    for (const type of ["continue", "next", "stepIn", "pause", "terminate", "kill", "restart"]) {
      assert.throws(() => decodeWebviewMessage({ type }));
    }
  });
});
