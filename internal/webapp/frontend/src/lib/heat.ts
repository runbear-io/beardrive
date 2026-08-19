// Read-heat arithmetic: how a /heat entry becomes a total, a sentence, a dot
// level, or a bar split. Pure, no React — so every surface that shows read
// counts (file header, folder listing, Dashboard) shares one total and they
// can never disagree about arithmetic. Unit-tested in heat.test.ts
// (`npm test`); re-exported from hooks/useBrowse.ts for existing importers.

import type { HeatEntry, HeatMap } from "../api/types";

/* Heat for one listing entry: a file's own bucket, or the subtree sum for a
   folder. Null when there is nothing to show. */
export function heatFor(heatMap: HeatMap | null, path: string, isDir: boolean): HeatEntry | null {
  if (!heatMap) return null;
  if (!isDir) return heatMap[path] || null;
  const agg = { human: 0, agent: 0, share: 0 };
  for (const [p, e] of Object.entries(heatMap)) {
    if (!p.startsWith(path + "/")) continue;
    agg.human += e.human || 0;
    agg.agent += e.agent || 0;
    agg.share += e.share || 0;
  }
  return agg.human || agg.agent || agg.share ? agg : null;
}

export function heatTotal(e: HeatEntry): number {
  return (e.human || 0) + (e.agent || 0) + (e.share || 0);
}

/* The total mixes three kinds of reader with independent debounces: your own
   revisits fold into one visit, but an agent or a share-link hit on the
   same file still moves the number. Break it out whenever more than people
   are reading, so a count that changed while you watched says who it
   counted — an unexplained total reads as a double-count. */
export function heatText(e: HeatEntry): string {
  const total = heatTotal(e);
  if (!total) return "";
  const s = total + (total === 1 ? " read" : " reads");
  if (!e.agent && !e.share) return s;
  const parts: string[] = [];
  if (e.human) parts.push(e.human + " human");
  if (e.agent) parts.push(e.agent + " agent");
  if (e.share) parts.push(e.share + " shared");
  return s + " (" + parts.join(", ") + ")";
}

/* Every surface that prints a read count prints this beside it. Defined here,
   next to the arithmetic, so four components cannot drift into four different
   promises about what the number counts. A member browsing their own project
   is a reader like any other — the count says so rather than quietly
   including them. */
export const HEAT_DISCLOSURE =
  "Includes your own views. Repeat opens by the same reader inside 10 minutes count once.";

/* Dot intensity 1–4, log-ish steps: 1–2, 3–9, 10–29, 30+ reads. */
export function heatLevel(e: HeatEntry): number {
  const total = heatTotal(e);
  if (!total) return 0;
  if (total < 3) return 1;
  if (total < 10) return 2;
  if (total < 30) return 3;
  return 4;
}

/* The Dashboard hot-path bar's split, as fractions of heatTotal summing to 1
   (all zero for an unread path). Each reader gets its own fraction from its
   own count — painting "everything that isn't agent" as human would show a
   share-only file as if a person had browsed the hub. */
export function hotPathSplit(e: HeatEntry): { agent: number; human: number; share: number } {
  const total = heatTotal(e);
  if (!total) return { agent: 0, human: 0, share: 0 };
  return {
    agent: (e.agent || 0) / total,
    human: (e.human || 0) / total,
    share: (e.share || 0) / total,
  };
}

/* Heat rows for paths the tree no longer has. A deleted or renamed file keeps
   its read history, and dropping it is how "the doc everyone read just
   vanished" became invisible — the Dashboard joined heat onto the file tree
   and silently discarded whatever didn't match. */
export function orphanPaths(heatMap: HeatMap | null, known: Set<string>): string[] {
  if (!heatMap) return [];
  return Object.keys(heatMap)
    .filter((p) => !known.has(p))
    .sort();
}

/* Freshness-scale helpers for the Dashboard treemap legend.
   Pure and dependency-free so they can be unit-tested (Insights.tsx can't —
   node's test runner doesn't do JSX). */

// The treemap colours files by age over a fixed 0..300d scale, so it moves
// ~1% per 3 days. Below this spread the gradient is visually one colour and
// the legend says so instead of implying signal the data doesn't carry.
export const FLAT_AGE_SPREAD = 7; // days

/* Observed age span of a set of files; null for an empty scope. Loops rather
   than Math.min(...arr) — a project can hold more files than the spread
   operator has argument slots. */
export function ageRange(days: number[]): { min: number; max: number } | null {
  if (!days.length) return null;
  let min = days[0],
    max = days[0];
  for (const d of days) {
    if (d < min) min = d;
    if (d > max) max = d;
  }
  return { min, max };
}

export const isFlatRange = (min: number, max: number) => max - min < FLAT_AGE_SPREAD;

/* "0–3d" / "2d" — a rounded span, collapsed when both ends round alike. */
export function ageSpanLabel(min: number, max: number): string {
  const a = Math.round(min),
    b = Math.round(max);
  return a === b ? `${a}d` : `${a}–${b}d`;
}

/* ---- scatter dot labels ---- */

// Only the danger quadrant gets named: it is the one the panel exists to
// surface, and a label per dot at hundreds of files is unreadable.
export const LABEL_MAX = 6;
const LABEL_LH = 11; // line height at the 11px label size = the collision gap
const LABEL_CH = 5.5; // ~average glyph width; only picks which side of the dot

export interface Dot {
  path: string;
  reads: number;
  cx: number;
  cy: number;
  r: number;
}
export interface PlacedLabel {
  path: string;
  name: string;
  x: number;
  y: number;
  anchor: "start" | "end";
}

/* Basenames for the busiest danger dots, placed beside their dot: to the
   right, flipped left when the text would leave the frame, and stacked down
   (up, if down runs out of frame) when two would collide. Pure so it can be
   unit-tested — Insights.tsx can't be, see above. */
export function placeLabels(
  dots: Dot[],
  bounds: { right: number; top: number; bottom: number },
): PlacedLabel[] {
  const out: PlacedLabel[] = [];
  for (const d of [...dots].sort((a, b) => b.reads - a.reads).slice(0, LABEL_MAX)) {
    const name = d.path.split("/").pop()!;
    let x = d.cx + d.r + 4,
      anchor: "start" | "end" = "start";
    if (x + name.length * LABEL_CH > bounds.right) {
      x = d.cx - d.r - 4;
      anchor = "end";
    }
    const free = (v: number) => out.every((o) => Math.abs(o.y - v) >= LABEL_LH);
    let y = d.cy;
    while (y <= bounds.bottom && !free(y)) y += LABEL_LH;
    if (y > bounds.bottom) {
      y = d.cy;
      while (y >= bounds.top && !free(y)) y -= LABEL_LH;
    }
    out.push({ path: d.path, name, x, y: Math.min(bounds.bottom, Math.max(bounds.top, y)), anchor });
  }
  return out;
}
