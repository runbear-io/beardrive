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
