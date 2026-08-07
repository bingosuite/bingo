import { join } from "node:path";
import process from "node:process";

import * as vscode from "vscode";

import { resolveBundledBinary } from "./binary.js";
import {
  ConfigurationError,
  validateBingoConfiguration,
} from "./configuration.js";
import { probeBingoHealth } from "./health.js";
import {
  defaultDelay,
  ServerManager,
  ServerManagerError,
} from "./serverManager.js";
import { spawnDetachedServer } from "./serverProcess.js";

const debugType = "bingo";

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

export function activate(context: vscode.ExtensionContext): void {
  const output = vscode.window.createOutputChannel("bingo Server");
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
  );
}
