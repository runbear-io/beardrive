/* Click-to-edit, running INSIDE the sandboxed iframe that renders a synced
   HTML file.

   This is a separate bundle from the app, loaded only by the ?edit=1 render
   (server.go's serveEditable). A reader never downloads an editor, and the
   iframe keeps exactly the sandbox it always had — no allow-same-origin — so
   nothing here can reach the hub's session. It is injected FIRST in the
   document, before any of the page's own scripts, and hands the app one end
   of a MessageChannel (`bd-edit:hello`); everything after travels on that
   port, never on window, so the page's scripts can neither hear nor forge
   it. Back it takes `bd-edit:flush` (a numeric id, optionally `lock: true`),
   which posts any edit still held here and answers `bd-edit:flushed` with
   that id — so the app's Done saves only after every patch before the answer
   has landed — and, with `lock`, stops accepting edits until
   `bd-edit:unlock`. It also takes `bd-edit:ack` (each patch's `seq`, once the
   app has it in the document) and `bd-edit:port` (a MessagePort to the app's
   exit worker, lib/editexit.worker.ts): a tab closed before a patch was
   acknowledged posts every edited span down that port on the way out. This
   frame never holds a URL or credential and makes no request of its own, and
   only a person's input (Event.isTrusted) opens or changes an editor.

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
import { ackEditedSpan, unackedExitPatches, type EditedSpan } from "./lib/editbeacon";

/* ---- t = 0: before the page's own scripts exist ----

   The server injects this script first in the document, synchronous, so it
   runs in a realm no page script has touched yet. Everything a page script
   could later replace to see or forge this editor's traffic is taken now:
   the channel to the app, the methods that move messages on it, the
   isTrusted getter that tells a person's input from a script's, and the DOM
   reads the ranges come from. The app's end of the channel is handed up
   below (`bd-edit:hello`) before any page script can watch window messages;
   nothing in this editor ever travels on window again. */
const NativeMessageChannel = window.MessageChannel;
const portPost = MessagePort.prototype.postMessage;
const portClose = MessagePort.prototype.close;
const portStart = MessagePort.prototype.start;
const targetAddListener = EventTarget.prototype.addEventListener;
const getIsTrusted = Object.getOwnPropertyDescriptor(new Event("x"), "isTrusted")!.get!;
const getMsgData = Object.getOwnPropertyDescriptor(MessageEvent.prototype, "data")!.get!;
const getMsgPorts = Object.getOwnPropertyDescriptor(MessageEvent.prototype, "ports")!.get!;
const elGetAttribute = Element.prototype.getAttribute;
const elClosest = Element.prototype.closest;
const docQueryAll = window.Document.prototype.querySelectorAll;
const nativeSetTimeout = window.setTimeout.bind(window);
const nativeClearTimeout = window.clearTimeout.bind(window);
const postToParent = window.parent.postMessage.bind(window.parent);

/* Who is running. The page's scripts share this realm with the editor, and
   while the editor is open they must not write into it: a keystroke is a
   person's, but a page script's own keydown/input listener runs inside that
   very keystroke. So every callback is marked by who registered it —
   ProseMirror's (registered while the editor sets itself up or runs) count
   as the editor's (`inEditorCall` > 0), everyone else's run with it at 0 —
   and the DOM methods that could write into the open element refuse a caller
   at 0 (installGuards, below). */
let inEditorSetup = 0;
let inEditorCall = 0;
type AnyFn = (this: unknown, ...args: unknown[]) => unknown;
function asEditor<F extends (...args: never[]) => unknown>(fn: F): F {
  const f = fn as unknown as AnyFn;
  return function (this: unknown, ...a: unknown[]) {
    inEditorCall++;
    try {
      return f.apply(this, a);
    } finally {
      inEditorCall--;
    }
  } as unknown as F;
}
function asPage<F extends (...args: never[]) => unknown>(fn: F): F {
  const f = fn as unknown as AnyFn;
  return function (this: unknown, ...a: unknown[]) {
    const saved = inEditorCall;
    inEditorCall = 0;
    try {
      return f.apply(this, a);
    } finally {
      inEditorCall = saved;
    }
  } as unknown as F;
}
const forCaller = <F extends (...args: never[]) => unknown>(fn: F): F =>
  inEditorSetup > 0 || inEditorCall > 0 ? asEditor(fn) : asPage(fn);
// ProseMirror setting itself up: everything it registers is the editor's.
function editorSetup<T>(make: () => T): T {
  inEditorSetup++;
  inEditorCall++;
  try {
    return make();
  } finally {
    inEditorSetup--;
    inEditorCall--;
  }
}
// The editor's own listeners, registered on the untouched method.
const listen = (target: EventTarget, type: string, fn: (e: Event) => void, opts?: boolean | AddEventListenerOptions) =>
  targetAddListener.call(target, type, asEditor(fn), opts);

// Natives the guards call through, taken before anything can replace them.
const rawRemoveListener = EventTarget.prototype.removeEventListener;
const RawMutationObserver = window.MutationObserver;
const nodeContains = Node.prototype.contains;
const rangeIntersects = Range.prototype.intersectsNode;
const selectionContains = Selection.prototype.containsNode;
const innerHTMLDesc = Object.getOwnPropertyDescriptor(Element.prototype, "innerHTML")!;
const elInnerHTMLGet = innerHTMLDesc.get!;
const elInnerHTMLSet = innerHTMLDesc.set!;
const elRemoveAttribute = Element.prototype.removeAttribute;
const attrNameGet = Object.getOwnPropertyDescriptor(Attr.prototype, "name")!.get!;
const datasetGet = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "dataset")!.get!;
const nativeExecCommand = window.Document.prototype.execCommand;

// The hash the server computed over the STORED bytes, read while this script
// is still `currentScript`. The app compares it with the document it holds:
// if they differ, the ranges describe a file that has since moved on, and it
// reloads rather than splicing by them.
const self = document.currentScript as HTMLScriptElement | null;
const hash = self?.dataset.srcHash ?? "";
// The source length in UTF-16 units, which is the app's fallback check when it
// cannot compute a hash — crypto.subtle exists only in a secure context, and a
// LAN hub on plain HTTP is not one.
const len = Number(self?.dataset.srcLen ?? "-1");
// Every range the server stamped ("start,end;..."): an element may only ever
// be bound to one of these. Null on a server that does not send the list.
const serverRanges =
  self && self.hasAttribute("data-src-ranges")
    ? new Set((self.dataset.srcRanges ?? "").split(";").filter(Boolean))
    : null;
// Read, then gone: nothing a page script could use is left on the tag.
if (self) {
  for (const a of ["data-src-hash", "data-src-len", "data-src-ranges"]) elRemoveAttribute.call(self, a);
}

// This frame's end of the channel to the app; never on window or the DOM.
const appChannel = new NativeMessageChannel();
const appPort = appChannel.port1;
const send = (msg: Record<string, unknown>) => {
  portPost.call(appPort, msg);
};

/* A person's input, as opposed to a script's. Each trusted input event opens
   a window that lasts through its own task — the handler and ProseMirror's
   DOM-observer microtask after it — and closes on the next one. A document
   change outside such a window was made by a script, not by the reader. */
let trustedEditOpen = false;
let composing = false;
let trustedTimer: ReturnType<typeof setTimeout> | undefined;
const openTrustedWindow = (ms = 0) => {
  trustedEditOpen = true;
  nativeClearTimeout(trustedTimer);
  trustedTimer = nativeSetTimeout(() => {
    trustedEditOpen = false;
  }, ms);
};
for (const name of [
  "keydown",
  "beforeinput",
  "input",
  "textInput",
  "compositionstart",
  "compositionupdate",
  "compositionend",
  "paste",
  "cut",
  "drop",
]) {
  targetAddListener.call(
    document,
    name,
    (e: Event) => {
      if (!getIsTrusted.call(e)) return;
      if (name === "compositionstart") composing = true;
      else if (name === "compositionend") {
        composing = false;
        openTrustedWindow(60);
        return;
      }
      openTrustedWindow(0);
    },
    true,
  );
}

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
/* An element's range, as bound when the document was first read (initDOM):
   never the attribute again, which a page script may since have rewritten. */
function rangeOf(el: Element): Range | null {
  const b = boundRanges.get(el);
  return b ? { start: b.start, end: b.end } : null;
}

// The attribute as served, parsed once (initDOM).
function parseRange(el: Element): Range | null {
  const raw = elGetAttribute.call(el, SRC_ATTR);
  if (!raw) return null;
  const [a, b] = raw.split(",");
  const start = Number(a);
  const end = Number(b);
  if (!Number.isInteger(start) || !Number.isInteger(end) || start > end) return null;
  return { start, end };
}

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
/* Done's save is on the wire: no edit may start until the app says how it
   went (`bd-edit:unlock` if the editor stays; a page torn down otherwise). */
let locked = false;
/* The exit path (lib/editbeacon.ts): every patch is numbered, the app
   acknowledges each once it is in the document, and a page that goes with a
   patch unacknowledged hands every span edited on this render to the app's
   exit worker, which saves it with the app's own session. */
let patchSeq = 0;
let exitPort: MessagePort | null = null;
const editedSpans = new Map<string, EditedSpan>();
/* Elements bound to their source ranges, privately: the page can rewrite an
   attribute, but not this. Filled once, when the document is first read. */
const boundRanges = new WeakMap<Element, { start: number; end: number; initialHtml: string }>();

/* A page script tried to write into the open element. The write does not
   happen, the editor closes with the element as it was last saved — never
   flushed, so the script's markup is never sent — and the element is not
   offered for editing again on this render: the page's script has its hands
   on it. */
function clobberFromScript() {
  const el = host;
  const v = view;
  if (!el) return;
  view = null;
  host = null;
  range = null;
  boundRanges.delete(el);
  inEditorCall++;
  try {
    v?.destroy();
    elInnerHTMLSet.call(el, before);
    elRemoveAttribute.call(el, "data-bd-editing");
    elRemoveAttribute.call(el, "contenteditable");
  } catch {
    // a page that broke its own element: the editor is closed either way
  } finally {
    inEditorCall--;
  }
  send({ type: "bd-edit:clobbered" });
}

/* The guards, installed before any page script runs and frozen so none can
   take them back out. Each refuses a caller outside the editor:
   - writes into the open element (and everything in it): the editor closes
     (clobberFromScript) and the write never lands;
   - any change to a data-bd-src marker;
   - document.execCommand, which edits whatever is focused;
   and every listener, observer and timer runs as whoever registered it. */
function installGuards() {
  const touches = (n: unknown): boolean => {
    if (host === null) return false;
    if (n === host) return true;
    try {
      return nodeContains.call(host, n as Node);
    } catch {
      return false;
    }
  };
  const hostWrite = (self: unknown, args: unknown[]): boolean => {
    if (inEditorCall > 0 || host === null) return false;
    if (!touches(self) && !args.some(touches)) return false;
    clobberFromScript();
    return true;
  };
  const isSrcName = (n: unknown) => typeof n === "string" && n.toLowerCase() === SRC_ATTR;
  const localOf = (q: unknown) => (typeof q === "string" ? q.slice(q.indexOf(":") + 1) : q);
  const isSrcAttr = (a: unknown) => {
    try {
      return isSrcName(attrNameGet.call(a));
    } catch {
      return false;
    }
  };
  const srcWrite = (blocked: boolean) => inEditorCall === 0 && blocked;

  const freeze = (obj: object, name: string, value: unknown) =>
    Object.defineProperty(obj, name, { value, writable: false, configurable: false, enumerable: false });
  const methods = (
    proto: object,
    names: string[],
    blocked: (self: unknown, args: unknown[]) => boolean,
    refused?: unknown,
  ) => {
    for (const name of names) {
      const d = Object.getOwnPropertyDescriptor(proto, name);
      if (!d || typeof d.value !== "function") continue;
      const native = d.value as AnyFn;
      freeze(proto, name, function (this: unknown, ...a: unknown[]) {
        return blocked(this, a) ? refused : native.apply(this, a);
      });
    }
  };
  const setters = (proto: object, names: string[], blocked: (self: unknown) => boolean) => {
    for (const name of names) {
      const d = Object.getOwnPropertyDescriptor(proto, name);
      if (!d?.set) continue;
      const set = d.set;
      Object.defineProperty(proto, name, {
        configurable: false,
        enumerable: d.enumerable,
        get: d.get,
        set(this: unknown, v: unknown) {
          if (!blocked(this)) set.call(this, v);
        },
      });
    }
  };

  // The open element and everything in it.
  setters(Element.prototype, ["innerHTML", "outerHTML"], (s) => hostWrite(s, []));
  setters(HTMLElement.prototype, ["innerText", "outerText"], (s) => hostWrite(s, []));
  setters(Node.prototype, ["textContent", "nodeValue"], (s) => srcWrite(isSrcAttr(s)) || hostWrite(s, []));
  setters(CharacterData.prototype, ["data"], (s) => hostWrite(s, []));
  methods(CharacterData.prototype, ["appendData", "insertData", "deleteData", "replaceData"], hostWrite);
  methods(CharacterData.prototype, ["before", "after", "replaceWith", "remove"], hostWrite);
  methods(window.Text.prototype, ["splitText"], hostWrite);
  methods(Node.prototype, ["appendChild", "insertBefore", "replaceChild", "removeChild", "normalize"], hostWrite);
  methods(
    Element.prototype,
    [
      "insertAdjacentHTML",
      "insertAdjacentText",
      "insertAdjacentElement",
      "append",
      "prepend",
      "replaceChildren",
      "replaceWith",
      "remove",
      "before",
      "after",
    ],
    hostWrite,
  );
  const rangeWrite = (self: unknown, args: unknown[]): boolean => {
    if (inEditorCall > 0 || host === null) return false;
    let hits = args.some(touches);
    try {
      hits ||= rangeIntersects.call(self as globalThis.Range, host);
    } catch {
      // not a Range: leave it to the native method to refuse
    }
    if (!hits) return false;
    clobberFromScript();
    return true;
  };
  methods(window.Range.prototype, ["deleteContents", "extractContents", "insertNode", "surroundContents"], rangeWrite);
  methods(Selection.prototype, ["deleteFromDocument"], (self) => {
    if (inEditorCall > 0 || host === null) return false;
    try {
      if (!selectionContains.call(self as Selection, host, true)) return false;
    } catch {
      return false;
    }
    clobberFromScript();
    return true;
  });
  freeze(window.Document.prototype, "execCommand", function (this: unknown, ...a: unknown[]) {
    if (inEditorCall === 0) return false;
    return (nativeExecCommand as unknown as AnyFn).apply(this, a);
  });

  // The range markers.
  methods(Element.prototype, ["setAttribute", "removeAttribute", "toggleAttribute"], (_s, a) =>
    srcWrite(isSrcName(a[0])),
  );
  methods(Element.prototype, ["setAttributeNS"], (_s, a) => srcWrite(isSrcName(localOf(a[1]))));
  methods(Element.prototype, ["removeAttributeNS"], (_s, a) => srcWrite(isSrcName(a[1])));
  methods(Element.prototype, ["setAttributeNode", "setAttributeNodeNS", "removeAttributeNode"], (_s, a) =>
    srcWrite(isSrcAttr(a[0])),
  );
  methods(NamedNodeMap.prototype, ["setNamedItem", "setNamedItemNS"], (_s, a) => srcWrite(isSrcAttr(a[0])));
  methods(NamedNodeMap.prototype, ["removeNamedItem"], (_s, a) => srcWrite(isSrcName(a[0])));
  methods(NamedNodeMap.prototype, ["removeNamedItemNS"], (_s, a) => srcWrite(isSrcName(a[1])));
  setters(Attr.prototype, ["value"], (s) => srcWrite(isSrcAttr(s)));
  Object.defineProperty(HTMLElement.prototype, "dataset", {
    configurable: false,
    enumerable: true,
    get(this: HTMLElement) {
      const map = datasetGet.call(this) as DOMStringMap;
      const locked = (p: string | symbol) => p === "bdSrc" && inEditorCall === 0;
      return new Proxy(map, {
        get: (t, p) => Reflect.get(t, p),
        set: (t, p, v) => (locked(p) ? true : Reflect.set(t, p, v)),
        deleteProperty: (t, p) => (locked(p) ? true : Reflect.deleteProperty(t, p)),
        defineProperty: (t, p, d) => (locked(p) ? true : Reflect.defineProperty(t, p, d)),
      });
    },
  });

  // Listeners, observers and timers run as whoever registered them.
  const wrapped = new WeakMap<object, AnyFn>();
  const wrapListener = (l: unknown): unknown => {
    if (l === null || (typeof l !== "function" && typeof l !== "object")) return l;
    let w = wrapped.get(l);
    if (!w) {
      const call: AnyFn =
        typeof l === "function"
          ? (l as AnyFn)
          : function (this: unknown, ...a: unknown[]) {
              if ("handleEvent" in l && typeof l.handleEvent === "function") return l.handleEvent(...a);
              return undefined;
            };
      w = forCaller(call);
      wrapped.set(l, w);
    }
    return w;
  };
  freeze(EventTarget.prototype, "addEventListener", function (this: EventTarget, type: string, l: unknown, o?: unknown) {
    return targetAddListener.call(this, type, wrapListener(l) as EventListener, o as AddEventListenerOptions);
  });
  freeze(EventTarget.prototype, "removeEventListener", function (this: EventTarget, type: string, l: unknown, o?: unknown) {
    const w = l !== null && (typeof l === "function" || typeof l === "object") ? wrapped.get(l) : undefined;
    return rawRemoveListener.call(this, type, (w ?? l) as EventListener, o as EventListenerOptions);
  });
  class GuardedMutationObserver extends RawMutationObserver {
    constructor(cb: MutationCallback) {
      super(forCaller(cb));
    }
  }
  freeze(window, "MutationObserver", GuardedMutationObserver);
  for (const name of ["setTimeout", "setInterval", "requestAnimationFrame", "requestIdleCallback", "queueMicrotask"]) {
    const native: unknown = Reflect.get(window, name);
    if (typeof native !== "function") continue;
    freeze(window, name, function (this: unknown, cb: unknown, ...rest: unknown[]) {
      return (native as AnyFn).call(window, typeof cb === "function" ? forCaller(cb as AnyFn) : cb, ...rest);
    });
  }
}
installGuards();

// The document as markup, or null if there is no live editor.
function currentHTML(): string | null {
  return view ? serializeInner(view.state.doc) : null;
}

function flush() {
  if (!host || !range) return;
  const html = currentHTML();
  if (html === null || html === before) return;
  before = html;
  const seq = ++patchSeq;
  const k = range.start + "," + range.end;
  const prev = editedSpans.get(k);
  const history = prev?.history ?? new Map<number, string>();
  history.set(seq, html);
  editedSpans.set(k, {
    start: range.start,
    end: range.end,
    html,
    seq,
    acked: false,
    ackedHtml: prev?.ackedHtml,
    history,
  });
  send({ type: "bd-edit:patch", seq, start: range.start, end: range.end, html });
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

  // Set up as the editor: every listener and observer ProseMirror registers
  // here runs as the editor, and only the editor may write into the element.
  view = editorSetup(
    () =>
      new EditorView(
        { mount: el },
        {
          state,
          editable: () => !locked,
          dispatchTransaction(tr) {
            if (locked && tr.docChanged) return;
            if (tr.docChanged && !trustedEditOpen && !composing) {
              /* The element changed with no input from the reader: the
                 page's own script wrote into it (or faked an event). That is
                 not an edit to save — close the editor and say so. */
              clobberFromScript();
              return;
            }
            view!.updateState(view!.state.apply(tr));
            /* Reported on EVERY change, with no debounce of its own.

               There used to be a 700ms idle here, mirroring the save timer.
               It meant that for 700ms after the last keystroke the edit
               existed ONLY inside this iframe — and pressing Done in that
               window tears the iframe down with the edit still pending,
               losing it silently. That is not an edge case: typing and
               immediately clicking Done is what finishing an edit looks like.

               There is nothing to gain by waiting. A patch is a postMessage
               into the same tab; the debounce that actually matters is the
               app's save timer, which still batches the writes that reach
               the hub. */
            if (tr.docChanged) flush();
          },
        },
      ),
  );
  view.focus();
  if (x !== undefined && y !== undefined) caretAt(view, x, y);
}

// ---- wiring ----

function nearestEditable(target: EventTarget | null): HTMLElement | null {
  const el = target instanceof Element ? (elClosest.call(target, `[${SRC_ATTR}]`) as Element | null) : null;
  return el instanceof HTMLElement ? el : null;
}

listen(
  document,
  "click",
  (ev: Event) => {
    // Only a person's click opens an editor: never el.click() from a script.
    if (!getIsTrusted.call(ev)) return;
    const e = ev as MouseEvent;
    if (locked) return; // no element opens or closes while Done saves
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
listen(
  document,
  "mouseover",
  (e: Event) => {
    const el = nearestEditable(e.target);
    if (el) editable(el);
  },
  true,
);

/* The rest needs the document's body, which this script — first in the
   document — runs ahead of. */
function initDOM() {
  /* A page whose own script rewrites the DOM can replace the element out from
     under an open editor, and the keystrokes go with it. There is no way to
     stop that from here — the page's script is the page — so the honest move
     is to notice and say so. Detaching is the observable form of every version
     of this: a React re-render, a chart redraw, a template swap. */
  new RawMutationObserver(
    asEditor(() => {
      if (host && !host.isConnected) clobberFromScript();
    }),
  ).observe(document.documentElement, { childList: true, subtree: true });

  const style = document.createElement("style");
  style.textContent = `
  [${SRC_ATTR}][data-bd-edit="yes"]:hover { outline: 1px dashed rgba(99,102,241,.7); outline-offset: 2px; cursor: text; }
  [${SRC_ATTR}][data-bd-edit="no"]:hover  { outline: 1px dotted rgba(120,120,120,.5); outline-offset: 2px; cursor: not-allowed; }
  [${SRC_ATTR}][data-bd-editing] { outline: 2px solid rgba(99,102,241,.9); outline-offset: 2px; }
  [${SRC_ATTR}][data-bd-editing]:focus { outline: 2px solid rgba(99,102,241,.9); }
`;
  (document.head ?? document.documentElement).appendChild(style);

  /* Bind each element to its range, now and once. A range the server did
     not stamp, or one two elements claim (a page script cloned or forged a
     marker), is bound to nothing. The element's content goes with its range,
     so the app can check it is what the source holds there. */
  const found: Array<{ el: Element; r: Range }> = [];
  const claims = new Map<string, number>();
  for (const el of docQueryAll.call(document, `[${SRC_ATTR}]`) as NodeListOf<Element>) {
    const r = parseRange(el);
    if (!r) continue;
    const k = r.start + "," + r.end;
    if (serverRanges !== null && !serverRanges.has(k)) continue;
    found.push({ el, r });
    claims.set(k, (claims.get(k) ?? 0) + 1);
  }
  const ranges: Array<{ start: number; end: number; html: string }> = [];
  for (const { el, r } of found) {
    if (claims.get(r.start + "," + r.end) !== 1) continue;
    const initialHtml = elInnerHTMLGet.call(el) as string;
    boundRanges.set(el, { start: r.start, end: r.end, initialHtml });
    ranges.push({ start: r.start, end: r.end, html: initialHtml });
  }
  send({ type: "bd-edit:ready", hash, len, ranges });

  // The markers are in the served HTML, so their presence says nothing about
  // whether this script has run. Anything waiting to interact — a test, or a
  // person — needs to know the listeners are actually installed, and this is
  // the only observable difference between "rendered" and "editable".
  document.documentElement.setAttribute("data-bd-ready", "");
}

/* Leaving with an edit the app never acknowledged: hand it to the exit
   worker, once per unload. What goes is recorded as acknowledged — the
   ancestor (`ackedHtml`) for an edit made after the page comes back from the
   bfcache, so that one neither drops nor repeats this. The worker, not this
   frame, decides whether and how it is saved. */
function exitUnacked() {
  if (!exitPort) return;
  const patches = unackedExitPatches(editedSpans.values());
  if (patches === null) return;
  for (const s of editedSpans.values()) {
    s.acked = true;
    s.ackedHtml = s.html;
  }
  portPost.call(exitPort, { type: "bd-edit:exit", patches });
}

listen(window, "beforeunload", () => {
  unmount();
  exitUnacked();
});
// Flush on pagehide too; keep the open element mounted when entering bfcache.
listen(window, "pagehide", (e: Event) => {
  if ((e as PageTransitionEvent).persisted) flush();
  else unmount();
  exitUnacked();
});

/* The app, on this frame's sealed port — never window:
   - bd-edit:ack     a patch is in the app's document
   - bd-edit:port    the exit worker's port (again, after a bfcache restore)
   - bd-edit:flush   Done: post what the open editor holds, then answer; the
                     answer follows every patch on the channel, so once the
                     app has it, it has them all. With `lock`, nothing more is
                     typed until bd-edit:unlock (its save failed or asked a
                     question) or the page is torn down (it saved). */
listen(appPort, "message", (e: Event) => {
  const msg = getMsgData.call(e) as { type?: unknown; id?: unknown; lock?: unknown; seq?: unknown } | null;
  if (!msg || typeof msg !== "object") return;
  if (msg.type === "bd-edit:ack" && typeof msg.seq === "number") {
    ackEditedSpan(editedSpans.values(), msg.seq);
    return;
  }
  if (msg.type === "bd-edit:port") {
    const next = (getMsgPorts.call(e) as ReadonlyArray<MessagePort>)[0];
    if (!next) return;
    if (exitPort) portClose.call(exitPort);
    exitPort = next;
    document.documentElement.setAttribute("data-bd-exit", "ready");
    return;
  }
  if (msg.type === "bd-edit:flush" && typeof msg.id === "number") {
    flush();
    if (msg.lock === true) {
      locked = true;
      view?.setProps({ editable: () => !locked });
    }
    send({ type: "bd-edit:flushed", id: msg.id });
    return;
  }
  if (msg.type === "bd-edit:unlock") {
    locked = false;
    view?.setProps({ editable: () => !locked });
  }
});
portStart.call(appPort);

if (document.readyState !== "loading") initDOM();
else listen(document, "DOMContentLoaded", initDOM, { once: true });

// The app's end of the channel, before any page script can see a message.
postToParent({ type: "bd-edit:hello" }, "*", [appChannel.port2]);
