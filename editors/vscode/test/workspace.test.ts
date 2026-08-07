import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it } from "node:test";

type JsonRecord = Record<string, unknown>;

const repositoryRoot = resolve(process.cwd(), "../..");

describe("repository VS Code integration", () => {
  it("keeps Go tooling as a recommendation, not an extension dependency", () => {
    const manifest = readJSON("editors/vscode/package.json");
    const workspaceExtensions = readJSON(".vscode/extensions.json");
    const recommendations = requireArray(workspaceExtensions.recommendations);
    const extensionDependencies =
      manifest.extensionDependencies === undefined
        ? []
        : requireArray(manifest.extensionDependencies);

    assert.ok(recommendations.includes("golang.go"));
    assert.equal(extensionDependencies.includes("golang.go"), false);
  });

  it("keeps runtime and package scripts independent of Delve", () => {
    const manifest = readJSON("editors/vscode/package.json");
    const sources = [
      JSON.stringify(requireRecord(manifest.scripts)),
      readText("editors/vscode/src/configuration.ts"),
      readText("editors/vscode/src/extension.ts"),
      readText("editors/vscode/src/serverManager.ts"),
      readText("editors/vscode/src/serverProcess.ts"),
      readText("editors/vscode/scripts/clean.mjs"),
      readText("editors/vscode/scripts/package.mjs"),
      readText("editors/vscode/scripts/prepare-binary.mjs"),
      readText("editors/vscode/scripts/verify-reproducible.mjs"),
    ].join("\n");

    assert.doesNotMatch(sources, /\bdlv\b/i);
    assert.doesNotMatch(sources, /getExtension\s*\(\s*["']golang\.go/);
    assert.doesNotMatch(sources, /["']type["']\s*:\s*["']go["']/);
  });

  it("uses only bingo product debugging and prepares extension development", () => {
    const launch = readJSON(".vscode/launch.json");
    const configurations = requireArray(launch.configurations).map(requireRecord);

    assert.ok(configurations.length > 0);
    assert.equal(
      configurations.some((configuration) => configuration.type === "go"),
      false,
    );
    const bingoConfigurations = configurations.filter(
      (configuration) => configuration.type === "bingo",
    );
    assert.ok(bingoConfigurations.length > 0);
    for (const configuration of bingoConfigurations) {
      assert.equal(configuration.type, "bingo");
      assert.equal("mode" in configuration, false);
      assert.equal("debugServer" in configuration, false);
      assertLifecycleDefaults(configuration);
    }

    const binaryLaunch = bingoConfigurations.find(
      (configuration) => configuration.request === "launch",
    );
    assert.notEqual(binaryLaunch, undefined);
    assert.equal(binaryLaunch?.preLaunchTask, "bingo: prepare F5");

    const extensionHost = configurations.find(
      (configuration) => configuration.type === "extensionHost",
    );
    assert.notEqual(extensionHost, undefined);
    assert.equal(extensionHost?.preLaunchTask, "bingo: prepare F5");
    assert.deepEqual(extensionHost?.args, [
      "--extensionDevelopmentPath=${workspaceFolder}/editors/vscode",
    ]);
  });

  it("defines the deterministic extension and target preparation task", () => {
    const tasksConfig = readJSON(".vscode/tasks.json");
    const tasks = requireArray(tasksConfig.tasks).map(requireRecord);
    const buildTask = tasks.find(
      (task) => task.label === "bingo: prepare F5",
    );

    assert.notEqual(buildTask, undefined);
    assert.equal(buildTask?.type, "process");
    assert.equal(buildTask?.command, "just");
    assert.deepEqual(buildTask?.args, ["vscode-dev"]);
  });

  it("packages only the two supported native targets", () => {
    const platform = readText("editors/vscode/scripts/platform.mjs");
    const packageScript = readText("editors/vscode/scripts/package.mjs");
    const prepareScript = readText(
      "editors/vscode/scripts/prepare-binary.mjs",
    );

    assert.match(platform, /"linux-x64"/);
    assert.match(platform, /"darwin-arm64"/);
    assert.doesNotMatch(platform, /win32|ia32|linux-arm64|darwin-x64/);
    assert.match(packageScript, /"--target"/);
    assert.match(prepareScript, /"bingonative"/);
    assert.match(prepareScript, /"codesign"/);
    assert.match(prepareScript, /normalizeMachOUUID/);
  });
});

function assertLifecycleDefaults(record: JsonRecord): void {
  assert.equal(record.serverMode, "auto");
  assert.equal(record.managementHost, "127.0.0.1");
  assert.equal(record.managementPort, 6060);
  assert.equal(record.dapHost, "127.0.0.1");
  assert.equal(record.dapPort, 4711);
  assert.equal(record.serverReadyTimeoutMs, 5000);
  assert.equal(record.managedIdleTimeoutMs, 30000);
}

function readJSON(path: string): JsonRecord {
  return requireRecord(
    JSON.parse(readText(path)) as unknown,
  );
}

function readText(path: string): string {
  return readFileSync(resolve(repositoryRoot, path), "utf8");
}

function requireRecord(value: unknown): JsonRecord {
  assert.equal(typeof value, "object");
  assert.notEqual(value, null);
  assert.equal(Array.isArray(value), false);
  return value as JsonRecord;
}

function requireArray(value: unknown): unknown[] {
  assert.ok(Array.isArray(value));
  return value;
}
