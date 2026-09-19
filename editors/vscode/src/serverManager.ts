import type {
  BingoEndpoint,
  BingoServerConfiguration,
} from "./configuration.js";
import type {
  HealthProbe,
  HealthProbeResult,
} from "./health.js";
import { minimumHealthProbeTimeoutMs } from "./health.js";
import {
  supportedTargetFor,
  type RuntimePlatform,
  type SupportedTarget,
} from "./platform.js";
import type {
  ServerProcessObservation,
  ServerProcessOutcome,
  ServerSpawner,
} from "./serverProcess.js";

export type ServerManagerErrorCode =
  | "binaryUnavailable"
  | "cancelled"
  | "endpointOccupied"
  | "healthProbeFailed"
  | "invalidConfiguration"
  | "readinessTimedOut"
  | "spawnFailed"
  | "unsupportedAutoStart";

export class ServerManagerError extends Error {
  public override readonly name = "ServerManagerError";

  public constructor(
    public readonly code: ServerManagerErrorCode,
    message: string,
    options?: ErrorOptions,
  ) {
    super(message, options);
  }
}

export interface ServerManagerDependencies {
  readonly probe: HealthProbe;
  readonly resolveBinary: (target: SupportedTarget) => Promise<string>;
  readonly spawnServer: ServerSpawner;
  readonly delay: (milliseconds: number, signal: AbortSignal) => Promise<void>;
  readonly now: () => number;
  readonly runtime: RuntimePlatform;
  readonly logPathFor: (endpoint: BingoEndpoint) => Promise<string>;
  readonly log: (message: string) => void;
}

const probeTimeoutMs = 1000;
const pollIntervalMs = 100;
const finalWindowMs = 50;
const finalPollIntervalMs = 10;

interface StartupAttempt {
  readonly controller: AbortController;
  readonly promise: Promise<BingoEndpoint>;
  waiters: number;
}

export class ServerManager {
  readonly #dependencies: ServerManagerDependencies;
  readonly #inFlight = new Map<string, StartupAttempt>();
  #disposed = false;

  public constructor(dependencies: ServerManagerDependencies) {
    this.#dependencies = dependencies;
  }

  public ensureServer(
    config: BingoServerConfiguration,
    signal?: AbortSignal,
  ): Promise<BingoEndpoint> {
    if (this.#disposed || signal?.aborted === true) {
      return Promise.reject(cancelledError());
    }
    if (config.mode === "connectOnly") {
      return awaitWithCancellation(
        Promise.resolve(config.dapEndpoint),
        signal,
      );
    }

    this.validateConfiguration(config);
    const key = endpointKey(config);
    let attempt = this.#inFlight.get(key);
    if (attempt === undefined) {
      const controller = new AbortController();
      attempt = {
        controller,
        promise: this.#ensureAuto(config, controller.signal),
        waiters: 0,
      };
      this.#inFlight.set(key, attempt);
    }
    const ownedAttempt = attempt;
    ownedAttempt.waiters += 1;
    return awaitWithCancellation(ownedAttempt.promise, signal, () => {
      ownedAttempt.waiters -= 1;
      if (ownedAttempt.waiters === 0) {
        if (this.#inFlight.get(key) === ownedAttempt) {
          this.#inFlight.delete(key);
        }
        ownedAttempt.controller.abort();
      }
    });
  }

  public dispose(): void {
    if (this.#disposed) {
      return;
    }
    this.#disposed = true;
    for (const attempt of this.#inFlight.values()) {
      attempt.controller.abort();
    }
    this.#inFlight.clear();
  }

  async #ensureAuto(
    config: BingoServerConfiguration,
    signal: AbortSignal,
  ): Promise<BingoEndpoint> {
    const target = supportedTargetFor(this.#dependencies.runtime);
    if (target === undefined) {
      throw unsupportedTargetError(this.#dependencies.runtime);
    }

    this.#dependencies.log(
      `probing bingo management endpoint ${formatEndpoint(config.managementEndpoint)}`,
    );
    const initial = await this.#probe(
      config.managementEndpoint,
      config.dapEndpoint,
      Math.min(probeTimeoutMs, config.readyTimeoutMs),
      signal,
    );
    if (initial.kind === "compatible") {
      this.#dependencies.log(
        `reusing compatible bingo instance ${initial.health.instanceId}`,
      );
      return config.dapEndpoint;
    }
    if (initial.kind === "incompatible") {
      throw occupiedError(config, initial.reason);
    }
    if (initial.kind === "transportError") {
      throw new ServerManagerError(
        "healthProbeFailed",
        `cannot probe bingo management endpoint ${formatEndpoint(config.managementEndpoint)}: ${initial.error.message}`,
        { cause: initial.error },
      );
    }

    let binaryPath: string;
    try {
      binaryPath = await awaitWithCancellation(
        this.#dependencies.resolveBinary(target), signal,
      );
    } catch (error: unknown) {
      this.#throwIfCancelled(signal, error);
      throw new ServerManagerError(
        "binaryUnavailable",
        `cannot use the bundled ${target} bingo server: ${errorMessage(error)}`,
        { cause: error },
      );
    }
    this.#throwIfCancelled(signal);

    let logPath: string;
    try {
      logPath = await awaitWithCancellation(
        this.#dependencies.logPathFor(config.managementEndpoint), signal,
      );
    } catch (error: unknown) {
      this.#throwIfCancelled(signal, error);
      throw error;
    }
    this.#throwIfCancelled(signal);
    const args = serverArguments(config);
    this.#dependencies.log(
      `starting bundled bingo server; logs: ${logPath}`,
    );

    let childOutcome: ServerProcessOutcome | undefined;
    let observation: ServerProcessObservation;
    let observing = true;
    this.#throwIfCancelled(signal);
    try {
      observation = this.#dependencies.spawnServer(
        { binaryPath, args, logPath },
        (outcome) => {
          childOutcome = outcome;
          if (!observing && outcome.kind === "error") {
            this.#dependencies.log(
              `bingo server process error: ${outcome.error.message}; logs: ${logPath}`,
            );
          }
        },
      );
    } catch (error: unknown) {
      throw new ServerManagerError(
        "spawnFailed",
        `cannot start bundled bingo server for ${formatEndpoint(config.managementEndpoint)}: ${errorMessage(error)}; logs: ${logPath}`,
        { cause: error },
      );
    }

    const deadline = this.#dependencies.now() + config.readyTimeoutMs;
    let lastProbe: HealthProbeResult = initial;
    try {
      const probeReady = async (timeoutMs: number): Promise<boolean> => {
        lastProbe = await this.#probe(
          config.managementEndpoint,
          config.dapEndpoint,
          Math.min(probeTimeoutMs, timeoutMs),
          signal,
        );
        if (lastProbe.kind === "compatible") {
          this.#dependencies.log(
            `bingo instance ${lastProbe.health.instanceId} is ready at ${formatEndpoint(config.dapEndpoint)}`,
          );
          return true;
        }
        if (lastProbe.kind === "incompatible") {
          throw occupiedError(config, lastProbe.reason);
        }
        return false;
      };

      const initialRemaining = deadline - this.#dependencies.now();
      if (
        initialRemaining >= minimumHealthProbeTimeoutMs &&
        (await probeReady(initialRemaining))
      ) {
        return config.dapEndpoint;
      }

      for (;;) {
        const remaining = deadline - this.#dependencies.now();
        if (remaining <= 0) {
          break;
        }
        if (remaining <= minimumHealthProbeTimeoutMs) {
          await this.#dependencies.delay(remaining, signal);
          break;
        }
        const delayMs =
          remaining > finalWindowMs
            ? Math.min(pollIntervalMs, remaining - finalWindowMs)
            : Math.min(
                finalPollIntervalMs,
                remaining - minimumHealthProbeTimeoutMs,
              );
        await this.#dependencies.delay(delayMs, signal);
        this.#throwIfCancelled(signal);
        const probeRemaining = deadline - this.#dependencies.now();
        if (probeRemaining < minimumHealthProbeTimeoutMs) {
          if (probeRemaining > 0) {
            await this.#dependencies.delay(
              probeRemaining,
              signal,
            );
          }
          break;
        }
        if (await probeReady(probeRemaining)) {
          return config.dapEndpoint;
        }
      }
    } catch (error: unknown) {
      this.#throwIfCancelled(signal, error);
      throw error;
    } finally {
      observing = false;
      observation.stopObserving();
    }

    const outcome = describeOutcome(childOutcome);
    const probe = describeProbe(lastProbe);
    const message =
      `bundled bingo server did not become ready at ${formatEndpoint(config.managementEndpoint)} ` +
      `within ${String(config.readyTimeoutMs)}ms (${probe}; ${outcome}); ` +
      `DAP ${formatEndpoint(config.dapEndpoint)}; logs: ${logPath}`;
    throw new ServerManagerError(
      childOutcome === undefined ? "readinessTimedOut" : "spawnFailed",
      message,
    );
  }

  async #probe(
    managementEndpoint: BingoEndpoint,
    dapEndpoint: BingoEndpoint,
    timeoutMs: number,
    signal: AbortSignal,
  ): Promise<HealthProbeResult> {
    try {
      this.#throwIfCancelled(signal);
      const result = await awaitWithCancellation(
        this.#dependencies.probe(
          managementEndpoint, dapEndpoint, timeoutMs, signal,
        ),
        signal,
      );
      this.#throwIfCancelled(
        signal,
        result.kind === "transportError" ? result.error : undefined,
      );
      return result;
    } catch (error: unknown) {
      this.#throwIfCancelled(signal, error);
      throw error;
    }
  }

  #throwIfCancelled(signal: AbortSignal, error?: unknown): void {
    if (signal.aborted || isAbortError(error)) {
      throw cancelledError();
    }
  }

  public validateConfiguration(config: BingoServerConfiguration): void {
    if (config.mode === "connectOnly") {
      return;
    }
    if (
      config.managementEndpoint.host !== "127.0.0.1" ||
      config.dapEndpoint.host !== "127.0.0.1"
    ) {
      throw new ServerManagerError(
        "invalidConfiguration",
        'bingo serverMode "auto" requires managementHost and dapHost to be 127.0.0.1; use "connectOnly" for remote or custom endpoints',
      );
    }
    if (
      formatEndpoint(config.managementEndpoint) ===
      formatEndpoint(config.dapEndpoint)
    ) {
      throw new ServerManagerError(
        "invalidConfiguration",
        'bingo serverMode "auto" requires distinct management and DAP endpoints; choose different managementPort and dapPort values',
      );
    }
    const target = supportedTargetFor(this.#dependencies.runtime);
    if (target === undefined) {
      throw unsupportedTargetError(this.#dependencies.runtime);
    }
  }
}

function serverArguments(config: BingoServerConfiguration): string[] {
  return [
    "-addr",
    formatEndpoint(config.managementEndpoint),
    "-dap-addr",
    formatEndpoint(config.dapEndpoint),
    "-idle-timeout",
    `${String(config.idleTimeoutMs)}ms`,
  ];
}

function endpointKey(config: BingoServerConfiguration): string {
  return `${formatEndpoint(config.managementEndpoint)}|${formatEndpoint(config.dapEndpoint)}`;
}

function formatEndpoint(endpoint: BingoEndpoint): string {
  return `${endpoint.host}:${String(endpoint.port)}`;
}

function occupiedError(
  config: BingoServerConfiguration,
  reason: string,
): ServerManagerError {
  return new ServerManagerError(
    "endpointOccupied",
    `cannot use bingo management endpoint ${formatEndpoint(config.managementEndpoint)}: ${reason}. Update the existing server or choose unused managementPort and dapPort values; bingo never stops or replaces a shared server`,
  );
}

function unsupportedTargetError(runtime: RuntimePlatform): ServerManagerError {
  return new ServerManagerError(
    "unsupportedAutoStart",
    `bingo server autostart supports only linux/x64 and darwin/arm64, not ${runtime.platform}/${runtime.arch}; use serverMode "connectOnly" with an existing server`,
  );
}

function describeOutcome(outcome: ServerProcessOutcome | undefined): string {
  if (outcome === undefined) {
    return "child is still running or produced no exit status";
  }
  if (outcome.kind === "error") {
    return `child error: ${outcome.error.message}`;
  }
  return `child exited with code ${String(outcome.code)} signal ${String(outcome.signal)}`;
}

function describeProbe(probe: HealthProbeResult): string {
  if (probe.kind === "transportError") {
    return `last health error: ${probe.error.message}`;
  }
  if (probe.kind === "incompatible") {
    return `last health response: ${probe.reason}`;
  }
  return `last health result: ${probe.kind}`;
}

function awaitWithCancellation<T>(
  promise: Promise<T>,
  signal?: AbortSignal,
  onSettled?: () => void,
): Promise<T> {
  if (signal === undefined && onSettled === undefined) {
    return promise;
  }
  return new Promise((resolve, reject) => {
    let settled = false;
    const finish = (complete: () => void): void => {
      if (settled) {
        return;
      }
      settled = true;
      signal?.removeEventListener("abort", onAbort);
      onSettled?.();
      complete();
    };
    const onAbort = (): void => {
      finish(() => { reject(cancelledError()); });
    };
    signal?.addEventListener("abort", onAbort, { once: true });
    void promise.then(
      (value) => { finish(() => { resolve(value); }); },
      (error: unknown) => {
        finish(() => {
          reject(error instanceof Error ? error : new Error(String(error), { cause: error }));
        });
      },
    );
    if (signal?.aborted === true) {
      onAbort();
    }
  });
}

function cancelledError(): ServerManagerError {
  return new ServerManagerError("cancelled", "bingo server startup was cancelled");
}

function isAbortError(error: unknown): boolean {
  return error instanceof Error && error.name === "AbortError";
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export function defaultDelay(
  milliseconds: number,
  signal: AbortSignal,
): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(abortException());
      return;
    }
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, milliseconds);
    const onAbort = (): void => {
      clearTimeout(timer);
      signal.removeEventListener("abort", onAbort);
      reject(abortException());
    };
    signal.addEventListener("abort", onAbort, { once: true });
    void Promise.resolve().then(() => {
      if (signal.aborted) {
        onAbort();
      }
    });
  });
}

function abortException(): Error {
  const error = new Error("operation cancelled");
  error.name = "AbortError";
  return error;
}
