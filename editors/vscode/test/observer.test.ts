import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { describe, it } from "node:test";

import {
  TelemetryObserver,
  telemetryURL,
  type ObserverDependencies,
  type Socket,
} from "../src/observer.js";
import { envelope, snapshot } from "./fixtures.js";

class FakeSocket extends EventEmitter implements Socket {
  public readyState = 0;
  public readonly sent: string[] = [];
  public closed = false;

  public onOpen(listener: () => void): void {
    this.on("open", listener);
  }

  public onMessage(listener: (data: Buffer) => void): void {
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

function setup(): {
  readonly observer: TelemetryObserver;
  readonly sockets: FakeSocket[];
  readonly delays: { resolve: () => void; milliseconds: number }[];
} {
  const sockets: FakeSocket[] = [];
  const delays: { resolve: () => void; milliseconds: number }[] = [];
  const dependencies: ObserverDependencies = {
    createSocket() {
      const socket = new FakeSocket();
      sockets.push(socket);
      return socket;
    },
    delay(milliseconds, signal) {
      return new Promise((resolve, reject) => {
        const delay = { resolve, milliseconds };
        delays.push(delay);
        signal.addEventListener("abort", () => {
          reject(new Error("cancelled"));
        });
      });
    },
    now: () => 123,
  };
  return {
    observer: new TelemetryObserver(
      {
        debugSessionId: "debug-1",
        debugSessionName: "level5",
        sessionId: "session/a",
        managementEndpoint: { host: "127.0.0.1", port: 6060 },
      },
      dependencies,
    ),
    sockets,
    delays,
  };
}

describe("telemetry observer", () => {
  it("builds encoded WS URLs including IPv6 hosts", () => {
    assert.equal(
      telemetryURL({ host: "::1", port: 6060 }, "id/a"),
      "ws://[::1]:6060/ws?session=id%2Fa",
    );
  });

  it("requests exactly one join snapshot and explicit refreshes only", () => {
    const { observer, sockets } = setup();
    observer.start();
    const socket = sockets[0]!;
    socket.open();
    assert.equal(socket.sent.length, 1);
    assert.deepEqual(JSON.parse(socket.sent[0]!), {
      v: "1.4",
      kind: "GoroutineSnapshot",
      payload: {},
    });
    socket.emit("message", envelope(1, "GoroutineSnapshot", snapshot()));
    socket.emit("message", envelope(2, "BreakpointHit", { breakpoint: { location: { file: "main.go", line: 4 } } }));
    assert.equal(socket.sent.length, 1);
    observer.refresh();
    assert.equal(socket.sent.length, 2);
    assert.ok(
      socket.sent.every(
        (item) =>
          (JSON.parse(item) as { readonly kind?: unknown }).kind ===
          "GoroutineSnapshot",
      ),
    );
    observer.dispose();
  });

  it("tracks gaps, rejects duplicates, and preserves selection across snapshots", () => {
    const { observer, sockets } = setup();
    observer.start();
    const socket = sockets[0]!;
    socket.open();
    socket.emit("message", envelope(1, "GoroutineSnapshot", snapshot()));
    observer.selectGoroutine(1);
    socket.emit("message", envelope(3, "GoroutineSnapshot", snapshot()));
    assert.equal(observer.model.seqGap, "missed events 2-2");
    assert.equal(observer.model.selectedGoroutine, 1);
    socket.emit("message", envelope(3, "ProcessExited", { exitCode: 0 }));
    assert.notEqual(observer.model.sessionState, "exited");
    socket.emit("message", envelope(2, "ProcessExited", { exitCode: 0 }));
    assert.equal(observer.model.sessionState, "exited");
    assert.equal(observer.model.seqGap, "out-of-order event 2 after 3");
    assert.equal(observer.model.lastSeq, 3);
    observer.dispose();
  });

  it("reconnects only while live and cancels pending reconnect on dispose", async () => {
    const { observer, sockets, delays } = setup();
    observer.start();
    sockets[0]!.open();
    sockets[0]!.close();
    assert.equal(observer.model.connection, "reconnecting");
    delays[0]!.resolve();
    await Promise.resolve();
    assert.equal(sockets.length, 2);
    sockets[1]!.close();
    observer.dispose();
    delays[1]!.resolve();
    await Promise.resolve();
    assert.equal(sockets.length, 2);
  });

  it("bounds repeated reconnects even when every short-lived socket opens", async () => {
    const { observer, sockets, delays } = setup();
    observer.start();
    for (let attempt = 0; attempt < 6; attempt += 1) {
      const socket = sockets[attempt]!;
      socket.open();
      socket.close();
      delays[attempt]!.resolve();
      await new Promise((resolve) => setImmediate(resolve));
    }
    assert.equal(sockets.length, 7);
    sockets[6]!.open();
    sockets[6]!.close();
    assert.equal(observer.model.connection, "error");
    assert.match(observer.model.error, /reconnect limit/);
    observer.dispose();
  });

  // Exhausting the ladder is terminal but never latches `fatal`, so a Refresh
  // that only re-sent a snapshot command would silently do nothing — there is no
  // socket left to send it on — and the panel would stay dead for the rest of
  // the session with no way back. Refresh must redial from EVERY terminal state.
  it("redials on refresh after the reconnect ladder is exhausted", async () => {
    const { observer, sockets, delays } = setup();
    observer.start();
    for (let attempt = 0; attempt < 6; attempt += 1) {
      const socket = sockets[attempt]!;
      socket.open();
      socket.close();
      delays[attempt]!.resolve();
      await new Promise((resolve) => setImmediate(resolve));
    }
    sockets[6]!.open();
    sockets[6]!.close();
    assert.equal(observer.model.connection, "error");
    const exhausted = sockets.length;

    observer.refresh();
    assert.equal(sockets.length, exhausted + 1, "refresh must open a new socket");
    assert.equal(observer.model.connection, "connecting");

    sockets[exhausted]!.open();
    assert.equal(observer.model.connection, "connected");
    assert.equal(
      sockets[exhausted]!.sent.length,
      1,
      "a recovered connection asks for a snapshot so the view repopulates",
    );

    // Recovery must restore the BUDGET, not merely dial once. Asserting the
    // label above would pass a regression that redials with the ladder still
    // spent, making Refresh single-use: the panel comes back and dies on the
    // next blip, which is the failure this spec exists to prevent.
    const spent = delays.length;
    sockets[exhausted]!.close();
    assert.notEqual(
      observer.model.connection,
      "error",
      "a recovered connection must not be one blip from terminal",
    );
    assert.equal(observer.model.connection, "reconnecting");
    assert.equal(delays.length, spent + 1, "the disconnect must enter the ladder");
    assert.equal(
      delays[spent]!.milliseconds,
      100,
      "the recovered ladder restarts at its floor rather than resuming where it stopped",
    );
    observer.dispose();
  });

  // Refresh bumps #connectionEpoch, and the sleeping #reconnect re-checks it
  // after its delay. Without that check a refresh issued during a pending
  // backoff races a second live socket into #socket and leaks the loser. The
  // budget spec above cannot see this: it drives a ladder that has already been
  // exhausted, so no backoff is ever in flight while refresh runs.
  it("does not double-dial when refresh interrupts a pending backoff", async () => {
    const { observer, sockets, delays } = setup();
    observer.start();
    sockets[0]!.open();
    sockets[0]!.close();
    assert.equal(delays.length, 1, "a backoff is pending");

    observer.refresh();
    assert.equal(sockets.length, 2, "refresh dials once");

    delays[0]!.resolve();
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(
      sockets.length,
      2,
      "the superseded backoff must abandon its dial rather than open a second socket",
    );
    observer.dispose();
  });

  it("closes a malformed stream instead of accepting unvalidated data", () => {
    const { observer, sockets } = setup();
    observer.start();
    const socket = sockets[0]!;
    socket.open();
    socket.emit(
      "message",
      Buffer.from('{"v":"9","kind":"Continued","seq":1,"payload":{}}'),
    );
    assert.equal(socket.closed, true);
    assert.match(observer.model.error, /incompatible/);
    observer.dispose();
  });
});
