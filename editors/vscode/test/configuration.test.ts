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
    assert.equal(validated.mode, "exec");
  });

  for (const mode of ["debug", "exec"] as const) {
    it(`accepts ${mode} launch with cwd and paths containing spaces without rewriting them`, () => {
      const config = {
        request: "launch",
        mode,
        program: "./my package",
        cwd: "/workspace with spaces/project",
        stopOnEntry: false,
      };
      const before = { ...config };
      const validated = validateBingoConfiguration(config);
      assert.equal(validated.request, "launch");
      assert.equal(validated.mode, mode);
      assert.deepEqual(config, before);
    });
  }

  it("accepts an empty cwd as the server's legacy default", () => {
    const validated = validateBingoConfiguration({
      request: "launch", program: "./target", cwd: "",
    });
    assert.equal(validated.request, "launch");
    assert.equal(validated.mode, "exec");
  });

  it("keeps remote source paths server-local instead of checking this machine's filesystem", () => {
    const validated = validateBingoConfiguration({
      request: "launch", mode: "debug", program: "/remote/server/source",
      cwd: "/remote/server", serverMode: "connectOnly", dapHost: "debug.internal",
    });
    assert.equal(validated.server.mode, "connectOnly");
  });

  for (const [label, fields] of [
    ["unknown launch mode", { mode: "test" }],
    ["empty launch mode", { mode: "" }],
    ["null launch mode", { mode: null }],
    ["numeric launch mode", { mode: 1 }],
    ["object cwd", { cwd: {} }],
    ["null cwd", { cwd: null }],
    ["NUL cwd", { cwd: "/tmp/\0source" }],
    ["NUL program", { program: "/tmp/\0target" }],
    ["invalid stopOnEntry", { stopOnEntry: "true" }],
  ] as const) {
    it(`rejects ${label} before startup`, () => {
      assert.throws(() => validateBingoConfiguration({
        request: "launch", program: "/tmp/target", ...fields,
      }), ConfigurationError);
    });
  }

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
