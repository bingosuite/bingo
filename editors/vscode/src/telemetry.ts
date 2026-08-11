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
// trims them — a truncated delta would silently corrupt this observer's
// lifecycle state — so they can legitimately exceed the packed-element caps and
// must NOT be validated against maximumGoroutines. What actually bounds them is
// the producer's runtime walk: both are set differences over the live goroutine
// set, which the debugger caps at its scan ceiling. This mirrors the producer's
// MaxLifecycleDeltaIDs and is drift-checked against it, because a producer that
// raised its scan ceiling without raising this would have every large snapshot
// deterministically rejected here.
export const maximumLifecycleIDs = 8192;

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
// only when the server omitted elements or one of its runtime scans was clipped,
// so its presence alone means "this is not everything".
//
// The clipped flags are per collection because the debugger walks goroutines and
// threads under independent ceilings. Each flag says only "this count is a lower
// bound"; the counts themselves are always the original scanned totals. Sharing
// one flag between them would mark an exact count as approximate.
export interface SnapshotTotals {
  readonly goroutines: number;
  readonly threads: number;
  readonly goroutinesClipped: boolean;
  readonly threadsClipped: boolean;
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
  const current = optionalInteger(payload.current, "current");
  if (current !== 0 && !ids.has(current)) {
    // The current goroutine is a required anchor, so a non-degraded snapshot
    // always carries it; a degraded one reports current 0. A current that names
    // a goroutine we did not receive is a dangling reference — the view would
    // either select something that does not exist or silently substitute.
    throw new TelemetryProtocolError(
      `current goroutine ${String(current)} is not among the delivered goroutines`,
    );
  }
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
    current,
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
  exactKeys(
    item,
    [],
    ["goroutines", "threads", "goroutinesClipped", "threadsClipped"],
    "totals",
  );
  return {
    goroutines: optionalInteger(item.goroutines, "totals.goroutines"),
    threads: optionalInteger(item.threads, "totals.threads"),
    goroutinesClipped: optionalBoolean(
      item.goroutinesClipped,
      "totals.goroutinesClipped",
    ),
    threadsClipped: optionalBoolean(item.threadsClipped, "totals.threadsClipped"),
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
    // SessionState bypasses the generic walk, so it must apply the same
    // bounded/unbounded split by hand: it is NOT a bounded kind, so an
    // over-long string here is this process's own limit being hit, not a
    // promise the server broke, and it must not latch the view dead.
    const limits: walkLimits = { remaining: maximumPayloadNodes, bounded: boundedKinds.has(kind) };
    sizedString(payload.sessionID, "sessionID", limits);
    // `state` is a closed four-value enum, so it is NOT sized like sessionID:
    // any string past the length cap is necessarily outside the enum, i.e. a
    // proven violation. Gating it on length first would report that proof as a
    // transient size overrun and retry a frame that can never become valid.
    if (typeof payload.state !== "string") {
      throw new TelemetryProtocolError("session state must be a string");
    }
    if (!["idle", "running", "suspended", "exited"].includes(payload.state)) {
      throw new TelemetryProtocolError(
        `unknown session state ${JSON.stringify(payload.state.slice(0, 64))}`,
      );
    }
    integer(payload.clients, "session clients", 0);
    return;
  }
  const budget = { remaining: maximumPayloadNodes, bounded: boundedKinds.has(kind) };
  validatePayloadValue(payload, kind, 0, budget);
}

// walkLimits carries the generic walk's node budget plus whether the kind being
// walked is one the CONTRACT bounds.
//
// The size checks below (string length, array length, object width, field-name
// length, node budget, depth) are the bounded family's limits. Applying them to
// a consumed-but-unbounded kind is legitimate defence for this process, but a
// failure there is NOT a proven contract violation — the server is explicitly
// allowed to emit a long `Error` message or a wide/deep `Frames` list — so it
// must not latch the observer dead. It stays an ordinary Error, which keeps the
// reconnect ladder, and the view recovers on the next snapshot. Only a violation
// of something the server actually promised is terminal; that is the same line
// `oversizedError` draws.
//
// What stays TelemetryProtocolError for every kind is the checks that are not
// size-derived at all: a value `JSON.parse` cannot have produced (`undefined`,
// a function — reachable only via a hand-built object, never off the wire) and a
// wrong-TYPED string. No server may produce those at any size. The walk checks
// JSON SHAPE, not per-kind schemas, so a well-formed-but-wrong body in a
// consumed unbounded kind is accepted and degrades to that kind's fallback.
// `SessionState` is validated by hand above and adds the one per-kind rule that
// IS a proof: its `state` enum is closed, so an unknown value — including one
// that is unknown only because it is enormous — is terminal, not a size overrun.
//
// `bounded` is LATENT today and that is deliberate, not an oversight: the two
// bounded kinds never reach this walk — `GoroutineSnapshot` is handled by the
// typed decoders above and `Goroutines` is shallow-ignored — so the bounded
// family's fatality comes from those decoders, NOT from here. Do not read this
// flag as that protection. It exists so that adding either kind to
// `consumedKinds` cannot silently downgrade a real violation to a retry.
interface walkLimits {
  remaining: number;
  readonly bounded: boolean;
}

// tooLarge classifies a size-derived rejection by whether the kind is bounded.
function tooLarge(limits: walkLimits, message: string): Error {
  return limits.bounded ? new TelemetryProtocolError(message) : new Error(message);
}

// sizedString splits the two failures a string can have: a wrong TYPE is
// structural and always terminal, while exceeding the length cap is size-derived
// and follows `tooLarge`'s bounded/unbounded rule.
function sizedString(value: unknown, label: string, limits: walkLimits): string {
  if (typeof value !== "string") {
    throw new TelemetryProtocolError(`${label} must be a string`);
  }
  if (value.length > maximumStringLength) {
    throw tooLarge(
      limits,
      `${label} exceeds ${String(maximumStringLength)} characters`,
    );
  }
  return value;
}

function validatePayloadValue(
  value: unknown,
  label: string,
  depth: number,
  limits: walkLimits,
): void {
  limits.remaining -= 1;
  if (limits.remaining < 0) {
    throw tooLarge(limits, `${label} payload is too large`);
  }
  if (depth > 12) {
    throw tooLarge(limits, `${label} payload nesting is too deep`);
  }
  if (value === null || typeof value === "boolean") {
    return;
  }
  // Any finite JSON number is legal here. Requiring a SAFE INTEGER made a plain
  // `1.5` — or any value past 2^53 — terminate a consumed UNBOUNDED kind, which
  // is the exact "latch the view dead over something the server was entitled to
  // send" failure this split exists to stop. Integer-ness is only a rule for the
  // bounded family's ids, and the typed decoders enforce it there. A non-finite
  // number can only arrive by magnitude overflow (`1e400` parses to `Infinity`),
  // so it is size-derived and follows `tooLarge`.
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw tooLarge(limits, `${label} payload contains an out-of-range number`);
    }
    return;
  }
  if (typeof value === "string") {
    if (value.length > maximumStringLength) {
      throw tooLarge(
        limits,
        `${label} exceeds ${String(maximumStringLength)} characters`,
      );
    }
    return;
  }
  if (Array.isArray(value)) {
    validatePayloadArray(value, label, depth, limits);
    return;
  }
  if (value !== null && typeof value === "object") {
    validatePayloadObject(value, label, depth, limits);
    return;
  }
  throw new TelemetryProtocolError(`${label} payload contains an unsupported value`);
}

function validatePayloadArray(
  value: readonly unknown[],
  label: string,
  depth: number,
  limits: walkLimits,
): void {
  if (value.length > maximumGoroutines) {
    throw tooLarge(limits, `${label} payload array is too large`);
  }
  for (const item of value) {
    validatePayloadValue(item, label, depth + 1, limits);
  }
}

function validatePayloadObject(
  value: object,
  label: string,
  depth: number,
  limits: walkLimits,
): void {
  const entries = Object.entries(value);
  if (entries.length > 128) {
    throw tooLarge(limits, `${label} payload object has too many fields`);
  }
  for (const [key, item] of entries) {
    if (key.length > maximumStringLength) {
      throw tooLarge(limits, `${label} field name is too long`);
    }
    validatePayloadValue(item, label, depth + 1, limits);
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
