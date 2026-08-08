import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it } from "node:test";

type JsonRecord = Record<string, unknown>;

const manifestPath = resolve(process.cwd(), "package.json");
const manifest = requireRecord(
  JSON.parse(readFileSync(manifestPath, "utf8")) as unknown,
);
const contributes = requireRecord(manifest.contributes);
const debuggers = requireArray(contributes.debuggers);
const debuggerContribution = requireRecord(debuggers[0]);
const expectedExtensionVersion = "0.3.1";

describe("extension manifest", () => {
  it("versions the managed-server runtime as an installable upgrade", () => {
    const lock = requireRecord(
      JSON.parse(
        readFileSync(resolve(process.cwd(), "package-lock.json"), "utf8"),
      ) as unknown,
    );
    const packages = requireRecord(lock.packages);

    assert.equal(manifest.version, expectedExtensionVersion);
    assert.equal(lock.version, expectedExtensionVersion);
    assert.equal(
      requireRecord(packages[""]).version,
      expectedExtensionVersion,
    );
  });

  it("owns only the bingo debugger type", () => {
    assert.equal(manifest.name, "bingo");
    assert.equal(manifest.publisher, "bingosuite");
    assert.equal(debuggers.length, 1);
    assert.equal(debuggerContribution.type, "bingo");
    assert.notEqual(debuggerContribution.type, "go");

    const extensionDependencies =
      manifest.extensionDependencies === undefined
        ? []
        : requireArray(manifest.extensionDependencies);
    assert.equal(extensionDependencies.includes("golang.go"), false);
  });

  it("enables Go source breakpoints", () => {
    const breakpoints = requireArray(contributes.breakpoints);
    assert.deepEqual(breakpoints, [{ language: "go" }]);
    assert.deepEqual(debuggerContribution.languages, ["go"]);
  });

  it("contributes the concurrency Activity Bar view and controls", () => {
    const containers = requireRecord(contributes.viewsContainers);
    const activity = requireArray(requireRecord(containers).activitybar).map(requireRecord);
    assert.deepEqual(
      activity.map((item) => item.id),
      ["bingo"],
    );
    assert.equal(activity[0]?.icon, "media/bingo-activity.svg");
    const views = requireArray(requireRecord(contributes.views).bingo).map(requireRecord);
    assert.equal(views.length, 1);
    assert.equal(views[0]?.id, "bingo.concurrency");
    assert.equal(views[0]?.type, "webview");

    const commands = requireArray(contributes.commands)
      .map(requireRecord)
      .map((command) => command.command);
    for (const command of [
      "bingo.concurrency.refresh",
      "bingo.concurrency.selectSession",
      "bingo.concurrency.fit",
      "bingo.concurrency.copySnapshot",
    ]) {
      assert.ok(commands.includes(command));
    }
    assert.equal(commands.includes("bingo.concurrency.focus"), false);
    const properties = requireRecord(requireRecord(contributes.configuration).properties);
    assert.equal(
      requireRecord(properties["bingo.concurrency.autoReveal"]).default,
      true,
    );
    assert.ok(requireArray(manifest.activationEvents).includes("onDebug"));
  });

  it("declares bingo endpoint defaults", () => {
    const attributes = requireRecord(
      debuggerContribution.configurationAttributes,
    );
    for (const request of ["launch", "attach"]) {
      const schema = requireRecord(attributes[request]);
      const properties = requireRecord(schema.properties);
      assertLifecycleDefaults(properties);
    }

    const initialConfigurations = requireArray(
      debuggerContribution.initialConfigurations,
    ).map(requireRecord);
    assert.ok(initialConfigurations.length > 0);
    for (const configuration of initialConfigurations) {
      assertLifecycleDefaults(configuration);
    }
  });

  it("ships launch, join, and PID attach snippets", () => {
    const snippets = requireArray(debuggerContribution.configurationSnippets);
    const bodies = snippets.map((snippet) =>
      requireRecord(requireRecord(snippet).body),
    );
    for (const body of bodies) {
      assertLifecycleDefaults(body);
    }

    assert.ok(
      bodies.some(
        (body) => body.request === "launch" && typeof body.program === "string",
      ),
    );
    assert.ok(
      bodies.some(
        (body) => body.request === "attach" && typeof body.session === "string",
      ),
    );
    assert.ok(
      bodies.some(
        (body) => body.request === "attach" && typeof body.pid === "number",
      ),
    );
  });

  function assertLifecycleDefaults(record: JsonRecord): void {
    assert.equal(valueOrDefault(record.serverMode), "auto");
    assert.equal(valueOrDefault(record.managementHost), "127.0.0.1");
    assert.equal(valueOrDefault(record.managementPort), 6060);
    assert.equal(valueOrDefault(record.dapHost), "127.0.0.1");
    assert.equal(valueOrDefault(record.dapPort), 4711);
    assert.equal(valueOrDefault(record.serverReadyTimeoutMs), 5000);
    assert.equal(valueOrDefault(record.managedIdleTimeoutMs), 30000);
  }

  function valueOrDefault(value: unknown): unknown {
    return typeof value === "object" && value !== null && "default" in value
      ? value.default
      : value;
  }
});

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
