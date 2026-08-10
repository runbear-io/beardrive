// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Excluded from tsconfig's include — it imports node: builtins, which the
// app's DOM-only lib set does not know about.
import { test } from "node:test";
import assert from "node:assert/strict";
import { parseDelimited, CSV_ROWS } from "./csv.ts";

const rows = (text: string, delim = ",", cap = CSV_ROWS) =>
  parseDelimited(text, delim, cap)?.rows;

test("plain file: first row is the header, one cell per field", () => {
  assert.deepEqual(rows("a,b,c\n1,2,3\n"), [
    ["a", "b", "c"],
    ["1", "2", "3"],
  ]);
});

test("a trailing newline does not add a blank row", () => {
  assert.equal(rows("a,b\n1,2\n")!.length, 2);
  assert.equal(rows("a,b\n1,2")!.length, 2); // and neither does its absence
});

test("a quoted comma stays inside its cell", () => {
  assert.deepEqual(rows('a,"b,c",d\n')![0], ["a", "b,c", "d"]);
});

test('"" is one literal quote', () => {
  assert.deepEqual(rows('x,y\n"he said ""hi"" ok",2\n')![1], ['he said "hi" ok', "2"]);
});

test("a newline inside quotes is content, not a row break", () => {
  const r = rows('a,b\n"two lines\nin one cell",2\n')!;
  assert.equal(r.length, 2);
  assert.deepEqual(r[1], ["two lines\nin one cell", "2"]);
});

test("CRLF ends a row like LF", () => {
  assert.deepEqual(rows("a,b\r\n1,2\r\n"), [
    ["a", "b"],
    ["1", "2"],
  ]);
});

test("a short row keeps its cells — no shifting, no throw", () => {
  const r = rows("a,b,c\n1,2\n")!;
  assert.deepEqual(r[1], ["1", "2"]); // the view pads the missing trailing cell
});

test("a long row is not truncated either", () => {
  assert.deepEqual(rows("a,b\n1,2,3\n")![1], ["1", "2", "3"]);
});

test("empty cells survive, including a trailing one", () => {
  assert.deepEqual(rows("a,b,c\n1,,\n")![1], ["1", "", ""]);
});

test("rows past the cap are counted, not kept", () => {
  const text = "a,b\n" + "1,2\n".repeat(10);
  const out = parseDelimited(text, ",", 4)!;
  assert.equal(out.rows.length, 4);
  assert.equal(out.truncated, 7); // 11 rows total, 4 kept
});

test("an unterminated quote falls back to text", () => {
  assert.equal(parseDelimited('a,b\n"never closed,2\n', ",", CSV_ROWS), null);
});

test("a file with no delimiter is text, not a one-column table", () => {
  assert.equal(parseDelimited("just some prose\nover two lines\n", ",", CSV_ROWS), null);
  assert.equal(parseDelimited("", ",", CSV_ROWS), null);
});

test("tabs: the same file with a tab delimiter", () => {
  assert.deepEqual(rows("a\tb\n1\t2\n", "\t"), [
    ["a", "b"],
    ["1", "2"],
  ]);
  // and a comma-delimited file read as TSV is not a table
  assert.equal(parseDelimited("a,b\n1,2\n", "\t", CSV_ROWS), null);
});

test("a quote that is not at the start of a cell is literal", () => {
  assert.deepEqual(rows('a,5" pipe,c\n')![0], ["a", '5" pipe', "c"]);
});
