import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it } from "node:test";
import { pathToFileURL } from "node:url";

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
      readText("editors/vscode/src/quickStart.ts"),
      readText("editors/vscode/src/debugConfiguration.ts"),
      readText("editors/vscode/src/extension.ts"),
      readText("editors/vscode/src/debugConfiguration.ts"),
      readText("editors/vscode/src/quickStart.ts"),
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

  it("exposes the normal bingo debug choices", () => {
    const launch = readJSON(".vscode/launch.json");
    const configurations = requireArray(launch.configurations).map(requireRecord);
    const serialized = JSON.stringify(configurations);

    assert.equal(configurations.length, 3);
    assert.deepEqual(
      configurations.map((configuration) => configuration.name),
      [
        "bingo: Debug example",
        "bingo: Debug spawntree telemetry demo",
        "bingo: Join running session",
      ],
    );
    for (const configuration of configurations) {
      assert.equal(configuration.type, "bingo");
      assert.equal("debugServer" in configuration, false);
      assert.equal("preLaunchTask" in configuration, false);
      for (const field of [
        "serverMode", "managementHost", "managementPort", "dapHost",
        "dapPort", "serverReadyTimeoutMs", "managedIdleTimeoutMs",
      ]) {
        assert.equal(field in configuration, false);
      }
    }
    assert.doesNotMatch(serialized, /extensionHost|Run bingo extension/);
    assert.doesNotMatch(serialized, /"type":"go"/);
    assert.doesNotMatch(serialized, /\bdlv\b/i);

    const sourceLaunches = configurations.filter(
      (configuration) => configuration.request === "launch",
    );
    assert.equal(sourceLaunches.length, 2);
    for (const launch of sourceLaunches) {
      assert.equal(launch.mode, "debug");
      assert.equal("stopOnEntry" in launch, false);
    }
    const exampleLaunch = sourceLaunches.find(
      (configuration) =>
        configuration.name === "bingo: Debug example",
    );
    assert.notEqual(exampleLaunch, undefined);
    assert.equal(
      exampleLaunch?.program,
      "${workspaceFolder}/examples/${input:bingoExample}",
    );

    const spawntreeLaunch = sourceLaunches.find(
      (configuration) =>
        configuration.name === "bingo: Debug spawntree telemetry demo",
    );
    assert.notEqual(spawntreeLaunch, undefined);
    assert.equal(spawntreeLaunch?.program, "${workspaceFolder}/examples/spawntree");

    const inputs = requireArray(launch.inputs).map(requireRecord);
    assert.equal(inputs.length, 2);
    const examplePicker = inputs.find((input) => input.id === "bingoExample");
    assert.notEqual(examplePicker, undefined);
    assert.equal(examplePicker?.type, "pickString");
    assert.equal(examplePicker?.default, "level1-loop");
    assert.deepEqual(examplePicker?.options, [
      "level1-loop",
      "level2-channel",
      "level3-worker-pool",
      "level4-pipeline",
      "level5-workflow",
    ]);

    const sessionJoin = configurations.find(
      (configuration) => configuration.request === "attach",
    );
    assert.notEqual(sessionJoin, undefined);
    assert.equal(sessionJoin?.name, "bingo: Join running session");
    assert.equal(sessionJoin?.session, "${input:bingoSession}");
    assert.equal("preLaunchTask" in (sessionJoin ?? {}), false);
  });

  it("retains optional terminal binary tasks without requiring them for F5", () => {
    const tasksConfig = readJSON(".vscode/tasks.json");
    const tasks = requireArray(tasksConfig.tasks).map(requireRecord);

    assert.equal(tasks.length, 2);
    const targetTask = tasks.find((task) => task.label === "bingo: build examples");
    assert.notEqual(targetTask, undefined);
    assert.equal(targetTask?.type, "process");
    assert.equal(targetTask?.command, "just");
    assert.deepEqual(targetTask?.args, ["build-examples"]);

    const spawntreeTask = tasks.find((task) => task.label === "bingo: build spawntree");
    assert.notEqual(spawntreeTask, undefined);
    assert.equal(spawntreeTask?.type, "process");
    assert.equal(spawntreeTask?.command, "just");
    assert.deepEqual(spawntreeTask?.args, ["build-spawntree"]);

    const launch = readJSON(".vscode/launch.json");
    const configurations = requireArray(launch.configurations).map(requireRecord);
    for (const sourceLaunch of configurations.filter(
      (configuration) => configuration.request === "launch",
    )) {
      assert.equal(sourceLaunch.mode, "debug");
      assert.equal(sourceLaunch.preLaunchTask, undefined);
    }

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
    const buildScript = readText("scripts/build-binary.sh");
    const workflow = readText(".github/workflows/vscode-extension.yml");

    assert.match(platform, /"linux-x64"/);
    assert.match(platform, /"darwin-arm64"/);
    assert.doesNotMatch(platform, /win32|ia32|linux-arm64|darwin-x64/);
    assert.match(platform, /BINGO_VSCODE_TARGET/);
    assert.doesNotMatch(platform, /darwinCrossBuild/);
    assert.match(platform, /linuxCrossBuild/);
    assert.match(packageScript, /"--target"/);
    assert.match(prepareScript, /run\("bash", \[/);
    assert.match(prepareScript, /"scripts", "build-binary\.sh"/);
    assert.match(prepareScript, /target\.goos,\s+target\.goarch/);
    assert.match(prepareScript, /BINGO_REPRODUCIBLE: "1"/);
    assert.doesNotMatch(prepareScript, /run\("go"|normalizeMachOUUID/);
    assert.match(buildScript, /bingonative/);
    assert.match(buildScript, /codesign/);
    assert.match(buildScript, /normalizeMachOUUID/);
    assert.ok(buildScript.indexOf("normalizeMachOUUID") < buildScript.indexOf("codesign"));
    assert.match(workflow, /BINGO_VSCODE_TARGET: \$\{\{ matrix\.target \}\}/);
    assert.match(workflow, /runner: macos-15/);
    assert.doesNotMatch(workflow, /runner: macos-14(?:\s|$)/);
    assert.match(workflow, /test "\$\(uname -m\)" = "\$\{\{ matrix\.unamearch \}\}"/);
    assert.match(workflow, /runner\.arch == 'ARM64'/);
  });

  it("rejects unsupported package builders before preparation while keeping Linux cross-builds", () => {
    const platformModule = pathToFileURL(
      resolve(repositoryRoot, "editors/vscode/scripts/platform.mjs"),
    ).href;
    for (const [platform, arch, target, allowed] of [
      ["darwin", "arm64", "darwin-arm64", true],
      ["linux", "x64", "linux-x64", true],
      ["darwin", "arm64", "linux-x64", true],
      ["darwin", "x64", "darwin-arm64", false],
      ["darwin", "x64", "linux-x64", false],
      ["linux", "x64", "darwin-arm64", false],
      ["linux", "arm64", "linux-x64", false],
      ["win32", "x64", "linux-x64", false],
    ] as const) {
      const result = spawnSync(process.execPath, ["--input-type=module", "-e", `
        Object.defineProperty(process, "platform", { value: ${JSON.stringify(platform)} });
        Object.defineProperty(process, "arch", { value: ${JSON.stringify(arch)} });
        const { targetDetails } = await import(${JSON.stringify(platformModule)});
        console.log(JSON.stringify(targetDetails()));
      `], {
        encoding: "utf8",
        env: { ...process.env, BINGO_VSCODE_TARGET: target },
      });
      assert.ifError(result.error);
      assert.equal(result.status === 0, allowed, `${platform}/${arch} -> ${target}: ${result.stderr}`);
      if (allowed) {
        const details = requireRecord(JSON.parse(result.stdout) as unknown);
        assert.equal(details.name, target);
        assert.equal(details.goos, target === "linux-x64" ? "linux" : "darwin");
        assert.equal(details.goarch, target === "linux-x64" ? "amd64" : "arm64");
      } else {
        assert.match(result.stderr, /cannot be packaged/);
      }
    }
  });

  it("keeps floor/current Electron coverage unprivileged and runtime compatible with Node18", () => {
    const workflow = readText(".github/workflows/vscode-extension.yml");
    assert.match(workflow, /version: \["1\.85\.2", "1\.137\.0"\]/);
    assert.match(workflow, /permissions:\s+contents: read/);
    assert.doesNotMatch(workflow, /pull_request_target|contents: write|secrets\./);
    const manifest = readJSON("editors/vscode/package.json");
    assert.match(String(requireRecord(manifest.scripts).build), /--target=node18/);
  });

  it("carries the CI matrix version through the runner into the Electron runtime assertion", () => {
    const workflow = readText(".github/workflows/vscode-extension.yml");
    const runner = readText("editors/vscode/scripts/run-vscode-integration.mjs");
    const integration = readText("editors/vscode/test/vscodeIntegration.ts");
    const workflowKey = /^\s+([A-Z_]+): \$\{\{ matrix\.version \}\}$/m.exec(workflow)?.[1];
    const runnerKey = /const version = process\.env\.([A-Z_]+) \?\? "1\.107\.1";/.exec(runner)?.[1];
    assert.equal(workflowKey, "VSCODE_TEST_VERSION");
    assert.equal(runnerKey, workflowKey);
    assert.match(runner, /await runTests\(\{\s+version,/);
    assert.match(runner, /extensionTestsEnv: \{\s+\.\.\.process\.env,\s+VSCODE_TEST_VERSION: version,/);
    assert.match(integration, /assert\.equal\(\s+vscode\.version,\s+process\.env\.VSCODE_TEST_VERSION,/);
  });

  it("uses supported editor layout without focus timers or mutating user debug settings", () => {
    const extension = readText("editors/vscode/src/extension.ts") +
      readText("editors/vscode/src/debugConfiguration.ts");
    const view = readText("editors/vscode/src/concurrencyView.ts");
    assert.doesNotMatch(extension, /setTimeout|firstStop|ConfigurationTarget\.Global|\.update\(/);
    assert.match(view, /createWebviewPanel\(/);
    assert.match(view, /ViewColumn\.Beside/);
    assert.match(view, /preserveFocus/);
    assert.match(extension, /registerWebviewViewProvider\(/);
    assert.doesNotMatch(extension + view, /moveView|moveActiveEditor|workbench\.action\.debug\.run/);
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
