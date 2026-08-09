import type { ChildProcess } from "node:child_process";

export type ProcessSignal = (
  pid: number,
  signal: NodeJS.Signals | number,
) => boolean;

export type OwnedProcessOutcome =
  | { readonly kind: "error"; readonly error: Error }
  | {
      readonly kind: "exit";
      readonly code: number | null;
      readonly signal: NodeJS.Signals | null;
    };

export interface OwnedProcess {
  readonly pid: number | undefined;
  readonly outcome: Promise<OwnedProcessOutcome>;
  isFinished(): boolean;
  ref(): void;
}

export interface OwnedProcessCleanupOptions {
  readonly gracefulTimeoutMs?: number;
  readonly exitTimeoutMs?: number;
  readonly groupTimeoutMs?: number;
  readonly pollIntervalMs?: number;
  readonly signalProcess?: ProcessSignal;
}

export function observeOwnedProcess(child: ChildProcess): OwnedProcess;

export function waitForOwnedProcessExit(
  ownedProcess: OwnedProcess,
  timeoutMs: number,
  message: string,
): Promise<OwnedProcessOutcome>;

export function terminateOwnedProcessGroup(
  ownedProcess: OwnedProcess,
  options?: OwnedProcessCleanupOptions,
): Promise<OwnedProcessOutcome>;
