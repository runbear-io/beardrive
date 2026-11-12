import { test } from "node:test";
import assert from "node:assert/strict";
import type { Node } from "../api/types.ts";
import { applyTreeDeltas } from "./treedelta.ts";

test("applyTreeDeltas inserts nested puts and prunes empty parent directories on delete", () => {
  const start: Node = {
    name: "",
    dir: true,
    children: [
      {
        name: "nested",
        dir: true,
        children: [{ name: "only.md", size: 5, time: "2026-11-12T10:00:00Z" }],
      },
    ],
  };
  const next = applyTreeDeltas(start, [
    { path: "nested/only.md", op: "delete", time: "2026-11-12T10:01:00Z" },
    { path: "docs/guide.md", op: "put", size: 42, time: "2026-11-12T10:02:00Z" },
  ]);
  assert.equal(next.children?.length, 1);
  assert.equal(next.children?.[0]?.name, "docs");
  assert.equal(next.children?.[0]?.children?.[0]?.name, "guide.md");
  assert.equal(next.children?.[0]?.children?.[0]?.size, 42);
});
