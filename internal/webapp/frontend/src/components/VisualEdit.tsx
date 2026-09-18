import { useEffect, useRef, useState } from "react";
import * as Y from "yjs";
import { openSharedFile, type SaveState } from "../lib/sharedfile";
import { type CollabStatus } from "../lib/collab";

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

// What the injected script sends up. It can send nothing else, and this
// component sends nothing down.
type Ready = { type: "bd-edit:ready"; hash: string; len: number; ranges: Span[] };
type Patch = { type: "bd-edit:patch"; start: number; end: number; html: string };
type Clobbered = { type: "bd-edit:clobbered" };
type FromFrame = Ready | Patch | Clobbered;

type Span = { start: number; end: number };

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
      if (!anchored && attempt.current < 1) {
        attempt.current++;
        setGen((g) => g + 1);
      }
    }, 11_000);
    const stuckTimer = setTimeout(() => setStuck(true), 24_000);

    const file = openSharedFile({
      apiBase,
      path,
      seed: seed.current,
      baseSha: shaRef.current,
      who: meRef.current?.name,
      me: meRef.current,
      onConflictCopy: (p) => cb.current.onConflictCopy?.(p),
      // Nothing to mount either way: the surface is the iframe, which is
      // already on screen. The room is joined for the CRDT alone.
      onReady: () => {
        docReady = true;
        void anchorRanges();
      },
      onSolo: () => {
        // No relay, so no room and no peer that could double-seed it. Filling
        // the document locally keeps ONE code path for splicing instead of a
        // second plain-string one that would only ever run on an older hub.
        if (!file.collab.text.length) {
          file.collab.text.insert(0, seed.current);
        }
        docReady = true;
        void anchorRanges();
      },
      onState: (s) => cb.current.onStateChange?.(s),
      onCollab: (s) => cb.current.onCollab?.(s),
      onPeers: (n) => cb.current.onPeers?.(n),
      onSaved: (t) => cb.current.onSaved?.(t),
      onWriting: () => cb.current.onWriting?.(),
      soloText: () => seed.current,
    });

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
      { from: Y.RelativePosition; to: Y.RelativePosition; span: number }
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
      const text = file.collab.text.toString();
      if (!text) return; // nothing to anchor into yet — try again on the next update
      // A range that runs past the end cannot belong to this document. Cheap,
      // and it fails closed on any future way the two could drift apart.
      if (ranges.some((r) => r.end > text.length || r.start > r.end)) {
        ranges = null;
        setGen((g) => g + 1);
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
        setGen((g) => g + 1);
        return;
      }
      anchors.clear();
      for (const s of ranges) {
        anchors.set(key(s), {
          from: Y.createRelativePositionFromTypeIndex(file.collab.text, s.start),
          to: Y.createRelativePositionFromTypeIndex(file.collab.text, s.end),
          span: s.end - s.start,
        });
      }
      anchored = true;
      setArmed(true);
      cb.current.onRendered?.();
    };
    let srcHash = "";
    let srcLen = -1;
    let anchored = false;

    const onMessage = async (e: MessageEvent) => {
      // The iframe is sandboxed WITHOUT allow-same-origin, so its origin is
      // the string "null" and checking it proves nothing. Identity has to come
      // from the window itself.
      if (!frame.current || e.source !== frame.current.contentWindow) return;
      const msg = e.data as FromFrame;
      if (!msg || typeof msg !== "object") return;

      if (msg.type === "bd-edit:clobbered") {
        setWarning(
          "This page rewrites itself as it runs, and it replaced the part you were editing. That edit was not saved.",
        );
        return;
      }

      if (msg.type === "bd-edit:ready") {
        ranges = msg.ranges;
        srcHash = msg.hash;
        srcLen = msg.len;
        await anchorRanges(); // no-op until the document is seeded too
        return;
      }

      if (msg.type !== "bd-edit:patch") return;
      const a = anchors.get(key(msg));
      if (!a) return; // a range from a render this side has already replaced
      const doc = file.collab.doc;
      const from = Y.createAbsolutePositionFromRelativePosition(a.from, doc);
      const to = Y.createAbsolutePositionFromRelativePosition(a.to, doc);
      if (!from || !to || from.index > to.index) return;

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
        setGen((g) => g + 1);
        return;
      }

      if (text.toString().slice(from.index, to.index) === msg.html) return;
      // One transaction, so peers see the replacement rather than a delete
      // followed by a moment of missing content.
      doc.transact(() => {
        text.delete(from.index, span);
        text.insert(from.index, msg.html);
      });
    };

    // The document filling in is itself a reason to try anchoring again: the
    // relay may seed AFTER onReady fires, and a peer's first update is the
    // other way an empty room becomes a real one.
    const retry = () => void anchorRanges();
    file.collab.text.observe(retry);

    window.addEventListener("message", onMessage);
    return () => {
      clearTimeout(retryTimer);
      clearTimeout(stuckTimer);
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
        window.removeEventListener("message", onMessage);
        file.destroy();
      }, 300);
    };
    // The document, and the render generation. Not the callbacks: they are
    // inline arrows at the call site and would rebuild the room — and with it
    // the relay connection and every anchor — on an unrelated re-render. The
    // same trap Editor.tsx documents, and the reason they all go through a ref.
  }, [apiBase, path, gen]);

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
        src={fileURL + (fileURL.includes("?") ? "&" : "?") + "edit=1"}
        title={path}
      />
    </>
  );
}
