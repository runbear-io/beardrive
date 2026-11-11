import * as Y from "yjs";
import { WebsocketProvider } from "y-websocket";
import { Awareness } from "y-protocols/awareness";
import type { Splice } from "./merge3";

/* A Yjs document, held by the hub.

   This file used to be a hand-rolled provider: SSE down, POST up, and ~250
   lines of machinery that existed because nobody owned the document. A seed
   CLAIM with a grace timer, because two clients seeding one file build two
   documents that duplicate every character on merge. A byte cap on a log that
   only grows, and a rebuild-from-scratch when it was hit. A resync frame for
   a reader that fell behind. A solo fallback that let a client edit its own
   buffer and then overwrite everyone else's work.

   All of it was compensation for a missing owner, and the hub is the owner
   now (webapp/ycollab.go): it builds the document from the file before anyone
   attaches, holds the one copy everybody converges on, and writes it back.
   So the compensations are not disabled, they are gone — and what is left is
   a provider somebody else maintains, doing a state-vector handshake on
   reconnect instead of replaying a room's entire history.

   What remains here is the shape the editors already expect (`text`,
   `awareness`, `peerCount`, `connect`, `destroy`), so swapping the transport
   underneath them changed almost nothing above. */

// "solo": this server has no room route at all (see stop()); the file is
// saved by this client alone and nothing will ever go live. A room that is
// merely SLOW never becomes solo: the provider keeps retrying for the life of
// the editor, and the caller hears about the wait through onStalled.
export type CollabStatus = "connecting" | "live" | "offline" | "solo";

// The origin of a hub update that arrived over HTTP before the socket existed.
const HTTP_REMOTE = Symbol("collab-http-remote");

export class CollabDoc {
  readonly doc = new Y.Doc();
  readonly text: Y.Text;
  readonly awareness: Awareness;

  private ws: WebsocketProvider | null = null;
  private closed = false;
  private reached = false;
  private stallTimer: ReturnType<typeof setTimeout> | null = null;

  // Where each OTHER participant's caret sits, by name, as an index into the
  // text. A method rather than a helper beside the editor, so a caller that
  // only reads it does not have to import Yjs itself.
  remoteCursors(): Array<{ name: string; pos: number }> {
    const out: Array<{ name: string; pos: number }> = [];
    this.awareness.getStates().forEach((st, id) => {
      if (id === this.doc.clientID) return;
      const c = (st as { cursor?: { head?: unknown }; user?: { name?: string } }).cursor;
      if (!c?.head) return;
      const abs = Y.createAbsolutePositionFromRelativePosition(Y.createRelativePositionFromJSON(c.head), this.doc);
      if (abs) out.push({ name: (st as { user?: { name?: string } }).user?.name ?? "Someone", pos: abs.index });
    });
    return out;
  }

  constructor(
    private readonly url: string,
    private readonly onStatus: (s: CollabStatus) => void,
    private readonly onReady: () => void,
    // Called once when the first handshake has not finished after
    // STALLED_MS — a deploy rolling, a cold start, a proxy that will not
    // upgrade. Advisory only: the provider is NOT torn down, it keeps
    // reconnecting, and when it does get through onReady fires as usual. The
    // caller may let the user type in the meantime and hand that typing to
    // the hub over HTTP (sharedfile.ts, staging).
    //
    // This used to be onUnavailable, which ended the transport for good after
    // 8s: a room that answered at 8.6s was never joined, and the editor wrote
    // its own copy against everybody else's for the rest of the session.
    private readonly onStalled: () => void,
    private readonly me?: { name: string; colour: string },
  ) {
    this.text = this.doc.getText("body");
    this.awareness = new Awareness(this.doc);
    // y-codemirror.next reads these two fields to label and colour a remote
    // caret. Without a local state this client is invisible to everyone else
    // and its own peer count never moves off zero.
    if (this.me) {
      this.awareness.setLocalStateField("user", {
        name: this.me.name,
        color: this.me.colour,
        // The selection tint: the caret colour at a fifth of its strength.
        colorLight: this.me.colour.replace(/\)$/, " / 0.2)"),
      });
    }
  }

  connect() {
    this.onStatus("connecting");
    const u = new URL(this.url, location.href);
    const path = u.searchParams.get("path") ?? "";
    const base =
      (u.protocol === "https:" ? "wss://" : "ws://") + u.host + u.pathname;

    /* The room name is NOT decoration, and this comment used to say it was.

       To the SERVER it is: the hub names the room itself from (project, path)
       after resolving who is asking, because a caller who could name the room
       could join any project's document by asking for its name. That part
       still holds.

       To the BROWSER it is the whole identity of the document. y-websocket
       keys its cross-tab BroadcastChannel on serverUrl + "/" + roomname and
       nothing else — the query params are not in it. With one fixed name for
       every file, every editor open in the same browser shared one channel
       and applied each other's updates: two different files, opened in two
       tabs, mixed into each other. Shipped that way.

       Two fixes, both kept. The channel is DISABLED, because the hub holds
       the document and is the one source of truth — a tab-to-tab side channel
       is a second path for the same bytes and is exactly what let the mistake
       matter. And the name carries the path anyway, so that if anybody ever
       turns the channel back on it is keyed correctly. encodeURIComponent
       because y-websocket puts the name straight into the URL. */
    const provider = new WebsocketProvider(base, encodeURIComponent(path), this.doc, {
      params: { path },
      awareness: this.awareness,
      connect: true,
      disableBc: true,
    });
    this.ws = provider;

    // Live means the handshake finished, not merely that a socket opened: a
    // connection that opens and is dropped before syncing never reaches the
    // hub's document, and must not read as live in between.
    provider.on("status", (e: { status: string }) => {
      if (this.closed) return;
      this.onStatus(e.status === "connected" ? (provider.synced ? "live" : "connecting") : "offline");
    });
    provider.on("sync", (synced: boolean) => {
      if (synced && !this.closed) this.onStatus("live");
      // "Synced" is the document having arrived, which is the moment the
      // editor may mount on it. Nothing is seeded here: the hub built it from
      // the file before this client existed.
      if (synced && !this.closed) {
        this.reached = true;
        this.onReady();
      }
    });
    /* A slow first handshake is reported, never acted on here. y-websocket
       retries on its own — which is right, a flaky network should heal — and
       this document stays EMPTY until the hub's arrives: nothing local is
       ever written into it before "sync", so joining late can never merge a
       locally built copy of the file with the hub's and double the text. */
    this.stallTimer = setTimeout(() => {
      this.stallTimer = null;
      if (this.closed || this.reached) return;
      this.onStalled();
    }, STALLED_MS);
  }

  /* There is no room to join: the server has no such route at all (a
     single-volume server). Never called for a room that is merely slow. */
  stop() {
    if (this.closed) return;
    this.closed = true;
    if (this.stallTimer) clearTimeout(this.stallTimer);
    this.ws?.destroy();
    this.ws = null;
    this.onStatus("solo");
  }

  /* This document's whole state as one Yjs update, base64: what a client that
     cannot reach the socket hands the hub over HTTP. Applying it to the room
     is idempotent — whatever the room already holds is not added twice. */
  encodeState(): string {
    const u8 = Y.encodeStateAsUpdate(this.doc);
    let bin = "";
    for (let i = 0; i < u8.length; i += 0x8000) {
      bin += String.fromCharCode(...u8.subarray(i, i + 0x8000));
    }
    return btoa(bin);
  }

  /* Apply splices (lib/merge3.ts diffSplices: old-text coordinates, last
     first) in one transaction, so peers see one change and every character
     outside them keeps its identity. */
  applySplices(splices: Splice[]) {
    if (!splices.length) return;
    this.doc.transact(() => {
      for (const s of splices) {
        if (s.deleteCount) this.text.delete(s.index, s.deleteCount);
        if (s.insertText) this.text.insert(s.index, s.insertText);
      }
    });
  }

  /* Did this change come from the hub, or from the person at this keyboard?

     Yjs stamps every transaction with an origin, and y-websocket applies what
     it receives with the provider as that origin. So this is the difference
     between "a peer typed" and "I typed" — which is the difference between a
     file that is written once per quiet period and one written once per quiet
     period PER EDITOR (see sharedfile.ts, and docs/hub-load-prd.md Stage 4).

     A local edit's origin is whatever CodeMirror's binding used, never the
     provider, so testing against the provider identifies remote changes
     precisely rather than guessing at the local ones. */
  isRemote(origin: unknown): boolean {
    return origin === HTTP_REMOTE || (this.ws !== null && origin === this.ws);
  }

  /* Apply a Yjs update the hub answered over HTTP (ycollab/sync's `update`:
     the room's state with a staging checkpoint in it). It carries the room's
     own item IDs, so applying it is idempotent with the same items arriving
     on the socket — before or after — and a join that follows splices only
     what the room does not hold yet. Stamped remote: it is the hub's change,
     never a keystroke here. */
  applyRemoteUpdate(b64: string): void {
    const bin = atob(b64);
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    Y.applyUpdate(this.doc, bytes, this.ws ?? HTTP_REMOTE);
  }

  /* How many OTHER editors are in this document right now, from awareness.
     It is what tells a write on this path apart: with a co-editor present the
     change is their snapshot of the document I already have, so warning me
     about it is noise. Alone, the same event is somebody editing outside the
     room — a CLI, another device — which I really do need to know about. */
  peerCount(): number {
    return Math.max(0, this.awareness.getStates().size - 1);
  }

  destroy() {
    this.closed = true;
    if (this.stallTimer) clearTimeout(this.stallTimer);
    this.ws?.destroy();
    this.ws = null;
    this.awareness.destroy();
    this.doc.destroy();
  }
}

// How long the first handshake may take before the caller is told the room is
// slow. Short, because nothing is given up when it fires: the user just gets
// a buffer to type into while the provider keeps trying.
const STALLED_MS = 2_500;
