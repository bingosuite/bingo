import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { parseHTML } from "linkedom";

import type {
  ConcurrencyViewModel,
  DebugInspection,
  SessionModel,
} from "../src/model.js";
import { emptyInspection, toSessionViewModel } from "../src/model.js";
import { mountConcurrencyView } from "../src/webviewApp.js";
import { decodeAction } from "../src/messages.js";
import type { SourceSnippet } from "../src/sourceModel.js";
import { goroutine, snapshot, thread } from "./fixtures.js";

function content(message: Record<string, unknown>): Record<string, unknown> {
  return message.type === "action" ? decodeAction(message).action : message;
}

function model(
  patch: Partial<SessionModel> = {},
  inspection?: DebugInspection,
  spawnSource?: SourceSnippet,
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
    sessions: [
      toSessionViewModel(
        session,
        inspection ?? emptyInspection(session.selectedGoroutine),
        spawnSource,
      ),
    ],
  };
}

describe("concurrency webview DOM", () => {
  it("renders nodes, edges, threads, inspector, lifecycle, and accessibility", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const messages: Record<string, unknown>[] = [];
    const render = mountConcurrencyView(document, {
      postMessage(message) {
        messages.push(content(message));
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

  it("keeps ready-state inspection limits visible even when no variable fits", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const render = mountConcurrencyView(document, { postMessage() {} });
    for (const variables of [[], [{
      name: "value", value: "42", type: "int", variablesReference: 0,
    }]]) {
      render(model({}, {
        ...emptyInspection(1),
        stackStatus: "ready", stackMessage: "Stack truncated at 200 frames.",
        localsStatus: "ready", localsMessage: "Variable inspection budget exhausted.",
        variables,
      }));
      const inspector = document.querySelector(".debug-inspection")?.textContent ??
        document.body.textContent ?? "";
      assert.match(inspector, /Stack truncated at 200 frames/);
      assert.match(inspector, /Variable inspection budget exhausted/);
    }
  });

  it("filters and selects through DOM events", () => {
    const { document, window } = parseHTML("<html><body><div id=app></div></body></html>");
    const messages: Record<string, unknown>[] = [];
    mountConcurrencyView(document, { postMessage: (message) => messages.push(content(message)) })(model());
    const search = document.querySelector<HTMLInputElement>('input[type="search"]')!;
    search.value = "running";
    search.dispatchEvent(new window.Event("input"));
    assert.equal(document.querySelectorAll(".tree-node.filtered").length, 0);
    document.querySelectorAll<SVGGElement>(".tree-node")[1]?.dispatchEvent(new window.Event("click"));
    assert.deepEqual(messages.at(-1), { type: "selectGoroutine", id: 2 });
  });

  it("renders interactive stack frames, locals, expansion, and source navigation", () => {
    const { document } = parseHTML(
      "<html><body><div id=app></div></body></html>",
    );
    const messages: Record<string, unknown>[] = [];
    const inspection: DebugInspection = {
      ...emptyInspection(1),
      stackStatus: "ready",
      frames: [
        {
          id: 1,
          name: "main.worker",
          file: "/workspace/main.go",
          line: 42,
          column: 3,
        },
        {
          id: 2,
          name: "main.main",
          file: "/workspace/main.go",
          line: 12,
          column: 1,
        },
      ],
      selectedFrameId: 1,
      localsStatus: "ready",
      variables: [
        {
          name: "jobs",
          value: "[]string len: 2, cap: 2",
          type: "[]string",
          variablesReference: 65_536,
        },
      ],
    };
    mountConcurrencyView(document, {
      postMessage: (message) => messages.push(content(message)),
    })(model({}, inspection));

    assert.equal(document.querySelectorAll(".stack-frame").length, 2);
    assert.match(
      document.querySelector(".debug-inspection")?.textContent ?? "",
      /main\.worker/,
    );
    assert.match(
      document.querySelector(".variable-tree")?.textContent ?? "",
      /jobs/,
    );

    document
      .querySelectorAll<HTMLButtonElement>(".frame-name")[1]
      ?.click();
    assert.deepEqual(messages.at(-1), { type: "selectFrame", id: 2 });

    document
      .querySelector<HTMLButtonElement>(".variable-expand")
      ?.click();
    assert.deepEqual(messages.at(-1), {
      type: "expandVariable",
      reference: 65_536,
    });

    document
      .querySelector<HTMLButtonElement>(".stack-frame .source-link")
      ?.click();
    assert.deepEqual(messages.at(-1), {
      type: "openSource",
      target: "frame",
      frameId: 1,
    });
  });

  it("explains degraded concurrency data without hiding debugger inspection", () => {
    const { document } = parseHTML(
      "<html><body><div id=app></div></body></html>",
    );
    const degraded = snapshot(
      [goroutine(0, 0, { current: true, status: "unknown" })],
      [],
    );
    const inspection: DebugInspection = {
      ...emptyInspection(0),
      stackStatus: "ready",
      frames: [
        {
          id: 1,
          name: "runtime.rt0_go",
          file: "/workspace/main.go",
          line: 1,
          column: 1,
        },
      ],
      selectedFrameId: 1,
      localsStatus: "ready",
      variables: [],
    };
    mountConcurrencyView(document, { postMessage() {} })(
      model(
        {
          snapshot: degraded,
          selectedGoroutine: 0,
        },
        inspection,
      ),
    );

    assert.match(document.body.textContent, /common before runtime initialization/);
    assert.match(document.body.textContent, /runtime\.rt0_go/);
    assert.match(
      document.body.textContent,
      /did not identify the stopped goroutine/,
    );
    assert.doesNotMatch(
      document.body.textContent,
      /DWARF or runtime data was unavailable/,
    );
  });

  it("moves DOM focus with keyboard tree selection", () => {
    const { document, window } = parseHTML(
      "<html><body><div id=app></div></body></html>",
    );
    const messages: Record<string, unknown>[] = [];
    mountConcurrencyView(document, {
      postMessage: (message) => messages.push(content(message)),
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

  // `connection === "error"` is reached by a protocol latch AND by an exhausted
  // reconnect ladder, which an ordinary stopped server also produces. The old
  // copy diagnosed only incompatibility, so the far more common "the server went
  // away" case sent people auditing versions instead of restarting bingo.
  it("does not blame incompatibility for an exhausted reconnect with no snapshot", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const render = mountConcurrencyView(document, { postMessage() {} });

    render(
      model({
        connection: "error",
        snapshot: undefined,
        error: "reconnect attempts exhausted",
      }),
    );

    const text = document.body.textContent;
    assert.match(text, /Telemetry disconnected/);
    assert.match(text, /Refresh retries the connection/);
    assert.match(text, /server is still running and compatible/);
    assert.doesNotMatch(text, /probably not compatible/);
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

describe("creation source and bounded inspection DOM", () => {
  it("renders the selected non-stopped node's parent and full creation/start identities", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const messages: Record<string, unknown>[] = [];
    const created = {
      file: "/workspace/supervisor.go",
      line: 12,
      function: "main.supervise",
    };
    const view = model({
      selectedGoroutine: 2,
      snapshot: snapshot([
        goroutine(1, 0, { current: true }),
        goroutine(2, 1, {
          createdLoc: created,
          startLoc: { file: "/workspace/worker.go", line: 30, function: "main.supervise.gowrap1" },
        }),
      ]),
    }, {
      ...emptyInspection(2),
      stackStatus: "unavailable",
      stackMessage: "Only the stopped goroutine's stack is available.",
    }, {
      status: "ready",
      message: "Local file on disk, not verified against the binary.",
      lines: [
        { number: 11, text: "// create worker", highlighted: false },
        { number: 12, text: "go worker(job)", highlighted: true },
        { number: 13, text: "wait()", highlighted: false },
      ],
    });
    mountConcurrencyView(document, { postMessage: (message) => messages.push(message) })(view, 7);
    const inspector = document.querySelector(".inspector")!;
    assert.match(inspector.textContent ?? "", /Parentg1/);
    assert.match(inspector.textContent ?? "", /main.supervise.*\/workspace\/supervisor.go:12/);
    assert.match(inspector.textContent ?? "", /main.supervise.gowrap1.*worker.go:30/);
    assert.match(inspector.textContent ?? "", /compiler-generated wrapper/);
    assert.match(inspector.textContent ?? "", /Only the stopped goroutine/);
    assert.equal(document.querySelectorAll(".creation-line").length, 1);
    assert.equal(document.querySelector(".creation-line")?.textContent, "12  go worker(job)\n");
    assert.equal(document.querySelector(".creation-line")?.getAttribute("aria-current"), "location");
    document.querySelector<HTMLButtonElement>(".spawn-source .source-link")!.click();
    assert.deepEqual(decodeAction(messages.at(-1)), {
      context: { generation: 7, revision: 1, debugSessionId: "debug", goroutineId: 2 },
      action: { type: "openSource", target: "created", frameId: 0 },
    });
  });

  for (const status of ["idle", "loading", "unavailable"] as const) {
    it(`shows an honest ${status} source state without invented snippet`, () => {
      const { document } = parseHTML("<html><body><div id=app></div></body></html>");
      mountConcurrencyView(document, { postMessage() {} })(model({}, undefined, {
        status,
        message: `${status}: source not read`,
        lines: [],
      }));
      assert.match(document.querySelector(".spawn-source")?.textContent ?? "", /source not read/);
      assert.equal(document.querySelectorAll(".source-snippet").length, 0);
    });
  }

  it("keeps hostile source, function names and variable values inert", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const hostile = '<script>alert(1)</script><img src=x onerror="alert(1)">';
    mountConcurrencyView(document, { postMessage() {} })(model({
      snapshot: snapshot([goroutine(1, 0, {
        current: true,
        createdLoc: { file: "command:workbench.action.closeWindow", line: 1, function: hostile },
      })]),
    }, {
      ...emptyInspection(1),
      stackStatus: "ready",
      localsStatus: "ready",
      variables: [{ name: hostile, value: "javascript:alert(1)", type: hostile, variablesReference: 0 }],
    }, {
      status: "ready",
      message: "Not verified against the binary.",
      lines: [{ number: 1, text: hostile, highlighted: true }],
    }));
    assert.equal(document.querySelectorAll("script,img,a,iframe").length, 0);
    assert.ok(document.querySelector(".creation-line")?.textContent?.includes(hostile));
    assert.match(document.querySelector(".variable-value")?.textContent ?? "", /javascript:alert/);
  });

  it("retains old document action provenance even when numeric IDs are reused", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const messages: Record<string, unknown>[] = [];
    const render = mountConcurrencyView(document, { postMessage: (message) => messages.push(message) });
    render(model(), 1);
    const old = document.querySelector<HTMLButtonElement>(".spawn-source .source-link")!;
    const replacement = model();
    render({
      ...replacement,
      revision: 2,
      activeDebugSessionId: "replacement",
      sessions: replacement.sessions.map((session) => ({ ...session, debugSessionId: "replacement" })),
    }, 3);
    old.click();
    assert.deepEqual(decodeAction(messages.at(-1)).context,
      { generation: 1, revision: 1, debugSessionId: "debug", goroutineId: 1 });
  });

  for (const count of [999, 1000, 1001, 10_000]) {
    it(`bounds ${String(count)} root variables to 1000 displayed nodes`, () => {
      const { document } = parseHTML("<html><body><div id=app></div></body></html>");
      mountConcurrencyView(document, { postMessage() {} })(model({}, {
        ...emptyInspection(1), stackStatus: "ready", localsStatus: "ready",
        variables: Array.from({ length: count }, (_, index) => ({
          name: `v${String(index)}`, value: "1", type: "int", variablesReference: 0,
        })),
      }));
      assert.equal(document.querySelectorAll(".variable").length, Math.min(count, 1000));
      assert.equal(document.querySelector(".variable-tree")?.textContent?.includes("limit reached"), count > 1000);
    });
  }

  it("renders a cyclic reference once and reports the cycle", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const variable = { name: "self", value: "*Node", type: "Node", variablesReference: 1 };
    mountConcurrencyView(document, { postMessage() {} })(model({}, {
      ...emptyInspection(1), stackStatus: "ready", localsStatus: "ready",
      variables: [variable], variablesByReference: { "1": [variable] },
    }));
    assert.equal(document.querySelectorAll(".variable").length, 2);
    assert.match(document.querySelector(".variable-tree")?.textContent ?? "", /Circular reference/);
  });

  it("does not multiply a shared subtree across a wide alias fanout", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const aliases = Array.from({ length: 500 }, (_, index) => ({
      name: `alias${String(index)}`, value: "*Node", type: "Node", variablesReference: 1,
    }));
    mountConcurrencyView(document, { postMessage() {} })(model({}, {
      ...emptyInspection(1), stackStatus: "ready", localsStatus: "ready",
      variables: aliases,
      variablesByReference: { "1": [{ name: "unique-leaf", value: "1", type: "int", variablesReference: 0 }] },
    }));
    assert.equal(document.querySelectorAll(".variable").length, 501);
    assert.equal(document.querySelector(".variable-tree")?.textContent?.split("unique-leaf").length, 2);
    assert.match(document.querySelector(".variable-tree")?.textContent ?? "", /Shared reference/);
  });

  it("stops rendering a 10000-reference chain at depth20", () => {
    const { document } = parseHTML("<html><body><div id=app></div></body></html>");
    const variables = Array.from({ length: 10_000 }, (_, index) => ({
      name: `depth${String(index)}`, value: "*Node", type: "Node", variablesReference: index + 1,
    }));
    mountConcurrencyView(document, { postMessage() {} })(model({}, {
      ...emptyInspection(1), stackStatus: "ready", localsStatus: "ready",
      variables: variables.slice(0, 1),
      variablesByReference: Object.fromEntries(variables.slice(1).map((variable, index) => [String(index + 1), [variable]])),
    }));
    assert.equal(document.querySelectorAll(".variable").length, 21);
    assert.match(document.querySelector(".variable-tree")?.textContent ?? "", /depth or node limit/);
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
