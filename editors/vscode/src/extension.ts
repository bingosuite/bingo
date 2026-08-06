import * as vscode from "vscode";

import {
  ConfigurationError,
  validateBingoConfiguration,
} from "./configuration.js";

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
        dapHost: validated.endpoint.host,
        dapPort: validated.endpoint.port,
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
  public createDebugAdapterDescriptor(
    session: vscode.DebugSession,
  ): vscode.DebugAdapterDescriptor {
    const { endpoint } = validateBingoConfiguration(session.configuration);
    return new vscode.DebugAdapterServer(endpoint.port, endpoint.host);
  }
}

export function activate(context: vscode.ExtensionContext): void {
  context.subscriptions.push(
    vscode.debug.registerDebugConfigurationProvider(
      debugType,
      new BingoDebugConfigurationProvider(),
    ),
    vscode.debug.registerDebugAdapterDescriptorFactory(
      debugType,
      new BingoDebugAdapterDescriptorFactory(),
    ),
  );
}
