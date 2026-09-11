import { randomBytes } from "node:crypto";
import { mkdirSync, rmSync } from "node:fs";
import { join } from "node:path";
import process from "node:process";
import { fileURLToPath, URL } from "node:url";

import { runTests } from "@vscode/test-electron";

const extensionRoot = fileURLToPath(new URL("../", import.meta.url));
const repositoryRoot = fileURLToPath(new URL("../../../", import.meta.url));
const runID = randomBytes(16).toString("hex");
const scratch = join(
  repositoryRoot,
  "dist",
  `.vscode-integration-${runID}`,
);
const profile = join(
  repositoryRoot,
  "dist",
  // VS Code places a Unix socket here; long worktree paths exhaust its 103-byte
  // path limit. Exclusive mkdir below fails safely on a random-name collision.
  runID.slice(0, 11),
);
const extensions = join(
  repositoryRoot,
  "dist",
  `.ve-${runID}`,
);
rmSync(scratch, { force: true, recursive: true });
mkdirSync(scratch, { recursive: true, mode: 0o700 });
rmSync(extensions, { force: true, recursive: true });
mkdirSync(profile, { recursive: true, mode: 0o700 });
mkdirSync(extensions, { recursive: true, mode: 0o700 });
// The unpredictable, mode-0700 directory is private despite temp-variable heuristics.
process.env.TMPDIR = scratch; // NOSONAR
process.env.TMP = scratch; // NOSONAR
process.env.TEMP = scratch; // NOSONAR

try {
  await runTests({
    version: process.env.VSCODE_TEST_VERSION ?? "1.107.1",
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
      TMPDIR: scratch, // NOSONAR -- same private directory described above.
      TMP: scratch, // NOSONAR
      TEMP: scratch, // NOSONAR
    },
  });
} finally {
  rmSync(scratch, { force: true, recursive: true });
  rmSync(profile, { force: true, recursive: true });
  rmSync(extensions, { force: true, recursive: true });
}
