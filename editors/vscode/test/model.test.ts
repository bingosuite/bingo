import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  appendLifecycle,
  maximumTimelineEntries,
  serializeSnapshot,
  toSessionViewModel,
  type SessionModel,
} from "../src/model.js";
import { snapshot } from "./fixtures.js";

describe("concurrency model", () => {
  it("retains only the bounded lifecycle tail", () => {
    let timeline = appendLifecycle(
      [],
      { ...snapshot(), created: Array.from({ length: 80 }, (_, index) => index + 1) },
      1,
    );
    timeline = appendLifecycle(
      timeline,
      { ...snapshot(), exited: Array.from({ length: 80 }, (_, index) => index + 1) },
      2,
    );
    assert.equal(timeline.length, maximumTimelineEntries);
    assert.equal(timeline.at(-1)?.action, "exited");
  });

  it("serializes hostile tracee strings as inert JSON", () => {
    const model: SessionModel = {
      debugSessionId: "debug",
      debugSessionName: "<script>alert(1)</script>",
      sessionId: "session",
      connection: "connected",
      sessionState: "suspended",
      clients: 2,
      lastStop: "",
      error: "",
      seqGap: "",
      lastSeq: 1,
      snapshot: {
        ...snapshot(),
        goroutines: [
          {
            ...snapshot().goroutines[0]!,
            waitReason: "</script><img src=x onerror=alert(1)>",
          },
        ],
      },
      selectedGoroutine: 1,
      timeline: [],
    };
    const serialized = serializeSnapshot(model);
    const decoded = JSON.parse(serialized) as {
      readonly snapshot: {
        readonly goroutines: readonly [{ readonly waitReason: string }];
      };
    };
    assert.equal(decoded.snapshot.goroutines[0].waitReason, "</script><img src=x onerror=alert(1)>");
  });

  it("recognizes an unknown synthetic snapshot even when its stop location resolved", () => {
    const base = snapshot();
    const view = toSessionViewModel({
      debugSessionId: "debug",
      debugSessionName: "debug",
      sessionId: "session",
      connection: "connected",
      sessionState: "suspended",
      clients: 1,
      lastStop: "",
      error: "",
      seqGap: "",
      lastSeq: 1,
      snapshot: {
        ...base,
        current: 0,
        goroutines: [{
          ...base.goroutines[0]!,
          id: 0,
          status: "unknown",
          current: true,
          currentLoc: {
            file: "/tmp/main.go",
            line: 42,
            function: "main.run",
          },
        }],
        threads: [],
      },
      selectedGoroutine: 0,
      timeline: [],
    });

    assert.equal(view.degraded, true);
  });
});
