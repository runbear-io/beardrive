import { test } from "node:test";
import assert from "node:assert/strict";
import {
  actionItems,
  isMeetingNote,
  offlineEditable,
  itemsMarkdown,
  openItems,
  previousNote,
  sameName,
  sections,
  sharpShrink,
  summary,
  wordCount,
} from "./meeting.ts";

const NOTE = `# Daily — 2026-10-16

## Snow
- Konny - Briana
  - [ ] Review triage issues @Mino

## Liam

## Discussions
- [ ] Reddit log @Bobb
- [x] Cost dashboard @Isac
\`\`\`
## not a heading
- [ ] not a task either? it is a line in a fence
\`\`\`
`;

test("isMeetingNote: any daily-meetings folder, markdown only", () => {
  assert.equal(isMeetingNote("areas/team/daily-meetings/2026-10/2026-10-16.md"), true);
  assert.equal(isMeetingNote("daily-meetings/x.md"), true);
  assert.equal(isMeetingNote("daily-meetings/x.txt"), false);
  assert.equal(isMeetingNote("notes/daily-meetings.md"), false);
});

test("sections: ## headings, bodies, fences ignored", () => {
  const s = sections(NOTE);
  assert.deepEqual(
    s.map((x) => x.name),
    ["Snow", "Liam", "Discussions"],
  );
  assert.equal(s[1].body, "");
  assert.match(s[0].body, /Konny/);
  assert.equal(NOTE.slice(s[0].from, s[0].headingEnd), "## Snow");
  assert.ok(NOTE.slice(s[2].from, s[2].to).includes("## not a heading"));
  assert.equal(summary(s[0]), "Konny - Briana · Review triage issues @Mino");
});

test("sections end at an h1", () => {
  const s = sections("## A\nx\n# Top\ny\n## B\nz");
  assert.equal(s.length, 2);
  assert.equal(s[0].body, "x");
});

test("actionItems: owner, done, section", () => {
  const a = actionItems(NOTE);
  assert.equal(a.length, 4); // the fenced task line is still a task line
  assert.deepEqual(a[0], { text: "Review triage issues", owner: "Mino", done: false, section: "Snow", line: 5 });
  assert.equal(a[2].done, true);
  assert.equal(a[2].section, "Discussions");
  assert.equal(itemsMarkdown(a.slice(1, 3)), "- [ ] Reddit log @Bobb\n- [x] Cost dashboard @Isac");
});

test("openItems: unchecked only, trimmed", () => {
  assert.deepEqual(openItems("- [ ] a @x\n  - [ ] b\n- [x] c\ntext"), ["- [ ] a @x", "- [ ] b"]);
});

test("previousNote: latest earlier date across month folders", () => {
  const root = "areas/team/daily-meetings/";
  const files = [
    root + "2026-09/2026-09-30.md",
    root + "2026-10/2026-10-01.md",
    root + "2026-10/2026-10-03.md",
    root + "2026-10/2026-10-02.md",
    "other/2026-10-02.md",
  ];
  assert.equal(previousNote(root + "2026-10/2026-10-03.md", files), root + "2026-10/2026-10-02.md");
  assert.equal(previousNote(root + "2026-10/2026-10-01.md", files), root + "2026-09/2026-09-30.md");
  assert.equal(previousNote(root + "2026-09/2026-09-30.md", files), null);
  assert.equal(previousNote("notes/2026-10-03.md", files), null);
});

test("sharpShrink mirrors the hub", () => {
  assert.equal(sharpShrink(10, 0), true);
  assert.equal(sharpShrink(0, 0), false);
  assert.equal(sharpShrink(100, 1), false); // under the ratio floor
  assert.equal(sharpShrink(2006, 400), true);
  assert.equal(sharpShrink(2006, 402), false);
});

test("wordCount and sameName", () => {
  assert.equal(wordCount("## Snow\n- a b — c"), 4);
  assert.equal(sameName("Liam", "Liam Hwang"), true);
  assert.equal(sameName("liam hwang", "Liam  Hwang"), true);
  assert.equal(sameName("Snow", "Liam"), false);
});

test("offline, only your own section is editable — and nothing when you have none", () => {
  const note = "# Daily\nintro\n\n## Snow\n- shipped\n\n## Mino\n- reviewed\n";
  const secs = sections(note);
  const at = (needle: string): [number, number] => {
    const i = note.indexOf(needle);
    return [i, i + 1];
  };
  assert.equal(offlineEditable(secs, "Snow", [at("shipped")]), true);
  assert.equal(offlineEditable(secs, "Snow", [at("reviewed")]), false);
  assert.equal(offlineEditable(secs, "Snow", [at("intro")]), false);
  // Only another attendee's section, none of mine: nothing is editable.
  const only = "## Mino\n- reviewed\n";
  assert.equal(offlineEditable(sections(only), "Snow", [[only.indexOf("reviewed"), only.indexOf("reviewed") + 1]]), false);
});
