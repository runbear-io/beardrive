/* Text-quote anchors for review comments.

   A comment remembers the text the reader selected plus a little context on
   either side (the W3C TextQuoteSelector shape) — never a DOM offset. The
   same file renders through markdown, an iframe, or a <pre>, and the hub
   re-renders it after every edit; a quote survives all of that, and it is
   what an agent greps the source for (`bdrive comments` does the same match
   against the raw file).

   Matching ignores whitespace and case. The browser reports a selection's
   whitespace its own way (block boundaries become newlines in
   Selection.toString() and nothing at all in the text nodes), and
   text-transform changes case on screen but not in the DOM. Everything else
   must match exactly.

   Shared by the app (markdown and plain-text views) and inline-comment.ts,
   the bootstrap inside a commented HTML page — so nothing here may import
   from the app. */

export type Anchor = {
  quote?: string;
  prefix?: string;
  suffix?: string;
  line?: number;
  element?: string;
};

// How much context is kept either side of a quote.
const CONTEXT = 48;

/* squash keeps the characters a match is decided on, and where each came
   from in the original string. */
export function squash(s: string): { text: string; map: number[] } {
  let text = "";
  const map: number[] = [];
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (/\s/.test(c) || c === "​" || c === "­") continue;
    const l = c.toLowerCase();
    text += l.length === 1 ? l : c;
    map.push(i);
  }
  return { text, map };
}

/* bestMatch finds needle in hay, and when it occurs more than once prefers
   the occurrence whose neighbouring text agrees with the recorded context
   for longest. -1 when absent. All three are already squashed. */
export function bestMatch(hay: string, needle: string, pre = "", suf = ""): number {
  if (!needle) return -1;
  let best = -1;
  let bestScore = -1;
  for (let i = hay.indexOf(needle); i !== -1; i = hay.indexOf(needle, i + 1)) {
    let score = 0;
    for (let k = 1; k <= pre.length && i - k >= 0 && hay[i - k] === pre[pre.length - k]; k++) score++;
    const end = i + needle.length;
    for (let k = 0; k < suf.length && end + k < hay.length && hay[end + k] === suf[k]; k++) score++;
    if (score > bestScore) {
      best = i;
      bestScore = score;
    }
  }
  return best;
}

/* locate finds an anchor's quote in a plain string, returning the [start,
   end) span in that string's own indices, or null. */
export function locate(all: string, a: Anchor): [number, number] | null {
  const q = squash(a.quote || "").text;
  if (!q) return null;
  const { text, map } = squash(all);
  const i = bestMatch(text, q, squash(a.prefix || "").text, squash(a.suffix || "").text);
  if (i < 0) return null;
  return [map[i], map[i + q.length - 1] + 1];
}

/* contextAround is the prefix/suffix pair for a span of a plain string,
   whitespace collapsed so a stored anchor stays small and readable. */
export function contextAround(all: string, start: number, end: number): { prefix: string; suffix: string } {
  const tidy = (s: string) => s.replace(/\s+/g, " ");
  return {
    prefix: tidy(all.slice(Math.max(0, start - CONTEXT), start)).trimStart(),
    suffix: tidy(all.slice(end, end + CONTEXT)).trimEnd(),
  };
}

// ---- the DOM half ----

const SKIP = new Set(["SCRIPT", "STYLE", "NOSCRIPT", "TEMPLATE", "TEXTAREA"]);

type TextIndex = { all: string; nodes: Text[]; starts: number[] };

/* textIndex is every visible text node under root, concatenated, with where
   each one starts — the string anchors are located in. */
export function textIndex(root: Node): TextIndex {
  const nodes: Text[] = [];
  const starts: number[] = [];
  let all = "";
  const doc = root.ownerDocument || (root as Document);
  const walker = doc.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode(n) {
      for (let p = n.parentElement; p && p !== root; p = p.parentElement) {
        if (SKIP.has(p.tagName) || p.hasAttribute("data-bd-ignore")) return NodeFilter.FILTER_REJECT;
      }
      return NodeFilter.FILTER_ACCEPT;
    },
  });
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    nodes.push(n as Text);
    starts.push(all.length);
    all += (n as Text).data;
  }
  return { all, nodes, starts };
}

function pointAt(ix: TextIndex, at: number, end: boolean): [Text, number] | null {
  if (!ix.nodes.length) return null;
  // Last node starting at or before `at`; an END point exactly on a node
  // boundary belongs to the node before it, so the range does not run on
  // into the next one.
  let lo = 0;
  let hi = ix.nodes.length - 1;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (ix.starts[mid] < at || (!end && ix.starts[mid] === at)) lo = mid;
    else hi = mid - 1;
  }
  return [ix.nodes[lo], Math.min(at - ix.starts[lo], ix.nodes[lo].data.length)];
}

/* rangeFor finds an anchor's quote under root as a live Range, or null. */
export function rangeFor(root: Node, a: Anchor, ix = textIndex(root)): Range | null {
  const span = locate(ix.all, a);
  if (!span) return null;
  const s = pointAt(ix, span[0], false);
  const e = pointAt(ix, span[1], true);
  if (!s || !e) return null;
  const r = (root.ownerDocument || (root as Document)).createRange();
  try {
    r.setStart(s[0], s[1]);
    r.setEnd(e[0], e[1]);
  } catch {
    return null;
  }
  return r;
}

/* offsetOf is where a DOM point falls in textIndex(root).all. */
function offsetOf(ix: TextIndex, node: Node, offset: number): number {
  const i = ix.nodes.indexOf(node as Text);
  if (i >= 0) return ix.starts[i] + offset;
  // An element boundary: the first text node at or after it.
  const doc = node.ownerDocument!;
  const probe = doc.createRange();
  probe.setStart(node, offset);
  for (let k = 0; k < ix.nodes.length; k++) {
    if (probe.comparePoint(ix.nodes[k], 0) >= 0) return ix.starts[k];
  }
  return ix.all.length;
}

/* describe turns a selection under root into an anchor. `shown` is what the
   reader saw selected (Selection.toString(), with the browser's own line
   breaks), which reads better in a comment than the raw node text. When
   `lines` is set, root's text IS the file (a plain-text view), so the line
   number is known exactly and recorded too. */
export function describe(root: Node, range: Range, shown: string, lines = false): Anchor | null {
  const ix = textIndex(root);
  const start = offsetOf(ix, range.startContainer, range.startOffset);
  const end = offsetOf(ix, range.endContainer, range.endOffset);
  if (end <= start) return null;
  const quote = (shown || ix.all.slice(start, end)).trim();
  if (!squash(quote).text) return null;
  const a: Anchor = { quote, ...contextAround(ix.all, start, end) };
  if (lines) a.line = ix.all.slice(0, start).split("\n").length;
  return a;
}

/* cssPath names an element by its position under <body>: tag, id when it
   has one, nth-of-type otherwise. Good enough to find the element again in
   the same page and to tell an agent where on the page a pin was dropped. */
export function cssPath(el: Element): string {
  const parts: string[] = [];
  for (let e: Element | null = el; e && e.tagName !== "BODY" && e.tagName !== "HTML"; e = e.parentElement) {
    const tag = e.tagName.toLowerCase();
    if (e.id && /^[A-Za-z][\w-]*$/.test(e.id)) {
      parts.unshift(tag + "#" + e.id);
      break;
    }
    const parent: Element | null = e.parentElement;
    const same = parent ? Array.from(parent.children).filter((c) => c.tagName === e!.tagName) : [];
    parts.unshift(same.length > 1 ? `${tag}:nth-of-type(${same.indexOf(e) + 1})` : tag);
    if (parts.length >= 8) break;
  }
  return parts.join(" > ");
}

/* hitTest reports which of the ranges a click at (x, y) landed in. */
export function hitTest(doc: Document, x: number, y: number, ranges: Map<string, Range>): string | null {
  const d = doc as Document & {
    caretPositionFromPoint?: (x: number, y: number) => { offsetNode: Node; offset: number } | null;
  };
  let node: Node | null = null;
  let offset = 0;
  if (d.caretPositionFromPoint) {
    const p = d.caretPositionFromPoint(x, y);
    if (p) [node, offset] = [p.offsetNode, p.offset];
  } else if (doc.caretRangeFromPoint) {
    const r = doc.caretRangeFromPoint(x, y);
    if (r) [node, offset] = [r.startContainer, r.startOffset];
  }
  if (!node) return null;
  for (const [id, r] of ranges) {
    try {
      if (r.isPointInRange(node, offset) && !(r.endContainer === node && r.endOffset === offset)) return id;
    } catch {
      /* a range from a document that has since changed */
    }
  }
  return null;
}
