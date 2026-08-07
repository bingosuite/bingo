export interface TimeoutHandle {
  unref?(): void;
}

export interface TimeoutTimers {
  set(milliseconds: number, callback: () => void): TimeoutHandle;
  clear(timer: TimeoutHandle): void;
}

export function withTimeout<T>(
  operation: Promise<T>,
  milliseconds: number,
  message: string,
  timers?: TimeoutTimers,
): Promise<T>;
