import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  filterFullTree,
  filterTree,
  layoutSpawnTree,
  maximumFilterAncestorDepth,
} from "../src/tree.js";
import { goroutine } from "./fixtures.js";

describe("spawn tree", () => {
  it("normalizes missing parents, multiple roots, cycles, and stable ordering", () => {
    const input = [
      goroutine(8, 7),
      goroutine(2, 3),
      goroutine(5, 99),
      goroutine(3, 2),
      goroutine(1),
      goroutine(7, 1),
    ];
    const first = layoutSpawnTree(input);
    const second = layoutSpawnTree([...input].reverse());
    assert.deepEqual(first, second);
    assert.equal(first.nodes.length, input.length);
    assert.equal(new Set(first.nodes.map((node) => node.goroutine.id)).size, input.length);
    assert.ok(first.nodes.some((node) => node.goroutine.id === 5 && node.parentId === 0));
    assert.ok(
      first.nodes.some(
        (node) => (node.goroutine.id === 2 || node.goroutine.id === 3) && node.parentId === 0,
      ),
    );
  });

  it("caps huge inputs and produces deterministic coordinates", () => {
    const input = Array.from({ length: 1000 }, (_, index) =>
      goroutine(index + 1, index),
    );
    const layout = layoutSpawnTree(input, 120);
    assert.equal(layout.nodes.length, 120);
    assert.equal(layout.omitted, 880);
    assert.deepEqual(layout.nodes[0]?.x, 42);
    assert.deepEqual(layout.nodes[0]?.y, 42);
    assert.ok(layout.width > 0);
    assert.ok(layout.height > 0);
  });

  it("retains preferred selections beyond the cap and filters with ancestors", () => {
    const input = Array.from({ length: 20 }, (_, index) =>
      goroutine(index + 1, index, {
        status: index === 19 ? "needle" : "waiting",
      }),
    );
    const layout = layoutSpawnTree(input, 5, [20]);
    assert.ok(layout.nodes.some((node) => node.goroutine.id === 20));
    const full = layoutSpawnTree(input);
    const filtered = filterTree(full, "needle");
    assert.deepEqual(
      filtered.nodes.map((node) => node.goroutine.id),
      [16, 17, 18, 19, 20],
    );
    assert.equal(maximumFilterAncestorDepth, 4);
  });

  it("bounds ancestor context and relayouts a 500-node filtered chain", () => {
    const input = Array.from({ length: 500 }, (_, index) =>
      goroutine(index + 1, index, {
        waitReason: index === 499 ? "needle" : "waiting",
      }),
    );
    const filtered = filterTree(layoutSpawnTree(input), "needle");
    assert.deepEqual(
      filtered.nodes.map((node) => node.goroutine.id),
      [496, 497, 498, 499, 500],
    );
    assert.deepEqual(
      filtered.nodes.map((node) => node.depth),
      [0, 1, 2, 3, 4],
    );
    assert.equal(filtered.width, 1142);
    assert.equal(filtered.height, 496);
  });

  it("finds matches beyond the rendering cap from the full snapshot", () => {
    const input = Array.from({ length: 1000 }, (_, index) =>
      goroutine(index + 1, index, {
        waitReason: index === 999 ? "needle" : "waiting",
      }),
    );
    const capped = layoutSpawnTree(input);
    assert.equal(capped.nodes.some((node) => node.goroutine.id === 1000), false);

    const filtered = filterFullTree(capped, "needle", input);
    assert.deepEqual(
      filtered.nodes.map((node) => node.goroutine.id),
      [996, 997, 998, 999, 1000],
    );
    assert.equal(filtered.nodes.at(-1)?.goroutine.waitReason, "needle");
    assert.equal(filtered.omitted, 995);
  });
});
