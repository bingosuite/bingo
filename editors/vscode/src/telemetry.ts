import { TextDecoder } from "node:util";

export const wireProtocolVersion = "1.3";
export const snapshotCommandKind = "GoroutineSnapshot";
export const maximumEnvelopeBytes = 8 * 1024 * 1024;
export const maximumGoroutines = 8193;
export const maximumThreads = 2049;
export const maximumStringLength = 4096;

const maximumPayloadNodes = 20_000;
const utf8 = new TextDecoder("utf-8", { fatal: true });

const eventKinds = new Set([
  "BreakpointHit",
  "Panic",
  "Stepped",
  "Paused",
  "Output",
  "ProcessExited",
  "BreakpointSet",
  "BreakpointCleared",
  "Continued",
  "Locals",
  "Frames",
  "Goroutines",
  "Evaluate",
  "GoroutineSnapshot",
  "SessionState",
  "Error",
  "Restarted",
]);

export interface Location {
  readonly file: string;
  readonly line: number;
  readonly function: string;
}

export interface Goroutine {
  readonly id: number;
  readonly parentId: number;
  readonly status: string;
  readonly waitReason: string;
  readonly currentLoc: Location;
  readonly startLoc: Location;
  readonly createdLoc: Location;
  readonly threadId: number;
  readonly current: boolean;
}

export interface Thread {
  readonly id: number;
  readonly mid: number;
  readonly goid: number;
  readonly spinning: boolean;
  readonly currentLoc: Location;
  readonly current: boolean;
}

export interface Snapshot {
  readonly goroutines: readonly Goroutine[];
  readonly threads: readonly Thread[];
  readonly current: number;
  readonly created: readonly number[];
  readonly exited: readonly number[];
}

export interface DecodedEvent {
  readonly kind: string;
  readonly seq: number;
  readonly payload: Record<string, unknown>;
  readonly snapshot?: Snapshot;
}

export interface SequenceResult {
  readonly accept: boolean;
  readonly gap?: string;
}

export type TelemetryData =
  | string
  | Buffer
  | ArrayBuffer
  | readonly Buffer[];

export class SequenceTracker {
  #last = 0;
  readonly #seen = new Set<number>();
  readonly #recent: number[] = [];

  public reset(): void {
    this.#last = 0;
    this.#seen.clear();
    this.#recent.length = 0;
  }

  public observe(seq: number): SequenceResult {
    if (!Number.isSafeInteger(seq) || seq < 1) {
      return { accept: false, gap: `invalid event sequence ${String(seq)}` };
    }
    if (this.#seen.has(seq)) {
      return {
        accept: false,
        gap: `duplicate event ${String(seq)}`,
      };
    }
    this.#seen.add(seq);
    this.#recent.push(seq);
    if (this.#recent.length > 4096) {
      const oldest = this.#recent.shift();
      if (oldest !== undefined) {
        this.#seen.delete(oldest);
      }
    }
    if (seq < this.#last) {
      return {
        accept: true,
        gap: `out-of-order event ${String(seq)} after ${String(this.#last)}`,
      };
    }
    const gap =
      this.#last > 0 && seq > this.#last + 1
        ? `missed events ${String(this.#last + 1)}-${String(seq - 1)}`
        : undefined;
    this.#last = seq;
    return gap === undefined ? { accept: true } : { accept: true, gap };
  }
}

export function snapshotCommand(): string {
  return JSON.stringify({
    v: wireProtocolVersion,
    kind: snapshotCommandKind,
    payload: {},
  });
}

export function decodeEvent(data: TelemetryData): DecodedEvent {
  const text = decodeText(data);
  let decoded: unknown;
  try {
    decoded = JSON.parse(text) as unknown;
  } catch {
    throw new Error("telemetry event is not valid JSON");
  }
  const envelope = record(decoded, "telemetry event");
  exactKeys(envelope, ["v", "kind", "seq", "payload"], [], "telemetry event");
  if (envelope.v !== wireProtocolVersion) {
    throw new Error(
      `telemetry protocol ${JSON.stringify(envelope.v)} is incompatible with ${wireProtocolVersion}`,
    );
  }
  const kind = boundedString(envelope.kind, "event kind");
  if (!eventKinds.has(kind)) {
    throw new Error(`unknown telemetry event kind ${JSON.stringify(kind)}`);
  }
  const seq = integer(envelope.seq, "event seq", 1);
  const payload = record(envelope.payload, "event payload");
  if (kind === snapshotCommandKind) {
    return { kind, seq, payload, snapshot: decodeSnapshot(payload) };
  }
  validatePayload(kind, payload);
  return { kind, seq, payload };
}

function decodeText(data: TelemetryData): string {
  const bytes = dataBytes(data);
  if (bytes > maximumEnvelopeBytes) {
    throw new Error("telemetry event exceeds 8 MiB");
  }
  if (typeof data === "string") {
    return data;
  }
  try {
    if (isBufferList(data)) {
      return utf8.decode(Buffer.concat(data));
    }
    return utf8.decode(data);
  } catch {
    throw new Error("telemetry event is not valid UTF-8");
  }
}

function dataBytes(data: TelemetryData): number {
  if (typeof data === "string") {
    return Buffer.byteLength(data);
  }
  if (isBufferList(data)) {
    let size = 0;
    for (const part of data) {
      size += part.length;
      if (size > maximumEnvelopeBytes) {
        return size;
      }
    }
    return size;
  }
  return data.byteLength;
}

function isBufferList(data: TelemetryData): data is readonly Buffer[] {
  return Array.isArray(data);
}

function decodeSnapshot(payload: Record<string, unknown>): Snapshot {
  exactKeys(
    payload,
    ["goroutines", "threads"],
    ["current", "created", "exited"],
    "snapshot",
  );
  const rawGoroutines = protocolArray(
    payload.goroutines,
    "goroutines",
    maximumGoroutines,
  );
  const rawThreads = protocolArray(payload.threads, "threads", maximumThreads);
  const goroutines = rawGoroutines.map((value, index) =>
    decodeGoroutine(value, `goroutines[${String(index)}]`),
  );
  const ids = new Set<number>();
  for (const goroutine of goroutines) {
    if (ids.has(goroutine.id)) {
      throw new Error(`duplicate goroutine id ${String(goroutine.id)}`);
    }

    ids.add(goroutine.id);
  }
  return {
    goroutines,
    threads: rawThreads.map((value, index) =>
      decodeThread(value, `threads[${String(index)}]`),
    ),
    current: optionalInteger(payload.current, "current"),
    created: decodeIDs(payload.created, "created"),
    exited: decodeIDs(payload.exited, "exited"),
  };
}

function protocolArray(
  value: unknown,
  label: string,
  maximum: number,
): readonly unknown[] {
  // Go's encoding/json represents a nil slice as null; on an early degraded
  // stop that is the protocol's empty collection rather than malformed input.
  return value === null ? [] : boundedArray(value, label, maximum);
}

function decodeGoroutine(value: unknown, label: string): Goroutine {
  const item = record(value, label);
  exactKeys(
    item,
    ["id", "status", "currentLoc"],
    [
      "parentId",
      "waitReason",
      "startLoc",
      "createdLoc",
      "threadId",
      "current",
    ],
    label,
  );
  return {
    id: integer(item.id, `${label}.id`, 0),
    parentId: optionalInteger(item.parentId, `${label}.parentId`),
    status: boundedString(item.status, `${label}.status`),
    waitReason: optionalString(item.waitReason, `${label}.waitReason`),
    currentLoc: decodeLocation(item.currentLoc, `${label}.currentLoc`),
    startLoc: decodeLocation(item.startLoc, `${label}.startLoc`, true),
    createdLoc: decodeLocation(item.createdLoc, `${label}.createdLoc`, true),
    threadId: optionalInteger(item.threadId, `${label}.threadId`),
    current: optionalBoolean(item.current, `${label}.current`),
  };
}

function decodeThread(value: unknown, label: string): Thread {
  const item = record(value, label);
  exactKeys(
    item,
    ["id"],
    ["mid", "goid", "spinning", "currentLoc", "current"],
    label,
  );
  return {
    id: integer(item.id, `${label}.id`, 0),
    mid: optionalInteger(item.mid, `${label}.mid`),
    goid: optionalInteger(item.goid, `${label}.goid`),
    spinning: optionalBoolean(item.spinning, `${label}.spinning`),
    currentLoc: decodeLocation(item.currentLoc, `${label}.currentLoc`, true),
    current: optionalBoolean(item.current, `${label}.current`),
  };
}

function decodeLocation(
  value: unknown,
  label: string,
  optional = false,
): Location {
  if (value === undefined && optional) {
    return { file: "", line: 0, function: "" };
  }
  const item = record(value, label);
  exactKeys(item, ["file", "line"], ["function"], label);
  return {
    file: boundedString(item.file, `${label}.file`),
    line: integer(item.line, `${label}.line`, 0),
    function: optionalString(item.function, `${label}.function`),
  };
}

function validatePayload(
  kind: string,
  payload: Record<string, unknown>,
): void {
  if (kind === "SessionState") {
    exactKeys(payload, ["sessionID", "state", "clients"], [], "SessionState");
    boundedString(payload.sessionID, "sessionID");
    const state = boundedString(payload.state, "session state");
    if (!["idle", "running", "suspended", "exited"].includes(state)) {
      throw new Error(`unknown session state ${JSON.stringify(state)}`);
    }
    integer(payload.clients, "session clients", 0);
    return;
  }
  const budget = { remaining: maximumPayloadNodes };
  validatePayloadValue(payload, kind, 0, budget);
}

function validatePayloadValue(
  value: unknown,
  label: string,
  depth: number,
  budget: { remaining: number },
): void {
  budget.remaining -= 1;
  if (budget.remaining < 0) {
    throw new Error(`${label} payload is too large`);
  }
  if (depth > 12) {
    throw new Error(`${label} payload nesting is too deep`);
  }
  if (
    value === null ||
    typeof value === "boolean" ||
    (typeof value === "number" && Number.isSafeInteger(value))
  ) {
    return;
  }
  if (typeof value === "string") {
    boundedString(value, label);
    return;
  }
  if (Array.isArray(value)) {
    validatePayloadArray(value, label, depth, budget);
    return;
  }
  if (value !== null && typeof value === "object") {
    validatePayloadObject(value, label, depth, budget);
    return;
  }
  throw new TypeError(`${label} payload contains an unsupported value`);
}

function validatePayloadArray(
  value: readonly unknown[],
  label: string,
  depth: number,
  budget: { remaining: number },
): void {
  if (value.length > maximumGoroutines) {
    throw new Error(`${label} payload array is too large`);
  }
  for (const item of value) {
    validatePayloadValue(item, label, depth + 1, budget);
  }
}

function validatePayloadObject(
  value: object,
  label: string,
  depth: number,
  budget: { remaining: number },
): void {
  const entries = Object.entries(value);
  if (entries.length > 128) {
    throw new Error(`${label} payload object has too many fields`);
  }
  for (const [key, item] of entries) {
    boundedString(key, `${label} field name`);
    validatePayloadValue(item, label, depth + 1, budget);
  }
}

function decodeIDs(value: unknown, label: string): readonly number[] {
  if (value === undefined) {
    return [];
  }
  const result = boundedArray(value, label, maximumGoroutines).map(
    (item, index) => integer(item, `${label}[${String(index)}]`, 1),
  );
  if (new Set(result).size !== result.length) {
    throw new Error(`${label} contains duplicate goroutine ids`);
  }
  return result;
}

function exactKeys(
  value: Record<string, unknown>,
  required: readonly string[],
  optional: readonly string[],
  label: string,
): void {
  const allowed = new Set([...required, ...optional]);
  for (const key of Object.keys(value)) {
    if (!allowed.has(key)) {
      throw new Error(`${label} has unknown field ${JSON.stringify(key)}`);
    }
  }
  for (const key of required) {
    if (!Object.hasOwn(value, key)) {
      throw new Error(`${label} is missing ${JSON.stringify(key)}`);
    }
  }
}

function boundedArray(
  value: unknown,
  label: string,
  maximum: number,
): unknown[] {
  if (!Array.isArray(value)) {
    throw new TypeError(`${label} must be an array`);
  }
  if (value.length > maximum) {
    throw new Error(`${label} exceeds the ${String(maximum)} item limit`);
  }
  return value;
}

function boundedString(value: unknown, label: string): string {
  if (typeof value !== "string") {
    throw new TypeError(`${label} must be a string`);
  }
  if (value.length > maximumStringLength) {
    throw new Error(`${label} exceeds ${String(maximumStringLength)} characters`);
  }
  return value;
}

function optionalString(value: unknown, label: string): string {
  return value === undefined ? "" : boundedString(value, label);
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

function optionalInteger(value: unknown, label: string): number {
  return value === undefined ? 0 : integer(value, label, 0);
}

function optionalBoolean(value: unknown, label: string): boolean {
  if (value === undefined) {
    return false;
  }
  if (typeof value !== "boolean") {
    throw new TypeError(`${label} must be a boolean`);
  }
  return value;
}

function record(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new TypeError(`${label} must be an object`);
  }
  return value as Record<string, unknown>;
}
