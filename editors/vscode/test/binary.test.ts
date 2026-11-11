import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import {
  chmod,
  mkdir,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { join, resolve } from "node:path";
import { afterEach, describe, it } from "node:test";

import { resolveBundledBinary, verifyDarwinSignature } from "../src/binary.js";

const temporaryDirectories: string[] = [];

afterEach(async () => {
  await Promise.all(
    temporaryDirectories.splice(0).map(async (directory) => {
      await rm(directory, { force: true, recursive: true });
    }),
  );
});

async function extensionFixture(
  target = "darwin-arm64",
): Promise<{ readonly root: string; readonly binary: string }> {
  const root = resolve(
    process.cwd(),
    "dist",
    "test-artifacts",
    `bingo-extension-${randomUUID()}`,
  );
  await mkdir(root, { recursive: true });
  temporaryDirectories.push(root);
  const bin = join(root, "bin");
  await mkdir(bin);
  await writeFile(join(bin, "target.json"), JSON.stringify({ target }));
  const binary = join(bin, "bingo");
  await writeFile(binary, "binary", { mode: 0o644 });
  await chmod(binary, 0o644);
  return { root, binary };
}

describe("bundled binary resolution", () => {
  it("finds the extension-local binary and repairs executable mode", async () => {
    const fixture = await extensionFixture();
    const verified: string[] = [];
    assert.equal(
      await resolveBundledBinary(fixture.root, "darwin-arm64", (path) => {
        verified.push(path);
        return Promise.resolve();
      }),
      fixture.binary,
    );
    assert.deepEqual(verified, [fixture.binary]);
    assert.notEqual((await stat(fixture.binary)).mode & 0o111, 0);
  });

  it("does not run macOS signing tools for the Linux package", async () => {
    const fixture = await extensionFixture("linux-x64");
    assert.equal(await resolveBundledBinary(fixture.root, "linux-x64", () => {
      throw new Error("Linux must not use codesign");
    }), fixture.binary);
  });

  it("verifies the signature before checking the debugger entitlement", async () => {
    const calls: string[][] = [];
    await verifyDarwinSignature("/extension path/bin/bingo", (args) => {
      calls.push([...args]);
      return Promise.resolve("<plist><dict><key>com.apple.security.cs.debugger</key><true/></dict></plist>");
    });
    assert.deepEqual(calls, [
      ["--verify", "--strict", "/extension path/bin/bingo"],
      ["--display", "--entitlements", "-", "--xml", "/extension path/bin/bingo"],
    ]);
  });

  for (const entitlement of ["", "<key>com.apple.security.cs.debugger</key><false/>"]) {
    it(`rejects a missing or disabled debugger entitlement: ${entitlement}`, async () => {
      await assert.rejects(verifyDarwinSignature("/extension/bin/bingo", () => Promise.resolve(entitlement)),
        /entitlement is missing or disabled.*Reinstall the darwin-arm64/);
    });
  }

  it("reports signature failure without attempting launch or entitlement inspection", async () => {
    let calls = 0;
    await assert.rejects(verifyDarwinSignature("/extension/bin/bingo", () => {
      calls += 1;
      return Promise.reject(new Error("invalid signature"));
    }), /invalid signature.*Reinstall the darwin-arm64/);
    assert.equal(calls, 1);
  });

  it("rejects the wrong package target", async () => {
    const fixture = await extensionFixture("linux-x64");
    await assert.rejects(
      resolveBundledBinary(fixture.root, "darwin-arm64"),
      /expected darwin-arm64/,
    );
  });

  it("rejects a missing binary", async () => {
    const fixture = await extensionFixture();
    await rm(fixture.binary);
    await assert.rejects(
      resolveBundledBinary(fixture.root, "darwin-arm64"),
      /cannot find/,
    );
  });

  it("rejects a directory in place of the binary", async () => {
    const fixture = await extensionFixture();
    await rm(fixture.binary);
    await mkdir(fixture.binary);
    await assert.rejects(
      resolveBundledBinary(fixture.root, "darwin-arm64"),
      /not a file/,
    );
  });
});
