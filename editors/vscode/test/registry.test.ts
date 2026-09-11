import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { describe, it } from "node:test";

import type { ObserverDependencies, Socket } from "../src/observer.js";
import { emptyInspection } from "../src/model.js";
import { SessionRegistry } from "../src/registry.js";
import { emptySource } from "../src/sourceModel.js";

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
  it("keeps source context session-local and never revives removed source on ID reuse", () => {
    const registry = new SessionRegistry({
      createSocket: () => new SocketStub(),
      delay: () => Promise.resolve(),
      now: () => 0,
    });
    const base = {
      debugSessionName: "source",
      managementEndpoint: { host: "127.0.0.1", port: 6060 },
    };
    const a = { ...base, debugSessionId: "a", sessionId: "one" };
    registry.add(a);
    registry.add({ ...base, debugSessionId: "b", sessionId: "two" });
    const source = {
      status: "ready" as const,
      message: "local source",
      lines: [{ number: 5, text: "go worker()", highlighted: true }],
    };
    assert.equal(registry.updateSource("a", source), true);
    assert.equal(registry.viewModel.sessions.find((s) => s.debugSessionId === "a")?.spawnSource, source);
    assert.equal(registry.viewModel.sessions.find((s) => s.debugSessionId === "b")?.spawnSource, emptySource);
    assert.equal(registry.viewModel.activeDebugSessionId, "b");
    registry.select("a");
    registry.remove("a");
    const revision = registry.viewModel.revision;
    assert.equal(registry.updateSource("a", source), false);
    assert.equal(registry.viewModel.revision, revision);
    registry.add({ ...a, sessionId: "replacement" });
    assert.equal(registry.viewModel.sessions.find((s) => s.debugSessionId === "a")?.spawnSource, emptySource);
    registry.dispose();
    assert.equal(registry.updateSource("a", source), false);
    assert.equal(registry.viewModel.sessions.length, 0);
  });

  it("publishes only the newest source when source initialization reenters notification", () => {
    const registry = new SessionRegistry({
      createSocket: () => new SocketStub(),
      delay: () => Promise.resolve(),
      now: () => 0,
    });
    registry.add({
      debugSessionId: "a", debugSessionName: "source", sessionId: "one",
      managementEndpoint: { host: "127.0.0.1", port: 6060 },
    });
    let initialize = true;
    const stopUpdating = registry.onChange(() => {
      if (initialize) {
        initialize = false;
        registry.updateSource("a", { status: "loading", message: "new selection", lines: [] });
      }
    });
    const observed: string[] = [];
    const stopObserving = registry.onChange((view) => {
      observed.push(view.sessions[0]!.spawnSource.message);
    });
    registry.select("a");
    assert.deepEqual(observed, ["new selection"]);
    stopUpdating();
    stopObserving();
    registry.dispose();
  });

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
    const inspection = {
      ...emptyInspection(1),
      stackStatus: "ready" as const,
      stackMessage: "",
    };
    assert.equal(registry.updateInspection("a", inspection), true);
    assert.equal(registry.inspectionFor("a"), inspection);
    assert.equal(
      registry.viewModel.sessions.find(
        (session) => session.debugSessionId === "a",
      )?.inspection,
      inspection,
    );
    const delivered: number[] = [];
    let updateFromListener = true;
    const unsubscribeUpdater = registry.onChange(() => {
      if (!updateFromListener) {
        return;
      }
      updateFromListener = false;
      registry.updateInspection("a", {
        ...inspection,
        stackMessage: "newest",
      });
    });
    const unsubscribeReader = registry.onChange((next) => {
      delivered.push(next.revision);
      assert.equal(
        next.sessions.find((session) => session.debugSessionId === "a")
          ?.inspection.stackMessage,
        "newest",
      );
    });
    registry.select("a");
    assert.equal(delivered.length, 1);
    unsubscribeUpdater();
    unsubscribeReader();
    registry.remove("a");
    assert.equal(sockets[0]?.closed, true);
    assert.equal(registry.inspectionFor("a"), undefined);
    assert.equal(registry.viewModel.activeDebugSessionId, "b");
    registry.dispose();
    assert.equal(sockets[1]?.closed, true);
  });
});
