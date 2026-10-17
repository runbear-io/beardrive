import { test } from "node:test";
import assert from "node:assert/strict";
import { parseRecents, pushRecent } from "./recents.ts";

test("a reopened file moves to the front without a duplicate", () => {
  const a = pushRecent(pushRecent(pushRecent([], "a.md", 1), "b.md", 2), "a.md", 3);
  assert.deepEqual(a, [
    { path: "a.md", at: 3 },
    { path: "b.md", at: 2 },
  ]);
});

test("the list is capped", () => {
  let l: { path: string; at: number }[] = [];
  for (let i = 0; i < 20; i++) l = pushRecent(l, "f" + i, i, 5);
  assert.equal(l.length, 5);
  assert.equal(l[0].path, "f19");
});

test("garbage in storage reads as nothing", () => {
  assert.deepEqual(parseRecents("{"), []);
  assert.deepEqual(parseRecents('{"a":1}'), []);
  assert.deepEqual(parseRecents('[{"path":"x"},{"path":"y","at":2},null]'), [{ path: "y", at: 2 }]);
});
