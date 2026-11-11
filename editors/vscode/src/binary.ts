import { execFile } from "node:child_process";
import {
  chmod,
  readFile,
  stat,
} from "node:fs/promises";
import { join } from "node:path";
import { promisify } from "node:util";

import type { SupportedTarget } from "./platform.js";

export async function resolveBundledBinary(
  extensionPath: string,
  expectedTarget: SupportedTarget,
  verifySignature: (path: string) => Promise<void> = verifyDarwinSignature,
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
  if (expectedTarget === "darwin-arm64") {
    await verifySignature(binaryPath);
  }
  return binaryPath;
}

export async function verifyDarwinSignature(
  binaryPath: string,
  run: (args: readonly string[]) => Promise<string> = async (args) => {
    const { stdout } = await promisify(execFile)("/usr/bin/codesign", [...args], {
      timeout: 5000,
      maxBuffer: 64 * 1024,
    });
    return stdout;
  },
): Promise<void> {
  try {
    await run(["--verify", "--strict", binaryPath]);
    const entitlements = await run(["--display", "--entitlements", "-", "--xml", binaryPath]);
    if (!/<key>com\.apple\.security\.cs\.debugger<\/key>\s*<true\s*\/>/u.test(entitlements)) {
      throw new Error("com.apple.security.cs.debugger entitlement is missing or disabled");
    }
  } catch (error: unknown) {
    throw new Error(
      `Cannot use bundled macOS debugger ${binaryPath}: ${errorMessage(error)}. Reinstall the darwin-arm64 Bingo VSIX and reload VS Code. For source development, rebuild with scripts/build-binary.sh; do not use an unsigned go build output.`,
      { cause: error },
    );
  }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
