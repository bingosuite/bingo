import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { resolve } from "node:path";
import { after, describe, it } from "node:test";

import type WebSocket from "ws";
import { WebSocketServer } from "ws";

import { toSessionViewModel, type SessionModel } from "../src/model.js";
import { observerDependencies, TelemetryObserver } from "../src/observer.js";
import {
  decodeEvent,
  decodeGoroutineList,
  maximumEnvelopeBytes,
  maximumGoroutines,
  maximumThreads,
  maximumTransportBytes,
  TelemetryProtocolError,
  transportSlackBytes,
  wireProtocolVersion,
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
    assert.throws(
      () => decodeEvent(frame(1, "SessionState", { sessionID: "a", state: "nope", clients: 0 })),
      TelemetryProtocolError,
    );
    assert.throws(
      () => decodeEvent(frame(1, "Error", { message: "x".repeat(5000) })),
      TelemetryProtocolError,
    );
    assert.throws(
      () => decodeEvent(frame(1, "GoroutineSnapshot", { goroutines: [], threads: [], nope: 1 })),
      TelemetryProtocolError,
    );
    assert.throws(
      () => decodeEvent(frame(1, "BreakpointHit", { frames: richGoroutines(3000) })),
      TelemetryProtocolError,
    );
  });
});

describe("snapshot totals", () => {
  it("decodes a full totals object", () => {
    const decoded = decodeEvent(
      frame(1, "GoroutineSnapshot", {
        goroutines: [{ id: 1, status: "running", current: true, currentLoc: { file: "a.go", line: 1 } }],
        threads: [],
        totals: { goroutines: 41203, threads: 64, clipped: true },
      }),
    );
    assert.deepEqual(decoded.snapshot?.totals, {
      goroutines: 41203,
      threads: 64,
      clipped: true,
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
      clipped: false,
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
      totals: { goroutines: 8192, clipped: true },
    });
    assert.equal(list.goroutines.length, 1);
    assert.deepEqual(list.totals, { goroutines: 8192, threads: 0, clipped: true });
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
  // reaches 8192 — well past the 5000 packed-element cap. Rejecting those was a
  // false positive that, now that protocol errors are terminal, killed the view
  // permanently on exactly the workload this contract exists for.
  function deltaFrame(count: number): string {
    const ids: number[] = [];
    for (let id = 1; id <= count; id += 1) {
      ids.push(id);
    }
    return frame(1, "GoroutineSnapshot", {
      goroutines: [
        { id: 1, status: "running", current: true, currentLoc: { file: "a.go", line: 1 } },
      ],
      threads: [],
      current: 1,
      created: ids,
      exited: ids.map((id) => id + 1_000_000),
    });
  }

  it("accepts a delta larger than the packed-element cap", () => {
    const decoded = decodeEvent(deltaFrame(maximumGoroutines + 1));
    assert.equal(decoded.snapshot?.created.length, maximumGoroutines + 1);
    assert.equal(decoded.snapshot?.exited.length, maximumGoroutines + 1);
  });

  it("accepts the largest delta the runtime scan can produce", () => {
    const decoded = decodeEvent(deltaFrame(8192));
    assert.equal(decoded.snapshot?.created.length, 8192);
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

describe("truthful omission surfacing", () => {
  it("reports server omissions separately from the view cap", () => {
    const view = toSessionViewModel(
      baseModel({
        snapshot: {
          ...snapshot([goroutine(1, 0, { current: true }), goroutine(2, 1)], [thread(10, 1)]),
          totals: { goroutines: 41203, threads: 64, clipped: false },
        },
      }),
    );
    assert.deepEqual(view.serverTotals, {
      goroutines: 41203,
      threads: 64,
      clipped: false,
      goroutinesOmitted: 41201,
      threadsOmitted: 63,
    });
    assert.equal(view.tree.omitted, 0, "the local view cap dropped nothing");
  });

  it("has no server totals on a complete snapshot", () => {
    const view = toSessionViewModel(baseModel({ snapshot: snapshot() }));
    assert.equal(view.serverTotals, undefined);
  });

  it("never reports a total below what actually arrived", () => {
    const view = toSessionViewModel(
      baseModel({
        snapshot: {
          ...snapshot([goroutine(1, 0, { current: true }), goroutine(2, 1)]),
          totals: { goroutines: 1, threads: 0, clipped: false },
        },
      }),
    );
    assert.equal(view.serverTotals?.goroutines, 2);
    assert.equal(view.serverTotals?.goroutinesOmitted, 0);
  });

  it("marks a clipped scan as a lower bound", () => {
    const view = toSessionViewModel(
      baseModel({
        snapshot: {
          ...snapshot([goroutine(1, 0, { current: true })], [thread(10, 1)]),
          totals: { goroutines: 8192, threads: 64, clipped: true },
        },
      }),
    );
    assert.equal(view.serverTotals?.clipped, true);
    assert.equal(view.serverTotals?.threads, 64);
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
          totals: { goroutines: 5000, threads: 2, clipped: false },
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
    return {
      observer,
      connections: () => connections,
      settled: async () => {
        await new Promise((resolve) => setTimeout(resolve, 250));
      },
    };
  }

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
              totals: { goroutines: 41203, threads: 64, clipped: true },
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
    assert.equal(live.observer.model.snapshot?.totals?.clipped, true);
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

  it("fails once without reconnecting on a frame above the transport slack", async () => {
    const live = await liveObserver((socket) => {
      socket.on("message", () => {
        socket.send("z".repeat(maximumTransportBytes + 1));
      });
    });
    live.observer.start();
    await live.settled();

    assert.equal(live.connections(), 1, "an oversized frame must not be retried");
    assert.equal(live.observer.model.connection, "error");
    assert.match(live.observer.model.error, /transport limit/);
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
