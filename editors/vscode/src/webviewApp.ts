import type {
  ConcurrencyViewModel,
  SessionViewModel,
} from "./model.js";
import { filterFullTree, type TreeNode } from "./tree.js";

export interface WebviewHost {
  postMessage(message: Record<string, unknown>): void;
}

const svgNamespace = "http://www.w3.org/2000/svg";
const focusSelectors = {
  "session-selector": '[data-focus="session-selector"]',
  refresh: '[data-focus="refresh"]',
  "copy-snapshot": '[data-focus="copy-snapshot"]',
  fit: '[data-focus="fit"]',
  "zoom-out": '[data-focus="zoom-out"]',
  "zoom-in": '[data-focus="zoom-in"]',
} as const;
const focusIDSelectors = {
  "bingo-goroutine-filter": "#bingo-goroutine-filter",
  "concurrency-tree": "#concurrency-tree",
} as const;

type FocusToken = keyof typeof focusSelectors;
type FocusID = keyof typeof focusIDSelectors;
type FocusIdentity =
  | { readonly kind: "control"; readonly token: FocusToken }
  | { readonly kind: "id"; readonly id: FocusID }
  | { readonly kind: "goroutine"; readonly id: number };

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
    const documentWasFocused = document.hasFocus();
    const focused = documentWasFocused
      ? focusIdentity(document.activeElement)
      : undefined;
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
    if (documentWasFocused) {
      restoreFocus(root, focused);
    }
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
    selector.dataset.focus = "session-selector";
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
        session.connection === "connected"
          ? "Waiting for a suspended snapshot"
          : "Connecting to telemetry",
        "Snapshots arrive at entry, breakpoints, pauses, and explicit refreshes.",
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
  const refresh = button(document, "Refresh", "refresh", () => {
    host.postMessage({ type: "refresh" });
  });
  const copy = button(document, "Copy snapshot", "copy-snapshot", () => {
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
  const values = [
    ["Goroutines", session.snapshot?.goroutines.length ?? 0],
    ["Threads", session.snapshot?.threads.length ?? 0],
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
  const fit = button(document, "Fit", "fit", () => {
    currentTransform = { x: 20, y: 20, scale: 1 };
    state.setTransform(currentTransform);
    updateTransform();
  });
  const zoomIn = button(document, "+", "zoom-in", () => zoom(1.15));
  zoomIn.setAttribute("aria-label", "Zoom in");
  const zoomOut = button(document, "−", "zoom-out", () => zoom(0.85));
  zoomOut.setAttribute("aria-label", "Zoom out");
  controls.append(fit, zoomOut, zoomIn);
  panel.append(controls);

  if (session.tree.nodes.length === 0) {
    panel.append(
      emptyState(
        document,
        "No goroutines in this snapshot",
        "The target may be exiting or runtime inspection degraded.",
      ),
    );
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
    const visible = session.tree.nodes;
    const current = visible.findIndex(
      (node) => node.goroutine.id === session.selectedGoroutine,
    );
    const direction = event.key === "ArrowDown" || event.key === "ArrowRight" ? 1 : -1;
    // A filtered-out selection leaves current at -1, which is not an index one
    // step before the list: modulo arithmetic on it lands on len-2 in reverse
    // and skips the last visible node. Enter the list from the matching end.
    const next =
      current < 0
        ? visible[direction === 1 ? 0 : visible.length - 1]
        : visible[(current + direction + visible.length) % visible.length];
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
  if (list.childElementCount === 0) {
    list.append(emptyState(document, "No thread data", "Runtime thread inspection was unavailable."));
  }
  panel.append(list);
  return panel;
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
  focusToken: FocusToken,
  action: () => void,
): HTMLButtonElement {
  const element = document.createElement("button");
  element.type = "button";
  element.textContent = label;
  element.dataset.focus = focusToken;
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

function focusIdentity(
  element: Element | null | undefined,
): FocusIdentity | undefined {
  if (element === null || element === undefined) {
    return undefined;
  }
  const token = element.getAttribute("data-focus");
  if (token !== null && isFocusToken(token)) {
    return { kind: "control", token };
  }
  if (isFocusID(element.id)) {
    return { kind: "id", id: element.id };
  }
  const goid = element.getAttribute("data-goid");
  if (goid !== null) {
    const id = Number(goid);
    if (Number.isSafeInteger(id) && id > 0) {
      return { kind: "goroutine", id };
    }
  }
  return undefined;
}

function restoreFocus(
  root: HTMLElement,
  identity: FocusIdentity | undefined,
): void {
  if (identity === undefined) {
    return;
  }
  const selector =
    identity.kind === "control"
      ? focusSelectors[identity.token]
      : identity.kind === "id"
        ? focusIDSelectors[identity.id]
        : `[data-goid="${String(identity.id)}"]`;
  root.querySelector<HTMLElement | SVGElement>(selector)?.focus();
}

function isFocusToken(value: string): value is FocusToken {
  return Object.prototype.hasOwnProperty.call(focusSelectors, value);
}

function isFocusID(value: string): value is FocusID {
  return Object.prototype.hasOwnProperty.call(focusIDSelectors, value);
}
