import { join } from "node:path";
import process from "node:process";

import * as vscode from "vscode";

import { resolveBundledBinary } from "./binary.js";
import {
  ConfigurationError,
  resolveServerConfiguration,
  validateBingoConfiguration,
} from "./configuration.js";
import { probeBingoHealth } from "./health.js";
import {
  defaultDelay,
  ServerManager,
  ServerManagerError,
} from "./serverManager.js";
import { spawnDetachedServer } from "./serverProcess.js";
import {
  concurrencyViewId,
  ConcurrencyViewProvider,
  copySnapshot,
} from "./concurrencyView.js";
import type { ConcurrencyViewModel } from "./model.js";
import { SessionRegistry } from "./registry.js";
import { decodeSessionAnnouncement } from "./sessionEvent.js";

const debugType = "bingo";

export interface BingoExtensionAPI {
  readonly version: 1;
  getConcurrencyState(): ConcurrencyViewModel;
  getLastRenderedRevision(): number;
  getConcurrencyViewStatus(): {
    readonly resolved: boolean;
    readonly ready: boolean;
  };
}

class BingoDebugConfigurationProvider
  implements vscode.DebugConfigurationProvider
{
  public resolveDebugConfigurationWithSubstitutedVariables(
    _folder: vscode.WorkspaceFolder | undefined,
    config: vscode.DebugConfiguration,
  ): vscode.DebugConfiguration | undefined {
    try {
      const validated = validateBingoConfiguration(config);
      return {
        ...config,
        serverMode: validated.server.mode,
        managementHost: validated.server.managementEndpoint.host,
        managementPort: validated.server.managementEndpoint.port,
        dapHost: validated.endpoint.host,
        dapPort: validated.endpoint.port,
        serverReadyTimeoutMs: validated.server.readyTimeoutMs,
        managedIdleTimeoutMs: validated.server.idleTimeoutMs,
      };
    } catch (error: unknown) {
      const message =
        error instanceof ConfigurationError
          ? error.message
          : `invalid bingo debug configuration: ${String(error)}`;
      void vscode.window.showErrorMessage(message);
      return undefined;
    }
  }
}

class BingoDebugAdapterDescriptorFactory
  implements vscode.DebugAdapterDescriptorFactory
{
  public constructor(
    private readonly manager: ServerManager,
    private readonly output: vscode.OutputChannel,
  ) {}

  public createDebugAdapterDescriptor(
    session: vscode.DebugSession,
  ): vscode.ProviderResult<vscode.DebugAdapterDescriptor> {
    return this.createDescriptor(session);
  }

  private async createDescriptor(
    session: vscode.DebugSession,
  ): Promise<vscode.DebugAdapterDescriptor> {
    const { endpoint, server } = validateBingoConfiguration(
      session.configuration,
    );
    try {
      await this.manager.ensureServer(server);
    } catch (error: unknown) {
      const message =
        error instanceof ServerManagerError
          ? error.message
          : `cannot prepare bingo server: ${String(error)}`;
      this.output.appendLine(message);
      void vscode.window.showErrorMessage(message);
      throw error;
    }
    return new vscode.DebugAdapterServer(endpoint.port, endpoint.host);
  }
}

export function activate(context: vscode.ExtensionContext): BingoExtensionAPI {
  const output = vscode.window.createOutputChannel("bingo Server");
  const registry = new SessionRegistry();
  const concurrencyView = new ConcurrencyViewProvider(
    context.extensionUri,
    registry,
  );
  const status = vscode.window.createStatusBarItem(
    vscode.StatusBarAlignment.Left,
    10,
  );
  status.command = "bingo.concurrency.focus";
  status.name = "Bingo Concurrency";
  let autoRevealed = false;
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

  context.subscriptions.push(
    output,
    status,
    registry,
    concurrencyView,
    { dispose: unsubscribeStatus },
    {
      dispose(): void {
        manager.dispose();
      },
    },
    vscode.debug.registerDebugConfigurationProvider(
      debugType,
      new BingoDebugConfigurationProvider(),
    ),
    vscode.debug.registerDebugAdapterDescriptorFactory(
      debugType,
      new BingoDebugAdapterDescriptorFactory(manager, output),
    ),
    vscode.window.registerWebviewViewProvider(
      concurrencyViewId,
      concurrencyView,
      { webviewOptions: { retainContextWhenHidden: false } },
    ),
    vscode.debug.onDidStartDebugSession((session) => {
      if (session.type === debugType) {
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
      if (
        added &&
        !autoRevealed &&
        vscode.workspace
          .getConfiguration("bingo.concurrency")
          .get<boolean>("autoReveal", true)
      ) {
        autoRevealed = true;
        void vscode.commands.executeCommand(`${concurrencyViewId}.focus`);
      }
    }),
    vscode.debug.onDidChangeActiveDebugSession((session) => {
      if (session?.type === debugType) {
        registry.select(session.id);
      }
    }),
    vscode.debug.onDidTerminateDebugSession((session) => {
      registry.remove(session.id);
    }),
    vscode.commands.registerCommand("bingo.concurrency.refresh", () => {
      registry.refresh();
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
        await vscode.commands.executeCommand(`${concurrencyViewId}.focus`);
      }
    }),
    vscode.commands.registerCommand("bingo.concurrency.fit", async () => {
      await vscode.commands.executeCommand(`${concurrencyViewId}.focus`);
      concurrencyView.fit();
    }),
    vscode.commands.registerCommand("bingo.concurrency.copySnapshot", () =>
      copySnapshot(registry),
    ),
  );
  return {
    version: 1,
    getConcurrencyState: () => registry.viewModel,
    getLastRenderedRevision: () => concurrencyView.lastRenderedRevision,
    getConcurrencyViewStatus: () => concurrencyView.status,
  };
}
