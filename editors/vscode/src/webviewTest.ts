// The driver is enabled only in an isolated ExtensionMode.Test host. It observes
// and clicks the real document, so E2E assertions cannot pass on model state alone.
export type TestOperation = "inspect" | "selectGoroutine" | "selectFrame" | "expandVariable" | "openCreationSource";

export interface DisplayedState {
  readonly inspector: string;
  readonly creation: string;
  readonly highlightedLine: string;
  readonly frames: readonly string[];
  readonly variables: readonly string[];
}

export function driveDocument(document: Document, operation: TestOperation, target: number): DisplayedState {
  if (operation === "selectGoroutine") {
    document.querySelector<SVGGElement>(`[data-goid="${String(target)}"]`)?.dispatchEvent(
      new document.defaultView!.Event("click", { bubbles: true }),
    );
  } else if (operation === "selectFrame") {
    document.querySelector<HTMLButtonElement>(`[data-frame-id="${String(target)}"]`)?.click();
  } else if (operation === "expandVariable") {
    document.querySelector<HTMLButtonElement>(`[data-reference="${String(target)}"]`)?.click();
  } else if (operation === "openCreationSource") {
    document.querySelector<HTMLButtonElement>(".spawn-source .source-link")?.click();
  }
  return {
    inspector: (document.querySelector(".inspector")?.textContent ?? "").slice(0, 16_384),
    creation: (document.querySelector(".spawn-source")?.textContent ?? "").slice(0, 8192),
    highlightedLine: (document.querySelector(".creation-line")?.textContent ?? "").slice(0, 512),
    frames: [...document.querySelectorAll(".frame-name")].slice(0, 200).map((e) => (e.textContent ?? "").slice(0, 256)),
    variables: [...document.querySelectorAll(".variable-name")].slice(0, 1000).map((e) => (e.textContent ?? "").slice(0, 256)),
  };
}
