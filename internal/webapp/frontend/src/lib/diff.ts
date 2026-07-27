// Line-level diff between two versions of a file. Pure, no React, no
// dependency: an LCS is ~40 lines, which is less code than auditing a diff
// package would be. Unit-tested in diff.test.ts (`npm test`).

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

// Building the LCS table is O(n·m) in time and memory. Beyond this many
// cells we stop and report the change coarsely instead.
// ponytail: whole-file replacement past the budget; switch to a Myers
// diff (O(nd), no table) if real files start hitting it.
const CELL_BUDGET = 4_000_000;

export function lcsDiff(a: string[], b: string[]): DiffLine[] {
  // Equal prefix and suffix are the common case — a one-line edit in a
  // 2,000-line file — and trimming them keeps the table tiny.
  let p = 0;
  while (p < a.length && p < b.length && a[p] === b[p]) p++;
  let s = 0;
  while (s < a.length - p && s < b.length - p && a[a.length - 1 - s] === b[b.length - 1 - s]) s++;

  const out: DiffLine[] = [];
  for (let i = 0; i < p; i++) out.push({ op: "=", line: a[i], an: i + 1, bn: i + 1 });

  const am = a.slice(p, a.length - s);
  const bm = b.slice(p, b.length - s);
  const n = am.length;
  const m = bm.length;
  const del = (i: number) => out.push({ op: "-", line: am[i], an: p + i + 1 });
  const add = (j: number) => out.push({ op: "+", line: bm[j], bn: p + j + 1 });

  if (n * m > CELL_BUDGET) {
    for (let i = 0; i < n; i++) del(i);
    for (let j = 0; j < m; j++) add(j);
  } else {
    // L[i][j] = length of the LCS of am[i:] and bm[j:].
    const L: Uint32Array[] = [];
    for (let i = 0; i <= n; i++) L.push(new Uint32Array(m + 1));
    for (let i = n - 1; i >= 0; i--) {
      for (let j = m - 1; j >= 0; j--) {
        L[i][j] =
          am[i] === bm[j] ? L[i + 1][j + 1] + 1 : Math.max(L[i + 1][j], L[i][j + 1]);
      }
    }
    let i = 0;
    let j = 0;
    while (i < n && j < m) {
      if (am[i] === bm[j]) {
        out.push({ op: "=", line: am[i], an: p + i + 1, bn: p + j + 1 });
        i++;
        j++;
      } else if (L[i + 1][j] >= L[i][j + 1]) {
        del(i++);
      } else {
        add(j++);
      }
    }
    while (i < n) del(i++);
    while (j < m) add(j++);
  }

  for (let k = 0; k < s; k++) {
    const ai = a.length - s + k;
    out.push({ op: "=", line: a[ai], an: ai + 1, bn: b.length - s + k + 1 });
  }
  return out;
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
