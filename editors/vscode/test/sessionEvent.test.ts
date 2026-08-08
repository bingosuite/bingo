import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  decodeSessionAnnouncement,
  sessionDAPEventName,
} from "../src/sessionEvent.js";

describe("DAP session discovery", () => {
  it("accepts only the versioned namespaced event body", () => {
    assert.equal(
      decodeSessionAnnouncement(sessionDAPEventName, {
        version: 1,
        sessionId: "session-1",
      })?.sessionId,
      "session-1",
    );
    assert.equal(
      decodeSessionAnnouncement("output", { version: 1, sessionId: "x" }),
      undefined,
    );
    assert.throws(
      () => decodeSessionAnnouncement(sessionDAPEventName, {
        version: 2,
        sessionId: "x",
      }),
      /unsupported/,
    );
    assert.throws(
      () => decodeSessionAnnouncement(sessionDAPEventName, {
        version: 1,
        sessionId: "",
      }),
      /invalid sessionId/,
    );
  });
});
