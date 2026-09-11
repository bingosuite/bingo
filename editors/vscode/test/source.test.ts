import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { mkdir, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { join, resolve, sep } from "node:path";
import { describe, it, type TestContext } from "node:test";
import { pathToFileURL } from "node:url";
import { promisify } from "node:util";

import type { SessionModel } from "../src/model.js";
import {
  readSourceContext,
  sourceLimits,
  SpawnSourceController,
  validSourcePath,
  type SourceRegistry,
  type SourceWorkspace,
} from "../src/source.js";
import { emptySource, type SourceSnippet } from "../src/sourceModel.js";
import type { Location, Snapshot } from "../src/telemetry.js";
import { goroutine, snapshot } from "./fixtures.js";

async function sourceFixture(t: TestContext): Promise<{
  readonly root: string;
  readonly outside: string;
  readonly file: string;
  readonly workspace: SourceWorkspace;
}> {
  const directory = resolve(
    process.cwd(),
    "dist",
    "test-artifacts",
    `bingo-source-${randomUUID()}`,
  );
  t.after(async () => {
    await rm(directory, { recursive: true, force: true });
  });
  const root = join(directory, "workspace");
  const outside = join(directory, "workspace-sibling");
  await Promise.all([
    mkdir(root, { recursive: true }),
    mkdir(outside, { recursive: true }),
  ]);
  const file = join(root, "main.go");
  await writeFile(file, "package main\n\nfunc main() {\n\tgo worker()\n}\n");
  return { root, outside, file, workspace: { trusted: true, roots: [root] } };
}

function location(file: string, line = 4): Location {
  return { file, line, function: "main.main" };
}

function assertUnavailable(result: SourceSnippet, message: RegExp): void {
  assert.equal(result.status, "unavailable");
  assert.match(result.message, message);
  assert.deepEqual(result.lines, []);
}

function assertReady(result: SourceSnippet): void {
  assert.equal(result.status, "ready", result.message);
  assert.ok(result.lines.length <= sourceLimits.radius * 2 + 1);
  assert.equal(result.lines.filter((line) => line.highlighted).length, 1);
  for (const line of result.lines) {
    assert.ok(
      line.text.length <= sourceLimits.lineLength,
      `displayed line ${String(line.number)} exceeds the cap including its clipping marker`,
    );
  }
}

describe("source path validation", () => {
  it("accepts only bounded absolute local paths, not URI or relative spellings", () => {
    for (const file of [
      "/workspace/main.go",
      "/workspace/a b/日本語.go",
      "/workspace/a#b?c.go",
      "/workspace/<script>.go",
      `/${"a".repeat(4095)}`,
    ]) {
      assert.equal(validSourcePath(file), true, file);
    }
    for (const file of [
      "",
      "main.go",
      "./main.go",
      "../main.go",
      "workspace/main.go",
      "file:///workspace/main.go",
      "vscode-remote://ssh-remote+host/workspace/main.go",
      "https://example.test/main.go",
      "C:\\workspace\\main.go",
      "//host/share/main.go",
      `/${"a".repeat(4096)}`,
    ]) {
      assert.equal(validSourcePath(file), false, JSON.stringify(file));
    }
  });

  it("rejects every C0 control character and DEL, not just newlines and NUL", () => {
    for (const code of [...Array.from({ length: 32 }, (_, index) => index), 127]) {
      assert.equal(
        validSourcePath(`/workspace/main${String.fromCharCode(code)}.go`),
        false,
        `control character ${String(code)}`,
      );
    }
  });
});

describe("local creation source context", () => {
  it("reads a trusted workspace file and identifies its recorded creation line", async (t) => {
    const fixture = await sourceFixture(t);
    const result = await readSourceContext(location(fixture.file), fixture.workspace);
    assertReady(result);
    assert.deepEqual(result.lines, [
      { number: 1, text: "package main", highlighted: false },
      { number: 2, text: "", highlighted: false },
      { number: 3, text: "func main() {", highlighted: false },
      { number: 4, text: "\tgo worker()", highlighted: true },
      { number: 5, text: "}", highlighted: false },
      { number: 6, text: "", highlighted: false },
    ]);
  });

  it("refuses an untrusted workspace before accessing its roots", async (t) => {
    const fixture = await sourceFixture(t);
    let rootAccesses = 0;
    const workspace: SourceWorkspace = {
      trusted: false,
      get roots(): readonly string[] {
        rootAccesses += 1;
        throw new Error("untrusted roots must not be inspected");
      },
    };
    assertUnavailable(
      await readSourceContext(location(fixture.file), workspace),
      /trusted workspace/i,
    );
    assert.equal(rootAccesses, 0);
  });

  it("does not treat an absolute path as permission to read outside workspace roots", async (t) => {
    const fixture = await sourceFixture(t);
    const secret = join(fixture.outside, "private.go");
    await writeFile(secret, "private source must not appear");
    const result = await readSourceContext(location(secret, 1), fixture.workspace);
    assertUnavailable(result, /outside the trusted workspace/i);
    assert.doesNotMatch(JSON.stringify(result), /private source must not appear/);
    assertUnavailable(
      await readSourceContext(location(fixture.file), { trusted: true, roots: [] }),
      /outside the trusted workspace/i,
    );
  });

  it("rejects parent traversal and a sibling sharing the workspace's string prefix", async (t) => {
    const fixture = await sourceFixture(t);
    const secret = join(fixture.outside, "private.go");
    await writeFile(secret, "outside");
    for (const file of [
      secret,
      `${fixture.root}${sep}..${sep}workspace-sibling${sep}private.go`,
    ]) {
      assertUnavailable(
        await readSourceContext(location(file, 1), fixture.workspace),
        /outside the trusted workspace/i,
      );
    }
  });

  it("checks canonical paths for both file symlinks and symlinked parents", async (t) => {
    const fixture = await sourceFixture(t);
    const inside = join(fixture.root, "inside.go");
    await symlink(fixture.file, inside);
    assertReady(await readSourceContext(location(inside), fixture.workspace));

    const externalFile = join(fixture.outside, "private.go");
    await writeFile(externalFile, "outside");
    const escapedFile = join(fixture.root, "escaped.go");
    const escapedDirectory = join(fixture.root, "escaped-directory");
    await symlink(externalFile, escapedFile);
    await symlink(fixture.outside, escapedDirectory);
    for (const file of [escapedFile, join(escapedDirectory, "private.go")]) {
      assertUnavailable(
        await readSourceContext(location(file, 1), fixture.workspace),
        /outside the trusted workspace/i,
      );
    }
  });

  it("resolves symlinked workspace roots and permits a file under any workspace root", async (t) => {
    const fixture = await sourceFixture(t);
    const alias = join(fixture.outside, "workspace-alias");
    await symlink(fixture.root, alias);
    const canonicalFile = await realpath(fixture.file);
    for (const file of [canonicalFile, join(alias, "main.go")]) {
      assertReady(
        await readSourceContext(location(file), { trusted: true, roots: [alias] }),
      );
    }
    assertReady(
      await readSourceContext(location(fixture.file), {
        trusted: true,
        roots: [fixture.outside, fixture.root],
      }),
    );
  });

  it("returns explicit unavailability for missing files, broken links, and directories", async (t) => {
    const fixture = await sourceFixture(t);
    const missing = join(fixture.root, "missing.go");
    const broken = join(fixture.root, "broken.go");
    const directory = join(fixture.root, "directory.go");
    await symlink(missing, broken);
    await mkdir(directory);
    for (const file of [missing, broken]) {
      assertUnavailable(
        await readSourceContext(location(file), fixture.workspace),
        /cannot preview creation source/i,
      );
    }
    assertUnavailable(
      await readSourceContext(location(directory), fixture.workspace),
      /not a regular file/i,
    );
    assertUnavailable(
      await readSourceContext(location(fixture.root), fixture.workspace),
      /outside the trusted workspace/i,
    );
  });

  it("rejects FIFOs without waiting for a writer", {
    skip: process.platform !== "linux" && process.platform !== "darwin",
    timeout: 5_000,
  }, async (t) => {
    const fixture = await sourceFixture(t);
    const fifo = join(fixture.root, "pipe.go");
    await promisify(execFile)("mkfifo", [fifo]);
    assertUnavailable(
      await readSourceContext(location(fifo, 1), fixture.workspace),
      /not a regular file/i,
    );
  });

  it("rejects URI, relative, and control-containing paths before opening anything", async (t) => {
    const fixture = await sourceFixture(t);
    for (const file of [
      pathToFileURL(fixture.file).href,
      "main.go",
      "../main.go",
      `${fixture.file}\0`,
      `${fixture.file}\n`,
      `${fixture.file}\u007f`,
    ]) {
      assertUnavailable(
        await readSourceContext(location(file), fixture.workspace),
        /no valid local creation source location/i,
      );
    }
  });

  it("rejects nonpositive, fractional, nonfinite, unsafe, and over-limit line numbers", async (t) => {
    const fixture = await sourceFixture(t);
    for (const line of [0, -1, 1.5, NaN, Infinity, -Infinity, Number.MAX_SAFE_INTEGER + 1]) {
      assertUnavailable(
        await readSourceContext(location(fixture.file, line), fixture.workspace),
        /no valid local creation source location/i,
      );
    }
    assertUnavailable(
      await readSourceContext(location(fixture.file, sourceLimits.lines + 1), fixture.workspace),
      /creation line exceeds/i,
    );
  });

  it("preserves UTF-8 and hostile markup as inert raw text, not encoded or interpreted content", async (t) => {
    const fixture = await sourceFixture(t);
    const hostile = '<script>globalThis.pwned = true</script><img src=x onerror="boom()">&"\'';
    const unicode = "\tgo 日本語(\"🦆\", \"é\", \"e\u0301\")";
    await writeFile(fixture.file, `${hostile}\n${unicode}`);
    const result = await readSourceContext(location(fixture.file, 2), fixture.workspace);
    assertReady(result);
    assert.deepEqual(result.lines, [
      { number: 1, text: hostile, highlighted: false },
      { number: 2, text: unicode, highlighted: true },
    ]);
  });

  it("rejects malformed UTF-8 rather than replacing bytes and claiming exact source", async (t) => {
    const fixture = await sourceFixture(t);
    for (const invalid of [
      Buffer.from([0xff]),
      Buffer.from([0xc0, 0xaf]),
      Buffer.from([0xed, 0xa0, 0x80]),
      Buffer.from([0xf0, 0x9f, 0x92]),
    ]) {
      await writeFile(fixture.file, Buffer.concat([Buffer.from("package main\n"), invalid]));
      assertUnavailable(
        await readSourceContext(location(fixture.file, 1), fixture.workspace),
        /cannot preview creation source/i,
      );
    }
  });

  it("accepts exactly the byte limit and rejects one byte more", async (t) => {
    const fixture = await sourceFixture(t);
    await writeFile(fixture.file, Buffer.alloc(sourceLimits.bytes, 0x61));
    assertReady(await readSourceContext(location(fixture.file, 1), fixture.workspace));
    await writeFile(fixture.file, Buffer.alloc(sourceLimits.bytes + 1, 0x61));
    assertUnavailable(
      await readSourceContext(location(fixture.file, 1), fixture.workspace),
      new RegExp(`${String(sourceLimits.bytes)}-byte preview limit`, "i"),
    );
  });

  it("enforces byte size rather than JavaScript character count for multibyte source", async (t) => {
    const fixture = await sourceFixture(t);
    const atLimit = "é".repeat(sourceLimits.bytes / 2);
    assert.equal(Buffer.byteLength(atLimit), sourceLimits.bytes);
    assert.ok(atLimit.length < sourceLimits.bytes);
    await writeFile(fixture.file, atLimit);
    assertReady(await readSourceContext(location(fixture.file, 1), fixture.workspace));
    await writeFile(fixture.file, `${atLimit}a`);
    assertUnavailable(
      await readSourceContext(location(fixture.file, 1), fixture.workspace),
      /byte preview limit/i,
    );
  });

  it("accepts exactly the line limit, including its last line, and rejects one extra line", async (t) => {
    const fixture = await sourceFixture(t);
    const lines = Array.from({ length: sourceLimits.lines }, (_, index) => String(index + 1));
    await writeFile(fixture.file, lines.join("\n"));
    const result = await readSourceContext(location(fixture.file, sourceLimits.lines), fixture.workspace);
    assertReady(result);
    assert.deepEqual(result.lines.at(-1), {
      number: sourceLimits.lines,
      text: String(sourceLimits.lines),
      highlighted: true,
    });
    await writeFile(fixture.file, `${lines.join("\n")}\n`);
    assertUnavailable(
      await readSourceContext(location(fixture.file, 1), fixture.workspace),
      /exceeds the source preview line limit/i,
    );
  });

  it("retains the line-length boundary and includes the clipping marker inside the 300-character cap", async (t) => {
    const fixture = await sourceFixture(t);
    assert.equal(sourceLimits.lineLength, 300);
    const short = "s".repeat(sourceLimits.lineLength - 1);
    const exact = "x".repeat(sourceLimits.lineLength);
    const long = `${"y".repeat(sourceLimits.lineLength)}z`;
    await writeFile(fixture.file, `${short}\n${exact}\n${long}`);
    const result = await readSourceContext(location(fixture.file, 3), fixture.workspace);
    assertReady(result);
    assert.equal(result.lines[0]?.text, short);
    assert.equal(result.lines[1]?.text, exact);
    const clipped = result.lines[2]?.text;
    assert.ok(clipped);
    assert.equal(clipped.length, sourceLimits.lineLength);
    const marker = " [line clipped]";
    assert.equal(clipped, `${"y".repeat(sourceLimits.lineLength - marker.length)}${marker}`);
    assert.equal(clipped.includes("z"), false);
    assert.equal(result.lines[2]?.highlighted, true);
  });

  it("returns precisely the snippet radius, clipping only at file boundaries", async (t) => {
    const fixture = await sourceFixture(t);
    const text = Array.from({ length: 20 }, (_, index) => `line ${String(index + 1)}`).join("\n");
    await writeFile(fixture.file, text);
    for (const selected of [1, 2, 10, 19, 20]) {
      const result = await readSourceContext(location(fixture.file, selected), fixture.workspace);
      assertReady(result);
      const first = Math.max(1, selected - sourceLimits.radius);
      const last = Math.min(20, selected + sourceLimits.radius);
      assert.deepEqual(
        result.lines,
        Array.from({ length: last - first + 1 }, (_, index) => ({
          number: first + index,
          text: `line ${String(first + index)}`,
          highlighted: first + index === selected,
        })),
      );
    }
  });

  it("normalizes CRLF without retaining carriage returns and includes a trailing blank line", async (t) => {
    const fixture = await sourceFixture(t);
    await writeFile(fixture.file, "first\r\nsecond\r\n");
    const result = await readSourceContext(location(fixture.file, 3), fixture.workspace);
    assertReady(result);
    assert.deepEqual(result.lines, [
      { number: 1, text: "first", highlighted: false },
      { number: 2, text: "second", highlighted: false },
      { number: 3, text: "", highlighted: true },
    ]);
  });

  it("reports absent recorded lines instead of highlighting a nearby substitute", async (t) => {
    const fixture = await sourceFixture(t);
    await writeFile(fixture.file, "first\nsecond");
    assertUnavailable(
      await readSourceContext(location(fixture.file, 3), fixture.workspace),
      /creation line is absent.*may differ from the compiled source/i,
    );
    await writeFile(fixture.file, "");
    const empty = await readSourceContext(location(fixture.file, 1), fixture.workspace);
    assertReady(empty);
    assert.deepEqual(empty.lines, [{ number: 1, text: "", highlighted: true }]);
    assertUnavailable(
      await readSourceContext(location(fixture.file, 2), fixture.workspace),
      /creation line is absent/i,
    );
  });

  it("shows edited disk text with honest binary and unsaved-edit caveats", async (t) => {
    const fixture = await sourceFixture(t);
    const original = await readSourceContext(location(fixture.file), fixture.workspace);
    assertReady(original);
    await writeFile(fixture.file, "package main\n// added after build\nfunc main() {\n\tchanged()\n}\n");
    const edited = await readSourceContext(location(fixture.file), fixture.workspace);
    assertReady(edited);
    assert.equal(edited.lines.find((line) => line.highlighted)?.text, "\tchanged()");
    assert.notDeepEqual(edited.lines, original.lines);
    assert.match(edited.message, /local file on disk/i);
    assert.match(edited.message, /not verified against the binary/i);
    assert.match(edited.message, /unsaved edits are not shown/i);
    assert.match(edited.message, /recorded creation line.*may have moved/i);
  });
});

class RegistryStub implements SourceRegistry {
  readonly #listeners = new Set<() => void>();
  readonly models = new Map<string, SessionModel>();
  readonly sources = new Map<string, SourceSnippet>();
  readonly updates: { readonly id: string; readonly source: SourceSnippet }[] = [];
  activeId: string | undefined;

  public constructor(model?: SessionModel) {
    if (model !== undefined) {
      this.activate(model);
    }
  }

  public get listenerCount(): number {
    return this.#listeners.size;
  }

  public onChange(listener: () => void): () => void {
    this.#listeners.add(listener);
    return () => {
      this.#listeners.delete(listener);
    };
  }

  public activeModel(): SessionModel | undefined {
    return this.activeId === undefined ? undefined : this.models.get(this.activeId);
  }

  public updateSource(id: string, source: SourceSnippet): boolean {
    if (!this.models.has(id)) {
      return false;
    }
    this.sources.set(id, source);
    this.updates.push({ id, source });
    this.emit();
    return true;
  }

  public activate(model: SessionModel): void {
    this.models.set(model.debugSessionId, model);
    if (!this.sources.has(model.debugSessionId)) {
      this.sources.set(model.debugSessionId, emptySource);
    }
    this.activeId = model.debugSessionId;
    this.emit();
  }

  public select(selectedGoroutine: number): void {
    const model = this.activeModel();
    assert.ok(model);
    this.activate({ ...model, selectedGoroutine });
  }

  public replaceSnapshot(value: Snapshot | undefined): void {
    const model = this.activeModel();
    assert.ok(model);
    this.activate({ ...model, snapshot: value });
  }

  public remove(id: string): void {
    this.models.delete(id);
    this.sources.delete(id);
    this.emit();
  }

  public emit(): void {
    for (const listener of [...this.#listeners]) {
      listener();
    }
  }
}

function modelFor(value: Snapshot, debugSessionId = "debug"): SessionModel {
  return {
    debugSessionId,
    debugSessionName: debugSessionId,
    sessionId: `hub-${debugSessionId}`,
    connection: "connected",
    sessionState: "suspended",
    clients: 1,
    lastStop: "Breakpoint",
    error: "",
    seqGap: "",
    lastSeq: 1,
    snapshot: value,
    selectedGoroutine: value.current,
    timeline: [],
  };
}

function sourceSnapshot(count = 3): Snapshot {
  return snapshot(Array.from({ length: count }, (_, index) =>
    goroutine(index + 1, 0, {
      current: index === 0,
      createdLoc: location(`/workspace/source-${String(index + 1)}.go`, index + 1),
    }),
  ));
}

function snippet(text: string): SourceSnippet {
  return {
    status: "ready",
    message: "Local file on disk, not verified against the binary.",
    lines: [{ number: 1, text, highlighted: true }],
  };
}

class ReadQueue {
  readonly calls: {
    readonly location: Location;
    readonly resolve: (value: SourceSnippet) => void;
    readonly reject: (error: unknown) => void;
  }[] = [];
  active = 0;
  maximumActive = 0;

  public readonly read = (value: Location): Promise<SourceSnippet> => {
    this.active += 1;
    this.maximumActive = Math.max(this.maximumActive, this.active);
    return new Promise<SourceSnippet>((resolve, reject) => {
      this.calls.push({ location: value, resolve, reject });
    }).finally(() => {
      this.active -= 1;
    });
  };

  public resolve(index: number, value: SourceSnippet): void {
    const call = this.calls[index];
    assert.ok(call, `read ${String(index)} must have started`);
    call.resolve(value);
  }

  public reject(index: number, error: unknown): void {
    const call = this.calls[index];
    assert.ok(call, `read ${String(index)} must have started`);
    call.reject(error);
  }
}

function settle(): Promise<void> {
  return new Promise((resolve) => {
    setImmediate(resolve);
  });
}

function controllerFixture(t: TestContext, value = sourceSnapshot()): {
  readonly registry: RegistryStub;
  readonly reads: ReadQueue;
  readonly controller: SpawnSourceController;
} {
  const registry = new RegistryStub(modelFor(value));
  const reads = new ReadQueue();
  const controller = new SpawnSourceController(registry, reads.read);
  t.after(() => {
    controller.dispose();
  });
  return { registry, reads, controller };
}

describe("spawn source controller", () => {
  it("starts one read, publishes loading then the result, and does not loop on its own updates", async (t) => {
    const { registry, reads } = controllerFixture(t);
    assert.equal(reads.calls.length, 1);
    assert.equal(reads.active, 1);
    assert.deepEqual(reads.calls[0]?.location, location("/workspace/source-1.go", 1));
    assert.equal(registry.sources.get("debug")?.status, "loading");
    assert.deepEqual(registry.sources.get("debug")?.lines, []);
    const result = snippet("go worker()");
    reads.resolve(0, result);
    await settle();
    assert.equal(registry.sources.get("debug"), result);
    assert.deepEqual(registry.updates.map(({ source }) => source.status), ["loading", "ready"]);
    assert.equal(reads.calls.length, 1);
    assert.equal(reads.active, 0);
  });

  it("does no I/O without an active session or a snapshot and starts when one arrives", async (t) => {
    const registry = new RegistryStub();
    const reads = new ReadQueue();
    const controller = new SpawnSourceController(registry, reads.read);
    t.after(() => { controller.dispose(); });
    registry.emit();
    registry.activate({ ...modelFor(sourceSnapshot()), snapshot: undefined });
    registry.emit();
    assert.equal(reads.calls.length, 0);
    assert.equal(registry.updates.length, 0);
    registry.replaceSnapshot(sourceSnapshot());
    assert.equal(reads.calls.length, 1);
    reads.resolve(0, snippet("available"));
    await settle();
    assert.equal(registry.sources.get("debug")?.status, "ready");
  });

  it("allows a selected non-current goroutine and reads its creation site, not its stack or start site", async (t) => {
    const value = sourceSnapshot();
    const registry = new RegistryStub({ ...modelFor(value), selectedGoroutine: 2 });
    const reads = new ReadQueue();
    const controller = new SpawnSourceController(registry, reads.read);
    t.after(() => { controller.dispose(); });
    assert.equal(value.current, 1);
    assert.equal(value.goroutines[1]?.current, false);
    assert.deepEqual(reads.calls[0]?.location, value.goroutines[1]?.createdLoc);
    assert.notDeepEqual(reads.calls[0]?.location, value.goroutines[1]?.currentLoc);
    assert.notDeepEqual(reads.calls[0]?.location, value.goroutines[1]?.startLoc);
    reads.resolve(0, snippet("non-current creation source"));
    await settle();
    assert.equal(registry.sources.get("debug")?.lines[0]?.text, "non-current creation source");
  });

  it("finishes explicitly when the selected goroutine is absent, without reading a substitute", async (t) => {
    const registry = new RegistryStub({ ...modelFor(sourceSnapshot()), selectedGoroutine: 999 });
    const reads = new ReadQueue();
    const controller = new SpawnSourceController(registry, reads.read);
    t.after(() => { controller.dispose(); });
    await settle();
    assert.equal(reads.calls.length, 0);
    const result = registry.sources.get("debug");
    assert.ok(result);
    assertUnavailable(result, /no creation location.*this goroutine/i);
    registry.select(1);
    assert.equal(reads.calls.length, 1);
    reads.resolve(0, snippet("valid selection"));
    await settle();
    assert.equal(registry.sources.get("debug")?.status, "ready");
  });

  it("coalesces redundant registry updates while a read is pending and after completion", async (t) => {
    const { registry, reads } = controllerFixture(t);
    for (let index = 0; index < 30; index += 1) {
      registry.emit();
      registry.select(1);
    }
    assert.equal(reads.calls.length, 1);
    assert.equal(registry.updates.length, 1);
    reads.resolve(0, snippet("once"));
    await settle();
    registry.emit();
    registry.select(1);
    await settle();
    assert.equal(reads.calls.length, 1);
    assert.equal(registry.updates.length, 2);
    assert.equal(reads.maximumActive, 1);
  });

  it("keeps only one read in flight and skips intermediate selections in favor of the latest", async (t) => {
    const { registry, reads } = controllerFixture(t, sourceSnapshot(20));
    for (let id = 2; id <= 20; id += 1) {
      registry.select(id);
    }
    assert.equal(reads.calls.length, 1);
    assert.equal(reads.active, 1);
    const stale = snippet("must not publish");
    reads.resolve(0, stale);
    await settle();
    assert.equal(reads.calls.length, 2);
    assert.equal(reads.calls[1]?.location.file, "/workspace/source-20.go");
    assert.equal(registry.sources.get("debug")?.status, "loading");
    assert.equal(registry.updates.some(({ source }) => source === stale), false);
    const latest = snippet("latest");
    reads.resolve(1, latest);
    await settle();
    assert.equal(registry.sources.get("debug"), latest);
    assert.equal(reads.maximumActive, 1);
    assert.equal(reads.active, 0);
  });

  it("uses cached source for a repeated location selected by another goroutine", async (t) => {
    const shared = location("/workspace/shared.go", 12);
    const value = snapshot([
      goroutine(1, 0, { current: true, createdLoc: shared }),
      goroutine(2, 1, { createdLoc: { ...shared } }),
    ]);
    const { registry, reads } = controllerFixture(t, value);
    const result = snippet("shared source");
    reads.resolve(0, result);
    await settle();
    registry.select(2);
    await settle();
    assert.equal(reads.calls.length, 1);
    assert.equal(registry.sources.get("debug"), result);
  });

  it("keeps cache keys distinct for different creation lines in the same file", async (t) => {
    const value = snapshot([
      goroutine(1, 0, { current: true, createdLoc: location("/workspace/shared.go", 1) }),
      goroutine(2, 1, { createdLoc: location("/workspace/shared.go", 20) }),
    ]);
    const { registry, reads } = controllerFixture(t, value);
    const first = snippet("line one");
    reads.resolve(0, first);
    await settle();
    registry.select(2);
    assert.equal(reads.calls.length, 2);
    assert.equal(reads.calls[1]?.location.line, 20);
    reads.resolve(1, snippet("line twenty"));
    await settle();
    registry.select(1);
    await settle();
    assert.equal(reads.calls.length, 2);
    assert.equal(registry.sources.get("debug"), first);
  });

  it("bounds retained cache entries at sixteen and rereads an evicted location", async (t) => {
    assert.equal(sourceLimits.cacheEntries, 16);
    const { registry, reads } = controllerFixture(t, sourceSnapshot(17));
    for (let id = 1; id <= 16; id += 1) {
      registry.select(id);
      reads.resolve(id - 1, snippet(`source ${String(id)}`));
      await settle();
    }
    assert.equal(reads.calls.length, 16);
    registry.select(1);
    await settle();
    assert.equal(reads.calls.length, 16, "all sixteen completed entries fit");
    assert.equal(registry.sources.get("debug")?.lines[0]?.text, "source 1");
    registry.select(17);
    assert.equal(reads.calls.length, 17);
    reads.resolve(16, snippet("source 17"));
    await settle();

    let misses = 0;
    for (let id = 1; id <= 16; id += 1) {
      const before: number = reads.calls.length;
      registry.select(id);
      if (reads.calls.length > before) {
        misses += 1;
        assert.equal(reads.calls.length, before + 1);
        reads.resolve(before, snippet(`source ${String(id)} reread`));
      }
      await settle();
    }
    assert.ok(misses >= 1, "a seventeenth completed location cannot leave all sixteen earlier entries cached");
    assert.equal(reads.maximumActive, 1);
  });

  it("ignores a stale snapshot result even when the new snapshot has the same source location", async (t) => {
    const { registry, reads } = controllerFixture(t);
    registry.replaceSnapshot(sourceSnapshot());
    assert.equal(reads.calls.length, 1);
    const stale = snippet("old snapshot");
    reads.resolve(0, stale);
    await settle();
    assert.equal(reads.calls.length, 2);
    assert.deepEqual(reads.calls[1]?.location, reads.calls[0]?.location);
    assert.equal(registry.updates.some(({ source }) => source === stale), false);
    const fresh = snippet("new snapshot");
    reads.resolve(1, fresh);
    await settle();
    assert.equal(registry.sources.get("debug"), fresh);
    assert.equal(reads.maximumActive, 1);
  });

  it("invalidates completed cache entries when a new snapshot arrives", async (t) => {
    const { registry, reads } = controllerFixture(t);
    reads.resolve(0, snippet("before snapshot"));
    await settle();
    registry.replaceSnapshot(sourceSnapshot());
    assert.equal(reads.calls.length, 2);
    assert.equal(registry.sources.get("debug")?.status, "loading");
    reads.resolve(1, snippet("after snapshot"));
    await settle();
    assert.equal(registry.sources.get("debug")?.lines[0]?.text, "after snapshot");
  });

  it("ignores pending results after snapshot removal and can resume from a later snapshot", async (t) => {
    const { registry, reads } = controllerFixture(t);
    registry.replaceSnapshot(undefined);
    const updates = registry.updates.length;
    reads.resolve(0, snippet("snapshot disappeared"));
    await settle();
    assert.equal(registry.updates.length, updates);
    assert.equal(reads.calls.length, 1);
    registry.replaceSnapshot(sourceSnapshot());
    assert.equal(reads.calls.length, 2);
    reads.resolve(1, snippet("restored snapshot"));
    await settle();
    assert.equal(registry.sources.get("debug")?.lines[0]?.text, "restored snapshot");
  });

  it("does not publish an old session's pending read into either session after an active-session switch", async (t) => {
    const { registry, reads } = controllerFixture(t);
    const value = registry.activeModel()?.snapshot;
    assert.ok(value);
    registry.activate(modelFor(value, "other-debug"));
    assert.equal(reads.calls.length, 1);
    const stale = snippet("old session");
    reads.resolve(0, stale);
    await settle();
    assert.equal(registry.updates.some(({ source }) => source === stale), false);
    assert.equal(reads.calls.length, 2);
    const latest = snippet("new session");
    reads.resolve(1, latest);
    await settle();
    assert.equal(registry.sources.get("other-debug"), latest);
    assert.notEqual(registry.sources.get("debug"), latest);
    assert.equal(reads.maximumActive, 1);
  });

  it("invalidates the cache across sessions even if they share the identical snapshot object", async (t) => {
    const value = sourceSnapshot();
    const { registry, reads } = controllerFixture(t, value);
    reads.resolve(0, snippet("session one"));
    await settle();
    registry.activate(modelFor(value, "other-debug"));
    assert.equal(reads.calls.length, 2);
    reads.resolve(1, snippet("session two"));
    await settle();
    registry.activate(modelFor(value));
    assert.equal(reads.calls.length, 3);
    reads.resolve(2, snippet("session one revisited"));
    await settle();
    assert.equal(registry.sources.get("debug")?.lines[0]?.text, "session one revisited");
  });

  it("drops a terminated session's pending result without recreating the removed session", async (t) => {
    const { registry, reads } = controllerFixture(t);
    registry.remove("debug");
    const updates = registry.updates.length;
    reads.resolve(0, snippet("terminated"));
    await settle();
    assert.equal(registry.models.has("debug"), false);
    assert.equal(registry.sources.has("debug"), false);
    assert.equal(registry.updates.length, updates);
    assert.equal(reads.calls.length, 1);
    assert.equal(reads.active, 0);
  });

  it("refresh invalidates an in-flight result without starting a concurrent read", async (t) => {
    const { registry, reads, controller } = controllerFixture(t);
    controller.refresh();
    controller.refresh();
    assert.equal(reads.calls.length, 1);
    const stale = snippet("before refresh");
    reads.resolve(0, stale);
    await settle();
    assert.equal(reads.calls.length, 2);
    assert.equal(registry.updates.some(({ source }) => source === stale), false);
    assert.equal(registry.sources.get("debug")?.status, "loading");
    reads.resolve(1, snippet("after refresh"));
    await settle();
    assert.equal(registry.sources.get("debug")?.lines[0]?.text, "after refresh");
    assert.equal(reads.maximumActive, 1);
  });

  it("refresh clears previously cached success and unavailability results", async (t) => {
    const { registry, reads, controller } = controllerFixture(t);
    const unavailable: SourceSnippet = { status: "unavailable", message: "file missing", lines: [] };
    reads.resolve(0, unavailable);
    await settle();
    registry.select(2);
    reads.resolve(1, snippet("second file"));
    await settle();
    registry.select(1);
    await settle();
    assert.equal(registry.sources.get("debug"), unavailable);
    assert.equal(reads.calls.length, 2);
    controller.refresh();
    assert.equal(reads.calls.length, 3);
    reads.resolve(2, snippet("file now exists"));
    await settle();
    registry.select(2);
    assert.equal(reads.calls.length, 4);
    reads.resolve(3, snippet("second file reread"));
    await settle();
    assert.equal(registry.sources.get("debug")?.lines[0]?.text, "second file reread");
  });

  it("dispose unsubscribes and suppresses late success, further changes, and refresh", async (t) => {
    const { registry, reads, controller } = controllerFixture(t);
    assert.equal(registry.listenerCount, 1);
    controller.dispose();
    assert.equal(registry.listenerCount, 0);
    const updates = registry.updates.length;
    registry.select(2);
    controller.refresh();
    reads.resolve(0, snippet("disposed"));
    await settle();
    assert.equal(reads.calls.length, 1);
    assert.equal(registry.updates.length, updates);
    assert.equal(reads.active, 0);
  });

  it("turns a rejected read into explicit unavailability and remains usable on refresh", async (t) => {
    const { registry, reads, controller } = controllerFixture(t);
    reads.reject(0, new Error("permission denied"));
    await settle();
    const failed = registry.sources.get("debug");
    assert.ok(failed);
    assertUnavailable(failed, /cannot preview creation source: permission denied/i);
    assert.equal(reads.active, 0);
    controller.refresh();
    assert.equal(reads.calls.length, 2);
    reads.resolve(1, snippet("recovered"));
    await settle();
    assert.equal(registry.sources.get("debug")?.status, "ready");
  });

  it("handles a non-Error rejection without leaving loading stuck", async (t) => {
    const { registry, reads } = controllerFixture(t);
    reads.reject(0, "read rejected");
    await settle();
    const result = registry.sources.get("debug");
    assert.ok(result);
    assertUnavailable(result, /cannot preview creation source: read rejected/i);
    assert.equal(reads.active, 0);
  });

  it("handles a synchronous reader throw without leaving loading stuck", async (t) => {
    const registry = new RegistryStub(modelFor(sourceSnapshot()));
    const read = (): Promise<SourceSnippet> => {
      throw new Error("synchronous failure");
    };
    const controller = new SpawnSourceController(registry, read);
    t.after(() => { controller.dispose(); });
    await settle();
    const result = registry.sources.get("debug");
    assert.ok(result);
    assertUnavailable(result, /cannot preview creation source: synchronous failure/i);
  });

  it("ignores a stale failure and still starts the latest queued selection", async (t) => {
    const { registry, reads } = controllerFixture(t);
    registry.select(2);
    reads.reject(0, new Error("stale failure"));
    await settle();
    assert.equal(reads.calls.length, 2);
    assert.equal(reads.calls[1]?.location.file, "/workspace/source-2.go");
    assert.equal(registry.sources.get("debug")?.status, "loading");
    assert.equal(registry.updates.some(({ source }) => source.message.includes("stale failure")), false);
    reads.resolve(1, snippet("selected source"));
    await settle();
    assert.equal(registry.sources.get("debug")?.lines[0]?.text, "selected source");
    assert.equal(reads.maximumActive, 1);
  });

  it("catches a read rejection after disposal without publishing or restarting work", async (t) => {
    const { registry, reads, controller } = controllerFixture(t);
    controller.dispose();
    const updates = registry.updates.length;
    reads.reject(0, new Error("closed reader"));
    await settle();
    assert.equal(registry.updates.length, updates);
    assert.equal(reads.calls.length, 1);
    assert.equal(reads.active, 0);
  });
});
