import {
  chmod,
  readFile,
  stat,
} from "node:fs/promises";
import { join } from "node:path";

import type { SupportedTarget } from "./platform.js";

export async function resolveBundledBinary(
  extensionPath: string,
  expectedTarget: SupportedTarget,
): Promise<string> {
  const binaryPath = join(extensionPath, "bin", "bingo");
  const markerPath = join(extensionPath, "bin", "target.json");

  let marker: unknown;
  try {
    marker = JSON.parse(await readFile(markerPath, "utf8")) as unknown;
  } catch (error: unknown) {
    throw new Error(
      `cannot read bundled bingo target marker ${markerPath}: ${errorMessage(error)}`,
    );
  }
  const markerTarget =
    typeof marker === "object" && marker !== null && "target" in marker
      ? marker.target
      : undefined;
  if (markerTarget !== expectedTarget) {
    throw new Error(
      `bundled bingo target is ${JSON.stringify(markerTarget)}, expected ${expectedTarget}`,
    );
  }

  let info;
  try {
    info = await stat(binaryPath);
  } catch (error: unknown) {
    throw new Error(
      `cannot find bundled bingo executable ${binaryPath}: ${errorMessage(error)}`,
    );
  }
  if (!info.isFile()) {
    throw new Error(`bundled bingo executable is not a file: ${binaryPath}`);
  }
  if ((info.mode & 0o111) === 0) {
    try {
      await chmod(binaryPath, 0o755);
    } catch (error: unknown) {
      throw new Error(
        `cannot make bundled bingo executable runnable ${binaryPath}: ${errorMessage(error)}`,
      );
    }
  }
  return binaryPath;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
