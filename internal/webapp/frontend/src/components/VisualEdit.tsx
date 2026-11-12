import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import * as Y from "yjs";
import { openSharedFile, type SaveState, type SharedFile } from "../lib/sharedfile";
import { diffSplices, merge3PreferRight } from "../lib/merge3";
import { frameChannel, saveLocked } from "../lib/frameflush";
import { type CollabStatus } from "../lib/collab";
import { askShrink, withdrawShrink } from "./ShrinkDialog";
import { withReceipt } from "../hooks/useBlob";

/* Click-to-edit for a synced HTML file: the app's half.

   The iframe renders the file with ?edit=1, which the server answers with the
   same bytes plus a marker on every element whose text came literally from the
   source (server.go's serveEditable, internal/webapp/htmledit.go). The script
   it injects reports a patch when someone edits one of them — an element's new
   inner HTML, and the byte range in the source it belongs to.

   This component turns that into a splice.

   Nothing here re-serializes the document. That is the whole point: the file
   keeps its indentation, its comments and its <script> blocks, because the
   only bytes that move are the ones inside the element that was edited. An
   agent re-reading the file afterwards sees what it wrote, and the history
   diff is one line rather than a whole-file rewrite.

   The CRDT underneath is the SAME Y.Text the source editor binds CodeMirror
   to — the file's source as plain text. So somebody clicking a headline here
   and somebody typing markup in the source editor are in one room, and a
   patch from either merges with the other. */

// What the injected script sends up. The one thing sent down is a flush
// request (lib/frameflush.ts), answered by `bd-edit:flushed` after every patch
// the frame held.
type Ready = { type: "bd-edit:ready"; hash: string; len: number; ranges: Span[] };
// `seq` numbers the frame's patches; each is acknowledged (`bd-edit:ack`) once
// it is in the document, and one never acknowledged is handed on the way out
// to this component's exit worker (lib/editexit.worker.ts).
type Patch = { type: "bd-edit:patch"; seq?: number; start: number; end: number; html: string };
type Clobbered = { type: "bd-edit:clobbered" };
type Flushed = { type: "bd-edit:flushed"; id: number };
type FromFrame = Ready | Patch | Clobbered | Flushed;

// `html`: the element's content as the frame found it when it bound the range
// (inline-edit.ts initDOM), checked against the source before it is anchored.
type Span = { start: number; end: number; html?: string };

/* Markup as the browser's own HTML parser reads it back: a <template> parses
   with the real HTML5 parser and serializes the result, so two spellings of
   the same content (entities, void tags, attribute quoting) compare equal —
   while nothing in it runs or loads. The editor's own markers are not
   content: a stamped child's data-bd-src (and the edit verdicts) are dropped. */
const EDITOR_ATTRS = ["data-bd-src", "data-bd-edit", "data-bd-editing"];
function canonicalHTML(raw: string): string {
  const tpl = document.createElement("template");
  tpl.innerHTML = raw;
  for (const el of tpl.content.querySelectorAll("*")) {
    for (const a of EDITOR_ATTRS) el.removeAttribute(a);
  }
  return tpl.innerHTML;
}

/* A hash of the document, when the browser will give us one.

   crypto.subtle is only defined in a secure context, so a self-hosted hub
   reached over plain HTTP on a LAN — which BearDrive supports and documents —
   has no WebCrypto at all. Returning null there is deliberate: the caller
   falls back to comparing lengths, which is weaker but real, rather than
   throwing and leaving click-to-edit silently inert on those hubs. */
async function sha256Hex(s: string): Promise<string | null> {
  if (!globalThis.crypto?.subtle) return null;
  try {
    const buf = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
    return [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, "0")).join("");
  } catch {
    return null;
  }
}

/* Automatic re-renders after the frame's ranges stopped describing the
   document, per (project, file, base version). A page whose served bytes
   never match the shared document — a hub process serving an older fold, a
   script that rewrites the source — used to reload as fast as it could load
   (55 times in 7s). Each retry now waits twice as long as the last, and past
   the cap the editor stops and says so. Module-level so a remount (each
   reload is one) does not reset the count. */
const MISMATCH_BACKOFF_MS = [250, 500, 1000, 2000, 4000];
const mismatchReloads = new Map<string, number>();

export function VisualEdit({
  apiBase,
  fileURL,
  path,
  initial,
  me,
  onStateChange,
  onCollab,
  onPeers,
  onConflictCopy,
  baseSha,
  onSaved,
  onWriting,
  onRendered,
  handle,
}: {
  apiBase: string;
  fileURL: string;
  path: string;
  initial: string;
  me?: { name: string; colour: string };
  onStateChange?: (s: SaveState) => void;
  onCollab?: (s: CollabStatus) => void;
  onPeers?: (n: number) => void;
  onConflictCopy?: (path: string) => void;
  baseSha?: string;
  onSaved?: (text: string) => void;
  onWriting?: () => void;
  onRendered?: () => void;
  // The open file, for a caller that must save it now (EditView's Done).
  handle?: { current: SharedFile | null };
}) {
  const frame = useRef<HTMLIFrameElement>(null);
  const [warning, setWarning] = useState<string | null>(null);
  /* Whether clicks will actually do anything yet.

     Arming needs the shared document to hold the file, and it can fail to:
     a room whose seed claim is held by a client that never filled it hands
     out an empty document forever. Before this, that state looked exactly
     like a working editor — text took keystrokes, nothing ever saved, and
     nothing said why. A visible "not ready" beats a silent no-op. */
  const [armed, setArmed] = useState(false);
  const [stuck, setStuck] = useState(false);
  // Bounded: one automatic retry, never a reload loop.
  const attempt = useRef(0);
  // Bumped to force the iframe to reload when the ranges it holds stop
  // describing the document this side holds.
  const [gen, setGen] = useState(0);

  const cb = useRef({ onStateChange, onCollab, onPeers, onSaved, onWriting, onRendered, onConflictCopy });
  cb.current = { onStateChange, onCollab, onPeers, onSaved, onWriting, onRendered, onConflictCopy };
  const seed = useRef(initial);
  const meRef = useRef(me);
  meRef.current = me;
  const shaRef = useRef(baseSha);
  shaRef.current = baseSha;
  const fileRef = useRef<ReturnType<typeof openSharedFile> | null>(null);

  /* A co-editor saved the document we share.

     This surface never re-reads the file — the iframe is the view and the CRDT
     is the truth — so nothing here would otherwise notice that the hub's head
     moved. What it must notice is the VERSION: every save carries If-Match,
     and a base from before a peer's save is refused for a conflict that does
     not exist, which parked this editor's work in a .bdrive-conflict- copy of
     the file against itself.

     Only the sha is taken, never content: the bytes are already ours through
     the room. */
  useEffect(() => {
    fileRef.current?.rebase(baseSha);
  }, [baseSha]);

  /* The frame's channel. Its script runs first in the document, before any of
     the page's own, and hands up one end of a MessageChannel (`bd-edit:hello`)
     before a page script exists to see it; every message after that travels
     on that port, so nothing the page's scripts post on window is heard.

     Only the FIRST hello from a frame document is taken, so it must not be
     missed: this listener is in place in the same task that commits the
     iframe (a layout effect), before the frame can have loaded anything. */
  const helloPort = useRef<MessagePort | null>(null);
  const helloTake = useRef<((port: MessagePort) => void) | null>(null);
  useLayoutEffect(() => {
    helloPort.current = null;
    const onHello = (e: MessageEvent) => {
      if (!frame.current || e.source !== frame.current.contentWindow || helloPort.current) return;
      const d: unknown = e.data;
      if (!d || typeof d !== "object" || !("type" in d) || d.type !== "bd-edit:hello" || !e.ports[0]) return;
      helloPort.current = e.ports[0];
      helloTake.current?.(e.ports[0]);
    };
    window.addEventListener("message", onHello);
    return () => window.removeEventListener("message", onHello);
  }, [apiBase, path, fileURL, gen]);

  useEffect(() => {
    /* The two halves arrive independently and BOTH are needed before a range
       means anything: the iframe's ranges, and a shared document that actually
       holds the file.

       Getting this wrong is silent and intermittent. A relative position taken
       against an EMPTY Y.Text — which is what the document is until the relay
       seeds it — resolves to index 0, so every anchor collapses onto the start
       of the file and a patch either vanishes or lands on the wrong bytes.
       Whether that happens is a race between an iframe load and an SSE
       handshake, which is the worst kind of bug to own: it passes locally and
       corrupts a document in the field. */
    let ranges: Span[] | null = null;
    let docReady = false;
    let stageDoc: Y.Doc | null = null;
    let stageTimer: ReturnType<typeof setTimeout> | undefined;
    const STAGE_IDLE_MS = 700;
    setArmed(false);
    setStuck(false);

    /* Editing can fail to arm through no fault of this page.

       The shared document is seeded by whichever client the hub tells to; a
       connection that takes that claim and disappears — a reload, a duplicate
       tab, a dropped stream — leaves the room claimed and empty, and everyone
       after it is handed a blank document and silently cannot save. The hub
       expires such a claim (seedClaimGrace), so the fix is to ASK AGAIN rather
       than to sit there: one automatic retry past that grace recovers it
       without the reader ever knowing. Only if that also fails is there
       anything worth telling them. */
    const retryTimer = setTimeout(() => {
      if (!anchored && attempt.current < 1 && !mismatchReloads.has(mismatchKey())) {
        attempt.current++;
        reload();
      }
    }, 11_000);
    const stuckTimer = setTimeout(() => setStuck(true), 24_000);

    /* Messages from the frame, handled one at a time in arrival order; Done's
       save first drains them (frame.current answers a flush only after every
       patch it posted before), so a word typed just before Done is in the
       document — and in its save, shrink check and failure — before the save
       decides whether the editor may close. */
    const channel = frameChannel();
    let frameReady = false;
    /* The exit save (lib/editexit.worker.ts): a patch that never reaches here
       — a tab closed between the keystroke and its message — is posted by
       the frame down a MessagePort to a dedicated worker on this origin,
       which outlives the tab long enough to save it with the app's own
       session. The frame never sees a URL or a credential. The worker is
       armed once the ranges are anchored, kept on what the file holds as
       saves land, and disarmed whenever the frame is replaced on purpose. */
    let exitWorker: Worker | null = null;
    try {
      exitWorker =
        typeof Worker !== "undefined"
          ? new Worker(new URL("../lib/editexit.worker.ts", import.meta.url), { type: "module" })
          : null;
    } catch {
      exitWorker = null; // no worker (a CSP, an old browser): no exit save, as before
    }
    let workerDisarmed = false;
    let frameInteracted = false;
    let latestSavedText = seed.current;
    let latestBaseSha: string | undefined = shaRef.current;
    // The frame's end of the sealed channel (see helloPort above).
    let appPort: MessagePort | null = null;
    const noteFrameInteraction = () => {
      if (frameInteracted || document.activeElement !== frame.current) return;
      frameInteracted = true;
      if (exitWorker && !workerDisarmed) exitWorker.postMessage({ type: "interacted" });
    };
    // A real unload of this page (never a component cleanup): the worker may
    // save what the frame hands it.
    let hadPageHide = false;
    const onParentUnload = () => {
      hadPageHide = true;
      noteFrameInteraction();
      if (!exitWorker || workerDisarmed || !frameInteracted) return;
      exitWorker.postMessage({ type: "parent-unload" });
    };
    /* Back from the bfcache: the exit that went on the way out closed its
       port, and this page may be closed again with another edit the app has
       not heard. A fresh port to the worker, for the same render. */
    const onPageShow = (e: PageTransitionEvent) => {
      if (!hadPageHide && !e.persisted) return;
      hadPageHide = false;
      if (!anchored || !exitWorker || workerDisarmed || !appPort) return;
      const mc = new MessageChannel();
      exitWorker.postMessage(
        {
          type: "rearm",
          savedText: latestSavedText,
          baseSha: latestBaseSha,
          hasBaseSha: true,
          docText: activeDocText(),
          interacted: frameInteracted,
        },
        [mc.port1],
      );
      appPort.postMessage({ type: "bd-edit:port" }, [mc.port2]);
    };
    const activeDocText = () =>
      stageDoc ? stageDoc.getText("body").toString() : (fileRef.current?.collab.text.toString() ?? "");
    const syncExitWorker = () => {
      if (!exitWorker || workerDisarmed) return;
      exitWorker.postMessage({
        type: "sync",
        savedText: latestSavedText,
        baseSha: latestBaseSha,
        hasBaseSha: true,
        docText: activeDocText(),
      });
    };
    const disarmExitWorker = () => {
      if (!exitWorker || workerDisarmed) return;
      workerDisarmed = true;
      exitWorker.postMessage({ type: "disarm" });
      exitWorker.terminate();
    };
    // Every deliberate re-render of the frame: its unload is no tab closing.
    const reload = () => {
      disarmExitWorker();
      appPort?.close();
      appPort = null;
      setGen((g) => g + 1);
    };
    const mismatchKey = () => `${apiBase}\0${path}\0${shaRef.current ?? ""}`;
    let mismatchTimer: ReturnType<typeof setTimeout> | undefined;
    // A reload because the frame and the document disagree: backed off and capped.
    const mismatchReload = () => {
      setArmed(false);
      disarmExitWorker();
      if (mismatchTimer !== undefined) return;
      const k = mismatchKey();
      const n = mismatchReloads.get(k) ?? 0;
      if (n >= MISMATCH_BACKOFF_MS.length) {
        setWarning(
          "This page keeps changing while it loads, so click-to-edit could not attach. Reload the page to try again, or use Edit source.",
        );
        return;
      }
      mismatchReloads.set(k, n + 1);
      mismatchTimer = setTimeout(() => {
        mismatchTimer = undefined;
        reload();
      }, MISMATCH_BACKOFF_MS[n]);
    };
    const armExitWorker = (renderText: string) => {
      if (!exitWorker || workerDisarmed || !appPort) return;
      const mc = new MessageChannel();
      exitWorker.postMessage(
        {
          type: "arm",
          apiBase,
          path,
          who: meRef.current?.name,
          renderText,
          renderLen: renderText.length,
          allowedRanges: [...anchors.keys()],
          savedText: latestSavedText,
          baseSha: latestBaseSha,
          docText: renderText,
          interacted: frameInteracted,
        },
        [mc.port1],
      );
      appPort.postMessage({ type: "bd-edit:port" }, [mc.port2]);
    };
    const ackPatch = (msg: Patch) => {
      if (typeof msg.seq === "number" && exitWorker && !workerDisarmed) {
        exitWorker.postMessage({ type: "ack", seq: msg.seq });
      }
    };
    const drainFrame = async (lock: boolean) => {
      const el = frame.current;
      const port = appPort;
      if (!el || !port) return;
      // A frame that reloads or is replaced cannot answer: stop waiting then.
      let reloaded = false;
      const onLoad = () => {
        reloaded = true;
      };
      el.addEventListener("load", onLoad);
      try {
        await channel.drain(
          (m) => port.postMessage(m),
          () => !frameReady || reloaded || frame.current !== el || appPort !== port,
          lock,
        );
      } finally {
        el.removeEventListener("load", onLoad);
      }
      // Saving now: the idle save the last patch armed has nothing left to do.
      clearTimeout(stageTimer);
      stageTimer = undefined;
    };
    /* Lock the frame and drain it: every patch it posted is in the document
       and nothing more can be typed until the returned release. Done's save
       and the join (a staging buffer folded into the room) both hold one;
       counted per frame channel, so neither unlocks the other early and a
       release meant for a frame since replaced never touches the new one. */
    const portLocks = new Map<MessagePort, number>();
    const lockFrame = async (): Promise<() => void> => {
      const port = appPort;
      if (!port) return () => {};
      portLocks.set(port, (portLocks.get(port) ?? 0) + 1);
      let released = false;
      const release = () => {
        if (released) return;
        released = true;
        const left = (portLocks.get(port) ?? 1) - 1;
        if (left > 0) {
          portLocks.set(port, left);
          return;
        }
        portLocks.delete(port);
        port.postMessage({ type: "bd-edit:unlock" });
      };
      try {
        await drainFrame(true);
      } catch (e) {
        release();
        throw e;
      }
      return release;
    };

    const file = (fileRef.current = openSharedFile({
      apiBase,
      path,
      seed: seed.current,
      baseSha: shaRef.current,
      who: meRef.current?.name,
      me: meRef.current,
      onConflictCopy: (p) => cb.current.onConflictCopy?.(p),
      onShrinkRefused: (ask) => askShrink(path, ask),
      onShrinkWithdrawn: withdrawShrink,
      // The socket synced while staging: every patch the frame has posted
      // must be in the staging buffer before the join reads it, and nothing
      // typed while the join folds it into the room. Unlocked once joined.
      beforeJoin: async () => (stageDoc ? lockFrame() : () => {}),
      // Nothing to mount either way: the surface is the iframe, which is
      // already on screen. The room is joined for the CRDT alone.
      onReady: () => {
        clearTimeout(stageTimer);
        stageTimer = undefined;
        if (stageDoc) {
          const prev = stageDoc;
          stageDoc = null;
          const prevText = prev.getText("body").toString();
          const nextText = file.collab.text.toString();
          if (anchored && prevText === nextText) {
            for (const [k, a] of anchors) {
              const f = Y.createAbsolutePositionFromRelativePosition(a.from, prev);
              const t = Y.createAbsolutePositionFromRelativePosition(a.to, prev);
              if (f && t) {
                anchors.set(k, {
                  from: Y.createRelativePositionFromTypeIndex(file.collab.text, f.index),
                  to: Y.createRelativePositionFromTypeIndex(file.collab.text, t.index),
                  span: a.span,
                  orig: a.orig,
                });
              }
            }
          } else {
            anchors.clear();
            anchored = false;
            if (prevText !== nextText) mismatchReload();
          }
          prev.destroy();
        }
        docReady = true;
        void anchorRanges();
      },
      onSolo: () => {
        if (!file.collab.text.length) {
          file.collab.text.insert(0, seed.current);
        }
        docReady = true;
        void anchorRanges();
      },
      onStalled: () => {
        if (docReady || stageDoc) return;
        stageDoc = new Y.Doc();
        stageDoc.getText("body").insert(0, seed.current);
        docReady = true;
        void anchorRanges();
      },
      onState: (s) => cb.current.onStateChange?.(s),
      onCollab: (s) => cb.current.onCollab?.(s),
      onPeers: (n) => cb.current.onPeers?.(n),
      onSaved: (t) => cb.current.onSaved?.(t),
      onWriting: () => cb.current.onWriting?.(),
      onBaseChange: (savedText, nextBaseSha) => {
        latestSavedText = savedText;
        latestBaseSha = nextBaseSha;
        syncExitWorker();
      },
      soloText: () => (stageDoc ? stageDoc.getText("body").toString() : seed.current),
      soloApply: ({ from, to, insert }) => {
        if (!stageDoc) return;
        stageDoc.transact(() => {
          const t = stageDoc!.getText("body");
          if (to > from) t.delete(from, to - from);
          if (insert) t.insert(from, insert);
        });
      },
    }));
    /* Done: the frame is drained AND locked, so nothing typed while the save
       is on the wire can be left out of it; it is unlocked again unless the
       save landed (lib/frameflush.ts saveLocked). Concurrent calls share one. */
    let savingFrame: Promise<boolean> | null = null;
    const exposed: SharedFile = {
      ...file,
      saveNow: () => {
        let release = () => {};
        return (savingFrame ??= saveLocked(
          async () => {
            release = await lockFrame();
          },
          () => file.saveNow(),
          () => release(),
        ).finally(() => {
          savingFrame = null;
        }));
      },
    };
    if (handle) handle.current = exposed;

    /* Ranges are held as RELATIVE positions, not offsets.

       The server measured those offsets against the bytes it served. The
       moment a peer edits anything earlier in the file, every one of them is
       wrong by the length of that edit — and a splice by a stale offset lands
       in the middle of somebody else's markup. A Yjs relative position is the
       answer to exactly this: it names a place in the document rather than a
       number, and it survives concurrent insertions before it. */
    // The stamped span length travels with the anchor so a resolved range can
    // be sanity-checked before anything is deleted (see the splice below).
    const anchors = new Map<
      string,
      // `orig`: the source the iframe's element was last rendered from (the
      // stamped bytes, then each patch this side applied) — the base a patch
      // and a co-editor's typing in the same element are merged against.
      { from: Y.RelativePosition; to: Y.RelativePosition; span: number; orig: string }
    >();
    const key = (s: Span) => s.start + "," + s.end;

    /* Anchoring, attempted whenever something that could make it possible has
       happened: the ranges arriving, the relay seeding, a peer's update.

       An EMPTY shared document is the case worth naming, because it is the one
       that used to pass the old guard. Joining a room whose log has not
       replayed yet gives a Y.Text of length zero — and a relative position
       taken against it resolves to index 0, so every anchor silently collapses
       onto the start of the file. "Nothing to anchor into" has to mean WAIT,
       never "anchor at 0"; there is no document to describe yet, and one is
       almost certainly moments away. */
    const anchorRanges = async () => {
      if (anchored || !ranges || !docReady) return;
      /* Read the CRDT ITSELF, never file.current().

         current() falls back to the seed when the shared document is empty,
         which is exactly the state this check exists to catch — so asking it
         reported a healthy full-length document while the Y.Text the anchors
         are about to be built against held nothing. Both positions then
         resolved to the same empty span, which became from=0 to=end once the
         document filled, and the first patch replaced THE WHOLE FILE with one
         paragraph. The guard has to look at the thing being measured. */
      const activeYText = stageDoc ? stageDoc.getText("body") : file.collab.text;
      const text = activeYText.toString();
      if (!text) return; // nothing to anchor into yet — try again on the next update
      // A range that runs past the end cannot belong to this document. Cheap,
      // and it fails closed on any future way the two could drift apart.
      if (ranges.some((r) => r.end > text.length || r.start > r.end)) {
        ranges = null;
        mismatchReload();
        return;
      }
      /* The ranges describe the document the SERVER served. If this side holds
         a different one — a peer saved between the render and the load — every
         range is off, and the honest move is to render again.

         Length is the fallback, not the preference: it misses an edit that
         happens to preserve length, which a hash would catch. It is what there
         is on a hub without WebCrypto. Both are compared in UTF-16 units,
         which is what the server counts and what a JS string is indexed in. */
      const digest = await sha256Hex(text);
      const moved =
        digest !== null ? digest !== srcHash : srcLen >= 0 && text.length !== srcLen;
      if (moved) {
        ranges = null;
        mismatchReload();
        return;
      }
      anchors.clear();
      for (const s of ranges) {
        /* The frame read each element's content when it bound it to its
           range. Content that is not what the source holds there — a page
           script rewrote the element, or forged a marker onto another one
           before the frame could bind it — is not anchored: nothing typed
           into it can reach the file, here or by the exit worker. */
        if (typeof s.html === "string" && canonicalHTML(s.html) !== canonicalHTML(text.slice(s.start, s.end))) {
          continue;
        }
        anchors.set(key(s), {
          from: Y.createRelativePositionFromTypeIndex(activeYText, s.start),
          to: Y.createRelativePositionFromTypeIndex(activeYText, s.end),
          span: s.end - s.start,
          orig: text.slice(s.start, s.end),
        });
      }
      anchored = true;
      armExitWorker(text);
      setArmed(true);
      cb.current.onRendered?.();
    };
    let srcHash = "";
    let srcLen = -1;
    let anchored = false;

    /* Synchronous unless it has to wait for anchoring: a patch that arrives
       with the ranges anchored is in the document before this returns, so a
       pagehide right after its message event (before any microtask) already
       sends it. Only `ready`, and a patch that comes before anchoring, wait —
       and the channel holds every message behind them meanwhile. */
    const handleMessage = (msg: FromFrame): void | Promise<void> => {
      if (msg.type === "bd-edit:flushed") {
        channel.flushed(msg.id);
        return;
      }

      if (msg.type === "bd-edit:clobbered") {
        setWarning(
          "This page rewrites itself as it runs, and it replaced the part you were editing. That edit was not saved.",
        );
        return;
      }

      if (msg.type === "bd-edit:ready") {
        anchored = false;
        anchors.clear();
        ranges = msg.ranges;
        srcHash = msg.hash;
        srcLen = msg.len;
        frameReady = true;
        return anchorRanges(); // no-op until the document is seeded too
      }

      if (msg.type !== "bd-edit:patch") return;
      noteFrameInteraction();
      if (!frameInteracted) return;
      if (!anchored)
        return anchorRanges().then(() => {
          if (!applyPatch(msg)) return;
          ackPatch(msg);
          syncExitWorker();
        });
      if (!applyPatch(msg)) return;
      ackPatch(msg);
      syncExitWorker();
    };
    const applyPatch = (msg: Patch): boolean => {
      const a = anchors.get(key(msg));
      if (!a) return false; // a range from a render this side has already replaced
      if (stageDoc) {
        const sFrom = Y.createAbsolutePositionFromRelativePosition(a.from, stageDoc);
        const sTo = Y.createAbsolutePositionFromRelativePosition(a.to, stageDoc);
        if (!sFrom || !sTo || sFrom.index > sTo.index) return false;
        const sText = stageDoc.getText("body");
        const sSpan = sTo.index - sFrom.index;
        const sWhole = sFrom.index === 0 && sTo.index === sText.length;
        if ((sWhole && a.span < sText.length) || sSpan > a.span * 4 + 256) {
          anchors.clear();
          anchored = false;
          mismatchReload();
          return false;
        }
        const sLive = sText.toString().slice(sFrom.index, sTo.index);
        const sWant = merge3PreferRight(a.orig, sLive, msg.html);
        a.orig = msg.html;
        if (sLive === sWant) return true;
        stageDoc.transact(() => {
          for (const s of diffSplices(sLive, sWant)) {
            if (s.deleteCount > 0) sText.delete(sFrom.index + s.index, s.deleteCount);
            if (s.insertText) sText.insert(sFrom.index + s.index, s.insertText);
          }
        });
        cb.current.onStateChange?.("dirty");
        clearTimeout(stageTimer);
        stageTimer = setTimeout(() => void file.saveNow(), STAGE_IDLE_MS);
        return true;
      }
      const doc = file.collab.doc;
      const from = Y.createAbsolutePositionFromRelativePosition(a.from, doc);
      const to = Y.createAbsolutePositionFromRelativePosition(a.to, doc);
      if (!from || !to || from.index > to.index) return false;
      const text = file.collab.text;

      /* Last line of defence: a patch may not swallow the document.

         Everything above is meant to guarantee this range is the paragraph it
         claims to be, and everything above has already been wrong once — a
         bad anchor resolved to from=0,to=end and one edit replaced an entire
         file with a single paragraph. A resolved span should be about the size
         of the span that was stamped; a peer editing inside it moves that a
         little, not by orders of magnitude.

         So: refuse anything that has grown implausibly, and refuse outright
         anything that would rewrite the whole document when the stamped range
         was only part of it. Dropping an edit is a bad afternoon. Deleting
         somebody's file is not recoverable from the UI. */
      const span = to.index - from.index;
      const whole = from.index === 0 && to.index === text.length;
      if ((whole && a.span < text.length) || span > a.span * 4 + 256) {
        console.warn("bdrive: refusing an implausible patch range", {
          span,
          stamped: a.span,
          docLength: text.length,
        });
        // The ranges cannot be trusted any more; re-render and re-anchor.
        anchors.clear();
        anchored = false;
        mismatchReload();
        return false;
      }

      const live = text.toString().slice(from.index, to.index);
      /* Only the characters that changed, not the whole element. A co-editor
         typing elsewhere in the same paragraph is in `live` but not in the
         patch, which the iframe built from what it last rendered — replacing
         the span wholesale erased their keystrokes. So: their change
         (live − orig) and ours (html − orig), merged three ways; only the
         words where they collide take the patch, and a co-editor's edit to
         other words of the same element survives the collision. */
      const want = merge3PreferRight(a.orig, live, msg.html);
      a.orig = msg.html;
      if (live === want) return true;
      // Splices run last-first in `live` coordinates, so shifting by the
      // span's start keeps every one valid. One transaction, so peers see one
      // change rather than a moment of missing content.
      file.collab.applySplices(
        diffSplices(live, want).map((s) => ({ ...s, index: s.index + from.index })),
      );
      return true;
    };
    // Every message from the frame arrives on its sealed port, in order.
    const adoptPort = (port: MessagePort) => {
      if (appPort === port) return;
      appPort?.close();
      appPort = port;
      anchored = false;
      ranges = null;
      anchors.clear();
      frameReady = false;
      port.onmessage = (ev: MessageEvent) => {
        const msg = ev.data as FromFrame;
        if (!msg || typeof msg !== "object") return;
        channel.enqueue(() => handleMessage(msg));
      };
    };

    // The document filling in is itself a reason to try anchoring again: the
    // relay may seed AFTER onReady fires, and a peer's first update is the
    // other way an empty room becomes a real one.
    const retry = () => {
      syncExitWorker();
      void anchorRanges();
    };
    file.collab.text.observe(retry);

    helloTake.current = adoptPort;
    if (helloPort.current) adoptPort(helloPort.current);
    window.addEventListener("blur", noteFrameInteraction);
    window.addEventListener("beforeunload", onParentUnload);
    window.addEventListener("pagehide", onParentUnload);
    window.addEventListener("pageshow", onPageShow);
    return () => {
      if (handle?.current === exposed) handle.current = null;
      if (helloTake.current === adoptPort) helloTake.current = null;
      window.removeEventListener("blur", noteFrameInteraction);
      window.removeEventListener("beforeunload", onParentUnload);
      window.removeEventListener("pagehide", onParentUnload);
      window.removeEventListener("pageshow", onPageShow);
      frameReady = false;
      channel.close();
      clearTimeout(retryTimer);
      clearTimeout(stuckTimer);
      clearTimeout(mismatchTimer);
      clearTimeout(stageTimer);
      file.collab.text.unobserve(retry);
      /* Torn down a beat later, so a patch already in flight still lands.

         Defensive, and honestly so: with the iframe's debounce gone a patch is
         posted during the keystroke itself, so by the time a click reaches
         Done the browser has long since delivered it — removing this delay
         does not fail any test here. It stays because the thing it guards is
         silent: postMessage is asynchronous, this listener disappearing a tick
         early would drop the message with no error anywhere, and the reader
         would simply find their last word missing. 300ms of late teardown is a
         cheap price for not having to be right about task ordering across
         browsers. */
      setTimeout(() => {
        appPort?.close();
        appPort = null;
        clearTimeout(stageTimer);
        file.destroy();
        disarmExitWorker();
        stageDoc?.destroy();
      }, 300);
    };
    // The document, and the render generation. Not the callbacks: they are
    // inline arrows at the call site and would rebuild the room — and with it
    // the relay connection and every anchor — on an unrelated re-render. The
    // same trap Editor.tsx documents, and the reason they all go through a ref.
  }, [apiBase, path, gen]);

  // Receipts read once per render generation: a save made from this very
  // frame records a new one, and re-pointing the live iframe at it would
  // reload the page under the typist.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const src = useMemo(() => withReceipt(fileURL + (fileURL.includes("?") ? "&" : "?") + "edit=1", apiBase), [apiBase, fileURL, gen]);

  return (
    <>
      {warning && (
        <div id="edit-clobbered" className="banner">
          {warning}
        </div>
      )}
      {stuck && !armed && (
        <div id="edit-not-ready" className="banner">
          This file could not be opened for editing — the shared document never
          loaded. Reload the page to try again, or use <b>Edit source</b>.
        </div>
      )}
      <iframe
        key={gen}
        ref={frame}
        className="htmlview"
        // Unchanged from the reading view. Editing buys no extra capability:
        // allow-same-origin here would hand every synced page the hub's
        // session, which is the wall this whole feature had to not touch.
        sandbox="allow-scripts"
        src={src}
        title={path}
      />
    </>
  );
}
