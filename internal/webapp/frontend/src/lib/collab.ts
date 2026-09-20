import * as Y from "yjs";
import { WebsocketProvider } from "y-websocket";
import {
  Awareness,
  applyAwarenessUpdate,
  encodeAwarenessUpdate,
} from "y-protocols/awareness";

/* A Yjs provider over the hub's collab relay.

   Not y-websocket: the transport is the same SSE-down / POST-up pair the rest
   of the app already uses, which needs no new server dependency, no upgrade
   handshake, and nothing special from a proxy that already carries /events.
   Typing latency is one POST, batched — for a document that is well inside
   what a person notices.

   The hub never parses these bytes. It stores them in arrival order and hands
   the log to whoever joins next, which is all a Yjs peer needs to converge.

   The one rule that matters: a Yjs document seeded independently by two
   clients from the same text is NOT the same document — the items carry
   different ids and a merge duplicates every character. So only the client the
   hub calls `seed` builds from the file; everyone else builds from the log. */

export type CollabStatus = "connecting" | "live" | "offline";

/* Updates are coalesced into one POST per tick. Long enough to batch a burst
   of keystrokes, short enough that a watcher sees you type.

   120ms, not 60: at 60 a fast typist put ~16 POSTs a second on the wire, and
   nobody can see the difference between a caret that lags 60ms and one that
   lags 120. This is the uplink's whole cost model — the stream only carries
   the DOWNlink, so every byte this client produces leaves as an HTTP request
   (see docs/collab-provider-prd.md, where that is the argument for replacing
   the transport rather than tuning it). */
const FLUSH_MS = 120;

/* Cursor moves are coalesced harder, and not sent at all when nobody is
   looking.

   A caret is a courtesy; the document is the point. Every arrow key used to
   be its own POST — holding one down is a request per repeat, and moving
   around a file you are editing ALONE spent a request per keypress drawing a
   caret for nobody. Nothing downstream can tell 5 updates a second from 16. */
const CURSOR_MS = 200;

export class CollabDoc {
  readonly doc = new Y.Doc();
  readonly text: Y.Text;
  readonly awareness: Awareness;

  private es: EventSource | null = null;
  private pending: Uint8Array[] = [];
  private timer: ReturnType<typeof setTimeout> | null = null;
  private closed = false;
  private cursorTimer: ReturnType<typeof setTimeout> | null = null;
  // Whether a POST of document updates is in flight. See flush().
  private sending = false;
  // Whether this client has ever said it is here. Until it has, staying quiet
  // would make it invisible rather than cheap.
  private announced = false;
  // Whether a `hello` has ever arrived. Distinguishes a dropped connection
  // (retry, keep the document) from a relay that does not exist (give up on
  // co-editing and let the editor open solo).
  private everConnected = false;
  // Updates that came FROM the relay must not be echoed back to it.
  private applying = false;
  // Identifies this stream to the relay, so our own updates and cursor moves
  // are not mailed back to us. Sent on the stream URL and on every POST; the
  // relay treats an unknown id as "no sender" and fans out to everyone, which
  // is what an older hub does anyway.
  private readonly cid =
    globalThis.crypto?.randomUUID?.() ?? String(Math.random()).slice(2);
  // Set when the HUB holds this document (ycollab.go). Then none of the
  // relay machinery below runs: no seed claim, no log to replay, no resync
  // frame, no solo fallback — the provider does the sync protocol and the
  // server is the one copy everybody converges on.
  private ws: WebsocketProvider | null = null;

  constructor(
    private readonly url: string,
    private readonly seedText: string,
    private readonly onStatus: (s: CollabStatus) => void,
    private readonly onSeeded: () => void,
    // Called when the relay turns out not to be reachable at all, so the
    // caller can fall back to plain single-writer editing.
    private readonly onUnavailable: () => void,
    private readonly me?: { name: string; colour: string },
    // Whether the HUB holds this document (/api/config collab.held). Not
    // sniffed: a hub too old to serve the route and a proxy that refuses the
    // upgrade fail identically, and guessing wrong means an editor waiting
    // for a document nobody will send.
    private readonly held = false,
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
    this.awareness.on("update", this.onAwareness);
    this.doc.on("update", (u: Uint8Array) => {
      // When the hub holds the document the provider IS the transport: it
      // sends updates over the socket and answers sync step 1 with state
      // vectors. Queueing them for the relay's POST as well would be the
      // same bytes twice, at a route that only serves GET — which is what
      // the 405s in the console were.
      if (this.held || this.applying) return;
      this.pending.push(u);
      if (!this.timer) this.timer = setTimeout(() => this.flush(), FLUSH_MS);
    });
  }

  // Awareness is relayed, never logged: it says where a caret is this second,
  // so a joiner replaying it would get cursors for people who have gone home.
  private onAwareness = ({
    added,
    updated,
    removed,
  }: {
    added: number[];
    updated: number[];
    removed: number[];
  }) => {
    const changed = added.concat(updated, removed);
    if (!changed.length || this.closed) return;
    // Only our own state is ours to publish. The rest of `changed` is what
    // just arrived FROM the relay, and re-broadcasting it sends every peer's
    // caret back to every peer — the relay already fans out to everyone.
    if (!changed.includes(this.doc.clientID)) return;
    if (this.cursorTimer) return; // one POST per window, carrying the latest
    this.cursorTimer = setTimeout(() => {
      this.cursorTimer = null;
      this.publishAwareness();
    }, CURSOR_MS);
  };

  /* Publish where this client's caret is.

     `force` is for the two moments that are about existence rather than
     position: the first announcement, and a new arrival. Awareness is relayed
     and never logged (a joiner replaying it would get cursors for people who
     have gone home), so our announcement is lost to anyone who shows up after
     it — if both clients stayed quiet while they each believed they were
     alone, two people in one document would never discover each other. */
  private publishAwareness(force = false) {
    // Same reason as the doc updates above: y-websocket carries awareness on
    // the socket, so the relay's POST is both redundant and a 405.
    if (this.closed || this.held) return;
    if (!force && this.announced && this.awareness.getStates().size <= 1) return;
    this.announced = true;
    const update = encodeAwarenessUpdate(this.awareness, [this.doc.clientID]);
    void fetch(this.url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ awareness: bytesToB64(update), cid: this.cid }),
    }).catch(() => {
      // A lost cursor position corrects itself on the next keystroke.
    });
  }

  /* The hub holds the document.

     y-websocket rather than the hand-rolled SSE-down/POST-up provider below,
     and rather than @hocuspocus/provider which the plan named: ygo speaks
     y-websocket natively and Hocuspocus only behind a server flag, so this is
     one fewer thing that has to agree. What it buys is the part that was
     hand-rolled badly — a state-vector handshake instead of replaying the
     whole room log on every reconnect, and backoff that is somebody else's
     problem.

     Nothing seeds here. The server built the document from the file before
     anyone attached (fileSeed.LoadDoc), which is what makes the seed claim,
     its grace timer, and the blank-document failure it papered over all
     unnecessary. */
  private connectHeld() {
    const u = new URL(this.url, location.href);
    const path = u.searchParams.get("path") ?? "";
    const base =
      (u.protocol === "https:" ? "wss://" : "ws://") + u.host + u.pathname;
    // The room argument is decoration: the hub names the room itself from
    // (project, path) after it has resolved who is asking, because a caller
    // who could name it could join any project's document by asking for it.
    const provider = new WebsocketProvider(base, "held", this.doc, {
      params: { path },
      awareness: this.awareness,
      connect: true,
    });
    this.ws = provider;
    provider.on("status", (e: { status: string }) => {
      this.onStatus(e.status === "connected" ? "live" : "offline");
    });
    provider.on("sync", (synced: boolean) => {
      // "Synced" is the document having arrived, which is the moment the
      // editor may mount on it — the same moment the relay signalled with
      // its `hello`.
      if (synced) this.onSeeded();
    });
  }

  connect() {
    this.onStatus("connecting");
    if (this.held) return this.connectHeld();
    const es = new EventSource(
      this.url + (this.url.includes("?") ? "&" : "?") + "cid=" + this.cid,
    );
    this.es = es;
    es.onmessage = (e) => {
      let f: {
        type: string;
        seed?: boolean;
        log?: string[];
        update?: string;
        awareness?: string;
      };
      try {
        f = JSON.parse(e.data);
      } catch {
        return;
      }
      if (f.type === "hello") {
        this.everConnected = true;
        this.applying = true;
        try {
          for (const u of f.log ?? []) Y.applyUpdate(this.doc, b64ToBytes(u));
        } finally {
          this.applying = false;
        }
        // Empty room: somebody has to put the file's text into the document,
        // and the hub picked us. Done OUTSIDE `applying` so it is broadcast.
        if (f.seed && this.text.length === 0 && this.seedText) {
          this.text.insert(0, this.seedText);
        }
        this.onSeeded();
        this.onStatus("live");
        return;
      }
      if (f.type === "update" && f.update) {
        this.applying = true;
        try {
          Y.applyUpdate(this.doc, b64ToBytes(f.update));
        } finally {
          this.applying = false;
        }
        return;
      }
      if (f.type === "awareness" && f.awareness) {
        const before = this.awareness.getStates().size;
        applyAwarenessUpdate(this.awareness, b64ToBytes(f.awareness), this);
        // Somebody new. They cannot have heard our announcement — it went out
        // before they arrived and nothing replays it — so answer with one.
        if (this.awareness.getStates().size > before) this.publishAwareness(true);
        return;
      }
      if (f.type === "resync") {
        // We missed updates, so this document is no longer trustworthy.
        // Reconnecting re-reads the whole log.
        this.reconnect();
      }
    };
    es.onerror = () => {
      this.onStatus("offline");
      // EventSource retries on its own. But if it has never once connected,
      // the relay is not there at all — an older hub with no such route, or a
      // 403 — and something has to let the editor open anyway, or the caller
      // waits forever for a `hello` that is not coming.
      if (!this.everConnected) this.onUnavailable();
    };
  }

  private reconnect() {
    this.es?.close();
    if (this.closed) return;
    // A fresh doc, or the replayed log would merge into the one we have and
    // double every character it already contains.
    this.connect();
  }

  /* One POST in flight at a time.

     `timer` was cleared before the await, so a keystroke landing mid-request
     armed a SECOND flush that started while the first was still going: fast
     typing put several POSTs on the wire at once, each with its own headers,
     cookie and round trip.

     Serialized, not cancelled. Yjs updates are DELTAS — dropping one in
     flight deletes those keystrokes from every peer and silently diverges the
     document — so anything typed during a send is merged into the next one
     instead. (A cursor position is the opposite kind of value, which is why
     publishAwareness coalesces to the latest and this does not.) */
  private async flush() {
    this.timer = null;
    if (this.sending || !this.pending.length || this.closed) return;
    this.sending = true;
    const merged = Y.mergeUpdates(this.pending);
    this.pending = [];
    let failed = false;
    try {
      const res = await fetch(this.url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ update: bytesToB64(merged), cid: this.cid }),
      });
      if (res.ok) {
        const out = await res.json().catch(() => ({}));
        // The room filled up and was emptied: everyone rebuilds from the file
        // the snapshotter just wrote.
        if (out.full) this.reconnect();
      }
    } catch {
      // Put it back: an update that never reached the relay is an edit no
      // peer will ever see, which is worse than sending it twice (Yjs
      // updates are idempotent).
      this.pending.unshift(merged);
      this.onStatus("offline");
      failed = true;
    } finally {
      this.sending = false;
      // Whatever was typed while that request was in flight, as one more
      // request rather than one per keystroke. Deliberately not armed on the
      // failure path: a dead relay would turn into a POST every 120ms, and a
      // failed update already waits for the next keystroke the way it always
      // has.
      if (!this.closed && !this.timer && this.pending.length && !failed) {
        this.timer = setTimeout(() => this.flush(), FLUSH_MS);
      }
    }
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
    this.awareness.off("update", this.onAwareness);
    if (this.timer) clearTimeout(this.timer);
    if (this.cursorTimer) clearTimeout(this.cursorTimer);
    this.es?.close();
    this.awareness.destroy();
    this.doc.destroy();
  }
}

function b64ToBytes(s: string): Uint8Array {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function bytesToB64(b: Uint8Array): string {
  let s = "";
  for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
  return btoa(s);
}
