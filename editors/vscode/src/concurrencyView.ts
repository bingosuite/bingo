import { randomBytes } from "node:crypto";

import * as vscode from "vscode";

import { WebviewDeliveryState } from "./documentGeneration.js";
import type { ConcurrencyViewModel, DebugStackFrame } from "./model.js";
import { decodeAction, decodeWebviewMessage, type WebviewMessage } from "./messages.js";
import { validSourcePath } from "./source.js";
import type { Goroutine, Location } from "./telemetry.js";
import type { DisplayedState, TestOperation } from "./webviewTest.js";
import type { SessionRegistry } from "./registry.js";

export const concurrencyViewId = "bingo.concurrency";

export interface ConcurrencyViewActions {
  selectFrame(frameId: number): void;
  expandVariable(reference: number): void;
  refreshInspection(): void;
  refreshSource?(): void;
  openSource(path: string, line: number, column: number, isCurrent: () => boolean): void;
}

function sourceLocation(
  target: Extract<WebviewMessage, { type: "openSource" }>["target"],
  goroutine: Goroutine | undefined,
  frame: DebugStackFrame | undefined,
): Location | DebugStackFrame | undefined {
  switch (target) {
    case "frame":
      return frame;
    case "created":
      return goroutine?.createdLoc;
    case "start":
      return goroutine?.startLoc;
    case "current":
      return goroutine?.currentLoc;
  }
}

export class ConcurrencyViewProvider implements vscode.WebviewViewProvider {
  #view: vscode.WebviewView | vscode.WebviewPanel | undefined;
  #disposed = false;
  #subscriptions: vscode.Disposable[] = [];
  #nextProbe = 0;
  readonly #probes = new Map<number, {
    readonly generation: number;
    readonly resolve: (state: DisplayedState) => void;
    readonly reject: (error: Error) => void;
  }>();
  #fitPending = false;
  readonly #delivery = new WebviewDeliveryState();
  #model: ConcurrencyViewModel;
  readonly #unsubscribe: () => void;

  public constructor(
    private readonly extensionUri: vscode.Uri,
    private readonly registry: SessionRegistry,
    private readonly actions: ConcurrencyViewActions,
    private readonly testing = false,
  ) {
    this.#model = registry.viewModel;
    this.#unsubscribe = registry.onChange((model) => {
      this.#model = model;
      this.#render();
    });
  }

  public resolveWebviewView(view: vscode.WebviewView): void {
    this.#attach(view, (listener) => view.onDidChangeVisibility(listener));
  }

  public resolvePanel(panel: vscode.WebviewPanel): void {
    this.#attach(panel, (listener) => panel.onDidChangeViewState(listener));
  }

  #attach(
    view: vscode.WebviewView | vscode.WebviewPanel,
    visibility: (listener: () => void) => vscode.Disposable,
  ): void {
    if (this.#disposed) {
      return;
    }
    this.#subscriptions.forEach((subscription) => { subscription.dispose(); });
    this.#rejectProbes();
    this.#delivery.beginDocument();
    this.#view = view;
    const dist = vscode.Uri.joinPath(this.extensionUri, "dist");
    view.webview.options = {
      enableScripts: true,
      localResourceRoots: [dist],
    };
    view.webview.html = this.#html(view.webview);
    const receive = view.webview.onDidReceiveMessage((message: unknown) => {
      if (this.#view === view && view.visible) {
        this.#receive(message);
      }
    });
    const visible = visibility(() => {
      if (this.#view === view && !view.visible) {
        this.#rejectProbes();
        this.#delivery.markHidden();
      }
    });
    const disposed = view.onDidDispose(() => {
      receive.dispose();
      visible.dispose();
      disposed.dispose();
      if (this.#view === view) {
        this.#rejectProbes();
        this.#delivery.beginDocument();
        this.#view = undefined;
        this.#subscriptions = [];
      }
    });
    this.#subscriptions = [receive, visible, disposed];
  }

  public fit(): void {
    this.#fitPending = true;
    this.#sendFit();
  }

  public async testUI(operation: TestOperation = "inspect", target = 0): Promise<DisplayedState> {
    if (!this.testing || !this.#delivery.ready || this.#view === undefined || this.#disposed) {
      throw new Error("Bingo test document is not ready.");
    }
    const id = ++this.#nextProbe;
    const view = this.#view;
    return new Promise<DisplayedState>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.#probes.delete(id);
        reject(new Error("Bingo document inspection timed out."));
      }, 5000);
      this.#probes.set(id, {
        generation: this.#delivery.captureGeneration(),
        resolve: (state) => { clearTimeout(timer); this.#probes.delete(id); resolve(state); },
        reject: (error) => { clearTimeout(timer); this.#probes.delete(id); reject(error); },
      });
      void view.webview.postMessage({ type: "testInspect", id, operation, target }).then(
        (sent) => { if (!sent) { this.#probes.get(id)?.reject(new Error("Bingo document is hidden.")); } },
        () => this.#probes.get(id)?.reject(new Error("Bingo document delivery failed.")),
      );
    });
  }

  public get lastRenderedRevision(): number {
    return this.#delivery.lastRenderedRevision;
  }

  public get status(): {
    readonly resolved: boolean;
    readonly ready: boolean;
    readonly visible: boolean;
  } {
    return {
      resolved: this.#view !== undefined,
      ready: this.#delivery.ready,
      visible: this.#view?.visible ?? false,
    };
  }

  public dispose(): void {
    this.#disposed = true;
    this.#delivery.beginDocument();
    this.#view = undefined;
    this.#subscriptions.forEach((subscription) => { subscription.dispose(); });
    this.#subscriptions = [];
    this.#rejectProbes();
    this.#unsubscribe();
  }

  #rejectProbes(): void {
    for (const probe of this.#probes.values()) {
      probe.reject(new Error("Bingo document changed or disposed."));
    }
  }

  #receive(value: unknown): void {
    if (this.#receiveProbe(value)) {
      return;
    }
    const message = this.#currentMessage(value);
    if (message === undefined) {
      return;
    }
    switch (message.type) {
      case "ready":
        this.#delivery.markReady();
        this.#render();
        this.#sendFit();
        break;
      case "rendered":
        if (
          this.#delivery.acknowledge({
            generation: message.generation,
            revision: message.revision,
          })
        ) {
          this.#render();
        }
        break;
      case "refresh":
        this.registry.refresh();
        this.actions.refreshSource?.();
        break;
      case "selectSession":
        this.registry.select(message.id);
        break;
      case "selectGoroutine":
        this.registry.selectGoroutine(message.id);
        break;
      case "selectFrame":
        this.actions.selectFrame(message.id);
        break;
      case "expandVariable":
        this.actions.expandVariable(message.reference);
        break;
      case "refreshInspection":
        this.actions.refreshInspection();
        break;
      case "openSource":
        this.#openSource(message);
        break;
      case "fit":
        this.fit();
        break;
      case "copySnapshot":
        void copySnapshot(this.registry);
        break;
    }
  }

  #receiveProbe(value: unknown): boolean {
    if (this.testing && typeof value === "object" && value !== null &&
      "type" in value && value.type === "testResult" && "id" in value &&
      typeof value.id === "number" && "state" in value) {
      const probe = this.#probes.get(value.id);
      if (probe !== undefined && this.#delivery.isCurrent(probe.generation)) {
        // Diagnostic data never drives source access, model mutations or commands.
        probe.resolve(value.state as DisplayedState);
      }
      return true;
    }
    return false;
  }

  #currentMessage(value: unknown): WebviewMessage | undefined {
    try {
      if (typeof value === "object" && value !== null && "type" in value &&
        (value.type === "ready" || value.type === "rendered")) {
        return decodeWebviewMessage(value);
      }
      const { context, action } = decodeAction(value);
      const active = this.registry.activeModel();
      if (!this.#delivery.isCurrent(context.generation) ||
        context.revision !== this.#model.revision ||
        context.debugSessionId !== (active?.debugSessionId ?? "") ||
        context.goroutineId !== (active?.selectedGoroutine ?? 0)) {
        return undefined;
      }
      return action;
    } catch {
      return undefined;
    }
  }

  #openSource(message: Extract<WebviewMessage, { type: "openSource" }>): void {
    const model = this.registry.activeModel();
    const inspection = model === undefined ? undefined : this.registry.inspectionFor(model.debugSessionId);
    const generation = this.#delivery.captureGeneration();
    const isCurrent = (): boolean => {
      const active = this.registry.activeModel();
      return !this.#disposed && this.#view !== undefined &&
        this.#delivery.isCurrent(generation) && model !== undefined &&
        active?.debugSessionId === model.debugSessionId &&
        active.snapshot === model.snapshot &&
        active.selectedGoroutine === model.selectedGoroutine &&
        this.registry.inspectionFor(model.debugSessionId) === inspection;
    };
    const goroutine = model?.snapshot?.goroutines.find((g) => g.id === model.selectedGoroutine);
    const frame = inspection?.frames.find((f) => f.id === message.frameId);
    const location = sourceLocation(message.target, goroutine, frame);
    if (location !== undefined && validSourcePath(location.file) && location.line > 0) {
      this.actions.openSource(location.file, location.line, "column" in location ? location.column : 0, isCurrent);
    } else {
      void vscode.window.showInformationMessage("No valid local source location is available.");
    }
  }

  #render(): void {
    if (this.#view === undefined) {
      return;
    }
    const delivery = this.#delivery.beginDelivery(this.#model.revision);
    if (delivery === undefined) {
      return;
    }
    const active = this.#model.sessions.find(
      (session) =>
        session.debugSessionId === this.#model.activeDebugSessionId,
    );
    if ("badge" in this.#view) {
      this.#view.badge =
      active?.snapshot === undefined
        ? undefined
        : {
            value: active.snapshot.goroutines.length,
            tooltip: `${String(active.snapshot.goroutines.length)} goroutines · ${String(active.snapshot.threads.length)} threads`,
          };
    }
    const revision = this.#model.revision;
    const view = this.#view;
    void view.webview
      .postMessage({
        type: "render",
        generation: delivery.generation,
        revision,
        model: this.#model,
      })
      .then(
        (delivered) => {
          if (!delivered && this.#view === view) {
            this.#delivery.rejectDelivery(delivery);
          }

        },
        () => {
          if (this.#view === view) {
            this.#delivery.rejectDelivery(delivery);
          }
        },
      );
  }

  #sendFit(): void {
    if (!this.#fitPending || !this.#delivery.ready || this.#view === undefined) {
      return;
    }
    const view = this.#view;
    const generation = this.#delivery.captureGeneration();
    void view.webview.postMessage({ type: "fit" }).then((delivered) => {
      if (
        delivered &&
        this.#view === view &&
        this.#delivery.isCurrent(generation)
      ) {
        this.#fitPending = false;
      }
    }, () => undefined);
  }

  #html(webview: vscode.Webview): string {
    const nonce = randomBytes(18).toString("base64");
    const script = webview.asWebviewUri(
      vscode.Uri.joinPath(this.extensionUri, "dist", "webview.js"),
    );
    return `<!doctype html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
  <style nonce="${nonce}">${styles}</style>
  <title>Bingo Concurrency</title>
</head>
<body>
  <div id="app"></div>
  <script nonce="${nonce}" src="${script.toString()}"></script>
</body>
</html>`;
  }
}

// Each surface owns its delivery generation, but both render the same registry.
// Closing the editor never tears down a session or the Activity Bar observer.
export class ConcurrencyEditor {
  #panel: vscode.WebviewPanel | undefined;
  #disposed = false;
  readonly #announced = new Set<string>();
  #sourceColumn: vscode.ViewColumn = vscode.ViewColumn.One;

  public constructor(private readonly provider: ConcurrencyViewProvider) {}

  public get sourceColumn(): vscode.ViewColumn {
    const visible = vscode.window.visibleTextEditors.find((editor) =>
      editor.viewColumn !== this.#panel?.viewColumn)?.viewColumn;
    if (visible !== undefined) {
      return visible;
    }
    if (this.#sourceColumn !== this.#panel?.viewColumn) {
      return this.#sourceColumn;
    }
    return this.#panel.viewColumn === vscode.ViewColumn.One ? vscode.ViewColumn.Two : vscode.ViewColumn.One;
  }

  public sessionStarted(id: string, autoReveal: boolean): void {
    if (this.#disposed || this.#announced.has(id)) {
      return;
    }
    this.#announced.add(id);
    if (autoReveal) {
      this.open(true);
    }
  }

  public sessionEnded(id: string): void {
    this.#announced.delete(id);
  }

  public open(preserveFocus = false): void {
    if (this.#disposed) {
      return;
    }
    if (this.#panel !== undefined) {
      this.#panel.reveal(this.#panel.viewColumn, preserveFocus);
      return;
    }
    this.#sourceColumn = vscode.window.activeTextEditor?.viewColumn ?? vscode.ViewColumn.One;
    const panel = vscode.window.createWebviewPanel(
      "bingo.concurrency.editor",
      "Bingo Concurrency",
      { viewColumn: vscode.ViewColumn.Beside, preserveFocus },
      { retainContextWhenHidden: false },
    );
    this.#panel = panel;
    this.provider.resolvePanel(panel);
    panel.onDidDispose(() => {
      if (this.#panel === panel) {
        this.#panel = undefined;
      }
    });
  }

  public dispose(): void {
    this.#disposed = true;
    this.#announced.clear();
    this.#panel?.dispose();
    this.#panel = undefined;
    this.provider.dispose();
  }
}

export async function copySnapshot(registry: SessionRegistry): Promise<void> {
  const snapshot = registry.activeSnapshotJSON();
  if (snapshot === undefined) {
    void vscode.window.showInformationMessage(
      "Bingo Concurrency has no active snapshot to copy.",
    );
    return;
  }
  await vscode.env.clipboard.writeText(snapshot);
  void vscode.window.setStatusBarMessage("Bingo concurrency snapshot copied", 2000);
}

export const styles = `
:root { color-scheme: light dark; font-family: var(--vscode-font-family); color: var(--vscode-foreground); }
* { box-sizing: border-box; }
body { padding: 0; margin: 0; background: var(--vscode-sideBar-background); }
button, input, select { font: inherit; color: inherit; }
button, select, input { border: 1px solid var(--vscode-input-border, transparent); background: var(--vscode-input-background); border-radius: 5px; }
button { cursor: pointer; padding: 5px 9px; }
button:hover { background: var(--vscode-toolbar-hoverBackground); }
button:focus-visible, input:focus-visible, select:focus-visible, .graph-viewport:focus-visible { outline: 2px solid var(--vscode-focusBorder); outline-offset: 1px; }
.app { min-height: 100vh; }
.topbar { position: sticky; top: 0; z-index: 5; display: flex; align-items: center; gap: 12px; justify-content: space-between; padding: 12px; background: var(--vscode-sideBar-background); border-bottom: 1px solid var(--vscode-sideBarSectionHeader-border, transparent); }
.brand { display: grid; gap: 2px; }
.brand strong { font-size: 14px; }
.brand span, .muted { color: var(--vscode-descriptionForeground); font-size: 11px; }
.session-selector { min-width: 120px; max-width: 48%; padding: 4px; }
.session { padding: 10px; display: grid; gap: 10px; }
.cards { display: grid; grid-template-columns: repeat(4, minmax(58px, 1fr)); gap: 7px; }
.card { display: grid; padding: 9px; border: 1px solid var(--vscode-widget-border); border-radius: 8px; background: var(--vscode-editorWidget-background); }
.card strong { font-size: 18px; color: var(--vscode-charts-blue); }
.card span { color: var(--vscode-descriptionForeground); font-size: 10px; text-transform: uppercase; }
.last-stop { grid-column: 1 / -1; padding: 7px 9px; border-left: 3px solid var(--vscode-debugIcon-breakpointCurrentStackframeForeground); background: var(--vscode-textBlockQuote-background); overflow-wrap: anywhere; }
.toolbar { display: flex; gap: 6px; }
.toolbar input { flex: 1; min-width: 80px; padding: 6px 8px; }
.workspace { display: grid; grid-template-columns: minmax(0, 2fr) minmax(270px, 1.2fr); gap: 10px; }
.graph-panel, .inspector, .threads, .timeline { position: relative; min-width: 0; border: 1px solid var(--vscode-widget-border); border-radius: 9px; background: var(--vscode-editorWidget-background); overflow: hidden; }
.graph-controls { position: absolute; right: 7px; top: 7px; z-index: 2; display: flex; gap: 4px; }
.graph-viewport { height: 410px; overflow: hidden; touch-action: none; }
.graph-viewport svg { width: 100%; height: 100%; }
.tree-edge { fill: none; stroke: var(--vscode-editorIndentGuide-background); stroke-width: 1.5; }
.tree-node { cursor: pointer; }
.tree-node rect { fill: var(--vscode-editorWidget-background); stroke: var(--vscode-widget-border); stroke-width: 1.5; }
.tree-node:hover rect, .tree-node.selected rect { stroke: var(--vscode-focusBorder); stroke-width: 2.5; }
.tree-node.current rect { fill: var(--vscode-list-activeSelectionBackground); stroke: var(--vscode-debugIcon-breakpointCurrentStackframeForeground); }
.tree-node.selected rect { fill: var(--vscode-list-inactiveSelectionBackground); filter: drop-shadow(0 0 3px var(--vscode-focusBorder)); }
.tree-node.current.selected rect { fill: var(--vscode-list-activeSelectionBackground); stroke: var(--vscode-focusBorder); stroke-width: 3; }
.tree-node text { fill: var(--vscode-foreground); pointer-events: none; }
.node-id { font-size: 14px; font-weight: 700; }
.node-status, .node-thread { font-size: 10px; fill: var(--vscode-descriptionForeground) !important; }
.omitted { position: absolute; bottom: 2px; left: 8px; color: var(--vscode-descriptionForeground); font-size: 10px; }
.server-omitted { position: absolute; bottom: 14px; left: 8px; color: var(--vscode-editorWarning-foreground); font-size: 10px; }
.threads .server-omitted { position: static; margin: 4px 0 0; }
.inspector, .threads, .timeline { padding: 11px; }
h2 { margin: 0 0 10px; font-size: 12px; text-transform: uppercase; color: var(--vscode-descriptionForeground); }
.inspector-title { display: block; margin-bottom: 10px; font-size: 15px; }
dl { margin: 0; display: grid; grid-template-columns: 58px 1fr; gap: 6px; }
dt { color: var(--vscode-descriptionForeground); }
dd { margin: 0; overflow-wrap: anywhere; }
.source-link { min-width: 0; padding: 0; border: 0; color: var(--vscode-textLink-foreground); background: transparent; text-align: left; overflow-wrap: anywhere; }
.source-link:hover { color: var(--vscode-textLink-activeForeground); background: transparent; text-decoration: underline; }
.debug-inspection { display: grid; gap: 7px; margin-top: 14px; padding-top: 12px; border-top: 1px solid var(--vscode-widget-border); }
.spawn-source { display: grid; gap: 6px; margin-top: 14px; }
.spawn-source h2 { margin: 0; }
.source-snippet { margin: 0; max-width: 100%; overflow: auto; font: 11px var(--vscode-editor-font-family); background: var(--vscode-editor-background); }
.source-line { display: block; white-space: pre; }
.creation-line { background: var(--vscode-editor-lineHighlightBackground); border-left: 3px solid var(--vscode-debugIcon-breakpointCurrentStackframeForeground); }
.inspection-heading { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.inspection-heading h2, .locals-heading { margin: 0; }
.subtle-button { padding: 2px 6px; color: var(--vscode-descriptionForeground); font-size: 10px; }
.inspection-state { margin: 0; padding: 8px; border-radius: 5px; color: var(--vscode-descriptionForeground); background: var(--vscode-textBlockQuote-background); overflow-wrap: anywhere; }
.inspection-state.error { color: var(--vscode-errorForeground); border-left: 2px solid var(--vscode-errorForeground); }
.inspection-state.unavailable { border-left: 2px solid var(--vscode-editorWarning-foreground); }
.inspection-state.loading { color: var(--vscode-progressBar-background); }
.inspect-current { justify-self: start; }
.stack-list { display: grid; gap: 4px; max-height: 240px; padding: 0; margin: 0; overflow: auto; list-style: none; }
.stack-frame { display: grid; gap: 2px; min-width: 0; padding: 6px 7px; border-left: 2px solid transparent; border-radius: 4px; background: var(--vscode-list-inactiveSelectionBackground); }
.stack-frame.selected { border-left-color: var(--vscode-debugIcon-breakpointCurrentStackframeForeground); background: var(--vscode-list-activeSelectionBackground); color: var(--vscode-list-activeSelectionForeground); }
.frame-name { min-width: 0; padding: 0; border: 0; background: transparent; text-align: left; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.frame-name:hover { background: transparent; text-decoration: underline; }
.frame-source { width: fit-content; max-width: 100%; font-size: 10px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.variable-tree, .variable-tree ul { display: grid; gap: 2px; padding: 0; margin: 0; list-style: none; }
.variable-tree ul { margin-left: 13px; padding-left: 5px; border-left: 1px solid var(--vscode-tree-inactiveIndentGuidesStroke, var(--vscode-editorIndentGuide-background)); }
.variable-row { display: grid; grid-template-columns: 18px minmax(55px, auto) minmax(0, 1fr); align-items: baseline; column-gap: 4px; min-width: 0; padding: 2px 0; }
.variable-expand { width: 18px; padding: 0; border: 0; background: transparent; }
.variable-expand:hover { background: var(--vscode-toolbar-hoverBackground); }
.variable-spacer { width: 18px; }
.variable-name { color: var(--vscode-symbolIcon-variableForeground, var(--vscode-foreground)); overflow-wrap: anywhere; }
.variable-value { min-width: 0; overflow: hidden; color: var(--vscode-debugTokenExpression-value); font-family: var(--vscode-editor-font-family); text-overflow: ellipsis; white-space: nowrap; }
.variable-type { grid-column: 3; color: var(--vscode-descriptionForeground); font-size: 10px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.thread-list { display: grid; grid-template-columns: repeat(auto-fill, minmax(105px, 1fr)); gap: 6px; }
.thread { display: grid; padding: 7px; border: 1px solid var(--vscode-widget-border); border-radius: 6px; }
.thread.current { border-color: var(--vscode-focusBorder); }
.thread span { color: var(--vscode-descriptionForeground); font-size: 10px; }
.timeline ol { display: flex; gap: 5px; padding: 0; margin: 0; list-style: none; overflow-x: auto; }
.timeline li { white-space: nowrap; padding: 4px 6px; border-radius: 999px; background: var(--vscode-badge-background); color: var(--vscode-badge-foreground); }
.timeline li.created { border-left: 3px solid var(--vscode-testing-iconPassed); }
.timeline li.exited { border-left: 3px solid var(--vscode-testing-iconFailed); }
.empty-state { min-height: 130px; display: grid; place-content: center; gap: 6px; padding: 18px; text-align: center; color: var(--vscode-descriptionForeground); }
.empty-state strong { color: var(--vscode-foreground); }
.callout { display: grid; gap: 3px; padding: 8px; border-radius: 6px; }
.callout.error { border-left: 3px solid var(--vscode-errorForeground); background: var(--vscode-inputValidation-errorBackground); }
.callout.warning { border-left: 3px solid var(--vscode-editorWarning-foreground); background: var(--vscode-inputValidation-warningBackground); }
@media (max-width: 520px) { .workspace { grid-template-columns: 1fr; } .inspector { order: -1; } .graph-viewport { height: 330px; } .cards { grid-template-columns: repeat(2, 1fr); } }
@media (forced-colors: active) { .tree-node rect, .graph-panel, .inspector, .threads, .timeline, .card { border: 1px solid CanvasText; } }
`;
