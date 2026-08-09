import type { Snapshot } from "./telemetry.js";
import { layoutSpawnTree, type TreeLayout } from "./tree.js";

export const maximumTimelineEntries = 100;

export type ConnectionState =
  | "connecting"
  | "connected"
  | "reconnecting"
  | "closed"
  | "error";

export interface TimelineEntry {
  readonly id: number;
  readonly action: "created" | "exited";
  readonly at: number;
}

export interface SessionModel {
  readonly debugSessionId: string;
  readonly debugSessionName: string;
  readonly sessionId: string;
  readonly connection: ConnectionState;
  readonly sessionState: string;
  readonly clients: number;
  readonly lastStop: string;
  readonly error: string;
  readonly seqGap: string;
  readonly lastSeq: number;
  readonly snapshot: Snapshot | undefined;
  readonly selectedGoroutine: number;
  readonly timeline: readonly TimelineEntry[];
}

export interface SessionViewModel extends SessionModel {
  readonly tree: TreeLayout;
  readonly degraded: boolean;

  // What the SERVER left off the wire, kept deliberately separate from
  // tree.omitted (this view's own filter and render cap). Conflating them would
  // let a truncated payload masquerade as a local display choice.
  readonly serverTotals: ServerTotals | undefined;
}

// ServerTotals restates a snapshot's SnapshotTotals in terms this view renders:
// the server's original counts and how many elements never reached us. The
// clipped flags are per collection — the debugger's goroutine and thread scans
// have independent ceilings — so each count is marked a lower bound only when
// its OWN scan was cut short.
export interface ServerTotals {
  readonly goroutines: number;
  readonly threads: number;
  readonly goroutinesClipped: boolean;
  readonly threadsClipped: boolean;
  readonly goroutinesOmitted: number;
  readonly threadsOmitted: number;
}

export interface ConcurrencyViewModel {
  readonly revision: number;
  readonly activeDebugSessionId: string;
  readonly sessions: readonly SessionViewModel[];
}

export function appendLifecycle(
  existing: readonly TimelineEntry[],
  snapshot: Snapshot,
  now: number,
): readonly TimelineEntry[] {
  const additions: TimelineEntry[] = [
    ...snapshot.created.map((id) => ({ id, action: "created" as const, at: now })),
    ...snapshot.exited.map((id) => ({ id, action: "exited" as const, at: now })),
  ];
  return [...existing, ...additions].slice(-maximumTimelineEntries);
}

export function toSessionViewModel(model: SessionModel): SessionViewModel {
  const snapshot = model.snapshot;
  return {
    ...model,
    tree: layoutSpawnTree(snapshot?.goroutines ?? [], undefined, [
      snapshot?.current ?? 0,
      model.selectedGoroutine,
    ]),
    degraded: snapshot === undefined ? false : isDegraded(snapshot),
    serverTotals: serverTotals(snapshot),
  };
}

export function serverTotals(
  snapshot: Snapshot | undefined,
): ServerTotals | undefined {
  const totals = snapshot?.totals;
  if (snapshot === undefined || totals === undefined) {
    return undefined;
  }
  // No clamping: the decoder already REJECTS totals below the delivered counts,
  // because a total that contradicts what arrived is dishonest data rather than
  // a truncation report. Normalizing it here as well would quietly repair the
  // contradiction and hide a broken producer behind a plausible-looking view.
  return {
    goroutines: totals.goroutines,
    threads: totals.threads,
    goroutinesClipped: totals.goroutinesClipped,
    threadsClipped: totals.threadsClipped,
    goroutinesOmitted: totals.goroutines - snapshot.goroutines.length,
    threadsOmitted: totals.threads - snapshot.threads.length,
  };
}

// formatServerCount renders a count the server may have understated. A trailing
// "+" marks a lower bound (the server's scan was clipped), and "n of m" shows
// how much never reached the wire.
export function formatServerCount(
  shown: number,
  total: number,
  clipped: boolean,
): string {
  const bound = clipped ? "+" : "";
  return shown >= total && !clipped
    ? String(shown)
    : shown >= total
      ? `${String(shown)}${bound}`
      : `${String(shown)} of ${String(total)}${bound}`;
}

export function serializeSnapshot(model: SessionModel): string {
  return JSON.stringify(
    {
      sessionId: model.sessionId,
      debugSessionName: model.debugSessionName,
      connection: model.connection,
      state: model.sessionState,
      seq: model.lastSeq,
      snapshot: model.snapshot ?? null,
      timeline: model.timeline,
    },
    undefined,
    2,
  );
}

function isDegraded(snapshot: Snapshot): boolean {
  if (snapshot.goroutines.length !== 1 || snapshot.threads.length !== 0) {
    return false;
  }
  const goroutine = snapshot.goroutines[0];
  return (
    goroutine?.startLoc.file.length === 0 &&
    goroutine.createdLoc.file.length === 0 &&
    goroutine.currentLoc.file.length === 0
  ) ?? false;
}
