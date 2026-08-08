import type { BingoEndpoint } from "./configuration.js";
import {
  type ConcurrencyViewModel,
  serializeSnapshot,
  type SessionModel,
  toSessionViewModel,
} from "./model.js";
import {
  TelemetryObserver,
  type ObserverDependencies,
} from "./observer.js";

export interface SessionRegistration {
  readonly debugSessionId: string;
  readonly debugSessionName: string;
  readonly sessionId: string;
  readonly managementEndpoint: BingoEndpoint;
}

export class SessionRegistry {
  readonly #sessions = new Map<
    string,
    { readonly observer: TelemetryObserver; readonly unsubscribe: () => void }
  >();
  readonly #listeners = new Set<(model: ConcurrencyViewModel) => void>();
  readonly #dependencies: ObserverDependencies | undefined;
  #activeDebugSessionId = "";
  #revision = 0;

  public constructor(dependencies?: ObserverDependencies) {
    this.#dependencies = dependencies;
  }

  public get viewModel(): ConcurrencyViewModel {
    const sessions = [...this.#sessions.values()]
      .map(({ observer }) => toSessionViewModel(observer.model))
      .sort((left, right) =>
        left.debugSessionName.localeCompare(right.debugSessionName) ||
        left.debugSessionId.localeCompare(right.debugSessionId),
      );
    return {
      revision: this.#revision,
      activeDebugSessionId: this.#activeDebugSessionId,
      sessions,
    };
  }

  public onChange(listener: (model: ConcurrencyViewModel) => void): () => void {
    this.#listeners.add(listener);
    return () => {
      this.#listeners.delete(listener);
    };
  }

  public add(registration: SessionRegistration): boolean {
    if (this.#sessions.has(registration.debugSessionId)) {
      return false;
    }
    const observer =
      this.#dependencies === undefined
        ? new TelemetryObserver(registration)
        : new TelemetryObserver(registration, this.#dependencies);
    const unsubscribe = observer.onChange(() => {
      this.#changed();
    });
    this.#sessions.set(registration.debugSessionId, { observer, unsubscribe });
    this.#activeDebugSessionId = registration.debugSessionId;
    observer.start();
    this.#changed();
    return true;
  }

  public remove(debugSessionId: string): void {
    const entry = this.#sessions.get(debugSessionId);
    if (entry === undefined) {
      return;
    }
    entry.unsubscribe();
    entry.observer.dispose();
    this.#sessions.delete(debugSessionId);
    if (this.#activeDebugSessionId === debugSessionId) {
      this.#activeDebugSessionId = this.viewModel.sessions[0]?.debugSessionId ?? "";
    }
    this.#changed();
  }

  public select(debugSessionId: string): boolean {
    if (!this.#sessions.has(debugSessionId)) {
      return false;
    }
    this.#activeDebugSessionId = debugSessionId;
    this.#changed();
    return true;
  }

  public selectGoroutine(id: number): void {
    this.#active()?.observer.selectGoroutine(id);
  }

  public refresh(): void {
    this.#active()?.observer.refresh();
  }

  public activeSnapshotJSON(): string | undefined {
    const model = this.#active()?.observer.model;
    return model === undefined ? undefined : serializeSnapshot(model);
  }

  public activeModel(): SessionModel | undefined {
    return this.#active()?.observer.model;
  }

  public dispose(): void {
    for (const { observer, unsubscribe } of this.#sessions.values()) {
      unsubscribe();
      observer.dispose();
    }
    this.#sessions.clear();
    this.#changed();
  }

  #active():
    | { readonly observer: TelemetryObserver; readonly unsubscribe: () => void }
    | undefined {
    return this.#sessions.get(this.#activeDebugSessionId);
  }

  #changed(): void {
    this.#revision += 1;
    const model = this.viewModel;
    for (const listener of this.#listeners) {
      listener(model);
    }
  }
}
