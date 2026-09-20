import { CollabDoc, type CollabStatus } from "./collab";
import { textEdit } from "./diff";
import { HttpError, putText } from "../api/http";
import { conflictName } from "./conflict";

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
  /** The version those bytes ARE (the read's ETag). Every save says it was
      built on this, so the hub can refuse one that would erase a write that
      landed in between. Absent (an older hub, a source with no ETag) means
      unconditional writes, exactly as before. */
  baseSha?: string;
  /** Names this client in a conflict copy's filename, the way a device id
      does on the sync path. */
  who?: string;
  /** A concurrent edit could not be merged, so this client's version was
      preserved beside the file instead of being dropped. */
  onConflictCopy?: (path: string) => void;
  me?: { name: string; colour: string };
  /** The document arrived: it is live and holds the truth. */
  onReady: (collab: CollabDoc) => void;
  /** The document could not be reached — an older hub, or a proxy that will
      not upgrade a websocket. The caller falls back to single-writer editing:
      the editor still opens and still saves, it just has no live
      collaboration. Deliberately NOT a second CRDT path. */
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
  let base = opts.baseSha;
  /* The texts of writes that have left but not landed.

     A SET, not a slot: a second idle timer can fire while the first PUT is
     still out — type, pause, type again on a slow link — and a single slot
     would forget the earlier write exactly when its own refetch arrives. That
     is not hypothetical, it is what the first version of this fix did, and
     the e2e caught it.

     `saved` only moves when a PUT RESOLVES, and the hub announces a write the
     moment it journals it — so the change frame, and the refetch it triggers,
     routinely beat our own response back. Without this, that refetch looked
     like a stranger's write: merge() saw text that matched neither `saved`
     nor a buffer we had typed further into, and raised "someone else changed
     this file" over the user's own save. It also stops a second idle timer
     re-sending bytes that are already on their way, which is how two
     byte-identical PUTs 4.5s apart ended up in one user's history. */
  const inFlight = new Set<string>();
  let timer: ReturnType<typeof setTimeout> | null = null;
  const setState = (s: SaveState) => opts.onState?.(s);

  const contentURL = (p: string) =>
    opts.apiBase + "upload/content?path=" + encodeURIComponent(p);

  const save = async (text: string) => {
    if (inFlight.has(text)) return; // already on its way; its response decides
    if (text === saved) {
      // Nothing to write IS clean, and saying so matters: seeding the room
      // marks the document dirty, and the save it schedules lands here — so
      // without this the status line read "unsaved" for the rest of a session
      // in which everything had been saved all along.
      setState("clean");
      return;
    }
    setState("saving");
    opts.onWriting?.();
    inFlight.add(text);
    try {
      const out = await putText(contentURL(opts.path), text, base);
      saved = text;
      if (out.sha) base = out.sha;
      setState("clean");
      opts.onSaved?.(text);
    } catch (e) {
      if (e instanceof HttpError && e.status === 409) {
        inFlight.delete(text); // preserve() writes under a different path
        await preserve(text, e);
        return;
      }
      // Keep the document: the CRDT is the truth until a save lands, and the
      // next edit schedules another attempt.
      setState("error");
    } finally {
      inFlight.delete(text);
    }
  };

  /* Somebody else's write landed on this path while we were editing, and the
     two versions cannot be reconciled here — if they could, merge() would
     already have done it, because a save only happens when this buffer has
     changes of its own.

     So neither version is dropped. Theirs is the file (it got there first);
     ours goes beside it under the same name the sync path has used since the
     beginning, `<name>.bdrive-conflict-<who>-<utc>`, which the reader already
     knows how to explain (lib/conflict.ts, ConflictBanner).

     The alternative is what this door did until now: take the body wholesale
     and erase their work silently. Two browsers holding different documents —
     one that had lost the co-editing relay — did exactly that to each other
     every few seconds, with nothing anywhere to say so. */
  const preserve = async (text: string, e: HttpError) => {
    let theirs = "";
    try {
      theirs = (JSON.parse(e.body) as { sha?: string }).sha ?? "";
    } catch {
      /* an older hub, or a body we cannot read: the copy still matters */
    }
    const copy = conflictName(opts.path, opts.who || "browser", new Date());
    try {
      await putText(contentURL(copy), text); // unconditional: a new path
      saved = text;
      base = theirs;
      setState("clean");
      opts.onConflictCopy?.(copy);
    } catch {
      // Could not even park it. Stay dirty and loud rather than pretend.
      setState("error");
    }
  };

  const collab = new CollabDoc(
    opts.apiBase + "ycollab?path=" + encodeURIComponent(opts.path),
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
    // back through the change stream, or nothing new at all. `inFlight`
    // covers the common case that `saved` cannot — the frame announcing our
    // own write arriving before that write's own response does.
    if (next === saved || inFlight.has(next)) return "same";
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
