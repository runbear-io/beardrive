// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
import { test } from "node:test";
import assert from "node:assert/strict";
import { ageRange, ageSpanLabel, isFlatRange, FLAT_AGE_SPREAD } from "./heat.ts";

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
