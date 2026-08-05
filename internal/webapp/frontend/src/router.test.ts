// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
import { test } from "node:test";
import assert from "node:assert/strict";
import { parseRoute, urlForView, historyFilterQuery } from "./router.ts";

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

// History filters live in the URL, so a narrowed feed is linkable and comes
// back on reload and Back. Round-trip: parse → build → same URL.
test("history filters round-trip through the URL", () => {
  const r = parseRoute("/p-1/history/notes?q=runbook&user=mira@acme.io&since=2026-07-01&until=2026-07-31", "hub");
  assert.equal(r.view, "history");
  assert.equal(r.viewTarget, "notes");
  assert.deepEqual(r.filters, {
    q: "runbook",
    user: "mira@acme.io",
    since: "2026-07-01",
    until: "2026-07-31",
  });
  assert.equal(
    urlForView("history", "p-1", "notes", r.filters),
    "/p-1/history/notes?q=runbook&user=mira%40acme.io&since=2026-07-01&until=2026-07-31",
  );
});

// An unfiltered feed keeps the bare URL it has always had — no empty ?q=.
test("no filters means no query string", () => {
  assert.equal(parseRoute("/p-1/history", "hub").filters, undefined);
  assert.equal(historyFilterQuery(undefined), "");
  assert.equal(historyFilterQuery({ q: "" }), "");
  assert.equal(urlForView("history", "p-1"), "/p-1/history");
  // other views ignore them entirely
  assert.equal(urlForView("dashboard", "p-1", "", { q: "x" }), "/p-1/dashboard");
});

// BEA-64: the History API takes ?path=/?prefix=, so a reader who has seen it
// types the query form at the page too. It used to be dropped, which rendered
// the whole project as if the file had no history.
test("?path= and ?prefix= name the history target and ask to be normalized", () => {
  const p = parseRoute("/p-1/history?path=guide.md", "hub");
  assert.equal(p.view, "history");
  assert.equal(p.viewTarget, "guide.md");
  assert.equal(p.queryTarget, true);
  assert.equal(urlForView("history", "p-1", p.viewTarget, p.filters), "/p-1/history/guide.md");

  // Aliases, not modes: the view decides file-vs-subtree from the tree.
  const x = parseRoute("/p-1/history?prefix=notes", "hub");
  assert.equal(x.viewTarget, "notes");
  assert.equal(x.queryTarget, true);

  // Same trailing-slash tolerance a path segment gets.
  const s = parseRoute("/p-1/history?prefix=notes/", "hub");
  assert.equal(s.viewTarget, "notes");
  assert.equal(s.queryTarget, true);

  // A value that is nothing but separators names no file.
  const empty = parseRoute("/p-1/history?path=/", "hub");
  assert.equal(empty.viewTarget, "");
  assert.ok(!empty.queryTarget);
});

// The path route is the canonical one, so it wins and stays put — otherwise
// arriving at a file's feed with a stray ?path= would bounce you elsewhere.
test("a path segment beats ?path=, and other views ignore it", () => {
  const r = parseRoute("/p-1/history/a.md?path=b.md", "hub");
  assert.equal(r.viewTarget, "a.md");
  assert.equal(r.queryTarget, undefined);

  const d = parseRoute("/p-1/dashboard?path=guide.md", "hub");
  assert.equal(d.view, "dashboard");
  assert.equal(d.viewTarget, "");
  assert.equal(d.queryTarget, undefined);

  // Not a view at all: a file named like the parameter must not be hijacked.
  const f = parseRoute("/p-1/notes/readme.md?path=guide.md", "hub");
  assert.equal(f.path, "notes/readme.md");
  assert.equal(f.queryTarget, undefined);
});

// The normalization is a redirect, so anything else in the query string has
// to survive it — losing the author filter on the hop is the same bug again.
test("filters survive the ?path= normalization", () => {
  const r = parseRoute("/p-1/history?path=guide.md&user=alice@x.io", "hub");
  assert.equal(r.viewTarget, "guide.md");
  assert.equal(r.queryTarget, true);
  assert.deepEqual(r.filters, { user: "alice@x.io" });
  assert.equal(
    urlForView("history", "p-1", r.viewTarget, r.filters),
    "/p-1/history/guide.md?user=alice%40x.io",
  );
});

// Encoded slashes survive both decodes, so a target that arrived
// double-encoded re-encodes to exactly one URL rather than another redirect.
test("an encoded separator in ?path= round-trips", () => {
  const r = parseRoute("/p-1/history?path=a%2Fb.md", "hub");
  assert.equal(r.viewTarget, "a/b.md");
  assert.equal(r.queryTarget, true);
  assert.equal(urlForView("history", "p-1", r.viewTarget), "/p-1/history/a/b.md");
});
