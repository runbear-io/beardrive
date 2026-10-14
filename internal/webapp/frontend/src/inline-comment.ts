/* Commenting, running INSIDE the sandboxed iframe that renders a synced HTML
   file.

   A separate bundle from the app, loaded only by the ?comment=1 render
   (comments.go's commentSuffix). The iframe keeps exactly the sandbox it
   always had — no allow-same-origin — so this cannot reach the hub's session
   and knows nothing about the comments themselves beyond where they point.

   It does three things, all by postMessage:
   - reports what the reader selected (or, in pick mode, clicked), so the app
     can offer to comment on it;
   - highlights the places existing comments point at, with a numbered pin
     beside each, when the app sends their anchors down;
   - reports a click on one of those, so the app can open the thread.

   What comes down is anchors only — quote, context, element path, an id and
   a number. Never a comment's text: this frame runs the page's own scripts,
   which can talk to the network, and the quotes are the page's own text
   while the comments are not. */

import { cssPath, describe, hitTest, rangeFor, type Anchor } from "./lib/anchor";

type Down =
  | { type: "bd-comment:anchors"; anchors: (Anchor & { id: string; n: number })[]; active: string | null }
  | { type: "bd-comment:pick"; on: boolean }
  | { type: "bd-comment:focus"; id: string }
  | { type: "bd-comment:clear" };

(() => {
  if (window.parent === window) return;
  const send = (msg: Record<string, unknown>) => parent.postMessage(msg, "*");
  const rect = (r: DOMRect) => ({ x: r.left, y: r.top, w: r.width, h: r.height });

  let anchors: (Anchor & { id: string; n: number })[] = [];
  let active: string | null = null;
  let picking = false;
  let ranges = new Map<string, Range>();
  let elements = new Map<string, Element>();

  /* The pins live in a closed shadow root hung off <html>, outside <body>:
     the page's own CSS cannot restyle them, and its scripts walking the body
     do not trip over them. */
  const host = document.createElement("bd-comment-layer");
  host.setAttribute("data-bd-ignore", "");
  host.style.cssText = "position:absolute;left:0;top:0;width:0;height:0;z-index:2147483647;";
  const shadow = host.attachShadow({ mode: "closed" });
  shadow.innerHTML = `<style>
    .pin { position: absolute; min-width: 20px; height: 20px; padding: 0 5px; box-sizing: border-box;
      border-radius: 10px 10px 10px 2px; background: #f5a623; color: #1a1204; font: 600 11px/20px -apple-system, system-ui, sans-serif;
      text-align: center; cursor: pointer; box-shadow: 0 2px 6px rgba(0,0,0,.35); user-select: none; }
    .pin.resolved { background: #9aa0a9; }
    .pin.active { outline: 2px solid #1a1204; outline-offset: 1px; }
    .hover { position: absolute; pointer-events: none; outline: 2px solid #f5a623; outline-offset: 2px; border-radius: 3px; }
  </style><div class="pins"></div><div class="hover" hidden></div>`;
  const pinBox = shadow.querySelector(".pins") as HTMLElement;
  const hoverBox = shadow.querySelector(".hover") as HTMLElement;
  const style = document.createElement("style");
  style.setAttribute("data-bd-ignore", "");
  style.textContent =
    "::highlight(bd-comment){background-color:rgba(245,166,35,.28)}" +
    "::highlight(bd-comment-active){background-color:rgba(245,166,35,.6)}";
  const mount = () => {
    if (!host.isConnected) document.documentElement.appendChild(host);
    if (!style.isConnected) (document.head || document.documentElement).appendChild(style);
  };

  const highlights = (globalThis as { CSS?: { highlights?: Map<string, unknown> } }).CSS?.highlights;
  const Highlight = (globalThis as { Highlight?: new (...r: Range[]) => unknown }).Highlight;

  function place() {
    mount();
    ranges = new Map();
    elements = new Map();
    for (const a of anchors) {
      if (a.quote) {
        const r = rangeFor(document.body, a);
        if (r) ranges.set(a.id, r);
      } else if (a.element) {
        try {
          const el = document.querySelector("body > " + a.element) || document.querySelector(a.element);
          if (el) elements.set(a.id, el);
        } catch {
          /* a selector this page no longer parses */
        }
      }
    }
    if (highlights && Highlight) {
      const open = anchors.filter((a) => ranges.has(a.id) && a.id !== active).map((a) => ranges.get(a.id)!);
      highlights.set("bd-comment", new Highlight(...open));
      const act = active && ranges.get(active);
      if (act) highlights.set("bd-comment-active", new Highlight(act));
      else highlights.delete("bd-comment-active");
    }
    pinBox.textContent = "";
    for (const a of anchors) {
      const box = ranges.get(a.id)?.getClientRects()[0] ?? elements.get(a.id)?.getBoundingClientRect();
      if (!box) continue;
      const pin = document.createElement("div");
      pin.className = "pin" + (a.id === active ? " active" : "");
      pin.textContent = String(a.n);
      // The tail (bottom-left corner) points at where the quote starts; a page
      // has no gutter of its own to put it in.
      pin.style.left = Math.max(0, box.left + scrollX - 8) + "px";
      pin.style.top = Math.max(0, box.top + scrollY - 20) + "px";
      pin.addEventListener("click", (e) => {
        e.stopPropagation();
        send({ type: "bd-comment:open", id: a.id });
      });
      pinBox.appendChild(pin);
    }
  }

  // The page can re-lay itself out at any time (fonts, images, its own
  // scripts); pins follow, coalesced to one pass per frame.
  let queued = false;
  const replace = () => {
    if (queued) return;
    queued = true;
    requestAnimationFrame(() => {
      queued = false;
      place();
    });
  };
  addEventListener("resize", replace);
  addEventListener("load", replace);
  new ResizeObserver(replace).observe(document.documentElement);

  // ---- selection ----

  const reportSelection = () =>
    setTimeout(() => {
      if (picking) return;
      const sel = getSelection();
      if (!sel || sel.isCollapsed || !sel.rangeCount) {
        send({ type: "bd-comment:select", anchor: null });
        return;
      }
      const r = sel.getRangeAt(0);
      const a = describe(document.body, r, sel.toString());
      send({ type: "bd-comment:select", anchor: a, rect: a ? rect(r.getBoundingClientRect()) : null });
    }, 0);
  document.addEventListener("mouseup", reportSelection);
  document.addEventListener("keyup", (e) => {
    if (e.key === "Escape" && picking) {
      setPick(false);
      send({ type: "bd-comment:pickoff" });
      return;
    }
    if (e.shiftKey || e.key.startsWith("Arrow")) reportSelection();
  });

  // ---- pick mode: comment on any element ----

  function setPick(on: boolean) {
    picking = on;
    hoverBox.hidden = true;
    document.documentElement.style.cursor = on ? "crosshair" : "";
  }
  document.addEventListener(
    "mousemove",
    (e) => {
      if (!picking) return;
      const el = e.target as Element;
      if (!el || el === document.documentElement || el === document.body) {
        hoverBox.hidden = true;
        return;
      }
      const b = el.getBoundingClientRect();
      Object.assign(hoverBox.style, {
        left: b.left + scrollX + "px", top: b.top + scrollY + "px",
        width: b.width + "px", height: b.height + "px",
      });
      hoverBox.hidden = false;
    },
    true,
  );
  document.addEventListener(
    "click",
    (e) => {
      if (picking) {
        // The page's own handlers never see a click meant for a comment.
        e.preventDefault();
        e.stopPropagation();
        const el = e.target as Element;
        if (!el || el === document.documentElement) return;
        const text = ((el as HTMLElement).innerText || el.textContent || "").replace(/\s+/g, " ").trim();
        const anchor: Anchor = { element: cssPath(el) };
        // Short text identifies the element for an agent better than a
        // selector does; a whole section's worth would only be noise.
        if (text && text.length <= 160) anchor.quote = text;
        setPick(false);
        send({ type: "bd-comment:picked", anchor, rect: rect(el.getBoundingClientRect()) });
        return;
      }
      const sel = getSelection();
      if (sel && !sel.isCollapsed) return;
      const hit = hitTest(document, e.clientX, e.clientY, ranges);
      if (hit) send({ type: "bd-comment:open", id: hit });
    },
    true,
  );

  // ---- from the app ----

  addEventListener("message", (e: MessageEvent) => {
    // Only the embedding app: the page's own scripts post from this window.
    if (e.source !== parent) return;
    const msg = e.data as Down;
    if (!msg || typeof msg !== "object") return;
    switch (msg.type) {
      case "bd-comment:anchors":
        anchors = Array.isArray(msg.anchors) ? msg.anchors : [];
        active = msg.active;
        place();
        break;
      case "bd-comment:pick":
        setPick(!!msg.on);
        break;
      case "bd-comment:focus": {
        active = msg.id;
        place();
        const r = ranges.get(msg.id)?.getBoundingClientRect() ?? elements.get(msg.id)?.getBoundingClientRect();
        if (r) scrollTo({ top: r.top + scrollY - innerHeight / 3, behavior: "smooth" });
        break;
      }
      case "bd-comment:clear":
        getSelection()?.removeAllRanges();
        break;
    }
  });

  send({ type: "bd-comment:ready" });
})();
