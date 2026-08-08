import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { EventEmitter } from "node:events";
import { mkdir, rm } from "node:fs/promises";
import { join, resolve } from "node:path";
import { afterEach, describe, it } from "node:test";
import type {
  ChildProcess,
  SpawnOptions,
} from "node:child_process";

import { spawnDetachedServer } from "../src/serverProcess.js";

const temporaryDirectories: string[] = [];

afterEach(async () => {
  await Promise.all(
    temporaryDirectories.splice(0).map(async (directory) => {
      await rm(directory, { force: true, recursive: true });
    }),
  );
});

describe("detached server process", () => {
  it("uses argv without a shell, detaches, logs through files, and only unrefs", async () => {
    const root = resolve(
      process.cwd(),
      "dist",
      "test-artifacts",
      `bingo-process-${randomUUID()}`,
    );
    await mkdir(root, { recursive: true });
    temporaryDirectories.push(root);
    const child = new EventEmitter() as ChildProcess & {
      unrefCalled?: boolean;
    };
    child.unref = () => {
      child.unrefCalled = true;
    };
    let captured:
      | {
          readonly command: string;
          readonly args: readonly string[];
          readonly options: SpawnOptions;
        }
      | undefined;

    const observation = spawnDetachedServer(
      {
        binaryPath: "/extension/bin/bingo",
        args: ["-addr", "127.0.0.1:6060"],
        logPath: join(root, "logs", "server.log"),
      },
      () => undefined,
      (command, args, options) => {
        captured = { command, args, options };
        return child;
      },
    );

    assert.equal(captured?.command, "/extension/bin/bingo");
    assert.deepEqual(captured?.args, ["-addr", "127.0.0.1:6060"]);
    assert.equal(captured?.options.detached, true);
    assert.equal(captured?.options.shell, false);
    const stdio = captured?.options.stdio;
    assert.ok(Array.isArray(stdio));
    assert.equal(stdio[0], "ignore");
    assert.equal(typeof stdio[1], "number");
    assert.equal(stdio[1], stdio[2]);
    assert.equal(child.unrefCalled, true);
    assert.equal("kill" in observation, false);
    observation.stopObserving();
  });
});
