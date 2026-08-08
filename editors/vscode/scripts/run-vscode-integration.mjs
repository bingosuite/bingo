import { mkdirSync, rmSync } from "node:fs";
import { join } from "node:path";
import process from "node:process";
import { fileURLToPath, URL } from "node:url";

import { runTests } from "@vscode/test-electron";

const extensionRoot = fileURLToPath(new URL("../", import.meta.url));
const repositoryRoot = fileURLToPath(new URL("../../../", import.meta.url));
const scratch = join(
  repositoryRoot,
  "dist",
  `.vscode-integration-${String(process.pid)}`,
);
const profile = join(
  repositoryRoot,
  "dist",
  `.vu-${String(process.pid)}`,
);
const extensions = join(
  repositoryRoot,
  "dist",
  `.ve-${String(process.pid)}`,
);
rmSync(scratch, { force: true, recursive: true });
mkdirSync(scratch, { recursive: true });
rmSync(profile, { force: true, recursive: true });
rmSync(extensions, { force: true, recursive: true });
mkdirSync(profile, { recursive: true });
mkdirSync(extensions, { recursive: true });
process.env.TMPDIR = scratch;
process.env.TMP = scratch;
process.env.TEMP = scratch;

try {
  await runTests({
    version: "1.107.1",
    cachePath: join(repositoryRoot, ".vscode-test"),
    extensionDevelopmentPath: extensionRoot,
    extensionTestsPath: join(
      extensionRoot,
      "dist",
      "test",
      "vscodeIntegration.js",
    ),
    launchArgs: [
      repositoryRoot,
      `--user-data-dir=${profile}`,
      `--extensions-dir=${extensions}`,
      "--disable-workspace-trust",
      "--skip-release-notes",
      "--skip-welcome",
    ],
    extensionTestsEnv: {
      ...process.env,
      TMPDIR: scratch,
      TMP: scratch,
      TEMP: scratch,
    },
  });
} finally {
  rmSync(scratch, { force: true, recursive: true });
  rmSync(profile, { force: true, recursive: true });
  rmSync(extensions, { force: true, recursive: true });
}
