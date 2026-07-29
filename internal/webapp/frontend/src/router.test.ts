// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
import { test } from "node:test";
import assert from "node:assert/strict";
import { parseRoute } from "./router.ts";

// A trailing slash is what a browser hands you when you copy a folder URL,
// so /notes/ has to be the same page as /notes.
test("trailing slashes are stripped off project paths", () => {
  const a = parseRoute("/p-1/notes/", "hub");
  assert.equal(a.path, "notes");
  assert.equal(a.trailingSlash, true);

  const b = parseRoute("/p-1/notes//", "hub");
  assert.equal(b.path, "notes");
  assert.equal(b.trailingSlash, true);

  const c = parseRoute("/p-1/notes/deep/", "hub");
  assert.equal(c.path, "notes/deep");
  assert.equal(c.trailingSlash, true);

  const f = parseRoute("/p-1/guide.md/", "hub");
  assert.equal(f.path, "guide.md");
  assert.equal(f.trailingSlash, true);
});

// Nothing to strip means no flag — otherwise the project root would ask for
// a redirect to itself, forever.
test("the project root does not ask for a redirect", () => {
  const r = parseRoute("/p-1/", "hub");
  assert.equal(r.project, "p-1");
  assert.equal(r.path, "");
  assert.ok(!r.trailingSlash);

  const bare = parseRoute("/p-1", "hub");
  assert.equal(bare.path, "");
  assert.ok(!bare.trailingSlash);
});

test("volume mode strips too", () => {
  const r = parseRoute("/notes/", "volume");
  assert.equal(r.path, "notes");
  assert.equal(r.trailingSlash, true);

  const root = parseRoute("/", "volume");
  assert.equal(root.path, "");
  assert.ok(!root.trailingSlash);
});

test("the ?v= version survives the rewrite", () => {
  const r = parseRoute("/p-1/notes/?v=abc123", "hub");
  assert.equal(r.path, "notes");
  assert.equal(r.version, "abc123");
  assert.equal(r.trailingSlash, true);
});

// View targets were already normalized at parse; this guards that.
test("view routes still resolve", () => {
  const r = parseRoute("/p-1/history/notes/", "hub");
  assert.equal(r.view, "history");
  assert.equal(r.viewTarget, "notes");
  assert.equal(r.path, "");
});

// Stripping happens on the still-encoded slice, so the redirect target
// re-encodes to exactly one URL rather than another one that redirects.
test("odd characters round-trip", () => {
  const r = parseRoute("/p-1/notes/a%20b/", "hub");
  assert.equal(r.path, "notes/a b");
  assert.equal(r.trailingSlash, true);
});
