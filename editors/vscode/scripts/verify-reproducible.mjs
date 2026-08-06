import { spawnSync } from "node:child_process";
import { log } from "node:console";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import process from "node:process";
import { fileURLToPath, URL } from "node:url";

const packageScript = fileURLToPath(
  new URL("./package.mjs", import.meta.url),
);
const vsix = new URL("../../../dist/bingo.vsix", import.meta.url);

const first = packageAndHash();
const second = packageAndHash();

if (first !== second) {
  throw new Error(`VSIX is not reproducible: ${first} != ${second}`);
}

log(`Reproducible VSIX: ${first}`);

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

  return createHash("sha256").update(readFileSync(vsix)).digest("hex");
}
