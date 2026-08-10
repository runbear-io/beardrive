// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Excluded from tsconfig's include — it imports node: builtins, which the
// app's DOM-only lib set does not know about.
import { test } from "node:test";
import assert from "node:assert/strict";
import { groupRuns, runFileCount } from "./runs.ts";
import type { HistoryEntry } from "../api/types";

// A history entry, newest-first order supplied by the caller.
const e = (path: string, note?: string, device = "mac-mini"): HistoryEntry => ({
  time: "2026-07-29T14:02:00Z",
  kind: "edit",
  path,
  note,
  device: { id: device },
});

test("the repro run: 14 ops across 10 paths reads as 10 files", () => {
  const note = "claude-session-abc123";
  const entries = [
    ...Array.from({ length: 5 }, () => e("ideas.md", note)), // churned five times
    e("memory.md", note), // rewritten
    ...Array.from({ length: 8 }, (_, k) => e(`old/${k}.md`, note)), // deleted
  ];
  const items = groupRuns(entries);
  assert.equal(items.length, 1);
  const run = items[0].run;
  assert.ok(run);
  assert.equal(runFileCount(run), 10);
  assert.equal(run.entries.length, 14); // every op still listed inside the card
  // idx addresses the flat feed — diffs and restore shas ride on this.
  assert.deepEqual(
    run.idx,
    Array.from({ length: 14 }, (_, i) => i),
  );
  assert.deepEqual(
    run.entries.map((x) => x.path),
    entries.map((x) => x.path),
  );
});

test("five ops on one path are bare rows, not a card claiming five files", () => {
  const items = groupRuns(Array.from({ length: 5 }, () => e("ideas.md", "session-1")));
  assert.equal(items.length, 5);
  assert.deepEqual(items, [{ i: 0 }, { i: 1 }, { i: 2 }, { i: 3 }, { i: 4 }]);
});

test("a single op with a note is a bare row", () => {
  assert.deepEqual(groupRuns([e("ideas.md", "session-1")]), [{ i: 0 }]);
});

test("two paths still earn a card when one was edited repeatedly", () => {
  const note = "session-1";
  const entries = [
    e("ideas.md", note),
    e("ideas.md", note),
    e("ideas.md", note),
    e("ideas.md", note),
    e("memory.md", note),
  ];
  const items = groupRuns(entries);
  assert.equal(items.length, 1);
  const run = items[0].run;
  assert.ok(run);
  assert.equal(runFileCount(run), 2);
  assert.equal(run.entries.length, 5);
});

test("the grouping key is note + device, so two devices are two runs", () => {
  const note = "session-1";
  const items = groupRuns([
    e("a.md", note, "mac-mini"),
    e("b.md", note, "mac-mini"),
    e("a.md", note, "laptop"),
    e("c.md", note, "laptop"),
  ]);
  assert.equal(items.length, 2);
  assert.deepEqual(
    items.map((it) => it.run?.entries.length),
    [2, 2],
  );
  assert.deepEqual(items.map((it) => it.run?.idx), [
    [0, 1],
    [2, 3],
  ]);
});

test("the key separator keeps a note+device pair from colliding with another", () => {
  // Two runs whose note and device concatenate to the same string under a
  // printable separator: "a b" + "c" vs "a" + "b c".
  const items = groupRuns([
    e("one.md", "a b", "c"),
    e("two.md", "a b", "c"),
    e("three.md", "a", "b c"),
    e("four.md", "a", "b c"),
  ]);
  assert.equal(items.length, 2);
  assert.deepEqual(
    items.map((it) => it.run?.entries.map((x) => x.path)),
    [
      ["one.md", "two.md"],
      ["three.md", "four.md"],
    ],
  );
});

test("note-less changes stay bare rows, cards sit where their newest op did", () => {
  const note = "session-1";
  const items = groupRuns([e("plain.md"), e("a.md", note), e("other.md"), e("b.md", note)]);
  assert.equal(items.length, 3);
  assert.deepEqual(items[0], { i: 0 });
  assert.equal(items[1].i, 1);
  assert.equal(items[1].run?.entries.length, 2);
  assert.deepEqual(items[2], { i: 2 });
});

// Pagination leans on this: the History view accumulates pages into one
// array, so a run whose ops straddle a page boundary must become ONE card
// when the next page lands — and a run that looked single-file on page 1
// must grow into a card, not sit beside a second copy of itself.
test("a run split across two pages groups into one card", () => {
  const note = "session-1";
  const page1 = [e("plain.md"), e("a.md", note)];
  const page2 = [e("b.md", note), e("c.md", note)];
  assert.deepEqual(groupRuns(page1), [{ i: 0 }, { i: 1 }]); // page 1 alone: bare rows
  const items = groupRuns(page1.concat(page2));
  assert.equal(items.length, 2);
  assert.deepEqual(items[0], { i: 0 });
  assert.deepEqual(items[1].run?.entries.map((x) => x.path), ["a.md", "b.md", "c.md"]);
  assert.deepEqual(items[1].run?.idx, [1, 2, 3]); // idx still addresses the flat feed
});

// ---- grouping on the session id (BEA-98) ----

const es = (path: string, session?: string, note = "claude-code session x", device = "mac-mini"): HistoryEntry => ({
  time: "2026-07-29T14:02:00Z",
  kind: "edit",
  path,
  note,
  session,
  device: { id: device },
});

test("legacy entries (no session) still group by note + device, unchanged", () => {
  const items = groupRuns([e("a.md", "session-1"), e("b.md", "session-1")]);
  assert.equal(items.length, 1);
  assert.equal(items[0].run?.entries.length, 2);
  assert.equal(items[0].run?.session, undefined);
});

test("one note, two sessions = two runs — a note cannot merge into someone's card", () => {
  const items = groupRuns([
    es("a.md", "sess-1"),
    es("b.md", "sess-1"),
    es("c.md", "sess-2"),
    es("d.md", "sess-2"),
  ]);
  assert.equal(items.length, 2);
  assert.deepEqual(
    items.map((i) => i.run?.session),
    ["sess-1", "sess-2"],
  );
});

test("one session on two devices is still two runs", () => {
  const items = groupRuns([
    es("a.md", "sess-1", "note", "mac-mini"),
    es("b.md", "sess-1", "note", "mac-mini"),
    es("c.md", "sess-1", "note", "linux-box"),
    es("d.md", "sess-1", "note", "linux-box"),
  ]);
  assert.equal(items.length, 2);
});

test("a session-keyed run never merges with a note-keyed one", () => {
  // The legacy rows carry the same note the session rows do; only the
  // session-bearing ones may group together.
  const items = groupRuns([
    es("a.md", "sess-1"),
    es("b.md", "sess-1"),
    e("c.md", "claude-code session x"),
    e("d.md", "claude-code session x"),
  ]);
  assert.equal(items.length, 2);
  assert.equal(items[0].run?.session, "sess-1");
  assert.equal(items[1].run?.session, undefined);
});

test("a session with no note at all still forms a run", () => {
  const items = groupRuns([es("a.md", "sess-1", ""), es("b.md", "sess-1", "")]);
  assert.equal(items.length, 1);
  assert.equal(items[0].run?.entries.length, 2);
});
