import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import type { CommentAnchor, CommentThread } from "../api/types";
import { useCommentActions } from "../hooks/useComments";
import { describe, hitTest, rangeFor } from "../lib/anchor";
import { modalConfirm } from "../modal";
import { toast } from "../toast";
import { Icon } from "./shell";

/* Review comments on a file, Vercel-toolbar style: select text (or, on an
   HTML page, pick an element) and leave a comment where it is; the threads
   show as highlights with numbered pins and in a panel at the side. An agent
   sweeps the open ones with `bdrive comments`.

   Two surfaces, one model. Markdown and plain text render in this document,
   so selections and highlights are handled here directly. An HTML file
   renders in a sandboxed iframe this page cannot reach into, so the same
   work is done by inline-comment.ts inside it, and this side only trades
   messages with it — anchors down, selections and clicks up.

   Highlights use the CSS Custom Highlight API rather than wrapping text in
   <mark>s: the markdown view's DOM belongs to React's
   dangerouslySetInnerHTML, which re-applies its markup on unrelated updates
   and would silently drop anything patched into it. A highlight is a Range
   painted from outside, so the document is never touched. */

type Draft = { anchor: CommentAnchor; x: number; y: number; composing: boolean };
type Numbered = CommentThread & { n: number };

type FromFrame =
  | { type: "bd-comment:ready" }
  | { type: "bd-comment:select"; anchor: CommentAnchor | null; rect?: Rect | null }
  | { type: "bd-comment:picked"; anchor: CommentAnchor; rect: Rect }
  | { type: "bd-comment:open"; id: string }
  | { type: "bd-comment:pickoff" };
type Rect = { x: number; y: number; w: number; h: number };

const HL = "bd-comment";
const HL_ACTIVE = "bd-comment-active";

export function CommentSurface(props: {
  apiBase: string;
  path: string;
  // The file renders in a sandboxed iframe (HTML) rather than in this page.
  framed: boolean;
  // The rendered text IS the file (plain text), so a selection's source line
  // is known exactly and is recorded with the comment.
  lines: boolean;
  threads: CommentThread[];
  focus: string | null;
  onFocus: (id: string | null) => void;
  onOpenPanel: () => void;
  picking: boolean;
  onPicking: (on: boolean) => void;
  children: ReactNode;
}) {
  const { apiBase, path, framed, lines, threads, focus, onFocus, onOpenPanel, picking, onPicking } = props;
  /* The surface adds no element around the file view: .page's children are
     a layout other code relies on (the frontmatter rail is a grid on .page,
     and specs read its direct children). A hidden marker finds the page, and
     everything this draws goes after the file view, marked data-bd-ignore
     so it is never mistaken for document text. */
  const marker = useRef<HTMLSpanElement>(null);
  const [page, setPage] = useState<HTMLElement | null>(null);
  useLayoutEffect(() => setPage(marker.current?.parentElement ?? null), []);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [body, setBody] = useState("");
  const [busy, setBusy] = useState(false);
  const actions = useCommentActions(apiBase);
  const numbered = useMemo(() => numberThreads(threads), [threads]);
  // Resolved threads leave the page: they are history, found in the panel.
  const shown = useMemo(() => numbered.filter((t) => !t.resolved && !t.missing), [numbered]);

  useEffect(() => {
    setDraft(null);
    setBody("");
  }, [path]);

  /* ---- the in-page surface (markdown, text) ---- */

  const ranges = useRef(new Map<string, Range>());
  const [pins, setPins] = useState<{ id: string; n: number; c: number; k: number; x: number; y: number }[]>([]);
  const [tick, setTick] = useState(0);

  // The rendered document changes under us (a refetch, a diagram landing,
  // React re-applying the markdown), and every Range into the old DOM goes
  // dead with it. Re-anchor on any change, once per frame.
  useEffect(() => {
    if (framed || !page) return;
    let queued = 0;
    const bump = () => {
      if (queued) return;
      queued = requestAnimationFrame(() => {
        queued = 0;
        setTick((t) => t + 1);
      });
    };
    // Only the document's own changes: a pin moving is a mutation too, and
    // reacting to it would re-place the pins forever.
    const mo = new MutationObserver((records) => {
      if (records.some((r) => !ignored(r.target))) bump();
    });
    mo.observe(page, { childList: true, subtree: true, characterData: true });
    const ro = new ResizeObserver(bump);
    ro.observe(page);
    return () => {
      mo.disconnect();
      ro.disconnect();
      cancelAnimationFrame(queued);
    };
  }, [framed, page]);

  useLayoutEffect(() => {
    if (framed || !page) return;
    const map = new Map<string, Range>();
    for (const t of shown) {
      const r = t.anchor.quote ? rangeFor(page, t.anchor) : null;
      if (r) map.set(t.id, r);
    }
    ranges.current = map;
    const reg = highlightRegistry();
    if (reg) {
      reg.set(HL, new Highlight(...[...map].filter(([id]) => id !== focus).map(([, r]) => r)));
      const act = focus ? map.get(focus) : undefined;
      if (act) reg.set(HL_ACTIVE, new Highlight(act));
      else reg.delete(HL_ACTIVE);
    }
    // Pins sit in the left gutter beside the line their text starts on, so
    // they never cover what was quoted. The gutter is the scroller's padding
    // and anything past it is clipped, so threads starting on one line stack
    // downwards (style.css, --k) rather than fanning out sideways.
    const base = page.getBoundingClientRect();
    const next: typeof pins = [];
    const perLine = new Map<number, number>();
    for (const t of shown) {
      const box = map.get(t.id)?.getClientRects()[0];
      if (!box) continue;
      const y = Math.round(box.top - base.top + box.height / 2);
      const k = perLine.get(y) ?? 0;
      perLine.set(y, k + 1);
      next.push({ id: t.id, n: t.n, c: t.messages.length, k, x: -38, y });
    }
    setPins(next);
  }, [shown, focus, tick, framed, page]);

  // While a comment is being written its text stays marked, since focusing
  // the composer takes the browser's own selection away.
  const draftRange = useRef<Range | null>(null);
  useEffect(() => {
    const reg = highlightRegistry();
    if (!reg || framed) return;
    if (draft?.composing && draftRange.current) reg.set(HL_ACTIVE, new Highlight(draftRange.current));
    else setTick((t) => t + 1);
  }, [draft?.composing, framed]);

  useEffect(
    () => () => {
      highlightRegistry()?.delete(HL);
      highlightRegistry()?.delete(HL_ACTIVE);
    },
    [],
  );

  // A thread chosen in the panel scrolls its text into view.
  useEffect(() => {
    if (!focus || framed) return;
    const r = ranges.current.get(focus);
    const el = r?.startContainer.parentElement;
    el?.scrollIntoView({ block: "center", behavior: "smooth" });
  }, [focus, framed]);

  // Layout, not passive: the document can be on screen in the same commit
  // that mounts this surface, and a selection made before a passive effect
  // ran would find no listener.
  useLayoutEffect(() => {
    if (framed || !page) return;
    const onSelect = (e: Event) => {
      if (ignored(e.target as Node)) return;
      if (e instanceof KeyboardEvent && !e.shiftKey) return;
      // After the browser has settled the selection the mouseup made.
      setTimeout(() => {
        const sel = getSelection();
        if (!sel || sel.isCollapsed || !sel.rangeCount) {
          setDraft((d) => (d?.composing ? d : null));
          return;
        }
        const range = sel.getRangeAt(0);
        if (!page.contains(range.commonAncestorContainer) || ignored(range.commonAncestorContainer)) return;
        // In a plain-text view the <pre> holds the file verbatim, so measuring
        // within it gives the source line exactly.
        const pre = lines ? (range.startContainer.parentElement?.closest("pre.plain") ?? null) : null;
        const anchor = pre && pre.contains(range.endContainer)
          ? describe(pre, range, sel.toString(), true)
          : describe(page, range, sel.toString());
        if (!anchor) return;
        const b = range.getBoundingClientRect();
        draftRange.current = range.cloneRange();
        setDraft({ anchor, x: b.right, y: b.bottom, composing: false });
      }, 0);
    };
    const onClick = (e: MouseEvent) => {
      if (ignored(e.target as Node)) return;
      const sel = getSelection();
      if (sel && !sel.isCollapsed) return;
      const hit = hitTest(document, e.clientX, e.clientY, ranges.current);
      if (hit) {
        onFocus(hit);
        onOpenPanel();
      }
    };
    page.addEventListener("mouseup", onSelect);
    page.addEventListener("keyup", onSelect);
    page.addEventListener("click", onClick);
    return () => {
      page.removeEventListener("mouseup", onSelect);
      page.removeEventListener("keyup", onSelect);
      page.removeEventListener("click", onClick);
    };
  }, [framed, page, lines, onFocus, onOpenPanel]);

  /* ---- the framed surface (HTML) ---- */

  const frame = useCallback(() => page?.querySelector<HTMLIFrameElement>("iframe.htmlview") ?? null, [page]);
  const toFrame = useCallback(
    (msg: Record<string, unknown>) => {
      frame()?.contentWindow?.postMessage(msg, "*");
    },
    [frame],
  );
  const anchorsDown = useCallback(() => {
    toFrame({
      type: "bd-comment:anchors",
      // Anchors only — never the comment text. See inline-comment.ts.
      anchors: shown.map((t) => ({ id: t.id, n: t.n, ...t.anchor })),
      active: focus,
    });
  }, [shown, focus, toFrame]);

  useEffect(() => {
    if (framed) anchorsDown();
  }, [framed, anchorsDown]);
  useEffect(() => {
    if (framed) toFrame({ type: "bd-comment:pick", on: picking });
  }, [framed, picking, toFrame]);
  useEffect(() => {
    if (framed && focus) toFrame({ type: "bd-comment:focus", id: focus });
  }, [framed, focus, toFrame]);

  useEffect(() => {
    if (!framed) return;
    const onMessage = (e: MessageEvent) => {
      // The frame is sandboxed without allow-same-origin, so its origin is
      // "null" and proves nothing; identity comes from the window. And what
      // it says is still the page's word — the page runs its own scripts —
      // so it only ever opens a composer, and everything it reported is shown
      // in that composer (AnchorView) before anything is stored.
      const f = frame();
      if (!f || e.source !== f.contentWindow) return;
      const msg = e.data as FromFrame;
      if (!msg || typeof msg !== "object") return;
      const off = f.getBoundingClientRect();
      switch (msg.type) {
        case "bd-comment:ready":
          anchorsDown();
          toFrame({ type: "bd-comment:pick", on: picking });
          break;
        case "bd-comment:select":
          if (!msg.anchor || !msg.rect) setDraft((d) => (d?.composing ? d : null));
          else setDraft({ anchor: msg.anchor, x: off.left + msg.rect.x + msg.rect.w, y: off.top + msg.rect.y + msg.rect.h, composing: false });
          break;
        case "bd-comment:picked":
          onPicking(false);
          setDraft({ anchor: msg.anchor, x: off.left + msg.rect.x + Math.min(msg.rect.w, 320), y: off.top + msg.rect.y + Math.min(msg.rect.h, 60), composing: true });
          break;
        case "bd-comment:open":
          if (typeof msg.id === "string") {
            onFocus(msg.id);
            onOpenPanel();
          }
          break;
        case "bd-comment:pickoff":
          onPicking(false);
          break;
      }
    };
    addEventListener("message", onMessage);
    return () => removeEventListener("message", onMessage);
  }, [framed, frame, anchorsDown, toFrame, picking, onPicking, onFocus, onOpenPanel]);

  /* ---- the floating button and composer ---- */

  // Scrolling moves the text out from under a floating button; drop it,
  // unless the reader is already typing into it.
  useEffect(() => {
    const content = document.getElementById("content");
    const drop = () => setDraft((d) => (d?.composing ? d : null));
    content?.addEventListener("scroll", drop, { passive: true });
    return () => content?.removeEventListener("scroll", drop);
  }, []);

  const submit = async () => {
    if (!draft || !body.trim() || busy) return;
    setBusy(true);
    try {
      const t = await actions.create(path, body, draft.anchor);
      setDraft(null);
      setBody("");
      getSelection()?.removeAllRanges();
      if (framed) toFrame({ type: "bd-comment:clear" });
      onFocus(t.id);
      onOpenPanel();
    } catch (e) {
      toast("Could not save the comment: " + (e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const pos = draft ? clampToViewport(draft.x, draft.y, draft.composing ? 320 : 120, draft.composing ? 170 : 34) : null;

  return (
    <>
      {props.children}
      <span ref={marker} hidden data-bd-ignore="" />
      {!framed &&
        pins.map((p) => (
          <button
            key={p.id}
            data-bd-ignore=""
            className={"cm-pin" + (p.id === focus ? " active" : "")}
            style={{ left: p.x, top: p.y, "--k": p.k } as CSSProperties}
            title={`Comment ${p.n} · ${p.c} message${p.c === 1 ? "" : "s"}`}
            aria-label={`Open comment ${p.n}`}
            onClick={() => {
              onFocus(p.id);
              onOpenPanel();
            }}
          >
            <Icon name="comment" />
            {p.n}
          </button>
        ))}
      {draft && pos && !draft.composing && (
        <button
          data-bd-ignore=""
          className="cm-float"
          style={{ left: pos.x, top: pos.y }}
          // Not on mousedown: that would collapse the selection the comment is about.
          onMouseDown={(e) => e.preventDefault()}
          onClick={() => setDraft({ ...draft, composing: true })}
        >
          <Icon name="comment" /> Comment
        </button>
      )}
      {draft && pos && draft.composing && (
        <div data-bd-ignore="" className="cm-compose" style={{ left: pos.x, top: pos.y }} role="dialog" aria-label="New comment">
          <AnchorView a={draft.anchor} />
          <textarea
            autoFocus
            value={body}
            placeholder="Leave feedback… (⌘↵ to post)"
            onChange={(e) => setBody(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) void submit();
              if (e.key === "Escape") {
                setDraft(null);
                setBody("");
              }
            }}
          />
          <div className="cm-row">
            <Button variant="subtle" onClick={() => { setDraft(null); setBody(""); }}>
              Cancel
            </Button>
            <Button variant="primary" disabled={!body.trim() || busy} onClick={() => void submit()}>
              Comment
            </Button>
          </div>
        </div>
      )}
    </>
  );
}

/* The thread list beside the page. */
export function CommentPanel(props: {
  apiBase: string;
  path: string;
  threads: CommentThread[];
  focus: string | null;
  onFocus: (id: string | null) => void;
  onClose: () => void;
  framed: boolean;
  picking: boolean;
  onPicking: (on: boolean) => void;
  me?: string;
  admin: boolean;
}) {
  const { apiBase, path, threads, focus, onFocus, onClose, framed, picking, onPicking, me, admin } = props;
  const actions = useCommentActions(apiBase);
  const numbered = useMemo(() => numberThreads(threads), [threads]);
  const open = numbered.filter((t) => !t.resolved);
  const done = numbered.filter((t) => t.resolved);
  const [tab, setTab] = useState<"open" | "resolved">("open");
  // A focused thread that is resolved lives on the other tab; follow it.
  const focusResolved = !!focus && done.some((t) => t.id === focus);
  useEffect(() => {
    if (focusResolved) setTab("resolved");
  }, [focus, focusResolved]);
  const list = tab === "open" ? open : done;
  const [general, setGeneral] = useState("");
  const [busy, setBusy] = useState(false);

  const postGeneral = async () => {
    if (!general.trim() || busy) return;
    setBusy(true);
    try {
      const t = await actions.create(path, general);
      setGeneral("");
      setTab("open");
      onFocus(t.id);
    } catch (e) {
      toast("Could not save the comment: " + (e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <aside className="cm-panel" aria-label="Comments">
      <header className="cm-head">
        <b>Comments</b>
        <div className="cm-tabs" role="tablist">
          <button role="tab" aria-selected={tab === "open"} onClick={() => setTab("open")}>
            Open{open.length > 0 && <span> · {open.length}</span>}
          </button>
          <button role="tab" aria-selected={tab === "resolved"} onClick={() => setTab("resolved")}>
            Resolved{done.length > 0 && <span> · {done.length}</span>}
          </button>
        </div>
        <button className="cm-x" onClick={onClose} aria-label="Close comments" title="Close">
          <Icon name="x" />
        </button>
      </header>
      {framed && (
        <div className="cm-pickbar">
          <Button variant="toolbar" aria-pressed={picking} onClick={() => onPicking(!picking)}>
            <Icon name="plus" /> {picking ? "Click an element on the page… (esc)" : "Pick an element"}
          </Button>
        </div>
      )}
      <div className="cm-list">
        {list.length === 0 && (
          <p className="cm-empty">
            {tab === "open"
              ? framed
                ? "No open comments. Select text on the page, or pick an element, to leave one."
                : "No open comments. Select text on the page to leave one."
              : "Nothing resolved yet."}
          </p>
        )}
        {list.map((t) => (
          <ThreadCard
            key={t.id}
            t={t}
            active={t.id === focus}
            onFocus={() => onFocus(t.id === focus ? null : t.id)}
            onJump={() => {
              // Re-focusing an already-focused thread must still scroll.
              onFocus(null);
              requestAnimationFrame(() => onFocus(t.id));
            }}
            actions={actions}
            canDelete={admin || (!!me && t.author === me.toLowerCase())}
          />
        ))}
      </div>
      <footer className="cm-foot">
        <textarea
          value={general}
          placeholder="Comment on the whole file…"
          onChange={(e) => setGeneral(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) void postGeneral();
          }}
        />
        <div className="cm-row">
          <span className="cm-hint">
            Agents sweep these with <code>bdrive comments</code>
          </span>
          <Button variant="primary" disabled={!general.trim() || busy} onClick={() => void postGeneral()}>
            Comment
          </Button>
        </div>
      </footer>
    </aside>
  );
}

function ThreadCard(props: {
  t: Numbered;
  active: boolean;
  onFocus: () => void;
  onJump: () => void;
  actions: ReturnType<typeof useCommentActions>;
  canDelete: boolean;
}) {
  const { t, active, onFocus, onJump, actions, canDelete } = props;
  // Only open threads are anchored in the page (CommentSurface marks those),
  // so a resolved one has no text to jump to.
  const anchored = !!(t.anchor.quote || t.anchor.element) && !t.missing && !t.resolved;
  const [reply, setReply] = useState("");
  const [busy, setBusy] = useState(false);
  const card = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (active) card.current?.scrollIntoView({ block: "nearest", behavior: "smooth" });
  }, [active]);

  const run = async (f: () => Promise<unknown>) => {
    if (busy) return;
    setBusy(true);
    try {
      await f();
    } catch (e) {
      toast((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const send = (action: "reply" | "resolve" | "reopen") =>
    run(async () => {
      await actions.act(t.id, action, reply);
      setReply("");
    });

  return (
    <div ref={card} className={"cm-card" + (active ? " active" : "") + (t.resolved ? " resolved" : "")} data-comment={t.id}>
      <div className="cm-card-top" onClick={onFocus}>
        <span className="cm-n">{t.n}</span>
        <AnchorView a={t.anchor} />
      </div>
      {(t.outdated || t.missing) && (
        <div className="cm-flag">{t.missing ? "The file no longer exists" : "The file changed since this comment"}</div>
      )}
      {t.messages.map((m) => (
        <div key={m.id} className="cm-msg">
          <span className="cm-av" aria-hidden="true">
            {initials(m.name || m.author || "?")}
          </span>
          <div className="cm-msg-main">
            <div className="cm-who">
              <b>{m.name || m.author || "someone"}</b> <time dateTime={m.time} title={new Date(m.time).toLocaleString()}>{ago(m.time)}</time>
            </div>
            <div className="cm-body">{m.body}</div>
          </div>
        </div>
      ))}
      {/* `bdrive comments` lists every open thread, so an open thread is
          exactly what the next agent sweep picks up. */}
      {!t.resolved && !t.missing && (
        <div className="cm-queued">
          <Icon name="sparkle" />
          <span>
            Queued for next agent sweep · <code>bdrive comments</code>
          </span>
        </div>
      )}
      {t.resolved && t.resolved_at && (
        <div className="cm-resolved">
          <Icon name="check" /> Resolved by {t.resolved_name || t.resolved_by || "someone"} {ago(t.resolved_at)}
        </div>
      )}
      <textarea
        className="cm-reply"
        value={reply}
        rows={reply ? 3 : 1}
        placeholder={t.resolved ? "Add a note…" : "Reply…"}
        onChange={(e) => setReply(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && reply.trim()) void send("reply");
        }}
      />
      <div className="cm-row">
        {canDelete && (
          <button
            className="cm-del"
            title="Delete this thread"
            aria-label="Delete this thread"
            onClick={() =>
              void run(async () => {
                if (await modalConfirm("Delete comment", "Delete this comment thread for everyone?", "Delete", true))
                  await actions.remove(t.id);
              })
            }
          >
            <Icon name="trash" />
          </button>
        )}
        <Button variant="toolbar" className="ghost cm-act" disabled={busy} onClick={() => void send(t.resolved ? "reopen" : "resolve")}>
          {!t.resolved && <Icon name="check" />}
          {t.resolved ? "Reopen" : reply.trim() ? "Reply & resolve" : "Resolve"}
        </Button>
        {anchored && (
          <Button variant="toolbar" className="ghost cm-act cm-jump" onClick={onJump}>
            <Icon name="jump" /> Jump to text
          </Button>
        )}
        <span className="cm-spacer" />
        {reply.trim() && (
          <Button variant="subtle" disabled={busy} onClick={() => void send("reply")}>
            Reply
          </Button>
        )}
      </div>
    </div>
  );
}

/* Everything stored in an anchor is shown, whole — context included, never
   clipped. On an HTML page all of it comes from the page's own script, and
   what an agent later reads about a comment must be something the reviewer
   could read before posting it, not a tail that rode along out of sight. A
   long quote scrolls inside its box rather than being cut. */
function AnchorView({ a }: { a: CommentAnchor }) {
  if (!a.quote && !a.element) return <div className="cm-quote cm-el">Whole file</div>;
  return (
    <div className="cm-anchor">
      {a.quote && (
        <div className="cm-quote">
          {a.prefix && <span className="cm-ctx">{a.prefix}</span>}
          <span className="cm-q">{a.quote}</span>
          {a.suffix && <span className="cm-ctx">{a.suffix}</span>}
        </div>
      )}
      {a.element && <div className="cm-quote cm-el">{a.element}</div>}
    </div>
  );
}

function initials(name: string): string {
  const words = name.trim().split(/[\s@._-]+/).filter(Boolean);
  if (!words.length) return "?";
  return (words[0][0] + (words[1]?.[0] ?? "")).toUpperCase();
}

// Numbers follow creation order, so a pin and its card agree and a number
// does not change when another thread is resolved.
function numberThreads(threads: CommentThread[]): Numbered[] {
  return [...threads]
    .sort((a, b) => a.created.localeCompare(b.created))
    .map((t, i) => ({ ...t, n: i + 1 }));
}

// Inside something this surface drew (a pin, the composer).
function ignored(n: Node | null): boolean {
  const el = n instanceof Element ? n : n?.parentElement;
  return !!el?.closest("[data-bd-ignore]");
}

function highlightRegistry(): HighlightRegistry | null {
  return typeof CSS !== "undefined" && "highlights" in CSS ? CSS.highlights : null;
}

function clampToViewport(x: number, y: number, w: number, h: number) {
  return {
    x: Math.max(8, Math.min(x - w / 2, innerWidth - w - 8)),
    y: Math.max(60, Math.min(y + 8, innerHeight - h - 8)),
  };
}


function ago(iso: string): string {
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto" });
  if (s < 60) return "just now";
  if (s < 3600) return rtf.format(-Math.round(s / 60), "minute");
  if (s < 86400) return rtf.format(-Math.round(s / 3600), "hour");
  if (s < 86400 * 30) return rtf.format(-Math.round(s / 86400), "day");
  return new Date(iso).toLocaleDateString();
}
