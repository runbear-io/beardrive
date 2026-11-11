/* Three-way text merge and minimal splices. Pure, no dependency.

   merge3(base, a, b) folds two versions that both started from `base` into
   one, or says it cannot. It is what lets an editor that is NOT in the live
   room (solo, or still waiting for the room to answer) take somebody else's
   write without either refusing it or overwriting its own unsaved typing.

   Three granularities, each tried only where the coarser one collided:

     lines  -> Myers O(ND) over lines; disjoint hunks anywhere in the file
               merge, identical hunks are emitted once.
     tokens -> the same algorithm over words / whitespace runs / single
               punctuation, inside a line-level collision only.
     chars  -> used only to recognise one side CONTINUING the other's
               insertion ("x" -> "xA" vs "x" -> "xAB"); a char-level
               collision is never resolved any other way.

   Two insertions at the same anchor are deduplicated when one already holds
   the other at its start or end (the same keystrokes reaching us twice), and
   otherwise concatenated in codepoint order, so the result does not depend on
   which side is called `a`: merge3(base, a, b) === merge3(base, b, a).

   { ok: false } is the honest answer only for the same token changed two
   different ways.

   merge3PreferRight(base, a, b) is the same merge for a caller that must get
   a text back: every region still merges as above, and only the minimal
   token span both sides changed differently takes `b`'s version. */

export type Merge3Result = { ok: true; text: string } | { ok: false };

export type Splice = { index: number; deleteCount: number; insertText: string };

type Hunk = { aS: number; aE: number; bS: number; bE: number };

// Beyond this many edit steps Myers' trace grows quadratically; past it the
// changed middle is reported as one hunk instead (still correct, coarser).
const MAX_D = 4_000;

/* Myers' greedy O((N+M)·D) shortest edit script, as hunks of [aS,aE) in `a`
   replaced by [bS,bE) of `b`. Inputs are interned ids, so a comparison is an
   integer compare whatever the token. */
function myers(a: Int32Array, b: Int32Array): Hunk[] {
  let p = 0;
  while (p < a.length && p < b.length && a[p] === b[p]) p++;
  let s = 0;
  while (s < a.length - p && s < b.length - p && a[a.length - 1 - s] === b[b.length - 1 - s]) s++;
  const n = a.length - p - s;
  const m = b.length - p - s;
  if (n === 0 && m === 0) return [];
  if (n === 0 || m === 0) return [{ aS: p, aE: p + n, bS: p, bE: p + m }];
  const A = a.subarray(p, p + n);
  const B = b.subarray(p, p + m);

  const max = Math.min(n + m, MAX_D);
  const trace: Int32Array[] = [];
  let found = -1;
  for (let d = 0; d <= max && found < 0; d++) {
    const prev = d > 0 ? trace[d - 1] : null;
    const v = new Int32Array(2 * d + 1);
    for (let k = -d; k <= d; k += 2) {
      let x: number;
      if (d === 0) x = 0;
      else if (k === -d || (k !== d && prev![k - 1 + d - 1] < prev![k + 1 + d - 1])) x = prev![k + 1 + d - 1];
      else x = prev![k - 1 + d - 1] + 1;
      let y = x - k;
      while (x < n && y < m && A[x] === B[y]) {
        x++;
        y++;
      }
      v[k + d] = x;
      if (x >= n && y >= m) {
        found = d;
        break;
      }
    }
    trace.push(v);
  }
  if (found < 0) return [{ aS: p, aE: p + n, bS: p, bE: p + m }];

  // Walk back collecting the matched diagonals, then report the gaps.
  const matches: number[] = []; // pairs (x, y), reversed
  let x = n;
  let y = m;
  for (let d = found; d > 0; d--) {
    const prev = trace[d - 1];
    const k = x - y;
    const pk = k === -d || (k !== d && prev[k - 1 + d - 1] < prev[k + 1 + d - 1]) ? k + 1 : k - 1;
    const px = prev[pk + d - 1];
    const py = px - pk;
    // The snake after the edit step.
    const sx = pk === k + 1 ? px : px + 1;
    const sy = pk === k + 1 ? py + 1 : py;
    while (x > sx && y > sy) {
      x--;
      y--;
      matches.push(x, y);
    }
    x = px;
    y = py;
  }
  while (x > 0 && y > 0) {
    x--;
    y--;
    matches.push(x, y);
  }

  const out: Hunk[] = [];
  let ai = 0;
  let bi = 0;
  for (let i = matches.length - 2; i >= 0; i -= 2) {
    const mx = matches[i];
    const my = matches[i + 1];
    if (mx > ai || my > bi) out.push({ aS: p + ai, aE: p + mx, bS: p + bi, bE: p + my });
    ai = mx + 1;
    bi = my + 1;
  }
  if (ai < n || bi < m) out.push({ aS: p + ai, aE: p + n, bS: p + bi, bE: p + m });
  return out;
}

// Lines keep their "\n", so joining tokens always reproduces the text exactly.
const LINE_RE = /[^\n]*\n|[^\n]+$/g;
// A word, a whitespace run, or one other character (a surrogate pair is one).
const TOKEN_RE = /\s+|[\p{L}\p{N}_]+|[^\s\p{L}\p{N}_]/gu;

const LINES = 0;
const TOKENS = 1;
const CHARS = 2;
type Level = typeof LINES | typeof TOKENS | typeof CHARS;

function split(text: string, level: Level): string[] {
  if (text === "") return [];
  if (level === LINES) return text.match(LINE_RE) ?? [text];
  if (level === TOKENS) return text.match(TOKEN_RE) ?? [text];
  return Array.from(text);
}

function intern(toks: string[], ids: Map<string, number>): Int32Array {
  const out = new Int32Array(toks.length);
  for (let i = 0; i < toks.length; i++) {
    let id = ids.get(toks[i]);
    if (id === undefined) {
      id = ids.size;
      ids.set(toks[i], id);
    }
    out[i] = id;
  }
  return out;
}

// Codepoint order, not UTF-16 unit order: the two disagree around surrogates,
// and the hub orders the same way (Go compares UTF-8 bytes, which sort as
// codepoints).
function cmpCodepoints(x: string, y: string): number {
  const xs = Array.from(x);
  const ys = Array.from(y);
  const n = Math.min(xs.length, ys.length);
  for (let i = 0; i < n; i++) {
    const d = xs[i].codePointAt(0)! - ys[i].codePointAt(0)!;
    if (d) return d;
  }
  return xs.length - ys.length;
}

/* Two insertions at one anchor, matching Go textmerge.Merge. The same text
   arriving twice is one insertion. Anything else is both, in an order
   neither side gets to pick: one side appending a line the other already
   appended is two lines, since only the caller knows whether a write landed.
   Over an EMPTY ancestor the anchor is the whole file: a body that begins
   with the whole other (at a line boundary) is that file written further,
   and two different bodies are two documents, so they conflict. */
function sameAnchor(x: string, y: string, concat: boolean, emptyBase: boolean): string | null {
  if (x === y) return x;
  if (emptyBase) {
    const extends_ = (long: string, short: string) =>
      long.startsWith(short) && (short === "" || short.endsWith("\n") || long[short.length] === "\n");
    if (x.length >= y.length && extends_(x, y)) return x;
    if (y.length > x.length && extends_(y, x)) return y;
    return null;
  }
  if (!concat) return null;
  return cmpCodepoints(x, y) <= 0 ? x + y : y + x;
}

type Side = { h: Hunk; toks: string[]; side: 0 | 1 };

/* With `preferB`, a collision takes `b`'s side of the colliding region rather
   than failing the merge — at the narrowest level that still collides: lines
   descend to tokens, and a token collision the char level cannot resolve is
   `b`'s tokens for that span alone. */
function mergeAt(base: string, a: string, b: string, level: Level, preferB = false): string | null {
  if (a === b) return a;
  if (a === base) return b;
  if (b === base) return a;

  const ids = new Map<string, number>();
  const bt = split(base, level);
  const at = split(a, level);
  const btk = split(b, level);
  const baseIds = intern(bt, ids);
  const all: Side[] = [
    ...myers(baseIds, intern(at, ids)).map((h): Side => ({ h, toks: at, side: 0 })),
    ...myers(baseIds, intern(btk, ids)).map((h): Side => ({ h, toks: btk, side: 1 })),
  ];
  all.sort((x, y) => x.h.aS - y.h.aS || x.h.aE - y.h.aE || x.side - y.side);

  const join = (toks: string[], s: number, e: number) => toks.slice(s, e).join("");
  // The base region [gs, ge) as one side sees it.
  const sideText = (group: Side[], side: 0 | 1, gs: number, ge: number) => {
    let out = "";
    let pos = gs;
    for (const g of group) {
      if (g.side !== side) continue;
      out += join(bt, pos, g.h.aS) + join(g.toks, g.h.bS, g.h.bE);
      pos = g.h.aE;
    }
    return out + join(bt, pos, ge);
  };

  let out = "";
  let pos = 0;
  let i = 0;
  while (i < all.length) {
    const group = [all[i]];
    let gs = all[i].h.aS;
    let ge = all[i].h.aE;
    i++;
    // Overlapping base ranges, or two insertions at the same point, collide.
    // Merely touching ranges do not: an edit to one line and an edit to the
    // next are two separate hunks.
    while (i < all.length) {
      const h = all[i].h;
      const overlaps = h.aS < ge && gs < h.aE;
      const sharedAnchor = h.aS === h.aE && gs === ge && h.aS === gs;
      const inside = h.aS === h.aE && gs < h.aS && h.aS < ge;
      if (!overlaps && !sharedAnchor && !inside) break;
      group.push(all[i]);
      gs = Math.min(gs, h.aS);
      ge = Math.max(ge, h.aE);
      i++;
    }
    out += join(bt, pos, gs);
    pos = ge;
    const sides = new Set(group.map((g) => g.side));
    if (sides.size === 1) {
      out += sideText(group, group[0].side, gs, ge);
      continue;
    }
    const ra = sideText(group, 0, gs, ge);
    const rb = sideText(group, 1, gs, ge);
    if (ra === rb) {
      out += ra;
      continue;
    }
    if (gs === ge) {
      const s = sameAnchor(ra, rb, level !== CHARS, bt.length === 0);
      if (s === null) {
        if (!preferB) return null;
        out += rb;
        continue;
      }
      out += s;
      continue;
    }
    if (level === CHARS) return null;
    // Chars only recognise a continued insertion; they are never asked to
    // pick a side, so a token collision there falls back to `b`'s tokens.
    const sub =
      level === TOKENS
        ? mergeAt(join(bt, gs, ge), ra, rb, CHARS)
        : mergeAt(join(bt, gs, ge), ra, rb, (level + 1) as Level, preferB);
    if (sub === null) {
      if (!preferB) return null;
      out += rb;
      continue;
    }
    out += sub;
  }
  return out + join(bt, pos, bt.length);
}

/* Lines are merged as Go textmerge.Merge merges them: every line terminated,
   and the final newline decided on its own afterwards. A line unit keeps its
   "\n", so an unterminated last line would otherwise be glued onto whatever
   lands after it — "x" + "x\nx" is "xx\nx", one line where Go has two. */
const terminated = (s: string) => (s === "" || s.endsWith("\n") ? s : s + "\n");

// The final newline is one more thing a side may change: whichever side
// changed it from the base wins, and both can only change it one way.
function withTrailing(base: string, a: string, b: string, text: string): string {
  let trailing = a.endsWith("\n");
  if (trailing === base.endsWith("\n")) trailing = b.endsWith("\n");
  if (text === "" || trailing === text.endsWith("\n")) return text;
  return trailing ? text + "\n" : text.slice(0, -1);
}

export function merge3(base: string, a: string, b: string): Merge3Result {
  const text = mergeAt(terminated(base), terminated(a), terminated(b), LINES);
  if (text === null) return { ok: false };
  return { ok: true, text: withTrailing(base, a, b, text) };
}

/* merge3, except a conflict is not an answer: where `left` and `right` changed
   the same base tokens differently, `right` wins THAT span, and every other
   edit either side made — including ones inside the same line — survives. */
export function merge3PreferRight(base: string, left: string, right: string): string {
  if (left === right) return left;
  if (base === left) return right;
  if (base === right) return left;
  const text = mergeAt(terminated(base), terminated(left), terminated(right), LINES, true) ?? terminated(right);
  return withTrailing(base, left, right, text);
}

/* The splices that turn `oldText` into `newText`, touching as little as
   possible: line hunks, refined to token hunks, trimmed to the characters
   that actually differ. Every untouched character keeps its identity in a
   CRDT, and with it every cursor and remote caret sitting in it.

   Indices are UTF-16 units in OLD-text coordinates and the list runs from the
   END of the text backwards, so applying them one after another — to a
   Y.Text, a string, or as a CodeMirror change set — never invalidates the
   ones still to come. */
export function diffSplices(oldText: string, newText: string): Splice[] {
  if (oldText === newText) return [];
  const out: Splice[] = [];
  const ids = new Map<string, number>();
  const ol = split(oldText, LINES);
  const nl = split(newText, LINES);
  const offs = (toks: string[]) => {
    const o = new Array<number>(toks.length + 1);
    o[0] = 0;
    for (let i = 0; i < toks.length; i++) o[i + 1] = o[i] + toks[i].length;
    return o;
  };
  const oo = offs(ol);
  const no = offs(nl);
  for (const h of myers(intern(ol, ids), intern(nl, ids))) {
    const oldSeg = oldText.slice(oo[h.aS], oo[h.aE]);
    const newSeg = newText.slice(no[h.bS], no[h.bE]);
    refine(oldSeg, newSeg, oo[h.aS], out);
  }
  return out.reverse();
}

function refine(oldSeg: string, newSeg: string, at: number, out: Splice[]) {
  const ids = new Map<string, number>();
  const ot = split(oldSeg, TOKENS);
  const nt = split(newSeg, TOKENS);
  let oi = 0;
  let ni = 0;
  let oc = 0;
  let nc = 0;
  for (const h of myers(intern(ot, ids), intern(nt, ids))) {
    while (oi < h.aS) oc += ot[oi++].length;
    while (ni < h.bS) nc += nt[ni++].length;
    let oe = oc;
    while (oi < h.aE) oe += ot[oi++].length;
    let ne = nc;
    while (ni < h.bE) ne += nt[ni++].length;
    let del = oldSeg.slice(oc, oe);
    let ins = newSeg.slice(nc, ne);
    let from = at + oc;
    // Trim what the replaced and replacing tokens share at either end.
    let p = 0;
    while (p < del.length && p < ins.length && del[p] === ins[p]) p++;
    let s = 0;
    while (s < del.length - p && s < ins.length - p && del[del.length - 1 - s] === ins[ins.length - 1 - s]) s++;
    // Never split a surrogate pair.
    const hi = p > 0 ? del.charCodeAt(p - 1) : 0;
    if (hi >= 0xd800 && hi <= 0xdbff) p--;
    const lo = s > 0 ? del.charCodeAt(del.length - s) : 0;
    if (lo >= 0xdc00 && lo <= 0xdfff) s--;
    from += p;
    del = del.slice(p, del.length - s);
    ins = ins.slice(p, ins.length - s);
    if (del.length || ins.length) out.push({ index: from, deleteCount: del.length, insertText: ins });
    oc = oe;
    nc = ne;
  }
}

// Apply splices from diffSplices to a plain string.
export function applySplices(text: string, splices: Splice[]): string {
  for (const s of splices) text = text.slice(0, s.index) + s.insertText + text.slice(s.index + s.deleteCount);
  return text;
}

/* Whether `head` still carries every line edit that took `base` to
   `candidate`, untouched — textmerge.RetainsChanges, line for line: no later
   edit (candidate → head) overlaps a span candidate inserted or rewrote,
   inserts inside one, or puts lines back where candidate deleted them, and a
   final newline candidate changed is still the way candidate left it. */
export function retainsChanges(base: string, candidate: string, head: string): boolean {
  const ids = new Map<string, number>();
  const lc = intern(split(candidate, LINES), ids);
  const ours = myers(intern(split(base, LINES), ids), lc);
  const later = myers(lc, intern(split(head, LINES), ids));
  for (const h1 of ours) {
    for (const h2 of later) {
      if (h1.bS < h1.bE) {
        // A replacement must keep clear even of an abutting edit (the old
        // lines going back in beside it); a pure insertion only of its inside.
        if (h1.aS < h1.aE ? !(h2.aE < h1.bS || h2.aS > h1.bE) : !(h2.aE <= h1.bS || h2.aS >= h1.bE)) return false;
      } else if (h2.bS < h2.bE && h2.aS <= h1.bS && h1.bS <= h2.aE) {
        return false; // lines put back where candidate deleted
      }
    }
  }
  const nl = (s: string) => s.endsWith("\n");
  return !(nl(candidate) !== nl(base) && nl(head) !== nl(candidate));
}
