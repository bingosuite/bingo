import { createHash } from "node:crypto";
import { log } from "node:console";
import {
  chmodSync,
  mkdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import process from "node:process";
import { fileURLToPath, URL } from "node:url";
import { spawnSync } from "node:child_process";

import { normalizeMachOUUID } from "./normalize-mach-o-uuid.mjs";
import { targetDetails } from "./platform.mjs";

const repositoryRoot = fileURLToPath(new URL("../../../", import.meta.url));
const extensionRoot = fileURLToPath(new URL("../", import.meta.url));
const binDirectory = join(extensionRoot, "bin");
const binaryPath = join(binDirectory, "bingo");
const requestedTarget = process.env.BINGO_VSCODE_TARGET;
const target = targetDetails(requestedTarget);

rmSync(binDirectory, { force: true, recursive: true });
mkdirSync(binDirectory, { recursive: true });

const buildArguments = ["build"];
if (target.name === "darwin-arm64") {
  buildArguments.push("-tags", "bingonative");
}
buildArguments.push(
  "-trimpath",
  "-buildvcs=false",
  "-ldflags=-buildid=",
  "-o",
  binaryPath,
  "./cmd/bingo",
);
run("go", buildArguments, {
  ...process.env,
  CGO_ENABLED: target.name === "darwin-arm64" ? "1" : "0",
  GOOS: target.goos,
  GOARCH: target.goarch,
});

if (target.name === "darwin-arm64") {
  normalizeMachOUUID(binaryPath);
  run(
    "codesign",
    [
      "--sign",
      "-",
      "--entitlements",
      "entitlements.plist",
      "--force",
      "--timestamp=none",
      binaryPath,
    ],
    process.env,
  );
  run("codesign", ["--verify", "--strict", binaryPath], process.env);
}

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
