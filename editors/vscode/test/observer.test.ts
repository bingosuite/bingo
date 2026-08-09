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
  readonly delays: { resolve: () => void }[];
} {
  const sockets: FakeSocket[] = [];
  const delays: { resolve: () => void }[] = [];
  const dependencies: ObserverDependencies = {
    createSocket() {
      const socket = new FakeSocket();
      sockets.push(socket);
      return socket;
    },
    delay(_milliseconds, signal) {
      return new Promise((resolve, reject) => {
        const delay = { resolve };
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
      v: "1.3",
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
