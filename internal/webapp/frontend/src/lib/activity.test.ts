import { test } from "node:test";
import assert from "node:assert/strict";
import {
  agentName,
  dateWindow,
  lastNanoBefore,
  dayLabel,
  diffBase,
  historyApiQuery,
  localDay,
  spanLabel,
  isAllTime,
  summaryDays,
  summaryLabel,
} from "./activity.ts";

const now = new Date(2026, 9, 16, 15, 30); // local 16 Oct 2026

test("presets count whole local days back from today", () => {
  assert.equal(localDay(dateWindow({ range: "today" }, now).since!), "2026-10-16");
  assert.equal(localDay(dateWindow({ range: "7d" }, now).since!), "2026-10-10");
  assert.equal(dateWindow({ range: "7d" }, now).since!.getHours(), 0);
  assert.equal(localDay(dateWindow({ range: "30d" }, now).since!), "2026-09-17");
  assert.equal(dateWindow({ range: "7d" }, now).until, undefined);
});

test("custom days are local and inclusive at both ends", () => {
  const w = dateWindow({ since: "2026-07-01", until: "2026-07-02" }, now);
  assert.equal(w.since!.getTime(), new Date(2026, 6, 1).getTime());
  assert.equal(w.until!.getTime(), new Date(2026, 6, 3).getTime());
  assert.deepEqual(dateWindow({ since: "nope" }, now), {});
});

test("the API query carries instants, not bare dates", () => {
  const q = historyApiQuery({ q: "a", actor: "agent", device: "d1", range: "today" }, now);
  const p = new URLSearchParams(q.slice(1));
  assert.equal(p.get("q"), "a");
  assert.equal(p.get("actor"), "agent");
  assert.equal(p.get("device"), "d1");
  assert.equal(new Date(p.get("since")!).getTime(), new Date(2026, 9, 16).getTime());
  assert.equal(p.get("range"), null);
  assert.equal(historyApiQuery(undefined, now), "");
});

test("labels", () => {
  assert.equal(dayLabel("2026-10-16", now), "Today");
  assert.equal(dayLabel("2026-10-15", now), "Yesterday");
  assert.equal(agentName("claude-code session 8f21e4"), "Claude Code");
  assert.equal(agentName("expanded the guide"), "Agent");
  assert.equal(spanLabel(8 * 60_000), "8 min");
  assert.equal(spanLabel(65 * 60_000), "1 h 5 min");
  assert.equal(spanLabel(0), "");
  assert.equal(summaryDays({ range: "30d" }), 30);
  assert.equal(summaryDays({}), 30);
});

test("the summary names a custom range as one", () => {
  assert.equal(summaryLabel({ range: "7d" }), "Last 7 days");
  assert.equal(summaryLabel({ since: "2026-01-01", until: "2026-01-03" }), "Custom range");
  assert.equal(summaryLabel({ until: "2026-01-03" }), "Custom range");
  assert.equal(summaryLabel({}), "All time");
  assert.equal(isAllTime({}), true);
  assert.equal(isAllTime({ range: "7d" }), false);
  assert.equal(isAllTime({ since: "2026-01-01" }), false);
});

test("a diff compares against an empty file only when there was no earlier version", () => {
  assert.equal(diffBase("edit", "abc"), "abc");
  assert.equal(diffBase("add", undefined), "");
  assert.equal(diffBase("add", "abc"), "abc"); // re-added after a delete
  assert.equal(diffBase("edit", undefined), undefined);
  assert.equal(diffBase("delete", undefined), undefined);
});

test("until names the last nanosecond of the day, not the last millisecond", () => {
  assert.equal(lastNanoBefore(new Date("2026-07-03T00:00:00.000Z")), "2026-07-02T23:59:59.999999999Z");
});
