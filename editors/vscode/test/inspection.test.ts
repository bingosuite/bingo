import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  DebugInspectionController,
  decodeScopeReferences,
  decodeStackFrames,
  decodeVariables,
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

  private emit(): void {
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
