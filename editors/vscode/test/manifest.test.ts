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

describe("extension manifest", () => {
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

  it("declares bingo endpoint defaults", () => {
    const attributes = requireRecord(
      debuggerContribution.configurationAttributes,
    );
    for (const request of ["launch", "attach"]) {
      const schema = requireRecord(attributes[request]);
      const properties = requireRecord(schema.properties);
      assert.equal(requireRecord(properties.dapHost).default, "localhost");
      assert.equal(requireRecord(properties.dapPort).default, 4711);
    }
  });

  it("ships launch, join, and PID attach snippets", () => {
    const snippets = requireArray(debuggerContribution.configurationSnippets);
    const bodies = snippets.map((snippet) =>
      requireRecord(requireRecord(snippet).body),
    );

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
