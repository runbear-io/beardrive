import { test } from "node:test";
import assert from "node:assert/strict";
import type { HistoryEntry, Node } from "../api/types.ts";
import { devicesFrom, mergeEntries, freshness, greeting, readsByFolder, sinceSummary, staleHot } from "./home.ts";

const e = (time: string, path: string, dev: string, extra: Partial<HistoryEntry> = {}): HistoryEntry => ({
  time,
  kind: "edit",
  path,
  device: { id: dev, name: dev + "-name" },
  ...extra,
});

test("greeting uses the first name and the hour", () => {
  assert.equal(greeting("Liam Hwang", new Date(2026, 0, 1, 9)), "Good morning, Liam");
  assert.equal(greeting("", new Date(2026, 0, 1, 14)), "Good afternoon");
});

test("since summary stops at the baseline and counts agents", () => {
  const list = [
    e("2026-10-16T10:00:00Z", "a.md", "d1", { session: "s", note: "claude-code session s" }),
    e("2026-10-16T09:00:00Z", "a.md", "d2"),
    e("2026-10-16T08:00:00Z", "b.md", "d2"),
    e("2026-10-15T08:00:00Z", "c.md", "d2"),
  ];
  const s = sinceSummary(list, Date.parse("2026-10-16T07:00:00Z"), true);
  assert.deepEqual(s, { changes: 3, files: 2, agent: 1, capped: false });
  assert.equal(sinceSummary(list.slice(0, 2), 0, true).capped, true);
});

test("devices: newest first, agent from the newest stamped change, you marked", () => {
  const d = devicesFrom(
    [
      e("2026-10-16T10:00:00Z", "a", "d1"),
      e("2026-10-16T09:00:00Z", "a", "d2", { user: "me@x" }),
      e("2026-10-16T08:00:00Z", "a", "d1", { session: "s", note: "codex session s" }),
    ],
    "me@x",
  );
  assert.deepEqual(
    d.map((x) => [x.id, x.agent, x.you]),
    [
      ["d1", "Codex", false],
      ["d2", undefined, true],
    ],
  );
});

test("hot-but-stale and folder reads", () => {
  const now = Date.parse("2026-10-16T00:00:00Z");
  const files: Node[] = [
    { name: "x.md", path: "docs/x.md", time: "2026-08-01T00:00:00Z" },
    { name: "y.md", path: "docs/y.md", time: "2026-10-15T00:00:00Z" },
  ];
  const heat = { "docs/x.md": { human: 5 }, "docs/y.md": { agent: 9 } };
  assert.deepEqual(
    staleHot(files, heat, now).map((a) => a.path),
    ["docs/x.md"],
  );
  const root: Node = { name: "", path: "", dir: true, children: [{ name: "docs", path: "docs", dir: true, children: files }] };
  assert.deepEqual(readsByFolder(root, heat, now), [{ name: "docs", reads: 14, fresh: "fresh" }]);
  assert.equal(freshness(null), "stale");
  assert.equal(freshness(20), "aging");
});

test("merging two pages keeps each change once, newest first", () => {
  const x = e("2026-10-16T10:00:00Z", "a", "d1");
  const y = e("2026-10-16T09:00:00Z", "b", "d1");
  assert.deepEqual(mergeEntries([x], [y, x]).map((v) => v.path), ["a", "b"]);
});
