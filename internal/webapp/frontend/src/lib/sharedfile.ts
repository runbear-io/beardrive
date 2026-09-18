import { CollabDoc, type CollabStatus } from "./collab";
import { textEdit } from "./diff";
import { putText } from "../api/http";

/* One open file, shared with everyone else who has it open, and written back
   to the hub when the typing stops.

   This is the half of editing that has nothing to do with a keyboard: join the
   room, hold the CRDT, save on idle, save once more on the way out. Both
   editing surfaces sit on it —

   - the source editor (Editor.tsx), which binds CodeMirror to `text`
   - the visual editor (VisualEdit.tsx), which splices patches into `text`
     when someone clicks a paragraph on the rendered page

   — and they share a Y.Text of the FILE SOURCE, which is what lets them
   co-edit each other. Somebody in CodeMirror and somebody clicking a headline
   are in the same room, and neither surface has to know the other exists.

   Nothing here knows what the text means. The source editor treats it as
   markup to type; the visual editor treats it as a string to splice by byte
   range; the file is the same either way. */

export type SaveState = "clean" | "dirty" | "saving" | "error";

/* What became of an outside write offered to an open document.
   "same" — nothing new: our own write coming back, or text we already hold.
   "merged" — spliced into the live buffer.
   "blocked" — refused, because applying it would have destroyed something. */
export type MergeResult = "same" | "merged" | "blocked";

// Idle time before the document is written to the file.
export const SAVE_IDLE_MS = 700;

export type SharedFile = {
  readonly collab: CollabDoc;
  /** Text as it stands, whether the relay ever answered or not. */
  current(): string;
  /** Force a save now, skipping the idle wait. */
  saveNow(): Promise<void>;
  /** Fold an outside write — an agent, a CLI, another device — into the open
      document. "blocked" means the caller should SAY so instead, because this
      refused to resolve it. */
  merge(next: string): MergeResult;
  destroy(): void;
};

export function openSharedFile(opts: {
  apiBase: string;
  path: string;
  /** The file's current bytes, used only if this client seeds the room. */
  seed: string;
  me?: { name: string; colour: string };
  /** The relay answered: the shared document is live and holds the truth. */
  onReady: (collab: CollabDoc) => void;
  /** No relay at all — an older hub, or a desktop build that does not proxy
      the route. The caller falls back to single-writer editing. */
  onSolo: () => void;
  onState?: (s: SaveState) => void;
  onCollab?: (s: CollabStatus) => void;
  onPeers?: (n: number) => void;
  onSaved?: (text: string) => void;
  /** Called immediately BEFORE a write goes out, not after it lands.
      The change stream announces a write as soon as the hub journals it,
      which can be before the PUT's own response gets back here — so a caller
      that only learns about its own write afterwards will see its own save
      arrive as if a stranger had made it. */
  onWriting?: () => void;
  /** Text to save when the relay never answered, so there is no CRDT to read
      it from. The solo surface owns its own buffer. */
  soloText?: () => string;
  /** Apply a merged-in outside write to that same solo buffer. Without it a
      relay-less surface cannot take one, and merge() says so rather than
      claiming a change it could not make. */
  soloApply?: (e: { from: number; to: number; insert: string }) => void;
}): SharedFile {
  let saved = opts.seed;
  let timer: ReturnType<typeof setTimeout> | null = null;
  const setState = (s: SaveState) => opts.onState?.(s);

  const save = async (text: string) => {
    if (text === saved) return;
    setState("saving");
    opts.onWriting?.();
    try {
      await putText(
        opts.apiBase + "upload/content?path=" + encodeURIComponent(opts.path),
        text,
      );
      saved = text;
      setState("clean");
      opts.onSaved?.(text);
    } catch {
      // Keep the document: the CRDT is the truth until a save lands, and the
      // next edit schedules another attempt.
      setState("error");
    }
  };

  const collab = new CollabDoc(
    opts.apiBase + "collab?path=" + encodeURIComponent(opts.path),
    opts.seed,
    (s) => opts.onCollab?.(s),
    () => opts.onReady(collab),
    () => opts.onSolo(),
    opts.me,
  );

  // Any change to the shared document — mine or a peer's — restarts the idle
  // timer. Whoever stops typing last writes the file, and because the content
  // is identical for everyone, a second writer is a no-op put of a blob the
  // store already has.
  const onDocChange = () => {
    setState("dirty");
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => save(collab.text.toString()), SAVE_IDLE_MS);
  };
  collab.text.observe(onDocChange);
  const onPeerChange = () => opts.onPeers?.(collab.peerCount());
  collab.awareness.on("change", onPeerChange);
  collab.connect();

  // Whichever surface is live holds the truth: the CRDT when the relay
  // answered, the surface's own buffer when it never did.
  const current = () =>
    collab.text.length ? collab.text.toString() : (opts.soloText?.() ?? "");

  /* Somebody wrote this file while it was open here.

     The editor deliberately does not re-seed itself from the server — that
     would reset the document under a typist's cursor — so this is the other
     way the change gets in: as the one splice that turns what we have into
     what the file says, leaving every untouched character (and so every
     cursor and every remote caret) exactly where it was.

     It refuses in the two cases where a splice would destroy something, and
     the refusal is the caller's banner. */
  const merge = (next: string): MergeResult => {
    // The file is at the content we last knew about: our own write coming
    // back through the change stream, or nothing new at all.
    if (next === saved) return "same";
    const cur = current();
    // Already in the buffer: a co-editor snapshotted the document we share.
    // Recording it is what keeps the check above true for the rest of the
    // session, so their next save is not mistaken for an outsider's.
    if (next === cur) {
      saved = next;
      return "same";
    }
    // Unsaved local edits: not ours to resolve. Splicing over a half-typed
    // sentence is the one thing this must never do.
    if (cur !== saved) return "blocked";
    // A co-editor in the room: they are looking at the same stale document
    // and would compute the same splice, and two identical splices into one
    // CRDT is the change applied twice.
    if (collab.peerCount() > 0) return "blocked";
    const e = textEdit(cur, next);
    if (!e) return "same";
    if (!collab.text.length && !opts.soloApply) return "blocked";
    // Before the splice, not after: the document change it causes schedules a
    // save, and this is what makes that save a no-op instead of a write-back
    // of what we just read.
    saved = next;
    if (collab.text.length) {
      // One transaction, so a peer sees a replacement rather than a delete
      // followed by a moment of missing text.
      collab.doc.transact(() => {
        collab.text.delete(e.from, e.to - e.from);
        collab.text.insert(e.from, e.insert);
      });
    } else {
      opts.soloApply!(e);
    }
    return "merged";
  };

  return {
    collab,
    current,
    merge,
    saveNow: () => save(current()),
    destroy() {
      if (timer) clearTimeout(timer);
      // Closing the tab mid-word must not lose the word.
      const text = current();
      if (text && text !== saved) void save(text);
      collab.awareness.off("change", onPeerChange);
      collab.text.unobserve(onDocChange);
      collab.destroy();
    },
  };
}
