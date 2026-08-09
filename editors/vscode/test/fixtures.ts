import type {
  Goroutine,
  Location,
  Snapshot,
  Thread,
} from "../src/telemetry.js";

export const location: Location = {
  file: "/workspace/main.go",
  line: 12,
  function: "main.worker",
};

export function goroutine(
  id: number,
  parentId = 0,
  overrides: Partial<Goroutine> = {},
): Goroutine {
  return {
    id,
    parentId,
    status: "waiting",
    waitReason: "chan receive",
    currentLoc: location,
    startLoc: location,
    createdLoc: location,
    threadId: 0,
    current: false,
    ...overrides,
  };
}

export function thread(id: number, goid = 0): Thread {
  return {
    id,
    mid: id,
    goid,
    spinning: false,
    currentLoc: location,
    current: false,
  };
}

export function snapshot(
  goroutines: readonly Goroutine[] = [goroutine(1, 0, { current: true })],
  threads: readonly Thread[] = [thread(10, 1)],
): Snapshot {
  return {
    goroutines,
    threads,
    current: goroutines[0]?.id ?? 0,
    created: [],
    exited: [],
  };
}

export function envelope(
  seq: number,
  kind: string,
  payload: unknown,
): Buffer {
  return Buffer.from(
    JSON.stringify({ v: "1.4", kind, seq, payload }),
    "utf8",
  );
}
