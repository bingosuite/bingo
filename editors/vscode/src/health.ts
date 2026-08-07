import { request } from "node:http";

import type { BingoEndpoint } from "./configuration.js";

export const bingoServiceIdentity = "bingo";
export const managementApiVersion = 1;
export const wireProtocolVersion = "1.2";

const maximumHealthBytes = 64 * 1024;

export interface CompatibleHealth {
  readonly instanceId: string;
  readonly dapAddress: string;
}

export type HealthProbeResult =
  | { readonly kind: "absent" }
  | { readonly kind: "compatible"; readonly health: CompatibleHealth }
  | { readonly kind: "incompatible"; readonly reason: string }
  | { readonly kind: "transportError"; readonly error: Error };

export interface HealthProbe {
  (
    managementEndpoint: BingoEndpoint,
    dapEndpoint: BingoEndpoint,
    timeoutMs: number,
    signal: AbortSignal,
  ): Promise<HealthProbeResult>;
}

export async function probeBingoHealth(
  managementEndpoint: BingoEndpoint,
  dapEndpoint: BingoEndpoint,
  timeoutMs: number,
  signal: AbortSignal,
): Promise<HealthProbeResult> {
  try {
    const response = await getHealth(
      managementEndpoint,
      timeoutMs,
      signal,
    );
    return validateHealthResponse(response.statusCode, response.body, dapEndpoint);
  } catch (error: unknown) {
    if (isErrorCode(error, "ECONNREFUSED")) {
      return { kind: "absent" };
    }
    return { kind: "transportError", error: toError(error) };
  }
}

function getHealth(
  endpoint: BingoEndpoint,
  timeoutMs: number,
  signal: AbortSignal,
): Promise<{ readonly statusCode: number; readonly body: string }> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(abortError());
      return;
    }

    const req = request(
      {
        host: endpoint.host,
        port: endpoint.port,
        path: "/api/health",
        method: "GET",
        headers: {
          Accept: "application/json",
          "Cache-Control": "no-cache",
        },
      },
      (response) => {
        const chunks: Buffer[] = [];
        let size = 0;
        response.on("data", (chunk: Buffer) => {
          size += chunk.length;
          if (size > maximumHealthBytes) {
            req.destroy(new Error("bingo health response exceeded 64 KiB"));
            return;
          }
          chunks.push(chunk);
        });
        response.on("end", () => {
          resolve({
            statusCode: response.statusCode ?? 0,
            body: Buffer.concat(chunks).toString("utf8"),
          });
        });
      },
    );

    const onAbort = (): void => {
      req.destroy(abortError());
    };
    signal.addEventListener("abort", onAbort, { once: true });
    req.setTimeout(timeoutMs, () => {
      req.destroy(
        new Error(`bingo health request timed out after ${String(timeoutMs)}ms`),
      );
    });
    req.once("close", () => {
      signal.removeEventListener("abort", onAbort);
    });
    req.once("error", reject);
    req.end();
  });
}

export function validateHealthResponse(
  statusCode: number,
  body: string,
  expectedDAP: BingoEndpoint,
): HealthProbeResult {
  if (statusCode !== 200) {
    return {
      kind: "incompatible",
      reason: `health endpoint returned HTTP ${String(statusCode)}`,
    };
  }

  let decoded: unknown;
  try {
    decoded = JSON.parse(body) as unknown;
  } catch {
    return { kind: "incompatible", reason: "health endpoint did not return JSON" };
  }
  if (!isRecord(decoded)) {
    return { kind: "incompatible", reason: "health response must be an object" };
  }
  if (decoded.service !== bingoServiceIdentity) {
    return {
      kind: "incompatible",
      reason: `health service identity is ${JSON.stringify(decoded.service)}, expected "bingo"`,
    };
  }
  if (decoded.managementApiVersion !== managementApiVersion) {
    return {
      kind: "incompatible",
      reason: `management API version is ${JSON.stringify(decoded.managementApiVersion)}, expected ${String(managementApiVersion)}`,
    };
  }
  if (decoded.wireProtocolVersion !== wireProtocolVersion) {
    return {
      kind: "incompatible",
      reason: `wire protocol version is ${JSON.stringify(decoded.wireProtocolVersion)}, expected ${wireProtocolVersion}`,
    };
  }
  if (typeof decoded.instanceId !== "string" || decoded.instanceId.length === 0) {
    return {
      kind: "incompatible",
      reason: "health response has no instanceId",
    };
  }
  if (!isRecord(decoded.dap) || decoded.dap.enabled !== true) {
    return {
      kind: "incompatible",
      reason: "bingo DAP listener is not enabled",
    };
  }
  if (typeof decoded.dap.address !== "string") {
    return {
      kind: "incompatible",
      reason: "health response has no DAP address",
    };
  }

  const advertised = parseAddress(decoded.dap.address);
  if (advertised === undefined) {
    return {
      kind: "incompatible",
      reason: `health DAP address is invalid: ${decoded.dap.address}`,
    };
  }
  if (advertised.port !== expectedDAP.port) {
    return {
      kind: "incompatible",
      reason: `health DAP port is ${String(advertised.port)}, expected ${String(expectedDAP.port)}`,
    };
  }
  if (!isWildcardHost(advertised.host) && advertised.host !== expectedDAP.host) {
    return {
      kind: "incompatible",
      reason: `health DAP host is ${advertised.host}, expected ${expectedDAP.host}`,
    };
  }

  return {
    kind: "compatible",
    health: {
      instanceId: decoded.instanceId,
      dapAddress: decoded.dap.address,
    },
  };
}

function parseAddress(value: string): BingoEndpoint | undefined {
  if (value.startsWith("[")) {
    const close = value.indexOf("]");
    if (close < 0 || value[close + 1] !== ":") {
      return undefined;
    }
    return endpointFromParts(value.slice(1, close), value.slice(close + 2));
  }
  const colon = value.lastIndexOf(":");
  if (colon < 0) {
    return undefined;
  }
  return endpointFromParts(value.slice(0, colon), value.slice(colon + 1));
}

function endpointFromParts(
  host: string,
  portText: string,
): BingoEndpoint | undefined {
  const port = Number(portText);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    return undefined;
  }
  return { host, port };
}

function isWildcardHost(host: string): boolean {
  return host === "" || host === "0.0.0.0" || host === "::";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isErrorCode(error: unknown, code: string): boolean {
  return (
    error instanceof Error &&
    "code" in error &&
    (error as Error & { code?: unknown }).code === code
  );
}

function toError(error: unknown): Error {
  return error instanceof Error ? error : new Error(String(error));
}

function abortError(): Error {
  const error = new Error("operation cancelled");
  error.name = "AbortError";
  return error;
}
