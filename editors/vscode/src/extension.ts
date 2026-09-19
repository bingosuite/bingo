import { join } from "node:path";
import process from "node:process";

import * as vscode from "vscode";

import { resolveBundledBinary } from "./binary.js";
import { resolveServerConfiguration } from "./configuration.js";
import {
  BingoDebugAdapterDescriptorFactory,
  BingoDebugConfigurationProvider,
} from "./debugConfiguration.js";
import { probeBingoHealth } from "./health.js";
import {
  defaultDelay,
  ServerManager,
} from "./serverManager.js";
import { spawnDetachedServer } from "./serverProcess.js";
import {
  concurrencyViewId,
  ConcurrencyViewProvider,
  ConcurrencyEditor,
  type ConcurrencyViewActions,
  copySnapshot,
} from "./concurrencyView.js";
import { DebugInspectionController } from "./inspection.js";
import { readSourceContext, SpawnSourceController, validSourcePath } from "./source.js";
import type { ConcurrencyViewModel } from "./model.js";
import { SessionRegistry } from "./registry.js";
import { decodeSessionAnnouncement } from "./sessionEvent.js";
import type { DisplayedState, TestOperation } from "./webviewTest.js";

const debugType = "bingo";

export interface BingoExtensionAPI {
  readonly version: 1;
  getConcurrencyState(): ConcurrencyViewModel;
  getLastRenderedRevision(): number;
  getConcurrencyViewStatus(): {
    readonly resolved: boolean;
    readonly ready: boolean;
    readonly visible: boolean;
  };
  getSidebarViewStatus(): { readonly resolved: boolean; readonly ready: boolean; readonly visible: boolean };
  testUI?(operation?: TestOperation, target?: number): Promise<DisplayedState>;
}

export function activate(context: vscode.ExtensionContext): BingoExtensionAPI {
  const output = vscode.window.createOutputChannel("bingo Server");
  const registry = new SessionRegistry();
  const debugSessions = new Map<string, vscode.DebugSession>();
  const inspection = new DebugInspectionController(registry, (id) =>
    debugSessions.get(id),
  );
  const source = new SpawnSourceController(registry, (location) =>
    readSourceContext(location, {
      trusted: vscode.workspace.isTrusted,
      roots: (vscode.workspace.workspaceFolders ?? [])
        .filter((folder) => folder.uri.scheme === "file")
        .map((folder) => folder.uri.fsPath),
    }),
  );
  const actions: ConcurrencyViewActions = {
    selectFrame: (frameId) => {
      inspection.selectFrame(frameId);
    },
    expandVariable: (reference) => {
      inspection.expandVariable(reference);
    },
    refreshInspection: () => {
      inspection.refresh();
    },
    refreshSource: () => {
      source.refresh();
    },
    openSource: (path, line, column, isCurrent) => {
      void openSource(path, line, column, editor.sourceColumn, isCurrent);
    },
  };
  const concurrencyView = new ConcurrencyViewProvider(
    context.extensionUri, registry, actions,
  );
  const editorView = new ConcurrencyViewProvider(
    context.extensionUri, registry, actions, context.extensionMode === vscode.ExtensionMode.Test,
  );
  const editor = new ConcurrencyEditor(editorView);
  const status = vscode.window.createStatusBarItem(
    vscode.StatusBarAlignment.Left,
    10,
  );
  status.command = "bingo.concurrency.openEditor";
  status.name = "Bingo Concurrency";
  const autoRevealEnabled = (): boolean =>
    vscode.workspace
      .getConfiguration("bingo.concurrency")
      .get<boolean>("autoReveal", true);
  const updateStatus = (): void => {
    const active = registry.activeModel();
    if (active === undefined) {
      status.hide();
      return;
    }
    const goroutines = active.snapshot?.goroutines.length ?? 0;
    const threads = active.snapshot?.threads.length ?? 0;
    status.text = `$(type-hierarchy) Bingo ${String(goroutines)}g · ${String(threads)}t`;
    status.tooltip = `${active.debugSessionName} · ${active.connection} · ${active.sessionState}`;
    status.show();
  };
  const unsubscribeStatus = registry.onChange(updateStatus);
  const manager = new ServerManager({
    probe: probeBingoHealth,
    resolveBinary: async (target) =>
      resolveBundledBinary(context.extensionPath, target),
    spawnServer: spawnDetachedServer,
    delay: defaultDelay,
    now: Date.now,
    runtime: {
      platform: process.platform,
      arch: process.arch,
    },
    logPathFor: (endpoint) =>
      Promise.resolve(
        join(
          context.globalStorageUri.fsPath,
          "server-logs",
          `bingo-${String(endpoint.port)}.log`,
        ),
      ),
    log: (message) => {
      output.appendLine(message);
    },
  });
  const configurationProvider = new BingoDebugConfigurationProvider(manager, output);

  context.subscriptions.push(
    output,
    status,
    registry,
    inspection,
    source,
    concurrencyView,
    editor,
    configurationProvider,
    { dispose: unsubscribeStatus },
    {
      dispose(): void {
        manager.dispose();
      },
    },
    vscode.debug.registerDebugConfigurationProvider(
      debugType,
      configurationProvider,
    ),
    vscode.debug.registerDebugConfigurationProvider(
      debugType,
      {
        provideDebugConfigurations: (folder, token) =>
          configurationProvider.provideDebugConfigurations(folder, token),
      },
      vscode.DebugConfigurationProviderTriggerKind.Dynamic,
    ),
    vscode.debug.registerDebugAdapterDescriptorFactory(
      debugType,
      new BingoDebugAdapterDescriptorFactory(),
    ),
    vscode.commands.registerCommand("bingo.debugGoPackage", () =>
      configurationProvider.debugGoPackage(),
    ),
    vscode.commands.registerCommand("bingo.showServerOutput", () => {
      output.show(true);
    }),
    vscode.debug.registerDebugAdapterTrackerFactory(debugType, {
      createDebugAdapterTracker(session) {
        return {
          onWillReceiveMessage(message: unknown): void {
            if (typeof message === "object" && message !== null &&
              "type" in message && message.type === "request" &&
              "command" in message &&
              (message.command === "continue" || message.command === "next" ||
                message.command === "stepIn" || message.command === "stepOut")) {
              inspection.resumed(session.id);
            }
          },
          onDidSendMessage(message: unknown): void {
            if (isDAPEvent(message, "stopped")) {
              inspection.stopped(session.id, stoppedThreadId(message));
            } else if (isDAPEvent(message, "continued")) {
              inspection.resumed(session.id);
            }
          },
        };
      },
    }),
    vscode.window.registerWebviewViewProvider(
      concurrencyViewId,
      concurrencyView,
      { webviewOptions: { retainContextWhenHidden: false } },
    ),
    vscode.debug.onDidStartDebugSession((session) => {
      if (session.type === debugType) {
        debugSessions.set(session.id, session);
        registry.select(session.id);
      }
    }),
    vscode.debug.onDidReceiveDebugSessionCustomEvent((event) => {
      let announcement;
      try {
        announcement = decodeSessionAnnouncement(event.event, event.body);
      } catch (error: unknown) {
        output.appendLine(
          `ignored invalid bingo session event: ${
            error instanceof Error ? error.message : String(error)
          }`,
        );
        return;
      }
      if (
        event.session.type !== debugType ||
        announcement === undefined
      ) {
        return;
      }
      debugSessions.set(event.session.id, event.session);
      let server;
      try {
        server = resolveServerConfiguration(event.session.configuration);
      } catch (error: unknown) {
        output.appendLine(
          `cannot start concurrency observer: ${
            error instanceof Error ? error.message : String(error)
          }`,
        );
        return;
      }
      const added = registry.add({
        debugSessionId: event.session.id,
        debugSessionName: event.session.name,
        sessionId: announcement.sessionId,
        managementEndpoint: server.managementEndpoint,
      });
      if (added) {
        editor.sessionStarted(event.session.id, autoRevealEnabled());
      }
    }),
    vscode.debug.onDidChangeActiveDebugSession((session) => {
      if (session?.type === debugType) {
        registry.select(session.id);
      }
    }),
    vscode.debug.onDidTerminateDebugSession((session) => {
      registry.remove(session.id);
      inspection.forgetSession(session.id);
      debugSessions.delete(session.id);
      editor.sessionEnded(session.id);
    }),
    vscode.commands.registerCommand("bingo.concurrency.refresh", () => {
      registry.refresh();
      source.refresh();
    }),
    vscode.commands.registerCommand("bingo.concurrency.selectSession", async () => {
      const sessions = registry.viewModel.sessions;
      const selected = await vscode.window.showQuickPick(
        sessions.map((session) => ({
          label: session.debugSessionName,
          description: session.sessionId,
          id: session.debugSessionId,
        })),
        { title: "Select Bingo Concurrency session" },
      );
      if (selected !== undefined) {
        registry.select(selected.id);
        editor.open();
      }
    }),
    vscode.commands.registerCommand("bingo.concurrency.openEditor", () => {
      editor.open();
    }),
    vscode.commands.registerCommand("bingo.concurrency.fit", () => {
      editor.open();
      editorView.fit();
    }),
    vscode.commands.registerCommand("bingo.concurrency.fitSidebar", () => {
      concurrencyView.fit();
    }),
    vscode.commands.registerCommand("bingo.concurrency.copySnapshot", () =>
      copySnapshot(registry),
    ),
  );
  return {
    version: 1,
    getConcurrencyState: () => registry.viewModel,
    getLastRenderedRevision: () => editorView.lastRenderedRevision,
    getConcurrencyViewStatus: () => editorView.status,
    getSidebarViewStatus: () => concurrencyView.status,
    ...(context.extensionMode === vscode.ExtensionMode.Test
      ? { testUI: (operation?: TestOperation, target?: number) => editorView.testUI(operation, target) }
      : {}),
  };
}

function isDAPEvent(value: unknown, event: string): boolean {
  return (
    typeof value === "object" &&
    value !== null &&
    "type" in value &&
    value.type === "event" &&
    "event" in value &&
    value.event === event
  );
}

function stoppedThreadId(value: unknown): number {
  if (
    typeof value !== "object" ||
    value === null ||
    !("body" in value) ||
    typeof value.body !== "object" ||
    value.body === null ||
    !("threadId" in value.body)
  ) {
    return 0;
  }
  const threadId = value.body.threadId;
  return typeof threadId === "number" &&
    Number.isSafeInteger(threadId) &&
    threadId > 0
    ? threadId
    : 0;
}

async function openSource(
  path: string,
  line: number,
  column: number,
  viewColumn: vscode.ViewColumn,
  isCurrent: () => boolean,
): Promise<void> {
  try {
    if (!validSourcePath(path)) {
      throw new Error("Source location is not a valid local absolute path.");
    }
    const document = await vscode.workspace.openTextDocument(
      vscode.Uri.file(path),
    );
    if (!isCurrent()) {
      return;
    }
    const position = new vscode.Position(
      Math.max(0, line - 1),
      Math.max(0, column - 1),
    );
    await vscode.window.showTextDocument(document, {
      selection: new vscode.Range(position, position),
      preserveFocus: false,
      viewColumn,
    });
  } catch (error: unknown) {
    if (!isCurrent()) {
      return;
    }
    void vscode.window.showErrorMessage(
      `Cannot open ${path}:${String(line)}: ${
        error instanceof Error ? error.message : String(error)
      }`,
    );
  }
}
