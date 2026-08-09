import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { parseHTML } from "linkedom";

import type { ConcurrencyViewModel, SessionModel } from "../src/model.js";
import { toSessionViewModel } from "../src/model.js";
import { mountConcurrencyView } from "../src/webviewApp.js";
import { goroutine, snapshot, thread } from "./fixtures.js";

const activeElements = new WeakMap<Document, Element>();
const focusedDocuments = new WeakMap<Document, boolean>();

function testDOM() {
  const { document, window } = parseHTML(
    "<html><body><div id=app></div></body></html>",
  );
  const focus = function (this: HTMLElement | SVGElement): void {
    activeElements.set(this.ownerDocument, this);
    focusedDocuments.set(this.ownerDocument, true);
  };
  for (const prototype of [
    window.HTMLElement.prototype,
    window.SVGElement.prototype,
  ]) {
    Object.defineProperty(prototype, "focus", {
      configurable: true,
      value: focus,
    });
  }

  const documentPrototype = Object.getPrototypeOf(document) as object;
  Object.defineProperty(documentPrototype, "activeElement", {
    configurable: true,
    get(this: Document): Element | null {
      return activeElements.get(this) ?? null;
    },
  });
  Object.defineProperty(documentPrototype, "hasFocus", {
    configurable: true,
    value(this: Document): boolean {
      return focusedDocuments.get(this) ?? false;
    },
  });

  return {
    document,
    window,
    setDocumentFocused: (focused: boolean): void => {
      focusedDocuments.set(document, focused);
    },
  };
}

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

function multiSessionModel(
  activeDebugSessionId = "debug",
  revision = 1,
): ConcurrencyViewModel {
  const base = model();
  const first = base.sessions[0];
  if (first === undefined) {
    throw new Error("test model requires a session");
  }
  return {
    ...base,
    revision,
    activeDebugSessionId,
    sessions: [
      first,
      {
        ...first,
        debugSessionId: "debug-2",
        debugSessionName: "Level 2",
        sessionId: "session-2",
      },
    ],
  };
}

describe("concurrency webview DOM", () => {
  it("renders nodes, edges, threads, inspector, lifecycle, and accessibility", () => {
    const { document } = testDOM();
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
    const { document, window } = testDOM();
    const messages: Record<string, unknown>[] = [];
    mountConcurrencyView(document, { postMessage: (message) => messages.push(message) })(model());
    const search = document.querySelector<HTMLInputElement>('input[type="search"]')!;
    search.value = "running";
    search.dispatchEvent(new window.Event("input"));
    assert.equal(document.querySelectorAll(".tree-node.filtered").length, 0);
    document.querySelectorAll<SVGGElement>(".tree-node")[1]?.dispatchEvent(new window.Event("click"));
    assert.deepEqual(messages.at(-1), { type: "selectGoroutine", id: 2 });
  });

  it("preserves session selector focus across selection rerenders", () => {
    const { document, window } = testDOM();
    const messages: Record<string, unknown>[] = [];
    const render = mountConcurrencyView(document, {
      postMessage: (message) => messages.push(message),
    });
    render(multiSessionModel());
    const before = document.querySelector<HTMLSelectElement>(
      '[data-focus="session-selector"]',
    );
    assert.ok(before);
    before.focus();
    const options = before.querySelectorAll<HTMLOptionElement>("option");
    const firstOption = options[0];
    const secondOption = options[1];
    assert.ok(firstOption);
    assert.ok(secondOption);
    firstOption.selected = false;
    secondOption.selected = true;
    before.dispatchEvent(new window.Event("change"));
    assert.deepEqual(messages.at(-1), {
      type: "selectSession",
      id: "debug-2",
    });

    render(multiSessionModel("debug-2", 2));
    const after = document.querySelector<HTMLSelectElement>(
      '[data-focus="session-selector"]',
    );
    assert.ok(after);
    assert.notEqual(after, before);
    assert.equal(document.activeElement, after);
  });

  it("preserves every toolbar control across model revisions", () => {
    const { document } = testDOM();
    const render = mountConcurrencyView(document, { postMessage() {} });
    render(model());
    const tokens = [
      "refresh",
      "copy-snapshot",
      "fit",
      "zoom-out",
      "zoom-in",
    ] as const;

    let revision = 2;
    for (const token of tokens) {
      const before = document.querySelector<HTMLButtonElement>(
        `[data-focus="${token}"]`,
      );
      assert.ok(before);
      before.focus();
      render({ ...model(), revision });
      revision += 1;
      const after = document.querySelector<HTMLButtonElement>(
        `[data-focus="${token}"]`,
      );
      assert.ok(after);
      assert.notEqual(after, before);
      assert.equal(document.activeElement, after);
    }
  });

  it("retains search, viewport, and tree-node focus behavior", () => {
    const { document } = testDOM();
    const render = mountConcurrencyView(document, { postMessage() {} });
    render(model());
    const selectors = [
      "#bingo-goroutine-filter",
      "#concurrency-tree",
      '[data-goid="1"]',
    ] as const;

    let revision = 2;
    for (const selector of selectors) {
      const before = document.querySelector<HTMLElement | SVGElement>(selector);
      assert.ok(before);
      before.focus();
      render({ ...model(), revision });
      revision += 1;
      const after = document.querySelector<HTMLElement | SVGElement>(selector);
      assert.ok(after);
      assert.notEqual(after, before);
      assert.equal(document.activeElement, after);
    }
  });

  it("does not restore focus when the webview document is blurred", () => {
    const { document, setDocumentFocused } = testDOM();
    const render = mountConcurrencyView(document, { postMessage() {} });
    render(model());
    const before = document.querySelector<HTMLInputElement>(
      "#bingo-goroutine-filter",
    );
    assert.ok(before);
    before.focus();
    setDocumentFocused(false);

    render({ ...model(), revision: 2 });
    const after = document.querySelector<HTMLInputElement>(
      "#bingo-goroutine-filter",
    );
    assert.ok(after);
    assert.notEqual(after, before);
    assert.equal(document.activeElement, before);
  });

  it("does nothing when the focused control is absent after rendering", () => {
    const { document } = testDOM();
    const render = mountConcurrencyView(document, { postMessage() {} });
    render(multiSessionModel());
    const before = document.querySelector<HTMLSelectElement>(
      '[data-focus="session-selector"]',
    );
    assert.ok(before);
    before.focus();

    render({ revision: 2, activeDebugSessionId: "", sessions: [] });
    assert.equal(
      document.querySelector('[data-focus="session-selector"]'),
      null,
    );
    assert.equal(document.activeElement, before);
  });

  it("moves DOM focus with keyboard tree selection", () => {
    const { document, window } = testDOM();
    const messages: Record<string, unknown>[] = [];
    mountConcurrencyView(document, {
      postMessage: (message) => messages.push(message),
    })(model());
    const first = document.querySelector<SVGGElement>('[data-goid="1"]')!;
    const second = document.querySelector<SVGGElement>('[data-goid="2"]')!;
    const scene = document.querySelector<SVGGElement>("svg > g")!;
    const querySelector = scene.querySelector.bind(scene);
    let selectorCalls = 0;
    Object.defineProperty(scene, "querySelector", {
      value: (selector: string): Element | null => {
        selectorCalls += 1;
        return querySelector(selector);
      },
    });
    first.focus();
    const event = new window.Event("keydown", {
      bubbles: true,
      cancelable: true,
    });
    Object.defineProperty(event, "key", { value: "ArrowDown" });
    first.dispatchEvent(event);

    assert.equal(document.activeElement, second);
    assert.equal(selectorCalls, 1);
    assert.deepEqual(messages.at(-1), {
      type: "selectGoroutine",
      id: 2,
    });
  });

  it("keeps matching descendants connected to visible ancestors", () => {
    const { document, window } = testDOM();
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
    const { document } = testDOM();
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

  it("finds, compacts, and fits matches beyond the rendering cap", () => {
    const { document, window } = testDOM();
    const goroutines = [goroutine(1, 0, { current: true })];
    for (let id = 2; id <= 1000; id += 1) {
      goroutines.push(
        goroutine(id, id - 1, id === 1000 ? { waitReason: "needle" } : {}),
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
      document.querySelector('[data-goid="996"]')?.getAttribute("aria-label") ??
        "",
      /displayed root, reported parent goroutine 995 is not displayed/,
    );
    assert.equal(
      document.querySelector('[data-goid="1000"]')?.getAttribute("transform"),
      "translate(874 370)",
    );

    search.value = "";
    search.dispatchEvent(new window.Event("input"));
    assert.equal(document.querySelectorAll(".tree-node").length, 500);
    assert.match(document.querySelector("svg")?.getAttribute("viewBox") ?? "", /41086$/);
  });

  it("keeps fit and zoom controls safe for an empty filter result", () => {
    const { document, window } = testDOM();
    mountConcurrencyView(document, { postMessage() {} })(model());
    const search = document.querySelector<HTMLInputElement>(
      'input[type="search"]',
    )!;
    search.value = "no-such-goroutine";
    search.dispatchEvent(new window.Event("input"));

    assert.equal(document.querySelectorAll(".tree-node").length, 0);
    assert.match(
      document.querySelector(".graph-panel")?.textContent ?? "",
      /No goroutines in this snapshot/,
    );
    const controls = [
      ...document.querySelectorAll<HTMLButtonElement>(
        ".graph-controls button",
      ),
    ];
    assert.deepEqual(
      controls.map((control) => control.textContent),
      ["Fit", "−", "+"],
    );
    assert.doesNotThrow(() => {
      for (const control of controls) {
        control.click();
      }
    });
    assert.equal(document.querySelectorAll(".tree-node").length, 0);
  });

  it("renders empty and error states", () => {
    const { document } = testDOM();
    const render = mountConcurrencyView(document, { postMessage() {} });
    render({ revision: 1, activeDebugSessionId: "", sessions: [] });
    assert.match(document.body.textContent, /Start a bingo debug session/);
    render(model({ error: "socket rejected", snapshot: undefined }));
    assert.equal(document.querySelector(".callout.error")?.getAttribute("role"), "alert");
    assert.match(document.body.textContent, /socket rejected/);
  });

  it("keeps hostile tracee strings inert", () => {
    const { document } = testDOM();
    const hostile = '<img src=x onerror="globalThis.pwned=true">';
    const snap = snapshot([goroutine(1, 0, { waitReason: hostile, current: true })]);
    mountConcurrencyView(document, { postMessage() {} })(
      model({ snapshot: snap, selectedGoroutine: 1 }),
    );
    assert.equal(document.querySelector("img"), null);
    assert.match(document.body.textContent, /<img src=x/);
  });
});
