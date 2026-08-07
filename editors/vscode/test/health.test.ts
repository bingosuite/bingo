import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import type { RequestListener, Server } from "node:http";
import type { AddressInfo } from "node:net";
import { resolve } from "node:path";
import { describe, it } from "node:test";

import {
  bingoServiceIdentity,
  managementApiVersion,
  probeBingoHealth,
  validateHealthResponse,
  wireProtocolVersion,
} from "../src/health.js";

const expectedDAP = { host: "127.0.0.1", port: 4711 };

function health(overrides: Record<string, unknown> = {}): string {
  return JSON.stringify({
    service: "bingo",
    managementApiVersion: 1,
    wireProtocolVersion: "1.2",
    instanceId: "instance-1",
    dap: {
      enabled: true,
      address: "127.0.0.1:4711",
    },
    managedIdleShutdown: {
      enabled: true,
      timeoutMs: 30000,
    },
    sessionCount: 0,
    ...overrides,
  });
}

describe("health compatibility", () => {
  it("accepts the exact bingo contract", () => {
    const result = validateHealthResponse(200, health(), expectedDAP);
    assert.equal(result.kind, "compatible");
  });

  it("accepts a wildcard advertised host but keeps port compatibility", () => {
    for (const address of ["0.0.0.0:4711", "[::]:4711", ":4711"]) {
      const result = validateHealthResponse(
        200,
        health({ dap: { enabled: true, address } }),
        expectedDAP,
      );
      assert.equal(result.kind, "compatible");
    }
  });

  it("rejects a response that closes before its declared body completes", async () => {
    await withHTTPServer((_request, response) => {
      response.writeHead(200, {
        "Content-Type": "application/json",
        "Content-Length": "1024",
      });
      response.write('{"service":"bingo"');
      setImmediate(() => {
        response.destroy();
      });
    }, async (port) => {
      const started = Date.now();
      const result = await probeWithSafetyAbort(port, 2000);
      assert.equal(result.kind, "transportError");
      if (result.kind === "transportError") {
        assert.match(result.error.message, /aborted|reset|closed/i);
      }
      assert.ok(
        Date.now() - started < 750,
        "partial response must fail without waiting for the request deadline",
      );
    });
  });

  it("enforces a wall-clock deadline against a slow-drip response", async () => {
    await withHTTPServer((_request, response) => {
      response.writeHead(200, { "Content-Type": "application/json" });
      response.write("{");
      const interval = setInterval(() => {
        response.write(" ");
      }, 20);
      response.once("close", () => {
        clearInterval(interval);
      });
    }, async (port) => {
      const started = Date.now();
      const result = await probeWithSafetyAbort(port, 120);
      const elapsed = Date.now() - started;
      assert.equal(result.kind, "transportError");
      if (result.kind === "transportError") {
        assert.match(result.error.message, /timed out after 120ms/);
      }
      assert.ok(elapsed >= 80, `wall timeout fired too early after ${String(elapsed)}ms`);
      assert.ok(elapsed < 750, `slow-drip probe took ${String(elapsed)}ms`);
    });
  });

  for (const [label, body] of [
    ["HTTP occupant", { status: 404, body: "not found" }],
    ["non-JSON occupant", { status: 200, body: "<html></html>" }],
    ["wrong identity", { status: 200, body: health({ service: "other" }) }],
    [
      "wrong management API",
      { status: 200, body: health({ managementApiVersion: 2 }) },
    ],
    [
      "wrong wire protocol",
      { status: 200, body: health({ wireProtocolVersion: "9" }) },
    ],
    [
      "disabled DAP",
      {
        status: 200,
        body: health({ dap: { enabled: false, address: "" } }),
      },
    ],
    [
      "wrong DAP port",
      {
        status: 200,
        body: health({
          dap: { enabled: true, address: "127.0.0.1:9999" },
        }),
      },
    ],
    [
      "wrong concrete DAP host",
      {
        status: 200,
        body: health({
          dap: { enabled: true, address: "192.0.2.1:4711" },
        }),
      },
    ],
  ] as const) {
    it(`rejects ${label}`, () => {
      const result = validateHealthResponse(body.status, body.body, expectedDAP);
      assert.equal(result.kind, "incompatible");
    });
  }

  it("drift-checks extension constants against Go sources", () => {
    const root = resolve(process.cwd(), "../..");
    const handler = readFileSync(
      resolve(root, "internal/server/handler.go"),
      "utf8",
    );
    const protocol = readFileSync(
      resolve(root, "pkg/protocol/protocol.go"),
      "utf8",
    );

    assert.match(
      handler,
      new RegExp(`serviceIdentity\\s*=\\s*"${bingoServiceIdentity}"`),
    );
    assert.match(
      handler,
      new RegExp(
        `ManagementAPIVersion\\s*=\\s*${String(managementApiVersion)}`,
      ),
    );
    assert.match(
      protocol,
      new RegExp(`const Version\\s*=\\s*"${wireProtocolVersion}"`),
    );
  });
});

async function probeWithSafetyAbort(
  port: number,
  timeoutMs: number,
): ReturnType<typeof probeBingoHealth> {
  const controller = new AbortController();
  const safety = setTimeout(() => {
    controller.abort();
  }, 1000);
  safety.unref();
  try {
    return await probeBingoHealth(
      { host: "127.0.0.1", port },
      expectedDAP,
      timeoutMs,
      controller.signal,
    );
  } finally {
    clearTimeout(safety);
  }
}

async function withHTTPServer(
  handler: RequestListener,
  run: (port: number) => Promise<void>,
): Promise<void> {
  const server = createServer(handler);
  await listen(server);
  try {
    const address = server.address();
    assert.notEqual(address, null);
    assert.equal(typeof address, "object");
    await run((address as AddressInfo).port);
  } finally {
    server.closeAllConnections();
    await close(server);
  }
}

function listen(server: Server): Promise<void> {
  return new Promise((resolveListen, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      server.removeListener("error", reject);
      resolveListen();
    });
  });
}

function close(server: Server): Promise<void> {
  return new Promise((resolveClose, reject) => {
    server.close((error) => {
      if (error === undefined) {
        resolveClose();
      } else {
        reject(error);
      }
    });
  });
}
