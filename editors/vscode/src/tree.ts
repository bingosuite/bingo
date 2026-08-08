import type { Goroutine } from "./telemetry.js";

export const maximumRenderedGoroutines = 500;

export interface TreeNode {
  readonly goroutine: Goroutine;
  readonly parentId: number;
  readonly depth: number;
  readonly order: number;
  readonly x: number;
  readonly y: number;
}

export interface TreeEdge {
  readonly from: number;
  readonly to: number;
}

export interface TreeLayout {
  readonly nodes: readonly TreeNode[];
  readonly edges: readonly TreeEdge[];
  readonly omitted: number;
  readonly width: number;
  readonly height: number;
}

export function layoutSpawnTree(
  goroutines: readonly Goroutine[],
  cap = maximumRenderedGoroutines,
  preferredIDs: readonly number[] = [],
): TreeLayout {
  const limit = Math.max(1, Math.min(maximumRenderedGoroutines, Math.trunc(cap)));
  const unique = uniqueGoroutines(goroutines);
  const selected = selectGoroutines(unique, preferredIDs, limit);
  const selectedIDs = new Set(selected.map((item) => item.id));
  const parent = parentTable(selected, selectedIDs);
  breakCycles(parent);
  const children = childrenTable(selected, parent);

  const nodes: TreeNode[] = [];
  const edges: TreeEdge[] = [];
  const visited = new Set<number>();
  let row = 0;
  const visit = (id: number, depth: number): void => {
    const goroutine = unique.get(id);
    if (goroutine === undefined || visited.has(id)) {
      return;
    }
    visited.add(id);
    const parentId = parent.get(id) ?? 0;
    nodes.push({
      goroutine,
      parentId,
      depth,
      order: row,
      x: 42 + depth * 208,
      y: 42 + row * 82,
    });
    row += 1;
    if (parentId !== 0) {
      edges.push({ from: parentId, to: id });
    }
    for (const child of children.get(id) ?? []) {
      visit(child, depth + 1);
    }
  };
  for (const root of children.get(0) ?? []) {
    visit(root, 0);
  }
  for (const item of selected) {
    visit(item.id, 0);
  }

  const maxDepth = nodes.reduce(
    (result, node) => Math.max(result, node.depth),
    0,
  );
  return {
    nodes,
    edges,
    omitted: Math.max(0, unique.size - selected.length),
    width: Math.max(420, 310 + maxDepth * 208),
    height: Math.max(240, 86 + nodes.length * 82),
  };
}

function uniqueGoroutines(
  goroutines: readonly Goroutine[],
): Map<number, Goroutine> {
  const unique = new Map<number, Goroutine>();
  for (const goroutine of [...goroutines].sort(compareGoroutines)) {
    if (!unique.has(goroutine.id)) {
      unique.set(goroutine.id, goroutine);
    }
  }
  return unique;
}

function selectGoroutines(
  unique: ReadonlyMap<number, Goroutine>,
  preferredIDs: readonly number[],
  limit: number,
): readonly Goroutine[] {
  const selectedIDs = new Set<number>();
  for (const id of preferredIDs) {
    if (unique.has(id) && selectedIDs.size < limit) {
      selectedIDs.add(id);
    }
  }
  for (const item of unique.values()) {
    if (selectedIDs.size >= limit) {
      break;
    }
    selectedIDs.add(item.id);
  }
  return [...selectedIDs]
    .map((id) => unique.get(id))
    .filter((item): item is Goroutine => item !== undefined)
    .sort(compareGoroutines);
}

function parentTable(
  selected: readonly Goroutine[],
  selectedIDs: ReadonlySet<number>,
): Map<number, number> {
  return new Map(
    selected.map((item) => [
      item.id,
      item.parentId !== item.id && selectedIDs.has(item.parentId)
        ? item.parentId
        : 0,
    ]),
  );
}

function childrenTable(
  selected: readonly Goroutine[],
  parent: ReadonlyMap<number, number>,
): Map<number, number[]> {
  const children = new Map<number, number[]>();
  for (const item of selected) {
    const parentID = parent.get(item.id) ?? 0;
    const group = children.get(parentID) ?? [];
    group.push(item.id);
    children.set(parentID, group);
  }
  for (const group of children.values()) {
    group.sort((left, right) => left - right);
  }
  return children;
}

export function filterTree(layout: TreeLayout, query: string): TreeLayout {
  const normalized = query.trim().toLocaleLowerCase();
  if (normalized.length === 0) {
    return layout;
  }
  const byID = new Map(
    layout.nodes.map((node) => [node.goroutine.id, node] as const),
  );
  const visible = new Set<number>();
  for (const node of layout.nodes) {
    if (!searchText(node.goroutine).includes(normalized)) {
      continue;
    }
    let current: TreeNode | undefined = node;
    while (current !== undefined && !visible.has(current.goroutine.id)) {
      visible.add(current.goroutine.id);
      current = byID.get(current.parentId);
    }
  }
  const goroutines = layout.nodes
    .filter((node) => visible.has(node.goroutine.id))
    .map((node) => node.goroutine);
  const filtered = layoutSpawnTree(goroutines, Math.max(1, goroutines.length));
  return {
    ...filtered,
    omitted: layout.omitted + layout.nodes.length - goroutines.length,
  };
}

function searchText(goroutine: Goroutine): string {
  return [
    String(goroutine.id),
    String(goroutine.parentId),
    String(goroutine.threadId),
    goroutine.status,
    goroutine.waitReason,
    locationText(goroutine.currentLoc),
    locationText(goroutine.startLoc),
    locationText(goroutine.createdLoc),
  ]
    .join(" ")
    .toLocaleLowerCase();
}

function locationText(location: Goroutine["currentLoc"]): string {
  return `${location.function} ${location.file} ${String(location.line)}`;
}

function breakCycles(parent: Map<number, number>): void {
  for (const start of parent.keys()) {
    const path: number[] = [];
    const positions = new Map<number, number>();
    let current = start;
    while (current !== 0) {
      const position = positions.get(current);
      if (position !== undefined) {
        const promoted = path
          .slice(position)
          .sort((left, right) => left - right)[0];
        if (promoted !== undefined) {
          parent.set(promoted, 0);
        }
        break;
      }
      positions.set(current, path.length);
      path.push(current);
      current = parent.get(current) ?? 0;
    }
  }
}

function compareGoroutines(left: Goroutine, right: Goroutine): number {
  return left.id - right.id;
}
