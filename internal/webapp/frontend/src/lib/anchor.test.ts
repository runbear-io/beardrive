// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Only the string half: the DOM half is driven by the e2e suite.
import { test } from "node:test";
import assert from "node:assert/strict";
import { bestMatch, contextAround, locate, squash } from "./anchor.ts";

test("squash drops whitespace and case, and maps back", () => {
  const { text, map } = squash("A b\n\tC");
  assert.equal(text, "abc");
  assert.deepEqual(map, [0, 2, 5]);
});

test("a selection's line breaks still match the text nodes", () => {
  // Selection.toString() puts newlines between blocks; the text nodes of
  // <h1>Title</h1><p>Body text</p> have none.
  const all = "TitleBody text";
  assert.deepEqual(locate(all, { quote: "Title\n\nBody" }), [0, 9]);
});

test("context picks between two occurrences", () => {
  const all = "Revenue grew by 12% this quarter. Costs grew by 12% too.";
  const first = locate(all, { quote: "grew by 12%", prefix: "Revenue ", suffix: " this" });
  const second = locate(all, { quote: "grew by 12%", prefix: "Costs ", suffix: " too." });
  assert.equal(all.slice(...first!), "grew by 12%");
  assert.ok(second![0] > first![0]);
  assert.equal(all.slice(...second!), "grew by 12%");
});

test("an absent or empty quote locates nothing", () => {
  assert.equal(locate("hello", { quote: "bye" }), null);
  assert.equal(locate("hello", { quote: "  \n" }), null);
  assert.equal(bestMatch("abc", ""), -1);
});

test("context is collapsed and bounded", () => {
  const all = "x".repeat(100) + "\n\n  quote  \n" + "y".repeat(100);
  const s = all.indexOf("quote");
  const { prefix, suffix } = contextAround(all, s, s + 5);
  assert.ok(prefix.endsWith("x ") && prefix.length <= 48);
  assert.ok(suffix.startsWith(" y") && suffix.length <= 48);
});
