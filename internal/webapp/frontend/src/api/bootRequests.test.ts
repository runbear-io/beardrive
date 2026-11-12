// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
import { test } from "node:test";
import assert from "node:assert/strict";
import { bootRequests } from "./bootRequests.ts";
import { parseRoute } from "../router.ts";
import { MD_EXT } from "../util.ts";

const ID = "097b15df-d152-49bd-ba58-294332d11e9e";
const base = `/api/p/${ID}/`;
const reqs = (u: string) => bootRequests(parseRoute(u, "hub"), (p) => MD_EXT.test(p));
const urls = (u: string) => reqs(u).map((r) => r.url);
const shell = ["/api/projects", "/api/orgs", `${base}tree?slim=1`];

test("a shared markdown link starts the tree, the document, its comments and its source", () => {
  assert.deepEqual(urls(`/${ID}/notes/a%20b.md`), [
    ...shell,
    `${base}render?path=notes%2Fa%20b.md`,
    `${base}heat?days=30`,
    `${base}comments?status=all&path=notes%2Fa%20b.md`,
    `${base}file?path=notes%2Fa%20b.md`,
  ]);
});

test("the keys are the ones the hooks read", () => {
  const keys = reqs(`/${ID}/a.md`).map((r) => r.queryKey);
  assert.deepEqual(keys, [
    ["projects"],
    ["orgs"],
    ["tree", base],
    ["render", base, "a.md", ""],
    ["heat", base],
    ["comments", base, "a.md"],
    ["text", `${base}file?path=a.md`],
  ]);
});

test("only the file's bytes are read as a blob; everything else is JSON", () => {
  for (const r of reqs(`/${ID}/a.md`)) {
    assert.equal(r.kind ?? "json", r.queryKey[0] === "text" ? "blob" : "json", r.url);
  }
});

test("a text, code or CSV read starts its bytes before React mounts", () => {
  for (const p of ["data.json", "src/main.ts", "rows.csv", "notes.txt"]) {
    const q = encodeURIComponent(p);
    const got = reqs(`/${ID}/${p}`);
    assert.deepEqual(got.map((r) => r.url), [...shell, `${base}file?path=${q}`, `${base}heat?days=30`], p);
    assert.equal(got[3].kind, "blob", p);
    assert.deepEqual(got[3].queryKey, ["text", `${base}file?path=${q}`], p);
  }
});

test("editing a markdown file starts its source, render and comments", () => {
  assert.deepEqual(urls(`/${ID}/edit/a.md`), [
    ...shell,
    `${base}file?path=a.md`,
    `${base}render?path=a.md`,
    `${base}comments?status=all&path=a.md`,
    `${base}heat?days=30`,
  ]);
});

test("editing a code file starts only its source", () => {
  assert.deepEqual(urls(`/${ID}/edit/data.json`), [...shell, `${base}file?path=data.json`, `${base}heat?days=30`]);
});

test("pinned versions, project views, HTML, images, PDFs and folders start only the shell and the tree", () => {
  for (const u of [
    `/${ID}`,
    `/${ID}/report.html`,
    `/${ID}/a.png`,
    `/${ID}/a.pdf`,
    `/${ID}/notes`,
    `/${ID}/a.md?v=${"a".repeat(64)}`,
    `/${ID}/history/a.md`,
  ]) {
    assert.deepEqual(urls(u), [...shell, `${base}heat?days=30`], u);
  }
});

test("a URL that does not name a project starts nothing", () => {
  for (const u of ["/", "/notes/a.md", "/join/abc", "/billing"]) {
    assert.deepEqual(urls(u), [], u);
  }
});
