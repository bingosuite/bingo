import { createHash } from "node:crypto";
import { log } from "node:console";
import {
  chmodSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import process from "node:process";
import { fileURLToPath, URL } from "node:url";
import { spawnSync } from "node:child_process";

import { targetDetails } from "./platform.mjs";

const repositoryRoot = fileURLToPath(new URL("../../../", import.meta.url));
const extensionRoot = fileURLToPath(new URL("../", import.meta.url));
const binDirectory = join(extensionRoot, "bin");
const binaryPath = join(binDirectory, "bingo");
const requestedTarget = process.env.BINGO_VSCODE_TARGET;
const target = targetDetails(requestedTarget);

mkdirSync(binDirectory, { recursive: true });

run("bash", [
  join(repositoryRoot, "scripts", "build-binary.sh"),
  "bingo",
  binaryPath,
  target.goos,
  target.goarch,
], {
  ...process.env,
  BINGO_REPRODUCIBLE: "1",
});

chmodSync(binaryPath, 0o755);
writeFileSync(
  join(binDirectory, "target.json"),
  `${JSON.stringify({ target: target.name })}\n`,
  { mode: 0o644 },
);

const hash = createHash("sha256")
  .update(readFileSync(binaryPath))
  .digest("hex");
log(`Prepared ${target.name} bingo binary: ${hash}`);

function run(command, args, env) {
  const result = spawnSync(command, args, {
    cwd: repositoryRoot,
    env,
    stdio: "inherit",
  });
  if (result.error !== undefined) {
    throw result.error;
  }
  if (result.status !== 0) {
    throw new Error(
      `${command} exited with status ${String(result.status)}`,
    );
  }
}
