// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Node has no localStorage, which is what makes the hostile-browser branches
// cheap to pin: the bare run IS the "storage missing" case.
import { test } from "node:test";
import assert from "node:assert/strict";
import { lastProject, rememberProject } from "./util.ts";

// Swap globalThis.localStorage for the length of one call, always putting the
// original back — later tests in the same process must not inherit a stub.
function withStorage(stub: unknown, fn: () => void) {
  const g = globalThis as { localStorage?: unknown };
  const had = "localStorage" in g;
  const prev = g.localStorage;
  g.localStorage = stub;
  try {
    fn();
  } finally {
    if (had) g.localStorage = prev;
    else delete g.localStorage;
  }
}

test("no storage at all: reads empty, writes stay silent", () => {
  assert.equal(lastProject(), "");
  assert.doesNotThrow(() => rememberProject("p1"));
});

test("round-trips the last project through storage", () => {
  const m = new Map<string, string>();
  withStorage(
    {
      getItem: (k: string) => m.get(k) ?? null,
      setItem: (k: string, v: string) => void m.set(k, v),
    },
    () => {
      assert.equal(lastProject(), "");
      rememberProject("p1");
      assert.equal(lastProject(), "p1");
      rememberProject("p2");
      assert.equal(lastProject(), "p2");
    },
  );
});

test("storage that throws is swallowed on both sides", () => {
  const boom = () => {
    throw new Error("SecurityError");
  };
  withStorage({ getItem: boom, setItem: boom }, () => {
    assert.equal(lastProject(), "");
    assert.doesNotThrow(() => rememberProject("p1"));
  });
});
