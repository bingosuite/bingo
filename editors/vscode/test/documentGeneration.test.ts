import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { WebviewDeliveryState } from "../src/documentGeneration.js";

describe("webview document generations", () => {
  it("ignores an old failure after the same revision starts in a recreated document", () => {
    const delivery = new WebviewDeliveryState();
    delivery.beginDocument();
    delivery.markReady();
    const stale = delivery.beginDelivery(7)!;

    delivery.markHidden();
    delivery.markReady();
    const current = delivery.beginDelivery(7)!;

    assert.equal(delivery.rejectDelivery(stale), false);
    assert.equal(delivery.ready, true);
    assert.equal(delivery.acknowledge(current.revision), true);
    assert.equal(delivery.lastRenderedRevision, 7);
  });
});
