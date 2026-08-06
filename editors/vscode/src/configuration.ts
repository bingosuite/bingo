export const defaultDapHost = "127.0.0.1";
export const defaultDapPort = 4711;

type JsonRecord = Record<string, unknown>;

export interface BingoEndpoint {
  readonly host: string;
  readonly port: number;
}

export interface ValidatedBingoConfiguration {
  readonly endpoint: BingoEndpoint;
  readonly request: "attach" | "launch";
}

export class ConfigurationError extends Error {
  public override readonly name = "ConfigurationError";
}

export function validateBingoConfiguration(
  value: unknown,
): ValidatedBingoConfiguration {
  const config = requireRecord(value, "debug configuration");
  const endpoint = resolveEndpoint(config);
  const request = requireNonEmptyString(config, "request");

  validateOptionalBoolean(config, "stopOnEntry");

  if (request === "launch") {
    requireNonEmptyString(config, "program");
    validateOptionalStringArray(config, "args");
    validateOptionalStringArray(config, "env");
    return { endpoint, request };
  }

  if (request === "attach") {
    validateAttach(config);
    return { endpoint, request };
  }

  throw new ConfigurationError(
    `bingo request must be "launch" or "attach", got ${JSON.stringify(request)}`,
  );
}

export function resolveEndpoint(value: unknown): BingoEndpoint {
  const config = requireRecord(value, "debug configuration");
  const hostValue = config.dapHost;
  const portValue = config.dapPort;

  const host =
    hostValue === undefined
      ? defaultDapHost
      : requireNonEmptyString(config, "dapHost");
  const port = portValue === undefined ? defaultDapPort : requirePort(portValue);

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

function requirePort(value: unknown): number {
  if (
    typeof value !== "number" ||
    !Number.isInteger(value) ||
    value < 1 ||
    value > 65535
  ) {
    throw new ConfigurationError(
      "bingo dapPort must be an integer between 1 and 65535",
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
