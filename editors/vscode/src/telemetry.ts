import { TextDecoder } from "node:util";

export const wireProtocolVersion = "1.4";
export const snapshotCommandKind = "GoroutineSnapshot";

// The decoder budget is the contract the server packs against
// (protocol.MaxGoroutineEventBytes). The transport must sit strictly ABOVE it so
// a frame that violates the contract is delivered and rejected here — as a
// deterministic protocol error — instead of being killed below the decoder by
// `ws`, where it looks indistinguishable from a flaky connection and drives a
// pointless reconnect ladder. See issue #194.
export const maximumEnvelopeBytes = 2 * 1024 * 1024;
export const transportSlackBytes = 64 * 1024;
export const maximumTransportBytes = maximumEnvelopeBytes + transportSlackBytes;

export const maximumGoroutines = 5000;
export const maximumThreads = 2048;
export const maximumStringLength = 4096;

// created/exited are lifecycle DELTAS, not packed elements. The packer never
// trims them — a truncated delta would silently corrupt a consumer's lifecycle
// state — so they can legitimately exceed the packed-element caps (the
// debugger's runtime scan reaches 8192). A count cap here could therefore only
// ever falsely reject a legal frame. The real bound is the byte contract
// already enforced before parse: a delta id costs at least two bytes on the
// wire (a digit plus its separator), so this ceiling can never reject a frame
// that passed that check, while still bounding the array explicitly.
const maximumLifecycleIDs = maximumEnvelopeBytes / 2;

const maximumPayloadNodes = 20_000;
const utf8 = new TextDecoder("utf-8", { fatal: true });

// The byte contract is scoped to the goroutine event family. Everything else on
// the wire is deliberately unbounded (a large Locals/Frames/Evaluate response is
// legal), so an oversized frame is only a contract violation for these two.
const boundedKinds = new Set(["GoroutineSnapshot", "Goroutines"]);

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

// The kinds the observer actually reads in applyEvent. Only these get deep
// payload validation; everything else is a broadcast the observer ignores, and
// walking a payload we never read is how a large-but-legal EventGoroutines used
// to take the whole view down. Keep this in lockstep with observer.ts.
const consumedKinds = new Set([
  "SessionState",
  "BreakpointHit",
  "Paused",
  "Stepped",
  "Panic",
  "Continued",
  "ProcessExited",
  "Error",
  "GoroutineSnapshot",
]);

// TelemetryProtocolError marks a violation of the wire contract: a malformed
// envelope, an unsupported version, or a payload the server should never have
// produced. It is deterministic — the same peer will produce it again — so
// reconnecting cannot help and the observer must stop instead.
export class TelemetryProtocolError extends Error {
  public constructor(message: string) {
    super(message);
    this.name = "TelemetryProtocolError";
  }
}

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

// SnapshotTotals reports the server's ORIGINAL untrimmed counts. It is present
// only when the server omitted elements or its runtime scan was clipped, so its
// presence alone means "this is not everything". When clipped is true the
// goroutine count is itself a lower bound, and the UI must say so.
export interface SnapshotTotals {
  readonly goroutines: number;
  readonly threads: number;
  readonly clipped: boolean;
}

export interface Snapshot {
  readonly goroutines: readonly Goroutine[];
  readonly threads: readonly Thread[];
  readonly current: number;
  readonly created: readonly number[];
  readonly exited: readonly number[];
  readonly totals?: SnapshotTotals;
}

export interface GoroutineList {
  readonly goroutines: readonly Goroutine[];
  readonly totals?: SnapshotTotals;
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
    throw new TelemetryProtocolError("telemetry event is not valid JSON");
  }
  const envelope = record(decoded, "telemetry event");
  exactKeys(envelope, ["v", "kind", "seq", "payload"], [], "telemetry event");
  if (envelope.v !== wireProtocolVersion) {
    throw new TelemetryProtocolError(
      `telemetry protocol ${JSON.stringify(envelope.v)} is incompatible with ${wireProtocolVersion}`,
    );
  }
  const kind = boundedString(envelope.kind, "event kind");
  if (!eventKinds.has(kind)) {
    throw new TelemetryProtocolError(
      `unknown telemetry event kind ${JSON.stringify(kind)}`,
    );
  }
  const seq = integer(envelope.seq, "event seq", 1);
  const payload = record(envelope.payload, "event payload");
  if (kind === snapshotCommandKind) {
    return { kind, seq, payload, snapshot: decodeSnapshot(payload) };
  }
  if (!consumedKinds.has(kind)) {
    // Shallow parse: the envelope stayed strict, but the body of an event the
    // observer never reads is not worth walking — and walking it is exactly how
    // a broadcast EventGoroutines used to exceed the node budget and kill the
    // view. The raw payload is deliberately NOT forwarded, so applyEvent can
    // never see an unvalidated value.
    return { kind, seq, payload: {} };
  }
  validatePayload(kind, payload);
  return { kind, seq, payload };
}

function decodeText(data: TelemetryData): string {
  const bytes = dataBytes(data);
  if (bytes > maximumEnvelopeBytes) {
    throw oversizedError(data, bytes);
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
    throw new TelemetryProtocolError("telemetry event is not valid UTF-8");
  }
}

// oversizedError decides whether an over-budget frame is a contract violation
// (fatal, since retrying reproduces it) or merely an event this observer does
// not bound (transient, so the reconnect ladder still applies).
//
// The kind is only known after parsing, and parsing must not happen before the
// byte check — so the envelope PREFIX is scanned instead. The server emits
// "v","kind","seq","payload" in that order, so the kind sits at the front. An
// unrecognisable prefix falls back to non-fatal: latching the view dead on a
// guess is worse than a reconnect.
function oversizedError(data: TelemetryData, bytes: number): Error {
  const message = `telemetry event of ${String(bytes)} bytes exceeds the ${String(maximumEnvelopeBytes)} byte contract`;
  const kind = envelopeKindPattern.exec(envelopePrefix(data))?.[1];
  return kind !== undefined && boundedKinds.has(kind)
    ? new TelemetryProtocolError(message)
    : new Error(message);
}

const envelopePrefixBytes = 512;
const envelopeKindPattern = /"kind"\s*:\s*"([A-Za-z]{1,64})"/u;

function envelopePrefix(data: TelemetryData): string {
  if (typeof data === "string") {
    return data.slice(0, envelopePrefixBytes);
  }
  if (isBufferList(data)) {
    const parts: Buffer[] = [];
    let size = 0;
    for (const part of data) {
      parts.push(part);
      size += part.length;
      if (size >= envelopePrefixBytes) {
        break;
      }
    }
    return bufferPrefix(Buffer.concat(parts));
  }
  return bufferPrefix(
    Buffer.isBuffer(data) ? data : Buffer.from(new Uint8Array(data)),
  );
}

// A prefix can slice a multibyte character in half; only ASCII is matched
// against it, so lossy decoding is correct here.
function bufferPrefix(buffer: Buffer): string {
  return buffer.subarray(0, envelopePrefixBytes).toString("latin1");
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
    ["current", "created", "exited", "totals"],
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
      throw new TelemetryProtocolError(
        `duplicate goroutine id ${String(goroutine.id)}`,
      );
    }

    ids.add(goroutine.id);
  }
  const totals = decodeTotals(payload.totals);
  const threads = rawThreads.map((value, index) =>
    decodeThread(value, `threads[${String(index)}]`),
  );
  if (totals !== undefined) {
    // Totals are the server's ORIGINAL counts, so they can never be below what
    // actually arrived. A total that is smaller is not a truncation report, it
    // is a contradiction — and silently trusting it would make the UI claim a
    // partial view is complete, or render a negative omission count.
    assertTotalCovers(totals.goroutines, goroutines.length, "totals.goroutines");
    assertTotalCovers(totals.threads, threads.length, "totals.threads");
  }
  return {
    goroutines,
    threads,
    current: optionalInteger(payload.current, "current"),
    created: decodeIDs(payload.created, "created"),
    exited: decodeIDs(payload.exited, "exited"),
    ...(totals === undefined ? {} : { totals }),
  };
}

function assertTotalCovers(total: number, delivered: number, label: string): void {
  if (total < delivered) {
    throw new TelemetryProtocolError(
      `${label} (${String(total)}) is below the ${String(delivered)} delivered`,
    );
  }
}

// decodeGoroutineList decodes the 1.4 EventGoroutines shape. The observer does
// not consume that event, so this is exported for the protocol-level tests that
// pin the shared shape rather than wired into decodeEvent.
export function decodeGoroutineList(
  payload: Record<string, unknown>,
): GoroutineList {
  exactKeys(payload, ["goroutines"], ["totals"], "goroutines payload");
  const raw = protocolArray(payload.goroutines, "goroutines", maximumGoroutines);
  const totals = decodeTotals(payload.totals);
  const goroutines = raw.map((value, index) =>
    decodeGoroutine(value, `goroutines[${String(index)}]`),
  );
  if (totals !== undefined) {
    assertTotalCovers(totals.goroutines, goroutines.length, "totals.goroutines");
    // This shape carries no threads at all, so any thread total is meaningless
    // rather than merely partial.
    if (totals.threads !== 0) {
      throw new TelemetryProtocolError(
        `totals.threads (${String(totals.threads)}) is meaningless without a thread list`,
      );
    }
  }
  return {
    goroutines,
    ...(totals === undefined ? {} : { totals }),
  };
}

function decodeTotals(value: unknown): SnapshotTotals | undefined {
  if (value === undefined) {
    return undefined;
  }
  const item = record(value, "totals");
  exactKeys(item, [], ["goroutines", "threads", "clipped"], "totals");
  return {
    goroutines: optionalInteger(item.goroutines, "totals.goroutines"),
    threads: optionalInteger(item.threads, "totals.threads"),
    clipped: optionalBoolean(item.clipped, "totals.clipped"),
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
      throw new TelemetryProtocolError(
      `unknown session state ${JSON.stringify(state)}`,
    );
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
    throw new TelemetryProtocolError(`${label} payload is too large`);
  }
  if (depth > 12) {
    throw new TelemetryProtocolError(`${label} payload nesting is too deep`);
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
  throw new TelemetryProtocolError(`${label} payload contains an unsupported value`);
}

function validatePayloadArray(
  value: readonly unknown[],
  label: string,
  depth: number,
  budget: { remaining: number },
): void {
  if (value.length > maximumGoroutines) {
    throw new TelemetryProtocolError(`${label} payload array is too large`);
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
    throw new TelemetryProtocolError(`${label} payload object has too many fields`);
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
  const result = boundedArray(value, label, maximumLifecycleIDs).map(
    (item, index) => integer(item, `${label}[${String(index)}]`, 1),
  );
  if (new Set(result).size !== result.length) {
    throw new TelemetryProtocolError(`${label} contains duplicate goroutine ids`);
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
      throw new TelemetryProtocolError(`${label} has unknown field ${JSON.stringify(key)}`);
    }
  }
  for (const key of required) {
    if (!Object.hasOwn(value, key)) {
      throw new TelemetryProtocolError(`${label} is missing ${JSON.stringify(key)}`);
    }
  }
}

function boundedArray(
  value: unknown,
  label: string,
  maximum: number,
): unknown[] {
  if (!Array.isArray(value)) {
    throw new TelemetryProtocolError(`${label} must be an array`);
  }
  if (value.length > maximum) {
    throw new TelemetryProtocolError(`${label} exceeds the ${String(maximum)} item limit`);
  }
  return value;
}

function boundedString(value: unknown, label: string): string {
  if (typeof value !== "string") {
    throw new TelemetryProtocolError(`${label} must be a string`);
  }
  if (value.length > maximumStringLength) {
    throw new TelemetryProtocolError(
      `${label} exceeds ${String(maximumStringLength)} characters`,
    );
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
    throw new TelemetryProtocolError(
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
    throw new TelemetryProtocolError(`${label} must be a boolean`);
  }
  return value;
}

function record(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new TelemetryProtocolError(`${label} must be an object`);
  }
  return value as Record<string, unknown>;
}
