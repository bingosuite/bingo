import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { TextDecoder } from "node:util";
import { describe, it } from "node:test";

import {
  decodeEvent,
  maximumStringLength,
  TelemetryProtocolError,
  wireProtocolVersion,
} from "../src/telemetry.js";

// The producer counts its per-element string limit in Go; this decoder counts it
// again in TypeScript. Both suites already assert the rule in their own terms,
// which is exactly how they could drift: a boundary the producer believes legal
// and this decoder rejects is a deterministic kill, reproduced on every retry,
// for the rest of the session. See issue #194.
//
// So the boundary cases are generated once by the Go packer into a checked-in
// fixture and enforced here against the identical wire strings. A Go spec
// regenerates and diff-gates the same file, so neither side can move alone.
interface BoundaryCase {
  readonly name: string;
  readonly inputBase64: string;
  readonly wire: string;
  readonly utf16Units: number;
  readonly accepted: boolean;
}

interface BoundaryFixture {
  readonly maxGoroutineStringLength: number;
  readonly cases: readonly BoundaryCase[];
}

const fixture = JSON.parse(
  readFileSync(
    resolve(process.cwd(), "..", "..", "pkg", "protocol", "testdata", "goroutine_string_boundary.json"),
    "utf8",
  ),
) as BoundaryFixture;

// Replacement-mode decoding, which is what encoding/json does to bytes that are
// not valid UTF-8: one U+FFFD per maximal invalid subpart.
const lenient = new TextDecoder("utf-8", { fatal: false });

function snapshotWithStatus(status: string): string {
  return JSON.stringify({
    v: wireProtocolVersion,
    kind: "GoroutineSnapshot",
    seq: 1,
    payload: {
      goroutines: [
        { id: 1, status: "running", current: true, currentLoc: { file: "a.go", line: 1 } },
        { id: 2, status, currentLoc: { file: "a.go", line: 1 } },
      ],
      threads: [],
      current: 1,
    },
  });
}

describe("cross-language string boundary fixture", () => {
  it("is denominated in the same limit this decoder enforces", () => {
    assert.equal(
      fixture.maxGoroutineStringLength,
      maximumStringLength,
      "the producer's string limit has moved; this decoder must follow in the same change",
    );
  });

  it("covers both verdicts at the boundary", () => {
    const accepted = fixture.cases.filter((c) => c.accepted).length;
    assert.ok(accepted >= 5, "the fixture must contain strings this decoder accepts");
    assert.ok(
      fixture.cases.length - accepted >= 5,
      "and strings it must reject, or a decoder that accepts everything would pass",
    );
  });

  for (const testCase of fixture.cases) {
    it(`agrees with the producer on ${testCase.name}`, () => {
      // The producer measured in UTF-16 code units because that is what this
      // runtime counts. Prove that is literally true of the delivered string
      // rather than trusting the label.
      assert.equal(
        testCase.wire.length,
        testCase.utf16Units,
        "the fixture's unit count must be this runtime's String.length",
      );

      // The producer's raw bytes become the wire string through JSON encoding,
      // which substitutes U+FFFD per invalid subpart. Node's replacement-mode
      // UTF-8 decoder must reach the same string, or the two languages disagree
      // about how many units an invalid byte costs.
      assert.equal(
        lenient.decode(Buffer.from(testCase.inputBase64, "base64")),
        testCase.wire,
        "Go and this runtime must agree on what an invalid byte becomes",
      );

      const frame = snapshotWithStatus(testCase.wire);
      if (testCase.accepted) {
        const decoded = decodeEvent(frame);
        assert.equal(
          decoded.snapshot?.goroutines.length,
          2,
          "a string the producer will emit must decode here",
        );
        return;
      }
      assert.throws(
        () => decodeEvent(frame),
        TelemetryProtocolError,
        "a string the producer refuses to emit must be refused here too",
      );
    });
  }
});
