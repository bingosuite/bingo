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
      readText("editors/vscode/scripts/clean.mjs"),
      readText("editors/vscode/scripts/package.mjs"),
      readText("editors/vscode/scripts/verify-reproducible.mjs"),
    ].join("\n");

    assert.doesNotMatch(sources, /\bdlv\b/i);
  });

  it("uses only bingo launch configurations and builds spawntree before F5", () => {
    const launch = readJSON(".vscode/launch.json");
    const configurations = requireArray(launch.configurations).map(requireRecord);

    assert.ok(configurations.length > 0);
    for (const configuration of configurations) {
      assert.equal(configuration.type, "bingo");
      assert.equal("mode" in configuration, false);
      assert.equal("debugServer" in configuration, false);
      assert.equal(configuration.dapHost, "localhost");
      assert.equal(configuration.dapPort, 4711);
    }

    const binaryLaunch = configurations.find(
      (configuration) => configuration.request === "launch",
    );
    assert.notEqual(binaryLaunch, undefined);
    assert.equal(binaryLaunch?.preLaunchTask, "bingo: build spawntree");
  });

  it("defines the deterministic spawntree build task used by launch.json", () => {
    const tasksConfig = readJSON(".vscode/tasks.json");
    const tasks = requireArray(tasksConfig.tasks).map(requireRecord);
    const buildTask = tasks.find(
      (task) => task.label === "bingo: build spawntree",
    );

    assert.notEqual(buildTask, undefined);
    assert.equal(buildTask?.type, "process");
    assert.equal(buildTask?.command, "just");
    assert.deepEqual(buildTask?.args, ["build-spawntree"]);
  });
});

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
