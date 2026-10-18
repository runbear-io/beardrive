// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
import { test } from "node:test";
import assert from "node:assert/strict";
import { bootRequests } from "./bootRequests.ts";
import { parseRoute } from "../router.ts";
import { MD_EXT } from "../util.ts";

const ID = "097b15df-d152-49bd-ba58-294332d11e9e";
const urls = (u: string) => bootRequests(parseRoute(u, "hub"), (p) => MD_EXT.test(p)).map((r) => r.url);

test("a shared markdown link starts the tree, the document and its comments", () => {
  assert.deepEqual(urls(`/${ID}/notes/a%20b.md`), [
    "/api/projects",
    "/api/orgs",
    `/api/p/${ID}/tree?slim=1`,
    `/api/p/${ID}/render?path=notes%2Fa%20b.md`,
    `/api/p/${ID}/heat?days=30`,
    `/api/p/${ID}/comments?status=all&path=notes%2Fa%20b.md`,
  ]);
});

test("the keys are the ones the hooks read", () => {
  const keys = bootRequests(parseRoute(`/${ID}/a.md`, "hub"), (p) => MD_EXT.test(p)).map((r) => r.queryKey);
  const base = `/api/p/${ID}/`;
  assert.deepEqual(keys, [["projects"], ["orgs"], ["tree", base], ["render", base, "a.md", ""], ["heat", base], ["comments", base, "a.md"]]);
});

test("anything but a plain markdown read starts only the shell and the tree", () => {
  for (const u of [`/${ID}`, `/${ID}/report.html`, `/${ID}/a.md?v=${"a".repeat(64)}`, `/${ID}/edit/a.md`, `/${ID}/history/a.md`]) {
    assert.deepEqual(urls(u), ["/api/projects", "/api/orgs", `/api/p/${ID}/tree?slim=1`, `/api/p/${ID}/heat?days=30`], u);
  }
});

test("a URL that does not name a project starts nothing", () => {
  for (const u of ["/", "/notes/a.md", "/join/abc", "/billing"]) {
    assert.deepEqual(urls(u), [], u);
  }
});
