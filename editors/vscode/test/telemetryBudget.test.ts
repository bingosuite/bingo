import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { readFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { resolve } from "node:path";
import { after, afterEach, describe, it } from "node:test";

import type WebSocket from "ws";
import { WebSocketServer } from "ws";

import {
  formatServerCount,
  toSessionViewModel,
  type SessionModel,
} from "../src/model.js";
import { serverOmissionText } from "../src/webviewApp.js";
import {
  observerDependencies,
  TelemetryObserver,
  type Socket,
} from "../src/observer.js";
import {
  decodeEvent,
  decodeGoroutineList,
  maximumEnvelopeBytes,
  maximumGoroutines,
  maximumLifecycleIDs,
  maximumStringLength,
  maximumThreads,
  maximumTransportBytes,
  TelemetryProtocolError,
  transportSlackBytes,
  wireProtocolVersion,
  type TelemetryData,
} from "../src/telemetry.js";
import { goroutine, snapshot, thread } from "./fixtures.js";

// Tests run with cwd at editors/vscode, so the Go source is two levels up.
const goSource = (relative: string): string =>
  readFileSync(resolve(process.cwd(), "..", "..", relative), "utf8");

function frame(seq: number, kind: string, payload: unknown): string {
  return JSON.stringify({ v: wireProtocolVersion, kind, seq, payload });
}

// richGoroutines builds the shape that used to kill the observer: three
// Locations per element, which is what pushes a broadcast EventGoroutines past
// the recursive node budget long before it approaches the byte budget.
function richGoroutines(count: number): unknown[] {
  const file = "/home/runner/go/src/github.com/bingosuite/bingo/internal/service/handler.go";
  const out: unknown[] = [];
  for (let id = 1; id <= count; id += 1) {
    out.push({
      id,
      parentId: id > 1 ? Math.floor(id / 2) : 0,
      status: "waiting",
      waitReason: "chan receive",
      currentLoc: { file, line: 1234, function: "service.(*Handler).Serve.func1" },
      startLoc: { file, line: 88, function: "service.(*Handler).Serve.gowrap1" },
      createdLoc: { file, line: 91, function: "service.(*Handler).Serve" },
      threadId: id % 16,
    });
  }
  return out;
}

function baseModel(overrides: Partial<SessionModel> = {}): SessionModel {
  return {
    debugSessionId: "debug-1",
    debugSessionName: "level5",
    sessionId: "session/a",
    connection: "connected",
    sessionState: "suspended",
    clients: 1,
    lastStop: "",
    error: "",
    seqGap: "",
    lastSeq: 1,
    snapshot: undefined,
    selectedGoroutine: 0,
    timeline: [],
    ...overrides,
  };
}

describe("telemetry budget contract", () => {
  it("keeps the transport ceiling strictly above the decoder contract", () => {
    assert.ok(
      maximumTransportBytes > maximumEnvelopeBytes,
      "an oversized frame must reach the decoder, not die inside ws",
    );
    assert.equal(maximumTransportBytes, maximumEnvelopeBytes + transportSlackBytes);
  });

  it("accepts a frame exactly at the decoder cap and rejects one byte more", () => {
    // padTo builds a legal envelope of EXACTLY the requested byte length by
    // widening one payload string, so the boundary is pinned rather than
    // approximated.
    const padTo = (size: number): string => {
      const base = frame(1, "GoroutineSnapshot", {
        goroutines: [],
        threads: [],
        current: 1,
        pad: "",
      });
      const filler = "f".repeat(size - Buffer.byteLength(base));
      const exact = frame(1, "GoroutineSnapshot", {
        goroutines: [],
        threads: [],
        current: 1,
        pad: filler,
      });
      assert.equal(Buffer.byteLength(exact), size);
      return exact;
    };

    // "pad" is not a snapshot key, so these are rejected for schema reasons —
    // what matters is WHICH rejection fires, i.e. that the size gate does not.
    assert.throws(
      () => decodeEvent(padTo(maximumEnvelopeBytes - 1)),
      /has unknown field "pad"/u,
      "one byte below the cap must pass the size gate",
    );
    assert.throws(
      () => decodeEvent(padTo(maximumEnvelopeBytes)),
      /has unknown field "pad"/u,
      "the cap is inclusive",
    );
    assert.throws(
      () => decodeEvent(padTo(maximumEnvelopeBytes + 1)),
      /exceeds the 2097152 byte contract/u,
      "one byte above the cap must fail the size gate",
    );
  });

  it("applies the same boundary to every carrier shape", () => {
    const atCap = "x".repeat(maximumEnvelopeBytes);
    const overCap = "x".repeat(maximumEnvelopeBytes + 1);
    for (const [label, at, over] of [
      ["string", atCap, overCap],
      ["buffer", Buffer.from(atCap), Buffer.from(overCap)],
      ["buffer list", [Buffer.from(atCap)], [Buffer.from(overCap)]],
    ] as const) {
      assert.throws(
        () => decodeEvent(at),
        /not valid JSON/u,
        `${label} at the cap must reach the parser`,
      );
      assert.throws(
        () => decodeEvent(over),
        /exceeds the 2097152 byte contract/u,
        `${label} above the cap must be rejected on size`,
      );
    }
  });

  it("pins the wire constants against the Go protocol source", () => {
    const source = goSource("pkg/protocol/protocol.go");
    assert.match(source, new RegExp(`const Version = "${wireProtocolVersion}"`));
    assert.match(
      source,
      new RegExp(
        `MaxGoroutineEventBytes\\s*=\\s*${String(maximumEnvelopeBytes / (1024 * 1024))} \\* 1024 \\* 1024`,
      ),
    );
    assert.match(
      source,
      new RegExp(`MaxSnapshotGoroutines\\s*=\\s*${String(maximumGoroutines)}\\b`),
    );
    assert.match(
      source,
      new RegExp(`MaxSnapshotThreads\\s*=\\s*${String(maximumThreads)}\\b`),
    );
  });

  it("pins the extension version against the manifest", () => {
    const manifest = JSON.parse(
      readFileSync(resolve(process.cwd(), "package.json"), "utf8"),
    ) as { readonly version: string };
    assert.equal(manifest.version, "0.4.0");
  });
});

describe("shallow parsing of unconsumed kinds", () => {
  const unconsumed = [
    "Output",
    "BreakpointSet",
    "BreakpointCleared",
    "Locals",
    "Frames",
    "Goroutines",
    "Evaluate",
    "Restarted",
  ] as const;

  for (const kind of unconsumed) {
    it(`skips the body of ${kind} even when it is enormous`, () => {
      // Sized deliberately: comfortably inside the byte contract, far beyond
      // the recursive node budget. Deep validation would throw here, which is
      // exactly the failure that used to take the whole view down.
      const raw = frame(4, kind, { goroutines: richGoroutines(3000) });
      assert.ok(
        Buffer.byteLength(raw) < maximumEnvelopeBytes,
        "the fixture must be legal by bytes so only the node budget could reject it",
      );
      const decoded = decodeEvent(raw);
      assert.equal(decoded.kind, kind);
      assert.deepEqual(
        decoded.payload,
        {},
        "an unconsumed payload must never reach applyEvent",
      );
      assert.equal(decoded.snapshot, undefined);
    });

    it(`skips the body of ${kind} even when it is malformed`, () => {
      const hostile = {
        goroutines: [{ id: -1, status: 5, currentLoc: "not-an-object" }],
        nested: { deeply: { broken: [null, Number.NaN, () => 1] } },
        "unknown-field": "x".repeat(50_000),
      };
      const decoded = decodeEvent(frame(5, kind, hostile));
      assert.deepEqual(decoded.payload, {});
    });
  }

  it("keeps the envelope strict for unconsumed kinds", () => {
    assert.throws(
      () => decodeEvent(JSON.stringify({ v: "1.2", kind: "Goroutines", seq: 1, payload: {} })),
      TelemetryProtocolError,
    );
    assert.throws(
      () => decodeEvent(JSON.stringify({ v: wireProtocolVersion, kind: "Goroutines", seq: 0, payload: {} })),
      TelemetryProtocolError,
    );
    assert.throws(
      () => decodeEvent(JSON.stringify({ v: wireProtocolVersion, kind: "Goroutines", seq: 1, payload: [] })),
      TelemetryProtocolError,
    );
    assert.throws(
      () =>
        decodeEvent(
          JSON.stringify({ v: wireProtocolVersion, kind: "Goroutines", seq: 1, payload: {}, extra: 1 }),
        ),
      TelemetryProtocolError,
    );
    assert.throws(() => decodeEvent(frame(1, "NotAKind", {})), TelemetryProtocolError);
  });

  it("still deep-validates every consumed kind", () => {
    // Structural violations are terminal for any kind — no server may emit them.
    assert.throws(
      () => decodeEvent(frame(1, "SessionState", { sessionID: "a", state: "nope", clients: 0 })),
      TelemetryProtocolError,
    );
    assert.throws(
      () => decodeEvent(frame(1, "GoroutineSnapshot", { goroutines: [], threads: [], nope: 1 })),
      TelemetryProtocolError,
    );
  });

  it("rejects an oversized consumed payload without calling it a violation", () => {
    // The walk still runs — this process will not build a model from an
    // unbounded stranger's payload — but the contract bounds only the goroutine
    // family, so exceeding those caps elsewhere is this decoder's own defence,
    // not a promise the server broke. Classifying it as a violation latches the
    // view dead over output the server was entitled to send.
    const oversized: readonly string[] = [
      frame(1, "Error", { message: "x".repeat(5000) }),
      frame(1, "BreakpointHit", { frames: richGoroutines(3000) }),
    ];
    for (const payload of oversized) {
      assert.throws(() => decodeEvent(payload), (error: unknown) => {
        assert.ok(error instanceof Error, "it is still rejected");
        assert.equal(
          error instanceof TelemetryProtocolError,
          false,
          "but stays recoverable, so the reconnect ladder still applies",
        );
        return true;
      });
    }
  });
});

describe("snapshot totals", () => {
  it("decodes a full totals object", () => {
    const decoded = decodeEvent(
      frame(1, "GoroutineSnapshot", {
        goroutines: [{ id: 1, status: "running", current: true, currentLoc: { file: "a.go", line: 1 } }],
        threads: [],
        totals: {
          goroutines: 41203,
          threads: 64,
          goroutinesClipped: true,
          threadsClipped: false,
        },
      }),
    );
    assert.deepEqual(decoded.snapshot?.totals, {
      goroutines: 41203,
      threads: 64,
      goroutinesClipped: true,
      threadsClipped: false,
    });
  });

  it("decodes a partial totals object", () => {
    const decoded = decodeEvent(
      frame(1, "GoroutineSnapshot", {
        goroutines: [{ id: 1, status: "running", currentLoc: { file: "a.go", line: 1 } }],
        threads: [],
        totals: { goroutines: 9001 },
      }),
    );
    assert.deepEqual(decoded.snapshot?.totals, {
      goroutines: 9001,
      threads: 0,
      goroutinesClipped: false,
      threadsClipped: false,
    });
  });

  it("leaves totals undefined when the server omits them", () => {
    const decoded = decodeEvent(
      frame(1, "GoroutineSnapshot", {
        goroutines: [{ id: 1, status: "running", currentLoc: { file: "a.go", line: 1 } }],
        threads: [],
      }),
    );
    assert.equal(decoded.snapshot?.totals, undefined);
  });

  it("rejects unknown keys inside totals", () => {
    assert.throws(
      () =>
        decodeEvent(
          frame(1, "GoroutineSnapshot", {
            goroutines: [],
            threads: [],
            totals: { goroutines: 1, surprise: true },
          }),
        ),
      TelemetryProtocolError,
    );
  });

  it("decodes the 1.4 goroutine list shape with totals", () => {
    const list = decodeGoroutineList({
      goroutines: [{ id: 3, status: "running", currentLoc: { file: "a.go", line: 1 } }],
      totals: { goroutines: 8192, goroutinesClipped: true },
    });
    assert.equal(list.goroutines.length, 1);
    assert.deepEqual(list.totals, {
      goroutines: 8192,
      threads: 0,
      goroutinesClipped: true,
      threadsClipped: false,
    });
  });

  it("rejects unknown keys in the goroutine list shape", () => {
    assert.throws(
      () => decodeGoroutineList({ goroutines: [], nope: 1 }),
      TelemetryProtocolError,
    );
  });
});

describe("lifecycle deltas are not capped like packed elements", () => {
  // The packer never trims created/exited, and the debugger's runtime scan
  // reaches maximumLifecycleIDs — well past the 5000 packed-element cap.
  // Rejecting those was a false positive that, now that protocol errors are
  // terminal, killed the view permanently on exactly the workload this contract
  // exists for.
  function deltaFrame(count: number, start = 1): string {
    const ids: number[] = [];
    for (let i = 0; i < count; i += 1) {
      ids.push(start + i);
    }
    return frame(1, "GoroutineSnapshot", {
      goroutines: [
        { id: 1, status: "running", current: true, currentLoc: { file: "a.go", line: 1 } },
      ],
      threads: [],
      current: 1,
      created: ids,
      exited: ids.map((id) => id + 10_000_000),
    });
  }

  it("accepts a delta one past the packed-element cap", () => {
    // The exact count that used to be rejected.
    const decoded = decodeEvent(deltaFrame(maximumGoroutines + 1));
    assert.equal(decoded.snapshot?.created.length, maximumGoroutines + 1);
    assert.equal(decoded.snapshot?.exited.length, maximumGoroutines + 1);
  });

  // The ceiling is written out rather than taken from the constant: a test that
  // reads the value it is checking would silently follow it down and stop
  // catching the very regression this exists for.
  const scanCeiling = 8192;

  it("accepts a delta at exactly the scan ceiling", () => {
    assert.equal(maximumLifecycleIDs, scanCeiling);
    const decoded = decodeEvent(deltaFrame(scanCeiling));
    assert.equal(decoded.snapshot?.created.length, scanCeiling);
    assert.equal(decoded.snapshot?.exited.length, scanCeiling);
  });

  it("rejects a delta one past the scan ceiling", () => {
    assert.throws(
      () => decodeEvent(deltaFrame(scanCeiling + 1)),
      /created exceeds the 8192 item limit/u,
    );
  });

  it("accepts the worst-case delta frame within the byte contract", () => {
    // Both deltas at the ceiling with maximum-width safe-integer ids: the
    // largest lifecycle payload the producer can ever emit.
    const maxSafe = Number.MAX_SAFE_INTEGER;
    const created: number[] = [];
    const exited: number[] = [];
    for (let i = 0; i < scanCeiling; i += 1) {
      created.push(maxSafe - i);
      exited.push(maxSafe - scanCeiling - i);
    }
    const raw = frame(1, "GoroutineSnapshot", {
      goroutines: [
        { id: 1, status: "running", current: true, currentLoc: { file: "a.go", line: 1 } },
      ],
      threads: [],
      current: 1,
      created,
      exited,
    });
    assert.ok(
      Buffer.byteLength(raw) < maximumEnvelopeBytes,
      "the worst-case delta frame must stay inside the byte contract",
    );
    const decoded = decodeEvent(raw);
    assert.equal(decoded.snapshot?.created.length, scanCeiling);
  });

  it("mirrors the producer's lifecycle-delta ceiling", () => {
    const source = goSource("pkg/protocol/protocol.go");
    assert.match(
      source,
      new RegExp(`MaxLifecycleDeltaIDs\\s*=\\s*${String(scanCeiling)}\\b`),
      "producer and consumer must agree on the delta ceiling",
    );
    assert.notEqual(
      maximumLifecycleIDs,
      maximumGoroutines,
      "deltas must not be bounded by the packed-element cap",
    );
  });

  it("still rejects duplicate ids inside a delta", () => {
    assert.throws(
      () =>
        decodeEvent(
          frame(1, "GoroutineSnapshot", {
            goroutines: [],
            threads: [],
            created: [4, 4],
          }),
        ),
      TelemetryProtocolError,
    );
  });
});

describe("oversize fatality is scoped to the bounded kinds", () => {
  function oversizeFrame(kind: string): string {
    const filler = "p".repeat(maximumEnvelopeBytes);
    return JSON.stringify({
      v: wireProtocolVersion,
      kind,
      seq: 1,
      payload: { filler },
    });
  }

  it("treats an oversized goroutine event as a fatal contract violation", () => {
    for (const kind of ["GoroutineSnapshot", "Goroutines"]) {
      assert.throws(
        () => decodeEvent(oversizeFrame(kind)),
        TelemetryProtocolError,
        `${kind} must be fatal`,
      );
    }
  });

  it("treats an oversized unbounded event as transient, not fatal", () => {
    // Locals/Frames/Evaluate are broadcast to every client and are deliberately
    // NOT covered by the byte contract, so a large variable expansion in the
    // Variables pane must not permanently kill the concurrency view.
    for (const kind of ["Locals", "Frames", "Evaluate", "Output", "Restarted"]) {
      let thrown: unknown;
      try {
        decodeEvent(oversizeFrame(kind));
      } catch (error: unknown) {
        thrown = error;
      }
      assert.ok(thrown instanceof Error, `${kind} must still be rejected`);
      assert.ok(
        !(thrown instanceof TelemetryProtocolError),
        `${kind} must stay recoverable, not latch the view dead`,
      );
    }
  });

  it("classifies an oversized frame delivered as a Buffer", () => {
    assert.throws(
      () => decodeEvent(Buffer.from(oversizeFrame("GoroutineSnapshot"), "utf8")),
      TelemetryProtocolError,
    );
    assert.doesNotThrow(() => {
      try {
        decodeEvent(Buffer.from(oversizeFrame("Locals"), "utf8"));
      } catch (error: unknown) {
        if (error instanceof TelemetryProtocolError) {
          throw error;
        }
      }
    });
  });

  it("falls back to non-fatal when the kind cannot be read", () => {
    let thrown: unknown;
    try {
      decodeEvent("~".repeat(maximumEnvelopeBytes + 1));
    } catch (error: unknown) {
      thrown = error;
    }
    assert.ok(thrown instanceof Error);
    assert.ok(
      !(thrown instanceof TelemetryProtocolError),
      "an unreadable prefix must not latch the view dead on a guess",
    );
  });
});

describe("protocol violations do not consume reconnect attempts", () => {
  // A dedicated harness: every entry pushed into `delays` is one reconnect
  // attempt entering the ladder, so "did not consume an attempt" is directly
  // observable rather than inferred from timing.
  class LadderSocket extends EventEmitter implements Socket {
    public readyState = 0;
    public readonly sent: string[] = [];
    public closed = false;

    public onOpen(listener: () => void): void {
      this.on("open", listener);
    }
    public onMessage(listener: (data: TelemetryData) => void): void {
      this.on("message", listener);
    }
    public onClose(listener: () => void): void {
      this.on("close", listener);
    }
    public onError(listener: (error: Error) => void): void {
      this.on("error", listener);
    }
    public send(data: string): void {
      this.sent.push(data);
    }
    public close(): void {
      if (this.closed) {
        return;
      }
      this.closed = true;
      this.readyState = 3;
      this.emit("close");
    }
    public open(): void {
      this.readyState = 1;
      this.emit("open");
    }
  }

  function ladder(): {
    readonly observer: TelemetryObserver;
    readonly sockets: LadderSocket[];
    readonly delays: { resolve: () => void }[];
  } {
    const sockets: LadderSocket[] = [];
    const delays: { resolve: () => void }[] = [];
    const observer = new TelemetryObserver(
      {
        debugSessionId: "debug-1",
        debugSessionName: "ladder",
        sessionId: "s1",
        managementEndpoint: { host: "127.0.0.1", port: 6060 },
      },
      {
        createSocket() {
          const socket = new LadderSocket();
          sockets.push(socket);
          return socket;
        },
        delay(_milliseconds, signal) {
          return new Promise((resolve, reject) => {
            delays.push({ resolve });
            signal.addEventListener("abort", () => {
              reject(new Error("cancelled"));
            });
          });
        },
        now: () => 0,
      },
    );
    return { observer, sockets, delays };
  }

  const violations: readonly (readonly [string, string])[] = [
    ["malformed JSON", "{not json"],
    [
      "incompatible version",
      JSON.stringify({ v: "1.2", kind: "GoroutineSnapshot", seq: 1, payload: {} }),
    ],
    [
      "unknown kind",
      JSON.stringify({ v: wireProtocolVersion, kind: "Nope", seq: 1, payload: {} }),
    ],
    [
      "unknown envelope key",
      JSON.stringify({
        v: wireProtocolVersion,
        kind: "GoroutineSnapshot",
        seq: 1,
        payload: {},
        extra: 1,
      }),
    ],
    [
      "missing envelope key",
      JSON.stringify({ v: wireProtocolVersion, kind: "GoroutineSnapshot", seq: 1 }),
    ],
    [
      "non-object payload",
      JSON.stringify({ v: wireProtocolVersion, kind: "GoroutineSnapshot", seq: 1, payload: [] }),
    ],
    [
      "unknown field in a consumed snapshot",
      frame(1, "GoroutineSnapshot", { goroutines: [], threads: [], nope: 1 }),
    ],
    [
      "unknown field in consumed totals",
      frame(1, "GoroutineSnapshot", {
        goroutines: [],
        threads: [],
        totals: { goroutines: 1, surprise: true },
      }),
    ],
    [
      "duplicate goroutine id in a consumed snapshot",
      frame(1, "GoroutineSnapshot", {
        goroutines: [
          { id: 7, status: "running", currentLoc: { file: "a.go", line: 1 } },
          { id: 7, status: "waiting", currentLoc: { file: "a.go", line: 2 } },
        ],
        threads: [],
      }),
    ],
    [
      "invalid sequence in a consumed payload",
      frame(0, "GoroutineSnapshot", { goroutines: [], threads: [] }),
    ],
  ];

  for (const [label, payload] of violations) {
    it(`terminates without a reconnect attempt: ${label}`, () => {
      const { observer, sockets, delays } = ladder();
      observer.start();
      const socket = sockets[0]!;
      socket.open();
      socket.emit("message", Buffer.from(payload, "utf8"));

      assert.equal(observer.model.connection, "error", "the view must terminate");
      assert.notEqual(observer.model.error, "", "and say why");
      assert.equal(delays.length, 0, "no reconnect attempt may be consumed");
      assert.equal(sockets.length, 1, "and no new connection may be opened");
      assert.ok(socket.closed, "the offending connection must be closed");

      // The ladder stays shut even if the socket's close is delivered late.
      socket.emit("close");
      assert.equal(delays.length, 0);
      assert.equal(sockets.length, 1);
      observer.dispose();
    });
  }

  // The contract bounds TWO events. Everything else on the wire is explicitly
  // allowed to be large, so applying the bounded family's caps to a consumed-but-
  // unbounded kind must never be treated as a proven violation. A user pasting a
  // long Watch expression produces exactly this: the failed evaluate is broadcast
  // as an EventError whose message carries the whole expression back. Latching on
  // it would kill the Concurrency view for the rest of the session over something
  // the server was entitled to send. See issue #194.
  const legalButLarge: readonly (readonly [string, string])[] = [
    [
      "an Error message longer than the bounded-family string cap",
      frame(1, "Error", {
        command: "Evaluate",
        message: "x".repeat(maximumStringLength + 1),
      }),
    ],
    [
      "a stop event whose function name exceeds the string cap",
      frame(1, "BreakpointHit", {
        breakpoint: { id: 1 },
        goroutine: {
          id: 1,
          currentLoc: { file: "a.go", line: 1, function: "f".repeat(maximumStringLength + 1) },
        },
        frames: [],
      }),
    ],
    [
      "a stack deeper than the bounded-family element cap",
      frame(1, "Stepped", {
        goroutine: { id: 1 },
        location: { file: "a.go", line: 1 },
        frames: Array.from({ length: maximumGoroutines + 1 }, (_, index) => ({
          index,
          location: { file: "a.go", line: 1 },
        })),
      }),
    ],
  ];

  for (const [label, payload] of legalButLarge) {
    it(`stays recoverable on ${label}`, () => {
      const { observer, sockets, delays } = ladder();
      observer.start();
      const socket = sockets[0]!;
      socket.open();
      socket.emit("message", Buffer.from(payload, "utf8"));
      socket.emit("close");

      assert.equal(
        delays.length,
        1,
        "an event the contract does not bound must keep the reconnect ladder",
      );
      assert.notEqual(
        observer.model.connection,
        "error",
        "and must not latch the view dead",
      );
      observer.dispose();
    });
  }

  // This is the precondition that makes the generic walk's `bounded` flag latent,
  // asserted rather than asserted-in-a-comment. Neither bounded kind reaches that
  // walk today: GoroutineSnapshot is answered by the typed decoders (which is what
  // makes the mirror case below fatal), and Goroutines is shallow-parsed. So the
  // walk's size checks only ever see kinds the contract does not bound, which is
  // precisely why a size overrun there must stay transient.
  //
  // If a future change consumes Goroutines, or drops the snapshot's typed path,
  // this fails — and that is the point. The bounded family's fatality would then
  // depend on `bounded` actually classifying, so it must be re-proven there
  // rather than silently downgraded to a retry.
  it("keeps both bounded kinds out of the generic payload walk", () => {
    const snapshotDecoded = decodeEvent(
      frame(1, "GoroutineSnapshot", {
        goroutines: [{ id: 1, status: "running", current: true, currentLoc: { file: "a.go", line: 1 } }],
        threads: [],
        current: 1,
      }),
    );
    assert.notEqual(
      snapshotDecoded.snapshot,
      undefined,
      "GoroutineSnapshot must be answered by the typed decoder, not the generic walk",
    );

    const listDecoded = decodeEvent(frame(1, "Goroutines", { goroutines: richGoroutines(1) }));
    assert.deepEqual(
      listDecoded.payload,
      {},
      "Goroutines must stay shallow-parsed, never walked",
    );
  });

  it("still terminates on the same overrun inside a BOUNDED kind", () => {
    // The mirror image: the identical string cap IS a proven violation here,
    // because the producer packs this kind against exactly that limit.
    const { observer, sockets, delays } = ladder();
    observer.start();
    const socket = sockets[0]!;
    socket.open();
    socket.emit(
      "message",
      Buffer.from(
        frame(1, "GoroutineSnapshot", {
          goroutines: [
            {
              id: 1,
              status: "x".repeat(maximumStringLength + 1),
              current: true,
              currentLoc: { file: "a.go", line: 1 },
            },
          ],
          threads: [],
          current: 1,
        }),
        "utf8",
      ),
    );

    assert.equal(observer.model.connection, "error");
    assert.equal(delays.length, 0, "retrying would replay the identical frame");
    observer.dispose();
  });

  it("still reconnects after a genuine transport close", () => {
    const { observer, sockets, delays } = ladder();
    observer.start();
    sockets[0]!.open();
    sockets[0]!.close();

    assert.equal(delays.length, 1, "a transport close consumes one attempt");
    assert.equal(observer.model.connection, "reconnecting");
    delays[0]!.resolve();
    return Promise.resolve().then(() => {
      assert.equal(sockets.length, 2, "and redials");
      observer.dispose();
    });
  });

  it("stops the ladder mid-flight when a violation follows a transport close", async () => {
    const { observer, sockets, delays } = ladder();
    observer.start();
    sockets[0]!.open();
    sockets[0]!.close();
    assert.equal(delays.length, 1);
    delays[0]!.resolve();
    await Promise.resolve();

    const second = sockets[1]!;
    second.open();
    second.emit("message", Buffer.from("{not json", "utf8"));

    assert.equal(observer.model.connection, "error");
    assert.equal(delays.length, 1, "the violation must not consume another attempt");
    assert.equal(sockets.length, 2, "and must not redial");
    observer.dispose();
  });

  it("does not treat a shallow-parsed unused kind as a violation", () => {
    const unused = [
      "Output",
      "BreakpointSet",
      "BreakpointCleared",
      "Locals",
      "Frames",
      "Goroutines",
      "Evaluate",
      "Restarted",
    ];
    const { observer, sockets, delays } = ladder();
    observer.start();
    const socket = sockets[0]!;
    socket.open();

    let seq = 1;
    for (const kind of unused) {
      // Bodies that would fail deep validation outright: huge, and malformed.
      socket.emit("message", Buffer.from(frame(seq++, kind, { goroutines: richGoroutines(3000) }), "utf8"));
      socket.emit(
        "message",
        Buffer.from(
          frame(seq++, kind, {
            broken: [null, { nested: { deeper: "x".repeat(maximumStringLength + 1) } }],
            "unknown-field": 1,
          }),
          "utf8",
        ),
      );
    }

    assert.equal(observer.model.connection, "connected", "unused kinds are not violations");
    assert.equal(observer.model.error, "");
    assert.equal(observer.model.seqGap, "");
    assert.equal(delays.length, 0, "and consume no reconnect attempts");
    assert.equal(sockets.length, 1);
    assert.equal(socket.closed, false, "the connection stays open");
    assert.equal(observer.model.lastSeq, seq - 1, "they still advance the sequence");
    observer.dispose();
  });
});

describe("per-element string limit", () => {
  // The producer and this decoder must agree EXACTLY on what a legal element
  // looks like. A string one unit over the limit is nowhere near the byte
  // budget, so a producer that only budgeted bytes would happily emit an
  // element this decoder is obliged to reject — killing the connection
  // deterministically on every retry.
  const ascii = (n: number): string => "a".repeat(n);
  // U+1F600 is astral: one code point, TWO UTF-16 code units.
  const astral = (units: number): string => "\u{1F600}".repeat(units / 2);

  function snapshotWith(mutate: (g: Record<string, unknown>) => void): string {
    const goroutine: Record<string, unknown> = {
      id: 2,
      status: "waiting",
      currentLoc: { file: "a.go", line: 1 },
    };
    mutate(goroutine);
    return frame(1, "GoroutineSnapshot", {
      goroutines: [
        { id: 1, status: "running", current: true, currentLoc: { file: "a.go", line: 1 } },
        goroutine,
      ],
      threads: [],
      current: 1,
    });
  }

  const fields: readonly (readonly [string, (g: Record<string, unknown>, v: string) => void])[] = [
    ["status", (g, v) => { g.status = v; }],
    ["waitReason", (g, v) => { g.waitReason = v; }],
    ["currentLoc.file", (g, v) => { g.currentLoc = { file: v, line: 1 }; }],
    ["currentLoc.function", (g, v) => { g.currentLoc = { file: "a.go", line: 1, function: v }; }],
    ["startLoc.file", (g, v) => { g.startLoc = { file: v, line: 1 }; }],
    ["startLoc.function", (g, v) => { g.startLoc = { file: "a.go", line: 1, function: v }; }],
    ["createdLoc.file", (g, v) => { g.createdLoc = { file: v, line: 1 }; }],
    ["createdLoc.function", (g, v) => { g.createdLoc = { file: "a.go", line: 1, function: v }; }],
  ];

  for (const [label, set] of fields) {
    it(`accepts ${label} exactly at the limit`, () => {
      const decoded = decodeEvent(
        snapshotWith((g) => { set(g, ascii(maximumStringLength)); }),
      );
      assert.equal(decoded.snapshot?.goroutines.length, 2);
    });

    it(`rejects ${label} one unit over the limit`, () => {
      assert.throws(
        () => decodeEvent(snapshotWith((g) => { set(g, ascii(maximumStringLength + 1)); })),
        TelemetryProtocolError,
      );
    });
  }

  it("counts astral characters as two units, matching the producer", () => {
    const atLimit = astral(maximumStringLength);
    assert.equal(atLimit.length, maximumStringLength, "4096 UTF-16 units");
    assert.equal([...atLimit].length, maximumStringLength / 2, "but only 2048 code points");
    assert.doesNotThrow(() =>
      decodeEvent(snapshotWith((g) => { g.currentLoc = { file: atLimit, line: 1 }; })),
    );

    assert.throws(
      () =>
        decodeEvent(
          snapshotWith((g) => { g.currentLoc = { file: `${atLimit}a`, line: 1 }; }),
        ),
      TelemetryProtocolError,
    );
  });

  it("rejects an over-limit string on a thread", () => {
    const withThread = (file: string): string =>
      frame(1, "GoroutineSnapshot", {
        goroutines: [],
        threads: [{ id: 7, currentLoc: { file, line: 1 } }],
      });
    assert.doesNotThrow(() => decodeEvent(withThread(ascii(maximumStringLength))));
    assert.throws(
      () => decodeEvent(withThread(ascii(maximumStringLength + 1))),
      TelemetryProtocolError,
    );
  });

  it("counts replacement characters as one unit each", () => {
    // Invalid UTF-8 reaches the wire as one U+FFFD per bad byte (encoding/json)
    // and is one UTF-16 unit here, which is what lets the producer's count
    // agree with this one for non-UTF-8-clean tracee strings.
    const atLimit = "\uFFFD".repeat(maximumStringLength);
    assert.equal(atLimit.length, maximumStringLength);
    assert.doesNotThrow(() =>
      decodeEvent(snapshotWith((g) => { g.currentLoc = { file: atLimit, line: 1 }; })),
    );
    assert.throws(
      () =>
        decodeEvent(
          snapshotWith((g) => {
            g.currentLoc = { file: `${atLimit}\uFFFD`, line: 1 };
          }),
        ),
      TelemetryProtocolError,
    );
  });

  it("rejects a string of 4096 astral code points", () => {
    // Looks like "exactly at the limit" by code point, but is 8192 UTF-16
    // units — the case a rune-based producer limit would get backwards.
    const full = "\u{1F600}".repeat(maximumStringLength);
    assert.equal([...full].length, maximumStringLength);
    assert.equal(full.length, 2 * maximumStringLength);
    assert.throws(
      () => decodeEvent(snapshotWith((g) => { g.currentLoc = { file: full, line: 1 }; })),
      TelemetryProtocolError,
    );
  });

  it("pins the limit against the Go producer constant", () => {
    const source = goSource("pkg/protocol/protocol.go");
    assert.match(
      source,
      new RegExp(`MaxGoroutineStringLength\\s*=\\s*${String(maximumStringLength)}\\b`),
      "producer and consumer must cap element strings identically",
    );
  });
});

describe("totals must not contradict what arrived", () => {
  it("rejects a goroutine total below the delivered count", () => {
    assert.throws(
      () =>
        decodeEvent(
          frame(1, "GoroutineSnapshot", {
            goroutines: [
              { id: 1, status: "running", currentLoc: { file: "a.go", line: 1 } },
              { id: 2, status: "waiting", currentLoc: { file: "a.go", line: 1 } },
            ],
            threads: [],
            totals: { goroutines: 1 },
          }),
        ),
      /totals.goroutines \(1\) is below the 2 delivered/u,
    );
  });

  it("rejects a thread total below the delivered count", () => {
    assert.throws(
      () =>
        decodeEvent(
          frame(1, "GoroutineSnapshot", {
            goroutines: [],
            threads: [{ id: 7 }, { id: 8 }],
            totals: { goroutines: 0, threads: 1 },
          }),
        ),
      /totals.threads \(1\) is below the 2 delivered/u,
    );
  });

  it("accepts totals equal to and above the delivered counts", () => {
    for (const goroutines of [2, 3, 41203]) {
      const decoded = decodeEvent(
        frame(1, "GoroutineSnapshot", {
          goroutines: [
            { id: 1, status: "running", currentLoc: { file: "a.go", line: 1 } },
            { id: 2, status: "waiting", currentLoc: { file: "a.go", line: 1 } },
          ],
          threads: [],
          totals: { goroutines },
        }),
      );
      assert.equal(decoded.snapshot?.totals?.goroutines, goroutines);
    }
  });

  it("rejects a contradictory total in the goroutine list shape", () => {
    assert.throws(
      () =>
        decodeGoroutineList({
          goroutines: [{ id: 1, status: "running", currentLoc: { file: "a.go", line: 1 } }],
          totals: { goroutines: 0 },
        }),
      TelemetryProtocolError,
    );
    assert.throws(
      () =>
        decodeGoroutineList({
          goroutines: [],
          totals: { goroutines: 0, threads: 5 },
        }),
      TelemetryProtocolError,
      "this shape carries no threads, so a thread total is meaningless",
    );
  });
});

describe("current goroutine must be delivered", () => {
  // The producer's invariant: current is either 0 or names a goroutine in the
  // payload. A degraded result drops it rather than pointing at nothing.
  it("rejects a current goid that was not delivered", () => {
    assert.throws(
      () =>
        decodeEvent(
          frame(1, "GoroutineSnapshot", {
            goroutines: [
              { id: 1, status: "running", currentLoc: { file: "a.go", line: 1 } },
            ],
            threads: [],
            current: 999,
          }),
        ),
      /current goroutine 999 is not among the delivered goroutines/u,
    );
  });

  it("accepts a degraded snapshot that reports no current", () => {
    const decoded = decodeEvent(
      frame(1, "GoroutineSnapshot", {
        goroutines: [],
        threads: [],
        created: [7, 8],
        exited: [3],
        totals: { goroutines: 6000 },
      }),
    );
    assert.equal(decoded.snapshot?.current, 0);
    assert.deepEqual(decoded.snapshot?.created, [7, 8]);
    assert.deepEqual(decoded.snapshot?.exited, [3]);
    assert.equal(decoded.snapshot?.totals?.goroutines, 6000);
  });

  it("accepts a current goid that is present", () => {
    const decoded = decodeEvent(
      frame(1, "GoroutineSnapshot", {
        goroutines: [
          { id: 4, status: "running", current: true, currentLoc: { file: "a.go", line: 1 } },
        ],
        threads: [],
        current: 4,
      }),
    );
    assert.equal(decoded.snapshot?.current, 4);
  });
});

describe("truthful omission surfacing", () => {
  it("reports server omissions separately from the view cap", () => {
    const view = toSessionViewModel(
      baseModel({
        snapshot: {
          ...snapshot([goroutine(1, 0, { current: true }), goroutine(2, 1)], [thread(10, 1)]),
          totals: {
            goroutines: 41203,
            threads: 64,
            goroutinesClipped: false,
            threadsClipped: false,
          },
        },
      }),
    );
    assert.deepEqual(view.serverTotals, {
      goroutines: 41203,
      threads: 64,
      goroutinesClipped: false,
      threadsClipped: false,
      goroutinesOmitted: 41201,
      threadsOmitted: 63,
    });
    assert.equal(view.tree.omitted, 0, "the local view cap dropped nothing");
  });

  it("has no server totals on a complete snapshot", () => {
    const view = toSessionViewModel(baseModel({ snapshot: snapshot() }));
    assert.equal(view.serverTotals, undefined);
  });

  it("rejects a total below what arrived rather than normalizing it", () => {
    // The contradiction is refused at the decoder, so the view never has to
    // paper over it. Silently clamping would repair dishonest data into
    // something plausible and hide a broken producer.
    assert.throws(
      () =>
        decodeEvent(
          frame(1, "GoroutineSnapshot", {
            goroutines: [
              { id: 1, status: "running", currentLoc: { file: "a.go", line: 1 } },
              { id: 2, status: "waiting", currentLoc: { file: "a.go", line: 1 } },
            ],
            threads: [],
            totals: { goroutines: 1 },
          }),
        ),
      TelemetryProtocolError,
    );

    // And the view model reports exactly what the wire said, unmodified.
    const view = toSessionViewModel(
      baseModel({
        snapshot: {
          ...snapshot([goroutine(1, 0, { current: true }), goroutine(2, 1)]),
          totals: {
            goroutines: 9,
            threads: 4,
            goroutinesClipped: false,
            threadsClipped: false,
          },
        },
      }),
    );
    assert.equal(view.serverTotals?.goroutines, 9);
    assert.equal(view.serverTotals?.goroutinesOmitted, 7);
  });

  // The four scan-clipping combinations: each count must be marked a lower bound
  // only when ITS OWN scan clipped. The ceilings are independent, so a shared
  // flag necessarily lies about one of them.
  for (const [label, goroutinesClipped, threadsClipped] of [
    ["neither", false, false],
    ["goroutines only", true, false],
    ["threads only", false, true],
    ["both", true, true],
  ] as const) {
    it(`marks lower bounds independently: ${label}`, () => {
      const view = toSessionViewModel(
        baseModel({
          snapshot: {
            ...snapshot([goroutine(1, 0, { current: true })], [thread(10, 1)]),
            totals: {
              goroutines: 8192,
              threads: 64,
              goroutinesClipped,
              threadsClipped,
            },
          },
        }),
      );
      assert.equal(view.serverTotals?.goroutinesClipped, goroutinesClipped);
      assert.equal(view.serverTotals?.threadsClipped, threadsClipped);
      assert.equal(view.serverTotals?.goroutines, 8192);
      assert.equal(view.serverTotals?.threads, 64);

      // And the rendered statistic follows each flag, not a shared one.
      assert.equal(
        formatServerCount(1, 8192, goroutinesClipped).endsWith("+"),
        goroutinesClipped,
      );
      assert.equal(
        formatServerCount(1, 64, threadsClipped).endsWith("+"),
        threadsClipped,
      );
    });
  }

  // The tree's note speaks only for goroutines; the thread list carries its own,
  // beside the data it describes. Reporting threads in both places made one
  // shortfall read as two. The rendered-DOM specs in webviewDom.test.ts cover
  // that each collection's flag reaches its own panel and no other.
  it("keeps the tree's omission note scoped to goroutines", () => {
    const note = (goroutinesClipped: boolean, threadsClipped: boolean): string =>
      serverOmissionText({
        goroutines: 8192,
        threads: 2048,
        goroutinesClipped,
        threadsClipped,
        goroutinesOmitted: 0,
        threadsOmitted: 0,
      }) ?? "";

    assert.equal(note(false, false), "", "nothing to report");
    assert.match(note(true, false), /stopped after finding 8192 goroutines/u);
    assert.doesNotMatch(note(true, false), /threads/u);
    assert.equal(
      note(false, true),
      "",
      "a clipped THREAD scan is the thread list's business, not the tree's",
    );
    assert.match(note(true, true), /8192 goroutines/u);
    assert.doesNotMatch(note(true, true), /threads/u);
  });

  it("keeps a goroutine whose parent was omitted as a root", () => {
    // The packer can drop a middle-of-tree ancestor, so an orphan must still be
    // laid out rather than vanishing with its missing parent.
    const view = toSessionViewModel(
      baseModel({
        snapshot: {
          ...snapshot(
            [goroutine(1, 0, { current: true }), goroutine(99, 4242)],
            [thread(10, 1)],
          ),
          totals: {
            goroutines: 5000,
            threads: 2,
            goroutinesClipped: false,
            threadsClipped: false,
          },
        },
      }),
    );
    const orphan = view.tree.nodes.find((node) => node.goroutine.id === 99);
    assert.ok(orphan, "the orphan must still be laid out");
    assert.equal(orphan.depth, 0, "an omitted parent leaves the child a root");
  });
});

describe("real WebSocket transport", () => {
  const servers: { close(): void }[] = [];
  // Teardown must not depend on a test reaching its last line. These tests run
  // a real reconnect ladder against a real socket, so an observer left alive by
  // a failed assertion keeps timers and connections running and the runner
  // hangs instead of reporting the failure. Everything created is tracked here
  // and released unconditionally, so a broken expectation — or a mutation being
  // checked — terminates red rather than wedging.
  const observers: TelemetryObserver[] = [];
  const openSockets: { terminate(): void }[] = [];

  afterEach(() => {
    for (const observer of observers) {
      observer.dispose();
    }
    observers.length = 0;
    for (const socket of openSockets) {
      socket.terminate();
    }
    openSockets.length = 0;
    for (const server of servers) {
      server.close();
    }
    servers.length = 0;
  });

  after(() => {
    for (const server of servers) {
      server.close();
    }
  });

  // liveObserver runs the real TelemetryObserver against a real ws server, so
  // the transport limit, the decoder limit, and the reconnect policy are all
  // exercised end to end rather than through a fake socket.
  async function liveObserver(
    onConnection: (socket: WebSocket, connections: number) => void,
  ): Promise<{
    readonly observer: TelemetryObserver;
    readonly connections: () => number;
    readonly settled: () => Promise<void>;
  }> {
    const http: Server = createServer();
    const wss = new WebSocketServer({ server: http });
    let connections = 0;
    wss.on("connection", (socket) => {
      connections += 1;
      openSockets.push(socket);
      onConnection(socket, connections);
    });
    await new Promise<void>((resolve) => {
      http.listen(0, "127.0.0.1", resolve);
    });
    servers.push({
      close: () => {
        wss.close();
        http.close();
      },
    });
    const port = (http.address() as AddressInfo).port;

    const observer = new TelemetryObserver(
      {
        debugSessionId: "debug-1",
        debugSessionName: "live",
        sessionId: "s1",
        managementEndpoint: { host: "127.0.0.1", port },
      },
      {
        // The real production socket factory, so maxPayload and the ws
        // oversized-frame rejection are genuinely exercised. Only the reconnect
        // delay is shortened, so a ladder — if one starts — shows up in-test.
        ...observerDependencies(),
        delay: (_milliseconds, signal) =>
          new Promise((resolve, reject) => {
            if (signal.aborted) {
              reject(new Error("cancelled"));
              return;
            }
            setTimeout(resolve, 1);
          }),
      },
    );
    observers.push(observer);
    return {
      observer,
      connections: () => connections,
      settled: async () => {
        await new Promise((resolve) => setTimeout(resolve, 250));
      },
    };
  }

  it("releases every observer even when a test fails before disposing", async () => {
    // Pins the teardown itself. The ladder below never completes on its own, so
    // if afterEach did not dispose it the runner would hang here rather than
    // move on — which is exactly how a failed assertion used to wedge instead
    // of reporting red.
    const live = await liveObserver((socket) => {
      socket.close();
    });
    live.observer.start();
    await live.settled();
    assert.ok(live.connections() >= 1, "the observer must have connected");
    // Deliberately no dispose() — afterEach owns it.
  });

  it("survives repeated large snapshots without reconnecting", async () => {
    const stops = 8;
    const live = await liveObserver((socket) => {
      socket.on("message", () => {
        for (let stop = 1; stop <= stops; stop += 1) {
          socket.send(
            frame(stop, "GoroutineSnapshot", {
              goroutines: richGoroutines(1250),
              threads: [],
              current: 1,
              totals: {
                goroutines: 41203,
                threads: 64,
                goroutinesClipped: true,
                threadsClipped: false,
              },
            }),
          );
        }
      });
    });
    live.observer.start();
    await live.settled();

    assert.equal(live.connections(), 1, "a large-but-legal stream must not reconnect");
    assert.equal(live.observer.model.connection, "connected");
    assert.equal(live.observer.model.error, "");
    assert.equal(live.observer.model.snapshot?.goroutines.length, 1250);
    assert.equal(live.observer.model.snapshot?.totals?.goroutinesClipped, true);
    assert.equal(live.observer.model.lastSeq, stops);
    live.observer.dispose();
  });

  it("fails once without reconnecting on a frame just above the decoder cap", async () => {
    const live = await liveObserver((socket) => {
      socket.on("message", () => {
        const filler = "p".repeat(maximumEnvelopeBytes);
        socket.send(
          JSON.stringify({
            v: wireProtocolVersion,
            kind: "GoroutineSnapshot",
            seq: 1,
            payload: { goroutines: [], threads: [], filler },
          }),
        );
      });
    });
    live.observer.start();
    await live.settled();

    assert.equal(live.connections(), 1, "a contract violation must not be retried");
    assert.equal(live.observer.model.connection, "error");
    assert.match(live.observer.model.error, /byte contract/);
    live.observer.dispose();
  });

  // hugeFrame builds a legal, well-formed event of `kind` whose marshalled size
  // is at least `bytes` — the shape a real oversized broadcast has, as opposed
  // to unparseable filler.
  function hugeFrame(kind: string, bytes: number): string {
    return frame(1, kind, { variables: [{ value: "v".repeat(bytes) }] });
  }

  it("reconnects after a legal ignored event above the transport cap", async () => {
    // The production scenario: a user expands a large variable, the hub
    // broadcasts EventLocals to EVERY client, and it lands on the observer —
    // which does not even read that kind. Locals/Frames/Evaluate are
    // deliberately outside the goroutine byte contract, so this is not a
    // contract violation and must not latch the view dead. Above the transport
    // cap `ws` has already discarded the frame, so the kind is unknowable and
    // the only safe classification is "transient".
    let oversized = 0;
    const live = await liveObserver((socket, connections) => {
      socket.on("message", () => {
        if (connections < 3) {
          oversized += 1;
          socket.send(hugeFrame("Locals", maximumTransportBytes + 1));
          return;
        }
        socket.send(frame(1, "GoroutineSnapshot", { goroutines: [], threads: [] }));
      });
    });
    live.observer.start();
    await live.settled();

    assert.ok(oversized >= 1, "the oversized Locals frame must actually have been sent");
    assert.ok(
      live.connections() >= 3,
      `a legal ignored event must reconnect, not latch; got ${String(live.connections())}`,
    );
    assert.equal(live.observer.model.connection, "connected");
    live.observer.dispose();
  });

  it("reconnects after a legal ignored event between the decoder and transport caps", async () => {
    // Delivered (so the kind IS readable) but over the decoder budget. The
    // prefix scan proves it is an unbounded kind, so it stays transient too.
    let oversized = 0;
    const live = await liveObserver((socket, connections) => {
      socket.on("message", () => {
        if (connections < 3) {
          oversized += 1;
          socket.send(hugeFrame("Locals", maximumEnvelopeBytes + 1));
          return;
        }
        socket.send(frame(1, "GoroutineSnapshot", { goroutines: [], threads: [] }));
      });
    });
    live.observer.start();
    await live.settled();

    assert.ok(oversized >= 1);
    assert.ok(
      live.connections() >= 3,
      `an oversized unbounded kind must reconnect; got ${String(live.connections())}`,
    );
    assert.equal(live.observer.model.connection, "connected");
    live.observer.dispose();
  });

  it("latches on a bounded kind one byte over the decoder cap", async () => {
    // The other half of the matrix: the prefix scan proves this IS a goroutine
    // event, so it is a contract violation the producer will reproduce exactly.
    const live = await liveObserver((socket) => {
      socket.on("message", () => {
        socket.send(hugeFrame("GoroutineSnapshot", maximumEnvelopeBytes + 1));
      });
    });
    live.observer.start();
    await live.settled();

    assert.equal(live.connections(), 1, "a proven violation must not be retried");
    assert.equal(live.observer.model.connection, "error");
    assert.match(live.observer.model.error, /byte contract/u);
    live.observer.dispose();
  });

  it("reconnects after a frame above the transport slack", async () => {
    // Unparseable filler above the transport cap: no kind can ever be read, so
    // the safe classification is the same transient one.
    let oversized = 0;
    const live = await liveObserver((socket, connections) => {
      socket.on("message", () => {
        if (connections < 3) {
          oversized += 1;
          socket.send("z".repeat(maximumTransportBytes + 1));
          return;
        }
        socket.send(frame(1, "GoroutineSnapshot", { goroutines: [], threads: [] }));
      });
    });
    live.observer.start();
    await live.settled();

    assert.ok(oversized >= 1, "the oversized frame must actually have been sent");
    assert.ok(
      live.connections() >= 3,
      `an unclassifiable oversized frame must reconnect, got ${String(live.connections())}`,
    );
    assert.equal(live.observer.model.connection, "connected");
    live.observer.dispose();
  });

  it("recovers from a fatal protocol error on an explicit refresh", async () => {
    // The fatal latch stops the AUTOMATIC ladder, which would replay the same
    // bad frame. An explicit user action is not a loop, so Refresh must redial.
    let stops = 0;
    const live = await liveObserver((socket) => {
      socket.on("message", () => {
        stops += 1;
        if (stops === 1) {
          socket.send(
            JSON.stringify({
              v: wireProtocolVersion,
              kind: "GoroutineSnapshot",
              seq: 1,
              payload: { goroutines: [], threads: [], filler: "p".repeat(maximumEnvelopeBytes) },
            }),
          );
          return;
        }
        socket.send(frame(1, "GoroutineSnapshot", { goroutines: [], threads: [] }));
      });
    });
    live.observer.start();
    await live.settled();

    assert.equal(live.observer.model.connection, "error", "the violation must latch");
    assert.equal(live.connections(), 1, "and must not retry on its own");

    live.observer.refresh();
    await live.settled();

    assert.equal(live.connections(), 2, "an explicit refresh redials");
    assert.equal(live.observer.model.connection, "connected");
    live.observer.dispose();
  });

  it("still reconnects after a genuine transport close", async () => {
    const live = await liveObserver((socket, connections) => {
      if (connections < 3) {
        socket.close();
        return;
      }
      socket.on("message", () => {
        socket.send(frame(1, "GoroutineSnapshot", { goroutines: [], threads: [] }));
      });
    });
    live.observer.start();
    await live.settled();

    assert.ok(
      live.connections() >= 3,
      `a transient close must still reconnect, got ${String(live.connections())}`,
    );
    live.observer.dispose();
  });
});
