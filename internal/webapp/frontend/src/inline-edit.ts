/* Click-to-edit, running INSIDE the sandboxed iframe that renders a synced
   HTML file.

   This is a separate bundle from the app, loaded only by the ?edit=1 render
   (server.go's serveEditable). A reader never downloads an editor, and the
   iframe keeps exactly the sandbox it always had — no allow-same-origin — so
   nothing here can reach the hub's session. It talks to the app by postMessage
   and accepts nothing back.

   Three things had to be true at once, and they decided the design:

   - The edited element must keep its own tag and the page's own CSS, because
     "what you see" has to be what the file renders. So the editor is mounted
     ONTO the element (ProseMirror's {mount}), not inside it — TipTap's own
     Editor appends a child, which would put a <p> inside an <h1>.
   - The file must survive editing byte-for-byte outside the edited element.
     So nothing here ever serializes the document; it reports ONE element's
     inner HTML and the source range it belongs to, and the app splices.
   - Markup the schema cannot model must never be silently dropped. So an
     element is only made editable once it has been proved to survive a
     parse/serialize round trip — see roundTrips. */

import { getSchema, Mark } from "@tiptap/core";
import Document from "@tiptap/extension-document";
import Text from "@tiptap/extension-text";
import Bold from "@tiptap/extension-bold";
import Italic from "@tiptap/extension-italic";
import Code from "@tiptap/extension-code";
import { DOMParser as PMParser, DOMSerializer, type Node as PMNode } from "@tiptap/pm/model";
import { EditorState, TextSelection } from "@tiptap/pm/state";
import { EditorView } from "@tiptap/pm/view";
import { keymap } from "@tiptap/pm/keymap";
import { baseKeymap, toggleMark } from "@tiptap/pm/commands";
import { history, undo, redo } from "@tiptap/pm/history";

const SRC_ATTR = "data-bd-src";

/* The schema IS the fidelity envelope. Every element whose markup this cannot
   represent fails the round-trip check and stays read-only, so widening it is
   how more of a document becomes editable — and narrowing it is how a document
   is protected from an editor that would flatten it.

   The document holds inline content directly, with no paragraph node. That is
   what lets the editor mount onto an <h1> or a <td> without inventing a block
   inside it, and it is also what makes Enter structurally impossible rather
   than merely discouraged: there is no block to split. */
/* Inline markup the schema has no opinion about, carried through unchanged.

   Without this, a paragraph containing <span class="badge">up 4%</span> could
   not be edited at all: the schema had nowhere to put the span, the round-trip
   check saw it vanish, and the element was refused. That is safe but it is not
   usable — real documents are full of spans, <sup>, <abbr>, <u>, and refusing
   every paragraph that contains one leaves an editor that mostly says no.

   So unknown inline elements become a mark that remembers its tag and its
   attributes verbatim and renders them straight back. It is best-effort by
   design — overlapping or nested cases can still come out differently — and
   that is fine, because the ROUND-TRIP CHECK IS STILL THE ARBITER. This can
   only ever make more paragraphs editable; anything it gets wrong is caught
   by the same gate as before and refused exactly as it was.

   Priority 10 keeps it out of the way: bold, italic, link and code parse at
   the default 50, so real marks always win and this picks up the remainder. */
const RawInline = Mark.create({
  name: "rawInline",
  // Several may sit on the same text (<span><sup>x</sup></span>), and typing
  // at an edge must not extend somebody's badge over the words after it.
  excludes: "",
  inclusive: false,
  addAttributes() {
    return { tag: { default: "span" }, attrs: { default: {} } };
  },
  parseHTML() {
    return [
      {
            // <a> is here rather than handled by TipTap's Link extension on
        // purpose. Link RE-RENDERS an anchor from its own defaults, adding
        // target="_blank" and rel="noopener noreferrer nofollow" that the
        // file never had — so the markup did not round-trip and the gate
        // refused EVERY paragraph containing a link, which is most prose.
        // Carried through verbatim it survives byte-for-byte, and the text
        // inside it still edits. Nothing here needs to understand a link;
        // it needs to not damage one.
        tag: "a, span, sup, sub, small, abbr, mark, u, s, del, ins, cite, q, var, samp, kbd, time, data, bdi, bdo, big, strike, tt, font, label, ruby, rt, rp, dfn",
        priority: 10,
        getAttrs: (el: HTMLElement) => ({
          tag: el.tagName.toLowerCase(),
          attrs: Object.fromEntries(
            [...el.attributes].map((a) => [a.name, a.value]),
          ),
        }),
      },
    ];
  },
  renderHTML({ mark }) {
    return [mark.attrs.tag as string, mark.attrs.attrs as Record<string, string>, 0];
  },
});

const schema = getSchema([
  Document.extend({ content: "inline*" }),
  Text,
  Bold,
  Italic,
  Code,
  RawInline,
]);

const parser = PMParser.fromSchema(schema);
const serializer = DOMSerializer.fromSchema(schema);
// "full" keeps significant whitespace: an indented file's text nodes carry the
// newlines around them, and dropping those would reflow the source on save.
const PARSE = { preserveWhitespace: "full" } as const;

function parseInner(el: HTMLElement): PMNode {
  return parser.parse(el, PARSE);
}

function serializeInner(doc: PMNode): string {
  const box = document.createElement("div");
  box.appendChild(serializer.serializeFragment(doc.content));
  return box.innerHTML;
}

/* The gate, and the reason this feature cannot quietly eat a teammate's markup.

   ProseMirror normalizes whatever it is given into its schema: hand it a <span
   class="badge"> with no matching node or mark and it hands back text. That is
   silent data loss in a file somebody else wrote, and it happens on a click
   with no typing at all.

   So an element becomes editable only if its content survives the trip. The
   comparison is against the browser's innerHTML rather than the source bytes,
   because the browser has already normalized entities and attribute quoting on
   parse and comparing against the raw source would fail on every document. The
   consequence — an edited element's inner markup may come back normalized,
   while everything outside it is untouched — is the documented ceiling. */
function roundTrips(el: HTMLElement): boolean {
  try {
    return serializeInner(parseInner(el)) === el.innerHTML;
  } catch {
    return false;
  }
}

type Range = { start: number; end: number };

function rangeOf(el: Element): Range | null {
  const raw = el.getAttribute(SRC_ATTR);
  if (!raw) return null;
  const [a, b] = raw.split(",");
  const start = Number(a);
  const end = Number(b);
  if (!Number.isInteger(start) || !Number.isInteger(end) || start > end) return null;
  return { start, end };
}

const send = (msg: Record<string, unknown>) => parent.postMessage(msg, "*");

/* Whether an element may be edited is settled once and remembered. The check
   parses, so doing it per mousemove would be the one thing on this page that
   makes reading a document slow. */
const verdicts = new WeakMap<HTMLElement, boolean>();

function editable(el: HTMLElement): boolean {
  let v = verdicts.get(el);
  if (v === undefined) {
    v = roundTrips(el);
    verdicts.set(el, v);
    el.setAttribute("data-bd-edit", v ? "yes" : "no");
  }
  return v;
}

// ---- the live editor, one element at a time ----

let view: EditorView | null = null;
let host: HTMLElement | null = null;
let range: Range | null = null;
let before = "";

// The document as markup, or null if there is no live editor.
function currentHTML(): string | null {
  return view ? serializeInner(view.state.doc) : null;
}

function flush() {
  if (!host || !range) return;
  const html = currentHTML();
  if (html === null || html === before) return;
  before = html;
  send({ type: "bd-edit:patch", start: range.start, end: range.end, html });
}

function unmount() {
  if (!view) return;
  flush();

  /* ProseMirror EMPTIES a mounted element when it is destroyed.

     `destroy()` ends with `this.dom.textContent = ""` whenever the view was
     created with {mount} — it assumes the owner is about to re-render, which
     is true of a framework and not of us: we mounted onto the page's own
     element and the page has no other copy of it. Left alone, clicking from
     one paragraph to the next wipes the first one off the screen. The FILE is
     fine (flush ran first), which makes it worse, not better: it reads as
     data loss, and nobody trusts an editor twice after seeing that.

     So the final markup is put back by hand. It is exactly what was just
     written to the file, so the page ends up showing what it now says. */
  const el = host;
  const html = currentHTML();
  view.destroy();
  view = null;
  if (el) {
    if (html !== null) el.innerHTML = html;
    el.removeAttribute("data-bd-editing");
    // The content is new DOM, so a verdict about the old nodes is stale —
    // and it would be wrong to keep one that was computed before an edit.
    verdicts.delete(el);
    el.removeAttribute("data-bd-edit");
  }
  host = null;
  range = null;
}

/* Put the caret where the click landed.

   The editor does not exist until the click has already happened, so the
   browser's own selection is thrown away when ProseMirror takes the element
   over — and a fresh EditorState starts at position 0. Without this, clicking
   into the middle of a sentence silently drops the caret at its start and the
   first thing typed appears in the wrong place, which is the single most
   noticeable way a click-to-edit surface can feel broken. */
function caretAt(v: EditorView, x: number, y: number) {
  const at = v.posAtCoords({ left: x, top: y });
  if (!at) return;
  const sel = TextSelection.near(v.state.doc.resolve(at.pos));
  v.dispatch(v.state.tr.setSelection(sel));
}

function mount(el: HTMLElement, x?: number, y?: number) {
  if (host === el) return;
  unmount();
  const r = rangeOf(el);
  if (!r) return;
  host = el;
  range = r;
  before = el.innerHTML;
  el.setAttribute("data-bd-editing", "");

  const state = EditorState.create({
    doc: parseInner(el),
    plugins: [
      history(),
      // Bound BEFORE baseKeymap so these win. Enter and Backspace-at-start are
      // structural: honouring them would mean adding or removing an element,
      // which is not something a single-range splice can express. They are
      // swallowed rather than left to fall through to a browser default.
      keymap({
        "Mod-b": toggleMark(schema.marks.bold),
        "Mod-i": toggleMark(schema.marks.italic),
        "Mod-z": undo,
        "Mod-y": redo,
        "Shift-Mod-z": redo,
        Enter: () => true,
        "Shift-Enter": () => true,
        Escape: () => {
          unmount();
          return true;
        },
      }),
      keymap(baseKeymap),
    ],
  });

  view = new EditorView(
    { mount: el },
    {
      state,
      dispatchTransaction(tr) {
        view!.updateState(view!.state.apply(tr));
        /* Reported on EVERY change, with no debounce of its own.

           There used to be a 700ms idle here, mirroring the save timer. It
           meant that for 700ms after the last keystroke the edit existed ONLY
           inside this iframe — and pressing Done in that window tears the
           iframe down with the edit still pending, losing it silently. That is
           not an edge case: typing and immediately clicking Done is what
           finishing an edit looks like.

           There is nothing to gain by waiting. A patch is a postMessage into
           the same tab; the debounce that actually matters is the app's save
           timer, which still batches the writes that reach the hub. */
        if (tr.docChanged) flush();
      },
    },
  );
  view.focus();
  if (x !== undefined && y !== undefined) caretAt(view, x, y);
}

// ---- wiring ----

function nearestEditable(target: EventTarget | null): HTMLElement | null {
  const el = (target as Element | null)?.closest?.(`[${SRC_ATTR}]`);
  return el instanceof HTMLElement ? el : null;
}

document.addEventListener(
  "click",
  (e) => {
    const el = nearestEditable(e.target);
    if (!el) {
      unmount(); // clicking away commits, the same as blurring
      return;
    }
    if (!editable(el)) return;
    mount(el, e.clientX, e.clientY);
  },
  true,
);

// Verdicts are computed on approach so the outline only ever appears on
// something that will actually open. An element that cannot be edited says so
// on hover instead of refusing a click for no visible reason.
document.addEventListener(
  "mouseover",
  (e) => {
    const el = nearestEditable(e.target);
    if (el) editable(el);
  },
  true,
);

/* A page whose own script rewrites the DOM can replace the element out from
   under an open editor, and the keystrokes go with it. There is no way to stop
   that from here — the page's script is the page — so the honest move is to
   notice and say so. Detaching is the observable form of every version of this:
   a React re-render, a chart redraw, a template swap. */
new MutationObserver(() => {
  if (host && !host.isConnected) {
    send({ type: "bd-edit:clobbered" });
    view?.destroy();
    view = null;
    host = null;
    range = null;
  }
}).observe(document.documentElement, { childList: true, subtree: true });

const style = document.createElement("style");
style.textContent = `
  [${SRC_ATTR}][data-bd-edit="yes"]:hover { outline: 1px dashed rgba(99,102,241,.7); outline-offset: 2px; cursor: text; }
  [${SRC_ATTR}][data-bd-edit="no"]:hover  { outline: 1px dotted rgba(120,120,120,.5); outline-offset: 2px; cursor: not-allowed; }
  [${SRC_ATTR}][data-bd-editing] { outline: 2px solid rgba(99,102,241,.9); outline-offset: 2px; }
  [${SRC_ATTR}][data-bd-editing]:focus { outline: 2px solid rgba(99,102,241,.9); }
`;
document.head.appendChild(style);

// The hash the server computed over the STORED bytes. The app compares it with
// the document it holds: if they differ, the ranges below describe a file that
// has since moved on, and it reloads rather than splicing by them.
const self =
  (document.currentScript as HTMLScriptElement | null) ??
  document.querySelector<HTMLScriptElement>("script[data-src-hash]");
const hash = self?.dataset.srcHash ?? "";
// The source length in UTF-16 units, which is the app's fallback check when it
// cannot compute a hash — crypto.subtle exists only in a secure context, and a
// LAN hub on plain HTTP is not one.
const len = Number(self?.dataset.srcLen ?? "-1");

send({
  type: "bd-edit:ready",
  hash,
  len,
  ranges: [...document.querySelectorAll(`[${SRC_ATTR}]`)]
    .map((el) => rangeOf(el))
    .filter(Boolean),
});

// The markers are in the served HTML, so their presence says nothing about
// whether this script has run. Anything waiting to interact — a test, or a
// person — needs to know the listeners are actually installed, and this is
// the only observable difference between "rendered" and "editable".
document.documentElement.setAttribute("data-bd-ready", "");

window.addEventListener("beforeunload", unmount);
