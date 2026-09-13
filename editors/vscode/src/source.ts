import { constants } from "node:fs";
import { open, realpath } from "node:fs/promises";
import { isAbsolute, relative, sep } from "node:path";

import type { SessionModel } from "./model.js";
import type { SourceSnippet } from "./sourceModel.js";
import type { Location, Snapshot } from "./telemetry.js";

export const sourceLimits = {
  bytes: 256 * 1024,
  lines: 10_000,
  lineLength: 300,
  radius: 4,
  cacheEntries: 16,
} as const;

export interface SourceWorkspace {
  readonly trusted: boolean;
  readonly roots: readonly string[];
}

function unavailable(message: string): SourceSnippet {
  return { status: "unavailable", message, lines: [] };
}

function within(root: string, path: string): boolean {
  const part = relative(root, path);
  return part !== "" && part !== ".." && !part.startsWith(`..${sep}`) && !isAbsolute(part);
}

export function validSourcePath(path: string): boolean {
  return path.length > 0 && path.length <= 4096 && isAbsolute(path) &&
    ![...path].some((char) => {
      const code = char.codePointAt(0);
      return code !== undefined && (code < 32 || code === 127);
    }) &&
    !path.startsWith("//");
}

// A target's DWARF path is not permission to read arbitrary local files. Resolve
// both sides of the workspace boundary before opening, then verify the opened
// inode and canonical path again before reading through the bounded descriptor.
export async function readSourceContext(
  location: Location,
  workspace: SourceWorkspace,
): Promise<SourceSnippet> {
  if (!workspace.trusted) {
    return unavailable("Source preview requires a trusted workspace.");
  }
  if (!validSourcePath(location.file) || !Number.isSafeInteger(location.line) || location.line < 1) {
    return unavailable("No valid local creation source location is available.");
  }
  if (location.line > sourceLimits.lines) {
    return unavailable("Creation line exceeds the source preview line limit.");
  }
  try {
    const roots = await Promise.all(workspace.roots.map((root) => realpath(root)));
    const path = await realpath(location.file);
    if (!roots.some((root) => within(root, path))) {
      return unavailable("Creation source is outside the trusted workspace; preview was not read.");
    }
    const file = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    try {
      const stat = await file.stat();
      const verifiedPath = await realpath(location.file);
      if (verifiedPath !== path || !stat.isFile()) {
        return unavailable("Creation source changed location or is not a regular file.");
      }
      const verifier = await open(verifiedPath, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
      try {
        const verified = await verifier.stat();
        if (verified.dev !== stat.dev || verified.ino !== stat.ino) {
          return unavailable("Creation source changed while opening; refresh to retry.");
        }
      } finally {
        await verifier.close();
      }
      if (stat.size > sourceLimits.bytes) {
        return unavailable(`Creation source exceeds the ${String(sourceLimits.bytes)}-byte preview limit.`);
      }
      const buffer = Buffer.alloc(sourceLimits.bytes + 1);
      let length = 0;
      while (length < buffer.length) {
        const result = await file.read(buffer, length, buffer.length - length, length);
        if (result.bytesRead === 0) {
          break;
        }
        length += result.bytesRead;
      }
      const after = await file.stat();
      if (length > sourceLimits.bytes || after.size !== stat.size || after.mtimeMs !== stat.mtimeMs) {
        return unavailable("Creation source grew or changed during preview; refresh to retry.");
      }
      const text = new TextDecoder("utf-8", { fatal: true }).decode(buffer.subarray(0, length));
      return sourceSnippet(text, location.line);
    } finally {
      await file.close();
    }
  } catch (error: unknown) {
    return unavailable(`Cannot preview creation source: ${error instanceof Error ? error.message : String(error)}`);
  }
}

function sourceSnippet(text: string, creationLine: number): SourceSnippet {
  const lines = text.split(/\r?\n/u);
  if (lines.length > sourceLimits.lines) {
    return unavailable("Creation source exceeds the source preview line limit.");
  }
  if (creationLine > lines.length) {
    return unavailable("Creation line is absent from the local file; it may differ from the compiled source.");
  }
  const first = Math.max(1, creationLine - sourceLimits.radius);
  const last = Math.min(lines.length, creationLine + sourceLimits.radius);
  const clipped = " [line clipped]";
  return {
    status: "ready",
    message: "Local file on disk, not verified against the binary; unsaved edits are not shown. Highlight marks the recorded creation line, which may have moved.",
    lines: lines.slice(first - 1, last).map((line, index) => ({
      number: first + index,
      text: line.length > sourceLimits.lineLength
        ? `${line.slice(0, sourceLimits.lineLength - clipped.length)}${clipped}`
        : line,
      highlighted: first + index === creationLine,
    })),
  };
}

export interface SourceRegistry {
  onChange(listener: () => void): () => void;
  activeModel(): SessionModel | undefined;
  updateSource(id: string, source: SourceSnippet): boolean;
}

interface SourceTarget {
  readonly session: string;
  readonly snapshot: Snapshot;
  readonly goroutine: number;
  readonly location: Location | undefined;
}

export class SpawnSourceController {
  readonly #unsubscribe: () => void;
  readonly #cache = new Map<string, SourceSnippet>();
  #target: SourceTarget | undefined;
  #version = 0;
  #busy = false;
  #disposed = false;

  public constructor(
    private readonly registry: SourceRegistry,
    private readonly read: (location: Location) => Promise<SourceSnippet>,
  ) {
    this.#unsubscribe = registry.onChange(() => this.#sync());
    this.#sync();
  }

  public refresh(): void {
    if (this.#disposed) {
      return;
    }
    this.#cache.clear();
    this.#target = undefined;
    this.#sync();
  }

  public dispose(): void {
    this.#disposed = true;
    this.#version += 1;
    this.#target = undefined;
    this.#cache.clear();
    this.#unsubscribe();
  }

  #sync(): void {
    if (this.#disposed) {
      return;
    }
    const model = this.registry.activeModel();
    if (model?.snapshot === undefined) {
      this.#target = undefined;
      this.#version += 1;
      this.#cache.clear();
      return;
    }
    if (this.#target?.session === model.debugSessionId &&
      this.#target.snapshot === model.snapshot &&
      this.#target.goroutine === model.selectedGoroutine) {
      return;
    }
    if (this.#target?.session !== model.debugSessionId || this.#target.snapshot !== model.snapshot) {
      this.#cache.clear();
    }
    this.#target = {
      session: model.debugSessionId,
      snapshot: model.snapshot,
      goroutine: model.selectedGoroutine,
      location: model.snapshot.goroutines.find((g) => g.id === model.selectedGoroutine)?.createdLoc,
    };
    this.#version += 1;
    this.registry.updateSource(model.debugSessionId, {
      status: "loading",
      message: "Loading local creation source...",
      lines: [],
    });
    void this.#load();
  }

  async #load(): Promise<void> {
    if (this.#busy || this.#disposed || this.#target === undefined) {
      return;
    }
    this.#busy = true;
    const target = this.#target;
    const version = this.#version;
    try {
      const location = target.location;
      const key = JSON.stringify(location);
      let result = this.#cache.get(key);
      result ??= location === undefined
        ? unavailable("No creation location is available for this goroutine.")
        : await this.read(location);
      if (!this.#disposed && this.#version === version) {
        if (this.#cache.size >= sourceLimits.cacheEntries) {
          this.#cache.delete(this.#cache.keys().next().value ?? "");
        }
        this.#cache.set(key, result);
        this.registry.updateSource(target.session, result);
      }
    } catch (error: unknown) {
      if (!this.#disposed && this.#version === version) {
        this.registry.updateSource(target.session, unavailable(
          `Cannot preview creation source: ${error instanceof Error ? error.message : String(error)}`,
        ));
      }
    } finally {
      this.#busy = false;
      if (!this.#disposed && this.#version !== version) {
        void this.#load();
      }
    }
  }
}
