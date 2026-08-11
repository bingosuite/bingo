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

  it("moves DOM focus with keyboard tree selection", () => {
    const { document, window } = parseHTML(
      "<html><body><div id=app></div></body></html>",
    );
    const messages: Record<string, unknown>[] = [];
    mountConcurrencyView(document, {
      postMessage: (message) => messages.push(message),
    })(model());
    const first = document.querySelector<SVGGElement>('[data-goid="1"]')!;
    const second = document.querySelector<SVGGElement>('[data-goid="2"]')!;
    let focused = "";
    Object.defineProperty(first, "focus", {
      value: () => {
        focused = "1";
      },
    });
    Object.defineProperty(second, "focus", {
      value: () => {
        focused = "2";
      },
    });
    first.focus();
    const event = new window.Event("keydown", {
      bubbles: true,
      cancelable: true,
    });
    Object.defineProperty(event, "key", { value: "ArrowDown" });
    first.dispatchEvent(event);

    assert.equal(focused, "2");
    assert.deepEqual(messages.at(-1), {
      type: "selectGoroutine",
      id: 2,
    });
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

  it("finds, compacts, and fits matches beyond the rendering cap", () => {
    const { document, window } = parseHTML("<html><body><div id=app></div></body></html>");
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
    const { document, window } = parseHTML(
      "<html><body><div id=app></div></body></html>",
    );
    mountConcurrencyView(document, { postMessage() {} })(model());
    const search = document.querySelector<HTMLInputElement>(
      'input[type="search"]',
    )!;
    search.value = "no-such-goroutine";
    search.dispatchEvent(new window.Event("input"));

    assert.equal(document.querySelectorAll(".tree-node").length, 0);
    assert.match(
      document.querySelector(".graph-panel")?.textContent ?? "",
      /No goroutines match this filter/,
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
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const render = mountConcurrencyView(document, { postMessage() {} });
    render({ revision: 1, activeDebugSessionId: "", sessions: [] });
    assert.match(document.body.textContent, /Start a bingo debug session/);
    render(model({ error: "socket rejected", snapshot: undefined }));
    assert.equal(document.querySelector(".callout.error")?.getAttribute("role"), "alert");
    assert.match(document.body.textContent, /socket rejected/);
  });

  // A panel that says "Connecting" while the observer has permanently stopped is
  // the worst of both worlds: it looks busy, so nobody presses the one control
  // that would bring it back. See issue #194.
  it("names the terminal connection state instead of claiming to be connecting", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const render = mountConcurrencyView(document, { postMessage() {} });

    render(model({ connection: "connecting", snapshot: undefined, error: "" }));
    assert.match(document.body.textContent, /Connecting to telemetry/);

    render(model({ connection: "error", snapshot: undefined, error: "reconnect limit reached" }));
    assert.match(document.body.textContent, /Telemetry disconnected/);
    assert.match(document.body.textContent, /Refresh retries the connection/);
    assert.doesNotMatch(document.body.textContent, /Connecting to telemetry/);
  });

  // An empty tree has two very different causes, and the totals settle which.
  // Blaming the target or a failed runtime read when the debugger reported
  // thousands of live goroutines is exactly the dishonesty this work removes.
  it("attributes an empty tree to omission when the totals prove the data existed", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    mountConcurrencyView(document, { postMessage() {} })(
      model({
        snapshot: {
          ...snapshot([], []),
          totals: {
            goroutines: 41203,
            threads: 64,
            goroutinesClipped: false,
            threadsClipped: false,
          },
        },
      }),
    );
    assert.match(document.body.textContent, /Goroutine data was omitted from this snapshot/);
    assert.match(document.body.textContent, /41203 live goroutines/);
    assert.match(document.body.textContent, /Thread data was omitted from this snapshot/);
    // Reported beside its own data, and exactly once: the same shortfall printed
    // in two places reads as two separate shortfalls.
    assert.equal(
      document.body.textContent.match(/threads were not sent in this event/gu),
      null,
      "an empty thread list explains itself; it must not also carry an omission note",
    );
    assert.doesNotMatch(document.body.textContent, /may be exiting/);
    assert.doesNotMatch(document.body.textContent, /thread inspection was unavailable/i);
  });

  it("still blames the runtime when there were no totals to contradict it", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    mountConcurrencyView(document, { postMessage() {} })(
      model({ snapshot: snapshot([], []) }),
    );
    assert.match(document.body.textContent, /No goroutines in this snapshot/);
    assert.match(document.body.textContent, /Runtime thread inspection was unavailable/);
  });

  // A filter that matched nothing is the user's own doing. Blaming the snapshot
  // limits there sends someone hunting a debugger problem that does not exist.
  it("blames the filter, not the debugger, when a search matches nothing", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    mountConcurrencyView(document, { postMessage() {} })(
      model({
        snapshot: {
          ...snapshot([goroutine(1, 0, { current: true })], []),
          totals: {
            goroutines: 41203,
            threads: 64,
            goroutinesClipped: false,
            threadsClipped: false,
          },
        },
      }),
    );
    const search = document.querySelector("#bingo-goroutine-filter") as HTMLInputElement;
    search.value = "definitely-no-such-goroutine";
    search.dispatchEvent(new (document.defaultView as unknown as { Event: typeof Event }).Event("input"));

    // Scoped to the tree panel: the thread list has its own, legitimate, empty
    // state and must not be mistaken for the tree's attribution.
    const graph = document.querySelector(".graph-panel")?.textContent ?? "";
    assert.match(graph, /No goroutines match this filter/);
    assert.doesNotMatch(graph, /none fit within the snapshot limits/);
  });

  // Zero is a measurement. Before a snapshot arrives nobody has taken one, and
  // "0" reads as "nothing is running" rather than "nothing has arrived".
  it("shows no counts before the first snapshot", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    mountConcurrencyView(document, { postMessage() {} })(
      model({ snapshot: undefined, connection: "connecting" }),
    );
    const cards = [...document.querySelectorAll(".card")].map((card) => ({
      label: card.querySelector("span")?.textContent ?? "",
      value: card.querySelector("strong")?.textContent ?? "",
    }));
    assert.equal(cards.find((c) => c.label === "Goroutines")?.value, "—");
    assert.equal(cards.find((c) => c.label === "Threads")?.value, "—");
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

describe("server-omission rendering", () => {
  // Goes through the real render path, not the formatter, so a card that reads
  // the wrong flag is caught. The debugger's goroutine and thread scans have
  // independent ceilings, so each count must be marked a lower bound only when
  // its OWN scan clipped.
  function cards(
    goroutinesClipped: boolean,
    threadsClipped: boolean,
  ): { readonly goroutines: string; readonly threads: string; readonly notes: string } {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    mountConcurrencyView(document, { postMessage() {} })(
      model({
        snapshot: {
          ...snapshot(
            [goroutine(1, 0, { current: true, threadId: 10 })],
            [thread(10, 1)],
          ),
          totals: {
            goroutines: 8192,
            threads: 2048,
            goroutinesClipped,
            threadsClipped,
          },
        },
      }),
    );
    const values = [...document.querySelectorAll(".card")].map((card) => ({
      label: card.querySelector("span")?.textContent ?? "",
      value: card.querySelector("strong")?.textContent ?? "",
    }));
    return {
      goroutines: values.find((v) => v.label === "Goroutines")?.value ?? "",
      threads: values.find((v) => v.label === "Threads")?.value ?? "",
      notes: [...document.querySelectorAll(".server-omitted")]
        .map((n) => n.textContent ?? "")
        .join(" | "),
    };
  }

  it("still states server omissions when the graph is empty", () => {
    // A fully degraded snapshot delivers nothing. A bare "No goroutines in this
    // snapshot" is taken as the runtime's answer rather than as this event being
    // empty while the debugger reported thousands. The empty state now carries
    // that attribution itself instead of appending a separate note beside a
    // headline that contradicts it, so the assertion is that the panel names the
    // omission AND still reports the clipped total as the floor it is.
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    mountConcurrencyView(document, { postMessage() {} })(
      model({
        snapshot: {
          ...snapshot([], []),
          totals: {
            goroutines: 8192,
            threads: 2048,
            goroutinesClipped: true,
            threadsClipped: true,
          },
        },
      }),
    );

    assert.equal(document.querySelectorAll(".tree-node").length, 0);
    const panel = document.querySelector(".graph-panel")?.textContent ?? "";
    assert.match(
      panel,
      /Goroutine data was omitted from this snapshot/u,
      "an empty graph hid what the server left out",
    );
    assert.match(panel, /at least 8192/u, "a clipped total was presented as exact");
    assert.doesNotMatch(
      panel,
      /No goroutines in this snapshot/u,
      "an omitted snapshot must not read as the runtime's own answer",
    );
  });

  it("still states server omissions when a filter matches nothing", () => {
    const { document, window } = parseHTML(
      "<html><body><div id=app></div></body></html>",
    );
    mountConcurrencyView(document, { postMessage() {} })(
      model({
        snapshot: {
          ...snapshot(
            [goroutine(1, 0, { current: true, threadId: 10 })],
            [thread(10, 1)],
          ),
          totals: {
            goroutines: 8192,
            // Threads are complete on purpose: a thread note in a different
            // panel must not be able to stand in for the goroutine one.
            threads: 1,
            goroutinesClipped: false,
            threadsClipped: false,
          },
        },
      }),
    );
    const search = document.querySelector<HTMLInputElement>(
      'input[type="search"]',
    )!;
    search.value = "no-such-goroutine";
    search.dispatchEvent(new window.Event("input"));

    assert.equal(document.querySelectorAll(".tree-node").length, 0);
    const notes = [...document.querySelectorAll(".graph-panel .server-omitted")]
      .map((n) => n.textContent ?? "");
    assert.equal(
      notes.length,
      1,
      "a filter matching nothing hid what the server left out of the goroutines",
    );
    assert.match(notes[0] ?? "", /8191 goroutines were not sent/u);
    assert.equal(
      document.querySelectorAll(".server-omitted").length,
      1,
      "the goroutine shortfall must be stated exactly once",
    );
  });

  it("does not repeat the shortfall the empty state already stated", () => {
    const { document } = parseHTML(
      "<html><body><div id=app></div></body></html>",
    );
    mountConcurrencyView(document, { postMessage() {} })(
      model({
        snapshot: {
          ...snapshot([], [thread(10)]),
          totals: {
            goroutines: 8192,
            threads: 1,
            goroutinesClipped: false,
            threadsClipped: false,
          },
        },
      }),
    );
    assert.match(
      document.querySelector(".empty-state")?.textContent ?? "",
      /8192 live goroutines/u,
      "the empty state must state the shortfall inline",
    );
    assert.equal(
      document.querySelectorAll(".graph-panel .server-omitted").length,
      0,
      "the inline shortfall must not be repeated as a second note",
    );
  });

  it("marks neither count when neither scan clipped", () => {
    const rendered = cards(false, false);
    assert.equal(rendered.goroutines.endsWith("+"), false);
    assert.equal(rendered.threads.endsWith("+"), false);
    assert.doesNotMatch(rendered.notes, /more may exist/u);
  });

  it("marks only goroutines when only the goroutine scan clipped", () => {
    const rendered = cards(true, false);
    assert.equal(rendered.goroutines.endsWith("+"), true);
    assert.equal(
      rendered.threads.endsWith("+"),
      false,
      "an exact thread count must not be shown as approximate",
    );
    assert.match(rendered.notes, /stopped after finding 8192 goroutines, so more may exist/u);
    assert.doesNotMatch(rendered.notes, /threads, so more may exist/u);
  });

  it("marks only threads when only the thread scan clipped", () => {
    const rendered = cards(false, true);
    assert.equal(
      rendered.goroutines.endsWith("+"),
      false,
      "an exact goroutine count must not be shown as approximate",
    );
    assert.equal(rendered.threads.endsWith("+"), true);
    assert.match(rendered.notes, /stopped after finding 2048 threads, so more may exist/u);
    assert.doesNotMatch(rendered.notes, /goroutines, so more may exist/u);
  });

  it("marks both counts when both scans clipped", () => {
    const rendered = cards(true, true);
    assert.equal(rendered.goroutines.endsWith("+"), true);
    assert.equal(rendered.threads.endsWith("+"), true);
    assert.match(rendered.notes, /stopped after finding 8192 goroutines, so more may exist/u);
    assert.match(rendered.notes, /stopped after finding 2048 threads, so more may exist/u);
  });
});
