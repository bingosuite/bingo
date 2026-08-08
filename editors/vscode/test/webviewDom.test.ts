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
    assert.equal(document.querySelector(".graph-viewport")?.getAttribute("tabindex"), "0");
    assert.equal(document.querySelector(".graph-viewport")?.id, "concurrency-tree");
    assert.deepEqual(messages.at(-1), { type: "rendered", revision: 1 });
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
