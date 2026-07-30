// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Excluded from tsconfig's include — it imports node: builtins, which the
// app's DOM-only lib set does not know about.
import { test } from "node:test";
import assert from "node:assert/strict";
import { heatFor, heatLevel, heatText, heatTotal, hotPathSplit } from "./heat.ts";
import type { HeatMap } from "../api/types.ts";

// One fixture, read by both surfaces: the file header (heatText/heatTotal) and
// the Dashboard hot-path bar (heatTotal/hotPathSplit).
const FIXTURE: HeatMap = {
  "guide.md": { human: 6, agent: 9, share: 1 },
  "notes/shared-only.md": { share: 4 },
  "notes/read-by-people.md": { human: 3 },
  "notes/untouched.md": {},
};

test("heatTotal is human + agent + share", () => {
  assert.equal(heatTotal(FIXTURE["guide.md"]), 16);
  assert.equal(heatTotal(FIXTURE["notes/shared-only.md"]), 4);
  assert.equal(heatTotal(FIXTURE["notes/untouched.md"]), 0);
});

test("heatText breaks out the same three numbers the total sums", () => {
  assert.equal(heatText(FIXTURE["guide.md"]), "16 reads (6 human, 9 agent, 1 shared)");
  assert.equal(heatText(FIXTURE["notes/shared-only.md"]), "4 reads (4 shared)");
  // People-only needs no breakout — the total is already unambiguous.
  assert.equal(heatText(FIXTURE["notes/read-by-people.md"]), "3 reads");
  assert.equal(heatText(FIXTURE["notes/untouched.md"]), "");
});

test("the file header and the hot-path bar cannot disagree about a total", () => {
  for (const [path, e] of Object.entries(FIXTURE)) {
    const total = heatTotal(e); // what the Dashboard row prints (Insights: p.total/p.reads)
    const header = heatText(e); // what the file header prints (FileView)
    if (!total) {
      assert.equal(header, "", path);
      continue;
    }
    assert.equal(Number(header.split(" ")[0]), total, path);
    // …and the bar's segments recompose to that same total, not to some
    // second sum computed at render time.
    const f = hotPathSplit(e);
    assert.equal(
      Math.round((f.agent + f.human + f.share) * total),
      total,
      path,
    );
  }
});

test("hotPathSplit gives each reader its own fraction, summing to 1", () => {
  const f = hotPathSplit(FIXTURE["guide.md"]);
  assert.equal(f.agent + f.human + f.share, 1);
  assert.equal(f.agent, 9 / 16);
  assert.equal(f.human, 6 / 16);
  assert.equal(f.share, 1 / 16);
});

test("share reads never paint as human", () => {
  const f = hotPathSplit(FIXTURE["notes/shared-only.md"]);
  assert.equal(f.human, 0); // the bug: 1 - agentFraction painted this 100% human
  assert.equal(f.share, 1);
  assert.equal(f.agent, 0);
});

test("an unread path splits to nothing rather than dividing by zero", () => {
  assert.deepEqual(hotPathSplit(FIXTURE["notes/untouched.md"]), {
    agent: 0,
    human: 0,
    share: 0,
  });
});

test("heatFor sums a folder subtree across all three readers", () => {
  const notes = heatFor(FIXTURE, "notes", true);
  assert.deepEqual(notes, { human: 3, agent: 0, share: 4 });
  assert.equal(heatTotal(notes!), 7);
  assert.equal(heatFor(FIXTURE, "guide.md", false), FIXTURE["guide.md"]);
  assert.equal(heatFor(null, "notes", true), null);
  assert.equal(heatFor({ "x/unread.md": {} }, "x", true), null);
});

test("heatLevel steps at 3, 10 and 30 reads", () => {
  assert.equal(heatLevel({}), 0);
  assert.equal(heatLevel({ human: 2 }), 1);
  assert.equal(heatLevel({ human: 1, agent: 1, share: 1 }), 2);
  assert.equal(heatLevel({ agent: 9 }), 2);
  assert.equal(heatLevel({ agent: 10 }), 3);
  assert.equal(heatLevel({ human: 15, share: 15 }), 4);
});
