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
  };
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
  return goroutine?.id === 0 && goroutine.status === "unknown" && goroutine.current === true;
}
