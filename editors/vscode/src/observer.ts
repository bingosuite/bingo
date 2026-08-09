import WebSocket from "ws";

import type { BingoEndpoint } from "./configuration.js";
import { appendLifecycle, type SessionModel } from "./model.js";
import {
  decodeEvent,
  maximumEnvelopeBytes,
  SequenceTracker,
  snapshotCommand,
  type TelemetryData,
} from "./telemetry.js";

const socketOpenState = 1;
const reconnectDelays = [100, 250, 500, 1000, 2000, 4000] as const;

export interface Socket {
  readonly readyState: number;
  onOpen(listener: () => void): void;
  onMessage(listener: (data: TelemetryData) => void): void;
  onClose(listener: () => void): void;
  onError(listener: (error: Error) => void): void;
  send(data: string): void;
  close(): void;
}

export type SocketFactory = (url: string) => Socket;

export interface ObserverDependencies {
  readonly createSocket: SocketFactory;
  readonly delay: (milliseconds: number, signal: AbortSignal) => Promise<void>;
  readonly now: () => number;
}

export interface ObserverOptions {
  readonly debugSessionId: string;
  readonly debugSessionName: string;
  readonly sessionId: string;
  readonly managementEndpoint: BingoEndpoint;
}

export class TelemetryObserver {
  readonly #dependencies: ObserverDependencies;
  readonly #options: ObserverOptions;
  readonly #lifetime = new AbortController();
  readonly #sequence = new SequenceTracker();
  readonly #listeners = new Set<(model: SessionModel) => void>();
  #socket: Socket | undefined;
  #connectionEpoch = 0;
  #disposed = false;
  #model: SessionModel;

  public constructor(
    options: ObserverOptions,
    dependencies: ObserverDependencies = defaultDependencies,
  ) {
    this.#options = {
      ...options,
      managementEndpoint: normalizeManagementEndpoint(
        options.managementEndpoint,
      ),
    };
    this.#dependencies = dependencies;
    this.#model = {
      debugSessionId: options.debugSessionId,
      debugSessionName: options.debugSessionName,
      sessionId: options.sessionId,
      connection: "connecting",
      sessionState: "unknown",
      clients: 0,
      lastStop: "",
      error: "",
      seqGap: "",
      lastSeq: 0,
      snapshot: undefined,
      selectedGoroutine: 0,
      timeline: [],
    };
  }

  public get model(): SessionModel {
    return this.#model;
  }

  public onChange(listener: (model: SessionModel) => void): () => void {
    this.#listeners.add(listener);
    return () => {
      this.#listeners.delete(listener);
    };
  }

  public start(): void {
    if (this.#disposed || this.#socket !== undefined) {
      return;
    }
    this.#connect(0);
  }

  public refresh(): void {
    this.#sendSnapshot();
  }

  public selectGoroutine(id: number): void {
    if (
      this.#model.snapshot?.goroutines.some((goroutine) => goroutine.id === id) ===
      true
    ) {
      this.#update({ selectedGoroutine: id });
    }
  }

  public dispose(): void {
    if (this.#disposed) {
      return;
    }
    this.#disposed = true;
    this.#lifetime.abort();
    this.#connectionEpoch += 1;
    const socket = this.#socket;
    this.#socket = undefined;
    socket?.close();
    this.#update({ connection: "closed" });
    this.#listeners.clear();
  }

  #connect(attempt: number): void {
    if (this.#disposed) {
      return;
    }
    const epoch = ++this.#connectionEpoch;
    this.#sequence.reset();
    this.#update({
      connection: attempt === 0 ? "connecting" : "reconnecting",
      error: "",
      seqGap: "",
      lastSeq: 0,
    });

    let socket: Socket;
    try {
      socket = this.#dependencies.createSocket(
        telemetryURL(
          this.#options.managementEndpoint,
          this.#options.sessionId,
        ),
      );
    } catch (error: unknown) {
      this.#update({ error: errorMessage(error) });
      void this.#reconnect(attempt + 1, epoch);
      return;
    }
    this.#socket = socket;
    socket.onOpen(() => {
      if (!this.#isCurrent(epoch, socket)) {
        return;
      }
      this.#update({ connection: "connected", error: "" });
      this.#sendSnapshot();
    });
    socket.onMessage((data) => {
      if (this.#isCurrent(epoch, socket)) {
        this.#receive(data);
      }
    });
    socket.onError((error) => {
      if (!this.#isCurrent(epoch, socket)) {
        return;
      }
      this.#update({ error: error.message });
      socket.close();
    });
    socket.onClose(() => {
      if (!this.#isCurrent(epoch, socket)) {
        return;
      }
      this.#socket = undefined;
      void this.#reconnect(attempt + 1, epoch);
    });
  }

  async #reconnect(attempt: number, epoch: number): Promise<void> {
    if (this.#disposed) {
      return;
    }
    const wait = reconnectDelays[attempt - 1];
    if (wait === undefined) {
      const detail = this.#model.error;
      this.#update({
        connection: "error",
        error:
          detail.length === 0
            ? "telemetry reconnect limit reached"
            : `telemetry reconnect limit reached: ${detail}`,
      });
      return;
    }
    this.#update({ connection: "reconnecting" });
    try {
      await this.#dependencies.delay(wait, this.#lifetime.signal);
    } catch {
      return;
    }
    if (!this.#disposed && this.#connectionEpoch === epoch) {
      this.#connect(attempt);
    }
  }

  #sendSnapshot(): void {
    const socket = this.#socket;
    if (socket?.readyState !== socketOpenState) {
      return;
    }
    try {
      socket.send(snapshotCommand());
    } catch (error: unknown) {
      this.#update({ error: `cannot request snapshot: ${errorMessage(error)}` });
      socket.close();
    }
  }

  #receive(data: TelemetryData): void {
    try {
      const event = decodeEvent(data);
      const sequence = this.#sequence.observe(event.seq);
      if (!sequence.accept) {
        this.#update({ seqGap: sequence.gap ?? "" });
        return;
      }
      const sequencePatch = {
        lastSeq: Math.max(this.#model.lastSeq, event.seq),
        ...(sequence.gap === undefined ? {} : { seqGap: sequence.gap }),
      };
      if (event.snapshot !== undefined) {
        const selected =
          this.#model.selectedGoroutine !== 0 &&
          event.snapshot.goroutines.some(
            (goroutine) => goroutine.id === this.#model.selectedGoroutine,
          )
            ? this.#model.selectedGoroutine
            : event.snapshot.current || event.snapshot.goroutines[0]?.id || 0;
        this.#update({
          ...sequencePatch,
          error: "",
          snapshot: event.snapshot,
          selectedGoroutine: selected,
          timeline: appendLifecycle(
            this.#model.timeline,
            event.snapshot,
            this.#dependencies.now(),
          ),
        });
        return;
      }
      this.#update(sequencePatch);
      this.#applyEvent(event.kind, event.payload);
    } catch (error: unknown) {
      this.#update({ error: errorMessage(error) });
      this.#socket?.close();
    }
  }

  #applyEvent(kind: string, payload: Record<string, unknown>): void {
    switch (kind) {
      case "SessionState":
        this.#update({
          sessionState: String(payload.state),
          clients: Number(payload.clients),
        });
        break;
      case "BreakpointHit":
        this.#update({ lastStop: describeStop("Breakpoint", payload) });
        break;
      case "Paused":
        this.#update({ lastStop: describeStop("Paused", payload) });
        break;
      case "Stepped":
        this.#update({ lastStop: describeStop("Stepped", payload) });
        break;
      case "Panic":
        this.#update({ lastStop: `Panic: ${payloadText(payload.message, "")}` });
        break;
      case "Continued":
        this.#update({ sessionState: "running", lastStop: "Continued" });
        break;
      case "ProcessExited":
        this.#update({
          sessionState: "exited",
          lastStop: `Exited (${payloadText(payload.exitCode, "?")})`,
        });
        break;
      case "Error":
        this.#update({ error: payloadText(payload.message, "debugger error") });
        break;
    }
  }

  #isCurrent(epoch: number, socket: Socket): boolean {
    return (
      !this.#disposed &&
      this.#connectionEpoch === epoch &&
      this.#socket === socket
    );
  }

  #update(patch: Partial<SessionModel>): void {
    this.#model = { ...this.#model, ...patch };
    for (const listener of this.#listeners) {
      listener(this.#model);
    }
  }
}

export function normalizeManagementEndpoint(
  endpoint: BingoEndpoint,
): BingoEndpoint {
  let host = endpoint.host.trim();
  if (host.startsWith("[") && host.endsWith("]")) {
    host = host.slice(1, -1);
  }
  host = host.toLocaleLowerCase();
  if (
    host.length === 0 ||
    /[\s/?#@]/u.test(host) ||
    !Number.isInteger(endpoint.port) ||
    endpoint.port < 1 ||
    endpoint.port > 65535
  ) {
    throw new Error("invalid bingo management endpoint");
  }
  return { host, port: endpoint.port };
}

export function telemetryURL(endpoint: BingoEndpoint, sessionId: string): string {
  const normalized = normalizeManagementEndpoint(endpoint);
  const host = normalized.host.includes(":")
    ? `[${normalized.host}]`
    : normalized.host;
  return `ws://${host}:${String(normalized.port)}/ws?session=${encodeURIComponent(sessionId)}`;
}

function describeStop(
  label: string,
  payload: Record<string, unknown>,
): string {
  const location =
    payload.location ??
    (typeof payload.breakpoint === "object" &&
    payload.breakpoint !== null &&
    "location" in payload.breakpoint
      ? (payload.breakpoint as { readonly location?: unknown }).location
      : undefined);
  if (typeof location !== "object" || location === null) {
    return label;
  }
  const item = location as Record<string, unknown>;
  const file = payloadText(item.file, "");
  const line = Number(item.line ?? 0);
  return file.length === 0 ? label : `${label} at ${file}:${String(line)}`;
}

function delay(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(new Error("cancelled"));
      return;
    }
    const finish = (): void => {
      signal.removeEventListener("abort", abort);
      resolve();
    };
    const timeout = setTimeout(finish, milliseconds);
    const abort = (): void => {
      clearTimeout(timeout);
      signal.removeEventListener("abort", abort);
      reject(new Error("cancelled"));
    };
    signal.addEventListener("abort", abort, { once: true });
  });
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

const defaultDependencies: ObserverDependencies = {
  createSocket: (url) => new NodeSocket(url),
  delay,
  now: Date.now,
};

function payloadText(value: unknown, fallback: string): string {
  return typeof value === "string" || typeof value === "number"
    ? String(value)
    : fallback;
}

class NodeSocket implements Socket {
  readonly #socket: WebSocket;

  public constructor(url: string) {
    this.#socket = new WebSocket(url, {
      handshakeTimeout: 5000,
      maxPayload: maximumEnvelopeBytes,
      perMessageDeflate: false,
    });
  }

  public get readyState(): number {
    return this.#socket.readyState;
  }

  public onOpen(listener: () => void): void {
    this.#socket.on("open", listener);
  }

  public onMessage(listener: (data: TelemetryData) => void): void {
    this.#socket.on("message", (data) => {
      listener(data);
    });
  }

  public onClose(listener: () => void): void {
    this.#socket.on("close", listener);
  }

  public onError(listener: (error: Error) => void): void {
    this.#socket.on("error", listener);
  }

  public send(data: string): void {
    this.#socket.send(data);
  }

  public close(): void {
    this.#socket.close();
  }
}
