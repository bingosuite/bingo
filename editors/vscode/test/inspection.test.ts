import assert from "node:assert/strict";
import { describe, it, type TestContext } from "node:test";

import {
  DebugInspectionController,
  decodeScopeReferences,
  decodeStackFrames,
  decodeVariables,
  inspectionLimits,
  type DebugSessionClient,
  type InspectionRegistry,
} from "../src/inspection.js";
import type { DebugInspection, SessionModel } from "../src/model.js";
import { emptyInspection } from "../src/model.js";
import type { Snapshot } from "../src/telemetry.js";
import { goroutine, snapshot } from "./fixtures.js";

class RegistryStub implements InspectionRegistry {
  readonly #listeners = new Set<() => void>();
  readonly inspections = new Map<string, DebugInspection>();
  updates = 0;

  public constructor(public model: SessionModel) {
    this.inspections.set(
      model.debugSessionId,
      emptyInspection(model.selectedGoroutine),
    );
  }

  public onChange(listener: () => void): () => void {
    this.#listeners.add(listener);
    return () => {
      this.#listeners.delete(listener);
    };
  }

  public activeModel(): SessionModel {
    return this.model;
  }

  public inspectionFor(debugSessionId: string): DebugInspection | undefined {
    return this.inspections.get(debugSessionId);
  }

  public updateInspection(
    debugSessionId: string,
    inspection: DebugInspection,
  ): boolean {
    if (!this.inspections.has(debugSessionId)) {
      return false;
    }
    this.updates += 1;
    this.inspections.set(debugSessionId, inspection);
    this.emit();
    return true;
  }

  public replaceSnapshot(next: Snapshot, selectedGoroutine: number): void {
    this.model = {
      ...this.model,
      snapshot: next,
      selectedGoroutine,
    };
    this.emit();
  }

  public setSessionState(sessionState: string): void {
    this.model = {
      ...this.model,
      sessionState,
    };
    this.emit();
  }

  public selectGoroutine(selectedGoroutine: number): void {
    this.model = {
      ...this.model,
      selectedGoroutine,
    };
    this.emit();
  }

  public replaceSession(debugSessionId: string, sessionId: string): void {
    this.model = { ...this.model, debugSessionId, sessionId };
    if (!this.inspections.has(debugSessionId)) {
      this.inspections.set(debugSessionId, emptyInspection());
    }
    this.emit();
  }

  public emit(): void {
    for (const listener of [...this.#listeners]) {
      listener();
    }
  }
}

function suspendedModel(value: Snapshot): SessionModel {
  return {
    debugSessionId: "debug",
    debugSessionName: "Debug",
    sessionId: "session",
    connection: "connected",
    sessionState: "suspended",
    clients: 1,
    lastStop: "Breakpoint",
    error: "",
    seqGap: "",
    lastSeq: 1,
    snapshot: value,
    selectedGoroutine: value.current,
    timeline: [],
  };
}

function settle(): Promise<void> {
  return new Promise((resolve) => {
    setImmediate(resolve);
  });
}

function deferred() {
  let resolve!: (value: unknown) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<unknown>((accept, fail) => {
    resolve = accept;
    reject = fail;
  });
  return { promise, resolve, reject };
}

class ControlledClient implements DebugSessionClient {
  readonly id = "debug";
  readonly requests: {
    readonly command: string;
    readonly args: unknown;
    readonly result: ReturnType<typeof deferred>;
    answered: boolean;
  }[] = [];

  public customRequest(command: string, args: unknown): Promise<unknown> {
    const result = deferred();
    this.requests.push({ command, args, result, answered: false });
    return result.promise;
  }

  public next(command: string) {
    const request = this.requests.find(
      (request) => request.command === command && !request.answered,
    );
    assert.ok(request, `no pending ${command} request`);
    request.answered = true;
    return request;
  }
}

class Clock {
  readonly pending = new Set<() => void>();

  readonly schedule = (callback: () => void, milliseconds: number) => {
    assert.equal(milliseconds, inspectionLimits.timeoutMs);
    this.pending.add(callback);
    return () => { this.pending.delete(callback); };
  };

  public expire(): void {
    for (const callback of [...this.pending]) {
      this.pending.delete(callback);
      callback();
    }
  }
}

const stack = {
  stackFrames: [
    { id: 1, name: "first", line: 1, column: 1 },
    { id: 2, name: "second", line: 2, column: 1 },
  ],
};

function variable(reference = 0, value = "value", name = "name") {
  return { name, value, type: "", variablesReference: reference };
}

function harness(context: TestContext) {
  const registry = new RegistryStub(suspendedModel(snapshot([
    goroutine(1, 0, { current: true }),
    goroutine(2, 1),
  ])));
  const client = new ControlledClient();
  const clock = new Clock();
  const controller = new DebugInspectionController(
    registry,
    (id) => id === client.id ? client : undefined,
    clock.schedule,
  );
  context.after(() => { controller.dispose(); });
  return { registry, client, clock, controller };
}

async function reachScopes(h: ReturnType<typeof harness>): Promise<void> {
  h.client.next("stackTrace").result.resolve(stack);
  await settle();
}

async function reachVariables(
  h: ReturnType<typeof harness>,
  references: readonly number[] = [10],
): Promise<void> {
  await reachScopes(h);
  h.client.next("scopes").result.resolve({
    scopes: references.map((variablesReference) => ({ variablesReference })),
  });
  await settle();
}

async function loadRoots(
  h: ReturnType<typeof harness>,
  variables = [variable(99)],
): Promise<void> {
  await reachVariables(h);
  h.client.next("variables").result.resolve({ variables });
  await settle();
  assert.equal(h.registry.inspectionFor("debug")?.localsStatus, "ready");
}

describe("debug inspection controller", () => {
  it("loads the stopped stack, frame locals, and expandable children", async () => {
    const registry = new RegistryStub(suspendedModel(snapshot()));
    const requests: {
      readonly command: string;
      readonly args: unknown;
    }[] = [];
    const client: DebugSessionClient = {
      id: "debug",
      customRequest(command, args) {
        requests.push({ command, args });
        switch (command) {
          case "stackTrace":
            return Promise.resolve({
              stackFrames: [
                {
                  id: 1,
                  name: "main.worker",
                  source: { path: "/workspace/main.go" },
                  line: 42,
                  column: 3,
                },
                {
                  id: 2,
                  name: "main.main",
                  source: { path: "/workspace/main.go" },
                  line: 12,
                  column: 1,
                },
              ],
            });
          case "scopes":
            return Promise.resolve({
              scopes: [{ variablesReference: 10 }],
            });
          case "variables":
            return Promise.resolve(
              (args as { variablesReference: number }).variablesReference ===
                65_536
                ? {
                    variables: [
                      {
                        name: "0",
                        value: '"one"',
                        type: "string",
                        variablesReference: 0,
                      },
                    ],
                  }
                : {
                    variables: [
                      {
                        name: "jobs",
                        value: "[]string len: 1, cap: 1",
                        type: "[]string",
                        variablesReference: 65_536,
                      },
                    ],
                  },
            );
          default:
            throw new Error(`unexpected request ${command}`);
        }
      },
    };
    const controller = new DebugInspectionController(
      registry,
      () => client,
    );
    await settle();

    const loaded = registry.inspectionFor("debug");
    assert.equal(loaded?.stackStatus, "ready");
    assert.equal(loaded?.frames.length, 2);
    assert.equal(loaded?.selectedFrameId, 1);
    assert.equal(loaded?.localsStatus, "ready");
    assert.equal(loaded?.variables[0]?.name, "jobs");
    assert.deepEqual(requests.slice(0, 3), [
      {
        command: "stackTrace",
        args: { threadId: 1, startFrame: 0, levels: 200 },
      },
      { command: "scopes", args: { frameId: 1 } },
      { command: "variables", args: { variablesReference: 10 } },
    ]);

    controller.selectFrame(2);
    await settle();
    assert.equal(registry.inspectionFor("debug")?.selectedFrameId, 2);

    controller.expandVariable(65_536);
    await settle();
    assert.equal(
      registry.inspectionFor("debug")?.variablesByReference["65536"]?.[0]
        ?.value,
      '"one"',
    );
    controller.dispose();
  });

  it("does not request an unsupported non-current goroutine stack", async () => {
    const value = snapshot([
      goroutine(1, 0, { current: true }),
      goroutine(2, 1),
    ]);
    const model = {
      ...suspendedModel(value),
      selectedGoroutine: 2,
    };
    const registry = new RegistryStub(model);
    let requests = 0;
    const controller = new DebugInspectionController(registry, () => ({
      id: "debug",
      customRequest() {
        requests += 1;
        return Promise.resolve({});
      },
    }));
    await settle();

    assert.equal(requests, 0);
    assert.equal(
      registry.inspectionFor("debug")?.stackStatus,
      "unavailable",
    );
    assert.match(
      registry.inspectionFor("debug")?.stackMessage ?? "",
      /stopped goroutine g1/,
    );
    controller.dispose();
  });

  it("rejects a late stack response from an older snapshot", async () => {
    const first = snapshot([goroutine(1, 0, { current: true })]);
    const second = snapshot([goroutine(2, 0, { current: true })]);
    const registry = new RegistryStub(suspendedModel(first));
    let resolveFirst:
      | ((value: {
          stackFrames: readonly Record<string, unknown>[];
        }) => void)
      | undefined;
    let stackRequests = 0;
    const client: DebugSessionClient = {
      id: "debug",
      customRequest(command) {
        if (command === "stackTrace") {
          stackRequests += 1;
          if (stackRequests === 1) {
            return new Promise((resolve) => {
              resolveFirst = resolve;
            });
          }
          return Promise.resolve({
            stackFrames: [
              {
                id: 2,
                name: "new.frame",
                source: { path: "/workspace/new.go" },
                line: 2,
                column: 1,
              },
            ],
          });
        }
        if (command === "scopes") {
          return Promise.resolve({ scopes: [] });
        }
        throw new Error(`unexpected request ${command}`);
      },
    };
    const controller = new DebugInspectionController(
      registry,
      () => client,
    );
    registry.replaceSnapshot(second, 2);
    await settle();
    resolveFirst?.({
      stackFrames: [
        {
          id: 1,
          name: "stale.frame",
          source: { path: "/workspace/stale.go" },
          line: 1,
          column: 1,
        },
      ],
    });
    await settle();

    assert.equal(registry.inspectionFor("debug")?.frames[0]?.name, "new.frame");
    controller.dispose();
  });

  it("refreshes inspection from an identity-less DAP step stop", async () => {
    const model = {
      ...suspendedModel(
        snapshot([
          goroutine(1, 0, { current: true }),
          goroutine(2, 1),
        ]),
      ),
      sessionState: "running",
    };
    const registry = new RegistryStub(model);
    const requests: {
      readonly command: string;
      readonly args: unknown;
    }[] = [];
    const controller = new DebugInspectionController(registry, () => ({
      id: "debug",
      customRequest(command, args) {
        requests.push({ command, args });
        if (command === "stackTrace") {
          return Promise.resolve({
            stackFrames: [
              {
                id: 5,
                name: "main.afterStep",
                source: { path: "/workspace/main.go" },
                line: 20,
                column: 1,
              },
            ],
          });
        }
        return Promise.resolve({ scopes: [] });
      },
    }));

    controller.stopped("debug", 0);
    registry.setSessionState("suspended");
    await settle();

    assert.deepEqual(requests[0], {
      command: "stackTrace",
      args: { threadId: 0, startFrame: 0, levels: 200 },
    });
    assert.equal(registry.inspectionFor("debug")?.targetGoroutine, 0);
    assert.equal(
      registry.inspectionFor("debug")?.frames[0]?.name,
      "main.afterStep",
    );
    registry.selectGoroutine(2);
    await settle();
    assert.equal(registry.inspectionFor("debug")?.targetGoroutine, 0);
    assert.equal(registry.model.selectedGoroutine, 2);
    controller.dispose();
  });

  it("rejects unknown and stale variable expansion references", async () => {
    const registry = new RegistryStub(suspendedModel(snapshot()));
    let resolveChildren: ((value: unknown) => void) | undefined;
    const variableReferences: number[] = [];
    const controller = new DebugInspectionController(registry, () => ({
      id: "debug",
      customRequest(command, args) {
        if (command === "stackTrace") {
          return Promise.resolve({
            stackFrames: [
              { id: 1, name: "first", line: 1, column: 1 },
              { id: 2, name: "second", line: 2, column: 1 },
            ],
          });
        }
        if (command === "scopes") {
          return Promise.resolve({
            scopes: [
              {
                variablesReference:
                  (args as { frameId: number }).frameId === 1 ? 10 : 20,
              },
            ],
          });
        }
        const reference = (args as { variablesReference: number })
          .variablesReference;
        variableReferences.push(reference);
        if (reference === 10) {
          return Promise.resolve({
            variables: [
              {
                name: "old",
                value: "old",
                variablesReference: 99,
              },
            ],
          });
        }
        if (reference === 20) {
          return Promise.resolve({
            variables: [
              {
                name: "current",
                value: "current",
                variablesReference: 0,
              },
            ],
          });
        }
        return new Promise((resolve) => {
          resolveChildren = resolve;
        });
      },
    }));
    await settle();

    controller.expandVariable(123_456);
    assert.deepEqual(variableReferences, [10]);
    controller.expandVariable(99);
    controller.selectFrame(2);
    await settle();
    resolveChildren?.({
      variables: [
        {
          name: "stale child",
          value: "stale",
          variablesReference: 0,
        },
      ],
    });
    await settle();

    const inspection = registry.inspectionFor("debug");
    assert.equal(inspection?.selectedFrameId, 2);
    assert.equal(inspection?.variables[0]?.name, "current");
    assert.equal(inspection?.variablesByReference["99"], undefined);
    controller.dispose();
  });
});

describe("debug inspection response codecs", () => {
  it("decodes bounded DAP stack, scope, and variable responses", () => {
    assert.deepEqual(
      decodeStackFrames({
        stackFrames: [
          {
            id: 1,
            name: "main.main",
            source: { path: "/workspace/main.go" },
            line: 9,
            column: 2,
          },
        ],
      }),
      [
        {
          id: 1,
          name: "main.main",
          file: "/workspace/main.go",
          line: 9,
          column: 2,
        },
      ],
    );
    assert.deepEqual(
      decodeScopeReferences({
        scopes: [
          { variablesReference: 4 },
          { variablesReference: 0 },
        ],
      }),
      [4],
    );
    assert.deepEqual(
      decodeVariables({
        variables: [
          {
            name: "answer",
            value: "42",
            type: "int",
            variablesReference: 0,
          },
        ],
      }),
      [
        {
          name: "answer",
          value: "42",
          type: "int",
          variablesReference: 0,
        },
      ],
    );
    assert.throws(
      () => decodeStackFrames({ stackFrames: [{ id: 0 }] }),
      /stack frame id/,
    );
    assert.throws(
      () => decodeVariables({ variables: "not-an-array" }),
      /must be an array/,
    );
  });
});

describe("inspection generations and deferred requests", () => {
  const transitions = {
    resume(h: ReturnType<typeof harness>) {
      h.registry.setSessionState("running");
    },
    "DAP resume before WebSocket state"(h: ReturnType<typeof harness>) {
      h.controller.resumed("debug");
    },
    selection(h: ReturnType<typeof harness>) {
      h.registry.selectGoroutine(2);
    },
    snapshot(h: ReturnType<typeof harness>) {
      h.registry.replaceSnapshot(snapshot([goroutine(1, 0, { current: true })]), 1);
    },
    session(h: ReturnType<typeof harness>) {
      h.registry.replaceSession("other", "other-session");
    },
    "native session"(h: ReturnType<typeof harness>) {
      h.registry.replaceSession("debug", "replacement-session");
    },
    forget(h: ReturnType<typeof harness>) {
      h.controller.forgetSession("debug");
    },
    stop(h: ReturnType<typeof harness>) {
      h.controller.stopped("debug", 1);
    },
    refresh(h: ReturnType<typeof harness>) {
      h.controller.refresh();
    },
    dispose(h: ReturnType<typeof harness>) {
      h.controller.dispose();
    },
  };
  for (const phase of ["stack", "scopes", "variables", "children"] as const) {
    for (const [name, transition] of Object.entries(transitions)) {
      it(`ignores late ${phase} after ${name} without further requests`, async (t) => {
        const h = harness(t);
        if (phase === "scopes") {
          await reachScopes(h);
        } else if (phase === "variables") {
          await reachVariables(h, [10, 20]);
        } else if (phase === "children") {
          await loadRoots(h);
          h.controller.expandVariable(99);
        }
        const command = phase === "stack" ? "stackTrace"
          : phase === "scopes" ? "scopes" : "variables";
        const request = h.client.next(command);
        transition(h);
        await settle();
        const count = h.client.requests.length;
        const updates = h.registry.updates;
        request.result.resolve(phase === "stack" ? stack
          : phase === "scopes" ? { scopes: [{ variablesReference: 88 }] }
          : { variables: [variable(100, "stale")] });
        await settle();
        assert.equal(h.client.requests.length, count);
        assert.equal(h.registry.updates, updates);
        assert.equal(h.clock.pending.size, name === "snapshot" ||
          name === "native session" || name === "stop" || name === "refresh" ? 1 : 0);
      });
    }
  }

  for (const phase of ["scopes", "variables", "children"] as const) {
    it(`rejects an older frame's ${phase} while the new frame wins out of order`, async (t) => {
      const h = harness(t);
      if (phase === "scopes") {
        await reachScopes(h);
      } else if (phase === "variables") {
        await reachVariables(h, [10, 20]);
      } else {
        await loadRoots(h);
        h.controller.expandVariable(99);
      }
      const request = h.client.next(phase === "scopes" ? "scopes" : "variables");
      h.controller.selectFrame(2);
      h.client.next("scopes").result.resolve({ scopes: [{ variablesReference: 50 }] });
      await settle();
      h.client.next("variables").result.resolve({ variables: [variable(0, "new frame")] });
      await settle();
      const inspection = h.registry.inspectionFor("debug");
      const count = h.client.requests.length;
      request.result.resolve(phase === "scopes"
        ? { scopes: [{ variablesReference: 99 }] }
        : { variables: [variable(99, "stale frame")] });
      await settle();
      assert.equal(h.registry.inspectionFor("debug"), inspection);
      assert.equal(inspection?.selectedFrameId, 2);
      assert.equal(inspection.variables[0]?.value, "new frame");
      assert.equal(h.client.requests.length, count);
    });
  }

  it("checks a generation after every scope's variable response, including synchronous state changes", async (t) => {
    const h = harness(t);
    await reachVariables(h, [10, 20, 30]);
    const response = h.client.next("variables");
    h.registry.setSessionState("running");
    response.result.resolve({ variables: [variable()] });
    await settle();
    assert.deepEqual(h.client.requests.map((r) => r.command), [
      "stackTrace", "scopes", "variables",
    ]);
    assert.equal(h.registry.inspectionFor("debug")?.stackStatus, "idle");
  });

  it("does not issue locals when a registry listener resumes during stack publication", async (t) => {
    const h = harness(t);
    h.registry.onChange(() => {
      if (h.registry.inspectionFor("debug")?.stackStatus === "ready") {
        h.registry.setSessionState("running");
      }
    });
    await reachScopes(h);
    assert.deepEqual(h.client.requests.map((r) => r.command), ["stackTrace"]);
    assert.equal(h.registry.inspectionFor("debug")?.stackStatus, "idle");
  });

  it("checks disposal before all public actions and future registry callbacks", async (t) => {
    const h = harness(t);
    await loadRoots(h);
    h.controller.dispose();
    h.controller.dispose();
    const count = h.client.requests.length;
    const updates = h.registry.updates;
    h.controller.refresh();
    h.controller.selectFrame(2);
    h.controller.expandVariable(99);
    h.controller.stopped("debug", 1);
    h.controller.resumed("debug");
    h.controller.forgetSession("debug");
    h.registry.replaceSnapshot(snapshot(), 1);
    h.registry.setSessionState("running");
    h.registry.setSessionState("suspended");
    await settle();
    assert.equal(h.client.requests.length, count);
    assert.equal(h.registry.updates, updates);
    assert.equal(h.clock.pending.size, 0);
  });

  it("does not change active inspection when an unrelated session is forgotten", async (t) => {
    const h = harness(t);
    h.controller.forgetSession("unrelated");
    await loadRoots(h);
    assert.equal(h.registry.inspectionFor("debug")?.localsStatus, "ready");
  });

  it("blocks all inspection while DAP is running but WebSocket still reports suspended", async (t) => {
    const h = harness(t);
    await loadRoots(h);
    h.controller.resumed("debug");
    assert.equal(h.registry.model.sessionState, "suspended");
    assert.equal(h.registry.inspectionFor("debug")?.stackStatus, "idle");
    const count = h.client.requests.length;
    h.controller.refresh();
    h.controller.selectFrame(2);
    h.controller.expandVariable(99);
    h.registry.replaceSnapshot(snapshot(), 1);
    h.registry.emit();
    await settle();
    assert.equal(h.client.requests.length, count);
    assert.equal(h.registry.inspectionFor("debug")?.stackStatus, "idle");
    assert.equal(h.clock.pending.size, 0);
  });

  it("a later DAP stop reopens inspection and preserves unknown stopped identity", async (t) => {
    const h = harness(t);
    await loadRoots(h);
    h.controller.resumed("debug");
    h.controller.stopped("debug", 0);
    const request = h.client.next("stackTrace");
    assert.deepEqual(request.args, {
      threadId: 0,
      startFrame: 0,
      levels: inspectionLimits.frames,
    });
    request.result.resolve(stack);
    await settle();
    h.client.next("scopes").result.resolve({ scopes: [] });
    await settle();
    assert.equal(h.registry.inspectionFor("debug")?.stackStatus, "ready");
    assert.equal(h.registry.inspectionFor("debug")?.localsStatus, "ready");
    assert.equal(h.registry.inspectionFor("debug")?.targetGoroutine, 0);
  });

  it("an inactive session's DAP resume does not invalidate the active session", async (t) => {
    const h = harness(t);
    await loadRoots(h);
    const inspection = h.registry.inspectionFor("debug");
    h.controller.resumed("other");
    h.controller.expandVariable(99);
    assert.equal(h.client.requests.length, 4);
    h.client.next("variables").result.resolve({ variables: [] });
    await settle();
    assert.equal(h.registry.inspectionFor("debug")?.frames, inspection?.frames);
    assert.equal(h.registry.inspectionFor("debug")?.localsStatus, "ready");
  });

  it("rejects an old same-frame response even after frame selection returns to its original id", async (t) => {
    const h = harness(t);
    await reachScopes(h);
    const original = h.client.next("scopes");
    h.controller.selectFrame(2);
    const intermediate = h.client.next("scopes");
    h.controller.selectFrame(1);
    h.client.next("scopes").result.resolve({ scopes: [{ variablesReference: 50 }] });
    await settle();
    h.client.next("variables").result.resolve({ variables: [variable(0, "latest")] });
    await settle();
    const inspection = h.registry.inspectionFor("debug");
    for (const request of [original, intermediate]) {
      request.result.resolve({ scopes: [{ variablesReference: 99 }] });
    }
    await settle();
    assert.equal(h.registry.inspectionFor("debug"), inspection);
    assert.equal(inspection?.selectedFrameId, 1);
    assert.equal(inspection?.variables[0]?.value, "latest");
    assert.equal(h.client.requests.length, 5);
  });
});

describe("inspection reference ownership", () => {
  it("deduplicates scopes and shares root, child, and cyclic references without more requests", async (t) => {
    const h = harness(t);
    await reachVariables(h, [10, 10, 0, 10]);
    h.client.next("variables").result.resolve({
      variables: [variable(99, "a"), variable(99, "b"), variable(10, "scope cycle")],
    });
    await settle();
    h.controller.expandVariable(99);
    h.controller.expandVariable(99);
    h.client.next("variables").result.resolve({
      variables: [variable(99, "self"), variable(10, "root")],
    });
    await settle();
    h.controller.expandVariable(10);
    h.controller.expandVariable(99);
    h.controller.expandVariable(10);
    await settle();
    assert.equal(h.client.requests.length, 4);
    const inspection = h.registry.inspectionFor("debug");
    assert.equal(inspection?.variablesByReference["10"]?.[2]?.value, "scope cycle");
    assert.equal(inspection?.variablesByReference["99"]?.[0]?.value, "self");
    assert.deepEqual(inspection?.loadingReferences, []);
  });

  it("merges concurrent children that finish in reverse order", async (t) => {
    const h = harness(t);
    await loadRoots(h, [variable(98), variable(99)]);
    h.controller.expandVariable(98);
    h.controller.expandVariable(99);
    const first = h.client.next("variables");
    const second = h.client.next("variables");
    second.result.resolve({ variables: [variable(0, "second")] });
    await settle();
    assert.deepEqual(h.registry.inspectionFor("debug")?.loadingReferences, [98]);
    first.result.resolve({ variables: [variable(0, "first")] });
    await settle();
    assert.equal(h.registry.inspectionFor("debug")?.variablesByReference["98"]?.[0]?.value, "first");
    assert.equal(h.registry.inspectionFor("debug")?.variablesByReference["99"]?.[0]?.value, "second");
    assert.deepEqual(h.registry.inspectionFor("debug")?.loadingReferences, []);
  });

  it("rejects unknown, fractional, infinite, and otherwise malformed UI references", async (t) => {
    const h = harness(t);
    await loadRoots(h);
    const count = h.client.requests.length;
    for (const reference of [0, -1, 0.5, NaN, Infinity, -Infinity, Number.MAX_SAFE_INTEGER + 1, 10, 100]) {
      h.controller.expandVariable(reference);
      h.controller.selectFrame(reference);
    }
    assert.equal(h.client.requests.length, count);
    assert.deepEqual(h.registry.inspectionFor("debug")?.loadingReferences, []);
  });

  it("does not repeat failed child requests or poison a later frame with their errors", async (t) => {
    const h = harness(t);
    await loadRoots(h);
    h.controller.expandVariable(99);
    h.client.next("variables").result.reject(new Error("child unavailable"));
    await settle();
    const inspection = h.registry.inspectionFor("debug");
    assert.match(inspection?.localsMessage ?? "", /Cannot expand variable: child unavailable/);
    assert.deepEqual(inspection?.loadingReferences, []);
    h.controller.expandVariable(99);
    assert.equal(h.client.requests.length, 4);
    h.controller.selectFrame(2);
    h.client.next("scopes").result.resolve({ scopes: [{ variablesReference: 10 }] });
    await settle();
    h.client.next("variables").result.resolve({ variables: [variable(99)] });
    await settle();
    h.controller.expandVariable(99);
    assert.equal(h.client.requests.length, 7);
    h.client.next("variables").result.resolve({ variables: [] });
    await settle();
    assert.equal(h.registry.inspectionFor("debug")?.localsMessage, "");
  });

  it("a malformed scope reference fails before any variable request", async (t) => {
    const h = harness(t);
    await reachScopes(h);
    h.client.next("scopes").result.resolve({
      scopes: [{ variablesReference: 10 }, { variablesReference: 1.5 }],
    });
    await settle();
    assert.equal(h.client.requests.length, 2);
    assert.equal(h.registry.inspectionFor("debug")?.localsStatus, "error");
    assert.match(h.registry.inspectionFor("debug")?.localsMessage ?? "", /scope variablesReference/);
  });
});

describe("aggregate inspection budgets", () => {
  for (const extraScope of [false, true]) {
    it(`keeps ${extraScope ? "over-limit" : "exact-limit"} scope roots bounded across the whole frame`, async (t) => {
      const h = harness(t);
      const count = inspectionLimits.variableNodes / inspectionLimits.variablesPerResponse;
      await reachVariables(h, Array.from({ length: count + Number(extraScope) }, (_, i) => i + 10));
      for (let i = 0; i < count; i += 1) {
        h.client.next("variables").result.resolve({
          variables: Array.from({ length: inspectionLimits.variablesPerResponse }, () => variable()),
        });
        await settle();
      }
      const inspection = h.registry.inspectionFor("debug");
      assert.equal(inspection?.variables.length, inspectionLimits.variableNodes);
      assert.equal(inspection?.localsStatus, "ready");
      assert.equal(h.client.requests.length, count + 2);
      assert.equal(inspection?.localsMessage.includes("node limit"), extraScope);
    });
  }

  it("charges expanded descendants against the root budget and stops deep fanout before sending", async (t) => {
    const h = harness(t);
    const group = (reference: number) => Array.from(
      { length: inspectionLimits.variablesPerResponse },
      (_, index) => variable(index === 0 ? reference : 0),
    );
    await loadRoots(h, group(100));
    for (let i = 1; i < inspectionLimits.variableNodes / inspectionLimits.variablesPerResponse; i += 1) {
      h.controller.expandVariable(99 + i);
      h.client.next("variables").result.resolve({ variables: group(100 + i) });
      await settle();
    }
    const before = h.client.requests.length;
    h.controller.expandVariable(103);
    await settle();
    const inspection = h.registry.inspectionFor("debug");
    const nodes = (inspection?.variables.length ?? 0) +
      Object.values(inspection?.variablesByReference ?? {}).reduce((sum, children) => sum + children.length, 0);
    assert.equal(nodes, inspectionLimits.variableNodes);
    assert.equal(h.client.requests.length, before);
    assert.match(inspection?.localsMessage ?? "", /truncated.*node limit/);
    assert.deepEqual(inspection?.loadingReferences, []);
  });

  it("shares the remaining node budget between concurrently completing responses", async (t) => {
    const h = harness(t);
    const root = Array.from({ length: inspectionLimits.variablesPerResponse }, (_, i) => variable(i < 4 ? i + 100 : 0));
    await loadRoots(h, root);
    for (let i = 0; i < 4; i += 1) {
      h.controller.expandVariable(i + 100);
    }
    const pending = Array.from({ length: 4 }, () => h.client.next("variables")).reverse();
    for (const request of pending) {
      request.result.resolve({
        variables: Array.from({ length: inspectionLimits.variablesPerResponse }, () => variable()),
      });
    }
    await settle();
    const inspection = h.registry.inspectionFor("debug");
    const nodes = (inspection?.variables.length ?? 0) +
      Object.values(inspection?.variablesByReference ?? {}).reduce((sum, children) => sum + children.length, 0);
    assert.equal(nodes, inspectionLimits.variableNodes);
    assert.match(inspection?.localsMessage ?? "", /truncated.*node limit/);
    assert.deepEqual(inspection?.loadingReferences, []);
  });

  it("charges a cached scope alias again before duplicating it into the outgoing model", async (t) => {
    const h = harness(t);
    await reachVariables(h, [10, 20, 30, 40]);
    for (let i = 0; i < 4; i += 1) {
      h.client.next("variables").result.resolve({
        variables: Array.from(
          { length: inspectionLimits.variablesPerResponse },
          () => variable(10),
        ),
      });
      await settle();
    }
    h.controller.expandVariable(10);
    await settle();
    const inspection = h.registry.inspectionFor("debug");
    assert.equal(h.client.requests.length, 6);
    assert.equal(inspection?.variables.length, inspectionLimits.variableNodes);
    assert.deepEqual(inspection?.variablesByReference["10"], []);
    assert.match(inspection?.localsMessage ?? "", /node limit/);
  });

  for (const extra of [0, 1]) {
    it(`enforces ${extra === 0 ? "exact" : "overflow"} UTF-8 aggregate text bytes rather than UTF-16 length`, async (t) => {
      const h = harness(t);
      const value = "😀".repeat(inspectionLimits.textBytes / 4);
      const count = inspectionLimits.variableBytes / inspectionLimits.textBytes;
      await loadRoots(h, Array.from({ length: count + extra }, () => variable(99, value, "")));
      const inspection = h.registry.inspectionFor("debug");
      assert.equal(inspection?.variables.length, count);
      assert.equal(inspection?.variables.reduce((sum, entry) =>
        sum + Buffer.byteLength(entry.name + entry.value + entry.type, "utf8"), 0), inspectionLimits.variableBytes);
      assert.equal(inspection?.localsMessage.includes("UTF-8 byte limit"), extra > 0);
      const before = h.client.requests.length;
      h.controller.expandVariable(99);
      await settle();
      assert.equal(h.client.requests.length, before);
      assert.match(h.registry.inspectionFor("debug")?.localsMessage ?? "", /UTF-8 byte limit/);
    });
  }

  it("charges names and types, not just values, against the byte ceiling", async (t) => {
    const h = harness(t);
    const variables = Array.from({ length: 6 }, () => ({
      ...variable(0, "v".repeat(inspectionLimits.textBytes), "n".repeat(inspectionLimits.textBytes)),
      type: "t".repeat(inspectionLimits.textBytes),
    }));
    await loadRoots(h, variables);
    const inspection = h.registry.inspectionFor("debug");
    assert.equal(inspection?.variables.length, 5);
    assert.match(inspection?.localsMessage ?? "", /UTF-8 byte limit/);
  });

  for (const extra of [0, 1]) {
    it(`caps ${extra === 0 ? "exact" : "overflow"} distinct references including scope roots`, async (t) => {
      const h = harness(t);
      await loadRoots(h, Array.from(
        { length: inspectionLimits.references - 1 + extra },
        (_, i) => variable(1_000 + i),
      ));
      const inspection = h.registry.inspectionFor("debug");
      assert.equal(inspection?.variables.length, inspectionLimits.references - 1);
      assert.equal(inspection?.localsMessage.includes("reference limit"), extra > 0);
      if (extra > 0) {
        h.controller.expandVariable(1_000);
        assert.equal(h.client.requests.length, 3);
      }
    });
  }

  it("caps aggregate empty-child requests without relying on node or byte limits", async (t) => {
    const h = harness(t);
    await loadRoots(h, Array.from({ length: inspectionLimits.requests }, (_, i) => variable(1_000 + i)));
    for (let i = 0; i < inspectionLimits.requests - 2; i += 1) {
      h.controller.expandVariable(1_000 + i);
      h.client.next("variables").result.resolve({ variables: [] });
      await settle();
    }
    assert.equal(h.client.requests.length, inspectionLimits.requests + 1);
    assert.equal(h.registry.inspectionFor("debug")?.localsMessage, "");
    h.controller.expandVariable(1_000 + inspectionLimits.requests - 2);
    await settle();
    assert.equal(h.client.requests.length, inspectionLimits.requests + 1);
    assert.match(h.registry.inspectionFor("debug")?.localsMessage ?? "", /truncated.*request limit/);
  });

  it("caps a deep chain's requests and renews budgets for a new frame", async (t) => {
    const h = harness(t);
    await loadRoots(h, [variable(100)]);
    for (let i = 0; i < inspectionLimits.requests - 2; i += 1) {
      h.controller.expandVariable(100 + i);
      h.client.next("variables").result.resolve({ variables: [variable(101 + i)] });
      await settle();
    }
    h.controller.expandVariable(100 + inspectionLimits.requests - 2);
    await settle();
    assert.match(h.registry.inspectionFor("debug")?.localsMessage ?? "", /request limit/);
    h.controller.selectFrame(2);
    h.client.next("scopes").result.resolve({ scopes: [{ variablesReference: 10 }] });
    await settle();
    h.client.next("variables").result.resolve({ variables: [variable(100)] });
    await settle();
    h.controller.expandVariable(100);
    h.client.next("variables").result.resolve({ variables: [variable()] });
    await settle();
    assert.equal(h.registry.inspectionFor("debug")?.localsMessage, "");
    assert.equal(Object.keys(h.registry.inspectionFor("debug")?.variablesByReference ?? {}).length, 1);
  });
});

describe("inspection request lifetime and failures", () => {
  it("bounds concurrent expansions, clears rejected loading state, and permits an unsent retry", async (t) => {
    const h = harness(t);
    await loadRoots(h, Array.from({ length: inspectionLimits.inFlight + 1 }, (_, i) => variable(100 + i)));
    for (let i = 0; i <= inspectionLimits.inFlight; i += 1) {
      h.controller.expandVariable(100 + i);
    }
    await settle();
    assert.equal(h.client.requests.length, 3 + inspectionLimits.inFlight);
    assert.equal(h.registry.inspectionFor("debug")?.loadingReferences.length, inspectionLimits.inFlight);
    assert.match(h.registry.inspectionFor("debug")?.localsMessage ?? "", /in flight/);
    h.client.next("variables").result.resolve({ variables: [] });
    await settle();
    h.controller.expandVariable(100 + inspectionLimits.inFlight);
    assert.equal(h.client.requests.length, 4 + inspectionLimits.inFlight);
    const retried = h.client.requests.at(-1);
    assert.ok(retried);
    retried.result.resolve({ variables: [] });
    await settle();
    assert.equal(h.registry.inspectionFor("debug")?.localsMessage, "");
  });

  it("retains wire slots after deadlines so repeated refreshes cannot accumulate hung requests", async (t) => {
    const h = harness(t);
    for (let i = 0; i < inspectionLimits.inFlight; i += 1) {
      if (i > 0) {
        h.controller.refresh();
      }
      assert.equal(h.clock.pending.size, 1);
      h.clock.expire();
      await settle();
      assert.equal(h.registry.inspectionFor("debug")?.stackStatus, "error");
      assert.match(h.registry.inspectionFor("debug")?.stackMessage ?? "", /stackTrace request timed out/);
    }
    h.controller.refresh();
    await settle();
    assert.equal(h.client.requests.length, inspectionLimits.inFlight);
    assert.equal(h.clock.pending.size, 0);
    assert.match(h.registry.inspectionFor("debug")?.stackMessage ?? "", /in flight/);
    h.client.next("stackTrace").result.reject(new Error("late transport failure"));
    await settle();
    h.controller.refresh();
    assert.equal(h.client.requests.length, inspectionLimits.inFlight + 1);
    assert.equal(h.clock.pending.size, 1);
  });

  it("retains wire slots across stale generations even when none of their UI waiters remain", async (t) => {
    const h = harness(t);
    for (let i = 0; i < inspectionLimits.inFlight + 10; i += 1) {
      h.controller.refresh();
    }
    await settle();
    assert.equal(h.client.requests.length, inspectionLimits.inFlight);
    assert.equal(h.clock.pending.size, 0);
    assert.match(h.registry.inspectionFor("debug")?.stackMessage ?? "", /in flight/);
    h.controller.dispose();
    for (const request of h.client.requests) {
      request.result.reject(new Error("late rejection"));
    }
    await settle();
    assert.equal(h.clock.pending.size, 0);
  });

  for (const phase of ["scopes", "variables", "children"] as const) {
    it(`reports ${phase} deadlines accurately without follow-up requests or permanent spinners`, async (t) => {
      const h = harness(t);
      if (phase === "scopes") {
        await reachScopes(h);
      } else if (phase === "variables") {
        await reachVariables(h, [10, 20]);
      } else {
        await loadRoots(h);
        h.controller.expandVariable(99);
      }
      const before = h.client.requests.length;
      h.clock.expire();
      await settle();
      const inspection = h.registry.inspectionFor("debug");
      assert.equal(inspection?.localsStatus, phase === "children" ? "ready" : "error");
      assert.match(inspection?.localsMessage ?? "", /request timed out/);
      assert.deepEqual(inspection?.loadingReferences, []);
      assert.equal(h.client.requests.length, before);
      const updates = h.registry.updates;
      h.client.next(phase === "scopes" ? "scopes" : "variables").result.resolve(
        phase === "scopes" ? { scopes: [{ variablesReference: 88 }] }
          : { variables: [variable(100, "late")] },
      );
      await settle();
      assert.equal(h.client.requests.length, before);
      assert.equal(h.registry.updates, updates);
    });
  }

  it("reports missing or mismatched sessions without issuing a request", (t) => {
    for (const wrongSession of [false, true]) {
      const registry = new RegistryStub(suspendedModel(snapshot()));
      let calls = 0;
      const controller = new DebugInspectionController(registry, () => wrongSession ? {
        id: "not-debug",
        customRequest() {
          calls += 1;
          return Promise.resolve({});
        },
      } : undefined);
      t.after(() => { controller.dispose(); });
      assert.equal(calls, 0);
      assert.equal(registry.inspectionFor("debug")?.stackStatus, "error");
      assert.match(registry.inspectionFor("debug")?.stackMessage ?? "", /matching VS Code debug session/);
    }
  });

  it("reports session loss between stack and scope without issuing the scope", async (t) => {
    const registry = new RegistryStub(suspendedModel(snapshot()));
    const client = new ControlledClient();
    let available = true;
    const controller = new DebugInspectionController(registry, () => available ? client : undefined);
    t.after(() => { controller.dispose(); });
    available = false;
    client.next("stackTrace").result.resolve(stack);
    await settle();
    assert.equal(client.requests.length, 1);
    assert.equal(registry.inspectionFor("debug")?.localsStatus, "error");
    assert.match(registry.inspectionFor("debug")?.localsMessage ?? "", /matching VS Code debug session/);
  });

  for (const error of [
    new Error("failed synchronously"),
    "plain rejection",
    { toString() { throw new Error("must not coerce arbitrary rejection"); } },
    Object.defineProperty(new Error(), "message", {
      get() { throw new Error("must not invoke message getters"); },
    }),
  ]) {
    it("normalizes synchronous transport failures without coercing arbitrary objects", async (t) => {
      const registry = new RegistryStub(suspendedModel(snapshot()));
      const controller = new DebugInspectionController(registry, () => ({
        id: "debug",
        customRequest() {
          // The transport boundary must also survive non-Error throws.
          // eslint-disable-next-line @typescript-eslint/only-throw-error
          throw error;
        },
      }));
      t.after(() => { controller.dispose(); });
      await settle();
      assert.equal(registry.inspectionFor("debug")?.stackStatus, "error");
      assert.match(registry.inspectionFor("debug")?.stackMessage ?? "", /Cannot load the call stack:/);
    });
  }

  it("bounds adapter error strings", async (t) => {
    const h = harness(t);
    h.client.next("stackTrace").result.reject(new Error("x".repeat(100_000)));
    await settle();
    const message = h.registry.inspectionFor("debug")?.stackMessage ?? "";
    assert.match(message, /…$/u);
    assert.ok(message.length < 550);
  });

  it("keeps malformed DAP stop ids synthetic instead of claiming a real goroutine", (t) => {
    const h = harness(t);
    h.registry.setSessionState("running");
    for (const threadId of [NaN, Infinity, 1.5, Number.MAX_SAFE_INTEGER + 1, -1, 0]) {
      h.controller.stopped("debug", threadId);
    }
    h.registry.setSessionState("suspended");
    const latest = h.client.requests.at(-1);
    assert.deepEqual(latest?.args, { threadId: 0, startFrame: 0, levels: inspectionLimits.frames });
    assert.equal(h.registry.inspectionFor("debug")?.targetGoroutine, 0);
  });
});

describe("bounded inspection decoding", () => {
  const codecCases = [
    {
      name: "stackFrames",
      limit: inspectionLimits.frames,
      item: (i: number) => ({ id: i + 1, name: "frame", line: 0, column: 0 }),
      decode: decodeStackFrames,
    },
    {
      name: "scopes",
      limit: inspectionLimits.scopes,
      item: (i: number) => ({ variablesReference: i + 1 }),
      decode: decodeScopeReferences,
    },
    {
      name: "variables",
      limit: inspectionLimits.variablesPerResponse,
      item: () => variable(),
      decode: decodeVariables,
    },
  ];
  for (const { name, limit, item, decode } of codecCases) {
    it(`accepts exactly ${String(limit)} ${name} and rejects one more rather than silently slicing`, () => {
      assert.equal(decode({ [name]: Array.from({ length: limit }, (_, i) => item(i)) }).length, limit);
      assert.throws(() => decode({ [name]: Array.from({ length: limit + 1 }, (_, i) => item(i)) }), /entry limit/);
    });
    it(`rejects over-limit ${name} arrays before walking any element`, () => {
      for (const length of [limit + 1, inspectionLimits.responseNodes + 1]) {
        const values = new Array<unknown>(length);
        let reads = 0;
        Object.defineProperty(values, 0, { get() { reads += 1; return item(0); } });
        assert.throws(() => decode({ [name]: values }), /entry limit/);
        assert.equal(reads, 0);
      }
    });
  }

  for (const field of ["name", "value", "type"] as const) {
    it(`enforces the UTF-8 boundary for variable ${field}`, () => {
      const text = "😀".repeat(inspectionLimits.textBytes / 4);
      const value = { ...variable(), [field]: text };
      assert.equal(decodeVariables({ variables: [value] })[0]?.[field], text);
      assert.throws(() => decodeVariables({ variables: [{ ...value, [field]: `${text}a` }] }), /UTF-8 byte limit/);
    });
  }

  it("checks frame names and source paths by UTF-8 bytes", () => {
    const text = "é".repeat(inspectionLimits.textBytes / 2);
    const frame = { id: 1, name: text, source: { path: text }, line: 0, column: 0 };
    assert.equal(decodeStackFrames({ stackFrames: [frame] })[0]?.file, text);
    assert.throws(() => decodeStackFrames({ stackFrames: [{ ...frame, name: `${text}x` }] }), /stack frame name.*UTF-8/);
    assert.throws(() => decodeStackFrames({ stackFrames: [{ ...frame, source: { path: `${text}x` } }] }), /stack frame source.*UTF-8/);
  });

  for (const invalid of [null, "1", -1, 1.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
    it(`rejects a malformed reference (${String(invalid)}) in both scope and variable responses`, () => {
      assert.throws(() => decodeScopeReferences({ scopes: [{ variablesReference: invalid }] }));
      assert.throws(() => decodeVariables({ variables: [{ ...variable(), variablesReference: invalid }] }));
    });
  }

  it("rejects duplicate frame ids and invalid or contradictory frame counts", () => {
    assert.throws(() => decodeStackFrames({ stackFrames: [stack.stackFrames[0], stack.stackFrames[0]] }), /unique/);
    for (const totalFrames of [-1, 1, 2.5, "10", Number.MAX_SAFE_INTEGER + 1]) {
      assert.throws(() => decodeStackFrames({ ...stack, totalFrames }), /totalFrames/);
    }
    assert.equal(decodeStackFrames({ ...stack, totalFrames: 2 }).length, 2);
  });

  it("reports known stack truncation without claiming a complete stack", async (t) => {
    const h = harness(t);
    h.client.next("stackTrace").result.resolve({ ...stack, totalFrames: 500 });
    await settle();
    assert.match(h.registry.inspectionFor("debug")?.stackMessage ?? "", /showing 2 of 500/);
  });

  it("honestly labels a full page when the adapter does not provide a total", async (t) => {
    const h = harness(t);
    h.client.next("stackTrace").result.resolve({
      stackFrames: Array.from({ length: inspectionLimits.frames }, (_, i) => ({
        id: i + 1, name: "frame", line: 0, column: 0,
      })),
    });
    await settle();
    assert.match(h.registry.inspectionFor("debug")?.stackMessage ?? "", /more may be available/);
  });

  it("bounds all decoded response text including unused fields at the exact byte boundary", () => {
    const overhead = Buffer.byteLength("variablespadding", "utf8");
    const text = "x".repeat(inspectionLimits.responseBytes - overhead);
    assert.deepEqual(decodeVariables({ variables: [], padding: text }), []);
    assert.throws(() => decodeVariables({ variables: [], padding: `${text}x` }), /response.*UTF-8/);
    assert.throws(() => decodeVariables({ variables: [], padding: "😀".repeat(inspectionLimits.responseBytes / 4) }), /response.*UTF-8/);
  });

  it("bounds a response's entire node count at and immediately above the limit", () => {
    const exact = { variables: [], padding: new Array<null>(inspectionLimits.responseNodes - 3).fill(null) };
    assert.deepEqual(decodeVariables(exact), []);
    assert.throws(() => decodeVariables({
      ...exact, padding: [...exact.padding, null],
    }), /node limit/);
  });

  it("bounds response object width without evaluating an over-limit accessor", () => {
    const response: Record<string, unknown> = { variables: [] };
    for (let i = 1; i < inspectionLimits.objectFields; i += 1) {
      response[String(i)] = null;
    }
    assert.deepEqual(decodeVariables(response), []);
    let reads = 0;
    Object.defineProperty(response, "tooMany", {
      enumerable: true,
      get() { reads += 1; return null; },
    });
    assert.throws(() => decodeVariables(response), /field limit/);
    assert.equal(reads, 0);
  });

  it("bounds nesting before recursion can overflow", () => {
    let nested: unknown = null;
    for (let i = 0; i < inspectionLimits.responseDepth - 1; i += 1) {
      nested = { nested };
    }
    assert.deepEqual(decodeVariables({ variables: [], nested }), []);
    assert.throws(() => decodeVariables({ variables: [], nested: { nested: { nested } } }), /depth limit/);
  });

  it("rejects non-JSON objects, holes, cycles, accessors, functions, and wrong field types", () => {
    const cycle: Record<string, unknown> = {};
    cycle.self = cycle;
    const values: unknown[] = [
      { variables: [cycle] },
      { variables: [new Date()] },
      { variables: new Array(1) },
      { variables: [variable()], unknown: () => {} },
      { variables: [{ ...variable(), name: null }] },
      { variables: [{ ...variable(), value: 42 }] },
      { variables: [{ ...variable(), type: {} }] },
      { variables: [{ ...variable(), variablesReference: undefined }] },
      { variables: [{ ...variable(), variablesReference: Symbol("reference") }] },
      Object.create({ variables: [] }),
    ];
    for (const value of values) {
      assert.throws(() => decodeVariables(value));
    }
    let reads = 0;
    for (const enumerable of [false, true]) {
      const response = Object.defineProperty({}, "variables", {
        enumerable,
        get() { reads += 1; return []; },
      });
      assert.throws(() => decodeVariables(response));
    }
    assert.equal(reads, 0);
  });

  it("accepts a shared ordinary JSON-shaped object without confusing it with a cycle", () => {
    const item = variable();
    assert.equal(decodeVariables({ variables: [item, item] }).length, 2);
  });

  it("surfaces oversized variable bodies as errors instead of reporting a silently clipped success", async (t) => {
    const h = harness(t);
    await reachVariables(h);
    h.client.next("variables").result.resolve({
      variables: Array.from({ length: inspectionLimits.variablesPerResponse + 1 }, () => variable()),
    });
    await settle();
    assert.equal(h.registry.inspectionFor("debug")?.localsStatus, "error");
    assert.match(h.registry.inspectionFor("debug")?.localsMessage ?? "", /501|500 entry limit/);
  });
});
