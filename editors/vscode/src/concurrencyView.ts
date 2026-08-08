import { randomBytes } from "node:crypto";

import * as vscode from "vscode";

import type { ConcurrencyViewModel } from "./model.js";
import { decodeWebviewMessage } from "./messages.js";
import type { SessionRegistry } from "./registry.js";

export const concurrencyViewId = "bingo.concurrency";

export class ConcurrencyViewProvider implements vscode.WebviewViewProvider {
  #view: vscode.WebviewView | undefined;
  #ready = false;
  #lastRenderedRevision = -1;
  #inFlightRevision = -1;
  #fitPending = false;
  #model: ConcurrencyViewModel;
  readonly #unsubscribe: () => void;

  public constructor(
    private readonly extensionUri: vscode.Uri,
    private readonly registry: SessionRegistry,
  ) {
    this.#model = registry.viewModel;
    this.#unsubscribe = registry.onChange((model) => {
      this.#model = model;
      this.#render();
    });
  }

  public resolveWebviewView(view: vscode.WebviewView): void {
    this.#view = view;
    this.#ready = false;
    this.#lastRenderedRevision = -1;
    this.#inFlightRevision = -1;
    const dist = vscode.Uri.joinPath(this.extensionUri, "dist");
    view.webview.options = {
      enableScripts: true,
      localResourceRoots: [dist],
    };
    view.webview.html = this.#html(view.webview);
    view.webview.onDidReceiveMessage((message: unknown) => {
      this.#receive(message);
    });
    const visibility = view.onDidChangeVisibility(() => {
      if (this.#view === view && !view.visible) {
        this.#ready = false;
        this.#inFlightRevision = -1;
      }
    });
    view.onDidDispose(() => {
      visibility.dispose();
      if (this.#view === view) {
        this.#view = undefined;
        this.#ready = false;
        this.#inFlightRevision = -1;
      }
    });
  }

  public fit(): void {
    this.#fitPending = true;
    this.#sendFit();
  }

  public get lastRenderedRevision(): number {
    return this.#lastRenderedRevision;
  }

  public get status(): {
    readonly resolved: boolean;
    readonly ready: boolean;
  } {
    return {
      resolved: this.#view !== undefined,
      ready: this.#ready,
    };
  }

  public dispose(): void {
    this.#unsubscribe();
  }

  #receive(value: unknown): void {
    let message;
    try {
      message = decodeWebviewMessage(value);
    } catch {
      return;
    }
    switch (message.type) {
      case "ready":
        this.#ready = true;
        this.#lastRenderedRevision = -1;
        this.#inFlightRevision = -1;
        this.#render();
        this.#sendFit();
        break;
      case "rendered":
        if (
          message.revision === this.#inFlightRevision
        ) {
          this.#lastRenderedRevision = message.revision;
          this.#inFlightRevision = -1;
          this.#render();
        }
        break;
      case "refresh":
        this.registry.refresh();
        break;
      case "selectSession":
        this.registry.select(message.id);
        break;
      case "selectGoroutine":
        this.registry.selectGoroutine(message.id);
        break;
      case "copySnapshot":
        void copySnapshot(this.registry);
        break;
    }
  }

  #render(): void {
    if (
      !this.#ready ||
      this.#view === undefined ||
      this.#inFlightRevision >= 0 ||
      this.#lastRenderedRevision === this.#model.revision
    ) {
      return;
    }
    this.#inFlightRevision = this.#model.revision;
    const active = this.#model.sessions.find(
      (session) =>
        session.debugSessionId === this.#model.activeDebugSessionId,
    );
    this.#view.badge =
      active?.snapshot === undefined
        ? undefined
        : {
            value: active.snapshot.goroutines.length,
            tooltip: `${String(active.snapshot.goroutines.length)} goroutines · ${String(active.snapshot.threads.length)} threads`,
          };
    const revision = this.#model.revision;
    void this.#view.webview
      .postMessage({
        type: "render",
        revision,
        model: this.#model,
      })
      .then((delivered) => {
        if (!delivered && this.#inFlightRevision === revision) {
          this.#inFlightRevision = -1;
          this.#ready = false;
        }
      }, () => {
        this.#inFlightRevision = -1;
        this.#ready = false;
      });
  }

  #sendFit(): void {
    if (!this.#fitPending || !this.#ready || this.#view === undefined) {
      return;
    }
    void this.#view.webview
      .postMessage({ type: "fit" })
      .then((delivered) => {
        if (delivered) {
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
.workspace { display: grid; grid-template-columns: minmax(0, 2fr) minmax(175px, 1fr); gap: 10px; }
.graph-panel, .inspector, .threads, .timeline { position: relative; min-width: 0; border: 1px solid var(--vscode-widget-border); border-radius: 9px; background: var(--vscode-editorWidget-background); overflow: hidden; }
.graph-controls { position: absolute; right: 7px; top: 7px; z-index: 2; display: flex; gap: 4px; }
.graph-viewport { height: 410px; overflow: hidden; touch-action: none; }
.graph-viewport svg { width: 100%; height: 100%; }
.tree-edge { fill: none; stroke: var(--vscode-editorIndentGuide-background); stroke-width: 1.5; }
.tree-node { cursor: pointer; }
.tree-node rect { fill: var(--vscode-editorWidget-background); stroke: var(--vscode-widget-border); stroke-width: 1.5; }
.tree-node:hover rect, .tree-node.selected rect { stroke: var(--vscode-focusBorder); stroke-width: 2.5; }
.tree-node.current rect { fill: var(--vscode-list-activeSelectionBackground); stroke: var(--vscode-debugIcon-breakpointCurrentStackframeForeground); }
.tree-node text { fill: var(--vscode-foreground); pointer-events: none; }
.node-id { font-size: 14px; font-weight: 700; }
.node-status, .node-thread { font-size: 10px; fill: var(--vscode-descriptionForeground) !important; }
.filtered { display: none; }
.omitted { position: absolute; bottom: 2px; left: 8px; color: var(--vscode-descriptionForeground); font-size: 10px; }
.inspector, .threads, .timeline { padding: 11px; }
h2 { margin: 0 0 10px; font-size: 12px; text-transform: uppercase; color: var(--vscode-descriptionForeground); }
.inspector-title { display: block; margin-bottom: 10px; font-size: 15px; }
dl { margin: 0; display: grid; grid-template-columns: 58px 1fr; gap: 6px; }
dt { color: var(--vscode-descriptionForeground); }
dd { margin: 0; overflow-wrap: anywhere; }
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
@media (max-width: 520px) { .workspace { grid-template-columns: 1fr; } .graph-viewport { height: 330px; } .cards { grid-template-columns: repeat(2, 1fr); } }
@media (forced-colors: active) { .tree-node rect, .graph-panel, .inspector, .threads, .timeline, .card { border: 1px solid CanvasText; } }
`;
