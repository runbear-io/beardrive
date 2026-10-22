// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Excluded from tsconfig's include — it imports node: builtins, which the
// app's DOM-only lib set does not know about.
import { test } from "node:test";
import assert from "node:assert/strict";
import { prepare, search } from "./fuzzy.ts";

// A realistic tree: the queries below are the ones a person actually types.
const TREE = [
  "docs/install-guide.md",
  "docs/getting-started.md",
  "specs/hub-config.md",
  "webapp/frontend/src/components/FileView.tsx",
  "design/onboarding.md",
  "guide/other/install.md",
  "launch/idea.md",
];

function rank(query: string, tree: string[] = TREE): string[] {
  return search(query, tree.map(prepare)).hits.map((h) => tree[h.index]);
}

// The painted text of each highlighted range.
function marked(query: string, label: string): string[] {
  const [hit] = search(query, [prepare(label)]).hits;
  assert.ok(hit, `${query} should match ${label}`);
  return hit.ranges.map(([a, b]) => label.slice(a, b));
}

test("an empty query matches nothing — the palette decides what an empty box shows", () => {
  assert.deepEqual(search("", TREE.map(prepare)), { hits: [], total: 0 });
  assert.deepEqual(search("   ", TREE.map(prepare)), { hits: [], total: 0 });
});

// The greedy walk this replaced anchored a token on the first place each
// letter occurs, so the file whose name you typed lost to paths that merely
// started earlier (each case below is one it got wrong on this repo's tree).
test("the file whose name you typed comes first", () => {
  const tree = [
    "internal/webapp/cli_e2e_test.go",
    "internal/webapp/cli_daemon_race_test.go",
    "internal/autostart/sec_linux_test.go",
    "web/docs/src/content/docs/reference/cli.md",
    "cmd/bdrive/autostart_help_test.go",
    ".claude/agents/security-hacker.md",
    "internal/webapp/auth.go",
    "internal/webapp/frontend/src/components/NavRail.tsx",
    "internal/webapp/frontend/src/nav.ts",
  ];
  assert.equal(rank("cli", tree)[0], "web/docs/src/content/docs/reference/cli.md");
  assert.equal(rank("auth", tree)[0], "internal/webapp/auth.go");
  assert.equal(rank("nav", tree)[0], "internal/webapp/frontend/src/nav.ts");
});

test("a name match is highlighted where it is, not where its letters first occur", () => {
  assert.deepEqual(marked("auth", "internal/webapp/auth.go"), ["auth"]);
});

// A space is just another character to a subsequence walk, and a path never
// contains one, so two words would otherwise match nothing at all.
test("a two-word query finds the file, first", () => {
  for (const [query, want] of [
    ["install guide", "docs/install-guide.md"],
    ["getting started", "docs/getting-started.md"],
    ["hub config", "specs/hub-config.md"],
    ["webapp components", "webapp/frontend/src/components/FileView.tsx"],
  ] as const) {
    assert.equal(rank(query)[0], want, query);
  }
});

test("token order is irrelevant", () => {
  const items = [prepare(TREE[3])];
  assert.equal(search("webapp components", items).hits[0].score, search("components webapp", items).hits[0].score);
});

test("every token must match — AND, not OR", () => {
  assert.equal(rank("install zzz").length, 0);
});

test("one typo per query is forgiven", () => {
  for (const [query, meant] of [
    ["fileveiw", "fileview"], // transposition
    ["insatll", "install"],
    ["onbaording", "onboarding"],
    ["launch ideas", "launch idea"], // a plural is a typo of the singular
  ] as const) {
    assert.ok(rank(meant).length > 0, meant);
    assert.equal(rank(query)[0], rank(meant)[0], query);
  }
  assert.equal(rank("insatll gudie").length, 0, "two typos are not one");
});

test("short tokens stay strict — one error on 4 characters matches nearly everything", () => {
  const tree = ["webapp/frontend/src/components/FileView.tsx"];
  assert.equal(rank("zile", tree).length, 0);
  assert.equal(rank("zilev", tree).length, 1);
});

test("a forgiven match never outranks a clean one", () => {
  // instal.md is one dropped letter away, and is not shown: the query
  // already found the file it names.
  assert.deepEqual(rank("install", ["notes/instal.md", "notes/install.md"]), ["notes/install.md"]);
  // Nothing clean: the misspelled name is all there is, and it is found.
  assert.deepEqual(rank("insatll", ["notes/install.md"]), ["notes/install.md"]);
});

test("a forgiven file name outranks letters scattered across a path", () => {
  const tree = ["xxf/xxi/xxl/xxe/xxv/xxe/xxi/xxw.md", "webapp/frontend/src/components/FileView.tsx"];
  assert.equal(rank("fileveiw", tree)[0], "webapp/frontend/src/components/FileView.tsx");
});

test("a query that found its file is not padded out with lookalikes", () => {
  const tree = ["src/palette.tsx", ...Array.from({ length: 12 }, (_, i) => `notes/palete-${i}.md`)];
  assert.deepEqual(rank("palette", tree), ["src/palette.tsx"]);
  // With nothing clean, a handful of the best guesses, not all twelve.
  assert.equal(rank("pallete", tree.slice(1)).length, 5);
});

test("a query that is the file name beats a longer name that starts with it", () => {
  const r = rank("home", ["packaging/homebrew", "src/components/Home.tsx", "src/lib/home.ts"]);
  assert.deepEqual(r.slice(0, 2).sort(), ["src/components/Home.tsx", "src/lib/home.ts"]);
});

test("one letter prefers a word start", () => {
  assert.equal(rank("g", ["notes/lg.md", "notes/guide.md"])[0], "notes/guide.md");
});

test("ranges are sorted, merged, and cover what was typed", () => {
  assert.deepEqual(marked("guide", "docs/install-guide.md"), ["guide"]);
  assert.deepEqual(marked("components webapp", TREE[3]), ["webapp", "components"]);
});

// macOS can store a 한글 name decomposed (NFD); a browser types it composed.
test("a decomposed name matches a composed query, and highlights its own characters", () => {
  const label = "회의록/주간회의.md".normalize("NFD");
  const parts = marked("회의", label);
  assert.deepEqual(
    parts.map((p) => p.normalize("NFC")),
    ["회의"],
  );
});

// "İ".toLowerCase() is two code units; an index computed on the lowercased
// copy would land one character off in the label the highlight paints.
test("a character whose lowercase is longer keeps the highlight in step", () => {
  assert.deepEqual(marked("deas", "sec13/İdeas.md"), ["deas"]);
});

test("nameOnly keeps every token inside the file name", () => {
  const tree = ["notes/deep/topic.md", "topics/index.md"];
  const names = (q: string) => search(q, tree.map(prepare), { nameOnly: true }).hits.map((h) => tree[h.index]);
  assert.deepEqual(names("topic"), ["notes/deep/topic.md"]);
  assert.deepEqual(names("notes topic"), []);
  assert.deepEqual(rank("notes topic", tree), ["notes/deep/topic.md"]);
});

test("total counts every match, not just the rows returned", () => {
  const tree = Array.from({ length: 60 }, (_, i) => `notes/day-${i}.md`);
  const r = search("day", tree.map(prepare), { limit: 10 });
  assert.equal(r.hits.length, 10);
  assert.equal(r.total, 60);
});
