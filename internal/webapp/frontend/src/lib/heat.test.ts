// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Excluded from tsconfig's include — it imports node: builtins, which the
// app's DOM-only lib set does not know about.
import { test } from "node:test";
import assert from "node:assert/strict";
import { heatFor, heatLevel, heatText, heatTotal, hotPathSplit } from "./heat.ts";
import { ageRange, ageSpanLabel, isFlatRange, FLAT_AGE_SPREAD, orphanPaths } from "./heat.ts";
import { placeLabels, LABEL_MAX } from "./heat.ts";
import type { HeatMap } from "../api/types.ts";

// One fixture, read by both surfaces: the file header (heatText/heatTotal) and
// the Dashboard hot-path bar (heatTotal/hotPathSplit).
const FIXTURE: HeatMap = {
  "guide.md": { human: 6, agent: 9, share: 1 },
  "notes/shared-only.md": { share: 4 },
  "notes/read-by-people.md": { human: 3 },
  "notes/untouched.md": {},
};

test("heatTotal is human + agent + share", () => {
  assert.equal(heatTotal(FIXTURE["guide.md"]), 16);
  assert.equal(heatTotal(FIXTURE["notes/shared-only.md"]), 4);
  assert.equal(heatTotal(FIXTURE["notes/untouched.md"]), 0);
});

test("heatText breaks out the same three numbers the total sums", () => {
  assert.equal(heatText(FIXTURE["guide.md"]), "16 reads (6 human, 9 agent, 1 shared)");
  assert.equal(heatText(FIXTURE["notes/shared-only.md"]), "4 reads (4 shared)");
  // People-only needs no breakout — the total is already unambiguous.
  assert.equal(heatText(FIXTURE["notes/read-by-people.md"]), "3 reads");
  assert.equal(heatText(FIXTURE["notes/untouched.md"]), "");
});

test("the file header and the hot-path bar cannot disagree about a total", () => {
  for (const [path, e] of Object.entries(FIXTURE)) {
    const total = heatTotal(e); // what the Dashboard row prints (Insights: p.total/p.reads)
    const header = heatText(e); // what the file header prints (FileView)
    if (!total) {
      assert.equal(header, "", path);
      continue;
    }
    assert.equal(Number(header.split(" ")[0]), total, path);
    // …and the bar's segments recompose to that same total, not to some
    // second sum computed at render time.
    const f = hotPathSplit(e);
    assert.equal(
      Math.round((f.agent + f.human + f.share) * total),
      total,
      path,
    );
  }
});

test("hotPathSplit gives each reader its own fraction, summing to 1", () => {
  const f = hotPathSplit(FIXTURE["guide.md"]);
  assert.equal(f.agent + f.human + f.share, 1);
  assert.equal(f.agent, 9 / 16);
  assert.equal(f.human, 6 / 16);
  assert.equal(f.share, 1 / 16);
});

test("share reads never paint as human", () => {
  const f = hotPathSplit(FIXTURE["notes/shared-only.md"]);
  assert.equal(f.human, 0); // the bug: 1 - agentFraction painted this 100% human
  assert.equal(f.share, 1);
  assert.equal(f.agent, 0);
});

test("an unread path splits to nothing rather than dividing by zero", () => {
  assert.deepEqual(hotPathSplit(FIXTURE["notes/untouched.md"]), {
    agent: 0,
    human: 0,
    share: 0,
  });
});

test("heatFor sums a folder subtree across all three readers", () => {
  const notes = heatFor(FIXTURE, "notes", true);
  assert.deepEqual(notes, { human: 3, agent: 0, share: 4 });
  assert.equal(heatTotal(notes!), 7);
  assert.equal(heatFor(FIXTURE, "guide.md", false), FIXTURE["guide.md"]);
  assert.equal(heatFor(null, "notes", true), null);
  assert.equal(heatFor({ "x/unread.md": {} }, "x", true), null);
});

test("heatLevel steps at 3, 10 and 30 reads", () => {
  assert.equal(heatLevel({}), 0);
  assert.equal(heatLevel({ human: 2 }), 1);
  assert.equal(heatLevel({ human: 1, agent: 1, share: 1 }), 2);
  assert.equal(heatLevel({ agent: 9 }), 2);
  assert.equal(heatLevel({ agent: 10 }), 3);
  assert.equal(heatLevel({ human: 15, share: 15 }), 4);
});

test("ageRange: empty scope has no range", () => {
  assert.equal(ageRange([]), null);
});

test("ageRange: min and max, unsorted input", () => {
  assert.deepEqual(ageRange([4, 0.5, 12, 2]), { min: 0.5, max: 12 });
  assert.deepEqual(ageRange([7]), { min: 7, max: 7 });
});

test("ageRange survives more files than the spread operator would take", () => {
  const days = Array.from({ length: 200_000 }, (_, i) => i % 97);
  assert.deepEqual(ageRange(days), { min: 0, max: 96 });
});

test("isFlatRange: the young-project case the legend must admit to", () => {
  assert.equal(isFlatRange(0, 3), true); // the repro: all content ≤3d old
  assert.equal(isFlatRange(0, 0), true);
  assert.equal(isFlatRange(0, FLAT_AGE_SPREAD), false); // boundary is exclusive
  assert.equal(isFlatRange(0, 140), false);
  assert.equal(isFlatRange(200, 203), true); // uniformly stale is flat too
});

test("ageSpanLabel: rounds, and collapses when both ends round alike", () => {
  assert.equal(ageSpanLabel(0.2, 2.8), "0–3d");
  assert.equal(ageSpanLabel(1.9, 2.1), "2d");
  assert.equal(ageSpanLabel(0, 0), "0d");
});

test("orphanPaths: heat rows whose file left the tree, sorted", () => {
  const known = new Set(["guide.md", "notes/read-by-people.md", "notes/untouched.md"]);
  // "notes/shared-only.md" was deleted; its reads must not vanish with it.
  assert.deepEqual(orphanPaths(FIXTURE, known), ["notes/shared-only.md"]);
  assert.deepEqual(orphanPaths(FIXTURE, new Set(Object.keys(FIXTURE))), []);
  assert.deepEqual(orphanPaths({}, known), []);
  assert.deepEqual(orphanPaths(null, known), []);
});

/* ---- placeLabels: the danger dots' basenames (BEA-60) ---- */

const BOUNDS = { right: 704, top: 28, bottom: 322 }; // W - M.r, M.t + 8, H - M.b - 4
const dot = (path: string, reads: number, cx: number, cy: number, r = 3) => ({
  path,
  reads,
  cx,
  cy,
  r,
});

test("placeLabels: basenames, busiest first, capped at six", () => {
  const dots = Array.from({ length: 9 }, (_, i) => dot(`archive/f${i}.md`, i, 400, 40 + i * 40));
  const out = placeLabels(dots, BOUNDS);
  assert.equal(out.length, LABEL_MAX);
  assert.deepEqual(
    out.map((l) => l.name),
    ["f8.md", "f7.md", "f6.md", "f5.md", "f4.md", "f3.md"],
  );
  assert.deepEqual(placeLabels([], BOUNDS), []);
});

test("placeLabels: sits right of the dot, flips left rather than leave the frame", () => {
  const [near] = placeLabels([dot("a/near-edge.md", 5, 400, 100, 7)], BOUNDS);
  assert.equal(near.anchor, "start");
  assert.equal(near.x, 411); // cx + r + 4
  const [far] = placeLabels([dot("a/a-very-long-filename.md", 5, 690, 100, 7)], BOUNDS);
  assert.equal(far.anchor, "end");
  assert.equal(far.x, 679); // cx - r - 4, text runs back into the frame
});

test("placeLabels: colliding labels stack instead of overprinting", () => {
  // Three dots within a couple of px of each other: the naive placement puts
  // all three basenames on the same baseline.
  const out = placeLabels(
    [dot("a/one.md", 9, 500, 100), dot("a/two.md", 8, 505, 101), dot("a/three.md", 7, 510, 99)],
    BOUNDS,
  );
  const ys = out.map((l) => l.y).sort((a, b) => a - b);
  for (let i = 1; i < ys.length; i++) assert.ok(ys[i] - ys[i - 1] >= 11, `overlap: ${ys}`);
  assert.ok(ys.every((y) => y >= BOUNDS.top && y <= BOUNDS.bottom));
});

test("placeLabels: stacks upward when downward would leave the frame", () => {
  const out = placeLabels(
    Array.from({ length: 5 }, (_, i) => dot(`a/f${i}.md`, 9 - i, 500, BOUNDS.bottom - 1)),
    BOUNDS,
  );
  const ys = out.map((l) => l.y).sort((a, b) => a - b);
  assert.ok(ys.every((y) => y >= BOUNDS.top && y <= BOUNDS.bottom), `out of frame: ${ys}`);
  for (let i = 1; i < ys.length; i++) assert.ok(ys[i] - ys[i - 1] >= 11, `overlap: ${ys}`);
});
