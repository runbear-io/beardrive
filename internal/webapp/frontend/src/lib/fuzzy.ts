/* Name search, for the ⌘K palette and the sidebar filter alike. Lives
   here, not in a component, so it can be unit-tested without a DOM (see
   fuzzy.test.ts). Palette.tsx keeps only rendering.

   Scoring is fzy's (github.com/jhawthorn/fzy, MIT): the best alignment of a
   token inside the label, found by dynamic programming. The greedy walk it
   replaced committed to the FIRST place each letter occurs, so "auth"
   anchored on the a in "internal/" and lost both the word-start and the
   consecutive-run bonus to it. Measured on a real 11k-file project, the file
   a person typed the name of came first 62% of the time; this ranks it first
   93% of the time, against 68–86% for fzf, uFuzzy, fuzzaldrin-plus and Fuse,
   and 54% for cmdk's own filter.

   What a path list needs on top of fzy:
   - A token that fits inside the file name is scored there, not across the
     folders above it, plus NAME_BONUS.
   - A query that IS the file name, with or without its extension, gets
     EXACT_BONUS.
   - Tokens split on whitespace. Every one must match (AND), in any order,
     and their scores add up. File paths never contain a space, so a
     two-word query would otherwise match nothing.
   - One typo per query is forgiven: a single dropped character in a token
     of MIN_TYPO_LEN or more (shorter, and one error matches nearly
     everything). Subsequence matching already tolerates a character missing
     from the label, so that one rule covers a transposition, a substitution
     and an insertion. The forgiven match pays TYPO_PENALTY. Once a clean
     match exists, a forgiven one must land in a file name AND outrank every
     clean match to be shown: letters scattered across a path lose to the
     file you misspelled, but a query that already found its file is not
     padded out with lookalikes. At most TYPO_ROWS are shown.
   - Labels are folded to NFC: macOS can store 한글 names decomposed, and
     a browser types them composed. */

export type Range = readonly [start: number, end: number]; // in the label, end exclusive

const GAP_LEADING = -0.005;
const GAP_TRAILING = -0.005;
const GAP_INNER = -0.01;
const MATCH_CONSECUTIVE = 1;
// Indexed by bonus class: none, after "/", after "-" "_" " ", after ".", camelCase.
const BONUS = [0, 0.9, 0.8, 0.6, 0.7];
const NAME_BONUS = 0.5;
const EXACT_BONUS = 2;
const TYPO_PENALTY = 1;
const TYPO_ROWS = 5;
const MIN_TYPO_LEN = 5;

/* ---- fold: lowercase + NFC, with a way back to the label ---- */

// A character that NFC may merge into the one before it: combining marks,
// and the vowel and final jamo of a decomposed Hangul syllable.
const JOINS = /[\p{M}\u1160-\u11ff\ud7b0-\ud7ff]/u;

interface Folded {
  text: string; // what is matched
  cased: string; // same length as text, case kept (for the camelCase bonus)
  from: Int32Array | null; // per text unit: where it starts in the label (null = same index)
  to: Int32Array | null; // ...and where it ends
}

function fold(s: string): Folded {
  // ASCII, or already composed with a lowercase of the same length: every
  // index carries over unchanged.
  if (!/[^\x00-\x7f]/.test(s) || s.normalize("NFC") === s) {
    const l = s.toLowerCase();
    if (l.length === s.length) return { text: l, cased: s, from: null, to: null };
  }
  let text = "",
    cased = "";
  const from: number[] = [],
    to: number[] = [];
  for (let i = 0; i < s.length; ) {
    let j = i + (s.codePointAt(i)! > 0xffff ? 2 : 1);
    while (j < s.length) {
      const cp = s.codePointAt(j)!;
      if (!JOINS.test(String.fromCodePoint(cp))) break;
      j += cp > 0xffff ? 2 : 1;
    }
    const chunk = s.slice(i, j);
    const n = chunk.normalize("NFC");
    // "İ".toLowerCase() is two code units. Keeping such a character as it is
    // keeps every index below in step with the label, which is what the
    // highlight paints from.
    let l = n.toLowerCase();
    if (l.length !== n.length) l = n;
    text += l;
    cased += n;
    // A character that took part in a merge highlights its whole cluster.
    const one = chunk.length === 1 && n.length === 1;
    for (let k = 0; k < n.length; k++) {
      from.push(i);
      to.push(one ? i + 1 : j);
    }
    i = j;
  }
  return { text, cased, from: Int32Array.from(from), to: Int32Array.from(to) };
}

function foldQuery(q: string): string {
  return q.normalize("NFC").toLowerCase().trim();
}

/* ---- prepared labels ---- */

export interface Prepared {
  readonly label: string;
  readonly text: string;
  readonly bonus: Uint8Array;
  readonly name: number; // where the last path segment starts in `text`
  readonly base: string; // that segment
  readonly stem: string; // ...without its extension
  readonly mask: number; // which of a–z (and "any digit") occur
  readonly nameMask: number; // ...in the name alone
  readonly from: Int32Array | null;
  readonly to: Int32Array | null;
}

function maskOf(s: string): number {
  let m = 0;
  for (let i = 0; i < s.length; i++) m |= bitOf(s.charCodeAt(i));
  return m;
}

function bitOf(c: number): number {
  return c >= 97 && c <= 122 ? 1 << (c - 97) : c >= 48 && c <= 57 ? 1 << 26 : 0;
}

// fzy's bonus class for the character at j, from the one before it.
function bonusClass(prev: number, c: number): number {
  if (prev === 47) return 1; // "/"
  if (prev === 45 || prev === 95 || prev === 32) return 2; // "-" "_" " "
  if (prev === 46) return 3; // "."
  return prev >= 97 && prev <= 122 && c >= 65 && c <= 90 ? 4 : 0; // camelCase
}

// Preparing is the expensive half, and the tree barely changes between two
// openings of the palette, so it is done once per label, not per keystroke.
const cache = new Map<string, Prepared>();
const CACHE_MAX = 200_000;

export function prepare(label: string): Prepared {
  let p = cache.get(label);
  if (p) return p;
  const { text, cased, from, to } = fold(label);
  const name = text.lastIndexOf("/") + 1;
  const bonus = new Uint8Array(text.length);
  let mask = 0,
    nameMask = 0,
    prev = 47; // a label starts as if after "/"
  for (let j = 0; j < text.length; j++) {
    const c = cased.charCodeAt(j);
    bonus[j] = bonusClass(prev, c);
    prev = c;
    const bit = bitOf(text.charCodeAt(j));
    mask |= bit;
    if (j >= name) nameMask |= bit;
  }
  const base = text.slice(name);
  const dot = base.lastIndexOf(".");
  p = { label, text, bonus, name, base, stem: dot > 0 ? base.slice(0, dot) : base, mask, nameMask, from, to };
  if (cache.size >= CACHE_MAX) cache.clear();
  cache.set(label, p);
  return p;
}

/* ---- fzy ---- */

// The leftmost place each character of the last token checked can match.
// No alignment puts tok[i] earlier, so fzy starts row i there.
let lo = new Int32Array(64);

function hasMatch(tok: string, text: string, from: number): boolean {
  if (lo.length < tok.length) lo = new Int32Array(2 * tok.length);
  let j = from;
  for (let i = 0; i < tok.length; i++) {
    j = text.indexOf(tok[i], j);
    if (j === -1) return false;
    lo[i] = j++;
  }
  return true;
}

// Two rows of D (best score ending in a match here) and M (best score so
// far), grown once and reused: scoring every label on every keystroke must
// not allocate.
let rows = new Float64Array(0);
function rowsFor(m: number): Float64Array {
  if (rows.length < 4 * m) rows = new Float64Array(Math.max(4 * m, 1024));
  return rows;
}

// The best alignment of `tok` inside p.text[from:]. Callers run hasMatch on
// the same token and range first. With `pos`, it also writes the matched text
// indices there.
function fzy(tok: string, p: Prepared, from: number, pos?: number[]): number {
  const n = tok.length,
    m = p.text.length - from;
  if (n === m) {
    if (pos) for (let i = 0; i < n; i++) pos.push(from + i);
    return Infinity;
  }
  const { text, bonus } = p;
  if (pos) return fzyPositions(tok, p, from, pos);
  if (n === 1) {
    // One character: its best bonus, less a gap that is the same wherever it
    // sits (fzy's leading and trailing gaps cost the same per character).
    let best = -Infinity;
    for (let j = lo[0]; j !== -1; j = text.indexOf(tok, j + 1)) best = Math.max(best, BONUS[bonus[j]]);
    return best + GAP_TRAILING * (m - 1);
  }
  const r = rowsFor(m);
  let lastD = 0,
    lastM = m,
    curD = 2 * m,
    curM = 3 * m;
  for (let i = 0; i < n; i++) {
    const qc = tok.charCodeAt(i);
    const gap = i === n - 1 ? GAP_TRAILING : GAP_INNER;
    let prev = -Infinity;
    for (let j = lo[i] - from; j < m; j++) {
      if (qc === text.charCodeAt(from + j)) {
        let s = -Infinity;
        if (i === 0) s = j * GAP_LEADING + BONUS[bonus[from + j]];
        else if (j > 0) s = Math.max(r[lastM + j - 1] + BONUS[bonus[from + j]], r[lastD + j - 1] + MATCH_CONSECUTIVE);
        r[curD + j] = s;
        r[curM + j] = prev = Math.max(s, prev + gap);
      } else {
        r[curD + j] = -Infinity;
        r[curM + j] = prev = prev + gap;
      }
    }
    [lastD, curD] = [curD, lastD];
    [lastM, curM] = [curM, lastM];
  }
  return r[lastM + m - 1];
}

// The same DP with the whole matrix kept, so the alignment can be walked back.
// Only ever run for the rows on screen.
function fzyPositions(tok: string, p: Prepared, from: number, pos: number[]): number {
  const n = tok.length,
    m = p.text.length - from;
  const { text, bonus } = p;
  const D = new Float64Array(n * m),
    M = new Float64Array(n * m);
  for (let i = 0; i < n; i++) {
    const qc = tok.charCodeAt(i);
    const gap = i === n - 1 ? GAP_TRAILING : GAP_INNER;
    let prev = -Infinity;
    for (let j = 0; j < m; j++) {
      const k = i * m + j;
      if (qc === text.charCodeAt(from + j)) {
        let s = -Infinity;
        if (i === 0) s = j * GAP_LEADING + BONUS[bonus[from + j]];
        else if (j > 0) s = Math.max(M[k - m - 1] + BONUS[bonus[from + j]], D[k - m - 1] + MATCH_CONSECUTIVE);
        D[k] = s;
        M[k] = prev = Math.max(s, prev + gap);
      } else {
        D[k] = -Infinity;
        M[k] = prev = prev + gap;
      }
    }
  }
  const out = new Array<number>(n);
  let required = false;
  for (let i = n - 1, j = m - 1; i >= 0; i--) {
    for (; j >= 0; j--) {
      const k = i * m + j;
      if (D[k] !== -Infinity && (required || D[k] === M[k])) {
        required = i > 0 && j > 0 && M[k] === D[k - m - 1] + MATCH_CONSECUTIVE;
        out[i] = from + j--;
        break;
      }
    }
  }
  pos.push(...out);
  return M[n * m - 1];
}

/* ---- tokens ---- */

interface Token {
  text: string;
  mask: number;
  // Every distinct one-character deletion, for the typo pass.
  drops: Array<{ text: string; mask: number }>;
}

interface Query {
  whole: string;
  tokens: Token[];
}

function parse(query: string): Query | null {
  const whole = foldQuery(query);
  if (!whole) return null;
  const tokens = whole.split(/\s+/).map((text): Token => {
    const drops: Token["drops"] = [];
    if (text.length >= MIN_TYPO_LEN) {
      const seen = new Set<string>();
      for (let k = 0; k < text.length; k++) {
        const d = text.slice(0, k) + text.slice(k + 1);
        if (!seen.has(d)) {
          seen.add(d);
          drops.push({ text: d, mask: maskOf(d) });
        }
      }
    }
    return { text, mask: maskOf(text), drops };
  });
  return { whole, tokens };
}

// One token against one label. The file name first, then the whole path.
// The letter masks turn most labels away before a single indexOf.
function scoreToken(text: string, mask: number, p: Prepared, nameOnly: boolean, pos?: number[]): number {
  if (mask & ~p.mask) return -Infinity;
  if (!(mask & ~p.nameMask) && hasMatch(text, p.text, p.name)) return fzy(text, p, p.name, pos) + NAME_BONUS;
  if (nameOnly || !hasMatch(text, p.text, 0)) return -Infinity;
  return fzy(text, p, 0, pos);
}

// Whether `tok` minus one of its characters is a subsequence of text[from:]:
// two greedy walks instead of one per deletion. suffix[i] is the latest
// place tok[i:] can start; dropping tok[k] works when tok[:k] ends before
// suffix[k + 1].
let suffix = new Int32Array(64);
function nearMatch(tok: string, text: string, from: number): boolean {
  const n = tok.length;
  if (suffix.length <= n) suffix = new Int32Array(2 * n);
  suffix[n] = text.length;
  let i = n - 1;
  for (; i >= 0; i--) {
    const at = suffix[i + 1] > from ? text.lastIndexOf(tok[i], suffix[i + 1] - 1) : -1;
    if (at < from) break;
    suffix[i] = at;
  }
  for (; i >= 0; i--) suffix[i] = -1;
  let j = from;
  for (let k = 0; k < n; k++) {
    if (suffix[k + 1] >= j) return true;
    j = text.indexOf(tok[k], j) + 1;
    if (j === 0) return false;
  }
  return true;
}

function scoreTypo(t: Token, p: Prepared, nameOnly: boolean, pos?: number[]): number {
  if (!nearMatch(t.text, p.text, nameOnly ? p.name : 0)) return -Infinity;
  let best = -Infinity,
    bestDrop = -1;
  for (let k = 0; k < t.drops.length; k++) {
    const s = scoreToken(t.drops[k].text, t.drops[k].mask, p, nameOnly);
    if (s > best) {
      best = s;
      bestDrop = k;
    }
  }
  if (bestDrop < 0) return -Infinity;
  if (pos) scoreToken(t.drops[bestDrop].text, t.drops[bestDrop].mask, p, nameOnly, pos);
  return best - TYPO_PENALTY;
}

// With `typo`, one token may be forgiven (and one must be: the clean pass
// already has the rest). With `nameOnly`, every token must land in the name.
interface Mode {
  nameOnly: boolean;
  typo: boolean;
}

function scoreLabel(q: Query, p: Prepared, mode: Mode, pos?: number[]): number {
  let total = 0,
    forgiven = false;
  for (const t of q.tokens) {
    let s = scoreToken(t.text, t.mask, p, mode.nameOnly, pos);
    if (s === -Infinity && mode.typo && !forgiven) {
      // At most one letter of the token may be missing from the label.
      const missing = t.mask & ~(mode.nameOnly ? p.nameMask : p.mask);
      if ((missing & (missing - 1)) === 0) s = scoreTypo(t, p, mode.nameOnly, pos);
      forgiven = true;
    }
    if (s === -Infinity) return -Infinity;
    total += s;
  }
  if (mode.typo && !forgiven) return -Infinity;
  if (q.whole === p.stem || q.whole === p.base) total += EXACT_BONUS;
  return total;
}

/* ---- ranking ---- */

export interface Hit {
  index: number; // into the prepared list
  score: number;
  ranges: Range[]; // in the label, sorted, merged
}

// The best `limit` hits, best first: score, then the shorter label, then
// the caller's order. A bounded insertion keeps a one-letter query, which
// matches nearly everything, from sorting the whole tree.
class Top {
  readonly idx: number[] = [];
  readonly score: number[] = [];
  private readonly limit: number;
  private readonly items: readonly Prepared[];
  constructor(limit: number, items: readonly Prepared[]) {
    this.limit = limit;
    this.items = items;
  }
  private better(s: number, i: number, k: number) {
    const t = this.score[k];
    if (s !== t) return s > t;
    const a = this.items[i].label.length,
      b = this.items[this.idx[k]].label.length;
    return a !== b ? a < b : i < this.idx[k];
  }
  add(i: number, s: number) {
    let k = this.idx.length;
    if (k === this.limit && !this.better(s, i, k - 1)) return;
    while (k > 0 && this.better(s, i, k - 1)) k--;
    this.idx.splice(k, 0, i);
    this.score.splice(k, 0, s);
    if (this.idx.length > this.limit) {
      this.idx.pop();
      this.score.pop();
    }
  }
}

function toRanges(pos: number[], p: Prepared): Range[] {
  pos.sort((a, b) => a - b);
  const out: Array<[number, number]> = [];
  for (const i of pos) {
    const a = p.from ? p.from[i] : i,
      b = p.to ? p.to[i] : i + 1;
    const last = out[out.length - 1];
    if (last && a <= last[1]) last[1] = Math.max(last[1], b);
    else out.push([a, b]);
  }
  return out;
}

/* Rank `items` against `query`: the best `limit` hits, and how many matched
   in all. An empty query matches nothing; what an empty box shows is the
   caller's to decide. With `nameOnly`, a token must match inside the last
   path segment — the sidebar filter, which lists names. */
export function search(
  query: string,
  items: readonly Prepared[],
  { limit = 40, nameOnly = false }: { limit?: number; nameOnly?: boolean } = {},
): { hits: Hit[]; total: number } {
  const q = parse(query);
  if (!q) return { hits: [], total: 0 };
  const exact: Mode = { nameOnly, typo: false };
  const clean = new Top(limit, items);
  const missed: number[] = [];
  let found = 0;
  for (let i = 0; i < items.length; i++) {
    const s = scoreLabel(q, items[i], exact);
    if (s === -Infinity) missed.push(i);
    else {
      found++;
      clean.add(i, s);
    }
  }
  const typo: Mode = { nameOnly: nameOnly || found > 0, typo: true };
  const forgiven = new Top(TYPO_ROWS, items);
  if (q.tokens.some((t) => t.drops.length)) {
    for (const i of missed) {
      const s = scoreLabel(q, items[i], typo);
      if (s !== -Infinity) forgiven.add(i, s);
    }
  }
  const merged = new Top(limit, items);
  clean.idx.forEach((i, k) => merged.add(i, clean.score[k]));
  const best = found ? clean.score[0] : -Infinity;
  const typoRows = new Set<number>();
  forgiven.idx.forEach((i, k) => {
    if (forgiven.score[k] > best) {
      typoRows.add(i);
      merged.add(i, forgiven.score[k]);
    }
  });
  const hits = merged.idx.map((index, k) => {
    const pos: number[] = [];
    scoreLabel(q, items[index], typoRows.has(index) ? typo : exact, pos);
    return { index, score: merged.score[k], ranges: toRanges(pos, items[index]) };
  });
  return { hits, total: found + typoRows.size };
}
