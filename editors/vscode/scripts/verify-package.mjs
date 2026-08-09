import {
  mkdirSync,
  readFileSync,
  rmSync,
  statSync,
} from "node:fs";
import { log } from "node:console";
import { join } from "node:path";
import process from "node:process";
import { fileURLToPath, URL } from "node:url";
import { spawnSync } from "node:child_process";

import { currentTarget } from "./platform.mjs";

const target = currentTarget();
const expectedExtensionVersion = "0.3.2";
const vsix = fileURLToPath(
  new URL(`../../../dist/bingo-${target}.vsix`, import.meta.url),
);
const outputDirectory = fileURLToPath(
  new URL("../../../dist/", import.meta.url),
);
const extracted = join(
  outputDirectory,
  `.verify-bingo-${target}-${String(process.pid)}`,
);
rmSync(extracted, { force: true, recursive: true });
mkdirSync(extracted, { recursive: true });

try {
  run("unzip", ["-q", vsix, "-d", extracted]);
  const entries = capture("unzip", ["-Z1", vsix])
    .split("\n")
    .filter((entry) => entry.length > 0);
  const expectedEntries = [
    "[Content_Types].xml",
    "extension.vsixmanifest",
    "extension/LICENSE.txt",
    "extension/bin/bingo",
    "extension/bin/target.json",
    "extension/dist/extension.js",
    "extension/dist/webview.js",
    "extension/media/bingo-activity.svg",
    "extension/package.json",
    "extension/readme.md",
  ].sort((left, right) => left.localeCompare(right));
  const sortedEntries = [...entries].sort((left, right) =>
    left.localeCompare(right),
  );
  if (JSON.stringify(sortedEntries) !== JSON.stringify(expectedEntries)) {
    throw new Error(
      `unexpected VSIX contents:\n${sortedEntries.join("\n")}`,
    );
  }
  const binaries = entries.filter((entry) => entry === "extension/bin/bingo");
  if (binaries.length !== 1) {
    throw new Error(
      `expected exactly one extension/bin/bingo, found ${String(binaries.length)}`,
    );
  }
  for (const required of [
    "extension/dist/extension.js",
    "extension/dist/webview.js",
    "extension/media/bingo-activity.svg",
  ]) {
    if (!entries.includes(required)) {
      throw new Error(`VSIX is missing required concurrency UI asset ${required}`);
    }
  }
  const binEntries = entries.filter((entry) =>
    entry.startsWith("extension/bin/"),
  );
  if (
    binEntries.length !== 2 ||
    !binEntries.includes("extension/bin/target.json")
  ) {
    throw new Error(`unexpected bin contents: ${binEntries.join(", ")}`);
  }

  const forbidden = entries.filter(
    (entry) =>
      entry.endsWith(".map") ||
      entry.includes("/src/") ||
      entry.includes("/test/") ||
      entry.includes("/scripts/") ||
      /(^|\/)\.env($|\.)/.test(entry) ||
      /\.(pem|key|p12|pfx)$/i.test(entry),
  );
  if (forbidden.length > 0) {
    throw new Error(`forbidden VSIX contents: ${forbidden.join(", ")}`);
  }

  const marker = JSON.parse(
    readFileSync(join(extracted, "extension", "bin", "target.json"), "utf8"),
  );
  if (marker.target !== target) {
    throw new Error(
      `package target marker is ${String(marker.target)}, expected ${target}`,
    );
  }
  const manifest = readFileSync(
    join(extracted, "extension.vsixmanifest"),
    "utf8",
  );
  if (!manifest.includes(`TargetPlatform="${target}"`)) {
    throw new Error(`VSIX manifest does not declare target ${target}`);
  }
  if (!manifest.includes(`Version="${expectedExtensionVersion}"`)) {
    throw new Error(
      `VSIX manifest does not declare version ${expectedExtensionVersion}`,
    );
  }
  const extensionPackage = JSON.parse(
    readFileSync(join(extracted, "extension", "package.json"), "utf8"),
  );
  if (extensionPackage.version !== expectedExtensionVersion) {
    throw new Error(
      `packaged extension version is ${String(extensionPackage.version)}, expected ${expectedExtensionVersion}`,
    );
  }

  const binary = join(extracted, "extension", "bin", "bingo");
  if ((statSync(binary).mode & 0o111) === 0) {
    throw new Error("packaged bingo binary is not executable");
  }
  const webviewBundle = join(extracted, "extension", "dist", "webview.js");
  if (statSync(webviewBundle).size > 250 * 1024) {
    throw new Error("packaged concurrency webview exceeds 250 KiB");
  }
  const fileOutput = capture("file", [binary]);
  if (
    target === "linux-x64" &&
    !(fileOutput.includes("ELF 64-bit") && fileOutput.includes("x86-64"))
  ) {
    throw new Error(`unexpected linux binary: ${fileOutput}`);
  }
  if (
    target === "darwin-arm64" &&
    !(fileOutput.includes("Mach-O 64-bit") && fileOutput.includes("arm64"))
  ) {
    throw new Error(`unexpected Darwin binary: ${fileOutput}`);
  }
  if (target === "darwin-arm64") {
    run("codesign", ["--verify", "--strict", binary]);
    const entitlements = capture("codesign", [
      "-d",
      "--entitlements",
      ":-",
      binary,
    ]);
    if (!entitlements.includes("com.apple.security.cs.debugger")) {
      throw new Error("packaged Darwin binary lacks debugger entitlement");
    }
  }

  log(`Verified ${target} VSIX contents and binary`);
} finally {
  rmSync(extracted, { force: true, recursive: true });
}

function run(command, args) {
  const result = spawnSync(command, args, { stdio: "inherit" });
  if (result.error !== undefined) {
    throw result.error;
  }
  if (result.status !== 0) {
    throw new Error(
      `${command} exited with status ${String(result.status)}`,
    );
  }
}

function capture(command, args) {
  const result = spawnSync(command, args, { encoding: "utf8" });
  if (result.error !== undefined) {
    throw result.error;
  }
  if (result.status !== 0) {
    throw new Error(
      `${command} exited with status ${String(result.status)}: ${result.stderr}`,
    );
  }
  return `${result.stdout}${result.stderr}`;
}
