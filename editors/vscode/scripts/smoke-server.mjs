import {
  closeSync,
  mkdtempSync,
  openSync,
  rmSync,
} from "node:fs";
import { request } from "node:http";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout } from "node:timers";
import { fileURLToPath, URL } from "node:url";
import { spawn, spawnSync } from "node:child_process";

import { currentTarget } from "./platform.mjs";

const target = currentTarget();
const vsix = fileURLToPath(
  new URL(`../../../dist/bingo-${target}.vsix`, import.meta.url),
);
const extracted = mkdtempSync(join(tmpdir(), "bingo-smoke-"));
const idleTimeoutMs = 2000;

try {
  run("unzip", ["-q", vsix, "-d", extracted]);
  const [managementPort, dapPort] = await unusedPorts(2);
  const binary = join(extracted, "extension", "bin", "bingo");
  const logPath = join(extracted, "server.log");
  const logFD = openSync(logPath, "a", 0o600);
  const child = spawn(
    binary,
    [
      "-addr",
      `127.0.0.1:${String(managementPort)}`,
      "-dap-addr",
      `127.0.0.1:${String(dapPort)}`,
      "-idle-timeout",
      `${String(idleTimeoutMs)}ms`,
    ],
    {
      detached: true,
      shell: false,
      stdio: ["ignore", logFD, logFD],
    },
  );
  closeSync(logFD);
  child.unref();

  const exit = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (code, signal) => {
      resolve({ code, signal });
    });
  });

  const health = await pollHealth(managementPort, dapPort, exit);
  if (
    health.service !== "bingo" ||
    health.managementApiVersion !== 1 ||
    health.wireProtocolVersion !== "1.2" ||
    health.dap?.enabled !== true ||
    health.dap.address !== `127.0.0.1:${String(dapPort)}` ||
    health.managedIdleShutdown?.enabled !== true ||
    health.managedIdleShutdown.timeoutMs !== idleTimeoutMs
  ) {
    throw new Error(`unexpected packaged server health: ${JSON.stringify(health)}`);
  }

  const outcome = await withTimeout(
    exit,
    idleTimeoutMs + 8000,
    "packaged server did not self-exit after its idle grace",
  );
  if (outcome.code !== 0 || outcome.signal !== null) {
    throw new Error(
      `packaged server exited with code ${String(outcome.code)} signal ${String(outcome.signal)}`,
    );
  }
  log(
    `Packaged ${target} server became healthy and self-exited cleanly`,
  );
} finally {
  rmSync(extracted, { force: true, recursive: true });
}

async function unusedPorts(count) {
  const servers = Array.from({ length: count }, () => createServer());
  try {
    await Promise.all(
      servers.map(
        (server) =>
          new Promise((resolve, reject) => {
            server.once("error", reject);
            server.listen(0, "127.0.0.1", resolve);
          }),
      ),
    );
    return servers.map((server) => {
      const address = server.address();
      if (address === null || typeof address === "string") {
        throw new Error("failed to reserve a loopback port");
      }
      return address.port;
    });
  } finally {
    await Promise.all(
      servers.map(
        (server) =>
          new Promise((resolve, reject) => {
            if (!server.listening) {
              resolve();
              return;
            }
            server.close((error) => {
              if (error === undefined) {
                resolve();
              } else {
                reject(error);
              }
            });
          }),
      ),
    );
  }
}

async function pollHealth(managementPort, dapPort, exit) {
  const deadline = Date.now() + 5000;
  for (;;) {
    const result = await Promise.race([
      getHealth(managementPort),
      exit.then((outcome) => {
        throw new Error(
          `packaged server exited before health readiness: ${JSON.stringify(outcome)}`,
        );
      }),
    ]).catch((error) => {
      if (error?.code === "ECONNREFUSED") {
        return undefined;
      }
      throw error;
    });
    if (result !== undefined) {
      if (result.dap?.address !== `127.0.0.1:${String(dapPort)}`) {
        throw new Error(`packaged server advertised wrong DAP endpoint`);
      }
      return result;
    }
    if (Date.now() >= deadline) {
      throw new Error("packaged server did not become healthy within 5000ms");
    }
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

function getHealth(port) {
  return new Promise((resolve, reject) => {
    const req = request(
      {
        host: "127.0.0.1",
        port,
        path: "/api/health",
        method: "GET",
      },
      (response) => {
        const chunks = [];
        response.on("data", (chunk) => chunks.push(chunk));
        response.on("end", () => {
          if (response.statusCode !== 200) {
            reject(new Error(`health returned HTTP ${String(response.statusCode)}`));
            return;
          }
          try {
            resolve(JSON.parse(Buffer.concat(chunks).toString("utf8")));
          } catch (error) {
            reject(error);
          }
        });
      },
    );
    req.setTimeout(1000, () => {
      req.destroy(new Error("health probe timed out"));
    });
    req.once("error", reject);
    req.end();
  });
}

function withTimeout(promise, milliseconds, message) {
  return Promise.race([
    promise,
    new Promise((_, reject) => {
      setTimeout(() => reject(new Error(message)), milliseconds);
    }),
  ]);
}

function run(command, args) {
  const result = spawnSync(command, args, { stdio: "inherit" });
  if (result.error !== undefined) {
    throw result.error;
  }
  if (result.status !== 0) {
    throw new Error(
      `${command} exited with status ${String(result.status)}`,
    );
  }
}
import { Buffer } from "node:buffer";
import { log } from "node:console";
