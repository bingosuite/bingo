import type { ConcurrencyViewModel } from "./model.js";
import { mountConcurrencyView } from "./webviewApp.js";

declare function acquireVsCodeApi(): {
  postMessage(message: Record<string, unknown>): void;
};

const vscode = acquireVsCodeApi();
const render = mountConcurrencyView(document, vscode);

window.addEventListener("message", (event: MessageEvent<unknown>) => {
  if (event.origin !== window.location.origin) {
    return;
  }
  if (isFitMessage(event.data)) {
    document.querySelector<HTMLButtonElement>(".graph-controls button")?.click();
    return;
  }
  if (!isRenderMessage(event.data)) {
    return;
  }
  if (event.data.revision !== event.data.model.revision) {
    return;
  }
  render(event.data.model);
});
vscode.postMessage({ type: "ready" });

function isRenderMessage(
  value: unknown,
): value is {
  readonly type: "render";
  readonly revision: number;
  readonly model: ConcurrencyViewModel;
} {
  return (
    typeof value === "object" &&
    value !== null &&
    "type" in value &&
    value.type === "render" &&
    "revision" in value &&
    Number.isSafeInteger(value.revision) &&
    "model" in value &&
    typeof value.model === "object" &&
    value.model !== null &&
    Object.keys(value).length === 3
  );
}

function isFitMessage(value: unknown): boolean {
  return (
    typeof value === "object" &&
    value !== null &&
    "type" in value &&
    value.type === "fit"
  );
}
