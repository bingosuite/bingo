import assert from "node:assert/strict";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { EventEmitter, once } from "node:events";
import {
  existsSync,
  mkdirSync,
  readFileSync,
  rmSync,
} from "node:fs";
import { createServer, connect, type Socket } from "node:net";
import { basename, join, resolve } from "node:path";

import type { BingoServerConfiguration } from "../src/configuration.js";
import { probeBingoHealth } from "../src/health.js";
import type { SessionModel } from "../src/model.js";
import { filterTree } from "../src/tree.js";
import { SessionRegistry } from "../src/registry.js";
import { defaultDelay, ServerManager } from "../src/serverManager.js";
import { spawnDetachedServer } from "../src/serverProcess.js";
import {
  decodeSessionAnnouncement,
  sessionDAPEventName,
} from "../src/sessionEvent.js";

const repositoryRoot = resolve(process.cwd(), "../..");
const target = packageTarget();
if (target === undefined) {
  throw new Error(`unsupported packaged E2E host ${process.platform}/${process.arch}`);
}

const examples = [
  { name: "level1-loop", line: 8, minimumDepth: 0 },
  { name: "level2-channel", line: 23, minimumDepth: 1 },
  { name: "level3-worker-pool", line: 44, minimumDepth: 1 },
  { name: "level4-pipeline", line: 65, minimumDepth: 1 },
  { name: "level5-workflow", line: 83, minimumDepth: 2 },
] as const;

function packageTarget(): "darwin-arm64" | "linux-x64" | undefined {
  if (process.platform === "darwin" && process.arch === "arm64") {
    return "darwin-arm64";
  }
  if (process.platform === "linux" && process.arch === "x64") {
    return "linux-x64";
  }
  return undefined;
}

void main().catch((error: unknown) => {
  process.stderr.write(
    `packaged concurrency E2E failed: ${
      error instanceof Error ? error.stack : String(error)
    }\n`,
  );
  process.exitCode = 1;
});

async function main(): Promise<void> {
  const scratch = resolve(
    repositoryRoot,
    "dist",
    `.concurrency-e2e-${target}-${String(process.pid)}`,
  );
  rmSync(scratch, { force: true, recursive: true });
  mkdirSync(scratch, { recursive: true });
  let child: ChildProcess | undefined;
  let manager: ServerManager | undefined;
  let succeeded = false;
  try {
    const vsix = resolve(repositoryRoot, "dist", `bingo-${target}.vsix`);
    const extracted = join(scratch, "vsix");
    mkdirSync(extracted);
    run("unzip", ["-q", vsix, "-d", extracted]);
    const binary = join(extracted, "extension", "bin", "bingo");
    if (process.platform === "darwin") {
      run("codesign", ["--verify", "--strict", binary]);
    }

    const managementPort = await freePort();
    const dapPort = await freePort();
    const config: BingoServerConfiguration = {
      mode: "auto",
      managementEndpoint: { host: "127.0.0.1", port: managementPort },
      dapEndpoint: { host: "127.0.0.1", port: dapPort },
      readyTimeoutMs: 10_000,
      idleTimeoutMs: 1500,
    };
    let spawns = 0;
    manager = new ServerManager({
      probe: probeBingoHealth,
      resolveBinary: () => Promise.resolve(binary),
      spawnServer(request, onOutcome) {
        spawns += 1;
        return spawnDetachedServer(request, onOutcome, (command, args, options) => {
          child = spawn(command, args, options);
          return child;
        });
      },
      delay: defaultDelay,
      now: Date.now,
      runtime: { platform: process.platform, arch: process.arch },
      logPathFor: () => Promise.resolve(join(scratch, "server.log")),
      log: (message) => {
        process.stdout.write(`[manager] ${message}\n`);
      },
    });
    await manager.ensureServer(config);
    const firstHealth = await health(config.managementEndpoint);
    await new Promise((resolveReady) => setImmediate(resolveReady));
    await manager.ensureServer(config);
    const reusedHealth = await health(config.managementEndpoint);
    assert.equal(
      spawns,
      1,
      "compatible server must be reused without a competing spawn",
    );
    assert.equal(reusedHealth.instanceId, firstHealth.instanceId);

    const observedDepths: number[] = [];
    for (const [index, example] of examples.entries()) {
      const result = await runExample(
        config,
        example.name,
        example.line,
        `debug-${String(index + 1)}`,
      );
      observedDepths.push(result.depth);
      assert.ok(
        result.depth >= example.minimumDepth,
        `${example.name} hierarchy depth ${String(result.depth)} is below ${String(example.minimumDepth)}`,
      );
      assert.ok(
        result.threads > 0,
        `${example.name} snapshot has no runtime threads`,
      );
      process.stdout.write(
        `[snapshot] ${example.name}: session=${result.sessionId} goroutines=${String(result.goroutines)} threads=${String(result.threads)} depth=${String(result.depth)} seq=${String(result.seq)}\n`,
      );
    }
    const firstDepth = observedDepths[0];
    const lastDepth = observedDepths.at(-1);
    if (firstDepth === undefined || lastDepth === undefined) {
      throw new Error("progressive examples produced no hierarchy results");
    }
    assert.ok(firstDepth <= lastDepth);

    manager.dispose();
    if (child === undefined) {
      throw new Error("managed server was not spawned");
    }
    await waitForExit(child, config.idleTimeoutMs + 10_000);
    assert.equal(
      child.exitCode,
      0,
      "managed server must self-exit cleanly after idle",
    );
    process.stdout.write(
      `[idle] instance=${firstHealth.instanceId} self-exited after all sessions closed\n`,
    );
    succeeded = true;
  } finally {
    if (!succeeded) {
      manager?.dispose();
    }
    const serverLog = join(scratch, "server.log");
    if (!succeeded && existsSync(serverLog)) {
      process.stderr.write(`[server log]\n${readFileSync(serverLog, "utf8")}\n`);
    }
    if (!succeeded && child?.pid !== undefined && child.exitCode === null) {
      try {
        process.kill(child.pid, "SIGKILL");
      } catch {
        // The exact test-owned server may already have exited.
      }
      await Promise.race([once(child, "exit"), delay(3000)]);
    }
    rmSync(scratch, { force: true, recursive: true });
  }
}

async function runExample(
  config: BingoServerConfiguration,
  name: string,
  line: number,
  debugSessionId: string,
): Promise<{
  readonly sessionId: string;
  readonly goroutines: number;
  readonly threads: number;
  readonly depth: number;
  readonly seq: number;
}> {
  const client = await DAPClient.open(config.dapEndpoint.host, config.dapEndpoint.port);
  const initialize = client.request("initialize", {
    adapterID: "bingo",
    linesStartAt1: true,
    columnsStartAt1: true,
  });
  await client.response(initialize);
  const program = resolve(repositoryRoot, "build", "examples", name);
  const launch = client.request("launch", { program, stopOnEntry: true });
  const custom = await client.message(
    (message) =>
      message.type === "event" && message.event === sessionDAPEventName,
  );
  const announcement = decodeSessionAnnouncement(
    String(custom.event),
    custom.body,
  );
  assert.notEqual(announcement, undefined);
  const sessionId = announcement!.sessionId;
  await client.message(
    (message) => message.type === "event" && message.event === "initialized",
  );

  const registry = new SessionRegistry();
  assert.equal(
    registry.add({
      debugSessionId,
      debugSessionName: name,
      sessionId,
      managementEndpoint: config.managementEndpoint,
    }),
    true,
  );
  const initial = await waitForModel(
    registry,
    `${name} initial snapshot`,
    (model) => model.snapshot !== undefined,
    10_000,
  );

  const breakpoint = client.request("setBreakpoints", {
    source: {
      name: basename(program),
      path: resolve(repositoryRoot, "examples", name, "main.go"),
    },
    breakpoints: [{ line }],
    sourceModified: false,
  });
  const breakpointResponse = await client.response(breakpoint);
  assert.equal(
    ((breakpointResponse.body as { readonly breakpoints?: readonly { verified?: boolean }[] })
      .breakpoints?.[0]?.verified),
    true,
  );
  const configured = client.request("configurationDone", {});
  await Promise.all([client.response(configured), client.response(launch)]);
  await client.message(
    (message) =>
      message.type === "event" &&
      message.event === "stopped" &&
      (message.body as { readonly reason?: unknown } | undefined)?.reason ===
        "entry",
  );
  const continued = client.request("continue", { threadId: 1 });
  await client.response(continued);
  await client.message(
    (message) =>
      message.type === "event" &&
      message.event === "stopped" &&
      (message.body as { readonly reason?: unknown } | undefined)?.reason ===
        "breakpoint",
    30_000,
  );

  const previousSeq = initial.lastSeq;
  registry.refresh();
  const stopped = await waitForModel(
    registry,
    `${name} breakpoint snapshot`,
    (model) =>
      model.snapshot !== undefined &&
      model.snapshot !== initial.snapshot &&
      model.lastSeq > previousSeq,
    20_000,
  );
  const snapshot = stopped.snapshot!;
  const depth = applicationDepth(snapshot, name);
  if (name === "level5-workflow") {
    assert.ok(snapshot.goroutines.some((goroutine) => goroutine.parentId > 0));
    const selected = snapshot.goroutines.find((goroutine) => goroutine.parentId > 0)!;
    registry.selectGoroutine(selected.id);
    assert.equal(registry.activeModel()?.selectedGoroutine, selected.id);
    assert.ok(filterTree(stopped.tree, "inventory").nodes.length > 0);
    assert.match(registry.activeSnapshotJSON() ?? "", /"goroutines"/);
    const beforeRefresh = registry.activeModel()?.snapshot;
    registry.refresh();
    await waitForModel(
      registry,
      `${name} explicit refresh`,
      (model) => model.snapshot !== undefined && model.snapshot !== beforeRefresh,
      10_000,
    );
  }

  function applicationDepth(
    snapshot: NonNullable<SessionModel["snapshot"]>,
    example: string,
  ): number {
    const byID = new Map(snapshot.goroutines.map((goroutine) => [goroutine.id, goroutine]));
    const source = `/examples/${example}/`;
    let maximum = 0;
    for (const goroutine of snapshot.goroutines) {
      const locations = [
        goroutine.currentLoc.file,
        goroutine.startLoc.file,
        goroutine.createdLoc.file,
      ];
      if (!locations.some((file) => file.replaceAll("\\", "/").includes(source))) {
        continue;
      }
      let depth = 0;
      let current = goroutine;
      const seen = new Set<number>([current.id]);
      while (current.parentId > 0) {
        const parent = byID.get(current.parentId);
        if (parent === undefined || seen.has(parent.id)) {
          break;
        }
        seen.add(parent.id);
        depth += 1;
        current = parent;
      }
      maximum = Math.max(maximum, depth);
    }
    return maximum;
  }

  const disconnect = client.request("disconnect", { terminateDebuggee: true });
  await client.response(disconnect);
  client.close();
  registry.remove(debugSessionId);
  registry.dispose();
  return {
    sessionId,
    goroutines: snapshot.goroutines.length,
    threads: snapshot.threads.length,
    depth,
    seq: stopped.lastSeq,
  };
}

interface DAPMessage {
  readonly type?: unknown;
  readonly event?: unknown;
  readonly command?: unknown;
  readonly request_seq?: unknown;
  readonly body?: unknown;
  readonly [key: string]: unknown;
}

class DAPClient {
  readonly #events = new EventEmitter();
  readonly #messages: DAPMessage[] = [];
  #buffer = Buffer.alloc(0);
  #seq = 1;

  private constructor(private readonly socket: Socket) {
    socket.on("data", (chunk: Buffer) => {
      this.#buffer = Buffer.concat([this.#buffer, chunk]);
      this.#parse();
    });
  }

  public static async open(host: string, port: number): Promise<DAPClient> {
    const socket = connect({ host, port });
    await once(socket, "connect");
    return new DAPClient(socket);
  }

  public request(command: string, args: Record<string, unknown>): number {
    const seq = this.#seq++;
    const body = Buffer.from(
      JSON.stringify({ seq, type: "request", command, arguments: args }),
    );
    this.socket.write(
      Buffer.concat([
        Buffer.from(`Content-Length: ${String(body.length)}\r\n\r\n`),
        body,
      ]),
    );
    return seq;
  }

  public response(requestSeq: number): Promise<DAPMessage> {
    return this.message(
      (message) =>
        message.type === "response" && message.request_seq === requestSeq,
    );
  }

  public async message(
    predicate: (message: DAPMessage) => boolean,
    timeoutMs = 15_000,
  ): Promise<DAPMessage> {
    const existing = this.#messages.findIndex(predicate);
    if (existing >= 0) {
      return this.#messages.splice(existing, 1)[0]!;
    }
    return new Promise((resolveMessage, reject) => {
      const timeout = setTimeout(() => {
        this.#events.off("message", receive);
        reject(new Error("timed out waiting for DAP message"));
      }, timeoutMs);
      const receive = (message: DAPMessage): void => {
        if (!predicate(message)) {
          return;
        }
        clearTimeout(timeout);
        this.#events.off("message", receive);
        const index = this.#messages.indexOf(message);
        if (index >= 0) {
          this.#messages.splice(index, 1);
        }
        resolveMessage(message);
      };
      this.#events.on("message", receive);
    });
  }

  public close(): void {
    this.socket.end();
    this.socket.destroy();
  }

  #parse(): void {
    for (;;) {
      const headerEnd = this.#buffer.indexOf("\r\n\r\n");
      if (headerEnd < 0) {
        return;
      }
      const header = this.#buffer.subarray(0, headerEnd).toString("ascii");
      const match = /(?:^|\r\n)Content-Length: (\d+)(?:\r\n|$)/iu.exec(header);
      if (match?.[1] === undefined) {
        throw new Error("DAP response omitted Content-Length");
      }
      const length = Number(match[1]);
      const bodyStart = headerEnd + 4;
      if (this.#buffer.length < bodyStart + length) {
        return;
      }
      const raw = this.#buffer.subarray(bodyStart, bodyStart + length);
      this.#buffer = this.#buffer.subarray(bodyStart + length);
      const message = JSON.parse(raw.toString("utf8")) as DAPMessage;
      this.#messages.push(message);
      this.#events.emit("message", message);
    }
  }
}

async function waitForModel(
  registry: SessionRegistry,
  phase: string,
  predicate: (model: ReturnType<SessionRegistry["activeModel"]> & {}) => boolean,
  timeoutMs: number,
) {
  const current = registry.activeModel();
  if (current !== undefined && predicate(current)) {
    return {
      ...current,
      tree: registry.viewModel.sessions.find(
        (session) => session.debugSessionId === current.debugSessionId,
      )!.tree,
    };
  }
  return new Promise<
    ReturnType<typeof registry.activeModel> & { readonly tree: ReturnType<typeof filterTree> }
  >((resolveModel, reject) => {
    const timeout = setTimeout(() => {
      unsubscribe();
      reject(
        new Error(
          `timed out waiting for ${phase}: ${JSON.stringify(registry.activeModel())}`,
        ),
      );
    }, timeoutMs);
    const unsubscribe = registry.onChange(() => {
      const model = registry.activeModel();
      const view = registry.viewModel.sessions.find(
        (session) => session.debugSessionId === model?.debugSessionId,
      );
      if (model === undefined || view === undefined || !predicate(model)) {
        return;
      }
      clearTimeout(timeout);
      unsubscribe();
      resolveModel({ ...model, tree: view.tree });
    });
  });
}

async function health(endpoint: {
  readonly host: string;
  readonly port: number;
}): Promise<{ readonly instanceId: string }> {
  const controller = new AbortController();
  const result = await probeBingoHealth(
    endpoint,
    { host: "127.0.0.1", port: await configuredDAPPort(endpoint) },
    2000,
    controller.signal,
  );
  if (result.kind !== "compatible") {
    throw new Error(`server health is ${result.kind}`);
  }
  return { instanceId: result.health.instanceId };
}

async function configuredDAPPort(endpoint: {
  readonly host: string;
  readonly port: number;
}): Promise<number> {
  const response = await fetch(`http://${endpoint.host}:${String(endpoint.port)}/api/health`);
  const decoded = (await response.json()) as {
    readonly dap: { readonly address: string };
  };
  return Number(decoded.dap.address.slice(decoded.dap.address.lastIndexOf(":") + 1));
}

async function freePort(): Promise<number> {
  const server = createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert.notEqual(address, null);
  assert.equal(typeof address, "object");
  const port = (address as { readonly port: number }).port;
  server.close();
  await once(server, "close");
  return port;
}

async function waitForExit(process: ChildProcess, timeoutMs: number): Promise<void> {
  if (process.exitCode !== null) {
    return;
  }
  await Promise.race([
    once(process, "exit").then(() => undefined),
    delay(timeoutMs).then(() => {
      throw new Error("managed server did not self-exit");
    }),
  ]);
}

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolveDelay) => {
    setTimeout(resolveDelay, milliseconds);
  });
}

function run(command: string, args: readonly string[]): void {
  const result = spawnSync(command, args, { stdio: "inherit" });
  if (result.error !== undefined) {
    throw result.error;
  }
  if (result.status !== 0) {
    throw new Error(`${command} exited with ${String(result.status)}`);
  }
}
