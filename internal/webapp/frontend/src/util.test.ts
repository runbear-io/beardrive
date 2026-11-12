// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Node has no localStorage, which is what makes the hostile-browser branches
// cheap to pin: the bare run IS the "storage missing" case.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  absTime,
  displayName,
  headingSlug,
  lastProject,
  rememberProject,
  resolveWiki,
  resolveWikiTarget,
  splitWikiTarget,
} from "./util.ts";

// Swap globalThis.localStorage for the length of one call, always putting the
// original back — later tests in the same process must not inherit a stub.
function withStorage(stub: unknown, fn: () => void) {
  const g = globalThis as { localStorage?: unknown };
  const had = "localStorage" in g;
  const prev = g.localStorage;
  g.localStorage = stub;
  try {
    fn();
  } finally {
    if (had) g.localStorage = prev;
    else delete g.localStorage;
  }
}

test("no storage at all: reads empty, writes stay silent", () => {
  assert.equal(lastProject(), "");
  assert.doesNotThrow(() => rememberProject("p1"));
});

test("round-trips the last project through storage", () => {
  const m = new Map<string, string>();
  withStorage(
    {
      getItem: (k: string) => m.get(k) ?? null,
      setItem: (k: string, v: string) => void m.set(k, v),
    },
    () => {
      assert.equal(lastProject(), "");
      rememberProject("p1");
      assert.equal(lastProject(), "p1");
      rememberProject("p2");
      assert.equal(lastProject(), "p2");
    },
  );
});

/* The wikilink match matrix. It used to run at click time inside the file
   view, where a browser was the only way to reach it; the rules did not
   change when they moved (BEA-136), so this is the whole contract. */
const FILES = [
  { path: "guide.md", name: "guide.md" },
  { path: "notes/readme.md", name: "readme.md" },
  { path: "notes/deep/Topic.md", name: "Topic.md" },
  { path: "LICENSE", name: "LICENSE" },
  // Same basename as a file higher up, to pin the path-before-basename rule.
  { path: "archive/guide.md", name: "guide.md" },
];

test("wikilink target resolves by path, then basename", () => {
  const hit = (t: string) => resolveWiki(t, FILES)?.path;
  assert.equal(hit("notes/readme.md"), "notes/readme.md"); // exact path
  assert.equal(hit("notes/readme"), "notes/readme.md"); // path, .md implied
  assert.equal(hit("readme"), "notes/readme.md"); // basename, .md implied
  assert.equal(hit("readme.md"), "notes/readme.md"); // basename
  assert.equal(hit("LICENSE"), "LICENSE"); // no extension at all
  assert.equal(hit("x y"), undefined); // spaces are just characters
  assert.equal(hit("nowhere"), undefined); // a miss is undefined, not a throw
});

test("wikilink matching is case-insensitive on both sides", () => {
  const hit = (t: string) => resolveWiki(t, FILES)?.path;
  assert.equal(hit("GUIDE"), "guide.md");
  assert.equal(hit("NOTES/DEEP/topic"), "notes/deep/Topic.md");
  assert.equal(hit("topic.MD"), "notes/deep/Topic.md");
});

test("an exact path beats a basename anywhere else in the tree", () => {
  assert.equal(resolveWiki("archive/guide", FILES)?.path, "archive/guide.md");
  // Bare "guide" is ambiguous by basename; the top-level path wins.
  assert.equal(resolveWiki("guide", FILES)?.path, "guide.md");
});

test("a #heading or ^block suffix names the fragment, not the file", () => {
  assert.deepEqual(splitWikiTarget("other-doc#Target Heading"), { file: "other-doc", fragment: "#target-heading" });
  assert.deepEqual(splitWikiTarget("doc#^abc123"), { file: "doc", fragment: "#abc123" });
  assert.deepEqual(splitWikiTarget("doc^abc123"), { file: "doc", fragment: "#abc123" });
  assert.deepEqual(splitWikiTarget("#Only Here"), { file: "", fragment: "#only-here" });
  assert.deepEqual(splitWikiTarget("doc#"), { file: "doc", fragment: "" });
  assert.equal(resolveWiki("guide#Setup", FILES)?.path, "guide.md");
  assert.deepEqual(resolveWikiTarget("notes/readme#Install Steps", FILES), {
    path: "notes/readme.md",
    fragment: "#install-steps",
  });
  assert.deepEqual(resolveWikiTarget("#Top", FILES), { path: "", fragment: "#top" });
  assert.equal(resolveWikiTarget("nowhere#x", FILES), undefined);
});

test("heading slugs match the hub's ids, Unicode kept", () => {
  assert.equal(headingSlug("Target Heading"), "target-heading");
  assert.equal(headingSlug("  Hello, World!  "), "hello-world");
  assert.equal(headingSlug("snake_case-name"), "snake-case-name");
  assert.equal(headingSlug("한글 제목"), "한글-제목");
  // NFD input slugs to the NFC id the server emits.
  assert.equal(headingSlug("한글".normalize("NFD")), "한글");
  assert.equal(headingSlug("Ünïcode"), "ünïcode");
  assert.equal(headingSlug("!!!"), "heading");
});

test("wikilinks match across NFC/NFD and return the tree's own path", () => {
  const nfd = "notes/회의록.md".normalize("NFD");
  const files = [{ path: nfd, name: "회의록.md".normalize("NFD") }];
  assert.equal(resolveWiki("회의록", files)?.path, nfd); // NFC target, NFD file
  assert.equal(resolveWiki("notes/회의록".normalize("NFD"), files)?.path, nfd);
  assert.deepEqual(resolveWikiTarget("회의록#안건", files), { path: nfd, fragment: "#안건" });
});

test("storage that throws is swallowed on both sides", () => {
  const boom = () => {
    throw new Error("SecurityError");
  };
  withStorage({ getItem: boom, setItem: boom }, () => {
    assert.equal(lastProject(), "");
    assert.doesNotThrow(() => rememberProject("p1"));
  });
});

test("displayName prefers the display name, then the email's local part", () => {
  assert.equal(displayName({ user: "snow@x.io", user_name: "Snow Lee" }), "Snow Lee");
  assert.equal(displayName({ user: "snow@x.io" }), "snow");
  assert.equal(displayName({ author: "laptop" }), "laptop");
  assert.equal(displayName({}), "unknown");
});

test("absTime renders date · time in the given locale", () => {
  const d = new Date(2026, 8, 30, 8, 53);
  assert.equal(absTime(d, "en-US").replace(/ /g, " "), "Sep 30, 2026 · 8:53 AM");
  assert.equal(absTime("not a date"), "");
});
