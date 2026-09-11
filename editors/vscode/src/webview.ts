import type { ConcurrencyViewModel } from "./model.js";
import { mountConcurrencyView } from "./webviewApp.js";
import { driveDocument } from "./webviewTest.js";

declare function acquireVsCodeApi(): {
  postMessage(message: Record<string, unknown>): void;
};

const vscode = acquireVsCodeApi();
const render = mountConcurrencyView(document, vscode);

window.addEventListener("message", (event: MessageEvent<unknown>) => {
  if (event.origin !== window.location.origin) {
    return;
  }
  const probe = event.data;
  if (typeof probe === "object" && probe !== null && "type" in probe &&
    probe.type === "testInspect" && "id" in probe && Number.isSafeInteger(probe.id) &&
    "target" in probe && typeof probe.target === "number" && Number.isSafeInteger(probe.target) &&
    probe.target >= 0 && "operation" in probe &&
    (probe.operation === "inspect" || probe.operation === "selectGoroutine" ||
      probe.operation === "selectFrame" || probe.operation === "expandVariable" ||
      probe.operation === "openCreationSource")) {
    vscode.postMessage({
      type: "testResult",
      id: probe.id,
      state: driveDocument(document, probe.operation, probe.target),
    });
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
  render(event.data.model, event.data.generation);
});
vscode.postMessage({ type: "ready" });

function isRenderMessage(
  value: unknown,
): value is {
  readonly type: "render";
  readonly generation: number;
  readonly revision: number;
  readonly model: ConcurrencyViewModel;
} {
  return (
    typeof value === "object" &&
    value !== null &&
    "type" in value &&
    value.type === "render" &&
    "generation" in value &&
    typeof value.generation === "number" &&
    Number.isSafeInteger(value.generation) &&
    value.generation > 0 &&
    "revision" in value &&
    Number.isSafeInteger(value.revision) &&
    "model" in value &&
    typeof value.model === "object" &&
    value.model !== null &&
    Object.keys(value).length === 4
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
