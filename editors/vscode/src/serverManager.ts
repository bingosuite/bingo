import type {
  BingoEndpoint,
  BingoServerConfiguration,
} from "./configuration.js";
import type {
  HealthProbe,
  HealthProbeResult,
} from "./health.js";
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

export class ServerManager {
  readonly #dependencies: ServerManagerDependencies;
  readonly #inFlight = new Map<string, Promise<BingoEndpoint>>();
  readonly #lifetime = new AbortController();
  #disposed = false;

  public constructor(dependencies: ServerManagerDependencies) {
    this.#dependencies = dependencies;
  }

  public ensureServer(
    config: BingoServerConfiguration,
    signal?: AbortSignal,
  ): Promise<BingoEndpoint> {
    if (this.#disposed) {
      return Promise.reject(cancelledError());
    }
    if (config.mode === "connectOnly") {
      return awaitWithCancellation(
        Promise.resolve(config.dapEndpoint),
        signal,
      );
    }

    this.#validateAutoConfiguration(config);
    const key = endpointKey(config);
    let ensured = this.#inFlight.get(key);
    if (ensured === undefined) {
      ensured = this.#ensureAuto(config);
      this.#inFlight.set(key, ensured);
      void ensured.finally(() => {
        if (this.#inFlight.get(key) === ensured) {
          this.#inFlight.delete(key);
        }
      }).catch(() => undefined);
    }
    return awaitWithCancellation(ensured, signal);
  }

  public dispose(): void {
    if (this.#disposed) {
      return;
    }
    this.#disposed = true;
    this.#lifetime.abort();
    this.#inFlight.clear();
  }

  async #ensureAuto(
    config: BingoServerConfiguration,
  ): Promise<BingoEndpoint> {
    const target = supportedTargetFor(this.#dependencies.runtime);
    if (target === undefined) {
      throw unsupportedTargetError(this.#dependencies.runtime);
    }

    this.#dependencies.log(
      `probing bingo management endpoint ${formatEndpoint(config.managementEndpoint)}`,
    );
    const initial = await this.#dependencies.probe(
      config.managementEndpoint,
      config.dapEndpoint,
      Math.min(probeTimeoutMs, config.readyTimeoutMs),
      this.#lifetime.signal,
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
      binaryPath = await this.#dependencies.resolveBinary(target);
    } catch (error: unknown) {
      throw new ServerManagerError(
        "binaryUnavailable",
        `cannot use the bundled ${target} bingo server: ${errorMessage(error)}`,
        { cause: error },
      );
    }

    const logPath = await this.#dependencies.logPathFor(
      config.managementEndpoint,
    );
    const args = serverArguments(config);
    this.#dependencies.log(
      `starting bundled bingo server; logs: ${logPath}`,
    );

    let childOutcome: ServerProcessOutcome | undefined;
    let observation: ServerProcessObservation;
    try {
      observation = this.#dependencies.spawnServer(
        { binaryPath, args, logPath },
        (outcome) => {
          childOutcome = outcome;
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
      for (;;) {
        const remaining = deadline - this.#dependencies.now();
        if (remaining <= 0) {
          break;
        }
        await this.#dependencies.delay(
          Math.min(pollIntervalMs, remaining),
          this.#lifetime.signal,
        );
        lastProbe = await this.#dependencies.probe(
          config.managementEndpoint,
          config.dapEndpoint,
          Math.min(probeTimeoutMs, remaining),
          this.#lifetime.signal,
        );
        if (lastProbe.kind === "compatible") {
          this.#dependencies.log(
            `bingo instance ${lastProbe.health.instanceId} is ready at ${formatEndpoint(config.dapEndpoint)}`,
          );
          return config.dapEndpoint;
        }
        if (lastProbe.kind === "incompatible") {
          throw occupiedError(config, lastProbe.reason);
        }
      }
    } catch (error: unknown) {
      if (isAbortError(error)) {
        throw cancelledError();
      }
      throw error;
    } finally {
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

  #validateAutoConfiguration(config: BingoServerConfiguration): void {
    if (
      config.managementEndpoint.host !== "127.0.0.1" ||
      config.dapEndpoint.host !== "127.0.0.1"
    ) {
      throw new ServerManagerError(
        "invalidConfiguration",
        'bingo serverMode "auto" requires managementHost and dapHost to be 127.0.0.1; use "connectOnly" for remote or custom endpoints',
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
    `cannot use bingo management endpoint ${formatEndpoint(config.managementEndpoint)}: ${reason}; no server was started`,
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
): Promise<T> {
  if (signal === undefined) {
    return promise;
  }
  if (signal.aborted) {
    return Promise.reject(cancelledError());
  }
  return new Promise((resolve, reject) => {
    const onAbort = (): void => {
      reject(cancelledError());
    };
    signal.addEventListener("abort", onAbort, { once: true });
    void promise.then(resolve, reject).finally(() => {
      signal.removeEventListener("abort", onAbort);
    });
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
