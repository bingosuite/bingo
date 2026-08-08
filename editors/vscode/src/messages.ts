import type { ConcurrencyViewModel } from "./model.js";

export type HostMessage =
  | {
      readonly type: "render";
      readonly revision: number;
      readonly model: ConcurrencyViewModel;
    }
  | { readonly type: "fit" };

export type WebviewMessage =
  | { readonly type: "ready" }
  | { readonly type: "rendered"; readonly revision: number }
  | { readonly type: "selectGoroutine"; readonly id: number }
  | { readonly type: "selectSession"; readonly id: string }
  | { readonly type: "refresh" }
  | { readonly type: "fit" }
  | { readonly type: "copySnapshot" };

export function decodeWebviewMessage(value: unknown): WebviewMessage {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("webview message must be an object");
  }
  const message = value as Record<string, unknown>;
  if (typeof message.type !== "string") {
    throw new Error("webview message requires a type");
  }
  switch (message.type) {
    case "ready":
    case "refresh":
    case "fit":
    case "copySnapshot":
      exactKeys(message, ["type"]);
      return { type: message.type };
    case "rendered":
      exactKeys(message, ["type", "revision"]);
      return {
        type: "rendered",
        revision: safeInteger(message.revision, "revision", 0),
      };
    case "selectGoroutine":
      exactKeys(message, ["type", "id"]);
      return {
        type: "selectGoroutine",
        id: safeInteger(message.id, "goroutine id", 1),
      };
    case "selectSession":
      exactKeys(message, ["type", "id"]);
      if (
        typeof message.id !== "string" ||
        message.id.length === 0 ||
        message.id.length > 256
      ) {
        throw new Error("debug session id must be a bounded non-empty string");
      }
      return {
        type: "selectSession",
        id: message.id,
      };
    default:
      throw new Error(`unknown webview message ${JSON.stringify(message.type)}`);
  }
}

function exactKeys(
  value: Record<string, unknown>,
  expected: readonly string[],
): void {
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  if (
    actual.length !== wanted.length ||
    actual.some((key, index) => key !== wanted[index])
  ) {
    throw new Error("webview message has unexpected fields");
  }
}

function safeInteger(value: unknown, label: string, minimum: number): number {
  if (
    typeof value !== "number" ||
    !Number.isSafeInteger(value) ||
    value < minimum
  ) {
    throw new Error(`${label} must be a safe integer >= ${String(minimum)}`);
  }
  return value;
}
