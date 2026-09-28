// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
import { test } from "node:test";
import assert from "node:assert/strict";
import { READ_MIN, clampReadWidth } from "./readwidth.ts";

test("clampReadWidth keeps a width inside [READ_MIN, room]", () => {
  assert.equal(clampReadWidth(1000, 1400), 1000);
  assert.equal(clampReadWidth(200, 1400), READ_MIN);
  assert.equal(clampReadWidth(3000, 1400), 1400);
});

test("clampReadWidth rounds to whole pixels", () => {
  assert.equal(clampReadWidth(900.6, 1400), 901);
});

test("a window narrower than READ_MIN still yields READ_MIN, never less", () => {
  // The column is width:100% under its max-width, so asking for more than the
  // room is harmless; asking for less than the floor would make the drag lie.
  assert.equal(clampReadWidth(700, 400), READ_MIN);
});
