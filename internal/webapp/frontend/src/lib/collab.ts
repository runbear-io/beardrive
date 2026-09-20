import * as Y from "yjs";
import { WebsocketProvider } from "y-websocket";
import { Awareness } from "y-protocols/awareness";

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

export type CollabStatus = "connecting" | "live" | "offline";

export class CollabDoc {
  readonly doc = new Y.Doc();
  readonly text: Y.Text;
  readonly awareness: Awareness;

  private ws: WebsocketProvider | null = null;
  private closed = false;

  constructor(
    private readonly url: string,
    private readonly onStatus: (s: CollabStatus) => void,
    private readonly onReady: () => void,
    // Called when the document cannot be reached at all — an older hub with
    // no such route, or a proxy that will not upgrade a websocket. The editor
    // still opens and still saves; what it loses is LIVE collaboration, not
    // the ability to write. That is deliberately not a second CRDT path: a
    // client editing its own copy of a shared document is how two browsers
    // came to overwrite each other.
    private readonly onUnavailable: () => void,
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
        colorLight: this.me.colour + "33",
      });
    }
  }

  connect() {
    this.onStatus("connecting");
    const u = new URL(this.url, location.href);
    const path = u.searchParams.get("path") ?? "";
    const base =
      (u.protocol === "https:" ? "wss://" : "ws://") + u.host + u.pathname;

    /* The room argument is decoration.

       y-websocket appends it to the URL, but the hub names the room itself
       from (project, path) after it has resolved who is asking — because a
       caller who could name the room could join any project's document by
       asking for its name. */
    const provider = new WebsocketProvider(base, "held", this.doc, {
      params: { path },
      awareness: this.awareness,
      connect: true,
    });
    this.ws = provider;

    provider.on("status", (e: { status: string }) => {
      if (this.closed) return;
      this.onStatus(e.status === "connected" ? "live" : "offline");
    });
    provider.on("sync", (synced: boolean) => {
      // "Synced" is the document having arrived, which is the moment the
      // editor may mount on it. Nothing is seeded here: the hub built it from
      // the file before this client existed.
      if (synced && !this.closed) this.onReady();
    });
    /* A connection that never succeeds has to be reported, or the editor
       waits forever for a document that is not coming. y-websocket retries on
       its own — which is right, a flaky network should heal — so this asks
       only once, well past the first few attempts. */
    setTimeout(() => {
      if (!this.closed && !provider.synced) this.onUnavailable();
    }, UNREACHABLE_MS);
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
    this.ws?.destroy();
    this.ws = null;
    this.awareness.destroy();
    this.doc.destroy();
  }
}

// How long to let the provider retry before telling the caller the document
// is unreachable. Long enough to cover a reconnect on a bad network, short
// enough that a hub without the route does not leave an editor waiting.
const UNREACHABLE_MS = 8_000;
