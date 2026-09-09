import {
  type DebugInspection,
  type DebugStackFrame,
  type DebugVariable,
  emptyInspection,
  type SessionModel,
} from "./model.js";
import type { Snapshot } from "./telemetry.js";

const maximumFrames = 200;
const maximumVariables = 500;
const maximumDebugTextLength = 16_384;

export interface DebugSessionClient {
  readonly id: string;
  customRequest(command: string, args?: unknown): PromiseLike<unknown>;
}

export interface InspectionRegistry {
  onChange(listener: () => void): () => void;
  activeModel(): SessionModel | undefined;
  inspectionFor(debugSessionId: string): DebugInspection | undefined;
  updateInspection(
    debugSessionId: string,
    inspection: DebugInspection,
  ): boolean;
}

interface InspectionTarget {
  readonly debugSessionId: string;
  readonly snapshot: Snapshot;
  readonly selectedGoroutine: number;
  readonly stackThreadId: number;
  readonly stopVersion: number;
}

export class DebugInspectionController {
  readonly #unsubscribe: () => void;
  readonly #stops = new Map<
    string,
    { readonly threadId: number; readonly version: number }
  >();
  #lastTarget: InspectionTarget | undefined;
  #targetVersion = 0;
  #localsVersion = 0;
  #nextStopVersion = 0;

  public constructor(
    private readonly registry: InspectionRegistry,
    private readonly getSession: (
      debugSessionId: string,
    ) => DebugSessionClient | undefined,
  ) {
    this.#unsubscribe = registry.onChange(() => {
      this.#sync();
    });
    this.#sync();
  }

  public selectFrame(frameId: number): void {
    const target = this.#currentTarget();
    const inspection =
      target === undefined
        ? undefined
        : this.registry.inspectionFor(target.debugSessionId);
    if (
      target === undefined ||
      inspection === undefined ||
      !inspection.frames.some((frame) => frame.id === frameId)
    ) {
      return;
    }
    const localsVersion = ++this.#localsVersion;
    const targetVersion = this.#targetVersion;
    this.registry.updateInspection(target.debugSessionId, {
      ...inspection,
      selectedFrameId: frameId,
      localsStatus: "loading",
      localsMessage: "",
      variables: [],
      variablesByReference: {},
      loadingReferences: [],
    });
    void this.#loadLocals(target, targetVersion, localsVersion, frameId);
  }

  public stopped(debugSessionId: string, threadId: number): void {
    this.#stops.set(debugSessionId, {
      threadId: threadId > 0 ? threadId : 0,
      version: ++this.#nextStopVersion,
    });
    if (this.registry.activeModel()?.debugSessionId === debugSessionId) {
      this.#lastTarget = undefined;
      this.#sync();
    }
  }

  public forgetSession(debugSessionId: string): void {
    this.#stops.delete(debugSessionId);
    if (this.#lastTarget?.debugSessionId === debugSessionId) {
      this.#lastTarget = undefined;
      this.#targetVersion += 1;
      this.#localsVersion += 1;
    }
  }

  public expandVariable(reference: number): void {
    if (reference <= 0) {
      return;
    }
    const target = this.#currentTarget();
    const inspection =
      target === undefined
        ? undefined
        : this.registry.inspectionFor(target.debugSessionId);
    const key = String(reference);
    if (
      target === undefined ||
      inspection === undefined ||
      !hasVariableReference(inspection, reference) ||
      Object.hasOwn(inspection.variablesByReference, key) ||
      inspection.loadingReferences.includes(reference)
    ) {
      return;
    }
    const session = this.getSession(target.debugSessionId);
    if (session === undefined) {
      return;
    }
    const targetVersion = this.#targetVersion;
    const localsVersion = this.#localsVersion;
    const frameId = inspection.selectedFrameId;
    this.registry.updateInspection(target.debugSessionId, {
      ...inspection,
      loadingReferences: [...inspection.loadingReferences, reference],
    });
    void this.#loadVariableChildren(
      target,
      targetVersion,
      localsVersion,
      frameId,
      session,
      reference,
    );
  }

  public refresh(): void {
    this.#lastTarget = undefined;
    this.#sync();
  }

  public dispose(): void {
    this.#targetVersion += 1;
    this.#localsVersion += 1;
    this.#unsubscribe();
  }

  #sync(): void {
    const model = this.registry.activeModel();
    if (
      model === undefined ||
      model.snapshot === undefined ||
      model.sessionState !== "suspended"
    ) {
      this.#lastTarget = undefined;
      this.#targetVersion += 1;
      this.#localsVersion += 1;
      if (model !== undefined) {
        const inspection = this.registry.inspectionFor(model.debugSessionId);
        if (
          inspection !== undefined &&
          (inspection.stackStatus !== "idle" ||
            inspection.targetGoroutine !== model.selectedGoroutine)
        ) {
          this.registry.updateInspection(
            model.debugSessionId,
            emptyInspection(model.selectedGoroutine),
          );
        }
      }
      return;
    }
    const target: InspectionTarget = {
      debugSessionId: model.debugSessionId,
      snapshot: model.snapshot,
      selectedGoroutine: model.selectedGoroutine,
      stackThreadId:
        this.#stops.get(model.debugSessionId)?.threadId ??
        (model.snapshot.current > 0 ? model.snapshot.current : 0),
      stopVersion: this.#stops.get(model.debugSessionId)?.version ?? 0,
    };
    if (
      this.#lastTarget?.debugSessionId === target.debugSessionId &&
      this.#lastTarget.snapshot === target.snapshot &&
      this.#lastTarget.selectedGoroutine === target.selectedGoroutine &&
      this.#lastTarget.stackThreadId === target.stackThreadId &&
      this.#lastTarget.stopVersion === target.stopVersion
    ) {
      return;
    }
    this.#lastTarget = target;
    const targetVersion = ++this.#targetVersion;
    this.#localsVersion += 1;

    if (
      target.stackThreadId > 0 &&
      target.selectedGoroutine !== target.stackThreadId
    ) {
      this.registry.updateInspection(target.debugSessionId, {
        ...emptyInspection(inspectionGoroutine(target)),
        stackStatus: "unavailable",
        stackMessage: `Call stack and locals are currently available only for the stopped goroutine g${String(target.stackThreadId)}.`,
        localsStatus: "unavailable",
      });
      return;
    }

    const session = this.getSession(target.debugSessionId);
    if (session === undefined) {
      this.registry.updateInspection(target.debugSessionId, {
        ...emptyInspection(inspectionGoroutine(target)),
        stackStatus: "error",
        stackMessage: "The matching VS Code debug session is no longer available.",
        localsStatus: "error",
      });
      return;
    }
    this.registry.updateInspection(target.debugSessionId, {
      ...emptyInspection(inspectionGoroutine(target)),
      stackStatus: "loading",
      localsStatus: "loading",
    });
    void this.#loadStack(target, targetVersion, session);
  }

  async #loadStack(
    target: InspectionTarget,
    targetVersion: number,
    session: DebugSessionClient,
  ): Promise<void> {
    try {
      const response = await session.customRequest("stackTrace", {
        threadId: target.stackThreadId,
        startFrame: 0,
        levels: maximumFrames,
      });
      if (!this.#isCurrent(target, targetVersion)) {
        return;
      }
      const frames = decodeStackFrames(response);
      if (frames.length === 0) {
        this.registry.updateInspection(target.debugSessionId, {
          ...emptyInspection(inspectionGoroutine(target)),
          stackStatus: "unavailable",
          stackMessage:
            "No stack frames were returned for this stop. Select the stopped goroutine or pause in user code.",
          localsStatus: "unavailable",
        });
        return;
      }
      const selectedFrameId = frames[0]?.id ?? 0;
      const localsVersion = ++this.#localsVersion;
      this.registry.updateInspection(target.debugSessionId, {
        ...emptyInspection(inspectionGoroutine(target)),
        stackStatus: "ready",
        frames,
        selectedFrameId,
        localsStatus: "loading",
      });
      await this.#loadLocals(
        target,
        targetVersion,
        localsVersion,
        selectedFrameId,
      );
    } catch (error: unknown) {
      if (!this.#isCurrent(target, targetVersion)) {
        return;
      }
      this.registry.updateInspection(target.debugSessionId, {
        ...emptyInspection(inspectionGoroutine(target)),
        stackStatus: "error",
        stackMessage: `Cannot load the call stack: ${errorMessage(error)}`,
        localsStatus: "error",
      });
    }
  }

  async #loadLocals(
    target: InspectionTarget,
    targetVersion: number,
    localsVersion: number,
    frameId: number,
  ): Promise<void> {
    const session = this.getSession(target.debugSessionId);
    if (session === undefined) {
      if (
        this.#isCurrent(target, targetVersion) &&
        this.#localsVersion === localsVersion
      ) {
        const inspection = this.registry.inspectionFor(target.debugSessionId);
        if (inspection !== undefined) {
          this.registry.updateInspection(target.debugSessionId, {
            ...inspection,
            localsStatus: "error",
            localsMessage:
              "The matching VS Code debug session is no longer available.",
          });
        }
      }
      return;
    }
    try {
      const scopeResponse = await session.customRequest("scopes", { frameId });
      const references = decodeScopeReferences(scopeResponse);
      const variables: DebugVariable[] = [];
      for (const variablesReference of references) {
        if (variables.length >= maximumVariables) {
          break;
        }
        const group = decodeVariables(
          await session.customRequest("variables", {
            variablesReference,
          }),
        );
        variables.push(
          ...group.slice(0, maximumVariables - variables.length),
        );
      }
      if (
        !this.#isCurrent(target, targetVersion) ||
        this.#localsVersion !== localsVersion
      ) {
        return;
      }
      const inspection = this.registry.inspectionFor(target.debugSessionId);
      if (
        inspection === undefined ||
        inspection.selectedFrameId !== frameId
      ) {
        return;
      }
      this.registry.updateInspection(target.debugSessionId, {
        ...inspection,
        localsStatus: "ready",
        localsMessage: "",
        variables,
        variablesByReference: {},
        loadingReferences: [],
      });
    } catch (error: unknown) {
      if (
        !this.#isCurrent(target, targetVersion) ||
        this.#localsVersion !== localsVersion
      ) {
        return;
      }
      const inspection = this.registry.inspectionFor(target.debugSessionId);
      if (inspection === undefined) {
        return;
      }
      this.registry.updateInspection(target.debugSessionId, {
        ...inspection,
        localsStatus: "error",
        localsMessage: `Cannot load locals: ${errorMessage(error)}`,
      });
    }
  }

  async #loadVariableChildren(
    target: InspectionTarget,
    targetVersion: number,
    localsVersion: number,
    frameId: number,
    session: DebugSessionClient,
    reference: number,
  ): Promise<void> {
    try {
      const children = decodeVariables(
        await session.customRequest("variables", {
          variablesReference: reference,
        }),
      );
      if (
        !this.#isCurrentExpansion(
          target,
          targetVersion,
          localsVersion,
          frameId,
          reference,
        )
      ) {
        return;
      }
      const inspection = this.registry.inspectionFor(target.debugSessionId);
      if (inspection === undefined) {
        return;
      }
      this.registry.updateInspection(target.debugSessionId, {
        ...inspection,
        variablesByReference: {
          ...inspection.variablesByReference,
          [String(reference)]: children,
        },
        loadingReferences: inspection.loadingReferences.filter(
          (item) => item !== reference,
        ),
      });
    } catch (error: unknown) {
      if (
        !this.#isCurrentExpansion(
          target,
          targetVersion,
          localsVersion,
          frameId,
          reference,
        )
      ) {
        return;
      }
      const inspection = this.registry.inspectionFor(target.debugSessionId);
      if (inspection === undefined) {
        return;
      }
      this.registry.updateInspection(target.debugSessionId, {
        ...inspection,
        localsMessage: `Cannot expand variable: ${errorMessage(error)}`,
        loadingReferences: inspection.loadingReferences.filter(
          (item) => item !== reference,
        ),
      });
    }
  }

  #currentTarget(): InspectionTarget | undefined {
    const model = this.registry.activeModel();
    if (
      model === undefined ||
      model.snapshot === undefined ||
      model.sessionState !== "suspended"
    ) {
      return undefined;
    }
    return {
      debugSessionId: model.debugSessionId,
      snapshot: model.snapshot,
      selectedGoroutine: model.selectedGoroutine,
      stackThreadId:
        this.#stops.get(model.debugSessionId)?.threadId ??
        (model.snapshot.current > 0 ? model.snapshot.current : 0),
      stopVersion: this.#stops.get(model.debugSessionId)?.version ?? 0,
    };
  }

  #isCurrent(
    target: InspectionTarget,
    targetVersion: number,
  ): boolean {
    const current = this.#currentTarget();
    return (
      this.#targetVersion === targetVersion &&
      current?.debugSessionId === target.debugSessionId &&
      current.snapshot === target.snapshot &&
      current.selectedGoroutine === target.selectedGoroutine &&
      current.stackThreadId === target.stackThreadId &&
      current.stopVersion === target.stopVersion
    );
  }

  #isCurrentExpansion(
    target: InspectionTarget,
    targetVersion: number,
    localsVersion: number,
    frameId: number,
    reference: number,
  ): boolean {
    if (
      !this.#isCurrent(target, targetVersion) ||
      this.#localsVersion !== localsVersion
    ) {
      return false;
    }
    const inspection = this.registry.inspectionFor(target.debugSessionId);
    return (
      inspection?.selectedFrameId === frameId &&
      hasVariableReference(inspection, reference)
    );
  }
}

function inspectionGoroutine(target: InspectionTarget): number {
  return target.stackThreadId > 0 ? target.selectedGoroutine : 0;
}

function hasVariableReference(
  inspection: DebugInspection,
  reference: number,
): boolean {
  const pending: DebugVariable[] = [...inspection.variables];
  const seen = new Set<number>();
  while (pending.length > 0) {
    const variable = pending.pop();
    if (variable === undefined) {
      continue;
    }
    if (variable.variablesReference === reference) {
      return true;
    }
    const childReference = variable.variablesReference;
    if (childReference <= 0 || seen.has(childReference)) {
      continue;
    }
    seen.add(childReference);
    pending.push(
      ...(inspection.variablesByReference[String(childReference)] ?? []),
    );
  }
  return false;
}

export function decodeStackFrames(value: unknown): readonly DebugStackFrame[] {
  const response = record(value, "stack trace response");
  const frames = array(response.stackFrames, "stackFrames");
  return frames.slice(0, maximumFrames).map((item, index) => {
    const frame = record(item, `stack frame ${String(index)}`);
    const source =
      frame.source === undefined
        ? {}
        : record(frame.source, `stack frame ${String(index)} source`);
    return {
      id: integer(frame.id, "stack frame id", 1),
      name: text(frame.name, "stack frame name", "?"),
      file: text(source.path ?? source.name, "stack frame source", ""),
      line: integer(frame.line, "stack frame line", 0),
      column: integer(frame.column, "stack frame column", 0),
    };
  });
}

export function decodeScopeReferences(value: unknown): readonly number[] {
  const response = record(value, "scopes response");
  const scopes = array(response.scopes, "scopes");
  return scopes
    .slice(0, 32)
    .map((item, index) =>
      integer(
        record(item, `scope ${String(index)}`).variablesReference,
        "scope variablesReference",
        0,
      ),
    )
    .filter((reference) => reference > 0);
}

export function decodeVariables(value: unknown): readonly DebugVariable[] {
  const response = record(value, "variables response");
  return array(response.variables, "variables")
    .slice(0, maximumVariables)
    .map((item, index) => {
      const variable = record(item, `variable ${String(index)}`);
      return {
        name: text(variable.name, "variable name", "?"),
        value: text(variable.value, "variable value", ""),
        type: text(variable.type, "variable type", ""),
        variablesReference: integer(
          variable.variablesReference,
          "variable variablesReference",
          0,
        ),
      };
    });
}

function record(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new TypeError(`${label} must be an object`);
  }
  return value as Record<string, unknown>;
}

function array(value: unknown, label: string): readonly unknown[] {
  if (!Array.isArray(value)) {
    throw new TypeError(`${label} must be an array`);
  }
  return value;
}

function integer(value: unknown, label: string, minimum: number): number {
  if (
    typeof value !== "number" ||
    !Number.isSafeInteger(value) ||
    value < minimum
  ) {
    throw new TypeError(
      `${label} must be a safe integer >= ${String(minimum)}`,
    );
  }
  return value;
}

function text(value: unknown, label: string, fallback: string): string {
  if (value === undefined) {
    return fallback;
  }
  if (typeof value !== "string") {
    throw new TypeError(`${label} must be a string`);
  }
  if (value.length <= maximumDebugTextLength) {
    return value;
  }
  return `${value.slice(0, maximumDebugTextLength - 1)}…`;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
