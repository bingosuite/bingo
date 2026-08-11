import type {
  ConcurrencyViewModel,
  ConnectionState,
  ServerTotals,
  SessionViewModel,
} from "./model.js";
import { formatServerCount } from "./model.js";
import { filterFullTree, type TreeNode } from "./tree.js";

export interface WebviewHost {
  postMessage(message: Record<string, unknown>): void;
}

const svgNamespace = "http://www.w3.org/2000/svg";

export function mountConcurrencyView(
  document: Document,
  host: WebviewHost,
): (model: ConcurrencyViewModel, generation?: number) => void {
  const root = document.getElementById("app");
  if (root === null) {
    throw new Error("Bingo Concurrency root is missing");
  }
  let query = "";
  let transform = { x: 20, y: 20, scale: 1 };

  return (model, generation = 1) => {
    const focused = focusIdentity(document.activeElement);
    root.replaceChildren();
    root.className = "app";
    root.setAttribute("aria-live", "polite");
    const session =
      model.sessions.find(
        (item) => item.debugSessionId === model.activeDebugSessionId,
      ) ?? model.sessions[0];
    root.append(
      header(document, model, session, host),
      session === undefined
        ? emptyState(
            document,
            "Start a bingo debug session",
            "Press F5 and choose a progressive example. This view joins automatically.",
          )
        : renderSession(document, session, host, {
            query,
            setQuery(value) {
              query = value;
            },
            transform,
            setTransform(value) {
              transform = value;
            },
          }),
    );
    restoreFocus(root, focused);
    host.postMessage({
      type: "rendered",
      generation,
      revision: model.revision,
    });
  };
}

function header(
  document: Document,
  model: ConcurrencyViewModel,
  session: SessionViewModel | undefined,
  host: WebviewHost,
): HTMLElement {
  const element = document.createElement("header");
  element.className = "topbar";
  const brand = document.createElement("div");
  brand.className = "brand";
  const title = document.createElement("strong");
  title.textContent = "Bingo Concurrency";
  const subtitle = document.createElement("span");
  subtitle.textContent =
    session === undefined
      ? "Waiting for a debug session"
      : `${session.connection} · ${session.sessionState} · seq ${String(session.lastSeq)}`;
  brand.append(title, subtitle);
  element.append(brand);
  if (model.sessions.length > 0) {
    const selector = document.createElement("select");
    selector.className = "session-selector";
    selector.setAttribute("aria-label", "Active bingo debug session");
    for (const item of model.sessions) {
      const option = document.createElement("option");
      option.value = item.debugSessionId;
      option.textContent = item.debugSessionName;
      option.selected = item.debugSessionId === session?.debugSessionId;
      selector.append(option);
    }
    selector.addEventListener("change", () => {
      host.postMessage({ type: "selectSession", id: selector.value });
    });
    element.append(selector);
  }
  return element;
}

interface RenderState {
  readonly query: string;
  readonly setQuery: (value: string) => void;
  readonly transform: { readonly x: number; readonly y: number; readonly scale: number };
  readonly setTransform: (value: {
    readonly x: number;
    readonly y: number;
    readonly scale: number;
  }) => void;
}

function renderSession(
  document: Document,
  session: SessionViewModel,
  host: WebviewHost,
  state: RenderState,
): HTMLElement {
  const main = document.createElement("main");
  main.className = "session";
  main.append(summaryCards(document, session));
  if (session.error.length > 0) {
    main.append(callout(document, "error", "Telemetry error", session.error));
  } else if (session.seqGap.length > 0) {
    main.append(callout(document, "warning", "Sequence gap", session.seqGap));
  } else if (session.degraded) {
    main.append(
      callout(
        document,
        "warning",
        "Degraded runtime snapshot",
        "DWARF or runtime data was unavailable; showing the synthetic fallback.",
      ),
    );
  }
  if (session.snapshot === undefined) {
    main.append(
      emptyState(
        document,
        waitingHeadline(session.connection),
        session.connection === "error"
          ? "Refresh retries the connection. If the error above repeats, the server and this extension are probably not compatible."
          : "Snapshots arrive at entry, breakpoints, pauses, and explicit refreshes.",
      ),
    );
    return main;
  }

  const toolbar = document.createElement("div");
  toolbar.className = "toolbar";
  const search = document.createElement("input");
  search.type = "search";
  search.id = "bingo-goroutine-filter";
  search.placeholder = "Filter goroutines";
  search.value = state.query;
  search.setAttribute("aria-label", "Filter goroutines");
  const refresh = button(document, "Refresh", () => {
    host.postMessage({ type: "refresh" });
  });
  const copy = button(document, "Copy snapshot", () => {
    host.postMessage({ type: "copySnapshot" });
  });
  toolbar.append(search, refresh, copy);
  main.append(toolbar);

  const workspace = document.createElement("div");
  workspace.className = "workspace";
  let graph = renderGraph(
    document,
    filteredSession(session, state.query),
    host,
    state,
  );
  const side = renderInspector(document, session);
  workspace.append(graph, side);
  main.append(workspace, renderThreads(document, session), renderTimeline(document, session));

  search.addEventListener("input", () => {
    state.setQuery(search.value);
    const fitted = { x: 20, y: 20, scale: 1 };
    state.setTransform(fitted);
    const replacement = renderGraph(
      document,
      filteredSession(session, search.value),
      host,
      { ...state, query: search.value, transform: fitted },
    );
    graph.replaceWith(replacement);
    graph = replacement;
  });
  return main;
}

function summaryCards(
  document: Document,
  session: SessionViewModel,
): HTMLElement {
  const cards = document.createElement("section");
  cards.className = "cards";
  cards.setAttribute("aria-label", "Concurrency summary");
  const totals = session.serverTotals;
  const shownGoroutines = session.snapshot?.goroutines.length ?? 0;
  const shownThreads = session.snapshot?.threads.length ?? 0;
  // Before the first snapshot there is no count to report. Rendering 0 states a
  // fact about the target that nobody has measured yet, and reads as "nothing is
  // running" rather than "nothing has arrived".
  const pending = session.snapshot === undefined;
  const values = [
    [
      "Goroutines",
      pending
        ? "—"
        : totals === undefined
          ? shownGoroutines
          : formatServerCount(
              shownGoroutines,
              totals.goroutines,
              totals.goroutinesClipped,
            ),
    ],
    // The thread statistic reports the machine's real thread count from the
    // server totals when they are present; the list below shows only what was
    // packed, so the two must not silently disagree. Each count is marked a
    // lower bound only when its OWN scan clipped — the ceilings are independent.
    [
      "Threads",
      pending
        ? "—"
        : totals === undefined
          ? shownThreads
          : formatServerCount(shownThreads, totals.threads, totals.threadsClipped),
    ],
    ["Clients", session.clients],
    ["Current", session.snapshot?.current || "—"],
  ] as const;
  for (const [label, value] of values) {
    const card = document.createElement("div");
    card.className = "card";
    const number = document.createElement("strong");
    number.textContent = String(value);
    const caption = document.createElement("span");
    caption.textContent = label;
    card.append(number, caption);
    cards.append(card);
  }
  const status = document.createElement("div");
  status.className = "last-stop";
  status.textContent =
    session.lastStop.length > 0 ? session.lastStop : "Awaiting first stop";
  cards.append(status);
  return cards;
}

function renderGraph(
  document: Document,
  session: SessionViewModel,
  host: WebviewHost,
  state: RenderState,
): HTMLElement {
  const panel = document.createElement("section");
  panel.className = "graph-panel";
  panel.setAttribute("aria-label", "Goroutine spawn tree");
  let currentTransform = state.transform;
  const scene = document.createElementNS(svgNamespace, "g");
  const controls = document.createElement("div");
  controls.className = "graph-controls";
  const fit = button(document, "Fit", () => {
    currentTransform = { x: 20, y: 20, scale: 1 };
    state.setTransform(currentTransform);
    updateTransform();
  });
  const zoomIn = button(document, "+", () => zoom(1.15));
  zoomIn.setAttribute("aria-label", "Zoom in");
  const zoomOut = button(document, "−", () => zoom(0.85));
  zoomOut.setAttribute("aria-label", "Zoom out");
  controls.append(fit, zoomOut, zoomIn);
  panel.append(controls);

  if (session.tree.nodes.length === 0) {
    panel.append(emptyTreeState(document, session, state.query));
    return panel;
  }

  const viewport = document.createElement("div");
  viewport.className = "graph-viewport";
  viewport.id = "concurrency-tree";
  viewport.tabIndex = 0;
  const svg = document.createElementNS(svgNamespace, "svg");
  svg.setAttribute("viewBox", `0 0 ${String(session.tree.width)} ${String(session.tree.height)}`);
  svg.setAttribute("role", "tree");
  svg.setAttribute("aria-label", "Goroutine spawn hierarchy");
  const activeScene = scene;
  svg.append(activeScene);
  const byID = new Map(
    session.tree.nodes.map((node) => [node.goroutine.id, node]),
  );
  for (const edge of session.tree.edges) {
    const from = byID.get(edge.from);
    const to = byID.get(edge.to);
    if (from === undefined || to === undefined) {
      continue;
    }
    const path = document.createElementNS(svgNamespace, "path");
    const startX = from.x + 150;
    const startY = from.y + 24;
    const endX = to.x;
    const endY = to.y + 24;
    const bend = startX + (endX - startX) / 2;
    path.setAttribute(
      "d",
      `M ${String(startX)} ${String(startY)} C ${String(bend)} ${String(startY)}, ${String(bend)} ${String(endY)}, ${String(endX)} ${String(endY)}`,
    );
    path.setAttribute("class", "tree-edge");
    path.dataset.from = String(edge.from);
    path.dataset.to = String(edge.to);
    activeScene.append(path);
  }
  const siblings = new Map<number, TreeNode[]>();
  for (const node of session.tree.nodes) {
    const group = siblings.get(node.parentId) ?? [];
    group.push(node);
    siblings.set(node.parentId, group);
  }
  for (const node of session.tree.nodes) {
    const group = siblings.get(node.parentId) ?? [];
    activeScene.append(
      renderNode(
        document,
        node,
        session.selectedGoroutine,
        host,
        group.indexOf(node) + 1,
        group.length,
        byID.has(node.goroutine.parentId),
      ),
    );
  }
  if (session.tree.omitted > 0) {
    const note = document.createElement("p");
    note.className = "omitted";
    note.textContent = `${String(session.tree.omitted)} additional goroutines omitted by the filter or visual cap`;
    panel.append(note);
  }
  const serverNote = serverOmissionText(session.serverTotals);
  if (serverNote !== undefined) {
    const note = document.createElement("p");
    note.className = "server-omitted";
    note.textContent = serverNote;
    panel.append(note);
  }
  viewport.append(svg);
  panel.append(viewport);

  let dragging: { readonly x: number; readonly y: number } | undefined;
  viewport.addEventListener("wheel", (event) => {
    event.preventDefault();
    zoom(event.deltaY < 0 ? 1.1 : 0.9);
  });
  viewport.addEventListener("pointerdown", (event) => {
    dragging = pointerPosition(event.clientX, event.clientY);
    viewport.setPointerCapture(event.pointerId);
  });
  viewport.addEventListener("pointermove", (event) => {
    if (dragging === undefined) {
      return;
    }
    const pointer = pointerPosition(event.clientX, event.clientY);
    state.setTransform({
      ...currentTransform,
      x: currentTransform.x + pointer.x - dragging.x,
      y: currentTransform.y + pointer.y - dragging.y,
    });
    currentTransform = {
      ...currentTransform,
      x: currentTransform.x + pointer.x - dragging.x,
      y: currentTransform.y + pointer.y - dragging.y,
    };
    dragging = pointer;
    updateTransform();
  });
  viewport.addEventListener("pointerup", () => {
    dragging = undefined;
  });
  viewport.addEventListener("keydown", (event) => {
    if (!["ArrowDown", "ArrowRight", "ArrowUp", "ArrowLeft"].includes(event.key)) {
      return;
    }
    event.preventDefault();
    const visible = session.tree.nodes.filter((node) => {
      const element = activeScene.querySelector<SVGGElement>(
        `[data-goid="${String(node.goroutine.id)}"]`,
      );
      return element?.classList.contains("filtered") !== true;
    });
    const current = visible.findIndex(
      (node) => node.goroutine.id === session.selectedGoroutine,
    );
    const direction = event.key === "ArrowDown" || event.key === "ArrowRight" ? 1 : -1;
    const next = visible[(current + direction + visible.length) % visible.length];
    if (next !== undefined) {
      activeScene
        .querySelector<SVGGElement>(
          `[data-goid="${String(next.goroutine.id)}"]`,
        )
        ?.focus();
      host.postMessage({ type: "selectGoroutine", id: next.goroutine.id });
    }
  });
  updateTransform();
  return panel;

  function zoom(factor: number): void {
    state.setTransform({
      ...currentTransform,
      scale: Math.min(3, Math.max(0.25, currentTransform.scale * factor)),
    });
    currentTransform = {
      ...currentTransform,
      scale: Math.min(3, Math.max(0.25, currentTransform.scale * factor)),
    };
    updateTransform();
  }

  function updateTransform(): void {
    scene.setAttribute(
      "transform",
      `translate(${String(currentTransform.x)} ${String(currentTransform.y)}) scale(${String(currentTransform.scale)})`,
    );
  }

  function pointerPosition(clientX: number, clientY: number): {
    readonly x: number;
    readonly y: number;
  } {
    const matrix = svg.getScreenCTM();
    if (matrix !== null) {
      const point = new DOMPoint(clientX, clientY).matrixTransform(
        matrix.inverse(),
      );
      return { x: point.x, y: point.y };
    }
    const bounds = svg.getBoundingClientRect();
    const unitsPerPixel = Math.max(
      session.tree.width / Math.max(bounds.width, 1),
      session.tree.height / Math.max(bounds.height, 1),
    );
    return {
      x: (clientX - bounds.left) * unitsPerPixel,
      y: (clientY - bounds.top) * unitsPerPixel,
    };
  }
}

function renderNode(
  document: Document,
  node: TreeNode,
  selected: number,
  host: WebviewHost,
  position: number,
  siblingCount: number,
  reportedParentDisplayed: boolean,
): SVGGElement {
  const group = document.createElementNS(svgNamespace, "g");
  group.setAttribute("transform", `translate(${String(node.x)} ${String(node.y)})`);
  group.setAttribute(
    "class",
    `tree-node${node.goroutine.current ? " current" : ""}${node.goroutine.id === selected ? " selected" : ""}`,
  );
  group.setAttribute("role", "treeitem");
  group.setAttribute(
    "aria-label",
    `${goroutineLabel(node.goroutine)}, ${hierarchyLabel(node, reportedParentDisplayed)}`,
  );
  group.setAttribute("aria-level", String(node.depth + 1));
  group.setAttribute("aria-posinset", String(position));
  group.setAttribute("aria-setsize", String(siblingCount));
  group.setAttribute("aria-selected", String(node.goroutine.id === selected));
  group.dataset.goid = String(node.goroutine.id);
  group.dataset.parent = String(node.parentId);
  group.dataset.search = goroutineLabel(node.goroutine).toLocaleLowerCase();
  group.tabIndex = node.goroutine.id === selected ? 0 : -1;

  const body = document.createElementNS(svgNamespace, "rect");
  body.setAttribute("width", "150");
  body.setAttribute("height", "50");
  body.setAttribute("rx", "11");
  const id = document.createElementNS(svgNamespace, "text");
  id.setAttribute("x", "12");
  id.setAttribute("y", "21");
  id.setAttribute("class", "node-id");
  id.textContent = `g${String(node.goroutine.id)}`;
  const status = document.createElementNS(svgNamespace, "text");
  status.setAttribute("x", "12");
  status.setAttribute("y", "39");
  status.setAttribute("class", "node-status");
  status.textContent = node.goroutine.status;
  group.append(body, id, status);
  if (node.goroutine.threadId > 0) {
    const badge = document.createElementNS(svgNamespace, "text");
    badge.setAttribute("x", "138");
    badge.setAttribute("y", "21");
    badge.setAttribute("text-anchor", "end");
    badge.setAttribute("class", "node-thread");
    badge.textContent = `t${String(node.goroutine.threadId)}`;
    group.append(badge);
  }
  if (node.goroutine.current) {
    const current = document.createElementNS(svgNamespace, "text");
    current.setAttribute("x", "138");
    current.setAttribute("y", "39");
    current.setAttribute("text-anchor", "end");
    current.setAttribute("class", "node-thread");
    current.textContent = "current";
    group.append(current);
  }
  group.addEventListener("click", () => {
    group.focus();
    host.postMessage({ type: "selectGoroutine", id: node.goroutine.id });
  });
  return group;
}

function hierarchyLabel(
  node: TreeNode,
  reportedParentDisplayed: boolean,
): string {
  if (node.parentId !== 0) {
    return `child of goroutine ${String(node.parentId)}`;
  }
  if (
    node.goroutine.parentId > 0 &&
    node.goroutine.parentId !== node.goroutine.id
  ) {
    if (reportedParentDisplayed) {
      return `displayed root after cycle normalization, reported parent goroutine ${String(node.goroutine.parentId)}`;
    }
    return `displayed root, reported parent goroutine ${String(node.goroutine.parentId)} is not displayed`;
  }
  return "root goroutine";
}

function filteredSession(
  session: SessionViewModel,
  query: string,
): SessionViewModel {
  return {
    ...session,
    tree: filterFullTree(
      session.tree,
      query,
      session.snapshot?.goroutines ?? [],
    ),
  };
}

function renderInspector(
  document: Document,
  session: SessionViewModel,
): HTMLElement {
  const panel = document.createElement("aside");
  panel.className = "inspector";
  const heading = document.createElement("h2");
  heading.textContent = "Selected goroutine";
  panel.append(heading);
  const goroutine = session.snapshot?.goroutines.find(
    (item) => item.id === session.selectedGoroutine,
  );
  if (goroutine === undefined) {
    panel.append(emptyState(document, "Nothing selected", "Choose a goroutine node."));
    return panel;
  }
  const title = document.createElement("strong");
  title.className = "inspector-title";
  title.textContent = `g${String(goroutine.id)} · ${goroutine.status}`;
  panel.append(title);
  const list = document.createElement("dl");
  const values: readonly [string, string][] = [
    ["Wait", goroutine.waitReason || "—"],
    ["Thread", goroutine.threadId > 0 ? `t${String(goroutine.threadId)}` : "not scheduled"],
    ["Current", locationText(goroutine.currentLoc)],
    ["Started", locationText(goroutine.startLoc)],
    ["Created", locationText(goroutine.createdLoc)],
  ];
  for (const [term, value] of values) {
    const dt = document.createElement("dt");
    dt.textContent = term;
    const dd = document.createElement("dd");
    dd.textContent = value;
    list.append(dt, dd);
  }
  panel.append(list);
  return panel;
}

function renderThreads(
  document: Document,
  session: SessionViewModel,
): HTMLElement {
  const panel = document.createElement("section");
  panel.className = "threads";
  const heading = document.createElement("h2");
  heading.textContent = "OS threads";
  panel.append(heading);
  const list = document.createElement("div");
  list.className = "thread-list";
  for (const thread of session.snapshot?.threads ?? []) {
    const item = document.createElement("div");
    item.className = `thread${thread.current ? " current" : ""}`;
    const label = document.createElement("strong");
    label.textContent = `t${String(thread.id)}`;
    const detail = document.createElement("span");
    const scheduled =
      thread.goid > 0 ? `g${String(thread.goid)}` : "idle";
    const schedulerState = thread.spinning ? " · spinning" : "";
    detail.textContent = scheduled + schedulerState;
    item.append(label, detail);
    list.append(item);
  }
  const noThreadsDelivered = list.childElementCount === 0;
  if (noThreadsDelivered) {
    // Same distinction as the goroutine tree: totals proving the debugger saw
    // threads means the event dropped them, not that the read failed.
    const totals = session.serverTotals;
    list.append(
      totals !== undefined && totals.threads > 0
        ? emptyState(
            document,
            "Thread data was omitted from this snapshot",
            `The debugger found ${String(totals.threads)}${totals.threadsClipped ? " or more" : ""} OS threads, but none fit within the snapshot limits.`,
          )
        : emptyState(document, "No thread data", "Runtime thread inspection was unavailable."),
    );
  }
  panel.append(list);
  const totals = session.serverTotals;
  // An empty list already carries its own full explanation above, so the note is
  // for a list that HAS entries but is short. Rendering both reads as two
  // separate shortfalls rather than one described twice.
  const threadNote =
    totals === undefined || noThreadsDelivered
      ? undefined
      : omissionNotes("threads", totals.threadsOmitted, totals.threads, totals.threadsClipped);
  if (threadNote !== undefined) {
    const note = document.createElement("p");
    note.className = "server-omitted";
    note.textContent = threadNote;
    panel.append(note);
  }
  return panel;
}

// scannedCount states a total that may itself be a floor. Writing a clipped
// total as a bare number reads as an exact census directly beside a note saying
// it is not one.
function scannedCount(count: number, clipped: boolean): string {
  return clipped ? `at least ${String(count)}` : String(count);
}

// omissionNotes states what the DEBUGGER left out of one collection, in its own
// words — never mixed with this view's filter or render cap. The two causes are
// reported separately because they mean different things: an omission is this
// event being partial, while a clipped scan means even the total is a floor.
//
// Each collection is reported beside its own data (goroutines under the tree,
// threads under the thread list) and therefore exactly once. Reporting threads
// in both places made the same fact read as two separate shortfalls.
function omissionNotes(
  noun: string,
  omitted: number,
  total: number,
  clipped: boolean,
): string | undefined {
  const notes: string[] = [];
  if (omitted > 0) {
    notes.push(
      `${String(omitted)} ${noun} were not sent in this event (of ${scannedCount(total, clipped)} found)`,
    );
  }
  if (clipped) {
    notes.push(
      `the debugger stopped after finding ${String(total)} ${noun}, so more may exist`,
    );
  }
  return notes.length === 0 ? undefined : notes.join(" · ");
}

export function serverOmissionText(
  totals: ServerTotals | undefined,
): string | undefined {
  return totals === undefined
    ? undefined
    : omissionNotes(
        "goroutines",
        totals.goroutinesOmitted,
        totals.goroutines,
        totals.goroutinesClipped,
      );
}

function renderTimeline(
  document: Document,
  session: SessionViewModel,
): HTMLElement {
  const panel = document.createElement("section");
  panel.className = "timeline";
  const heading = document.createElement("h2");
  heading.textContent = "Lifecycle";
  panel.append(heading);
  const list = document.createElement("ol");
  for (const entry of [...session.timeline].reverse()) {
    const item = document.createElement("li");
    item.className = entry.action;
    item.textContent = `${entry.action === "created" ? "+" : "−"} g${String(entry.id)}`;
    list.append(item);
  }
  if (list.childElementCount === 0) {
    const empty = document.createElement("p");
    empty.className = "muted";
    empty.textContent = "Created and exited goroutines will appear here.";
    panel.append(empty);
  } else {
    panel.append(list);
  }
  return panel;
}

// waitingHeadline names the state the view is ACTUALLY in while no snapshot has
// arrived. Saying "Connecting" once the observer has stopped trying is the worst
// of both: the panel looks busy, so nobody presses Refresh — the one action that
// would bring it back.
function waitingHeadline(connection: ConnectionState): string {
  switch (connection) {
    case "connected":
      return "Waiting for a suspended snapshot";
    case "reconnecting":
      return "Reconnecting to telemetry";
    case "error":
      return "Telemetry disconnected";
    case "closed":
      return "Telemetry connection closed";
    default:
      return "Connecting to telemetry";
  }
}

// emptyTreeState attributes an empty tree to the cause the evidence supports.
// A filter that matched nothing is the user's own doing and says so; blaming the
// snapshot limits there would send someone hunting a debugger problem that does
// not exist. Only once the snapshot itself delivered nothing do the totals
// decide between "the event dropped it" and "the runtime could not be read".
function emptyTreeState(
  document: Document,
  session: SessionViewModel,
  query: string,
): HTMLElement {
  if (query.trim().length > 0 && (session.snapshot?.goroutines.length ?? 0) > 0) {
    return emptyState(
      document,
      "No goroutines match this filter",
      "Clear or change the filter to show this snapshot.",
    );
  }
  const totals = session.serverTotals;
  if (totals !== undefined && totals.goroutines > 0) {
    return emptyState(
      document,
      "Goroutine data was omitted from this snapshot",
      `The debugger found ${scannedCount(totals.goroutines, totals.goroutinesClipped)} live goroutines, but none fit within the snapshot limits.`,
    );
  }
  return emptyState(
    document,
    "No goroutines in this snapshot",
    "The target may be exiting or runtime inspection degraded.",
  );
}

function emptyState(
  document: Document,
  titleText: string,
  detailText: string,
): HTMLElement {
  const state = document.createElement("div");
  state.className = "empty-state";
  const title = document.createElement("strong");
  title.textContent = titleText;
  const detail = document.createElement("span");
  detail.textContent = detailText;
  state.append(title, detail);
  return state;
}

function callout(
  document: Document,
  level: "error" | "warning",
  titleText: string,
  detailText: string,
): HTMLElement {
  const item = document.createElement("div");
  item.className = `callout ${level}`;
  item.setAttribute("role", level === "error" ? "alert" : "status");
  const title = document.createElement("strong");
  title.textContent = titleText;
  const detail = document.createElement("span");
  detail.textContent = detailText;
  item.append(title, detail);
  return item;
}

function button(
  document: Document,
  label: string,
  action: () => void,
): HTMLButtonElement {
  const element = document.createElement("button");
  element.type = "button";
  element.textContent = label;
  element.addEventListener("click", action);
  return element;
}

function locationText(location: {
  readonly file: string;
  readonly line: number;
  readonly function: string;
}): string {
  if (location.file.length === 0 && location.function.length === 0) {
    return "unknown";
  }
  let source = location.file;
  if (source.length > 0 && location.line > 0) {
    source += `:${String(location.line)}`;
  }
  return [location.function, source].filter((item) => item.length > 0).join(" · ");
}

function goroutineLabel(goroutine: {
  readonly id: number;
  readonly status: string;
  readonly waitReason: string;
  readonly current: boolean;
  readonly currentLoc: { readonly function: string; readonly file: string };
  readonly startLoc: { readonly function: string; readonly file: string };
  readonly createdLoc: { readonly function: string; readonly file: string };
}): string {
  return [
    `goroutine ${String(goroutine.id)}`,
    goroutine.current ? "current" : "",
    goroutine.status,
    goroutine.waitReason,
    goroutine.currentLoc.function,
    goroutine.currentLoc.file,
    goroutine.startLoc.function,
    goroutine.startLoc.file,
    goroutine.createdLoc.function,
    goroutine.createdLoc.file,
  ]
    .filter((item) => item.length > 0)
    .join(" ");
}

function focusIdentity(element: Element | null | undefined): string {
  if (element === null || element === undefined) {
    return "";
  }
  if (element.id.length > 0) {
    return `#${element.id}`;
  }
  const goid = (element as HTMLElement | SVGElement).dataset.goid;
  if (goid !== undefined) {
    return `[data-goid="${goid}"]`;
  }
  return "";
}

function restoreFocus(root: HTMLElement, identity: string): void {
  if (identity.length > 0) {
    root.querySelector<HTMLElement | SVGElement>(identity)?.focus();
  }
}
