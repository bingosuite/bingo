import { spawnSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import process from "node:process";
import { fileURLToPath, URL } from "node:url";

mkdirSync(new URL("../../../dist/", import.meta.url), { recursive: true });

const vsce = fileURLToPath(
  new URL("../node_modules/@vscode/vsce/vsce", import.meta.url),
);
const result = spawnSync(
  process.execPath,
  [
    vsce,
    "package",
    "--no-dependencies",
    "--out",
    "../../dist/bingo.vsix",
  ],
  {
    env: {
      ...process.env,
      SOURCE_DATE_EPOCH: "315532800",
    },
    stdio: "inherit",
  },
);

if (result.error !== undefined) {
  throw result.error;
}
if (result.status !== 0) {
  throw new Error(`vsce exited with status ${String(result.status)}`);
}
