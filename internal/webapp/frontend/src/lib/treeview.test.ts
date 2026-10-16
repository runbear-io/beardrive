import { test } from "node:test";
import assert from "node:assert/strict";
import type { Node } from "../api/types";
import { buildRows, filterTree, systemKey, treeStats } from "./treeview.ts";

const f = (path: string, time = "2026-01-01T00:00:00Z"): Node => ({
  name: path.slice(path.lastIndexOf("/") + 1),
  path,
  time,
});
const d = (path: string, children: Node[]): Node => ({
  name: path.slice(path.lastIndexOf("/") + 1),
  path,
  dir: true,
  children,
});

const COPY = "log.md.bdrive-conflict-laptop-20260814T060945Z";

function names(rows: ReturnType<typeof buildRows>): string[] {
  return rows.map((r) =>
    r.kind === "node" ? r.node.path : r.kind === "system" ? "[system " + r.count + "]" : "[more " + r.hidden + "]",
  );
}

test("conflict copies fold into a badge on their original", () => {
  const root = d("", [f("log.md"), f(COPY), f("orphan.md.bdrive-conflict-x-20260814T060945Z")]);
  const stats = treeStats(root, 0);
  const rows = buildRows(root, { expanded: new Set(), showAll: new Set(), stats });
  assert.deepEqual(names(rows), ["log.md", "orphan.md.bdrive-conflict-x-20260814T060945Z"]);
  assert.equal(rows[0].kind === "node" && rows[0].conflicts, 1);
});

test("system files group at the end of their level, collapsed", () => {
  const root = d("", [f(".bdriveignore"), f("_lint.py"), f("a.md")]);
  const stats = treeStats(root, 0);
  assert.deepEqual(names(buildRows(root, { expanded: new Set(), showAll: new Set(), stats })), ["a.md", "[system 2]"]);
  assert.deepEqual(
    names(buildRows(root, { expanded: new Set([systemKey("")]), showAll: new Set(), stats })),
    ["a.md", "[system 2]", ".bdriveignore", "_lint.py"],
  );
});

test("nested folders cap at the most recent children, keeping the selection", () => {
  const kids = Array.from({ length: 9 }, (_, i) => f(`p/f${i}.md`, `2026-01-0${i + 1}T00:00:00Z`));
  const root = d("", [d("p", kids)]);
  const stats = treeStats(root, 0);
  const exp = new Set(["p"]);
  assert.deepEqual(names(buildRows(root, { expanded: exp, showAll: new Set(), stats })), [
    "p", "p/f8.md", "p/f7.md", "p/f6.md", "p/f5.md", "p/f4.md", "[more 4]",
  ]);
  assert.deepEqual(names(buildRows(root, { expanded: exp, showAll: new Set(), stats, keep: "p/f0.md" })).slice(-2), [
    "p/f0.md", "[more 3]",
  ]);
  assert.equal(buildRows(root, { expanded: exp, showAll: new Set(["p"]), stats }).length, 10);
});

test("folders count files changed after the baseline", () => {
  const root = d("", [d("a", [f("a/x", "2026-02-01T00:00:00Z"), f("a/y", "2025-01-01T00:00:00Z")])]);
  const stats = treeStats(root, Date.parse("2026-01-01T00:00:00Z"));
  assert.equal(stats.fresh.get("a"), 1);
});

test("filter matches base names, exact and prefix first", () => {
  const r = filterTree("konny", ["projects/konny", "accounts/konny-old", "konnyx/deep"], [
    { path: "s/Konny-call.md", name: "Konny-call.md" },
    { path: "a/notkonny.md", name: "notkonny.md" },
    { path: "a/" + COPY.replace("log", "konny"), name: COPY.replace("log", "konny") },
  ]);
  assert.deepEqual(r.folders.map((h) => h.path), ["projects/konny", "accounts/konny-old"]);
  assert.deepEqual(r.files.map((h) => h.path), ["s/Konny-call.md", "a/notkonny.md"]);
  assert.equal(r.files[1].at, 3);
});
