// Line-level diff between two versions of a file. Pure, no React, no
// dependency: an LCS plus a patience-style anchor split is less code than
// auditing a diff package would be. Unit-tested in diff.test.ts (`npm test`).

export type DiffOp = "=" | "+" | "-";

export interface DiffLine {
  op: DiffOp;
  line: string;
  an?: number; // 1-based line number on the old side
  bn?: number; // 1-based line number on the new side
}

// splitLines treats a trailing newline as ending the last line, not as
// starting an empty one — so "a\n" and "a" are both one line, and only the
// no-trailing-newline case differs from a file that has one.
export function splitLines(text: string): string[] {
  if (text === "") return [];
  const lines = text.split("\n");
  if (lines[lines.length - 1] === "") lines.pop();
  return lines;
}

// Building the LCS table is O(n·m) in time and memory. A range past this many
// cells is first split on patience anchors (below); only a piece with no
// anchor at all that is still this big is reported coarsely, as its own lines
// removed then added — never the whole file.
const CELL_BUDGET = 4_000_000;

export function lcsDiff(a: string[], b: string[]): DiffLine[] {
  const out: DiffLine[] = [];
  diffRange(a, 0, a.length, b, 0, b.length, out);
  return out;
}

// Diffs a[aLo:aHi] against b[bLo:bHi] onto `out`. Equal prefix and suffix are
// the common case — a one-line edit in a 50,000-line file — and trimming them
// first is O(n) and leaves the expensive part a handful of lines.
function diffRange(a: string[], aLo: number, aHi: number, b: string[], bLo: number, bHi: number, out: DiffLine[]) {
  while (aLo < aHi && bLo < bHi && a[aLo] === b[bLo]) {
    out.push({ op: "=", line: a[aLo], an: aLo + 1, bn: bLo + 1 });
    aLo++;
    bLo++;
  }
  let s = 0;
  while (aHi - s > aLo && bHi - s > bLo && a[aHi - 1 - s] === b[bHi - 1 - s]) s++;
  diffMiddle(a, aLo, aHi - s, b, bLo, bHi - s, out);
  for (let k = 0; k < s; k++) {
    const ai = aHi - s + k;
    out.push({ op: "=", line: a[ai], an: ai + 1, bn: bHi - s + k + 1 });
  }
}

// The middle, which shares neither its first nor its last line.
function diffMiddle(a: string[], aLo: number, aHi: number, b: string[], bLo: number, bHi: number, out: DiffLine[]) {
  const n = aHi - aLo;
  const m = bHi - bLo;
  const del = (i: number) => out.push({ op: "-", line: a[i], an: i + 1 });
  const add = (j: number) => out.push({ op: "+", line: b[j], bn: j + 1 });
  if (n === 0 || m === 0) {
    for (let i = aLo; i < aHi; i++) del(i);
    for (let j = bLo; j < bHi; j++) add(j);
    return;
  }
  if (n * m <= CELL_BUDGET) return lcsTable(a, aLo, aHi, b, bLo, bHi, out);

  // Too big for a table: split on the lines that occur exactly once on each
  // side, in an order both sides agree on. Each gap between two anchors is
  // diffed on its own (recursing, so a big gap is split on ITS unique lines),
  // and the anchors themselves are context. Edits two screens apart in a
  // long file therefore cost two small gaps, not one giant table.
  const anchors = uniqueAnchors(a, aLo, aHi, b, bLo, bHi);
  if (anchors.length === 0) {
    // Nothing to hang the alignment on (all repeats, or nothing shared): this
    // piece, and only this piece, is reported as a replacement.
    for (let i = aLo; i < aHi; i++) del(i);
    for (let j = bLo; j < bHi; j++) add(j);
    return;
  }
  let pa = aLo;
  let pb = bLo;
  for (const [i, j] of anchors) {
    diffRange(a, pa, i, b, pb, j, out);
    out.push({ op: "=", line: a[i], an: i + 1, bn: j + 1 });
    pa = i + 1;
    pb = j + 1;
  }
  diffRange(a, pa, aHi, b, pb, bHi, out);
}

function lcsTable(a: string[], aLo: number, aHi: number, b: string[], bLo: number, bHi: number, out: DiffLine[]) {
  const n = aHi - aLo;
  const m = bHi - bLo;
  // L[i][j] = length of the LCS of a[aLo+i:aHi] and b[bLo+j:bHi].
  const L: Uint32Array[] = [];
  for (let i = 0; i <= n; i++) L.push(new Uint32Array(m + 1));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      L[i][j] = a[aLo + i] === b[bLo + j] ? L[i + 1][j + 1] + 1 : Math.max(L[i + 1][j], L[i][j + 1]);
    }
  }
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[aLo + i] === b[bLo + j]) {
      out.push({ op: "=", line: a[aLo + i], an: aLo + i + 1, bn: bLo + j + 1 });
      i++;
      j++;
    } else if (L[i + 1][j] >= L[i][j + 1]) {
      out.push({ op: "-", line: a[aLo + i], an: aLo + i + 1 });
      i++;
    } else {
      out.push({ op: "+", line: b[bLo + j], bn: bLo + j + 1 });
      j++;
    }
  }
  for (; i < n; i++) out.push({ op: "-", line: a[aLo + i], an: aLo + i + 1 });
  for (; j < m; j++) out.push({ op: "+", line: b[bLo + j], bn: bLo + j + 1 });
}

// The patience step: pairs (i, j) of lines that occur exactly once in
// a[aLo:aHi] and exactly once in b[bLo:bHi] with a[i] === b[j], cut down to
// the longest run increasing on both sides (an LIS over j, taken in i order,
// O(k log k)) — so the anchors never cross and the gaps between them are
// well-formed ranges.
export function uniqueAnchors(
  a: string[],
  aLo: number,
  aHi: number,
  b: string[],
  bLo: number,
  bHi: number,
): [number, number][] {
  // Per line: occurrences in a, in b, and the (last) index on each side.
  const seen = new Map<string, [number, number, number, number]>();
  for (let i = aLo; i < aHi; i++) {
    const c = seen.get(a[i]);
    if (c) {
      c[0]++;
      c[2] = i;
    } else seen.set(a[i], [1, 0, i, -1]);
  }
  for (let j = bLo; j < bHi; j++) {
    const c = seen.get(b[j]);
    if (!c) continue; // not in a: can never anchor
    c[1]++;
    c[3] = j;
  }
  const pairs: [number, number][] = [];
  for (let i = aLo; i < aHi; i++) {
    const c = seen.get(a[i])!;
    if (c[0] === 1 && c[1] === 1) pairs.push([i, c[3]]);
  }
  if (pairs.length === 0) return pairs;
  // tails[k] = index into pairs of the smallest-j end of an increasing run of
  // length k+1; prev links each pair to the one before it in its run.
  const tails: number[] = [];
  const prev = new Int32Array(pairs.length).fill(-1);
  for (let p = 0; p < pairs.length; p++) {
    const j = pairs[p][1];
    let lo = 0;
    let hi = tails.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (pairs[tails[mid]][1] < j) lo = mid + 1;
      else hi = mid;
    }
    if (lo > 0) prev[p] = tails[lo - 1];
    tails[lo] = p;
  }
  const run: [number, number][] = [];
  for (let p = tails[tails.length - 1]; p !== -1; p = prev[p]) run.push(pairs[p]);
  return run.reverse();
}

// diffText is the whole job: two blobs of text in, the rendered lines and
// the +N −M counts out.
export function diffText(prev: string, next: string): { lines: DiffLine[]; add: number; del: number } {
  const lines = lcsDiff(splitLines(prev), splitLines(next));
  return {
    lines,
    add: lines.filter((l) => l.op === "+").length,
    del: lines.filter((l) => l.op === "-").length,
  };
}

/* The smallest single replacement that turns `prev` into `next`: whatever the
   two share at the front and at the back is left alone.

   Coarser than the line diff above — one range, not a script — and that is the
   point. It is applied to a LIVE document somebody may have a cursor in, and
   every character it does not touch is a cursor, a selection and a remote
   caret that does not move. Replacing the whole document would move all of
   them. Offsets are UTF-16 code units, which is what both Y.Text and
   CodeMirror count in. */
export function textEdit(
  prev: string,
  next: string,
): { from: number; to: number; insert: string } | null {
  if (prev === next) return null;
  const max = Math.min(prev.length, next.length);
  let p = 0;
  while (p < max && prev[p] === next[p]) p++;
  // Bounded by what the prefix left, so the two never overlap on the shorter
  // string — which would produce a range running backwards.
  let s = 0;
  while (s < max - p && prev[prev.length - 1 - s] === next[next.length - 1 - s]) s++;
  return { from: p, to: prev.length - s, insert: next.slice(p, next.length - s) };
}
