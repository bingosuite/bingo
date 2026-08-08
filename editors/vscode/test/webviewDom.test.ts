import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { parseHTML } from "linkedom";

import type { ConcurrencyViewModel, SessionModel } from "../src/model.js";
import { toSessionViewModel } from "../src/model.js";
import { mountConcurrencyView } from "../src/webviewApp.js";
import { goroutine, snapshot, thread } from "./fixtures.js";

function model(
  patch: Partial<SessionModel> = {},
): ConcurrencyViewModel {
  const session: SessionModel = {
    debugSessionId: "debug",
    debugSessionName: "Level 5",
    sessionId: "session",
    connection: "connected",
    sessionState: "suspended",
    clients: 2,
    lastStop: "Breakpoint at inventory.go:42",
    error: "",
    seqGap: "",
    lastSeq: 1,
    snapshot: snapshot(
      [
        goroutine(1, 0, { current: true, threadId: 10 }),
        goroutine(2, 1, { status: "running", waitReason: "" }),
      ],
      [thread(10, 1), thread(11)],
    ),
    selectedGoroutine: 1,
    timeline: [{ id: 2, action: "created", at: 1 }],
    ...patch,
  };
  return {
    revision: 1,
    activeDebugSessionId: "debug",
    sessions: [toSessionViewModel(session)],
  };
}

describe("concurrency webview DOM", () => {
  it("renders nodes, edges, threads, inspector, lifecycle, and accessibility", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const messages: Record<string, unknown>[] = [];
    const render = mountConcurrencyView(document, {
      postMessage(message) {
        messages.push(message);
      },
    });
    render(model());
    assert.equal(document.querySelectorAll(".tree-node").length, 2);
    assert.equal(document.querySelectorAll(".tree-edge").length, 1);
    assert.equal(document.querySelectorAll(".thread").length, 2);
    assert.match(document.querySelector(".inspector")?.textContent ?? "", /main\.worker/);
    assert.match(document.querySelector(".timeline")?.textContent ?? "", /\+ g2/);
    assert.equal(document.querySelector("svg")?.getAttribute("role"), "tree");
    const treeItems = document.querySelectorAll(".tree-node");
    assert.equal(treeItems[0]?.getAttribute("aria-level"), "1");
    assert.match(treeItems[0]?.getAttribute("aria-label") ?? "", /root goroutine/);
    assert.equal(treeItems[0]?.getAttribute("aria-selected"), "true");
    assert.equal(treeItems[1]?.getAttribute("aria-level"), "2");
    assert.match(
      treeItems[1]?.getAttribute("aria-label") ?? "",
      /child of goroutine 1/,
    );
    assert.equal(treeItems[1]?.getAttribute("aria-selected"), "false");
    assert.equal(treeItems[1]?.getAttribute("aria-posinset"), "1");
    assert.equal(treeItems[1]?.getAttribute("aria-setsize"), "1");
    assert.equal(document.querySelector(".graph-viewport")?.getAttribute("tabindex"), "0");
    assert.equal(document.querySelector(".graph-viewport")?.id, "concurrency-tree");
    assert.deepEqual(messages.at(-1), {
      type: "rendered",
      generation: 1,
      revision: 1,
    });
  });

  it("filters and selects through DOM events", () => {
    const { document, window } = parseHTML("<html><body><div id=app></div></body></html>");
    const messages: Record<string, unknown>[] = [];
    mountConcurrencyView(document, { postMessage: (message) => messages.push(message) })(model());
    const search = document.querySelector<HTMLInputElement>('input[type="search"]')!;
    search.value = "running";
    search.dispatchEvent(new window.Event("input"));
    assert.equal(document.querySelectorAll(".tree-node.filtered").length, 0);
    document.querySelectorAll<SVGGElement>(".tree-node")[1]?.dispatchEvent(new window.Event("click"));
    assert.deepEqual(messages.at(-1), { type: "selectGoroutine", id: 2 });
  });

  it("keeps matching descendants connected to visible ancestors", () => {
    const { document, window } = parseHTML("<html><body><div id=app></div></body></html>");
    const nested = snapshot([
      goroutine(1, 0, { current: true }),
      goroutine(2, 1),
      goroutine(3, 2, { waitReason: "needle" }),
    ]);
    mountConcurrencyView(document, { postMessage() {} })(
      model({ snapshot: nested }),
    );
    const search = document.querySelector<HTMLInputElement>('input[type="search"]')!;
    search.value = "needle";
    search.dispatchEvent(new window.Event("input"));
    assert.equal(document.querySelectorAll(".tree-node.filtered").length, 0);
    assert.equal(document.querySelectorAll(".tree-edge.filtered").length, 0);
  });

  it("describes cycle-normalized roots without claiming their parent is absent", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    mountConcurrencyView(document, { postMessage() {} })(
      model({
        snapshot: snapshot([
          goroutine(1, 2, { current: true }),
          goroutine(2, 1),
        ]),
      }),
    );

    const label =
      document.querySelector('[data-goid="1"]')?.getAttribute("aria-label") ?? "";
    assert.match(label, /displayed root after cycle normalization/);
    assert.match(label, /reported parent goroutine 2/);
    assert.doesNotMatch(label, /not displayed/);
  });

  it("compacts and fits a large tree around filtered matches", () => {
    const { document, window } = parseHTML("<html><body><div id=app></div></body></html>");
    const goroutines = [goroutine(1, 0, { current: true })];
    for (let id = 2; id <= 500; id += 1) {
      goroutines.push(
        goroutine(id, id - 1, id === 500 ? { waitReason: "needle" } : {}),
      );
    }
    mountConcurrencyView(document, { postMessage() {} })(
      model({ snapshot: snapshot(goroutines), selectedGoroutine: 1 }),
    );
    assert.match(document.querySelector("svg")?.getAttribute("viewBox") ?? "", /41086$/);

    const search = document.querySelector<HTMLInputElement>('input[type="search"]')!;
    search.value = "needle";
    search.dispatchEvent(new window.Event("input"));

    assert.equal(document.querySelectorAll(".tree-node").length, 5);
    assert.equal(
      document.querySelector("svg")?.getAttribute("viewBox"),
      "0 0 1142 496",
    );
    assert.match(
      document.querySelector('[data-goid="496"]')?.getAttribute("aria-label") ??
        "",
      /displayed root, reported parent goroutine 495 is not displayed/,
    );
    assert.equal(
      document.querySelector('[data-goid="500"]')?.getAttribute("transform"),
      "translate(874 370)",
    );

    search.value = "";
    search.dispatchEvent(new window.Event("input"));
    assert.equal(document.querySelectorAll(".tree-node").length, 500);
    assert.match(document.querySelector("svg")?.getAttribute("viewBox") ?? "", /41086$/);
  });

  it("renders empty and error states", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const render = mountConcurrencyView(document, { postMessage() {} });
    render({ revision: 1, activeDebugSessionId: "", sessions: [] });
    assert.match(document.body.textContent, /Start a bingo debug session/);
    render(model({ error: "socket rejected", snapshot: undefined }));
    assert.equal(document.querySelector(".callout.error")?.getAttribute("role"), "alert");
    assert.match(document.body.textContent, /socket rejected/);
  });

  it("keeps hostile tracee strings inert", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const hostile = '<img src=x onerror="globalThis.pwned=true">';
    const snap = snapshot([goroutine(1, 0, { waitReason: hostile, current: true })]);
    mountConcurrencyView(document, { postMessage() {} })(
      model({ snapshot: snap, selectedGoroutine: 1 }),
    );
    assert.equal(document.querySelector("img"), null);
    assert.match(document.body.textContent, /<img src=x/);
  });
});
