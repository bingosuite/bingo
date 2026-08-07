export type ProcessSignal = (
  pid: number,
  signal: NodeJS.Signals,
) => boolean;

export function terminateOwnedProcessGroup(
  pid: number,
  exit: Promise<unknown>,
  signalProcess?: ProcessSignal,
): Promise<void>;
