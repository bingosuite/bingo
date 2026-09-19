import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { copyFile, mkdir, mkdtemp, readFile, readdir, rm, stat, writeFile } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { describe, it, type TestContext } from "node:test";
import { pathToFileURL } from "node:url";

describe("prepared server preservation", () => {
  for (const phase of ["compile", "sign", "verify"]) {
    it(`keeps the last binary and marker when the shared helper fails during ${phase}`, async (t) => {
      const fixture = await preparationFixture(t);
      const beforeBinary = await stat(fixture.binary);
      const beforeMarker = await stat(fixture.marker);
      const result = fixture.run(phase);
      assert.ifError(result.error);
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, new RegExp(`injected ${phase} failure`));
      assert.equal(await readFile(fixture.binary, "utf8"), "previous prepared server");
      assert.equal(await readFile(fixture.marker, "utf8"), '{"target":"darwin-arm64"}\n');
      const afterBinary = await stat(fixture.binary);
      const afterMarker = await stat(fixture.marker);
      assert.equal(afterBinary.ino, beforeBinary.ino);
      assert.equal(afterBinary.mode, beforeBinary.mode);
      assert.equal(afterMarker.ino, beforeMarker.ino);
      assert.equal(afterMarker.mtimeMs, beforeMarker.mtimeMs);
      assert.deepEqual((await readdir(dirname(fixture.binary))).sort(), ["bingo", "target.json"]);
    });
  }

  it("publishes a successful build without deleting unrelated entries the archive verifier should reject", async (t) => {
    const fixture = await preparationFixture(t);
    const unexpected = join(dirname(fixture.binary), "unexpected.txt");
    await writeFile(unexpected, "not owned by preparation");
    const result = fixture.run("");
    assert.ifError(result.error);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(await readFile(fixture.binary, "utf8"), "replacement prepared server");
    assert.equal((await stat(fixture.binary)).mode & 0o777, 0o755);
    assert.equal(await readFile(fixture.marker, "utf8"), '{"target":"darwin-arm64"}\n');
    assert.equal(await readFile(unexpected, "utf8"), "not owned by preparation");
  });
});

async function preparationFixture(t: TestContext) {
  const repository = resolve("../..");
  const parent = resolve("dist", "test-artifacts");
  await mkdir(parent, { recursive: true });
  const root = await mkdtemp(join(parent, "prepared server with spaces-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  for (const path of [
    "scripts/build-binary.sh", "scripts/tooling.sh", "go.mod",
    "editors/vscode/.nvmrc", "editors/vscode/scripts/prepare-binary.mjs",
    "editors/vscode/scripts/platform.mjs", "editors/vscode/scripts/normalize-mach-o-uuid.mjs",
  ]) {
    const destination = join(root, path);
    await mkdir(dirname(destination), { recursive: true });
    await copyFile(join(repository, path), destination);
  }
  const tools = join(root, "test-tools");
  await mkdir(tools);
  const goVersion = /^go (\d+\.\d+\.\d+)$/mu.exec(await readFile(join(root, "go.mod"), "utf8"))?.[1];
  assert.ok(goVersion);
  const nodeVersion = (await readFile(join(root, "editors/vscode/.nvmrc"), "utf8")).trim();
  const commands = {
    uname: `case "$1" in -s) echo Darwin ;; -m) echo arm64 ;; *) exit 2 ;; esac`,
    xcrun: "exit 0",
    node: `if [[ "$1" == --version ]]; then echo v${nodeVersion}.0.0; fi`,
    go: `
      if [[ "$1" == env && "$2" == GOVERSION ]]; then echo go${goVersion}; exit 0; fi
      output=
      while [[ "$#" -gt 0 ]]; do
        if [[ "$1" == -o ]]; then output=$2; shift; fi
        shift
      done
      [[ -n "$output" ]] || exit 2
      printf 'replacement prepared server' > "$output"
      if [[ "$BINGO_PREPARE_TEST_FAILURE" == compile ]]; then
        echo 'injected compile failure' >&2; exit 31
      fi
    `,
    codesign: `
      if [[ "$BINGO_PREPARE_TEST_FAILURE" == sign && "$1" == --sign ]]; then
        echo 'injected sign failure' >&2; exit 32
      fi
      if [[ "$BINGO_PREPARE_TEST_FAILURE" == verify && "$1" == --verify ]]; then
        echo 'injected verify failure' >&2; exit 33
      fi
    `,
  };
  for (const [name, body] of Object.entries(commands)) {
    await writeFile(join(tools, name), `#!/bin/bash\n${body}\n`, { mode: 0o755 });
  }
  const bin = join(root, "editors/vscode/bin");
  await mkdir(bin);
  const binary = join(bin, "bingo");
  const marker = join(bin, "target.json");
  await writeFile(binary, "previous prepared server", { mode: 0o755 });
  await writeFile(marker, '{"target":"darwin-arm64"}\n');
  const prepareURL = pathToFileURL(join(root, "editors/vscode/scripts/prepare-binary.mjs")).href;
  return {
    binary,
    marker,
    run: (failure: string) => spawnSync(process.execPath, ["--input-type=module", "-e", `
      Object.defineProperty(process, "platform", { value: "darwin" });
      Object.defineProperty(process, "arch", { value: "arm64" });
      await import(${JSON.stringify(prepareURL)});
    `], {
      cwd: root,
      encoding: "utf8",
      timeout: 10_000,
      env: {
        ...process.env,
        PATH: `${tools}:${process.env.PATH ?? ""}`,
        BINGO_VERSION: "dev",
        BINGO_COMMIT: "unknown",
        BINGO_VSCODE_TARGET: "darwin-arm64",
        BINGO_PREPARE_TEST_FAILURE: failure,
      },
    }),
  };
}
