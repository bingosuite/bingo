import type { ConcurrencyViewModel } from "./model.js";

export type HostMessage =
  | {
      readonly type: "render";
      readonly generation: number;
      readonly revision: number;
      readonly model: ConcurrencyViewModel;
    }
  | { readonly type: "fit" };

export type WebviewMessage =
  | { readonly type: "ready" }
  | {
      readonly type: "rendered";
      readonly generation: number;
      readonly revision: number;
    }
  | { readonly type: "selectGoroutine"; readonly id: number }
  | { readonly type: "selectFrame"; readonly id: number }
  | { readonly type: "expandVariable"; readonly reference: number }
  | { readonly type: "refreshInspection" }
  | {
      readonly type: "openSource";
      readonly target: "created" | "start" | "current" | "frame";
      readonly frameId: number;
    }
  | { readonly type: "selectSession"; readonly id: string }
  | { readonly type: "refresh" }
  | { readonly type: "fit" }
  | { readonly type: "copySnapshot" };

export function decodeWebviewMessage(value: unknown): WebviewMessage {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new TypeError("webview message must be an object");
  }
  const message = value as Record<string, unknown>;
  if (typeof message.type !== "string") {
    throw new TypeError("webview message requires a type");
  }
  switch (message.type) {
    case "ready":
    case "refresh":
    case "fit":
    case "copySnapshot":
    case "refreshInspection":
      exactKeys(message, ["type"]);
      return { type: message.type };
    case "rendered":
      exactKeys(message, ["type", "generation", "revision"]);
      return {
        type: "rendered",
        generation: safeInteger(message.generation, "generation", 1),
        revision: safeInteger(message.revision, "revision", 0),
      };
    case "selectGoroutine":
      exactKeys(message, ["type", "id"]);
      return {
        type: "selectGoroutine",
        id: safeInteger(message.id, "goroutine id", 0),
      };
    case "selectFrame":
      exactKeys(message, ["type", "id"]);
      return {
        type: "selectFrame",
        id: safeInteger(message.id, "stack frame id", 1),
      };
    case "expandVariable":
      exactKeys(message, ["type", "reference"]);
      return {
        type: "expandVariable",
        reference: safeInteger(
          message.reference,
          "variables reference",
          1,
        ),
      };
    case "openSource":
      exactKeys(message, ["type", "target", "frameId"]);
      if (
        message.target !== "created" && message.target !== "start" &&
        message.target !== "current" && message.target !== "frame"
      ) {
        throw new TypeError("source target must name displayed metadata");
      }
      return {
        type: "openSource",
        target: message.target,
        frameId: safeInteger(message.frameId, "frame id", message.target === "frame" ? 1 : 0),
      };
    case "selectSession":
      exactKeys(message, ["type", "id"]);
      if (
        typeof message.id !== "string" ||
        message.id.length === 0 ||
        message.id.length > 256
      ) {
        throw new TypeError("debug session id must be a bounded non-empty string");
      }

      return {
        type: "selectSession",
        id: message.id,
      };
    default:
      throw new TypeError(
        `unknown webview message ${JSON.stringify(message.type)}`,
      );
  }
}

export interface ActionContext {
  readonly generation: number;
  readonly revision: number;
  readonly debugSessionId: string;
  readonly goroutineId: number;
}

export function decodeAction(value: unknown): {
  readonly context: ActionContext;
  readonly action: WebviewMessage;
} {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new TypeError("action must be an object");
  }
  const envelope = value as Record<string, unknown>;
  exactKeys(envelope, ["type", "generation", "revision", "debugSessionId", "goroutineId", "action"]);
  if (envelope.type !== "action" || typeof envelope.debugSessionId !== "string" ||
    envelope.debugSessionId.length > 256) {
    throw new TypeError("action must name a bounded debug session");
  }
  const action = decodeWebviewMessage(envelope.action);
  if (action.type === "ready" || action.type === "rendered") {
    throw new TypeError("document lifecycle is not an action");
  }
  return {
    context: {
      generation: safeInteger(envelope.generation, "generation", 1),
      revision: safeInteger(envelope.revision, "revision", 0),
      debugSessionId: envelope.debugSessionId,
      goroutineId: safeInteger(envelope.goroutineId, "goroutine", 0),
    },
    action,
  };
}

function exactKeys(
  value: Record<string, unknown>,
  expected: readonly string[],
): void {
  const actual = Object.keys(value).sort((left, right) =>
    left.localeCompare(right),
  );
  const wanted = [...expected].sort((left, right) =>
    left.localeCompare(right),
  );
  if (
    actual.length !== wanted.length ||
    actual.some((key, index) => key !== wanted[index])
  ) {
    throw new TypeError("webview message has unexpected fields");
  }
}

function safeInteger(value: unknown, label: string, minimum: number): number {
  if (
    typeof value !== "number" ||
    !Number.isSafeInteger(value) ||
    value < minimum
  ) {
    throw new TypeError(
      `${label} must be a safe integer >= ${String(minimum)}`,
    );
  }
  return value;
}
