// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Excluded from tsconfig's include — it imports node: builtins, which the
// app's DOM-only lib set does not know about.
import { test } from "node:test";
import assert from "node:assert/strict";
import { splitLines, lcsDiff, diffText } from "./diff.ts";

const ops = (ls: ReturnType<typeof lcsDiff>) => ls.map((l) => l.op + l.line);

test("splitLines: trailing newline ends the last line", () => {
  assert.deepEqual(splitLines("a\nb\n"), ["a", "b"]);
  assert.deepEqual(splitLines("a\nb"), ["a", "b"]);
  assert.deepEqual(splitLines(""), []);
  assert.deepEqual(splitLines("\n"), [""]);
});

test("identical files produce only context", () => {
  const d = diffText("a\nb\nc\n", "a\nb\nc\n");
  assert.deepEqual(ops(d.lines), ["=a", "=b", "=c"]);
  assert.equal(d.add, 0);
  assert.equal(d.del, 0);
});

test("pure insertion", () => {
  const d = diffText("a\nc\n", "a\nb\nc\n");
  assert.deepEqual(ops(d.lines), ["=a", "+b", "=c"]);
  assert.equal(d.add, 1);
  assert.equal(d.del, 0);
});

test("pure deletion", () => {
  const d = diffText("a\nb\nc\n", "a\nc\n");
  assert.deepEqual(ops(d.lines), ["=a", "-b", "=c"]);
  assert.equal(d.add, 0);
  assert.equal(d.del, 1);
});

test("replacement — the seeded guide.md case", () => {
  const d = diffText(
    "# Guide\n\nFirst version of the guide.\n",
    "# Guide\n\nSecond version of the guide, with more detail.\n",
  );
  assert.deepEqual(ops(d.lines), [
    "=# Guide",
    "=",
    "-First version of the guide.",
    "+Second version of the guide, with more detail.",
  ]);
  assert.equal(d.add, 1);
  assert.equal(d.del, 1);
});

test("empty file on either side", () => {
  assert.deepEqual(ops(diffText("", "a\nb\n").lines), ["+a", "+b"]);
  assert.deepEqual(ops(diffText("a\nb\n", "").lines), ["-a", "-b"]);
  assert.deepEqual(ops(diffText("", "").lines), []);
});

test("no trailing newline is a change of its own", () => {
  // "a\nb" vs "a\nb\n" both split to ["a","b"], so the line diff is empty —
  // documented behaviour, not an accident: this is a line differ.
  assert.deepEqual(ops(diffText("a\nb", "a\nb\n").lines), ["=a", "=b"]);
  // A real edit on a file with no trailing newline still diffs.
  assert.deepEqual(ops(diffText("a\nb", "a\nc").lines), ["=a", "-b", "+c"]);
});

test("line numbers track both sides", () => {
  const d = diffText("a\nb\nc\n", "a\nx\nc\n");
  assert.deepEqual(
    d.lines.map((l) => [l.op, l.an, l.bn]),
    [
      ["=", 1, 1],
      ["-", 2, undefined],
      ["+", undefined, 2],
      ["=", 3, 3],
    ],
  );
});

test("a one-line edit in a long file stays cheap and local", () => {
  const big = Array.from({ length: 5000 }, (_, i) => "line " + i);
  const b = big.slice();
  b[2500] = "changed";
  const d = lcsDiff(big, b);
  assert.equal(d.length, 5001); // 5000 context + 1 added, 1 removed, minus the replaced line
  assert.deepEqual(
    d.filter((l) => l.op !== "=").map((l) => l.op + l.line),
    ["-line 2500", "+changed"],
  );
});

test("past the cell budget it degrades to whole-file replacement", () => {
  const a = Array.from({ length: 2100 }, (_, i) => "a" + i);
  const b = Array.from({ length: 2100 }, (_, i) => "b" + i);
  const d = lcsDiff(a, b);
  assert.equal(d.filter((l) => l.op === "-").length, 2100);
  assert.equal(d.filter((l) => l.op === "+").length, 2100);
  assert.equal(d.filter((l) => l.op === "=").length, 0);
});
