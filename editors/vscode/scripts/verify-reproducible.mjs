import { spawnSync } from "node:child_process";
import { log } from "node:console";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import process from "node:process";
import { fileURLToPath, URL } from "node:url";

import { currentTarget } from "./platform.mjs";

const packageScript = fileURLToPath(
  new URL("./package.mjs", import.meta.url),
);
const target = currentTarget();
const vsix = fileURLToPath(
  new URL(`../../../dist/bingo-${target}.vsix`, import.meta.url),
);
const binary = join(
  fileURLToPath(new URL("../", import.meta.url)),
  "bin",
  "bingo",
);

const first = packageAndHash();
const second = packageAndHash();

if (first.vsix !== second.vsix) {
  throw new Error(
    `VSIX is not reproducible: ${first.vsix} != ${second.vsix}`,
  );
}
if (first.binary !== second.binary) {
  throw new Error(
    `bundled binary is not reproducible: ${first.binary} != ${second.binary}`,
  );
}

log(`Reproducible binary (${target}): ${first.binary}`);
log(`Reproducible VSIX (${target}): ${first.vsix}`);

function packageAndHash() {
  const result = spawnSync(process.execPath, [packageScript], {
    stdio: "inherit",
  });
  if (result.error !== undefined) {
    throw result.error;
  }
  if (result.status !== 0) {
    throw new Error(`package script exited with status ${String(result.status)}`);
  }

  return {
    vsix: hash(vsix),
    binary: hash(binary),
  };
}

function hash(path) {
  return createHash("sha256").update(readFileSync(path)).digest("hex");
}
