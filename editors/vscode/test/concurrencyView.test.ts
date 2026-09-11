import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { resolve } from "node:path";
import { after, afterEach, before, beforeEach, describe, it } from "node:test";
import { pathToFileURL } from "node:url";

import { build } from "esbuild";
import type * as vscode from "vscode";

import type * as concurrencyView from "../src/concurrencyView.js";
import type { activate } from "../src/extension.js";
import type {
  ConcurrencyViewActions,
  ConcurrencyViewProvider,
} from "../src/concurrencyView.js";
import type { HostMessage, WebviewMessage } from "../src/messages.js";
import {
  emptyInspection,
  toSessionViewModel,
  type ConcurrencyViewModel,
  type DebugInspection,
  type SessionModel,
} from "../src/model.js";
import type { SessionRegistry } from "../src/registry.js";
import type { DisplayedState } from "../src/webviewTest.js";
import { goroutine, snapshot } from "./fixtures.js";

type ViewModule = typeof concurrencyView & { activate: typeof activate };
type Render = Extract<HostMessage, { type: "render" }>;

class Emitter<T> {
  readonly listeners = new Set<(value: T) => void>();

  public readonly event = (listener: (value: T) => void): vscode.Disposable => {
    this.listeners.add(listener);
    return { dispose: () => { this.listeners.delete(listener); } };
  };

  public fire(value: T): void {
    for (const listener of [...this.listeners]) {
      listener(value);
    }
  }
}

interface PostedMessage {
  readonly message: Record<string, unknown>;
  readonly resolve: (sent: boolean) => void;
  readonly reject: (error: Error) => void;
}

class FakeWebview {
  public options: vscode.WebviewOptions = {};
  public html = "";
  public holdMessages = false;
  readonly received = new Emitter<unknown>();
  readonly posts: PostedMessage[] = [];
  readonly onDidReceiveMessage = this.received.event;

  public asWebviewUri(uri: vscode.Uri): vscode.Uri {
    return uri;
  }

  public postMessage(message: Record<string, unknown>): Promise<boolean> {
    return new Promise((resolve, reject) => {
      this.posts.push({ message, resolve, reject });
      if (!this.holdMessages) {
        resolve(true);
      }
    });
  }

  public get renders(): readonly Render[] {
    return this.posts
      .filter((post) => post.message.type === "render")
      .map((post) => post.message as unknown as Render);
  }

  public get latest(): Render {
    const render = this.renders.at(-1);
    assert.ok(render, "a ready document must receive a render");
    return render;
  }

  public ready(): Render {
    this.received.fire({ type: "ready" });
    return this.latest;
  }

  public acknowledge(render = this.latest): void {
    this.received.fire({
      type: "rendered",
      generation: render.generation,
      revision: render.revision,
    });
  }
}

class FakeSurface {
  readonly webview = new FakeWebview();
  readonly disposed = new Emitter<void>();
  readonly visibility = new Emitter<void>();
  readonly onDidDispose = this.disposed.event;
  readonly onDidChangeVisibility = this.visibility.event;
  readonly onDidChangeViewState = this.visibility.event;
  readonly reveals: { column: vscode.ViewColumn | undefined; preserveFocus: boolean }[] = [];
  public visible = true;
  public viewColumn: vscode.ViewColumn = 2;
  public disposeCalls = 0;
  #closed = false;

  public reveal(column: vscode.ViewColumn | undefined, preserveFocus: boolean): void {
    this.reveals.push({ column, preserveFocus });
    this.visible = true;
    this.visibility.fire();
  }

  public hide(): void {
    this.visible = false;
    this.visibility.fire();
  }

  public dispose(): void {
    this.disposeCalls += 1;
    if (!this.#closed) {
      this.#closed = true;
      this.visible = false;
      this.disposed.fire();
      this.disposed.listeners.clear();
      this.visibility.listeners.clear();
      this.webview.received.listeners.clear();
    }
  }

  public asPanel(): vscode.WebviewPanel {
    return this as unknown as vscode.WebviewPanel;
  }

  public asSidebar(): vscode.WebviewView {
    return this as unknown as vscode.WebviewView;
  }
}

class FakeUri {
  public constructor(readonly path: string) {}

  public static joinPath(uri: FakeUri, ...parts: string[]): FakeUri {
    return new FakeUri([uri.path, ...parts].join("/"));
  }

  public toString(): string {
    return this.path;
  }
}

interface PanelCreation {
  readonly viewType: string;
  readonly title: string;
  readonly showOptions: { readonly viewColumn: number; readonly preserveFocus: boolean };
  readonly options: vscode.WebviewPanelOptions;
}

class VscodeMock {
  readonly panels: FakeSurface[] = [];
  readonly creations: PanelCreation[] = [];
  readonly information: string[] = [];
  readonly clipboard: string[] = [];
  readonly statuses: string[] = [];
  readonly Uri = FakeUri;
  readonly ViewColumn = { One: 1, Two: 2, Beside: -2 };
  readonly ExtensionMode = { Test: 3 };
  readonly StatusBarAlignment = { Left: 1 };
  readonly registeredCommands = new Map<string, () => unknown>();
  readonly registeredViews = new Map<string, ConcurrencyViewProvider>();
  readonly commands = {
    registerCommand: (id: string, callback: () => unknown): vscode.Disposable => {
      assert.equal(this.registeredCommands.has(id), false);
      this.registeredCommands.set(id, callback);
      return { dispose: () => { this.registeredCommands.delete(id); } };
    },
    executeCommand: (id: string): Promise<unknown> => {
      const callback = this.registeredCommands.get(id);
      assert.ok(callback, `command ${id} must be registered by activate`);
      return Promise.resolve(callback());
    },
  };
  readonly workspace = {
    isTrusted: true,
    workspaceFolders: [],
    getConfiguration: () => ({ get: () => false }),
  };
  readonly debug = {
    registerDebugConfigurationProvider: disposable,
    registerDebugAdapterDescriptorFactory: disposable,
    registerDebugAdapterTrackerFactory: disposable,
    onDidStartDebugSession: disposable,
    onDidReceiveDebugSessionCustomEvent: disposable,
    onDidChangeActiveDebugSession: disposable,
    onDidTerminateDebugSession: disposable,
  };
  readonly window = {
    activeTextEditor: { viewColumn: 1 } as { viewColumn: vscode.ViewColumn } | undefined,
    visibleTextEditors: [{ viewColumn: 1 }] as { viewColumn: vscode.ViewColumn }[],
    createOutputChannel: () => ({ appendLine() {}, dispose() {} }),
    createStatusBarItem: () => ({ show() {}, hide() {}, dispose() {} }),
    registerWebviewViewProvider: (id: string, provider: ConcurrencyViewProvider): vscode.Disposable => {
      assert.equal(this.registeredViews.has(id), false);
      this.registeredViews.set(id, provider);
      return { dispose: () => { this.registeredViews.delete(id); } };
    },
    createWebviewPanel: (
      viewType: string,
      title: string,
      showOptions: PanelCreation["showOptions"],
      options: vscode.WebviewPanelOptions,
    ): FakeSurface => {
      this.creations.push({ viewType, title, showOptions, options });
      const panel = new FakeSurface();
      this.panels.push(panel);
      return panel;
    },
    showInformationMessage: (message: string): Promise<undefined> => {
      this.information.push(message);
      return Promise.resolve(undefined);
    },
    setStatusBarMessage: (message: string): vscode.Disposable => {
      this.statuses.push(message);
      return { dispose() {} };
    },
  };
  readonly env = {
    clipboard: {
      writeText: (text: string): Promise<void> => {
        this.clipboard.push(text);
        return Promise.resolve();
      },
    },
  };

  public reset(): void {
    this.panels.length = 0;
    this.creations.length = 0;
    this.information.length = 0;
    this.clipboard.length = 0;
    this.statuses.length = 0;
    this.window.activeTextEditor = { viewColumn: 1 };
    this.window.visibleTextEditors = [{ viewColumn: 1 }];
  }
}

function disposable(): vscode.Disposable {
  return { dispose() {} };
}

function session(patch: Partial<SessionModel> = {}): SessionModel {
  return {
    debugSessionId: "debug-a",
    debugSessionName: "Worker",
    sessionId: "session-a",
    connection: "connected",
    sessionState: "suspended",
    clients: 2,
    lastStop: "Breakpoint",
    error: "",
    seqGap: "",
    lastSeq: 10,
    snapshot: snapshot([
      goroutine(7, 1, {
        current: true,
        createdLoc: { file: "/workspace/spawn.go", line: 21, function: "main.spawn" },
        startLoc: { file: "/workspace/worker.go", line: 32, function: "main.worker" },
        currentLoc: { file: "/workspace/stopped.go", line: 43, function: "main.stopped" },
      }),
      goroutine(8),
    ]),
    selectedGoroutine: 7,
    timeline: [],
    ...patch,
  };
}

function inspection(): DebugInspection {
  return {
    ...emptyInspection(7),
    stackStatus: "ready",
    localsStatus: "ready",
    selectedFrameId: 11,
    frames: [
      { id: 11, name: "main.caller", file: "/workspace/caller.go", line: 54, column: 6 },
      { id: 12, name: "main.parent", file: "/workspace/parent.go", line: 65, column: 7 },
    ],
  };
}

class FakeRegistry {
  public sessions: SessionModel[] = [session()];
  public activeId = "debug-a";
  public revision = 1;
  readonly inspections = new Map<string, DebugInspection>([["debug-a", inspection()]]);
  readonly changes = new Emitter<ConcurrencyViewModel>();
  readonly calls: unknown[][] = [];
  public unsubscribes = 0;

  public get viewModel(): ConcurrencyViewModel {
    return {
      revision: this.revision,
      activeDebugSessionId: this.activeId,
      sessions: this.sessions.map((model) =>
        toSessionViewModel(model, this.inspections.get(model.debugSessionId))),
    };
  }

  public onChange(listener: (model: ConcurrencyViewModel) => void): () => void {
    const subscription = this.changes.event(listener);
    return () => {
      this.unsubscribes += 1;
      subscription.dispose();
    };
  }

  public activeModel(): SessionModel | undefined {
    return this.sessions.find((model) => model.debugSessionId === this.activeId);
  }

  public inspectionFor(id: string): DebugInspection | undefined {
    return this.inspections.get(id);
  }

  public update(patch: Partial<SessionModel> = {}): void {
    this.sessions = this.sessions.map((model) =>
      model.debugSessionId === this.activeId ? { ...model, ...patch } : model);
    this.publish();
  }

  public publish(): void {
    this.revision += 1;
    this.changes.fire(this.viewModel);
  }

  public refresh(): void { this.calls.push(["refresh"]); }
  public select(id: string): void { this.calls.push(["selectSession", id]); }
  public selectGoroutine(id: number): void { this.calls.push(["selectGoroutine", id]); }
  public activeSnapshotJSON(): string { return '{"snapshot":"host-owned"}'; }

  public asRegistry(): SessionRegistry {
    return this as unknown as SessionRegistry;
  }
}

interface SourceRequest {
  readonly path: string;
  readonly line: number;
  readonly column: number;
  readonly isCurrent: () => boolean;
}

function action(render: Render, message: WebviewMessage): Record<string, unknown> {
  const active = render.model.sessions.find((model) =>
    model.debugSessionId === render.model.activeDebugSessionId);
  return {
    type: "action",
    generation: render.generation,
    revision: render.revision,
    debugSessionId: active?.debugSessionId ?? "",
    goroutineId: active?.selectedGoroutine ?? 0,
    action: message,
  };
}

describe("concurrency host surfaces", () => {
  const mock = new VscodeMock();
  const globals = globalThis as typeof globalThis & { __bingoViewTest?: VscodeMock };
  let module: ViewModule;
  const providers: ConcurrencyViewProvider[] = [];
  const subscriptions: vscode.Disposable[] = [];

  before(async () => {
    globals.__bingoViewTest = mock;
    // Bundle in memory so the real host code runs without Electron or a fake
    // vscode package leaking into other tests' module resolution.
    const result = await build({
      stdin: {
        contents: `
          export * from "./src/concurrencyView.ts";
          export { activate } from "./src/extension.ts";
        `,
        resolveDir: process.cwd(),
        loader: "ts",
      },
      bundle: true,
      platform: "node",
      format: "esm",
      write: false,
      plugins: [{
        name: "injected-vscode",
        setup(build) {
          build.onResolve({ filter: /^ws$/ }, () => ({
            path: pathToFileURL(createRequire(resolve(process.cwd(), "package.json")).resolve("ws")).href,
            external: true,
          }));
          build.onResolve({ filter: /^vscode$/ }, () => ({
            path: "vscode",
            namespace: "test-vscode",
          }));
          build.onLoad({ filter: /.*/, namespace: "test-vscode" }, () => ({
            contents: `
              const mock = globalThis.__bingoViewTest;
              export const Uri = mock.Uri;
              export const ViewColumn = mock.ViewColumn;
              export const ExtensionMode = mock.ExtensionMode;
              export const StatusBarAlignment = mock.StatusBarAlignment;
              export const window = mock.window;
              export const env = mock.env;
              export const workspace = mock.workspace;
              export const commands = mock.commands;
              export const debug = mock.debug;
              class UnexpectedAPI {
                constructor() { throw new Error("Unexpected VS Code API in Fit/provider test"); }
              }
              export { UnexpectedAPI as DebugAdapterServer, UnexpectedAPI as Position, UnexpectedAPI as Range };
            `,
            loader: "js",
          }));
        },
      }],
    });
    const output = result.outputFiles[0];
    assert.ok(output);
    module = await import(
      `data:text/javascript;base64,${Buffer.from(output.contents).toString("base64")}`
    ) as ViewModule;
  });

  beforeEach(() => { mock.reset(); });
  afterEach(() => {
    subscriptions.splice(0).reverse().forEach((subscription) => { subscription.dispose(); });
    providers.splice(0).forEach((provider) => { provider.dispose(); });
    mock.panels.forEach((panel) => { panel.dispose(); });
    assert.equal(mock.registeredCommands.size, 0);
    assert.equal(mock.registeredViews.size, 0);
  });
  after(() => { delete globals.__bingoViewTest; });

  function harness(registry = new FakeRegistry(), testing = false) {
    const calls: unknown[][] = [];
    const sources: SourceRequest[] = [];
    const actions: ConcurrencyViewActions = {
      selectFrame: (id) => { calls.push(["selectFrame", id]); },
      expandVariable: (reference) => { calls.push(["expandVariable", reference]); },
      refreshInspection: () => { calls.push(["refreshInspection"]); },
      refreshSource: () => { calls.push(["refreshSource"]); },
      openSource: (path, line, column, isCurrent) => {
        sources.push({ path, line, column, isCurrent });
      },
    };
    const provider = new module.ConcurrencyViewProvider(
      new FakeUri("/extension") as unknown as vscode.Uri,
      registry.asRegistry(),
      actions,
      testing,
    );
    providers.push(provider);
    return { registry, calls, sources, provider };
  }

  function attached() {
    const state = harness();
    const surface = new FakeSurface();
    state.provider.resolvePanel(surface.asPanel());
    const render = surface.webview.ready();
    surface.webview.acknowledge();
    return { ...state, surface, render };
  }

  function activatedSidebar(): FakeSurface {
    module.activate({
      extensionUri: new FakeUri("/extension"),
      extensionPath: "/extension",
      globalStorageUri: { fsPath: "/storage" },
      extensionMode: mock.ExtensionMode.Test,
      subscriptions,
    } as unknown as vscode.ExtensionContext);
    const provider = mock.registeredViews.get("bingo.concurrency");
    assert.ok(provider, "activate must register the Activity Bar provider");
    const sidebar = new FakeSurface();
    provider.resolveWebviewView(sidebar.asSidebar());
    sidebar.webview.ready();
    sidebar.webview.acknowledge();
    return sidebar;
  }

  function sidebarFitCommand(): string {
    const manifest = JSON.parse(readFileSync(resolve(process.cwd(), "package.json"), "utf8")) as {
      contributes: {
        commands: { command: string; icon?: string }[];
        menus: { "view/title": { command: string; when: string }[] };
      };
    };
    const fitCommands = manifest.contributes.commands
      .filter((command) => command.icon === "$(screen-full)")
      .map((command) => command.command);
    const routes = manifest.contributes.menus["view/title"].filter((route) =>
      route.when === "view == bingo.concurrency" && fitCommands.includes(route.command));
    assert.equal(routes.length, 1, "the sidebar must contribute exactly one Fit title action");
    return routes[0]!.command;
  }

  function fitCount(surface: FakeSurface): number {
    return surface.webview.posts.filter((post) => post.message.type === "fit").length;
  }

  for (const state of ["never-opened", "user-closed", "both-open"] as const) {
    it(`routes the registered Activity Bar title Fit only to the sidebar with editor ${state}`, async () => {
      const sidebar = activatedSidebar();
      let panel: FakeSurface | undefined;
      if (state !== "never-opened") {
        await mock.commands.executeCommand("bingo.concurrency.openEditor");
        panel = mock.panels[0]!;
        panel.webview.ready();
        panel.webview.acknowledge();
        if (state === "user-closed") {
          panel.dispose();
        }
      }
      const creations = mock.creations.length;
      const activeSource = mock.window.activeTextEditor;
      const sourceEditors = [...mock.window.visibleTextEditors];
      await mock.commands.executeCommand(sidebarFitCommand());
      assert.equal(fitCount(sidebar), 1);
      assert.equal(panel === undefined ? 0 : fitCount(panel), 0);
      assert.equal(mock.creations.length, creations, "sidebar Fit must not create an editor/group");
      assert.deepEqual(panel?.reveals ?? [], [], "sidebar Fit must not reveal or focus the editor");
      assert.equal(mock.window.activeTextEditor, activeSource);
      assert.deepEqual(mock.window.visibleTextEditors, sourceEditors);
      assert.equal(sidebar.visible, true);
      if (state === "user-closed") {
        assert.equal(panel?.visible, false);
      }
    });
  }

  it("preserves the explicit registered editor Fit command and surface-local webview Fit", async () => {
    const sidebar = activatedSidebar();
    await mock.commands.executeCommand("bingo.concurrency.fit");
    assert.equal(mock.creations.length, 1);
    assert.equal(mock.creations[0]?.showOptions.preserveFocus, false);
    const panel = mock.panels[0]!;
    panel.webview.ready();
    panel.webview.acknowledge();
    assert.equal(fitCount(panel), 1, "Fit queued before editor ready must reach that document");
    assert.equal(fitCount(sidebar), 0);
    await mock.commands.executeCommand("bingo.concurrency.fit");
    assert.equal(fitCount(panel), 2);
    assert.equal(mock.creations.length, 1);
    assert.deepEqual(panel.reveals, [{ column: 2, preserveFocus: false }]);

    panel.webview.received.fire(action(panel.webview.latest, { type: "fit" }));
    assert.equal(fitCount(panel), 3);
    assert.equal(fitCount(sidebar), 0);
    sidebar.webview.received.fire(action(sidebar.webview.latest, { type: "fit" }));
    assert.equal(fitCount(sidebar), 1);
    assert.equal(fitCount(panel), 3);
    assert.equal(panel.reveals.length, 1, "webview Fit must not reveal another surface");
  });

  it("automatically opens one beside-source editor without taking focus", () => {
    const { provider, registry } = harness();
    const editor = new module.ConcurrencyEditor(provider);
    editor.sessionStarted("debug-a", true);
    assert.deepEqual(mock.creations, [{
      viewType: "bingo.concurrency.editor",
      title: "Bingo Concurrency",
      showOptions: { viewColumn: -2, preserveFocus: true },
      options: { retainContextWhenHidden: false },
    }]);
    const panel = mock.panels[0]!;
    panel.webview.ready();
    panel.webview.acknowledge();
    for (let stop = 0; stop < 4; stop += 1) {
      registry.update({ lastStop: `Stop ${String(stop)}` });
      panel.webview.acknowledge();
      editor.sessionStarted("debug-a", true);
    }
    assert.equal(mock.panels.length, 1);
    assert.deepEqual(panel.reveals, []);
    panel.hide();
    editor.sessionStarted("debug-a", true);
    assert.equal(panel.visible, false);
    assert.deepEqual(panel.reveals, []);
    editor.sessionStarted("debug-b", true);
    assert.equal(mock.panels.length, 1);
    assert.equal(panel.visible, true);
    assert.deepEqual(panel.reveals, [{ column: 2, preserveFocus: true }]);
    editor.dispose();
  });

  it("respects auto-reveal opt-out and still allows explicit manual opening", () => {
    const { provider } = harness();
    const editor = new module.ConcurrencyEditor(provider);
    editor.sessionStarted("debug-a", false);
    editor.sessionStarted("debug-a", true);
    assert.equal(mock.panels.length, 0);
    editor.open();
    assert.equal(mock.creations[0]?.showOptions.preserveFocus, false);
    editor.open();
    assert.equal(mock.panels.length, 1);
    assert.deepEqual(mock.panels[0]?.reveals, [{ column: 2, preserveFocus: false }]);
    editor.dispose();
  });

  it("keeps a closed editor closed for that session but opens for a new session", () => {
    const { provider, registry } = harness();
    const editor = new module.ConcurrencyEditor(provider);
    editor.sessionStarted("debug-a", true);
    const first = mock.panels[0]!;
    first.dispose();
    registry.update({ lastStop: "A later stop" });
    editor.sessionStarted("debug-a", true);
    assert.equal(mock.panels.length, 1);
    assert.equal(registry.changes.listeners.size, 1);
    assert.deepEqual(registry.calls, []);
    editor.sessionStarted("debug-b", true);
    assert.equal(mock.panels.length, 2);
    assert.equal(mock.creations[1]?.showOptions.preserveFocus, true);
    editor.open();
    assert.equal(mock.panels.length, 2);
    assert.deepEqual(mock.panels[1]?.reveals, [{ column: 2, preserveFocus: false }]);
    editor.dispose();
  });

  it("allows manual reopening in the same session and clears ended-session announcements", () => {
    const { provider } = harness();
    const editor = new module.ConcurrencyEditor(provider);
    editor.sessionStarted("debug-a", true);
    mock.panels[0]!.dispose();
    editor.open();
    assert.equal(mock.panels.length, 2);
    assert.equal(mock.creations[1]?.showOptions.preserveFocus, false);
    mock.panels[1]!.dispose();
    editor.sessionEnded("debug-a");
    editor.sessionStarted("debug-a", true);
    assert.equal(mock.panels.length, 3);
    editor.dispose();
  });

  it("chooses a visible source group other than the panel, including a moved panel", () => {
    const { provider } = harness();
    const editor = new module.ConcurrencyEditor(provider);
    mock.window.activeTextEditor = { viewColumn: 3 };
    mock.window.visibleTextEditors = [{ viewColumn: 3 }];
    editor.open();
    assert.equal(editor.sourceColumn, 3);
    const panel = mock.panels[0]!;
    panel.viewColumn = 3;
    mock.window.visibleTextEditors = [{ viewColumn: 3 }, { viewColumn: 1 }];
    assert.equal(editor.sourceColumn, 1);
    mock.window.visibleTextEditors = [];
    assert.notEqual(editor.sourceColumn, panel.viewColumn);
    editor.dispose();
  });

  it("does not open, reveal, subscribe or act after editor disposal", () => {
    const { provider, registry } = harness();
    const editor = new module.ConcurrencyEditor(provider);
    editor.open();
    const panel = mock.panels[0]!;
    const staleReceive = [...panel.webview.received.listeners][0]!;
    const render = panel.webview.ready();
    editor.dispose();
    const posts = panel.webview.posts.length;
    editor.open();
    editor.sessionStarted("new-session", true);
    editor.sessionEnded("new-session");
    registry.update();
    staleReceive(action(render, { type: "refresh" }));
    assert.equal(mock.panels.length, 1);
    assert.equal(panel.disposeCalls, 1);
    assert.equal(panel.webview.posts.length, posts);
    assert.equal(registry.changes.listeners.size, 0);
    assert.equal(registry.unsubscribes, 1);
    assert.equal(panel.webview.received.listeners.size, 0);
    assert.equal(panel.visibility.listeners.size, 0);
    assert.equal(panel.disposed.listeners.size, 0);
    assert.deepEqual(registry.calls, []);
    assert.deepEqual(provider.status, { resolved: false, ready: false, visible: false });
  });

  it("gives the sidebar and editor independent readiness, acknowledgements and callbacks", () => {
    const registry = new FakeRegistry();
    const sidebarHost = harness(registry);
    const editorHost = harness(registry);
    const sidebar = new FakeSurface();
    sidebarHost.provider.resolveWebviewView(sidebar.asSidebar());
    const editor = new module.ConcurrencyEditor(editorHost.provider);
    editor.open();
    const panel = mock.panels[0]!;
    assert.equal(registry.changes.listeners.size, 2);
    sidebar.webview.ready();
    sidebar.webview.acknowledge();
    assert.equal(panel.webview.posts.length, 0);
    assert.equal(sidebarHost.provider.lastRenderedRevision, 1);
    assert.equal(editorHost.provider.lastRenderedRevision, -1);
    panel.webview.ready();
    panel.webview.acknowledge();
    sidebar.webview.received.fire(action(sidebar.webview.latest, {
      type: "openSource", target: "created", frameId: 0,
    }));
    panel.webview.received.fire(action(panel.webview.latest, {
      type: "openSource", target: "start", frameId: 0,
    }));
    assert.equal(sidebarHost.sources[0]?.path, "/workspace/spawn.go");
    assert.equal(editorHost.sources[0]?.path, "/workspace/worker.go");
    panel.hide();
    assert.equal(editorHost.sources[0]?.isCurrent(), false);
    assert.equal(sidebarHost.sources[0]?.isCurrent(), true);
    registry.update({ lastStop: "Next stop" });
    sidebar.webview.acknowledge();
    assert.equal(sidebarHost.provider.lastRenderedRevision, 2);
    assert.equal(panel.webview.renders.length, 1);
    panel.visible = true;
    const replacement = panel.webview.ready();
    panel.webview.acknowledge(sidebar.webview.latest);
    assert.equal(editorHost.provider.lastRenderedRevision, -1);
    panel.webview.acknowledge(replacement);
    assert.equal(editorHost.provider.lastRenderedRevision, 2);
    editor.dispose();
    assert.equal(registry.changes.listeners.size, 1);
    registry.update();
    sidebar.webview.acknowledge();
    assert.equal(sidebarHost.provider.lastRenderedRevision, 3);
  });

  it("removes the old surface's listeners when replacing it and on disposal", () => {
    const { provider, registry } = harness();
    const first = new FakeSurface();
    const replacement = new FakeSurface();
    provider.resolveWebviewView(first.asSidebar());
    const staleReceive = [...first.webview.received.listeners][0]!;
    const firstRender = first.webview.ready();
    provider.resolvePanel(replacement.asPanel());
    replacement.webview.ready();
    assert.equal(first.webview.received.listeners.size, 0);
    assert.equal(first.visibility.listeners.size, 0);
    assert.equal(first.disposed.listeners.size, 0);
    staleReceive(action(firstRender, { type: "refresh" }));
    first.dispose();
    assert.equal(provider.status.resolved, true);
    assert.deepEqual(registry.calls, []);
    provider.dispose();
    assert.equal(replacement.webview.received.listeners.size, 0);
    assert.equal(replacement.visibility.listeners.size, 0);
    assert.equal(replacement.disposed.listeners.size, 0);
    assert.equal(registry.changes.listeners.size, 0);
    const ignored = new FakeSurface();
    provider.resolveWebviewView(ignored.asSidebar());
    assert.equal(ignored.webview.html, "");
    assert.equal(ignored.webview.received.listeners.size, 0);
  });

  it("explicitly releases every surface listener when its disposal event arrives", () => {
    const { surface, provider, registry } = attached();
    assert.equal(surface.webview.received.listeners.size, 1);
    assert.equal(surface.visibility.listeners.size, 1);
    assert.equal(surface.disposed.listeners.size, 1);
    // Fire only the event, without the mock's automatic emitter cleanup, so
    // these assertions exercise the provider's own disposal subscriptions.
    surface.disposed.fire();
    assert.equal(surface.webview.received.listeners.size, 0);
    assert.equal(surface.visibility.listeners.size, 0);
    assert.equal(surface.disposed.listeners.size, 0);
    assert.equal(registry.changes.listeners.size, 1);
    assert.deepEqual(provider.status, { resolved: false, ready: false, visible: false });
    const replacement = new FakeSurface();
    provider.resolvePanel(replacement.asPanel());
    replacement.webview.ready();
    replacement.webview.acknowledge();
    assert.equal(provider.lastRenderedRevision, registry.revision);
    assert.equal(replacement.webview.received.listeners.size, 1);
  });

  for (const failure of ["false", "rejected"] as const) {
    for (const replacementKind of ["same surface", "new surface"] as const) {
      it(`ignores ${failure} from an old delivery after ${replacementKind} recreation at the same revision`, async () => {
        const { provider, registry } = harness();
        const old = new FakeSurface();
        old.webview.holdMessages = true;
        provider.resolvePanel(old.asPanel());
        const oldRender = old.webview.ready();
        const pending = old.webview.posts[0]!;
        let current = old;
        if (replacementKind === "same surface") {
          old.hide();
          old.visible = true;
        } else {
          old.dispose();
          current = new FakeSurface();
          provider.resolvePanel(current.asPanel());
        }
        const next = current.webview.ready();
        assert.equal(next.revision, oldRender.revision);
        assert.notEqual(next.generation, oldRender.generation);
        if (failure === "false") {
          pending.resolve(false);
        } else {
          pending.reject(new Error("old delivery failed"));
        }
        await Promise.resolve();
        assert.equal(provider.status.ready, true);
        current.webview.acknowledge(oldRender);
        assert.equal(provider.lastRenderedRevision, -1);
        current.webview.acknowledge(next);
        assert.equal(provider.lastRenderedRevision, next.revision);
        registry.update();
        assert.equal(current.webview.latest.revision, registry.revision);
      });
    }
  }

  it("invalidates a current failed delivery and waits for a fresh ready handshake", async () => {
    const { provider, registry } = harness();
    const panel = new FakeSurface();
    panel.webview.holdMessages = true;
    provider.resolvePanel(panel.asPanel());
    const stale = panel.webview.ready();
    panel.webview.posts[0]!.resolve(false);
    await Promise.resolve();
    assert.equal(provider.status.ready, false);
    registry.update();
    assert.equal(panel.webview.renders.length, 1);
    const next = panel.webview.ready();
    assert.equal(next.revision, registry.revision);
    assert.notEqual(next.generation, stale.generation);
  });

  it("does not let a stale fit completion consume the replacement document's pending fit", async () => {
    const { provider } = harness();
    const first = new FakeSurface();
    first.webview.holdMessages = true;
    provider.resolvePanel(first.asPanel());
    first.webview.ready();
    provider.fit();
    const fit = first.webview.posts.find((post) => post.message.type === "fit")!;
    first.dispose();
    const replacement = new FakeSurface();
    provider.resolvePanel(replacement.asPanel());
    fit.resolve(true);
    await Promise.resolve();
    replacement.webview.ready();
    assert.equal(replacement.webview.posts.filter((post) => post.message.type === "fit").length, 1);
  });

  it("coalesces updates behind the current render without accepting stale acknowledgements", () => {
    const { provider, registry } = harness();
    const panel = new FakeSurface();
    provider.resolvePanel(panel.asPanel());
    const first = panel.webview.ready();
    registry.update();
    registry.update();
    assert.equal(panel.webview.renders.length, 1);
    panel.webview.acknowledge({ ...first, revision: 999 });
    assert.equal(provider.lastRenderedRevision, -1);
    panel.webview.acknowledge(first);
    assert.equal(panel.webview.renders.length, 2);
    assert.equal(panel.webview.latest.revision, 3);
    panel.webview.acknowledge(first);
    assert.equal(provider.lastRenderedRevision, 1);
    panel.webview.acknowledge();
    assert.equal(provider.lastRenderedRevision, 3);
  });

  it("keeps diagnostic inspection unavailable in normal extension hosts", async () => {
    const { provider, surface, registry, calls, sources } = attached();
    await assert.rejects(provider.testUI(), /not ready/u);
    surface.webview.received.fire({
      type: "testResult", id: 1,
      state: { type: "openSource", path: "/attacker/file.go", line: 1 },
    });
    assert.equal(surface.webview.posts.some((post) => post.message.type === "testInspect"), false);
    assert.deepEqual(registry.calls, []);
    assert.deepEqual(calls, []);
    assert.deepEqual(sources, []);
  });

  for (const change of ["hide", "replace", "dispose"] as const) {
    it(`rejects pending diagnostic probes on ${change} without leaving a timer or listener`, async () => {
      const { provider, registry } = harness(new FakeRegistry(), true);
      const panel = new FakeSurface();
      provider.resolvePanel(panel.asPanel());
      panel.webview.ready();
      const probe = provider.testUI();
      const rejected = assert.rejects(probe, /changed|disposed/u);
      if (change === "hide") {
        panel.hide();
      } else if (change === "replace") {
        provider.resolvePanel(new FakeSurface().asPanel());
      } else {
        provider.dispose();
      }
      await rejected;
      if (change !== "hide") {
        assert.equal(panel.webview.received.listeners.size, 0);
        assert.equal(panel.visibility.listeners.size, 0);
      }
      assert.deepEqual(registry.calls, []);
    });
  }

  it("does not let an old diagnostic reply or post failure settle a replacement probe", async () => {
    const { provider } = harness(new FakeRegistry(), true);
    const old = new FakeSurface();
    old.webview.holdMessages = true;
    provider.resolvePanel(old.asPanel());
    old.webview.ready();
    const previous = provider.testUI();
    const rejected = assert.rejects(previous, /changed|disposed/u);
    const oldPost = old.webview.posts.find((post) => post.message.type === "testInspect")!;
    const replacement = new FakeSurface();
    provider.resolvePanel(replacement.asPanel());
    await rejected;
    replacement.webview.ready();
    let settled = false;
    const current = provider.testUI().then((state) => { settled = true; return state; });
    const currentPost = replacement.webview.posts.find((post) => post.message.type === "testInspect")!;
    const state: DisplayedState = {
      inspector: "g7", creation: "spawn.go:21", highlightedLine: "go worker()",
      frames: ["main.worker"], variables: ["count"],
    };
    assert.notEqual(oldPost.message.id, currentPost.message.id);
    replacement.webview.received.fire({ type: "testResult", id: oldPost.message.id, state });
    oldPost.resolve(false);
    await Promise.resolve();
    assert.equal(settled, false);
    replacement.webview.received.fire({ type: "testResult", id: currentPost.message.id, state });
    assert.deepEqual(await current, state);
  });

  it("dispatches valid read-only actions through their host-owned callbacks", async () => {
    const { surface, render, registry, calls } = attached();
    const actions: WebviewMessage[] = [
      { type: "refresh" },
      { type: "selectSession", id: "debug-b" },
      { type: "selectGoroutine", id: 8 },
      { type: "selectFrame", id: 11 },
      { type: "expandVariable", reference: 65536 },
      { type: "refreshInspection" },
      { type: "fit" },
      { type: "copySnapshot" },
    ];
    actions.forEach((message) => { surface.webview.received.fire(action(render, message)); });
    await Promise.resolve();
    assert.deepEqual(registry.calls, [
      ["refresh"], ["selectSession", "debug-b"], ["selectGoroutine", 8],
    ]);
    assert.deepEqual(calls, [
      ["refreshSource"], ["selectFrame", 11], ["expandVariable", 65536], ["refreshInspection"],
    ]);
    assert.equal(surface.webview.posts.filter((post) => post.message.type === "fit").length, 1);
    assert.deepEqual(mock.clipboard, ['{"snapshot":"host-owned"}']);
    assert.equal(mock.statuses.length, 1);
  });

  for (const field of ["generation", "revision", "debugSessionId", "goroutineId"] as const) {
    it(`rejects every actionable message with a stale ${field}`, () => {
      const { surface, render, registry, calls, sources } = attached();
      const messages: WebviewMessage[] = [
        { type: "refresh" },
        { type: "selectSession", id: "debug-b" },
        { type: "selectGoroutine", id: 8 },
        { type: "selectFrame", id: 11 },
        { type: "expandVariable", reference: 65536 },
        { type: "refreshInspection" },
        { type: "openSource", target: "created", frameId: 0 },
        { type: "fit" },
        { type: "copySnapshot" },
      ];
      const before = surface.webview.posts.length;
      for (const message of messages) {
        surface.webview.received.fire({
          ...action(render, message),
          [field]: field === "debugSessionId" ? "other-session" : 999,
        });
      }
      assert.deepEqual(registry.calls, []);
      assert.deepEqual(calls, []);
      assert.deepEqual(sources, []);
      assert.deepEqual(mock.clipboard, []);
      assert.equal(surface.webview.posts.length, before);
    });
  }

  it("silently ignores malformed actions, forged paths and run-control commands", () => {
    const { surface, render, registry, calls, sources } = attached();
    const valid = action(render, { type: "openSource", target: "created", frameId: 0 });
    const messages: unknown[] = [
      null, [], "refresh", {}, { type: "refresh" },
      { type: "openSource", path: "/attacker/path.go", line: 1 },
      { ...valid, path: "/attacker/path.go" },
      { ...valid, action: { type: "openSource", target: "created", frameId: 0, path: "/attacker/path.go" } },
      { ...valid, action: { type: "openSource", target: "frame", frameId: -1 } },
      { ...valid, action: { type: "openSource", target: "file", frameId: 0 } },
      { ...valid, action: { type: "continue" } },
      { ...valid, action: { type: "ready" } },
      { ...valid, generation: Number.NaN },
      { ...valid, revision: 1.5 },
      { ...valid, goroutineId: -1 },
      { ...valid, debugSessionId: 42 },
      { type: "ready", extra: true },
    ];
    for (const message of messages) {
      assert.doesNotThrow(() => { surface.webview.received.fire(message); });
    }
    assert.deepEqual(registry.calls, []);
    assert.deepEqual(calls, []);
    assert.deepEqual(sources, []);
    assert.deepEqual(mock.information, []);
    assert.equal(surface.webview.renders.length, 1);
  });

  for (const [target, path, line, column, frameId] of [
    ["created", "/workspace/spawn.go", 21, 0, 0],
    ["start", "/workspace/worker.go", 32, 0, 0],
    ["current", "/workspace/stopped.go", 43, 0, 0],
    ["frame", "/workspace/parent.go", 65, 7, 12],
  ] as const) {
    it(`looks up ${target} navigation in host metadata rather than trusting message paths`, () => {
      const { surface, render, sources } = attached();
      surface.webview.received.fire(action(render, { type: "openSource", target, frameId }));
      assert.equal(sources.length, 1);
      const request = sources[0]!;
      assert.deepEqual(
        { path: request.path, line: request.line, column: request.column },
        { path, line, column },
      );
      assert.equal(request.isCurrent(), true);
    });
  }

  it("refuses unknown frame and goroutine IDs and absent source metadata", () => {
    const { surface, render, sources, registry } = attached();
    surface.webview.received.fire(action(render, { type: "openSource", target: "frame", frameId: 999 }));
    registry.inspections.delete("debug-a");
    registry.publish();
    surface.webview.acknowledge();
    surface.webview.received.fire(action(surface.webview.latest, {
      type: "openSource", target: "frame", frameId: 11,
    }));
    registry.update({ selectedGoroutine: 999 });
    surface.webview.acknowledge();
    surface.webview.received.fire(action(surface.webview.latest, {
      type: "openSource", target: "created", frameId: 0,
    }));
    registry.update({ snapshot: undefined, selectedGoroutine: 0 });
    surface.webview.acknowledge();
    surface.webview.received.fire(action(surface.webview.latest, {
      type: "openSource", target: "current", frameId: 0,
    }));
    registry.sessions = [];
    registry.activeId = "";
    registry.publish();
    surface.webview.acknowledge();
    surface.webview.received.fire(action(surface.webview.latest, {
      type: "openSource", target: "start", frameId: 0,
    }));
    assert.deepEqual(sources, []);
    assert.equal(mock.information.length, 5);
  });

  it("navigates the selected goroutine's location rather than the current or first goroutine", () => {
    const { registry, surface, sources } = attached();
    registry.update({ selectedGoroutine: 8 });
    surface.webview.acknowledge();
    surface.webview.received.fire(action(surface.webview.latest, {
      type: "openSource", target: "created", frameId: 0,
    }));
    assert.equal(sources.length, 1);
    assert.equal(sources[0]?.path, "/workspace/main.go");
    assert.equal(sources[0]?.line, 12);
  });

  it("refuses invalid host paths and non-positive source lines", () => {
    const { surface, sources, registry } = attached();
    for (const [file, line] of [
      ["relative.go", 1], ["file:///workspace/a.go", 1], ["https://example.com/a.go", 1],
      ["//host/shared/a.go", 1], ["/workspace/a\u0000.go", 1], ["/workspace/a\n.go", 1],
      ["/workspace/a\u007f.go", 1], [`/${"a".repeat(4096)}`, 1], ["", 1],
      ["/workspace/valid.go", 0], ["/workspace/valid.go", -1],
    ] as const) {
      registry.update({
        snapshot: snapshot([goroutine(7, 0, {
          createdLoc: { file, line, function: "main.worker" },
        })]),
      });
      surface.webview.acknowledge();
      surface.webview.received.fire(action(surface.webview.latest, {
        type: "openSource", target: "created", frameId: 0,
      }));
    }
    assert.deepEqual(sources, []);
    assert.equal(mock.information.length, 11);
  });

  it("rejects a previously rendered action after the host changes but before the new render is acknowledged", () => {
    const { surface, registry, sources, render } = attached();
    registry.update({
      snapshot: snapshot([goroutine(7, 0, {
        createdLoc: { file: "/workspace/new.go", line: 89, function: "main.new" },
      })]),
    });
    surface.webview.received.fire(action(render, { type: "openSource", target: "created", frameId: 0 }));
    assert.equal(sources.length, 0);
    surface.webview.acknowledge();
    surface.webview.received.fire(action(surface.webview.latest, {
      type: "openSource", target: "created", frameId: 0,
    }));
    assert.equal(sources[0]?.path, "/workspace/new.go");
  });

  for (const change of ["snapshot", "selection", "session", "inspection", "hide", "close", "replace", "dispose"] as const) {
    it(`invalidates delayed source navigation after ${change}`, () => {
      const { surface, registry, sources, render, provider } = attached();
      surface.webview.received.fire(action(render, { type: "openSource", target: "created", frameId: 0 }));
      const request = sources[0]!;
      assert.equal(request.isCurrent(), true);
      switch (change) {
        case "snapshot": registry.update({ snapshot: snapshot([goroutine(7)]) }); break;
        case "selection": registry.update({ selectedGoroutine: 8 }); break;
        case "session":
          registry.sessions.push(session({ debugSessionId: "debug-b" }));
          registry.activeId = "debug-b";
          registry.publish();
          break;
        case "inspection":
          registry.inspections.set("debug-a", inspection());
          registry.publish();
          break;
        case "hide": surface.hide(); break;
        case "close": surface.dispose(); break;
        case "replace": provider.resolvePanel(new FakeSurface().asPanel()); break;
        case "dispose": provider.dispose(); break;
      }
      assert.equal(request.isCurrent(), false);
    });
  }

  it("invalidates a frame navigation continuation when a new inspection reuses the same frame ID", () => {
    const { surface, registry, sources, render } = attached();
    const message: WebviewMessage = { type: "openSource", target: "frame", frameId: 11 };
    surface.webview.received.fire(action(render, message));
    const previous = sources[0]!;
    assert.equal(previous.path, "/workspace/caller.go");
    assert.equal(previous.isCurrent(), true);
    const newer = inspection();
    registry.inspections.set("debug-a", {
      ...newer,
      frames: newer.frames.map((frame) =>
        frame.id === 11 ? { ...frame, file: "/workspace/new-caller.go", line: 78 } : frame),
    });
    registry.publish();
    assert.equal(previous.isCurrent(), false);
    surface.webview.acknowledge();
    surface.webview.received.fire(action(surface.webview.latest, message));
    const current = sources[1]!;
    assert.equal(current.path, "/workspace/new-caller.go");
    assert.equal(current.line, 78);
    assert.equal(current.isCurrent(), true);
    assert.equal(previous.isCurrent(), false);
  });

  it("ignores hidden-surface actions even when their former context is otherwise current", () => {
    const { surface, registry, sources, render } = attached();
    surface.hide();
    surface.webview.received.fire(action(render, { type: "refresh" }));
    surface.webview.received.fire(action(render, { type: "openSource", target: "created", frameId: 0 }));
    assert.deepEqual(registry.calls, []);
    assert.deepEqual(sources, []);
  });
});
