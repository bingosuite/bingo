import * as vscode from "vscode";

import {
  ConfigurationError,
  resolveServerConfiguration,
  validateBingoConfiguration,
} from "./configuration.js";
import {
  goPackageConfiguration,
  needsGoPackageConfiguration,
  type GoPackageContext,
} from "./quickStart.js";
import { type ServerManager, ServerManagerError } from "./serverManager.js";

export class BingoDebugConfigurationProvider
  implements vscode.DebugConfigurationProvider, vscode.Disposable
{
  readonly #pending = new Set<AbortController>();
  #disposed = false;

  public constructor(
    private readonly manager: ServerManager,
    private readonly output: vscode.OutputChannel,
  ) {}

  public async provideDebugConfigurations(
    folder: vscode.WorkspaceFolder | undefined,
    token?: vscode.CancellationToken,
  ): Promise<vscode.DebugConfiguration[]> {
    const config = await this.#defaultConfiguration(folder, token);
    return config === undefined ? [] : [config];
  }

  public resolveDebugConfiguration(
    folder: vscode.WorkspaceFolder | undefined,
    config: vscode.DebugConfiguration,
    token?: vscode.CancellationToken,
  ): vscode.ProviderResult<vscode.DebugConfiguration> {
    if (this.#cancelled(token)) {
      return undefined;
    }
    return needsGoPackageConfiguration(config)
      ? this.#defaultConfiguration(folder, token)
      : config;
  }

  public async resolveDebugConfigurationWithSubstitutedVariables(
    _folder: vscode.WorkspaceFolder | undefined,
    config: vscode.DebugConfiguration,
    token?: vscode.CancellationToken,
  ): Promise<vscode.DebugConfiguration | undefined> {
    if (this.#cancelled(token)) {
      return undefined;
    }
    const controller = new AbortController();
    const cancellation = token?.onCancellationRequested(() => { controller.abort(); });
    if (token?.isCancellationRequested === true) {
      controller.abort();
    }
    this.#pending.add(controller);
    try {
      this.#requireTrust();
      const validated = validateBingoConfiguration(config);
      this.manager.validateConfiguration(validated.server);
      // This hook has VS Code's startup cancellation token; the descriptor
      // factory does not. Never prepare a server after this request is cancelled.
      if (validated.server.mode === "auto") {
        await vscode.window.withProgress(
          {
            location: vscode.ProgressLocation.Notification,
            title: "Preparing bingo debugger",
            cancellable: true,
          },
          async (_progress, progressToken) => {
            const progressCancellation = progressToken.onCancellationRequested(() => {
              controller.abort();
            });
            try {
              if (progressToken.isCancellationRequested || token?.isCancellationRequested === true) {
                controller.abort();
              }
              await this.manager.ensureServer(validated.server, controller.signal);
            } finally {
              progressCancellation.dispose();
            }
          },
        );
      } else {
        await this.manager.ensureServer(validated.server, controller.signal);
      }
      if (controller.signal.aborted || this.#disposed) {
        return undefined;
      }
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
      if (controller.signal.aborted ||
        (error instanceof ServerManagerError && error.code === "cancelled")) {
        this.output.appendLine("bingo startup cancelled");
      } else {
        this.#reportError(error);
      }
      return undefined;
    } finally {
      cancellation?.dispose();
      this.#pending.delete(controller);
    }
  }

  public async debugGoPackage(): Promise<boolean> {
    const active = vscode.window.activeTextEditor?.document;
    const folder = active === undefined
      ? vscode.workspace.workspaceFolders?.length === 1
        ? vscode.workspace.workspaceFolders[0]
        : undefined
      : vscode.workspace.getWorkspaceFolder(active.uri);
    const config = await this.#defaultConfiguration(folder);
    if (config === undefined || this.#disposed) {
      return false;
    }
    try {
      return await vscode.debug.startDebugging(folder, config);
    } catch (error: unknown) {
      this.#reportError(error);
      return false;
    }
  }

  public dispose(): void {
    this.#disposed = true;
    for (const controller of this.#pending) {
      controller.abort();
    }
    this.#pending.clear();
  }

  async #defaultConfiguration(
    folder: vscode.WorkspaceFolder | undefined,
    token?: vscode.CancellationToken,
  ): Promise<vscode.DebugConfiguration | undefined> {
    if (this.#cancelled(token)) {
      return undefined;
    }
    try {
      this.#requireTrust();
      this.manager.validateConfiguration(resolveServerConfiguration({}));
      const config = await goPackageConfiguration(packageContext(folder));
      return this.#cancelled(token) ? undefined : config;
    } catch (error: unknown) {
      if (!this.#cancelled(token)) {
        this.#reportError(error);
      }
      return undefined;
    }
  }

  #cancelled(token?: vscode.CancellationToken): boolean {
    return this.#disposed || token?.isCancellationRequested === true;
  }

  #requireTrust(): void {
    if (!vscode.workspace.isTrusted) {
      throw new ConfigurationError("Trust this workspace before launching native Go debugging.");
    }
  }

  #reportError(error: unknown): void {
    const message = error instanceof Error ? error.message : String(error);
    this.output.appendLine(message);
    void vscode.window.showErrorMessage(message, "Show Logs").then((action) => {
      if (action === "Show Logs") {
        this.output.show(true);
      }
    });
  }
}

export class BingoDebugAdapterDescriptorFactory
  implements vscode.DebugAdapterDescriptorFactory
{
  public createDebugAdapterDescriptor(
    session: vscode.DebugSession,
  ): vscode.DebugAdapterDescriptor {
    const { endpoint } = validateBingoConfiguration(session.configuration);
    return new vscode.DebugAdapterServer(endpoint.port, endpoint.host);
  }
}

function packageContext(folder: vscode.WorkspaceFolder | undefined): GoPackageContext {
  const document = vscode.window.activeTextEditor?.document;
  return {
    ...(document === undefined ? {} : {
      activeDocument: {
        scheme: document.uri.scheme,
        fsPath: document.uri.fsPath,
        languageId: document.languageId,
        isUntitled: document.isUntitled,
      },
    }),
    ...(folder === undefined ? {} : { folder: folder.uri }),
    workspaceFolders: (vscode.workspace.workspaceFolders ?? []).map((item) => item.uri),
  };
}
