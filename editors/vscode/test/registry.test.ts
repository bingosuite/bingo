import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { describe, it } from "node:test";

import type { ObserverDependencies, Socket } from "../src/observer.js";
import { SessionRegistry } from "../src/registry.js";

class SocketStub extends EventEmitter implements Socket {
  public readyState = 0;
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
  public send(): void {}
  public close(): void {
    this.closed = true;
  }
}

describe("session registry", () => {
  it("supports multiple debug sessions, active selection, deduplication, and teardown", () => {
    const sockets: SocketStub[] = [];
    const dependencies: ObserverDependencies = {
      createSocket() {
        const socket = new SocketStub();
        sockets.push(socket);
        return socket;
      },
      delay: () => Promise.resolve(),
      now: () => 0,
    };
    const registry = new SessionRegistry(dependencies);
    const base = {
      debugSessionName: "session",
      managementEndpoint: { host: "127.0.0.1", port: 6060 },
    };
    assert.equal(registry.add({ ...base, debugSessionId: "a", sessionId: "one" }), true);
    assert.equal(registry.add({ ...base, debugSessionId: "a", sessionId: "duplicate" }), false);
    assert.equal(registry.add({ ...base, debugSessionId: "b", sessionId: "two" }), true);
    assert.equal(registry.viewModel.sessions.length, 2);
    assert.equal(registry.viewModel.activeDebugSessionId, "b");
    assert.equal(registry.select("a"), true);
    assert.equal(registry.viewModel.activeDebugSessionId, "a");
    registry.remove("a");
    assert.equal(sockets[0]?.closed, true);
    assert.equal(registry.viewModel.activeDebugSessionId, "b");
    registry.dispose();
    assert.equal(sockets[1]?.closed, true);
  });
});
