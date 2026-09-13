import {
  type DebugInspection,
  type DebugStackFrame,
  type DebugVariable,
  emptyInspection,
  type SessionModel,
} from "./model.js";
import type { Snapshot } from "./telemetry.js";

export const inspectionLimits = {
  frames: 200,
  scopes: 32,
  variablesPerResponse: 500,
  textBytes: 16_384,
  responseBytes: 512 * 1024,
  responseNodes: 12_000,
  responseDepth: 16,
  objectFields: 64,
  variableNodes: 2_000,
  variableBytes: 256 * 1024,
  references: 256,
  requests: 128,
  inFlight: 4,
  timeoutMs: 5_000,
} as const;

export type InspectionScheduler = (
  callback: () => void,
  milliseconds: number,
) => () => void;

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
  readonly sessionId: string;
  readonly snapshot: Snapshot;
  readonly selectedGoroutine: number;
  readonly stackThreadId: number;
  readonly stopVersion: number;
}

interface FrameGeneration {
  readonly target: InspectionTarget;
  readonly targetVersion: number;
  readonly frameId: number;
  readonly references: Set<number>;
  readonly visibleReferences: Set<number>;
  readonly requested: Set<number>;
  readonly cache: Map<number, readonly DebugVariable[]>;
  nodes: number;
  bytes: number;
  requests: number;
  truncated: string;
  errorReference: number;
}

class StaleInspectionError extends Error {}

export class DebugInspectionController {
  readonly #unsubscribe: () => void;
  readonly #stops = new Map<
    string,
    {
      readonly threadId: number;
      readonly version: number;
      readonly running: boolean;
    }
  >();
  #lastTarget: InspectionTarget | undefined;
  #targetVersion = 0;
  #nextStopVersion = 0;
  #frame: FrameGeneration | undefined;
  #disposed = false;
  #inFlight = 0;
  readonly #cancelWaiters = new Set<() => void>();

  public constructor(
    private readonly registry: InspectionRegistry,
    private readonly getSession: (
      debugSessionId: string,
    ) => DebugSessionClient | undefined,
    private readonly schedule: InspectionScheduler = scheduleTimeout,
  ) {
    this.#unsubscribe = registry.onChange(() => {
      this.#sync();
    });
    this.#sync();
  }

  public selectFrame(frameId: number): void {
    if (this.#disposed || !validReference(frameId)) {
      return;
    }
    const target = this.#currentTarget();
    const inspection =
      target === undefined
        ? undefined
        : this.registry.inspectionFor(target.debugSessionId);
    if (
      target === undefined ||
      inspection === undefined ||
      inspection.stackStatus !== "ready" ||
      !inspection.frames.some((frame) => frame.id === frameId)
    ) {
      return;
    }
    this.#cancelPending();
    const frame = this.#newFrame(target, this.#targetVersion, frameId);
    this.registry.updateInspection(target.debugSessionId, {
      ...inspection,
      selectedFrameId: frameId,
      localsStatus: "loading",
      localsMessage: "",
      variables: [],
      variablesByReference: {},
      loadingReferences: [],
    });
    void this.#loadLocals(frame);
  }

  public stopped(debugSessionId: string, threadId: number): void {
    if (this.#disposed) {
      return;
    }
    this.#stops.set(debugSessionId, {
      threadId: validReference(threadId) ? threadId : 0,
      version: ++this.#nextStopVersion,
      running: false,
    });
    if (this.registry.activeModel()?.debugSessionId === debugSessionId) {
      this.#lastTarget = undefined;
      this.#sync();
    }
  }

  public resumed(debugSessionId: string): void {
    if (this.#disposed) {
      return;
    }
    // WebSocket state can lag DAP run control. Only a later DAP stop may
    // reopen inspection after that transport has told us the stop is over.
    this.#stops.set(debugSessionId, {
      threadId: 0,
      version: ++this.#nextStopVersion,
      running: true,
    });
    if (this.registry.activeModel()?.debugSessionId === debugSessionId) {
      this.#sync();
    }
  }

  public forgetSession(debugSessionId: string): void {
    if (this.#disposed) {
      return;
    }
    this.#stops.delete(debugSessionId);
    if (this.#lastTarget?.debugSessionId === debugSessionId) {
      this.#lastTarget = undefined;
      this.#targetVersion += 1;
      this.#frame = undefined;
      this.#cancelPending();
    }
  }

  public expandVariable(reference: number): void {
    if (this.#disposed || !validReference(reference)) {
      return;
    }
    const target = this.#currentTarget();
    const inspection =
      target === undefined
        ? undefined
        : this.registry.inspectionFor(target.debugSessionId);
    const key = String(reference);
    const frame = this.#frame;
    if (
      target === undefined ||
      inspection === undefined ||
      frame === undefined ||
      !this.#isCurrentFrame(frame) ||
      inspection.localsStatus !== "ready" ||
      !frame.visibleReferences.has(reference) ||
      Object.hasOwn(inspection.variablesByReference, key) ||
      inspection.loadingReferences.includes(reference)
    ) {
      return;
    }
    const cached = frame.cache.get(reference);
    if (cached !== undefined) {
      // Scope roots are already displayed. Exposing them again through an
      // alias duplicates their serialized nodes even though no DAP read occurs.
      const children = this.#acceptVariables(frame, cached);
      this.registry.updateInspection(target.debugSessionId, {
        ...inspection,
        localsMessage: frame.truncated || inspection.localsMessage,
        variablesByReference: {
          ...inspection.variablesByReference,
          [key]: children,
        },
      });
      return;
    }
    if (frame.requested.has(reference)) {
      return;
    }
    if (frame.truncated !== "") {
      this.#localsMessage(frame, frame.truncated);
      return;
    }
    this.registry.updateInspection(target.debugSessionId, {
      ...inspection,
      loadingReferences: [...inspection.loadingReferences, reference],
    });
    void this.#loadVariableChildren(frame, reference);
  }

  public refresh(): void {
    if (this.#disposed) {
      return;
    }
    this.#lastTarget = undefined;
    this.#sync();
  }

  public dispose(): void {
    if (this.#disposed) {
      return;
    }
    this.#disposed = true;
    this.#targetVersion += 1;
    this.#frame = undefined;
    this.#lastTarget = undefined;
    this.#stops.clear();
    this.#cancelPending();
    this.#unsubscribe();
  }

  #sync(): void {
    if (this.#disposed) {
      return;
    }
    const model = this.registry.activeModel();
    const target = this.#currentTarget();
    if (target === undefined) {
      this.#lastTarget = undefined;
      this.#targetVersion += 1;
      this.#frame = undefined;
      this.#cancelPending();
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
    if (
      this.#lastTarget?.debugSessionId === target.debugSessionId &&
      this.#lastTarget.sessionId === target.sessionId &&
      this.#lastTarget.snapshot === target.snapshot &&
      this.#lastTarget.selectedGoroutine === target.selectedGoroutine &&
      this.#lastTarget.stackThreadId === target.stackThreadId &&
      this.#lastTarget.stopVersion === target.stopVersion
    ) {
      return;
    }
    this.#lastTarget = target;
    const targetVersion = ++this.#targetVersion;
    this.#frame = undefined;
    this.#cancelPending();

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

    if (!this.#sessionAvailable(target)) {
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
    void this.#loadStack(target, targetVersion);
  }

  async #loadStack(
    target: InspectionTarget,
    targetVersion: number,
  ): Promise<void> {
    try {
      const response = await this.#request(
        target,
        () => this.#isCurrent(target, targetVersion),
        "stackTrace",
        {
          threadId: target.stackThreadId,
          startFrame: 0,
          levels: inspectionLimits.frames,
        },
      );
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
      const frame = this.#newFrame(target, targetVersion, selectedFrameId);
      this.registry.updateInspection(target.debugSessionId, {
        ...emptyInspection(inspectionGoroutine(target)),
        stackStatus: "ready",
        stackMessage: stackTruncationMessage(response, frames.length),
        frames,
        selectedFrameId,
        localsStatus: "loading",
      });
      await this.#loadLocals(frame);
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

  async #loadLocals(frame: FrameGeneration): Promise<void> {
    const { target, frameId } = frame;
    try {
      const scopeResponse = await this.#request(
        target,
        () => this.#isCurrentFrame(frame),
        "scopes",
        { frameId },
        frame,
      );
      if (!this.#isCurrentFrame(frame)) {
        return;
      }
      const references = decodeScopeReferences(scopeResponse);
      for (const reference of references) {
        frame.references.add(reference);
      }
      const variables = await this.#scopeVariables(frame, references);
      if (variables === undefined || !this.#isCurrentFrame(frame)) {
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
        localsMessage: frame.truncated,
        variables,
        variablesByReference: {},
        loadingReferences: [],
      });
    } catch (error: unknown) {
      if (!this.#isCurrentFrame(frame)) {
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

  async #scopeVariables(
    frame: FrameGeneration,
    references: readonly number[],
  ): Promise<DebugVariable[] | undefined> {
    const variables: DebugVariable[] = [];
    for (const reference of references) {
      if (!this.#isCurrentFrame(frame)) {
        return undefined;
      }
      if (frame.truncated !== "") {
        break;
      }
      const group = await this.#variables(frame, reference);
      if (!this.#isCurrentFrame(frame)) {
        return undefined;
      }
      variables.push(...group);
    }
    return variables;
  }

  async #loadVariableChildren(
    frame: FrameGeneration,
    reference: number,
  ): Promise<void> {
    const { target } = frame;
    try {
      const children = await this.#variables(frame, reference);
      if (!this.#isCurrentFrame(frame)) {
        return;
      }
      const inspection = this.registry.inspectionFor(target.debugSessionId);
      if (inspection === undefined) {
        return;
      }
      this.registry.updateInspection(target.debugSessionId, {
        ...inspection,
        localsMessage: frame.truncated ||
          (frame.errorReference === reference ? "" : inspection.localsMessage),
        variablesByReference: {
          ...inspection.variablesByReference,
          [String(reference)]: children,
        },
        loadingReferences: inspection.loadingReferences.filter(
          (item) => item !== reference,
        ),
      });
    } catch (error: unknown) {
      if (!this.#isCurrentFrame(frame)) {
        return;
      }
      const inspection = this.registry.inspectionFor(target.debugSessionId);
      if (inspection === undefined) {
        return;
      }
      frame.errorReference = reference;
      this.registry.updateInspection(target.debugSessionId, {
        ...inspection,
        localsMessage: frame.truncated || `Cannot expand variable: ${errorMessage(error)}`,
        loadingReferences: inspection.loadingReferences.filter(
          (item) => item !== reference,
        ),
      });
    }
  }

  #currentTarget(): InspectionTarget | undefined {
    if (this.#disposed) {
      return undefined;
    }
    const model = this.registry.activeModel();
    if (
      model === undefined ||
      model.snapshot === undefined ||
      model.sessionState !== "suspended" ||
      this.#stops.get(model.debugSessionId)?.running === true
    ) {
      return undefined;
    }
    return {
      debugSessionId: model.debugSessionId,
      sessionId: model.sessionId,
      snapshot: model.snapshot,
      selectedGoroutine: model.selectedGoroutine,
      stackThreadId:
        this.#stops.get(model.debugSessionId)?.threadId ??
        (validReference(model.snapshot.current) ? model.snapshot.current : 0),
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
      current.sessionId === target.sessionId &&
      current.snapshot === target.snapshot &&
      current.selectedGoroutine === target.selectedGoroutine &&
      current.stackThreadId === target.stackThreadId &&
      current.stopVersion === target.stopVersion
    );
  }

  #isCurrentFrame(frame: FrameGeneration): boolean {
    if (
      this.#frame !== frame ||
      !this.#isCurrent(frame.target, frame.targetVersion)
    ) {
      return false;
    }
    const inspection = this.registry.inspectionFor(frame.target.debugSessionId);
    return inspection?.selectedFrameId === frame.frameId;
  }

  #newFrame(
    target: InspectionTarget,
    targetVersion: number,
    frameId: number,
  ): FrameGeneration {
    const frame: FrameGeneration = {
      target,
      targetVersion,
      frameId,
      references: new Set(),
      visibleReferences: new Set(),
      requested: new Set(),
      cache: new Map(),
      nodes: 0,
      bytes: 0,
      requests: 0,
      truncated: "",
      errorReference: 0,
    };
    this.#frame = frame;
    return frame;
  }

  #localsMessage(frame: FrameGeneration, message: string): void {
    if (!this.#isCurrentFrame(frame)) {
      return;
    }
    const inspection = this.registry.inspectionFor(frame.target.debugSessionId);
    if (inspection !== undefined) {
      this.registry.updateInspection(frame.target.debugSessionId, {
        ...inspection,
        localsMessage: message,
      });
    }
  }

  async #variables(
    frame: FrameGeneration,
    reference: number,
  ): Promise<readonly DebugVariable[]> {
    const cached = frame.cache.get(reference);
    if (cached !== undefined) {
      return cached;
    }
    let reason = "";
    if (frame.requests >= inspectionLimits.requests) {
      reason = `request limit (${String(inspectionLimits.requests)})`;
    } else if (frame.nodes >= inspectionLimits.variableNodes) {
      reason = `node limit (${String(inspectionLimits.variableNodes)})`;
    } else if (frame.bytes >= inspectionLimits.variableBytes) {
      reason = `UTF-8 byte limit (${String(inspectionLimits.variableBytes)})`;
    }
    if (reason !== "") {
      frame.truncated = `Variable inspection truncated: ${reason} reached. Select a frame or refresh to start a new inspection.`;
      return [];
    }
    const response = await this.#request(
      frame.target,
      () => this.#isCurrentFrame(frame),
      "variables",
      { variablesReference: reference },
      frame,
      reference,
    );
    if (!this.#isCurrentFrame(frame)) {
      throw new StaleInspectionError();
    }
    const accepted = this.#acceptVariables(frame, decodeVariables(response));
    frame.cache.set(reference, accepted);
    return accepted;
  }

  #acceptVariables(
    frame: FrameGeneration,
    variables: readonly DebugVariable[],
  ): readonly DebugVariable[] {
    const accepted: DebugVariable[] = [];
    for (const variable of variables) {
      const bytes = variableBytes(variable);
      const newReference =
        variable.variablesReference > 0 &&
        !frame.references.has(variable.variablesReference);
      let reason = "";
      if (frame.nodes >= inspectionLimits.variableNodes) {
        reason = `node limit (${String(inspectionLimits.variableNodes)})`;
      } else if (frame.bytes + bytes > inspectionLimits.variableBytes) {
        reason = `UTF-8 byte limit (${String(inspectionLimits.variableBytes)})`;
      } else if (
        newReference &&
        frame.references.size >= inspectionLimits.references
      ) {
        reason = `reference limit (${String(inspectionLimits.references)})`;
      }
      if (reason !== "") {
        frame.truncated = `Variable inspection truncated: ${reason} reached. Select a frame or refresh to start a new inspection.`;
        break;
      }
      frame.nodes += 1;
      frame.bytes += bytes;
      if (variable.variablesReference > 0) {
        frame.references.add(variable.variablesReference);
        frame.visibleReferences.add(variable.variablesReference);
      }
      accepted.push(variable);
    }
    return accepted;
  }

  #sessionAvailable(target: InspectionTarget): boolean {
    return this.getSession(target.debugSessionId)?.id === target.debugSessionId;
  }

  #cancelPending(): void {
    for (const cancel of this.#cancelWaiters) {
      cancel();
    }
  }

  #request(
    target: InspectionTarget,
    current: () => boolean,
    command: string,
    args: unknown,
    frame?: FrameGeneration,
    reference?: number,
  ): Promise<unknown> {
    if (!current()) {
      return Promise.reject(new StaleInspectionError());
    }
    const session = this.getSession(target.debugSessionId);
    if (session?.id !== target.debugSessionId) {
      return Promise.reject(new Error(
        "The matching VS Code debug session is no longer available.",
      ));
    }
    if (frame !== undefined && frame.requests >= inspectionLimits.requests) {
      frame.truncated = `Variable inspection truncated: request limit (${String(inspectionLimits.requests)}) reached. Select a frame or refresh to start a new inspection.`;
      return Promise.reject(new Error(frame.truncated));
    }
    if (this.#inFlight >= inspectionLimits.inFlight) {
      return Promise.reject(new Error(`The inspection request limit (${String(inspectionLimits.inFlight)} in flight) is reached. Retry after pending requests complete.`));
    }
    this.#inFlight += 1;
    if (frame !== undefined) {
      frame.requests += 1;
      if (reference !== undefined) {
        frame.requested.add(reference);
      }
    }
    return new Promise((resolve, reject) => {
      let settled = false;
      let cancelTimer = () => {};
      const finish = (error: Error | undefined, value?: unknown) => {
        if (settled) {
          return;
        }
        settled = true;
        cancelTimer();
        this.#cancelWaiters.delete(cancel);
        if (error !== undefined) {
          reject(error);
        } else {
          resolve(value);
        }
      };
      const cancel = () => {
        finish(new StaleInspectionError());
      };
      this.#cancelWaiters.add(cancel);
      cancelTimer = this.schedule(() => {
        finish(new Error(
          `${command} request timed out after ${String(inspectionLimits.timeoutMs)} ms.`,
        ));
      }, inspectionLimits.timeoutMs);
      // DAP customRequest cannot be cancelled. A stale/expired UI waiter must
      // not free its wire slot: otherwise refresh can accumulate hung requests.
      const complete = (error: unknown, value?: unknown) => {
        this.#inFlight -= 1;
        finish(
          error === undefined ? undefined : new Error(errorMessage(error)),
          value,
        );
      };
      void completeRequest(session, command, args, complete);
    });
  }
}

async function completeRequest(
  session: DebugSessionClient,
  command: string,
  args: unknown,
  complete: (error: unknown, value?: unknown) => void,
): Promise<void> {
  let value: unknown;
  try {
    value = await session.customRequest(command, args);
  } catch (error: unknown) {
    complete(error ?? new Error("The debug adapter request failed."));
    return;
  }
  complete(undefined, value);
}

function inspectionGoroutine(target: InspectionTarget): number {
  return target.stackThreadId > 0 ? target.selectedGoroutine : 0;
}

function validReference(value: number): boolean {
  return Number.isSafeInteger(value) && value > 0;
}

function variableBytes(variable: DebugVariable): number {
  return (
    Buffer.byteLength(variable.name, "utf8") +
    Buffer.byteLength(variable.value, "utf8") +
    Buffer.byteLength(variable.type, "utf8")
  );
}

function scheduleTimeout(callback: () => void, milliseconds: number): () => void {
  const timer = setTimeout(callback, milliseconds);
  timer.unref();
  return () => {
    clearTimeout(timer);
  };
}

export function decodeStackFrames(value: unknown): readonly DebugStackFrame[] {
  validateResponse(value, "stackFrames", inspectionLimits.frames);
  const response = record(value, "stack trace response");
  const frames = array(
    response.stackFrames,
    "stackFrames",
    inspectionLimits.frames,
  );
  if (response.totalFrames !== undefined) {
    integer(response.totalFrames, "totalFrames", frames.length);
  }
  const seen = new Set<number>();
  return frames.map((item, index) => {
    const frame = record(item, `stack frame ${String(index)}`);
    const id = integer(frame.id, "stack frame id", 1);
    if (seen.has(id)) {
      throw new TypeError("stack frame ids must be unique");
    }
    seen.add(id);
    const source =
      frame.source === undefined
        ? {}
        : record(frame.source, `stack frame ${String(index)} source`);
    return {
      id,
      name: text(frame.name, "stack frame name", "?"),
      file: text(source.path ?? source.name, "stack frame source", ""),
      line: integer(frame.line, "stack frame line", 0),
      column: integer(frame.column, "stack frame column", 0),
    };
  });
}

export function decodeScopeReferences(value: unknown): readonly number[] {
  validateResponse(value, "scopes", inspectionLimits.scopes);
  const response = record(value, "scopes response");
  const scopes = array(response.scopes, "scopes", inspectionLimits.scopes);
  return [
    ...new Set(
      scopes.map((item, index) =>
        integer(
          record(item, `scope ${String(index)}`).variablesReference,
          "scope variablesReference",
          0,
        ),
      ).filter((reference) => reference > 0),
    ),
  ];
}

export function decodeVariables(value: unknown): readonly DebugVariable[] {
  validateResponse(value, "variables", inspectionLimits.variablesPerResponse);
  const response = record(value, "variables response");
  return array(
    response.variables,
    "variables",
    inspectionLimits.variablesPerResponse,
  )
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
  return { ...value };
}

function array(value: unknown, label: string, maximum: number): readonly unknown[] {
  if (!Array.isArray(value)) {
    throw new TypeError(`${label} must be an array`);
  }
  if (value.length > maximum) {
    throw new RangeError(`${label} exceeds the ${String(maximum)} entry limit`);
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
  if (
    value.length > inspectionLimits.textBytes ||
    Buffer.byteLength(value, "utf8") > inspectionLimits.textBytes
  ) {
    throw new RangeError(
      `${label} exceeds the ${String(inspectionLimits.textBytes)} UTF-8 byte limit`,
    );
  }
  return value;
}

function errorMessage(error: unknown): string {
  const descriptor = error instanceof Error
    ? Object.getOwnPropertyDescriptor(error, "message")
    : undefined;
  let message = "The debug adapter request failed.";
  if (typeof descriptor?.value === "string") {
    message = descriptor.value;
  } else if (typeof error === "string") {
    message = error;
  }
  return message.length > 512 ? `${message.slice(0, 511)}…` : message;
}

function stackTruncationMessage(value: unknown, count: number): string {
  const total = record(value, "stack trace response").totalFrames;
  if (typeof total === "number" && total > count) {
    return `Call stack truncated: showing ${String(count)} of ${String(total)} frames.`;
  }
  return total === undefined && count === inspectionLimits.frames
    ? `Showing the first ${String(count)} stack frames; more may be available.`
    : "";
}

function validateResponse(
  value: unknown,
  collection: string,
  maximum: number,
): void {
  if (value !== null && typeof value === "object" && !Array.isArray(value)) {
    const descriptor = Object.getOwnPropertyDescriptor(value, collection);
    if (descriptor !== undefined && Object.hasOwn(descriptor, "value")) {
      array(descriptor.value, collection, maximum);
    }
  }
  let nodes = 0;
  let bytes = 0;
  const active = new Set<object>();
  const chargeText = (value: string) => {
    if (value.length > inspectionLimits.responseBytes - bytes) {
      throw new RangeError("inspection response exceeds the UTF-8 byte limit");
    }
    bytes += Buffer.byteLength(value, "utf8");
    if (bytes > inspectionLimits.responseBytes) {
      throw new RangeError("inspection response exceeds the UTF-8 byte limit");
    }
  };
  const visitArray = (value: readonly unknown[], depth: number): void => {
    if (value.length > inspectionLimits.responseNodes - nodes) {
      throw new RangeError("inspection response exceeds the node limit");
    }
    for (let index = 0; index < value.length; index += 1) {
      const descriptor = Object.getOwnPropertyDescriptor(value, index);
      if (descriptor === undefined || !Object.hasOwn(descriptor, "value")) {
        throw new TypeError("inspection response arrays must contain JSON values");
      }
      visit(descriptor.value, depth + 1);
    }
  };
  const visitObject = (value: object, depth: number): void => {
    const prototype: unknown = Object.getPrototypeOf(value);
    if (prototype !== Object.prototype && prototype !== null) {
      throw new TypeError("inspection response must contain plain JSON objects");
    }
    let fields = 0;
    for (const key in value) {
      if (!Object.hasOwn(value, key)) {
        continue;
      }
      fields += 1;
      if (fields > inspectionLimits.objectFields) {
        throw new RangeError("inspection response exceeds the object field limit");
      }
      chargeText(key);
      const descriptor = Object.getOwnPropertyDescriptor(value, key);
      if (descriptor === undefined || !Object.hasOwn(descriptor, "value")) {
        throw new TypeError("inspection response must not contain accessors");
      }
      visit(descriptor.value, depth + 1);
    }
  };
  const visit = (value: unknown, depth: number): void => {
    nodes += 1;
    if (
      nodes > inspectionLimits.responseNodes ||
      depth > inspectionLimits.responseDepth
    ) {
      throw new RangeError("inspection response exceeds the node or depth limit");
    }
    if (typeof value === "string") {
      chargeText(value);
      return;
    }
    if (
      value === null ||
      typeof value === "boolean" ||
      (typeof value === "number" && Number.isFinite(value))
    ) {
      return;
    }
    if (typeof value !== "object" || active.has(value)) {
      throw new TypeError("inspection response must contain acyclic JSON values");
    }
    active.add(value);
    if (Array.isArray(value)) {
      visitArray(value, depth);
    } else {
      visitObject(value, depth);
    }
    active.delete(value);
  };
  visit(value, 0);
}
