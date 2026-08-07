import {
  closeSync,
  mkdirSync,
  openSync,
} from "node:fs";
import { dirname } from "node:path";
import {
  spawn,
  type ChildProcess,
  type SpawnOptions,
} from "node:child_process";

export type ServerProcessOutcome =
  | { readonly kind: "error"; readonly error: Error }
  | {
      readonly kind: "exit";
      readonly code: number | null;
      readonly signal: NodeJS.Signals | null;
    };

export interface SpawnServerRequest {
  readonly binaryPath: string;
  readonly args: readonly string[];
  readonly logPath: string;
}

export interface ServerProcessObservation {
  stopObserving(): void;
}

export interface ServerSpawner {
  (
    request: SpawnServerRequest,
    onOutcome: (outcome: ServerProcessOutcome) => void,
  ): ServerProcessObservation;
}

type SpawnFunction = (
  command: string,
  args: readonly string[],
  options: SpawnOptions,
) => ChildProcess;

export function spawnDetachedServer(
  request: SpawnServerRequest,
  onOutcome: (outcome: ServerProcessOutcome) => void,
  spawnProcess: SpawnFunction = spawn,
): ServerProcessObservation {
  mkdirSync(dirname(request.logPath), { recursive: true });
  const logFD = openSync(request.logPath, "a", 0o600);
  let child: ChildProcess;
  try {
    child = spawnProcess(request.binaryPath, request.args, {
      detached: true,
      shell: false,
      stdio: ["ignore", logFD, logFD],
    });
  } finally {
    closeSync(logFD);
  }

  const handleError = (error: Error): void => {
    onOutcome({ kind: "error", error });
  };
  const handleExit = (
    code: number | null,
    signal: NodeJS.Signals | null,
  ): void => {
    onOutcome({ kind: "exit", code, signal });
  };
  child.on("error", handleError);
  child.on("exit", handleExit);
  child.unref();

  return {
    stopObserving(): void {
      child.off("error", handleError);
      child.off("exit", handleExit);
    },
  };
}
