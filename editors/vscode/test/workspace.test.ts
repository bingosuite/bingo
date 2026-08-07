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

    const productionProcessSources = [
      readText("editors/vscode/src/extension.ts"),
      readText("editors/vscode/src/serverManager.ts"),
      readText("editors/vscode/src/serverProcess.ts"),
    ].join("\n");
    assert.doesNotMatch(productionProcessSources, /SIGKILL|process\.kill/);
    assert.match(
      readText("editors/vscode/scripts/owned-process.mjs"),
      /signalProcess\(-pid, "SIGKILL"\)/,
    );
    const smoke = readText("editors/vscode/scripts/smoke-server.mjs");
    assert.match(
      smoke,
      /failure !== undefined && child !== undefined && !childExited/,
    );
    assert.match(smoke, /terminateOwnedProcessGroup/);
    assert.match(smoke, /if \(child === undefined \|\| childExited\)/);
  });

  it("keeps target and extension-development preparation separate", () => {
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
    assert.equal(binaryLaunch?.preLaunchTask, "bingo: build spawntree");

    const extensionHost = configurations.find(
      (configuration) => configuration.type === "extensionHost",
    );
    assert.notEqual(extensionHost, undefined);
    assert.equal(
      extensionHost?.preLaunchTask,
      "bingo: prepare extension host",
    );
    assert.deepEqual(extensionHost?.args, [
      "--extensionDevelopmentPath=${workspaceFolder}/editors/vscode",
    ]);
  });

  it("defines deterministic but independent extension and target tasks", () => {
    const tasksConfig = readJSON(".vscode/tasks.json");
    const tasks = requireArray(tasksConfig.tasks).map(requireRecord);
    const extensionTask = tasks.find(
      (task) => task.label === "bingo: prepare extension host",
    );
    const targetTask = tasks.find(
      (task) => task.label === "bingo: build spawntree",
    );

    assert.notEqual(extensionTask, undefined);
    assert.equal(extensionTask?.type, "process");
    assert.equal(extensionTask?.command, "just");
    assert.deepEqual(extensionTask?.args, ["vscode-dev"]);

    assert.notEqual(targetTask, undefined);
    assert.equal(targetTask?.type, "process");
    assert.equal(targetTask?.command, "just");
    assert.deepEqual(targetTask?.args, ["build-spawntree"]);

    const justfile = readText("justfile");
    const devStart = justfile.indexOf("vscode-dev:");
    const devEnd = justfile.indexOf("\n# ARGS:", devStart);
    const devRecipe = justfile.slice(devStart, devEnd);
    const install = devRecipe.indexOf(
      "npm --prefix editors/vscode ci --ignore-scripts",
    );
    const build = devRecipe.indexOf(
      "npm --prefix editors/vscode run build",
    );
    assert.ok(install >= 0);
    assert.ok(build > install);
  });

  it("packages only the two supported native targets", () => {
    const platform = readText("editors/vscode/scripts/platform.mjs");
    const packageScript = readText("editors/vscode/scripts/package.mjs");
    const prepareScript = readText(
      "editors/vscode/scripts/prepare-binary.mjs",
    );
    const workflow = readText(".github/workflows/vscode-extension.yml");

    assert.match(platform, /"linux-x64"/);
    assert.match(platform, /"darwin-arm64"/);
    assert.doesNotMatch(platform, /win32|ia32|linux-arm64|darwin-x64/);
    assert.match(platform, /BINGO_VSCODE_TARGET/);
    assert.match(platform, /darwinCrossBuild/);
    assert.match(packageScript, /"--target"/);
    assert.match(prepareScript, /"bingonative"/);
    assert.match(prepareScript, /"codesign"/);
    assert.match(prepareScript, /normalizeMachOUUID/);
    assert.match(workflow, /BINGO_VSCODE_TARGET: \$\{\{ matrix\.target \}\}/);
    assert.match(workflow, /runner: macos-15/);
    assert.doesNotMatch(workflow, /runner: macos-14(?:\s|$)/);
    assert.match(workflow, /test "\$\(uname -m\)" = "\$\{\{ matrix\.unamearch \}\}"/);
    assert.match(workflow, /runner\.arch == 'ARM64'/);
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
