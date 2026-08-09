import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  decodeEvent,
  maximumEnvelopeBytes,
  maximumGoroutines,
  maximumThreads,
  SequenceTracker,
  snapshotCommand,
} from "../src/telemetry.js";
import { envelope, goroutine, snapshot, thread } from "./fixtures.js";

describe("telemetry codec", () => {
  it("decodes protocol 1.4 snapshots and emits only the read-only command", () => {
    const decoded = decodeEvent(envelope(1, "GoroutineSnapshot", snapshot()));
    assert.equal(decoded.kind, "GoroutineSnapshot");
    assert.equal(decoded.snapshot?.goroutines[0]?.id, 1);
    assert.deepEqual(JSON.parse(snapshotCommand()), {
      v: "1.4",
      kind: "GoroutineSnapshot",
      payload: {},
    });
  });

  it("accepts nil Go slices as empty snapshot collections", () => {
    const decoded = decodeEvent(
      envelope(1, "GoroutineSnapshot", {
        goroutines: null,
        threads: null,
      }),
    );
    assert.deepEqual(decoded.snapshot?.goroutines, []);
    assert.deepEqual(decoded.snapshot?.threads, []);
  });

  it("accepts the synthetic unknown goroutine id", () => {
    const decoded = decodeEvent(
      envelope(
        1,
        "GoroutineSnapshot",
        snapshot([goroutine(0, 0, { status: "unknown", current: true })], []),
      ),
    );
    assert.equal(decoded.snapshot?.goroutines[0]?.id, 0);
    assert.equal(decoded.snapshot?.goroutines[0]?.status, "unknown");
  });

  it("accepts the bounded rich thread prefix plus current anchor", () => {
    const threads = Array.from({ length: maximumThreads }, (_, index) =>
      thread(index + 1),
    );
    const decoded = decodeEvent(
      envelope(1, "GoroutineSnapshot", snapshot(undefined, threads)),
    );
    assert.equal(decoded.snapshot?.threads.length, maximumThreads);
  });

  const minimalGoroutine = (id: number) => ({
    id,
    status: "waiting",
    currentLoc: { file: "", line: 0 },
  });

  it("accepts the packed goroutine cap and rejects one past it", () => {
    assert.equal(maximumGoroutines, 5000);
    const accepted = Array.from(
      { length: maximumGoroutines },
      (_, index) => goroutine(index + 1),
    );
    const acceptedEnvelope = envelope(1, "GoroutineSnapshot", {
      ...snapshot(),
      goroutines: accepted,
    });
    // The producer packs against exactly this budget, so a payload holding the
    // full element cap has to fit the decoder's byte contract too.
    assert.ok(acceptedEnvelope.byteLength <= maximumEnvelopeBytes);
    const decoded = decodeEvent(acceptedEnvelope);
    assert.equal(decoded.snapshot?.goroutines.length, maximumGoroutines);

    assert.throws(
      () =>
        decodeEvent(
          envelope(2, "GoroutineSnapshot", {
            ...snapshot(),
            goroutines: [
              ...accepted,
              minimalGoroutine(maximumGoroutines + 1),
            ],
          }),
        ),
      /5000 item limit/,
    );
  });

  it("rejects malformed, incompatible, oversized, and hostile payloads", () => {
    assert.throws(() => decodeEvent("{"), /valid JSON/);
    assert.throws(
      () => decodeEvent(JSON.stringify({ v: "9", kind: "Continued", seq: 1, payload: {} })),
      /incompatible/,
    );
    assert.throws(
      () => decodeEvent("x".repeat(maximumEnvelopeBytes + 1)),
      /exceeds the 2097152 byte contract/,
    );
    assert.throws(
      () =>
        decodeEvent(
          envelope(1, "GoroutineSnapshot", {
            ...snapshot(),
            goroutines: Array.from(
              { length: maximumGoroutines + 1 },
              (_, index) => goroutine(index + 1),
            ),
          }),
        ),
      /item limit/,
    );
    assert.throws(
      () =>
        decodeEvent(
          envelope(1, "SessionState", {
            sessionID: "<img onerror=alert(1)>",
            state: "x".repeat(5000),
            clients: 1,
          }),
        ),
      /characters/,
    );
    assert.throws(
      () =>
        decodeEvent(
          Buffer.from([
            0x7b, 0x22, 0x76, 0x22, 0x3a, 0x22, 0xff, 0x22, 0x7d,
          ]),
        ),
      /UTF-8/,
    );
    assert.throws(
      () =>
        decodeEvent(
          JSON.stringify({
            v: "1.4",
            kind: "Continued",
            seq: 1,
            payload: {},
            unexpected: true,
          }),
        ),
      /unknown field/,
    );
    assert.throws(
      () =>
        decodeEvent(
          JSON.stringify({
            v: "1.4",
            kind: "FutureEvent",
            seq: 1,
            payload: {},
          }),
        ),
      /unknown telemetry event kind/,
    );
    assert.throws(
      () =>
        decodeEvent(
          envelope(1, "GoroutineSnapshot", {
            ...snapshot(),
            goroutines: [goroutine(1), goroutine(1)],
          }),
        ),
      /duplicate goroutine id/,
    );
  });

  it("detects duplicates and gaps without accepting duplicates", () => {
    const tracker = new SequenceTracker();
    assert.deepEqual(tracker.observe(1), { accept: true });
    assert.deepEqual(tracker.observe(1), {
      accept: false,
      gap: "duplicate event 1",
    });
    assert.deepEqual(tracker.observe(4), {
      accept: true,
      gap: "missed events 2-3",
    });
    assert.deepEqual(tracker.observe(2), {
      accept: true,
      gap: "out-of-order event 2 after 4",
    });
    tracker.reset();
    assert.deepEqual(tracker.observe(2), { accept: true });
  });
});
