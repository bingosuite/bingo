import assert from "node:assert/strict";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import {
  existsSync,
  mkdirSync,
  readFileSync,
  rmSync,
} from "node:fs";
import { createServer, connect, type Socket } from "node:net";
import { basename, join, resolve } from "node:path";
import { parseHTML } from "linkedom";

import type { BingoServerConfiguration } from "../src/configuration.js";
import { probeBingoHealth } from "../src/health.js";
import { DebugInspectionController, type DebugSessionClient } from "../src/inspection.js";
import { decodeAction } from "../src/messages.js";
import type { DebugVariable, SessionModel, SessionViewModel } from "../src/model.js";
import { readSourceContext, SpawnSourceController } from "../src/source.js";
import { filterTree } from "../src/tree.js";
import { SessionRegistry } from "../src/registry.js";
import { defaultDelay, ServerManager } from "../src/serverManager.js";
import { spawnDetachedServer } from "../src/serverProcess.js";
import { mountConcurrencyView } from "../src/webviewApp.js";
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
  { name: "level1-loop", line: 8, local: "total", minimumDepth: 0 },
  { name: "level2-channel", line: 22, local: "output", minimumDepth: 1 },
  { name: "level3-worker-pool", line: 39, local: "jobs", minimumDepth: 1 },
  { name: "level4-pipeline", line: 63, local: "ctx", minimumDepth: 1 },
  { name: "level5-workflow", line: 83, local: "current", minimumDepth: 2 },
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
  const failures: unknown[] = [];
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
    let creationSnippets = 0;
    let expandedVariables = 0;
    for (const [index, example] of examples.entries()) {
      const result = await runExample(
        config,
        example,
        `debug-${String(index + 1)}`,
      );
      observedDepths.push(result.depth);
      creationSnippets += result.creationSnippet ? 1 : 0;
      expandedVariables += result.expandedVariable ? 1 : 0;
      assert.ok(
        result.depth >= example.minimumDepth,
        `${example.name} hierarchy depth ${String(result.depth)} is below ${String(example.minimumDepth)}`,
      );
      assert.ok(
        result.threads > 0,
        `${example.name} snapshot has no runtime threads`,
      );
      process.stdout.write(
        `[snapshot] ${example.name}: session=${result.sessionId} goroutines=${String(result.goroutines)} threads=${String(result.threads)} depth=${String(result.depth)} seq=${String(result.seq)} source=${String(result.creationSnippet)} expanded=${String(result.expandedVariable)}\n`,
      );
    }
    const firstDepth = observedDepths[0];
    const lastDepth = observedDepths.at(-1);
    if (firstDepth === undefined || lastDepth === undefined) {
      throw new Error("progressive examples produced no hierarchy results");
    }
    assert.ok(firstDepth <= lastDepth);
    assert.ok(creationSnippets > 0, "must render a real application's recorded go statement");
    assert.ok(expandedVariables > 0, "must expand at least one real structured local");

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
  } catch (error: unknown) {
    failures.push(error);
  } finally {
    try {
      manager?.dispose();
      const serverLog = join(scratch, "server.log");
      if (!succeeded && existsSync(serverLog)) {
        process.stderr.write(`[server log]\n${readFileSync(serverLog, "utf8")}\n`);
      }
      if (!succeeded && child?.pid !== undefined &&
        child.exitCode === null && child.signalCode === null) {
        try {
          process.kill(child.pid, "SIGKILL");
        } catch (error: unknown) {
          if (!isMissingProcess(error)) {
            failures.push(error);
          }
        }
        await waitForExit(child, 3000);
      }
    } catch (error: unknown) {
      failures.push(error);
    } finally {
      try {
        rmSync(scratch, { force: true, recursive: true });
      } catch (error: unknown) {
        failures.push(error);
      }
    }
  }
  throwFailures(failures, "packaged native E2E");
}

interface ExampleResult {
  readonly sessionId: string;
  readonly goroutines: number;
  readonly threads: number;
  readonly depth: number;
  readonly seq: number;
  readonly creationSnippet: boolean;
  readonly expandedVariable: boolean;
}

async function runExample(
  config: BingoServerConfiguration,
  example: (typeof examples)[number],
  debugSessionId: string,
): Promise<ExampleResult> {
  const { name, line } = example;
  const registry = new SessionRegistry();
  const { document } = parseHTML("<html><body><div id=app></div></body></html>");
  let client: DAPClient | undefined;
  let inspection: DebugInspectionController | undefined;
  let source: SpawnSourceController | undefined;
  let unsubscribeDAP: (() => void) | undefined;
  let unsubscribeRender: (() => void) | undefined;
  const failures: unknown[] = [];
  let result: ExampleResult | undefined;
  try {
    client = await DAPClient.open(config.dapEndpoint.host, config.dapEndpoint.port, debugSessionId);
    const connectedClient = client;
    inspection = new DebugInspectionController(
      registry,
      (id) => id === debugSessionId ? connectedClient : undefined,
    );
    source = new SpawnSourceController(
      registry,
      (location) => readSourceContext(location, { trusted: true, roots: [repositoryRoot] }),
    );
    const controller = inspection;
    unsubscribeDAP = client.onEvent((message) => {
      if (message.event === "stopped") {
        controller.stopped(debugSessionId, stoppedThreadId(message));
      }
    });
    const render = mountConcurrencyView(document, {
      postMessage(message) {
        if (message.type !== "action") {
          return;
        }
        const { context, action } = decodeAction(message);
        assert.equal(context.debugSessionId, debugSessionId);
        assert.equal(context.revision, registry.viewModel.revision);
        switch (action.type) {
          case "selectGoroutine":
            registry.selectGoroutine(action.id);
            break;
          case "selectFrame":
            controller.selectFrame(action.id);
            break;
          case "expandVariable":
            controller.expandVariable(action.reference);
            break;
          case "refreshInspection":
            controller.refresh();
            break;
          default:
            throw new Error(`unexpected native DOM action ${action.type}`);
        }
      },
    });
    unsubscribeRender = registry.onChange(render);
    render(registry.viewModel);

    await client.customRequest("initialize", {
      adapterID: "bingo",
      linesStartAt1: true,
      columnsStartAt1: true,
    });
    const program = resolve(repositoryRoot, "build", "examples", name);
    const sourceFile = resolve(repositoryRoot, "examples", name, "main.go");
    const launch = client.request("launch", { program, stopOnEntry: true });
    const custom = await client.message(
      (message) => message.type === "event" && message.event === sessionDAPEventName,
    );
    const announcement = decodeSessionAnnouncement(String(custom.event), custom.body);
    assert.ok(announcement, `${name} omitted the session announcement`);
    const sessionId = announcement.sessionId;
    await client.message(
      (message) => message.type === "event" && message.event === "initialized",
    );
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
      (model) => model.snapshot !== undefined && model.clients === 2,
      10_000,
    );
    const breakpointResponse = await client.customRequest("setBreakpoints", {
      source: { name: basename(program), path: sourceFile },
      breakpoints: [{ line }],
      sourceModified: false,
    });
    const breakpoint = record(breakpointResponse, "setBreakpoints").breakpoints;
    assert.ok(Array.isArray(breakpoint) && breakpoint.length === 1);
    const resolvedBreakpoint = record(breakpoint[0], "breakpoint");
    assert.equal(resolvedBreakpoint.verified, true, `${name} breakpoint was not verified`);
    const configured = client.request("configurationDone", {});
    await Promise.all([client.response(configured), client.response(launch)]);
    const entry = await client.message(
      (message) => isStop(message, "entry"),
    );
    await waitForModel(
      registry,
      `${name} entry inspection settled`,
      (model) => inspectionSettled(model),
      15_000,
    );
    await client.settleRequests();
    await client.customRequest("continue", { threadId: stoppedThreadId(entry) });
    const breakpointStop = await client.message(
      (message) => isStop(message, "breakpoint"),
      30_000,
    );
    const stopped = await waitForModel(
      registry,
      `${name} breakpoint snapshot`,
      (model) =>
        model.sessionState === "suspended" &&
        model.snapshot !== undefined &&
        model.snapshot !== initial.snapshot &&
        model.lastSeq > initial.lastSeq,
      20_000,
    );
    const snapshot = stopped.snapshot!;
    const depth = applicationDepth(snapshot, name);
    const stackThreadId = stoppedThreadId(breakpointStop);
    if (stackThreadId > 0) {
      assert.ok(snapshot.goroutines.some((goroutine) => goroutine.id === stackThreadId));
      click(document, `.tree-node[data-goid="${String(stackThreadId)}"]`);
    }
    const inspected = await waitForModel(
      registry,
      `${name} stack and locals`,
      (model) => {
        assertInspectionHealthy(model);
        return model.inspection.stackStatus === "ready" && model.inspection.localsStatus === "ready";
      },
      15_000,
    );
    assert.equal(inspected.inspection.targetGoroutine, stackThreadId);
    const top = inspected.inspection.frames[0];
    assert.ok(top, `${name} returned no stack frames`);
    assert.equal(top.file, sourceFile);
    assert.equal(top.line, resolvedBreakpoint.line ?? line);
    assert.equal(top.name, name === "level5-workflow" ? "main.inventoryStage" : "main.main");
    assert.equal(
      document.querySelector(".stack-frame .frame-name")?.textContent,
      top.name,
    );
    assert.ok(
      document.querySelector(".stack-frame .source-link")?.textContent?.includes(`:${String(top.line)}`),
      `${name} did not render its real frame location`,
    );
    assert.equal(
      document.querySelector(".stack-frame .source-link")?.getAttribute("title"),
      `${sourceFile}:${String(top.line)}`,
    );
    assert.ok(
      inspected.inspection.variables.some((variable) => variable.name === example.local),
      `${name} is missing expected local ${example.local}`,
    );
    assertVariablesDisplayed(document.querySelector(".variable-tree"), inspected.inspection.variables);

    // Exercise the DOM-to-controller frame route against the real adapter, not
    // a second independently queried stack that bypasses the displayed model.
    click(document, `.frame-name[data-frame-id="${String(top.id)}"]`);
    const reselected = await waitForModel(
      registry,
      `${name} selected frame locals`,
      (model) => {
        assertInspectionHealthy(model);
        return model.inspection.selectedFrameId === top.id && model.inspection.localsStatus === "ready";
      },
      15_000,
    );
    assertVariablesDisplayed(document.querySelector(".variable-tree"), reselected.inspection.variables);
    const expandable = reselected.inspection.variables.find((variable) => variable.variablesReference > 0);
    if (expandable !== undefined) {
      const reference = String(expandable.variablesReference);
      click(document, `.variable-expand[data-reference="${reference}"]`);
      const expanded = await waitForModel(
        registry,
        `${name} ${expandable.name} expansion`,
        (model) => {
          assertInspectionHealthy(model);
          return Object.hasOwn(model.inspection.variablesByReference, reference);
        },
        15_000,
      );
      const children = expanded.inspection.variablesByReference[reference];
      assert.ok(children !== undefined && children.length > 0, "expandable local must have children");
      const button = document.querySelector(`.variable-expand[data-reference="${reference}"]`);
      assert.equal(button?.getAttribute("aria-expanded"), "true");
      assertVariablesDisplayed(button?.closest(".variable")?.querySelector("ul"), children);
    }

    const created = snapshot.goroutines.find(
      (goroutine) =>
        goroutine.parentId > 0 &&
        goroutine.id !== snapshot.current &&
        goroutine.id !== stackThreadId &&
        goroutine.createdLoc.file === sourceFile,
    );
    if (example.minimumDepth > 0) {
      assert.ok(created, `${name} must expose a non-current application-created goroutine`);
    }
    if (created !== undefined) {
      assert.ok(snapshot.goroutines.some((goroutine) => goroutine.id === created.parentId));
      click(document, `.tree-node[data-goid="${String(created.id)}"]`);
      const selected = await waitForModel(
        registry,
        `${name} creation source`,
        (model) =>
          model.selectedGoroutine === created.id &&
          model.spawnSource.status !== "loading",
        10_000,
      );
      assert.equal(selected.spawnSource.status, "ready", selected.spawnSource.message);
      assert.equal(
        document.querySelector(".inspector-title")?.textContent,
        `g${String(created.id)} · ${created.status}`,
      );
      assert.equal(
        document.querySelector(`.tree-node[data-goid="${String(created.id)}"]`)?.getAttribute("aria-selected"),
        "true",
      );
      assert.equal(inspectorField(document, "Parent")?.textContent, `g${String(created.parentId)}`);
      assert.ok(
        inspectorField(document, "Created")?.textContent?.includes(`main.go:${String(created.createdLoc.line)}`),
        `${name} did not display its recorded creation location`,
      );
      const recordedSource = `${sourceFile}:${String(created.createdLoc.line)}`;
      assert.equal(
        inspectorField(document, "Created")?.querySelector(".source-link")?.getAttribute("title"),
        recordedSource,
      );
      assert.equal(
        document.querySelector(".spawn-source .source-link")?.getAttribute("title"),
        recordedSource,
      );
      const highlighted = selected.spawnSource.lines.filter((sourceLine) => sourceLine.highlighted);
      assert.equal(highlighted.length, 1);
      assert.equal(highlighted[0]?.number, created.createdLoc.line);
      const recordedLine = readFileSync(sourceFile, "utf8").split(/\r?\n/u)[created.createdLoc.line - 1];
      assert.equal(highlighted[0]?.text, recordedLine);
      assert.match(recordedLine ?? "", /\bgo\s+/u, "creation source must highlight the real go statement");
      const rendered = document.querySelectorAll(".spawn-source .creation-line");
      assert.equal(rendered.length, 1);
      assert.equal(rendered[0]?.getAttribute("data-line"), String(created.createdLoc.line));
      assert.equal(rendered[0]?.getAttribute("aria-current"), "location");
      assert.equal(rendered[0]?.textContent, `${String(created.createdLoc.line)}  ${recordedLine}\n`);
      if (stackThreadId > 0) {
        assert.equal(selected.inspection.stackStatus, "unavailable");
        assert.equal(document.querySelectorAll(".stack-frame").length, 0);
        assert.ok(
          document.querySelector(".debug-inspection")?.textContent?.includes(
            `only for the stopped goroutine g${String(stackThreadId)}`,
          ),
          "selecting a non-current goroutine must not misattribute the stopped stack",
        );
      }
    }
    if (name === "level5-workflow") {
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
    await waitForModel(
      registry,
      `${name} inspection settled before termination`,
      (model) => inspectionSettled(model),
      15_000,
    );
    await client.settleRequests();

    // Removing the last client would conceal a missing DAP termination event:
    // keep the real observer joined until one terminate completes independently.
    assert.equal(registry.activeModel()?.connection, "connected");
    assert.equal(registry.activeModel()?.clients, 2);
    assert.equal(client.terminatedCount, 0);
    const terminated = client.message(
      (message) => message.type === "event" && message.event === "terminated",
      10_000,
    );
    const idle = waitForModel(
      registry,
      `${name} terminated session state`,
      (model) => model.sessionState === "idle",
      10_000,
    );
    const [, , ended] = await Promise.all([
      client.customRequest("terminate", { restart: false }),
      terminated,
      idle,
    ]);
    assert.equal(client.terminatedCount, 1, "one terminate must emit exactly one terminated");
    assert.equal(ended.connection, "connected", "observer must remain joined through termination");
    assert.equal(ended.clients, 2, "termination must not depend on observer disconnect");
    assert.equal(ended.inspection.stackStatus, "idle");
    await client.customRequest("disconnect", { terminateDebuggee: false });
    await client.closed(5000);
    client.assertHealthy();
    assert.equal(client.terminatedCount, 1, "disconnect must not duplicate terminated");
    process.stdout.write(
      `[terminate] ${name}: terminated=${String(client.terminatedCount)} exited=${JSON.stringify(client.exitCodes)} observer=${ended.connection}/${ended.sessionState}\n`,
    );
    result = {
      sessionId,
      goroutines: snapshot.goroutines.length,
      threads: snapshot.threads.length,
      depth,
      seq: stopped.lastSeq,
      creationSnippet: created !== undefined,
      expandedVariable: expandable !== undefined,
    };
  } catch (error: unknown) {
    failures.push(error);
  } finally {
    unsubscribeDAP?.();
    inspection?.dispose();
    source?.dispose();
    unsubscribeRender?.();
    registry.remove(debugSessionId);
    registry.dispose();
    document.getElementById("app")?.replaceChildren();
    client?.close();
    try {
      await Promise.all([
        client?.closed(5000),
        waitForNoSessions(config.managementEndpoint, 10_000),
      ]);
    } catch (error: unknown) {
      failures.push(error);
    }
  }
  throwFailures(failures, name);
  assert.ok(result);
  return result;
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

function assertInspectionHealthy(model: SessionViewModel): void {
  assert.notEqual(model.inspection.stackStatus, "error", model.inspection.stackMessage);
  assert.notEqual(model.inspection.localsStatus, "error", model.inspection.localsMessage);
  assert.equal(model.inspection.localsMessage, "");
}

function inspectionSettled(model: SessionViewModel): boolean {
  assertInspectionHealthy(model);
  return model.inspection.stackStatus !== "loading" &&
    model.inspection.localsStatus !== "loading" &&
    model.inspection.loadingReferences.length === 0;
}

function assertVariablesDisplayed(
  container: Element | null | undefined,
  variables: readonly DebugVariable[],
): void {
  assert.ok(container, "real variables must have a DOM tree");
  const rows = [...container.children].map((item) => item.querySelector(".variable-row"));
  assert.deepEqual(
    rows.map((row) => ({
      name: row?.querySelector(".variable-name")?.textContent,
      value: row?.querySelector(".variable-value")?.textContent,
      type: row?.querySelector(".variable-type")?.textContent ?? "",
    })),
    variables.map(({ name, value, type }) => ({ name, value, type })),
  );
}

function click(document: Document, selector: string): void {
  const element = document.querySelector(selector);
  assert.ok(element, `missing native DOM action ${selector}`);
  assert.ok(document.defaultView);
  element.dispatchEvent(new document.defaultView.Event("click"));
}

function inspectorField(document: Document, label: string): Element | null | undefined {
  return [...document.querySelectorAll(".inspector > dl > dt")]
    .find((term) => term.textContent === label)?.nextElementSibling;
}

function record(value: unknown, label: string): Record<string, unknown> {
  assert.ok(value !== null && typeof value === "object" && !Array.isArray(value), `${label} must be an object`);
  return value as Record<string, unknown>;
}

function isStop(message: DAPMessage, reason: string): boolean {
  return message.type === "event" && message.event === "stopped" &&
    record(message.body, "stopped body").reason === reason;
}

function stoppedThreadId(message: DAPMessage): number {
  const threadId = record(message.body, "stopped body").threadId;
  if (threadId === undefined) {
    return 0;
  }
  assert.ok(typeof threadId === "number" && Number.isSafeInteger(threadId) && threadId >= 0);
  return threadId;
}

async function waitForNoSessions(
  endpoint: { readonly host: string; readonly port: number },
  timeoutMs: number,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const decoded = await healthDocument(endpoint, Math.min(2000, deadline - Date.now()));
    if (decoded.sessionCount === 0) {
      return;
    }
    await delay(20);
  }
  throw new Error("timed out waiting for the previous managed session to close");
}

interface DAPMessage {
  readonly type?: unknown;
  readonly event?: unknown;
  readonly command?: unknown;
  readonly request_seq?: unknown;
  readonly body?: unknown;
  readonly [key: string]: unknown;
}

class DAPClient implements DebugSessionClient {
  readonly #events = new EventEmitter();
  readonly #messages: DAPMessage[] = [];
  readonly #exitCodes: number[] = [];
  readonly #pendingRequests = new Set<Promise<DAPMessage>>();
  #buffer = Buffer.alloc(0);
  #seq = 1;
  #failure: Error | undefined;
  #unexpectedFailure: Error | undefined;
  #socketClosed = false;
  #terminatedCount = 0;
  readonly #onData = (chunk: Buffer): void => {
    try {
      this.#buffer = Buffer.concat([this.#buffer, chunk]);
      this.#parse();
    } catch (error: unknown) {
      this.#unexpectedFailure = error instanceof Error ? error : new Error(String(error));
      this.#fail(this.#unexpectedFailure);
      this.socket.destroy();
    }
  };
  readonly #onError = (error: Error): void => {
    this.#unexpectedFailure ??= error;
    this.#fail(error);
  };
  readonly #onClose = (): void => {
    this.#socketClosed = true;
    this.#fail(new Error("DAP socket closed"));
    this.socket.off("data", this.#onData);
    this.socket.off("error", this.#onError);
    this.socket.off("close", this.#onClose);
    this.#events.removeAllListeners();
    this.#messages.length = 0;
    this.#buffer = Buffer.alloc(0);
  };

  private constructor(private readonly socket: Socket, public readonly id: string) {
    socket.on("data", this.#onData);
    socket.on("error", this.#onError);
    socket.on("close", this.#onClose);
  }

  public static async open(host: string, port: number, id: string): Promise<DAPClient> {
    const socket = connect({ host, port });
    const client = new DAPClient(socket, id);
    try {
      await waitForSignal(socket, "connect", 5000, "DAP connection");
      return client;
    } catch (error: unknown) {
      client.close();
      await client.closed(5000);
      throw error;
    }
  }

  public get terminatedCount(): number {
    return this.#terminatedCount;
  }

  public get exitCodes(): readonly number[] {
    return [...this.#exitCodes];
  }

  public assertHealthy(): void {
    if (this.#unexpectedFailure !== undefined) {
      throw this.#unexpectedFailure;
    }
  }

  public onEvent(listener: (message: DAPMessage) => void): () => void {
    this.#events.on("event", listener);
    return () => {
      this.#events.off("event", listener);
    };
  }

  public request(command: string, args: unknown = {}): number {
    if (this.#failure !== undefined) {
      throw this.#failure;
    }
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

  public async customRequest(command: string, args?: unknown): Promise<unknown> {
    const pending = this.response(this.request(command, args));
    this.#pendingRequests.add(pending);
    try {
      const response = await pending;
      return response.body;
    } finally {
      this.#pendingRequests.delete(pending);
    }
  }

  public async settleRequests(): Promise<void> {
    // The controller can cancel an obsolete generation without canceling its
    // request on the wire. Retire that reply before allowing run control.
    await Promise.all([...this.#pendingRequests]);
  }

  public async response(requestSeq: number): Promise<DAPMessage> {
    const response = await this.message(
      (message) =>
        message.type === "response" && message.request_seq === requestSeq,
    );
    assert.equal(response.success, true, `DAP ${String(response.command)} failed: ${JSON.stringify(response)}`);
    return response;
  }

  public async message(
    predicate: (message: DAPMessage) => boolean,
    timeoutMs = 15_000,
  ): Promise<DAPMessage> {
    if (this.#failure !== undefined) {
      throw this.#failure;
    }
    const existing = this.#messages.findIndex(predicate);
    if (existing >= 0) {
      return this.#messages.splice(existing, 1)[0]!;
    }
    return new Promise((resolveMessage, reject) => {
      const cleanup = (): void => {
        clearTimeout(timeout);
        this.#events.off("message", receive);
        this.#events.off("failure", failed);
      };
      const failed = (error: Error): void => {
        cleanup();
        reject(error);
      };
      const timeout = setTimeout(() => {
        failed(new Error(`timed out waiting for DAP message; queued=${JSON.stringify(
          this.#messages.slice(-8).map(({ type, event, command, request_seq, success }) => ({
            type, event, command, request_seq, success,
          })),
        )}`));
      }, timeoutMs);
      const receive = (message: DAPMessage): void => {
        try {
          if (!predicate(message)) {
            return;
          }
          cleanup();
          const index = this.#messages.indexOf(message);
          if (index >= 0) {
            this.#messages.splice(index, 1);
          }
          resolveMessage(message);
        } catch (error: unknown) {
          failed(error instanceof Error ? error : new Error(String(error)));
        }
      };
      this.#events.on("message", receive);
      this.#events.on("failure", failed);
    });
  }

  public async closed(timeoutMs: number): Promise<void> {
    if (!this.#socketClosed) {
      await waitForSignal(this.socket, "close", timeoutMs, "DAP socket cleanup");
    }
  }

  public close(): void {
    this.#fail(new Error("DAP client disposed"));
    this.socket.destroy();
  }

  #fail(error: Error): void {
    this.#failure ??= error;
    this.#events.emit("failure", this.#failure);
  }

  #parse(): void {
    for (;;) {
      const headerEnd = this.#buffer.indexOf("\r\n\r\n");
      if (headerEnd < 0) {
        assert.ok(this.#buffer.length <= 8192, "DAP header exceeded test budget");
        return;
      }
      assert.ok(headerEnd <= 8192, "DAP header exceeded test budget");
      const header = this.#buffer.subarray(0, headerEnd).toString("ascii");
      const match = /(?:^|\r\n)Content-Length: (\d+)(?:\r\n|$)/iu.exec(header);
      if (match?.[1] === undefined) {
        throw new Error("DAP response omitted Content-Length");
      }
      const length = Number(match[1]);
      assert.ok(Number.isSafeInteger(length) && length > 0 && length <= 8 * 1024 * 1024,
        "DAP body exceeded test budget");
      const bodyStart = headerEnd + 4;
      if (this.#buffer.length < bodyStart + length) {
        return;
      }
      const raw = this.#buffer.subarray(bodyStart, bodyStart + length);
      this.#buffer = this.#buffer.subarray(bodyStart + length);
      const message = record(JSON.parse(raw.toString("utf8")), "DAP message") as DAPMessage;
      assert.ok(message.type === "event" || message.type === "response");
      if (message.type === "event") {
        if (message.event === "terminated") {
          this.#terminatedCount += 1;
        } else if (message.event === "exited") {
          const code = record(message.body, "exited body").exitCode;
          assert.ok(typeof code === "number" && Number.isSafeInteger(code), "exited must preserve an integer exit code");
          assert.equal(this.#exitCodes.length, 0, "one process must not emit duplicate exited events");
          this.#exitCodes.push(code);
        }
        this.#events.emit("event", message);
      }
      this.#messages.push(message);
      this.#events.emit("message", message);
      assert.ok(this.#messages.length <= 512, "unconsumed DAP history exceeded test budget");
    }
  }
}

async function waitForModel(
  registry: SessionRegistry,
  phase: string,
  predicate: (model: SessionViewModel) => boolean,
  timeoutMs: number,
): Promise<SessionViewModel> {
  return new Promise<SessionViewModel>((resolveModel, reject) => {
    const cleanup = (): void => {
      clearTimeout(timeout);
      unsubscribe();
    };
    const timeout = setTimeout(() => {
      cleanup();
      const model = registry.activeModel();
      reject(
        new Error(
          `timed out waiting for ${phase}: ${JSON.stringify({
            state: model?.sessionState,
            connection: model?.connection,
            error: model?.error,
            seq: model?.lastSeq,
            inspection: model === undefined ? undefined : registry.inspectionFor(model.debugSessionId),
          })}`,
        ),
      );
    }, timeoutMs);
    const check = (): void => {
      try {
        const model = registry.viewModel.sessions.find(
          (session) => session.debugSessionId === registry.viewModel.activeDebugSessionId,
        );
        if (model === undefined) {
          throw new Error(`${phase}: observed session was removed`);
        }
        assert.equal(model.error, "", `${phase}: telemetry error`);
        assert.notEqual(model.connection, "error", `${phase}: observer failed`);
        assert.notEqual(model.connection, "closed", `${phase}: observer closed`);
        assert.equal(model.seqGap, "", `${phase}: telemetry sequence gap`);
        if (predicate(model)) {
          cleanup();
          resolveModel(model);
        }
      } catch (error: unknown) {
        cleanup();
        reject(error instanceof Error ? error : new Error(String(error)));
      }
    };
    const unsubscribe = registry.onChange(check);
    check();
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
  const decoded = await healthDocument(endpoint, 2000);
  const address = record(decoded.dap, "health DAP").address;
  assert.equal(typeof address, "string");
  const port = Number((address as string).slice((address as string).lastIndexOf(":") + 1));
  assert.ok(Number.isSafeInteger(port) && port > 0 && port <= 65_535);
  return port;
}

async function healthDocument(
  endpoint: { readonly host: string; readonly port: number },
  timeoutMs: number,
): Promise<Record<string, unknown>> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), Math.max(1, timeoutMs));
  try {
    const response = await fetch(
      `http://${endpoint.host}:${String(endpoint.port)}/api/health`,
      { headers: { "Cache-Control": "no-cache" }, signal: controller.signal },
    );
    assert.equal(response.status, 200);
    return record(await response.json(), "health");
  } finally {
    clearTimeout(timer);
    controller.abort();
  }
}

async function freePort(): Promise<number> {
  const server = createServer();
  try {
    server.listen(0, "127.0.0.1");
    await waitForSignal(server, "listening", 5000, "port allocation");
    const address = server.address();
    assert.ok(address !== null && typeof address === "object");
    return address.port;
  } finally {
    const closed = waitForSignal(server, "close", 5000, "port allocation cleanup");
    server.close();
    await closed;
  }
}

async function waitForExit(process: ChildProcess, timeoutMs: number): Promise<void> {
  if (process.exitCode !== null || process.signalCode !== null) {
    return;
  }
  await waitForSignal(process, "exit", timeoutMs, "managed server exit");
}

function waitForSignal(
  emitter: EventEmitter,
  event: string,
  timeoutMs: number,
  phase: string,
): Promise<void> {
  return new Promise((resolveSignal, reject) => {
    const cleanup = (): void => {
      clearTimeout(timer);
      emitter.off(event, ready);
      emitter.off("error", failed);
      if (event !== "close") {
        emitter.off("close", closed);
      }
    };
    const ready = (): void => {
      cleanup();
      resolveSignal();
    };
    const failed = (error: Error): void => {
      cleanup();
      reject(error);
    };
    const closed = (): void => {
      failed(new Error(`${phase} closed before ${event}`));
    };
    const timer = setTimeout(() => {
      failed(new Error(`timed out waiting for ${phase}`));
    }, timeoutMs);
    emitter.once(event, ready);
    emitter.once("error", failed);
    if (event !== "close") {
      emitter.once("close", closed);
    }
  });
}

function isMissingProcess(error: unknown): boolean {
  return error !== null && typeof error === "object" &&
    "code" in error && error.code === "ESRCH";
}

function throwFailures(failures: readonly unknown[], phase: string): void {
  if (failures.length === 1) {
    throw failures[0];
  }
  if (failures.length > 1) {
    throw new AggregateError(failures, `${phase}: ${failures.map((failure) =>
      failure instanceof Error ? failure.stack ?? failure.message : String(failure),
    ).join("\n")}`);
  }
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
