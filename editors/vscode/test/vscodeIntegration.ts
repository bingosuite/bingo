import assert from "node:assert/strict";
import { once } from "node:events";
import { createServer, type Server, type Socket } from "node:net";

import * as vscode from "vscode";
import {
  WebSocketServer,
  type WebSocket,
} from "ws";

import type { BingoExtensionAPI } from "../src/extension.js";
import { sessionDAPEventName } from "../src/sessionEvent.js";

const extensionID = "bingosuite.bingo";
const debugSessionName = "Bingo concurrency integration";
const managedSessionID = "integration-session";

export async function run(): Promise<void> {
  const dap = await FakeDAPServer.start();
  const telemetry = await FakeTelemetryServer.start();
  let session: vscode.DebugSession | undefined;
  try {
    const extension =
      vscode.extensions.getExtension<BingoExtensionAPI>(extensionID);
    assert.notEqual(extension, undefined, `${extensionID} is not installed`);
    const api = await extension!.activate();
    assert.equal(api.version, 1);

    await vscode.commands.executeCommand("workbench.view.extension.bingo");
    await vscode.commands.executeCommand("bingo.concurrency.focus");
    await waitFor(
      () =>
        api.getConcurrencyViewStatus().ready ? true : undefined,
      10_000,
      "Bingo Concurrency webview activation",
    );
    let observedStart: vscode.DebugSession | undefined;
    const startSubscription = vscode.debug.onDidStartDebugSession(
      (candidate) => {
        if (candidate.name === debugSessionName) {
          observedStart = candidate;
        }
      },
    );
    const started = await vscode.debug.startDebugging(undefined, {
      type: "bingo",
      request: "launch",
      name: debugSessionName,
      program: "/integration/fake-target",
      stopOnEntry: true,
      serverMode: "connectOnly",
      managementHost: "127.0.0.1",
      managementPort: telemetry.port,
      dapHost: "127.0.0.1",
      dapPort: dap.port,
    });
    assert.equal(started, true);
    session = await waitFor(
      () => observedStart,
      10_000,
      "debug session start",
    );
    startSubscription.dispose();

    const model = await waitFor(
      () => {
        const current = api.getConcurrencyState();
        const observed = current.sessions[0];
        return observed?.sessionId === managedSessionID &&
          observed.snapshot?.goroutines.length === 2
          ? current
          : undefined;
      },
      10_000,
      "custom session event and telemetry snapshot",
    );
    assert.equal(model.sessions[0]?.tree.edges.length, 1);
    await vscode.commands.executeCommand("bingo.concurrency.focus");
    await waitFor(
      () =>
        api.getConcurrencyViewStatus().ready ? true : undefined,
      5000,
      "focused concurrency webview",
    );
    await waitFor(
      () =>
        api.getLastRenderedRevision() ===
        api.getConcurrencyState().revision
          ? true
          : undefined,
      10_000,
      "webview rendered acknowledgement",
    );

    const requestsBefore = telemetry.snapshotRequests;
    await vscode.commands.executeCommand("bingo.concurrency.refresh");
    await waitFor(
      () =>
        telemetry.snapshotRequests > requestsBefore ? true : undefined,
      5000,
      "explicit snapshot refresh",
    );

    await vscode.debug.stopDebugging(session);
    await waitFor(
      () =>
        api.getConcurrencyState().sessions.length === 0 ? true : undefined,
      10_000,
      "debug-session teardown",
    );
    session = undefined;
  } finally {
    if (session !== undefined) {
      await vscode.debug.stopDebugging(session);
    }
    await Promise.all([dap.close(), telemetry.close()]);
  }
}

class FakeDAPServer {
  readonly #server: Server;
  readonly #sockets = new Set<Socket>();
  #sequence = 1;
  #launchRequest = 0;

  private constructor(server: Server) {
    this.#server = server;
    server.on("connection", (socket) => {
      this.#sockets.add(socket);
      socket.once("close", () => {
        this.#sockets.delete(socket);
      });
      let buffer = Buffer.alloc(0);
      socket.on("data", (chunk: Buffer) => {
        buffer = Buffer.concat([buffer, chunk]);
        for (;;) {
          const headerEnd = buffer.indexOf("\r\n\r\n");
          if (headerEnd < 0) {
            return;
          }
          const header = buffer.subarray(0, headerEnd).toString("ascii");
          const match = /(?:^|\r\n)Content-Length: (\d+)(?:\r\n|$)/iu.exec(
            header,
          );
          assert.notEqual(match?.[1], undefined);
          const length = Number(match![1]);
          const bodyStart = headerEnd + 4;
          if (buffer.length < bodyStart + length) {
            return;
          }
          const request = JSON.parse(
            buffer
              .subarray(bodyStart, bodyStart + length)
              .toString("utf8"),
          ) as DAPRequest;
          buffer = buffer.subarray(bodyStart + length);
          this.#request(socket, request);
        }
      });
    });
  }

  public get port(): number {
    return listeningPort(this.#server.address());
  }

  public static async start(): Promise<FakeDAPServer> {
    const server = createServer();
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    return new FakeDAPServer(server);
  }

  public async close(): Promise<void> {
    for (const socket of this.#sockets) {
      socket.destroy();
    }
    if (!this.#server.listening) {
      return;
    }
    this.#server.close();
    await once(this.#server, "close");
  }

  #request(socket: Socket, request: DAPRequest): void {
    const command = request.command;
    switch (command) {
      case "initialize":
        this.#respond(socket, request, {
          supportsConfigurationDoneRequest: true,
          supportsTerminateRequest: true,
        });
        break;
      case "launch":
        this.#launchRequest = request.seq;
        this.#event(socket, sessionDAPEventName, {
          version: 1,
          sessionId: managedSessionID,
        });
        this.#event(socket, "initialized", {});
        break;
      case "setBreakpoints":
        this.#respond(socket, request, { breakpoints: [] });
        break;
      case "setFunctionBreakpoints":
      case "setInstructionBreakpoints":
        this.#respond(socket, request, { breakpoints: [] });
        break;
      case "setExceptionBreakpoints":
        this.#respond(socket, request, {});
        break;
      case "configurationDone":
        this.#respond(socket, request, {});
        this.#write(socket, {
          seq: this.#sequence++,
          type: "response",
          request_seq: this.#launchRequest,
          success: true,
          command: "launch",
        });
        this.#event(socket, "stopped", {
          reason: "entry",
          threadId: 1,
          allThreadsStopped: true,
        });
        break;
      case "threads":
        this.#respond(socket, request, {
          threads: [{ id: 1, name: "main" }],
        });
        break;
      case "stackTrace":
        this.#respond(socket, request, {
          stackFrames: [],
          totalFrames: 0,
        });
        break;
      case "scopes":
        this.#respond(socket, request, { scopes: [] });
        break;
      case "variables":
        this.#respond(socket, request, { variables: [] });
        break;
      case "loadedSources":
        this.#respond(socket, request, { sources: [] });
        break;
      case "terminate":
        this.#respond(socket, request, {});
        this.#event(socket, "terminated", {});
        setTimeout(() => {
          socket.end();
        }, 25);
        break;
      case "disconnect":
        this.#respond(socket, request, {});
        socket.end();
        break;
      default:
        this.#respond(socket, request, {});
        break;
    }
  }

  #respond(
    socket: Socket,
    request: DAPRequest,
    body: Record<string, unknown>,
  ): void {
    this.#write(socket, {
      seq: this.#sequence++,
      type: "response",
      request_seq: request.seq,
      success: true,
      command: request.command,
      body,
    });
  }

  #event(
    socket: Socket,
    event: string,
    body: Record<string, unknown>,
  ): void {
    this.#write(socket, {
      seq: this.#sequence++,
      type: "event",
      event,
      body,
    });
  }

  #write(socket: Socket, message: Record<string, unknown>): void {
    const body = Buffer.from(JSON.stringify(message));
    socket.write(
      Buffer.concat([
        Buffer.from(`Content-Length: ${String(body.length)}\r\n\r\n`),
        body,
      ]),
    );
  }
}

class FakeTelemetryServer {
  readonly #server: WebSocketServer;
  readonly #sockets = new Set<WebSocket>();
  #sequence = 0;
  public snapshotRequests = 0;

  private constructor(server: WebSocketServer) {
    this.#server = server;
    server.on("connection", (socket, request) => {
      assert.equal(
        request.url,
        `/ws?session=${encodeURIComponent(managedSessionID)}`,
      );
      this.#sockets.add(socket);
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
        const command = JSON.parse(raw.toString("utf8")) as {
          readonly v?: unknown;
          readonly kind?: unknown;
          readonly payload?: unknown;
        };
        assert.deepEqual(command, {
          v: "1.2",
          kind: "GoroutineSnapshot",
          payload: {},
        });
        this.snapshotRequests += 1;
        this.#send(socket, "GoroutineSnapshot", snapshotPayload());
      });
    });
  }

  public get port(): number {
    return listeningPort(this.#server.address());
  }

  public static async start(): Promise<FakeTelemetryServer> {
    const server = new WebSocketServer({
      host: "127.0.0.1",
      port: 0,
      perMessageDeflate: false,
    });
    await once(server, "listening");
    return new FakeTelemetryServer(server);
  }

  public async close(): Promise<void> {
    for (const socket of this.#sockets) {
      socket.close();
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

  #send(
    socket: WebSocket,
    kind: string,
    payload: Record<string, unknown>,
  ): void {
    this.#sequence += 1;
    socket.send(
      JSON.stringify({
        v: "1.2",
        kind,
        seq: this.#sequence,
        payload,
      }),
    );
  }
}

interface DAPRequest {
  readonly seq: number;
  readonly command: string;
}

function listeningPort(
  address: string | { readonly port: number } | null,
): number {
  assert.notEqual(address, null);
  assert.equal(typeof address, "object");
  return (address as { readonly port: number }).port;
}

function snapshotPayload(): Record<string, unknown> {
  const location = {
    file: "/workspace/main.go",
    line: 42,
    function: "main.worker",
  };
  return {
    goroutines: [
      {
        id: 1,
        status: "waiting",
        waitReason: "sync.WaitGroup.Wait",
        currentLoc: location,
        startLoc: location,
        createdLoc: location,
        current: true,
        threadId: 101,
      },
      {
        id: 2,
        parentId: 1,
        status: "running",
        currentLoc: location,
        startLoc: location,
        createdLoc: location,
        threadId: 102,
      },
    ],
    threads: [
      {
        id: 101,
        mid: 1,
        goid: 1,
        currentLoc: location,
        current: true,
      },
      {
        id: 102,
        mid: 2,
        goid: 2,
        currentLoc: location,
      },
    ],
    current: 1,
    created: [2],
  };
}

async function waitFor<T>(
  probe: () => T | undefined,
  timeoutMs: number,
  label: string,
): Promise<T> {
  const deadline = Date.now() + timeoutMs;
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
