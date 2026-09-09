import type { BingoEndpoint } from "./configuration.js";
import {
  type ConcurrencyViewModel,
  type DebugInspection,
  emptyInspection,
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
  readonly #inspections = new Map<string, DebugInspection>();
  readonly #dependencies: ObserverDependencies | undefined;
  #activeDebugSessionId = "";
  #revision = 0;
  #notifying = false;
  #notificationPending = false;

  public constructor(dependencies?: ObserverDependencies) {
    this.#dependencies = dependencies;
  }

  public get viewModel(): ConcurrencyViewModel {
    const sessions = [...this.#sessions.entries()]
      .map(([debugSessionId, { observer }]) =>
        toSessionViewModel(
          observer.model,
          this.#inspections.get(debugSessionId) ??
            emptyInspection(observer.model.selectedGoroutine),
        ),
      )
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
    this.#inspections.set(registration.debugSessionId, emptyInspection());
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
    this.#inspections.delete(debugSessionId);
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

  public inspectionFor(debugSessionId: string): DebugInspection | undefined {
    return this.#inspections.get(debugSessionId);
  }

  public updateInspection(
    debugSessionId: string,
    inspection: DebugInspection,
  ): boolean {
    if (!this.#sessions.has(debugSessionId)) {
      return false;
    }
    this.#inspections.set(debugSessionId, inspection);
    this.#changed();
    return true;
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
    this.#inspections.clear();
    this.#changed();
  }

  #active():
    | { readonly observer: TelemetryObserver; readonly unsubscribe: () => void }
    | undefined {
    return this.#sessions.get(this.#activeDebugSessionId);
  }

  #changed(): void {
    this.#revision += 1;
    if (this.#notifying) {
      this.#notificationPending = true;
      return;
    }
    this.#notifying = true;
    try {
      do {
        this.#notificationPending = false;
        const model = this.viewModel;
        for (const listener of this.#listeners) {
          listener(model);
          if (this.#notificationPending) {
            break;
          }
        }
      } while (this.#notificationPending);
    } finally {
      this.#notifying = false;
    }
  }
}
