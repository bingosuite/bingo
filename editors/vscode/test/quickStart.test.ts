import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { describe, it } from "node:test";

import {
  goPackageConfiguration,
  needsGoPackageConfiguration,
  type GoPackageContext,
} from "../src/quickStart.js";
import { validateBingoConfiguration } from "../src/configuration.js";

const local = (fsPath: string) => ({ scheme: "file", fsPath });
const active = (fsPath: string) => ({
  ...local(fsPath), languageId: "go", isUntitled: false,
});
const root = local("/workspace/project");
const entries = (...names: string[]) => Promise.resolve(
  names.map((name) => ({ name, isFile: () => true })),
);

describe("Go package quick start", () => {
  it("recognizes no-launch bootstrap configs without reinterpreting legacy binaries", () => {
    for (const config of [{}, { type: "bingo" }, { type: "bingo", request: "", name: "" }]) {
      assert.equal(needsGoPackageConfiguration(config), true);
    }
    for (const config of [
      { type: "go" },
      { request: "launch", program: "/workspace/target" },
      { request: "launch", mode: "exec", program: "/workspace/target" },
      { request: "launch", mode: "debug", program: "/workspace/app" },
      { request: "launch" },
      { mode: "debug" },
      { program: "/workspace/target" },
      { request: "attach", session: "shared-session" },
      { request: "attach", pid: 1234 },
    ]) {
      assert.equal(needsGoPackageConfiguration(config), false);
    }
  });

  it("generates a minimal directory launch from the active Go file, not the module root", async () => {
    const config = await goPackageConfiguration({
      activeDocument: active("/workspace/project/cmd/my app/main.go"),
      folder: root,
      workspaceFolders: [root],
    }, () => { throw new Error("active-file selection must not scan the workspace"); });
    assert.deepEqual(config, {
      type: "bingo",
      request: "launch",
      name: "bingo: Debug Go Package",
      mode: "debug",
      program: "/workspace/project/cmd/my app",
      cwd: "/workspace/project/cmd/my app",
    });
    assert.equal("preLaunchTask" in config, false);
    assert.equal("stopOnEntry" in config, false);
    assert.equal(validateBingoConfiguration(config).request, "launch");
  });

  it("uses a saved Go file without requiring a workspace", async () => {
    const config = await goPackageConfiguration({
      activeDocument: active("/source with spaces/main.go"),
      workspaceFolders: [],
    });
    assert.equal(config.program, "/source with spaces");
  });

  it("lets an active Go file disambiguate multi-root workspaces", async () => {
    const config = await goPackageConfiguration({
      activeDocument: active("/workspace/second/cmd/app/main.go"),
      workspaceFolders: [root, local("/workspace/second")],
    });
    assert.equal(config.program, "/workspace/second/cmd/app");
  });

  it("honors an explicitly selected folder instead of the active file in another root", async () => {
    const scanned: string[] = [];
    const config = await goPackageConfiguration({
      activeDocument: active("/workspace/project-other/main.go"),
      folder: root,
      workspaceFolders: [root, local("/workspace/project-other")],
    }, (path) => {
      scanned.push(path);
      return entries("main.go");
    });
    assert.deepEqual(scanned, [root.fsPath]);
    assert.equal(config.program, root.fsPath);
  });

  it("uses the single workspace only when it actually contains Go source", async (t) => {
    const parent = resolve("dist", "test-artifacts");
    await mkdir(parent, { recursive: true });
    const directory = await mkdtemp(join(parent, "Go package with spaces-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    await writeFile(join(directory, "main.go"), "package main\nfunc main() {}\n");
    const config = await goPackageConfiguration({
      activeDocument: { ...local(join(directory, "README.md")), languageId: "markdown", isUntitled: false },
      workspaceFolders: [local(directory)],
    });
    assert.equal(config.program, directory);
    assert.equal(config.cwd, directory);
  });

  for (const context of [
    { workspaceFolders: [] },
    { workspaceFolders: [root, local("/workspace/second")] },
  ]) {
    it(`refuses to guess with ${String(context.workspaceFolders.length)} workspace folders`, async () => {
      await assert.rejects(goPackageConfiguration(context, () => {
        throw new Error("ambiguous selection must not scan");
      }), /Open a saved Go file/);
    });
  }

  for (const files of [[], ["README.md"], ["main_test.go"], ["_ignored.go", ".ignored.go"]]) {
    it(`does not guess a nested package from ${JSON.stringify(files)}`, async () => {
      await assert.rejects(goPackageConfiguration({
        workspaceFolders: [root],
      }, () => entries(...files)), /No Go source files.*Open a Go file/);
    });
  }

  it("does not mistake a directory named main.go for source", async () => {
    await assert.rejects(goPackageConfiguration({
      workspaceFolders: [root],
    }, () => Promise.resolve([{ name: "main.go", isFile: () => false }])), /No Go source files/);
  });

  it("reports unreadable directories instead of claiming an empty package", async () => {
    await assert.rejects(goPackageConfiguration({
      workspaceFolders: [root],
    }, () => Promise.reject(new Error("permission denied"))),
    /Cannot read Go package directory.*permission denied/);
  });

  for (const [label, context, reason] of [
    ["untitled Go file", {
      activeDocument: { ...active("/untitled.go"), isUntitled: true },
      workspaceFolders: [root],
    }, /Save the Go file/],
    ["virtual Go file", {
      activeDocument: { ...active("/workspace/main.go"), scheme: "git" },
      workspaceFolders: [root],
    }, /saved local Go file/],
    ["virtual workspace", {
      workspaceFolders: [{ ...root, scheme: "vscode-remote" }],
    }, /server-local program/],
  ] satisfies [string, GoPackageContext, RegExp][]) {
    it(`reports ${label} rather than using an unrelated directory`, async () => {
      await assert.rejects(goPackageConfiguration(context, () => entries("main.go")), reason);
    });
  }
});
