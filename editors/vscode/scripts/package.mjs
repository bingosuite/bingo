import { spawnSync } from "node:child_process";
import { log } from "node:console";
import { mkdirSync, rmSync } from "node:fs";
import { join } from "node:path";
import process from "node:process";
import { fileURLToPath, URL } from "node:url";

import { currentTarget } from "./platform.mjs";

const target = currentTarget();
const extensionRoot = fileURLToPath(new URL("../", import.meta.url));
const outputDirectory = fileURLToPath(
  new URL("../../../dist/", import.meta.url),
);
const outputPath = join(outputDirectory, `bingo-${target}.vsix`);
mkdirSync(outputDirectory, { recursive: true });
rmSync(outputPath, { force: true });

run(process.execPath, [fileURLToPath(new URL("./prepare-binary.mjs", import.meta.url))]);

const vsce = fileURLToPath(
  new URL("../node_modules/@vscode/vsce/vsce", import.meta.url),
);
run(
  process.execPath,
  [
    vsce,
    "package",
    "--no-dependencies",
    "--target",
    target,
    "--out",
    outputPath,
  ],
  {
    ...process.env,
    SOURCE_DATE_EPOCH: "946684800",
  },
);

log(`Packaged ${outputPath}`);

function run(command, args, env = process.env) {
  const result = spawnSync(command, args, {
    cwd: extensionRoot,
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
