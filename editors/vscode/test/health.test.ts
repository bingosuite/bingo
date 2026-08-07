import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it } from "node:test";

import {
  bingoServiceIdentity,
  managementApiVersion,
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
