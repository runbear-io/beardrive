import type { HistoryEntry } from "../api/types";
import type { HistoryFilters } from "../router";

/* ---- Activity feed ----
   Pure helpers for the Activity view, no React: date windows in the reader's
   own zone, the API query a filter set becomes, and day labels. Unit-tested
   on node (`npm test`). */

export const RANGE_LABEL: Record<string, string> = {
  today: "Today",
  "7d": "Last 7 days",
  "30d": "Last 30 days",
};
const RANGE_DAYS: Record<string, number> = { today: 1, "7d": 7, "30d": 30 };

const p2 = (n: number) => String(n).padStart(2, "0");

// A local calendar day as YYYY-MM-DD — the reader's day, not UTC's.
export function localDay(d: Date): string {
  return d.getFullYear() + "-" + p2(d.getMonth() + 1) + "-" + p2(d.getDate());
}

function parseDay(s: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s);
  if (!m) return null;
  const d = new Date(+m[1], +m[2] - 1, +m[3]);
  return d.getMonth() === +m[2] - 1 ? d : null;
}

/* The instants a filter set covers. A preset counts whole local days back
   from today; custom since/until are local days, inclusive at both ends.
   The server takes RFC3339 instants, so the reader's midnight — not UTC's —
   is where a day starts. */
export function dateWindow(f: HistoryFilters | undefined, now = new Date()): { since?: Date; until?: Date } {
  const days = f?.range ? RANGE_DAYS[f.range] : undefined;
  if (days) return { since: new Date(now.getFullYear(), now.getMonth(), now.getDate() - days + 1) };
  const out: { since?: Date; until?: Date } = {};
  const s = f?.since ? parseDay(f.since) : null;
  const u = f?.until ? parseDay(f.until) : null;
  if (s) out.since = s;
  if (u) out.until = new Date(u.getFullYear(), u.getMonth(), u.getDate() + 1, 0, 0, 0, -1);
  return out;
}

// The history API's query for a filter set ("&k=v…", or "").
export function historyApiQuery(f: HistoryFilters | undefined, now = new Date()): string {
  const p = new URLSearchParams();
  for (const k of ["q", "user", "actor", "device"] as const) if (f?.[k]) p.set(k, f[k]!);
  const w = dateWindow(f, now);
  if (w.since) p.set("since", w.since.toISOString());
  if (w.until) p.set("until", w.until.toISOString());
  const s = p.toString();
  return s ? "&" + s : "";
}

// How many days the summary chart covers for a filter set with no custom
// bounds; custom since/until name the days themselves.
export function summaryDays(f: HistoryFilters | undefined): number {
  return (f?.range && RANGE_DAYS[f.range]) || 7;
}

// What the summary header calls its window.
export function summaryLabel(f: HistoryFilters | undefined): string {
  if (f?.range && RANGE_LABEL[f.range]) return RANGE_LABEL[f.range];
  return f?.since || f?.until ? "Custom range" : "Last 7 days";
}

// "Today", "Yesterday", or the reader's own date format.
export function dayLabel(day: string, now = new Date()): string {
  if (day === localDay(now)) return "Today";
  if (day === localDay(new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1))) return "Yesterday";
  const d = parseDay(day);
  return d ? d.toLocaleDateString([], { weekday: "long", month: "short", day: "numeric" }) : day;
}

/* What a change's diff compares against: the predecessor's sha when it is
   known, "" when there is genuinely none (a first version, diffed against
   an empty file), undefined when one exists but nobody named it — which
   must show no count rather than claim every line was added. */
export function diffBase(kind: string, prev: string | undefined): string | undefined {
  if (prev) return prev;
  return kind === "add" ? "" : undefined;
}

// An agent's change carries the session the sync hook stamped on it.
export function isAgent(e: Pick<HistoryEntry, "session">): boolean {
  return !!e.session;
}

/* Who made a run, as a reader names it: the hook's note reads
   "<label> session <id>", and the label is the agent ("claude-code" →
   "Claude Code"). Anything else is just "Agent". */
export function agentName(note: string | undefined): string {
  const m = /^([a-z0-9][a-z0-9-]*) session \S+$/i.exec((note || "").trim());
  if (!m) return "Agent";
  return m[1]
    .split("-")
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join(" ");
}

// "8 min", "1 h 5 min", or "" for a single instant.
export function spanLabel(ms: number): string {
  const min = Math.round(ms / 60_000);
  if (min < 1) return "";
  if (min < 60) return min + " min";
  return Math.floor(min / 60) + " h" + (min % 60 ? " " + (min % 60) + " min" : "");
}
