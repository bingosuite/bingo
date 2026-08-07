export const defaultDapHost = "127.0.0.1";
export const defaultDapPort = 4711;
export const defaultManagementHost = "127.0.0.1";
export const defaultManagementPort = 6060;
export const defaultServerMode = "auto";
export const defaultServerReadyTimeoutMs = 5000;
export const defaultManagedIdleTimeoutMs = 30000;

type JsonRecord = Record<string, unknown>;

export type ServerMode = "auto" | "connectOnly";

export interface BingoEndpoint {
  readonly host: string;
  readonly port: number;
}

export interface BingoServerConfiguration {
  readonly mode: ServerMode;
  readonly managementEndpoint: BingoEndpoint;
  readonly dapEndpoint: BingoEndpoint;
  readonly readyTimeoutMs: number;
  readonly idleTimeoutMs: number;
}

export interface ValidatedBingoConfiguration {
  readonly endpoint: BingoEndpoint;
  readonly request: "attach" | "launch";
  readonly server: BingoServerConfiguration;
}

export class ConfigurationError extends Error {
  public override readonly name = "ConfigurationError";
}

export function validateBingoConfiguration(
  value: unknown,
): ValidatedBingoConfiguration {
  const config = requireRecord(value, "debug configuration");
  const server = resolveServerConfiguration(config);
  const endpoint = server.dapEndpoint;
  const request = requireNonEmptyString(config, "request");

  validateOptionalBoolean(config, "stopOnEntry");

  if (request === "launch") {
    requireNonEmptyString(config, "program");
    validateOptionalStringArray(config, "args");
    validateOptionalStringArray(config, "env");
    return { endpoint, request, server };
  }

  if (request === "attach") {
    validateAttach(config);
    return { endpoint, request, server };
  }

  throw new ConfigurationError(
    `bingo request must be "launch" or "attach", got ${JSON.stringify(request)}`,
  );
}

export function resolveEndpoint(value: unknown): BingoEndpoint {
  const config = requireRecord(value, "debug configuration");
  return resolveNamedEndpoint(
    config,
    "dapHost",
    "dapPort",
    defaultDapHost,
    defaultDapPort,
  );
}

export function resolveServerConfiguration(
  value: unknown,
): BingoServerConfiguration {
  const config = requireRecord(value, "debug configuration");
  const mode = resolveServerMode(config.serverMode);
  const managementEndpoint = resolveNamedEndpoint(
    config,
    "managementHost",
    "managementPort",
    defaultManagementHost,
    defaultManagementPort,
  );
  const dapEndpoint = resolveEndpoint(config);
  const readyTimeoutMs = resolveBoundedInteger(
    config.serverReadyTimeoutMs,
    defaultServerReadyTimeoutMs,
    "serverReadyTimeoutMs",
    100,
    120000,
  );
  const idleTimeoutMs = resolveBoundedInteger(
    config.managedIdleTimeoutMs,
    defaultManagedIdleTimeoutMs,
    "managedIdleTimeoutMs",
    1,
    86400000,
  );

  return {
    mode,
    managementEndpoint,
    dapEndpoint,
    readyTimeoutMs,
    idleTimeoutMs,
  };
}

function resolveServerMode(value: unknown): ServerMode {
  if (value === undefined) {
    return defaultServerMode;
  }
  if (value === "auto" || value === "connectOnly") {
    return value;
  }
  throw new ConfigurationError(
    'bingo serverMode must be "auto" or "connectOnly"',
  );
}

function resolveNamedEndpoint(
  config: JsonRecord,
  hostKey: string,
  portKey: string,
  defaultHost: string,
  defaultPort: number,
): BingoEndpoint {
  const host =
    config[hostKey] === undefined
      ? defaultHost
      : requireNonEmptyString(config, hostKey);
  const port =
    config[portKey] === undefined
      ? defaultPort
      : requirePort(config[portKey], portKey);
  return { host, port };
}

function validateAttach(config: JsonRecord): void {
  const sessionValue = config.session;
  const pidValue = config.pid;
  const hasSession = sessionValue !== undefined;
  const hasPID = typeof pidValue === "number" && isPositiveInteger(pidValue);

  if (hasSession) {
    requireNonEmptyString(config, "session");
  }
  if (pidValue !== undefined && !hasPID) {
    throw new ConfigurationError("bingo pid must be a positive integer");
  }
  if (hasSession === hasPID) {
    throw new ConfigurationError(
      "bingo attach requires exactly one of session or pid",
    );
  }

  validateOptionalString(config, "binaryPath");
}

function validateOptionalBoolean(config: JsonRecord, key: string): void {
  const value = config[key];
  if (value !== undefined && typeof value !== "boolean") {
    throw new ConfigurationError(`bingo ${key} must be a boolean`);
  }
}

function validateOptionalString(config: JsonRecord, key: string): void {
  const value = config[key];
  if (value !== undefined && typeof value !== "string") {
    throw new ConfigurationError(`bingo ${key} must be a string`);
  }
}

function validateOptionalStringArray(config: JsonRecord, key: string): void {
  const value = config[key];
  if (
    value !== undefined &&
    (!Array.isArray(value) || value.some((item) => typeof item !== "string"))
  ) {
    throw new ConfigurationError(`bingo ${key} must be an array of strings`);
  }
}

function requirePort(value: unknown, key: string): number {
  if (
    typeof value !== "number" ||
    !Number.isInteger(value) ||
    value < 1 ||
    value > 65535
  ) {
    throw new ConfigurationError(
      `bingo ${key} must be an integer between 1 and 65535`,
    );
  }
  return value;
}

function resolveBoundedInteger(
  value: unknown,
  defaultValue: number,
  key: string,
  minimum: number,
  maximum: number,
): number {
  if (value === undefined) {
    return defaultValue;
  }
  if (
    typeof value !== "number" ||
    !Number.isInteger(value) ||
    value < minimum ||
    value > maximum
  ) {
    throw new ConfigurationError(
      `bingo ${key} must be an integer between ${String(minimum)} and ${String(maximum)}`,
    );
  }
  return value;
}

function requireNonEmptyString(config: JsonRecord, key: string): string {
  const value = config[key];
  if (typeof value !== "string" || value.trim().length === 0) {
    throw new ConfigurationError(`bingo ${key} must be a non-empty string`);
  }
  return value.trim();
}

function requireRecord(value: unknown, label: string): JsonRecord {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new ConfigurationError(`bingo ${label} must be an object`);
  }
  return value as JsonRecord;
}

function isPositiveInteger(value: number): boolean {
  return Number.isInteger(value) && value > 0;
}
