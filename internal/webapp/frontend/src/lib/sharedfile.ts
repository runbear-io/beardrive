import { CollabDoc, type CollabStatus } from "./collab";
import { textEdit } from "./diff";
import { HttpError, putText } from "../api/http";
import { conflictName } from "./conflict";
import { fetchBlobText, fileURLFor } from "../hooks/useBlob";

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
  merge(next: string, nextSha?: string): MergeResult;
  /** Tell this file the head sha moved without its content being news to us —
      a co-editor saved the document we already share. Without it every other
      member of a room keeps saving against a base the hub has moved past, and
      is refused for a conflict that does not exist. */
  rebase(sha?: string): void;
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
        /* A 409 in a shared room is usually a LOST RACE, not a disagreement.

           Everyone in the room holds one document, so a co-editor's save is
           a past state of the text we are about to write: their bytes are
           already in our CRDT. If they land between our reading `base` and
           our PUT arriving, the hub is right to refuse us — our base moved —
           but parking the result beside the file is nonsense, because our
           text already contains theirs. A real session did exactly that and
           collected dozens of conflict copies of files against themselves.

           No amount of care about WHEN we adopt a peer's sha closes this:
           the window is between the read and the write, and something has to
           happen after the refusal. So, once: re-read, check that our text
           still contains theirs, rebase and try again. Same shape as the
           hub's own journal append (appendConditional), for the same reason.

           If containment fails, the writer was an outsider — an agent, the
           CLI, another device — whose work is NOT in our document, and the
           conflict copy below is exactly right. */
        if (await rebaseOnLostRace(text)) return;
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

  /* rebaseOnLostRace turns a refused save into a retried one, ONCE, when the
     refusal was a race rather than a disagreement.

     Two ways a refusal is a race rather than a disagreement, and BOTH have
     to be handled — shipping only the first is what produced conflict copies
     that were subsets of the files they sat beside:

       ours contains theirs  -> our write is the superset; rebase and retry
       theirs contains ours  -> the file is ahead; drop the write entirely

     Returns true when there is nothing left to do. Deliberately not a loop:
     if neither containment holds, the path is genuinely contended by someone
     outside the room and a conflict copy is the honest answer. */
  const rebaseOnLostRace = async (text: string): Promise<boolean> => {
    try {
      const head = await fetchBlobText(fileURLFor(opts.apiBase, opts.path));
      if (head.kind !== "text" || !head.sha) return false;

      /* THE FILE IS AHEAD OF US.

         Our text holds nothing the file lacks — a peer saved a newer state of
         the document we share while our save was in flight, so what we were
         writing is simply out of date. There is nothing to write and nothing
         to preserve: parking it beside the file produces a conflict copy that
         is a SUBSET of the file it sits next to, which is what shipped in the
         first version of this fix and what put four more copies in a project
         minutes after it deployed.

         Record where the file is and go clean. Our own document catches up
         over the websocket like everyone else's. */
      const oursToTheirs = textEdit(text, head.text);
      if (!oursToTheirs || oursToTheirs.from === oursToTheirs.to) {
        saved = head.text;
        rebase(head.sha);
        setState("clean");
        return true;
      }

      // Their text must be contained in ours, or we would be erasing work
      // this document never saw. Same test merge() uses, same reason.
      const theirsToOurs = textEdit(head.text, text);
      if (theirsToOurs && theirsToOurs.from !== theirsToOurs.to) return false;
      rebase(head.sha);
      const out = await putText(contentURL(opts.path), text, base);
      saved = text;
      if (out.sha) base = out.sha;
      setState("clean");
      opts.onSaved?.(text);
      return true;
    } catch {
      return false; // fall through to the conflict copy
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

  /* MY changes restart the idle timer. A peer's do not.

     This used to be "any change, mine or a peer's — whoever stops typing last
     writes the file", on the reasoning that the content is identical for
     everyone so a second writer is a harmless no-op put. It is not harmless.
     In a room of N editors, 700ms after the last keystroke by ANYONE, all N
     clients PUT the identical full body: N uploads of the same text, and a
     History feed that grows N versions every time the room goes quiet. That
     is the "too many change histories" report, and four co-editors made it
     four of everything (docs/hub-load-prd.md Stage 4).

     Dropping to one writer loses nothing. Every copy is identical by
     construction — that is what the CRDT is for — and the hub snapshots the
     room itself when the last editor leaves (ycollab.go, OnLastPeer), so the
     closed-laptop case never depended on a bystander saving on the typist's
     behalf either.

     Seeding is a remote change too, which is a second thing this fixes: the
     document arriving from the hub used to mark the buffer dirty and schedule
     a save of bytes the file already contained. */
  const onDocChange = (_e: unknown, tx: { origin?: unknown }) => {
    if (collab.isRemote(tx?.origin)) return;
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
  /* The file's current sha, learned from somewhere other than our own write.

     `base` is the If-Match this editor saves against, and until now the ONLY
     thing that moved it was a successful save of our own. That is fine alone
     and wrong in a room: a co-editor's save moves the file's head, every other
     member keeps the sha from before it, and their next save is a 409 against
     a file that already contains their work.

     The hub is right to refuse it — a stale base is exactly what If-Match is
     for — and the conflict copy that followed was right for the writer it was
     designed for, an agent or a device outside the room. It was nonsense
     between two people in one document: the CRDT had already merged them, so
     the "conflict" was a copy of the file against itself.

     So whoever learns the new sha tells us. Nothing about the CONTENT is
     assumed here; the caller has just read this path from the hub. */
  const rebase = (sha?: string) => {
    if (sha) base = sha;
  };

  const merge = (next: string, nextSha?: string): MergeResult => {
    // The file is at the content we last knew about: our own write coming
    // back through the change stream, or nothing new at all. `inFlight`
    // covers the common case that `saved` cannot — the frame announcing our
    // own write arriving before that write's own response does.
    /* Adopting the sha is NOT the same decision as accepting the content —
       see the two-questions note below. It never happens here, though: at
       this point we have not yet read the buffer, so we cannot know whether
       their bytes are already in it. */
    if (next === saved || inFlight.has(next)) {
      rebase(nextSha);
      return "same";
    }
    const cur = current();
    // Already in the buffer: a co-editor snapshotted the document we share.
    // Recording it is what keeps the check above true for the rest of the
    // session, so their next save is not mistaken for an outsider's.
    if (next === cur) {
      saved = next;
      rebase(nextSha);
      return "same";
    }
    /* Two different questions, which used to be one.

       "Can we splice their write into the buffer?" and "is their VERSION a
       valid base for our next write?" are not the same question, and
       answering only the first is what put a co-editing session into a loop
       of conflict copies.

       In a room everyone shares one document, so a peer's save is a PAST
       STATE of the text we are holding: their bytes are already in our CRDT,
       arriving over the websocket rather than through this function. We must
       not splice it (it is already applied, and splicing would apply it
       twice) — but their sha is exactly the right base for our next save,
       because our text is theirs plus whatever we have typed since.

       Not assumed, checked. Turning their text into ours deletes nothing iff
       ours is a strict superset of theirs, which is precisely the condition
       that makes their version a safe base. An outsider's write — an agent,
       the CLI, another device — fails that test, keeps the old base, and is
       still refused with a conflict copy, which is what If-Match is for. */
    const theirsToOurs = textEdit(next, cur);
    const oursContainsTheirs = !theirsToOurs || theirsToOurs.from === theirsToOurs.to;

    // Unsaved local edits: not ours to resolve. Splicing over a half-typed
    // sentence is the one thing this must never do.
    if (cur !== saved) {
      if (oursContainsTheirs) rebase(nextSha);
      return "blocked";
    }
    // A co-editor in the room: they are looking at the same stale document
    // and would compute the same splice, and two identical splices into one
    // CRDT is the change applied twice.
    if (collab.peerCount() > 0) {
      if (oursContainsTheirs) rebase(nextSha);
      return "blocked";
    }
    const e = textEdit(cur, next);
    if (!e) return "same";
    if (!collab.text.length && !opts.soloApply) return "blocked";
    // Before the splice, not after: the document change it causes schedules a
    // save, and this is what makes that save a no-op instead of a write-back
    // of what we just read.
    saved = next;
    rebase(nextSha);
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
    rebase,
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
