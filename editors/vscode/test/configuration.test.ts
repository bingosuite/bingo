import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  ConfigurationError,
  defaultDapHost,
  defaultDapPort,
  defaultManagedIdleTimeoutMs,
  defaultManagementHost,
  defaultManagementPort,
  defaultServerMode,
  defaultServerReadyTimeoutMs,
  resolveEndpoint,
  resolveServerConfiguration,
  validateBingoConfiguration,
} from "../src/configuration.js";

describe("bingo endpoint", () => {
  it("uses the local bingo defaults", () => {
    assert.equal(defaultDapHost, "127.0.0.1");
    assert.deepEqual(resolveEndpoint({}), {
      host: defaultDapHost,
      port: defaultDapPort,
    });
  });

  it("accepts an explicit host and port", () => {
    assert.deepEqual(
      resolveEndpoint({ dapHost: "debug.internal", dapPort: 14711 }),
      { host: "debug.internal", port: 14711 },
    );
  });

  for (const [label, config] of [
    ["empty host", { dapHost: "" }],
    ["string port", { dapPort: "4711" }],
    ["zero port", { dapPort: 0 }],
    ["fractional port", { dapPort: 47.11 }],
    ["out-of-range port", { dapPort: 65536 }],
  ] as const) {
    it(`rejects ${label}`, () => {
      assert.throws(() => resolveEndpoint(config), ConfigurationError);
    });
  }
});

describe("bingo server configuration", () => {
  it("uses managed local defaults", () => {
    assert.deepEqual(resolveServerConfiguration({}), {
      mode: defaultServerMode,
      managementEndpoint: {
        host: defaultManagementHost,
        port: defaultManagementPort,
      },
      dapEndpoint: {
        host: defaultDapHost,
        port: defaultDapPort,
      },
      readyTimeoutMs: defaultServerReadyTimeoutMs,
      idleTimeoutMs: defaultManagedIdleTimeoutMs,
    });
  });

  it("accepts explicit connect-only endpoints and timing", () => {
    assert.deepEqual(
      resolveServerConfiguration({
        serverMode: "connectOnly",
        managementHost: "management.internal",
        managementPort: 16060,
        dapHost: "debug.internal",
        dapPort: 14711,
        serverReadyTimeoutMs: 10000,
        managedIdleTimeoutMs: 60000,
      }),
      {
        mode: "connectOnly",
        managementEndpoint: {
          host: "management.internal",
          port: 16060,
        },
        dapEndpoint: {
          host: "debug.internal",
          port: 14711,
        },
        readyTimeoutMs: 10000,
        idleTimeoutMs: 60000,
      },
    );
  });

  for (const [label, config] of [
    ["unknown mode", { serverMode: "launch" }],
    ["empty management host", { managementHost: "" }],
    ["invalid management port", { managementPort: 0 }],
    ["short readiness timeout", { serverReadyTimeoutMs: 99 }],
    ["fractional readiness timeout", { serverReadyTimeoutMs: 100.5 }],
    ["zero managed idle timeout", { managedIdleTimeoutMs: 0 }],
    ["excessive managed idle timeout", { managedIdleTimeoutMs: 86400001 }],
  ] as const) {
    it(`rejects ${label}`, () => {
      assert.throws(
        () => resolveServerConfiguration(config),
        ConfigurationError,
      );
    });
  }
});

describe("bingo debug configuration", () => {
  it("accepts binary launch arguments", () => {
    const validated = validateBingoConfiguration({
      request: "launch",
      program: "/tmp/target",
      args: ["one"],
      env: ["BINGO_TEST=1"],
      stopOnEntry: true,
    });

    assert.equal(validated.request, "launch");
    assert.equal(validated.server.mode, "auto");
  });

  it("accepts existing-session join", () => {
    const validated = validateBingoConfiguration({
      request: "attach",
      session: "session-123",
    });

    assert.equal(validated.request, "attach");
  });

  it("accepts PID attach with an optional binary path", () => {
    const validated = validateBingoConfiguration({
      request: "attach",
      pid: 1234,
      binaryPath: "/tmp/target",
      stopOnEntry: true,
    });

    assert.equal(validated.request, "attach");
  });

  it("rejects launch without a program", () => {
    assert.throws(
      () => validateBingoConfiguration({ request: "launch" }),
      /program/,
    );
  });

  it("rejects attach without a mode", () => {
    assert.throws(
      () => validateBingoConfiguration({ request: "attach" }),
      /exactly one/,
    );
  });

  it("rejects attach that mixes session join and PID attach", () => {
    assert.throws(
      () =>
        validateBingoConfiguration({
          request: "attach",
          session: "session-123",
          pid: 1234,
        }),
      /exactly one/,
    );
  });

  it("rejects an empty session instead of treating it as PID attach", () => {
    assert.throws(
      () =>
        validateBingoConfiguration({
          request: "attach",
          session: " ",
          pid: 1234,
        }),
      /session/,
    );
  });

  it("rejects non-string launch arrays", () => {
    assert.throws(
      () =>
        validateBingoConfiguration({
          request: "launch",
          program: "/tmp/target",
          args: [1],
        }),
      /args/,
    );
  });
});
