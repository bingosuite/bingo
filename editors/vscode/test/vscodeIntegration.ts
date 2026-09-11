import assert from "node:assert/strict";
import { once } from "node:events";
import { readFile } from "node:fs/promises";
import { createServer, type Server, type Socket } from "node:net";

import * as vscode from "vscode";
import { WebSocketServer, type WebSocket } from "ws";

import type { BingoExtensionAPI } from "../src/extension.js";
import type { SessionViewModel } from "../src/model.js";
import { sessionDAPEventName } from "../src/sessionEvent.js";

const extensionID = "bingosuite.bingo";
const managedSessionID = "integration-session";
const editorTitle = "Bingo Concurrency";

export async function run(): Promise<void> {
  assert.equal(
    vscode.version,
    process.env.VSCODE_TEST_VERSION,
    "Electron must run the exact editor version requested by the runner",
  );
  console.log(`Verified requested Electron editor version: ${vscode.version}`);
  const fixture = await loadFixture();
  const configuration = vscode.workspace.getConfiguration();
  const settings = [
    ["bingo.concurrency.autoReveal", true],
  ] as const;
  const previous = settings.map(([key]) => ({
    key,
    value: configuration.inspect(key)?.globalValue,
  }));
  try {
    for (const [key, value] of settings) {
      await configuration.update(key, value, vscode.ConfigurationTarget.Global);
    }
    const extension =
      vscode.extensions.getExtension<BingoExtensionAPI>(extensionID);
    assert.notEqual(extension, undefined, `${extensionID} is not installed`);
    const api = await extension!.activate();
    assert.equal(api.version, 1);
    assert.equal(typeof api.testUI, "function", "test host must expose its DOM driver");
    assert.equal(api.getConcurrencyViewStatus().resolved, false);

    for (const autoReveal of [true, false]) {
      await configuration.update(
        "bingo.concurrency.autoReveal",
        autoReveal,
        vscode.ConfigurationTarget.Global,
      );
      await runSession(api, fixture, autoReveal);
    }
  } finally {
    for (const { key, value } of previous) {
      await configuration.update(key, value, vscode.ConfigurationTarget.Global);
    }
  }
}

async function runSession(
  api: BingoExtensionAPI,
  fixture: SourceFixture,
  autoReveal: boolean,
): Promise<void> {
  const dap = await FakeDAPServer.start(fixture);
  let telemetry: FakeTelemetryServer | undefined;
  let session: vscode.DebugSession | undefined;
  let stopAttempted = false;
  let openedPanels = 0;
  const debugSessionName = `Bingo concurrency integration (${String(autoReveal)})`;
  const tabsSubscription = vscode.window.tabGroups.onDidChangeTabs((event) => {
    openedPanels += event.opened.filter(isConcurrencyTab).length;
  });
  const startSubscription = vscode.debug.onDidStartDebugSession((candidate) => {
    if (candidate.name === debugSessionName) {
      session = candidate;
    }
  });
  try {
    telemetry = await FakeTelemetryServer.start(fixture);
    const source = await vscode.window.showTextDocument(fixture.uri, {
      viewColumn: vscode.ViewColumn.One,
      preview: false,
    });
    await nativeDebugSidebar(api);

    assert.equal(await vscode.debug.startDebugging(undefined, {
      type: "bingo",
      request: "launch",
      name: debugSessionName,
      program: fixture.uri.fsPath,
      stopOnEntry: true,
      serverMode: "connectOnly",
      managementHost: "127.0.0.1",
      managementPort: telemetry.port,
      dapHost: "127.0.0.1",
      dapPort: dap.port,
    }), true);
    const started = await waitFor(() => session, "debug session start");
    const first = await readyModel(api, 1);
    assert.equal(first.sessionId, managedSessionID);
    assert.equal(first.snapshot?.goroutines.length, 2);
    assert.equal(first.tree.edges.length, 1);
    assert.equal(api.getSidebarViewStatus().visible, false);

    if (autoReveal) {
      await rendered(api);
      assert.equal(
        vscode.window.tabGroups.activeTabGroup.viewColumn,
        source.viewColumn,
        "automatic editor opening must preserve source focus",
      );
    } else {
      await started.customRequest("bingoTestBarrier");
      assert.equal(api.getConcurrencyViewStatus().resolved, false);
      assert.equal(concurrencyTabs().length, 0, "disabled auto-reveal must not open a panel");
      assert.equal(openedPanels, 0);
      await vscode.commands.executeCommand("bingo.concurrency.openEditor");
      await rendered(api);
    }
    assert.equal(openedPanels, 1);
    const panel = onlyConcurrencyTab();
    const panelColumn = panel.group.viewColumn;
    assert.notEqual(panelColumn, source.viewColumn, "concurrency must occupy a separate editor group");
    assert.equal(api.getSidebarViewStatus().visible, false, "native Debug must remain available");
    await assertDisplayedInspection(api, fixture);

    await vscode.commands.executeCommand("bingo.concurrency.openEditor");
    await waitFor(
      () => vscode.window.tabGroups.activeTabGroup.activeTab === panel ? true : undefined,
      "manual concurrency editor focus",
    );
    await nativeDebugSidebar(api);
    assertPanelOwner(api, panel, false);
    for (let iteration = 0; iteration < 2; iteration += 1) {
      await nextStop(started, api, dap);
      await assertNativeStopOwner(api, fixture, panel);
      assert.equal(openedPanels, 1, "later stops must reuse the original panel");
    }

    await vscode.commands.executeCommand("bingo.concurrency.openEditor");
    await vscode.commands.executeCommand("workbench.view.extension.bingo");
    await vscode.commands.executeCommand("bingo.concurrency.focus");
    await waitFor(
      () => {
        const sidebar = api.getSidebarViewStatus();
        return sidebar.ready && sidebar.visible ? true : undefined;
      },
      "manual Activity Bar observer",
    );
    assertPanelOwner(api, panel, true);
    const sidebarRequestsBefore = telemetry.snapshotRequests;
    await vscode.commands.executeCommand("bingo.concurrency.refresh");
    await waitFor(
      () => telemetry!.snapshotRequests > sidebarRequestsBefore ? true : undefined,
      "snapshot refresh with the Activity Bar selected",
    );
    await readyModel(api, dap.stopGeneration);
    await rendered(api);
    assertPanelOwner(api, panel, true);
    await nextStop(started, api, dap);
    await assertNativeStopOwner(api, fixture, panel);
    await vscode.commands.executeCommand("workbench.view.extension.bingo");
    await vscode.commands.executeCommand("bingo.concurrency.focus");
    await waitFor(
      () => api.getSidebarViewStatus().visible ? true : undefined,
      "Activity Bar selection before restart",
    );
    await nextStop(started, api, dap, "restart");
    await assertNativeStopOwner(api, fixture, panel);
    assert.equal(openedPanels, 1, "restart must not create or reveal another panel");

    await nativeDebugSidebar(api);
    await nextStop(started, api, dap, "restart");
    await assertNativeStopOwner(api, fixture, panel);
    const requestsBefore = telemetry.snapshotRequests;
    await vscode.commands.executeCommand("bingo.concurrency.refresh");
    await waitFor(
      () => telemetry!.snapshotRequests > requestsBefore ? true : undefined,
      "explicit snapshot refresh",
    );
    await readyModel(api, dap.stopGeneration);
    await rendered(api);
    await assertNativeStopOwner(api, fixture, panel);

    const beforeCreation = new vscode.Position(fixture.stoppedLine - 1, 0);
    await vscode.window.showTextDocument(fixture.uri, {
      viewColumn: vscode.ViewColumn.One,
      selection: new vscode.Range(beforeCreation, beforeCreation),
    });
    await vscode.commands.executeCommand("bingo.concurrency.openEditor");
    await rendered(api);
    await api.testUI!("openCreationSource");
    await waitFor(
      () => {
        const active = vscode.window.activeTextEditor;
        return active?.document.uri.fsPath === fixture.uri.fsPath &&
          active.selection.start.line === fixture.spawnLine - 1 &&
          active.selection.start.character === 0 &&
          active.selection.isEmpty
          ? active
          : undefined;
      },
      "creation-site source navigation",
    );
    assert.equal(vscode.window.activeTextEditor?.viewColumn, source.viewColumn);
    assert.equal(onlyConcurrencyTab(), panel, "source navigation must not replace the panel");
    assert.equal(onlyConcurrencyTab().group.viewColumn, panelColumn);
    assert.equal(api.getConcurrencyViewStatus().visible, true);
    assert.equal(api.getSidebarViewStatus().visible, false);

    assert.equal(await vscode.window.tabGroups.close(panel), true);
    await waitFor(
      () => !api.getConcurrencyViewStatus().resolved ? true : undefined,
      "concurrency editor close",
    );
    await nextStop(started, api, dap, "bingoTestStop", false);
    await nextStop(started, api, dap, "restart", false);
    await started.customRequest("bingoTestAnnounce");
    await started.customRequest("bingoTestBarrier");
    assert.equal(concurrencyTabs().length, 0, "a closed panel stays closed for this session");
    assert.equal(api.getConcurrencyViewStatus().resolved, false);
    assert.equal(api.getSidebarViewStatus().visible, false);
    assert.equal(openedPanels, 1);

    await vscode.commands.executeCommand("bingo.concurrency.openEditor");
    await rendered(api);
    assert.equal(concurrencyTabs().length, 1);
    assert.equal(openedPanels, 2, "manual reopen creates exactly one replacement document");
    const reopened = await api.testUI!("inspect");
    assert.match(reopened.inspector, /main\.produce/u);
    assert.match(reopened.inspector, /payload/u);
    assertCreationLine(reopened.highlightedLine, fixture);

    // A single stop request must complete the real VS Code terminate/disconnect
    // handshake. A second stop call can hide a broken fake adapter lifecycle.
    stopAttempted = true;
    await vscode.debug.stopDebugging(started);
    await waitFor(
      () => api.getConcurrencyState().sessions.length === 0 ? true : undefined,
      "debug-session teardown",
    );
    await waitFor(
      () => dap.activeConnections === 0 && telemetry!.activeConnections === 0
        ? true
        : undefined,
      "DAP and observer socket teardown",
    );
    assert.equal(dap.terminateRequests, 1);
    assert.equal(dap.disconnectRequests, 1);
    session = undefined;
    await rendered(api);
    await vscode.window.tabGroups.close(onlyConcurrencyTab());
    await waitFor(
      () => !api.getConcurrencyViewStatus().resolved ? true : undefined,
      "final panel disposal",
    );
  } finally {
    startSubscription.dispose();
    tabsSubscription.dispose();
    try {
      if (session !== undefined && !stopAttempted) {
        await vscode.debug.stopDebugging(session);
      }
    } finally {
      try {
        await Promise.all([dap.close(), telemetry?.close()]);
      } finally {
        for (const tab of concurrencyTabs()) {
          await vscode.window.tabGroups.close(tab);
        }
      }
    }
  }
}

async function assertDisplayedInspection(
  api: BingoExtensionAPI,
  fixture: SourceFixture,
): Promise<void> {
  const initial = await api.testUI!("inspect");
  assert.deepEqual(initial.frames, ["main.produce", "main.main"]);
  assert.ok(initial.variables.includes("answer"));
  assert.ok(initial.variables.includes("payload"));
  assert.match(initial.inspector, /42/u);
  assert.match(initial.inspector, /main\.go/u);
  assert.match(initial.creation, /defer close\(output\)/u);
  assert.ok(!initial.creation.includes("output := produce("), "source preview must stay near the go statement");
  assertCreationLine(initial.highlightedLine, fixture);

  let revision = api.getConcurrencyState().revision;
  await api.testUI!("expandVariable", 1000);
  await changedAndRendered(api, revision, (model) =>
    model.inspection.variablesByReference["1000"]?.length === 2,
  );
  const expanded = await api.testUI!("inspect");
  assert.ok(expanded.variables.includes("message"));
  assert.ok(expanded.variables.includes("count"));
  assert.match(expanded.inspector, /worker payload/u);

  revision = api.getConcurrencyState().revision;
  await api.testUI!("selectFrame", 2);
  await changedAndRendered(api, revision, (model) =>
    model.inspection.selectedFrameId === 2 && model.inspection.localsStatus === "ready",
  );
  const caller = await api.testUI!("inspect");
  assert.ok(caller.variables.includes("callerLabel"));
  assert.ok(!caller.variables.includes("answer"), "frame changes must replace displayed locals");
  assert.match(caller.inspector, /caller context/u);

  revision = api.getConcurrencyState().revision;
  await api.testUI!("expandVariable", 2000);
  await changedAndRendered(api, revision, (model) =>
    model.inspection.variablesByReference["2000"]?.length === 1,
  );
  const callerExpanded = await api.testUI!("inspect");
  assert.ok(callerExpanded.variables.includes("capacity"));
  assert.match(callerExpanded.inspector, /128/u);

  revision = api.getConcurrencyState().revision;
  await api.testUI!("selectGoroutine", 2);
  await changedAndRendered(api, revision, (model) =>
    model.selectedGoroutine === 2 &&
    model.inspection.stackStatus === "unavailable" &&
    model.spawnSource.status === "ready",
  );
  const foreign = await api.testUI!("inspect");
  assert.match(foreign.inspector, /g2/u);
  assert.match(foreign.inspector, /g1/u);
  assert.match(foreign.inspector, /102/u);
  assert.match(foreign.inspector, /only for the stopped goroutine/u);
  assert.deepEqual(foreign.frames, []);
  assert.deepEqual(foreign.variables, []);
  assertCreationLine(foreign.highlightedLine, fixture);

  revision = api.getConcurrencyState().revision;
  await api.testUI!("selectGoroutine", 1);
  await changedAndRendered(api, revision, (model) =>
    model.selectedGoroutine === 1 &&
    model.inspection.stackStatus === "ready" &&
    model.inspection.localsStatus === "ready",
  );
  assert.ok((await api.testUI!("inspect")).variables.includes("answer"));
}

function assertCreationLine(highlighted: string, fixture: SourceFixture): void {
  assert.equal(
    highlighted.trim().replace(/\s+/gu, " "),
    `${String(fixture.spawnLine)} ${fixture.spawnText}`,
    "the actual go statement and its recorded line number must be highlighted",
  );
}

async function nextStop(
  session: vscode.DebugSession,
  api: BingoExtensionAPI,
  dap: FakeDAPServer,
  command = "bingoTestStop",
  panelOpen = true,
): Promise<void> {
  const generation = dap.stopGeneration + 1;
  await session.customRequest(command);
  await readyModel(api, generation);
  await session.customRequest("bingoTestBarrier");
  if (panelOpen) {
    await rendered(api);
    assert.match(
      (await api.testUI!("inspect")).inspector,
      new RegExp(`stop-${String(generation)}`, "u"),
      "the rendered locals must belong to the new stop",
    );
  }
}

async function nativeDebugSidebar(api: BingoExtensionAPI): Promise<void> {
  await vscode.commands.executeCommand("workbench.view.debug");
  await waitFor(
    () => !api.getSidebarViewStatus().visible ? true : undefined,
    "native Run and Debug sidebar",
  );
}

function assertPanelOwner(
  api: BingoExtensionAPI,
  panel: vscode.Tab,
  sidebarVisible: boolean,
): void {
  assert.equal(onlyConcurrencyTab(), panel, "session updates must reuse the same editor");
  assert.equal(api.getConcurrencyViewStatus().visible, true);
  assert.equal(
    api.getSidebarViewStatus().visible,
    sidebarVisible,
    "session updates must not replace the user's sidebar selection",
  );
  assert.equal(
    vscode.window.tabGroups.activeTabGroup.activeTab,
    panel,
    "session updates must not steal the user's active editor group",
  );
}

async function assertNativeStopOwner(
  api: BingoExtensionAPI,
  fixture: SourceFixture,
  panel: vscode.Tab,
): Promise<void> {
  await waitFor(() => {
    const active = vscode.window.activeTextEditor;
    return active?.document.uri.fsPath === fixture.uri.fsPath &&
      active.selection.start.line === fixture.stoppedLine - 1 &&
      vscode.window.tabGroups.activeTabGroup.viewColumn === vscode.ViewColumn.One &&
      !api.getSidebarViewStatus().visible
      ? true
      : undefined;
  }, "native debugger source and Run and Debug focus");
  assert.equal(onlyConcurrencyTab(), panel, "native stops must not replace the Bingo editor");
  assert.equal(api.getConcurrencyViewStatus().visible, true, "Bingo must remain beside native source");
  assert.equal(
    api.getSidebarViewStatus().visible,
    false,
    "Bingo must not take focus back after native Debug handles a later stop",
  );
}

function isConcurrencyTab(tab: vscode.Tab): boolean {
  return tab.input instanceof vscode.TabInputWebview && tab.label === editorTitle;
}

function concurrencyTabs(): vscode.Tab[] {
  return vscode.window.tabGroups.all.flatMap((group) => group.tabs.filter(isConcurrencyTab));
}

function onlyConcurrencyTab(): vscode.Tab {
  const tabs = concurrencyTabs();
  assert.equal(tabs.length, 1, "exactly one reusable concurrency editor must exist");
  return tabs[0]!;
}

async function readyModel(api: BingoExtensionAPI, generation: number): Promise<SessionViewModel> {
  return waitFor(() => {
    const model = api.getConcurrencyState().sessions[0];
    return model?.snapshot !== undefined &&
      model.inspection.stackStatus === "ready" &&
      model.inspection.localsStatus === "ready" &&
      model.spawnSource.status === "ready" &&
      model.inspection.variables.some((variable) =>
        variable.name === "generation" && variable.value === `stop-${String(generation)}`,
      )
      ? model
      : undefined;
  }, `stop ${String(generation)} stack, locals and source`);
}

async function changedAndRendered(
  api: BingoExtensionAPI,
  revision: number,
  predicate: (model: SessionViewModel) => boolean,
): Promise<void> {
  await waitFor(() => {
    const state = api.getConcurrencyState();
    const model = state.sessions[0];
    return state.revision > revision && model !== undefined && predicate(model)
      ? true
      : undefined;
  }, "DOM action model update");
  await rendered(api);
}

async function rendered(api: BingoExtensionAPI): Promise<void> {
  await waitFor(() => {
    const status = api.getConcurrencyViewStatus();
    return status.ready && status.visible &&
      api.getLastRenderedRevision() === api.getConcurrencyState().revision
      ? true
      : undefined;
  }, "editor rendered acknowledgement");
}

class FakeDAPServer {
  readonly #server: Server;
  readonly #sockets = new Set<Socket>();
  #sequence = 1;
  public stopGeneration = 0;
  public terminateRequests = 0;
  public disconnectRequests = 0;

  private constructor(server: Server, private readonly fixture: SourceFixture) {
    this.#server = server;
    server.on("connection", (socket) => {
      this.#sockets.add(socket);
      socket.on("error", () => {
        socket.destroy();
      });
      socket.once("close", () => {
        this.#sockets.delete(socket);
      });
      let buffer = Buffer.alloc(0);
      let launchRequest = 0;
      socket.on("data", (chunk: Buffer) => {
        buffer = Buffer.concat([buffer, chunk]);
        for (;;) {
          const headerEnd = buffer.indexOf("\r\n\r\n");
          if (headerEnd < 0) {
            return;
          }
          const header = buffer.subarray(0, headerEnd).toString("ascii");
          const match = /(?:^|\r\n)Content-Length: (\d+)(?:\r\n|$)/iu.exec(header);
          assert.notEqual(match?.[1], undefined);
          const length = Number(match![1]);
          const bodyStart = headerEnd + 4;
          if (buffer.length < bodyStart + length) {
            return;
          }
          const request = JSON.parse(
            buffer.subarray(bodyStart, bodyStart + length).toString("utf8"),
          ) as DAPRequest;
          buffer = buffer.subarray(bodyStart + length);
          if (request.command === "launch") {
            launchRequest = request.seq;
          }
          this.#request(socket, request, launchRequest);
        }
      });
    });
  }

  public get port(): number {
    return listeningPort(this.#server.address());
  }

  public get activeConnections(): number {
    return this.#sockets.size;
  }

  public static async start(fixture: SourceFixture): Promise<FakeDAPServer> {
    const server = createServer();
    const fake = new FakeDAPServer(server, fixture);
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    return fake;
  }

  public async close(): Promise<void> {
    for (const socket of this.#sockets) {
      socket.destroy();
    }
    if (this.#server.listening) {
      const closed = once(this.#server, "close");
      this.#server.close();
      await closed;
    }
  }

  #request(socket: Socket, request: DAPRequest, launchRequest: number): void {
    switch (request.command) {
      case "initialize":
        this.#respond(socket, request, {
          supportsConfigurationDoneRequest: true,
          supportsTerminateRequest: true,
          supportsRestartRequest: true,
        });
        break;
      case "launch":
        this.#announce(socket);
        this.#event(socket, "initialized", {});
        break;
      case "setBreakpoints":
      case "setFunctionBreakpoints":
      case "setInstructionBreakpoints":
        this.#respond(socket, request, { breakpoints: [] });
        break;
      case "configurationDone":
        this.#respond(socket, request, {});
        this.#respond(socket, { seq: launchRequest, command: "launch" }, {});
        this.#stop(socket, "entry");
        break;
      case "bingoTestStop":
      case "restart":
        this.#respond(socket, request, {});
        this.#event(socket, "continued", { threadId: 1, allThreadsContinued: true });
        if (request.command === "restart") {
          this.#announce(socket);
        }
        this.#stop(socket, "breakpoint");
        break;
      case "bingoTestAnnounce":
        this.#announce(socket);
        this.#respond(socket, request, {});
        break;
      case "threads":
        this.#respond(socket, request, {
          threads: [{ id: 1, name: "producer" }, { id: 2, name: "worker" }],
        });
        break;
      case "stackTrace":
        this.#respond(socket, request, {
          stackFrames: [
            {
              id: 1,
              name: "main.produce",
              source: { name: "main.go", path: this.fixture.uri.fsPath },
              line: this.fixture.stoppedLine,
              column: 1,
            },
            {
              id: 2,
              name: "main.main",
              source: { name: "main.go", path: this.fixture.uri.fsPath },
              line: this.fixture.callerLine,
              column: 1,
            },
          ],
          totalFrames: 2,
        });
        break;
      case "scopes":
        this.#respond(socket, request, {
          scopes: [{
            name: "Locals",
            variablesReference: request.arguments?.frameId === 2 ? 200 : 100,
            expensive: false,
          }],
        });
        break;
      case "variables":
        this.#respond(socket, request, {
          variables: this.#variables(request.arguments?.variablesReference),
        });
        break;
      case "loadedSources":
        this.#respond(socket, request, { sources: [] });
        break;
      case "terminate":
        this.terminateRequests += 1;
        this.#respond(socket, request, {});
        this.#event(socket, "terminated", {});
        break;
      case "disconnect":
        this.disconnectRequests += 1;
        this.#respond(socket, request, {});
        socket.end();
        break;
      default:
        this.#respond(socket, request, {});
        break;
    }
  }

  #variables(reference: unknown): readonly Record<string, unknown>[] {
    const variable = (name: string, value: string, type: string, variablesReference = 0) =>
      ({ name, value, type, variablesReference });
    switch (reference) {
      case 100:
        return [
          variable("answer", "42", "int"),
          variable("generation", `stop-${String(this.stopGeneration)}`, "string"),
          variable("payload", "WorkerPayload", "main.Payload", 1000),
        ];
      case 1000:
        return [
          variable("message", '"worker payload"', "string"),
          variable("count", "7", "int"),
        ];
      case 200:
        return [
          variable("callerLabel", '"caller context"', "string"),
          variable("channel", "ChannelConfig", "main.Config", 2000),
        ];
      case 2000:
        return [variable("capacity", "128", "int")];
      default:
        return [];
    }
  }

  #stop(socket: Socket, reason: string): void {
    this.stopGeneration += 1;
    this.#event(socket, "stopped", {
      reason,
      threadId: 1,
      allThreadsStopped: true,
    });
  }

  #announce(socket: Socket): void {
    this.#event(socket, sessionDAPEventName, { version: 1, sessionId: managedSessionID });
  }

  #respond(socket: Socket, request: DAPRequest, body: Record<string, unknown>): void {
    this.#write(socket, {
      seq: this.#sequence++,
      type: "response",
      request_seq: request.seq,
      success: true,
      command: request.command,
      body,
    });
  }

  #event(socket: Socket, event: string, body: Record<string, unknown>): void {
    this.#write(socket, { seq: this.#sequence++, type: "event", event, body });
  }

  #write(socket: Socket, message: Record<string, unknown>): void {
    const body = Buffer.from(JSON.stringify(message));
    socket.write(Buffer.concat([
      Buffer.from(`Content-Length: ${String(body.length)}\r\n\r\n`),
      body,
    ]));
  }
}

class FakeTelemetryServer {
  readonly #server: WebSocketServer;
  readonly #sockets = new Set<WebSocket>();
  #sequence = 0;
  public snapshotRequests = 0;

  private constructor(server: WebSocketServer, fixture: SourceFixture) {
    this.#server = server;
    server.on("connection", (socket, request) => {
      assert.equal(request.url, `/ws?session=${encodeURIComponent(managedSessionID)}`);
      this.#sockets.add(socket);
      socket.on("error", () => {
        socket.terminate();
      });
      socket.once("close", () => {
        this.#sockets.delete(socket);
      });
      this.#send(socket, "SessionState", {
        sessionID: managedSessionID,
        state: "suspended",
        clients: 2,
      });
      socket.on("message", (raw) => {
        assert.ok(Buffer.isBuffer(raw), "telemetry command must be a text buffer");
        assert.deepEqual(JSON.parse(raw.toString("utf8")), {
          v: "1.4",
          kind: "GoroutineSnapshot",
          payload: {},
        });
        this.snapshotRequests += 1;
        this.#send(socket, "GoroutineSnapshot", snapshotPayload(fixture));
      });
    });
  }

  public get port(): number {
    return listeningPort(this.#server.address());
  }

  public get activeConnections(): number {
    return this.#sockets.size;
  }

  public static async start(fixture: SourceFixture): Promise<FakeTelemetryServer> {
    const server = new WebSocketServer({
      host: "127.0.0.1",
      port: 0,
      perMessageDeflate: false,
    });
    const fake = new FakeTelemetryServer(server, fixture);
    await once(server, "listening");
    return fake;
  }

  public async close(): Promise<void> {
    for (const socket of this.#sockets) {
      socket.terminate();
    }
    await new Promise<void>((resolveClose, reject) => {
      this.#server.close((error) => {
        if (error === undefined) {
          resolveClose();
        } else {
          reject(error);
        }
      });
    });
  }

  #send(socket: WebSocket, kind: string, payload: Record<string, unknown>): void {
    this.#sequence += 1;
    socket.send(JSON.stringify({ v: "1.4", kind, seq: this.#sequence, payload }));
  }
}

interface DAPRequest {
  readonly seq: number;
  readonly command: string;
  readonly arguments?: Readonly<Record<string, unknown>>;
}

interface SourceFixture {
  readonly uri: vscode.Uri;
  readonly spawnLine: number;
  readonly spawnText: string;
  readonly stoppedLine: number;
  readonly callerLine: number;
}

async function loadFixture(): Promise<SourceFixture> {
  const root = vscode.workspace.workspaceFolders?.[0]?.uri;
  assert.notEqual(root, undefined, "integration tests need the actual repository workspace");
  const uri = vscode.Uri.joinPath(root!, "examples", "level2-channel", "main.go");
  const contents = await readFile(uri.fsPath, "utf8");
  const lines = contents.split("\n");
  const spawnLine = lines.findIndex((line) => line.trim() === "go func() {") + 1;
  const stoppedLine = lines.findIndex((line) => line.trim() === "output <- value") + 1;
  const callerLine = lines.findIndex((line) => line.includes("output := produce(")) + 1;
  assert.ok(spawnLine > 0 && stoppedLine > 0 && callerLine > 0);
  return { uri, spawnLine, stoppedLine, callerLine, spawnText: lines[spawnLine - 1]!.trim() };
}

function listeningPort(address: string | { readonly port: number } | null): number {
  assert.notEqual(address, null);
  assert.equal(typeof address, "object");
  return (address as { readonly port: number }).port;
}

function snapshotPayload(fixture: SourceFixture): Record<string, unknown> {
  const location = {
    file: fixture.uri.fsPath,
    line: fixture.stoppedLine,
    function: "main.produce",
  };
  const createdLoc = { ...location, line: fixture.spawnLine };
  return {
    goroutines: [
      {
        id: 1,
        status: "running",
        currentLoc: location,
        startLoc: location,
        createdLoc,
        current: true,
        threadId: 101,
      },
      {
        id: 2,
        parentId: 1,
        status: "waiting",
        waitReason: "chan send",
        currentLoc: location,
        startLoc: location,
        createdLoc,
        threadId: 102,
      },
    ],
    threads: [
      { id: 101, mid: 1, goid: 1, currentLoc: location, current: true },
      { id: 102, mid: 2, goid: 2, currentLoc: location },
    ],
    current: 1,
    created: [2],
  };
}

async function waitFor<T>(
  probe: () => T | undefined,
  label: string,
): Promise<T> {
  const deadline = Date.now() + 10_000;
  for (;;) {
    const result = probe();
    if (result !== undefined) {
      return result;
    }
    if (Date.now() >= deadline) {
      throw new Error(`timed out waiting for ${label}`);
    }
    await new Promise((resolveDelay) => setTimeout(resolveDelay, 25));
  }
}
