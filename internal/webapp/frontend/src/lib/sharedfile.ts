import { CollabDoc, type CollabStatus } from "./collab";
import { hasCollabRoute } from "./collabroute";
import { diffSplices, merge3ForPath, retainsChanges } from "./merge3";
import { HttpError, putText, syncCollab } from "../api/http";
import { conflictName } from "./conflict";
import { fetchBlobText, fileURLFor } from "../hooks/useBlob";
import { byteLength, sharpShrink } from "./meeting";

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
function isShrinkRefusal(e: HttpError): boolean {
  try {
    return (JSON.parse(e.body) as { shrink?: boolean }).shrink === true;
  } catch {
    return false;
  }
}

export type MergeResult = "same" | "merged" | "blocked";

export type ShrinkAsk = {
  // The file's size and the document's, in bytes, when the shrink was seen.
  was: number;
  now: number;
  // Write the document as it stands at the moment this is called.
  confirm: () => Promise<void>;
  // Put the file's current bytes back into the document.
  keep: () => Promise<void>;
};

// Idle time before the document is written to the file.
export const SAVE_IDLE_MS = 700;

// Backoff between attempts to re-send a save that failed transiently (a 5xx,
// a 429, no answer). The last value repeats while the buffer stays unsaved.
export const SAVE_RETRY_MS = [2_000, 4_000, 8_000, 15_000];

// How long edits may sit queued in a document that has lost its socket before
// they are handed to the hub over HTTP instead.
const OFFLINE_FLUSH_MS = 2_000;

// Consecutive refusals one save may rebase across before its text is parked
// beside the file. A path contended this hard is not a race any more.
const MAX_REBASES = 4;

// Conflict-copy names tried past a taken second (another tab of the same
// account parking in the same seconds) before the park counts as failed. On
// the way out fewer: the page may be gone before the answers come back.
const MAX_CONFLICT_TRIES = 64;
const MAX_EXIT_COPY_TRIES = 8;

// Above this, a staging checkpoint names its ancestor by sha alone when it can
// (the hub reads the blob) instead of carrying it as text.
const INLINE_BASE_MAX = 32_000;

// How many of a held room's recent texts are remembered, to recognise a
// snapshot of this room coming back through the change stream (merge()).
const SEEN_ROOM_TEXTS = 32;

export type SharedFile = {
  readonly collab: CollabDoc;
  /** Text as it stands, whether the relay ever answered or not. */
  current(): string;
  /** Force a save now, skipping the idle wait (Done, Mod-s). In a live room
      this hands the hub the room's state only when this tab has typing no
      snapshot carries yet; clean or a bystander, it sends nothing. */
  saveNow(): Promise<boolean>;
  /** A shrink question is waiting on the user: leaving now would leave it
      unanswered (Done stays on the editor). */
  askingShrink(): boolean;
  /** Fold an outside write — an agent, a CLI, another device — into the open
      document. "blocked" means the caller should SAY so instead, because this
      refused to resolve it. `held` says the hub has already put this text
      into the shared document (a restore into a live room): it is recorded
      as the file's content and never spliced here, which would apply it
      twice. */
  merge(next: string, nextSha?: string, how?: { held?: boolean }): MergeResult;
  /** Tell this file the head sha moved without its content being news to us —
      a co-editor saved the document we already share. Without it every other
      member of a room keeps saving against a base the hub has moved past, and
      is refused for a conflict that does not exist. */
  rebase(sha?: string): void;
  destroy(): void;
};

export type SharedFileOptions = {
  apiBase: string;
  path: string;
  /** The file's current bytes: what a surface typing before the room answers
      starts from, and the base its typing is merged against when it does. */
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
  /** A save would empty or sharply shrink the file: the hub refused it, or —
      in a hub-held room, where the hub writes and so the refusal never
      reaches a client — this client saw the document shrink that far. The
      file keeps its bytes until the user picks one of the two answers. */
  onShrinkRefused?: (ask: ShrinkAsk) => void;
  /** The file was destroyed with that question still open: close it. Neither
      answer runs after destroy — both are no-ops on a closed file. */
  onShrinkWithdrawn?: (ask: ShrinkAsk) => void;
  /** Local edits made while the room is not live: they are queued in this
      tab's document and merge when the socket comes back (or are handed to
      the hub over HTTP if it stays away). 0 once they have. */
  onPending?: (n: number) => void;
  me?: { name: string; colour: string };
  /** The document arrived: it is live and holds the truth. Called again on
      every reconnect. Anything typed into a staging surface before it has
      already been merged into `collab.text` when this runs. */
  onReady: (collab: CollabDoc) => void;
  /** The server has no co-editing room at all (a single-volume server). The
      caller edits a buffer of its own and this file saves it with If-Match,
      rebasing over other writers three ways. Never called for a room that is
      merely slow — see onStalled. */
  onSolo: () => void;
  /** The room has not answered yet and is still being retried. The caller
      MAY mount a staging surface over `seed` (soloText/soloApply, saveNow on
      idle): its typing is checkpointed into the room over HTTP and merged
      into the shared document the moment the socket syncs. */
  onStalled?: () => void;
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
  /** The caller's own buffer while there is no shared document under it
      (solo, or staging before the room answers). Must return `seed` while
      nothing has been typed. */
  soloText?: () => string;
  /** Apply a merged-in change to that same buffer. Without it a relay-less
      surface cannot take one, and merge() says so rather than claiming a
      change it could not make. */
  soloApply?: (e: { from: number; to: number; insert: string }) => void;
  /** A surface whose edits reach `soloText` asynchronously (VisualEdit's
      iframe) brings them all in and stops taking more before the join reads
      the buffer; the join calls the returned release once it is done. */
  beforeJoin?: () => Promise<() => void>;
};

/* Three ways to be connected, and what writes the file in each:

   - hub-held (`hubHoldsIt`): the socket synced at least once. The hub is the
     ONLY writer (ycollab.go). Edits made while the socket is down queue in
     the Y.Doc and, if it stays down, are handed over as a Yjs update on
     POST ycollab/sync — never as a PUT that would race the hub's own write.
   - staging: the room has a route but has not answered yet. Nothing goes
     into the Y.Doc (a locally built copy of the file merged with the hub's
     would hold every character twice); the caller's buffer is checkpointed
     into the room as TEXT over POST ycollab/sync, and three-way merged into
     the shared document the moment the socket syncs.
   - solo: no room route at all. The file is written with If-Match by a
     single-flight loop that rebases over other writers with merge3, and
     parks a conflict copy only for a genuine same-token conflict. */
export function openSharedFile(opts: SharedFileOptions): SharedFile {
  // Every merge here is of this file: a .json/.jsonl merge that is not
  // valid JSON is a conflict, as the hub's is.
  const mergeForPath = (base: string, a: string, b: string) => merge3ForPath(opts.path, base, a, b);
  /* The text `base` names: the last version of the file this buffer is known
     to be built on. Every local change is (buffer − saved); every outside
     write is (next − saved). That is what makes a three-way merge possible. */
  let saved = opts.seed;
  let base = opts.baseSha;
  /* Only two texts are ever "ours" and so never merged back in: `saved`, the
     version the buffer is built on, and `inFlightSentText`, the one save on
     the wire right now (its echo through the change stream routinely beats
     the response back). Nothing older is remembered: a text this client once
     wrote can come back legitimately — a History restore from B back to A —
     and must then be merged like any other outside write. */
  const adopt = (text: string, sha?: string) => {
    saved = text;
    savedSha = sha;
    rebase(sha);
    // Only room states NEWER than `saved` stay recognisable as this room's
    // own past; a text not among them (an outside write, a restore) leaves
    // nothing older to match.
    const idx = seenRoomTexts.lastIndexOf(text);
    if (idx >= 0) seenRoomTexts.splice(0, idx + 1);
    else seenRoomTexts.length = 0;
  };
  /* The sha known to spell exactly `saved` — `base` can move on its own (a
     co-editor's save learned from the change stream). When the two agree the
     hub can read the ancestor itself, so a large one is not sent: the request
     stays small enough for a keepalive on the way out, and half the bytes. */
  let savedSha: string | undefined = opts.baseSha;
  const priorSentList = (head: Ambiguous | null): string[] | undefined => {
    if (!head) return undefined;
    const out: string[] = [];
    for (let c: Ambiguous | null = head; c !== null && out.length < 4; c = c.prev) {
      if (c.sent !== c.from && !out.includes(c.sent)) out.push(c.sent);
    }
    return out.length ? out : undefined;
  };
  const roomPayload = (text: string, prior: Ambiguous | null = ambiguousRoomCheckpoint) => {
    const prior_sent = priorSentList(prior);
    return savedSha && savedSha === base && saved.length > INLINE_BASE_MAX
      ? { base, text, ...(prior_sent ? { prior_sent } : {}) }
      : { base, base_text: saved, text, ...(prior_sent ? { prior_sent } : {}) };
  };

  let solo = false;
  // The room route answered 404/405 to an HTTP checkpoint: an older hub.
  // Staging then saves like solo, with If-Match.
  let noRoomHttp = false;
  let hubHoldsIt = false;
  let live = false;
  let gone = false;
  let pending = 0;
  let finalText: string | null = null;
  /* A late join whose buffer conflicts with the room: the buffer is the only
     copy of those words until its conflict copy lands, so it stays the text
     (and the editor stays on it) until then. See join(). */
  let pendingJoin = false;
  let joining = false; // a park of the buffer is on the wire
  /* join() is waiting for a staging checkpoint on the wire to land before it
     reads the buffer and the room: the save loop sends nothing more, so the
     join is the only thing that hands the rest of the buffer to the room. */
  let waitingToJoin = false;
  let parkedName: string | null = null;
  let parkedText: string | null = null; // what `parkedName` was minted for
  let parkedSha = ""; // the head `parkedText` clashed with, kept for a retry
  /* Every conflict copy this file opens gets a name no other copy from it
     has had: conflictName stamps whole UTC seconds, so each name is minted
     at least a second after the last. Two parks on the wire at once — a
     staging checkpoint's and a join's, or either and a teardown — can then
     never share a path, and a slow older PUT can never land over a newer
     one. A name is reused only to retry the same text. */
  let lastConflictMs = 0;
  const mintConflictPath = (now = Date.now()): string => {
    const ms = Math.max(now, lastConflictMs + 1000);
    lastConflictMs = ms;
    return conflictName(opts.path, opts.who || "browser", new Date(ms));
  };
  // The conflict copies whose PUT is on the wire right now, and what each holds.
  const conflictPuts = new Map<string, string>();
  // Bumped per park: only the newest park moves `saved` when it lands.
  let conflictSeq = 0;
  /* A clash (a staging checkpoint the room refused, or a solo write the file
     refused) whose buffer has not durably landed as a conflict copy yet.
     Sending it again on the way out would only be refused again: destroy()
     parks it, and the save loop's retry parks it again under the same name. */
  let stagingConflict = false;
  let joinTimer: ReturnType<typeof setTimeout> | null = null;
  let joinAttempt = 0;
  let readyWaiting = false; // the room synced; onReady waits for the join
  /* A staging checkpoint whose outcome is unknown: it failed in a way (5xx,
     429, 422, no answer) that may come after the hub already put `sent` into
     the room's document. Until a peek or a later checkpoint settles it, a
     join cannot know whether the room holds `sent` — and guessing "not" would
     splice its lines in a second time under fresh item IDs. See join().
     `from` is the ancestor it was sent against (`saved` then); `prev` is an
     earlier checkpoint still unsettled when this one went out, since a later
     one that never reached the room says nothing about it. */
  type Ambiguous = { sent: string; from: string; prev: Ambiguous | null };
  let ambiguousRoomCheckpoint: Ambiguous | null = null;
  // Set when a peek confirms an ambiguous checkpoint is in room memory while
  // its durable write had failed: saveLoop must still call syncCollab to flush
  // storage even when `current() === saved`.
  let roomNeedsFlush = false;

  /* A held room's file is a PAST state of the document by the time anybody
     reads it: with eight people typing, every snapshot the hub writes is
     behind the room the moment it lands. `seenRoomTexts` remembers, oldest
     first, the texts this room has held SINCE `saved` (every change, ours or
     a peer's), so a snapshot coming back through the change stream is
     recognised as our own history rather than refused as an outside write.
     adopt() drops everything up to the adopted text, so a restore to a state
     from before `saved` is never mistaken for one. Bounded: a snapshot older
     than this still passes when the room carries every edit it made. */
  const seenRoomTexts: string[] = [];
  const seeRoomText = (t: string) => {
    if (seenRoomTexts.at(-1) === t) return;
    seenRoomTexts.push(t);
    if (seenRoomTexts.length > SEEN_ROOM_TEXTS) seenRoomTexts.shift();
  };
  /* Typing in a held room that no snapshot is known to carry yet — what an
     explicit save (Done, Mod-s) or a closing tab still has to hand the hub.
     `lastLocalRoomText` is the room's text after the latest such edit;
     `localRoomSeq` counts those edits, so a request that settles can tell
     whether anything was typed while it was out. */
  let localDirtyInRoom = false;
  let lastLocalRoomText: string | null = null;
  let localRoomSeq = 0;
  const roomCaughtUp = () => {
    localDirtyInRoom = false;
    lastLocalRoomText = null;
  };
  // Settled a request that carried every local edit up to `sentSeq`.
  const roomHandedOver = (sentSeq: number) => {
    if (localRoomSeq === sentSeq) roomCaughtUp();
  };
  /* Edits made while a held room's socket is down, as watermarks rather than
     a count: `offlineEditSeq` numbers every one, `offlineAckedSeq` is the
     newest the hub has confirmed. Two flushes settling in any order can only
     move the acknowledgement forward — never subtract the same edits twice. */
  let offlineEditSeq = 0;
  let offlineAckedSeq = 0;
  let offlineFlushing: Promise<void> | null = null;

  /* Single-flight. At most one write is ever on the wire: a second idle timer
     firing while one is out only marks the buffer dirty, and the loop sends
     the latest text when the first lands — rebased onto whatever it learned.
     Two writes in flight from one buffer are two writes against one base,
     and the second is a 409 against our own first. */
  let saving: Promise<void> | null = null;
  let dirtyWhileSaving = false;
  /* The way-out write (flushExit) on the wire. It runs outside `saving` —
     the page may be gone before it lands — but a page that lives on (the
     bfcache) must not checkpoint or join past it: a join that peeks before it
     reaches the room splices its text in, and then it lands a second copy. */
  let exitSaving: Promise<void> | null = null;
  // The text on the wire, so its own echo through the change stream (which
  // routinely beats the response back) is not mistaken for a stranger's.
  // Cleared the moment the request settles.
  let inFlightSentText: string | null = null;
  // An outside write was merged into the buffer while `inFlightSentText` was out.
  let disturbed = false;

  let timer: ReturnType<typeof setTimeout> | null = null;
  let offlineTimer: ReturnType<typeof setTimeout> | null = null;
  const setState = (s: SaveState) => opts.onState?.(s);

  const contentURL = (p: string) =>
    opts.apiBase + "upload/content?path=" + encodeURIComponent(p);
  const syncURL = opts.apiBase + "ycollab/sync?path=" + encodeURIComponent(opts.path);

  /* A save the network or the hub failed for a moment (a 5xx, a 429, no
     answer) is tried again on its own, backing off to a cap, for as long as
     the buffer stays unsaved: typing that stopped would otherwise wait for a
     next keystroke that may never come. A refusal that cannot change by
     waiting (401/403/404/422) is never retried. The next edit's idle timer,
     the room going live, and destroy() each cancel it. */
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  let retryAttempt = 0;
  const cancelRetry = () => {
    clearTimeout(retryTimer ?? undefined);
    retryTimer = null;
  };
  const retryAfterFailure = (e: unknown) => {
    const transient = e instanceof HttpError ? e.status >= 500 || e.status === 429 : true;
    if (!transient || gone || hubHoldsIt) return;
    cancelRetry();
    const delay = SAVE_RETRY_MS[Math.min(retryAttempt, SAVE_RETRY_MS.length - 1)];
    retryAttempt++;
    retryTimer = setTimeout(() => {
      retryTimer = null;
      if (!gone && !hubHoldsIt) void save();
    }, delay);
  };
  // A write got through: the backoff starts over at its first step.
  const landedWrite = () => {
    cancelRetry();
    retryAttempt = 0;
  };

  const scheduleSave = () => {
    cancelRetry();
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => void save(), SAVE_IDLE_MS);
  };

  /* Replace what the buffer holds with `next`, touching only what differs:
     multi-hunk splices, so every character outside them — and every cursor
     and remote caret in them — stays where it was. */
  const applyText = (next: string): boolean => {
    if (gone) {
      finalText = next;
      return true;
    }
    const cur = current();
    if (cur === next) return true;
    const splices = diffSplices(cur, next);
    if (onShared()) {
      collab.applySplices(splices);
      return true;
    }
    if (!opts.soloApply) return false;
    for (const s of splices) opts.soloApply({ from: s.index, to: s.index + s.deleteCount, insert: s.insertText });
    return true;
  };

  const save = (): Promise<void> => {
    if (hubHoldsIt) return Promise.resolve(); // the hub writes a held room
    // The room has the file; the buffer waits on its conflict copy. A save
    // request is a nudge to try parking it again, never a write of its own.
    if (pendingJoin) return joining ? Promise.resolve() : join();
    if (saving) {
      dirtyWhileSaving = true;
      return saving;
    }
    const run = (async () => {
      try {
        while (exitSaving) await exitSaving;
        await saveLoop();
      } finally {
        saving = null;
      }
    })();
    saving = run;
    return run;
  };

  const saveLoop = async () => {
    let rebases = 0;
    for (;;) {
      dirtyWhileSaving = false;
      if (asking || hubHoldsIt || waitingToJoin) return;
      const text = current();
      if (text === saved && !roomNeedsFlush) {
        // Nothing to write IS clean, and saying so matters: a merge that
        // marked the buffer dirty and scheduled this save lands here.
        setState("clean");
        return;
      }
      setState("saving");
      opts.onWriting?.();
      if (stagingConflict) {
        // A park that failed: retry that same copy before anything else, so
        // the backoff timer re-sends the park rather than a write the hub
        // will refuse again. Newer typing is a new attempt from scratch.
        if (parkedName !== null && parkedText === text) {
          if ((await parkConflict(text, parkedSha)) === "stop") return;
          continue;
        }
        stagingConflict = false;
      }
      const r = solo || noRoomHttp ? await sendFile(text) : await sendRoom(text);
      if (r === "stop") return;
      if (r === "rebased") {
        if (++rebases > MAX_REBASES) {
          await parkConflict(current(), base ?? "");
          return;
        }
        continue;
      }
      if (!dirtyWhileSaving) {
        // Typing since the send that no timer has asked for yet: its own
        // idle timer will.
        setState(current() === saved && !roomNeedsFlush ? "clean" : "dirty");
        return;
      }
    }
  };
  let confirmShrinkOnce = false;

  /* Solo: PUT the whole file with If-Match. */
  const sendFile = async (text: string): Promise<"done" | "rebased" | "stop"> => {
    inFlightSentText = text;
    disturbed = false;
    let out: { sha?: string; merged?: boolean };
    try {
      out = await putText(contentURL(opts.path), text, base, confirmShrinkOnce);
    } catch (e) {
      inFlightSentText = null;
      if (e instanceof HttpError && e.status === 422 && isShrinkRefusal(e)) {
        confirmShrinkOnce = false;
        askShrink(text);
        return "stop";
      }
      /* A 409 is a write that landed since `saved`. Ours did not. Rebase over
         it three ways and send again: two people editing different parts of
         a file is not a conflict, and parking one of them beside the file —
         which is what this did until now — erased that edit from the file
         for everyone, every round, until the other side lost too. */
      if (e instanceof HttpError && e.status === 409) {
        return (await rebaseOnHead(saved, false)) ? "rebased" : "stop";
      }
      // Keep the buffer: it is the truth until a save lands. A transient
      // failure is retried on its own; anything else waits for the next edit.
      setState("error");
      retryAfterFailure(e);
      return "stop";
    }
    inFlightSentText = null;
    confirmShrinkOnce = false;
    landedWrite();
    if (out.merged) {
      // The hub folded our body into a newer file (upload.go): the file is
      // not `text`. Learn what it is, carrying anything typed since onto it.
      return (await rebaseOnHead(text, true)) ? "rebased" : "stop";
    }
    // A write merged in while ours was out landed AFTER ours (ours was not
    // refused), so it is the newer version and `saved` already names it.
    if (!disturbed) adopt(text, out.sha);
    opts.onSaved?.(text);
    return "done";
  };

  /* Read the file and rebase the buffer onto it: merge3(from, head, buffer).
     `from` is the version both descend from — `saved` after a refusal, the
     text we sent after the hub merged it. `landed` says our write is in the
     head. False when it could not, and then the buffer has been parked. */
  const rebaseOnHead = async (from: string, landed: boolean): Promise<boolean> => {
    let head;
    try {
      head = await fetchBlobText(fileURLFor(opts.apiBase, opts.path));
    } catch (e) {
      setState("error");
      // The buffer is still unsaved and nothing else may ever ask again (the
      // typing stopped): a read that failed for a moment arms the same retry
      // a failed save does, which re-sends and lands here again once the hub
      // recovers.
      if (current() !== saved) retryAfterFailure(e);
      return false;
    }
    if (head.kind !== "text") {
      setState("error");
      return false;
    }
    const cur = current();
    const res = mergeForPath(from, head.text, cur);
    if (!res.ok || !applyText(res.text)) {
      // The same words changed two different ways: genuinely contended.
      // Theirs is the file (it got there first); ours goes beside it. Only
      // the head moves now: `saved` moves when the copy durably lands.
      if (landed) rebase(head.sha);
      return (await parkConflict(cur, head.sha ?? "")) === "rebased";
    }
    adopt(head.text, head.sha);
    if (landed) opts.onSaved?.(head.text);
    return true;
  };

  /* Staging: hand the typing to the room as text. The hub merges
     (saved, room, text) and answers with the room's text, which this buffer
     is then rebased onto, so the next checkpoint is a small diff again. */
  const applyAmbiguousPeek = (peek: { sha: string; text: string; update?: string }) => {
    if (peek.update) collab.applyRemoteUpdate(peek.update);
    if (typeof peek.text !== "string") {
      ambiguousRoomCheckpoint = null;
      return;
    }
    let landed: Ambiguous | null = null;
    for (let c: Ambiguous | null = ambiguousRoomCheckpoint; c !== null; c = c.prev) {
      if (c.sent !== c.from && retainsChanges(c.from, c.sent, peek.text)) {
        landed = c;
        break;
      }
    }
    ambiguousRoomCheckpoint = null;
    if (landed) {
      roomNeedsFlush = true;
      // The room holds `sent`: what the buffer adds is only past it.
      const res = mergeForPath(landed.sent, peek.text, current());
      if (res.ok && applyText(res.text)) {
        saved = peek.text;
        savedSha = undefined;
        rebase(peek.sha);
      } else {
        saved = landed.sent;
        savedSha = undefined;
      }
    }
  };

  const sendRoom = async (text: string): Promise<"done" | "rebased" | "stop"> => {
    inFlightSentText = text;
    disturbed = false;
    let out: { sha: string; text: string; update?: string };
    const before = ambiguousRoomCheckpoint;
    try {
      ambiguousRoomCheckpoint = { sent: text, from: saved, prev: before };
      out = await syncCollab(syncURL, { ...roomPayload(text, before), ...(confirmShrinkOnce ? { confirm_shrink: true } : {}) });
    } catch (e) {
      inFlightSentText = null;
      // Refusals the hub gives before it touches the room's document; any
      // other failure may have landed `text` there first.
      if (e instanceof HttpError && [400, 401, 403, 404, 405, 409, 413].includes(e.status)) {
        ambiguousRoomCheckpoint = before;
      }
      if (e instanceof HttpError && (e.status === 404 || e.status === 405)) {
        noRoomHttp = true; // an older hub: save the file as solo does
        return "rebased";
      }
      if (e instanceof HttpError && e.status === 422 && isShrinkRefusal(e)) {
        confirmShrinkOnce = false;
        askShrink(text);
        return "stop";
      }
      if (e instanceof HttpError && e.status === 409 && !hubHoldsIt) {
        /* The room changed the same lines this checkpoint did. The checkpoint
           was `text`, but the buffer may have moved on while it was out — and
           its idle timer already asked for that, into this loop — so what is
           settled is the buffer as it is NOW, never the stale `text`. */
        let theirs: { sha?: string; text?: string } = {};
        try {
          theirs = JSON.parse(e.body) as typeof theirs;
        } catch {
          // an older hub, or a body we cannot read: park the buffer
        }
        const room = typeof theirs.text === "string" ? theirs.text : null;
        if (room !== null) {
          const res = mergeForPath(saved, room, current());
          if (res.ok && applyText(res.text)) {
            // Rebased onto the room: send the merge, as a diff against it.
            saved = room;
            savedSha = undefined;
            rebase(theirs.sha);
            return "rebased";
          }
        }
        return parkConflict(current(), theirs.sha ?? "");
      }
      setState(hubHoldsIt ? "dirty" : "error");
      retryAfterFailure(e);
      return "stop";
    }
    inFlightSentText = null;
    confirmShrinkOnce = false;
    landedWrite();
    /* The room's state with this checkpoint in it, with the room's own item
       IDs: in the document before anything else reads it, so a join waiting
       on this request splices only what the room does not hold yet — and the
       same items arriving on the socket, before or after, merge as no-ops.
       Without it a join that read the room before the socket delivered them
       inserted the checkpoint's lines a second time. */
    if (out.update) collab.applyRemoteUpdate(out.update);
    ambiguousRoomCheckpoint = null;
    roomNeedsFlush = false;
    // Joined while this was out: the join merged the same buffer into the
    // shared document, which now holds the truth.
    if (hubHoldsIt) return "stop";
    const res = mergeForPath(text, out.text, current());
    // The buffer is now built on `out.text` even with typing past `text` in
    // it: that is the base the join and the next checkpoint diff against.
    if (res.ok && applyText(res.text)) {
      if (!disturbed) adopt(out.text, out.sha);
    }
    // Otherwise the newest typing could not be carried across here; `saved`
    // stays where it was and the hub's own merge decides on the next checkpoint.
    opts.onSaved?.(out.text);
    return "done";
  };

  /* One question at a time: while the user is deciding, a second refusal or
     a second sighting of the same shrink must not stack another dialog. */
  let asking: ShrinkAsk | null = null;
  const askShrink = (text: string) => {
    if (asking || gone || !opts.onShrinkRefused) return;
    setState("error");
    asking = {
      was: byteLength(saved),
      now: byteLength(text),
      confirm: async () => {
        if (gone) return;
        if (hubHoldsIt) {
          setState("saving");
          opts.onWriting?.();
          // Edits queued offline are in this update too: it acknowledges them.
          const targetOfflineSeq = offlineEditSeq;
          const sentRoomSeq = localRoomSeq;
          try {
            const out = await syncCollab(syncURL, { update: collab.encodeState(), base, confirm_shrink: true });
            adopt(out.text, out.sha);
            offlineAcked(targetOfflineSeq, sentRoomSeq);
            const caughtUp = !localDirtyInRoom && offlineAckedSeq >= offlineEditSeq;
            setState(caughtUp ? "clean" : "dirty");
            opts.onSaved?.(saved);
            // Typed while the question was open: still queued, still sent.
            if (!live && !gone && offlineAckedSeq < offlineEditSeq) {
              asking = null;
              void flushOffline(false);
            }
          } catch {
            setState("error");
          } finally {
            asking = null;
          }
          return;
        }
        // Staging or solo: run through the single-flight `save()` loop so
        // join() awaits it, `ambiguousRoomCheckpoint` tracks it, and the
        // returned room text rebases any concurrent typing.
        confirmShrinkOnce = true;
        asking = null;
        roomNeedsFlush = true;
        await save();
      },
      keep: async () => {
        if (gone) return;
        try {
          const head = await fetchBlobText(fileURLFor(opts.apiBase, opts.path));
          if (gone || head.kind !== "text") return;
          adopt(head.text, head.sha);
          applyText(head.text);
          asking = null;
          /* Socket down: the refused update had already reached the room's
             document (the hub applies before it snapshots), so the room holds
             the shrunk text. Putting the bytes back is an edit the room must
             get — over HTTP now, or by the retry, pagehide or destroy() if
             this attempt fails. */
          if (hubHoldsIt && !live) {
            await flushOffline(false);
            if (gone) return;
            setState(offlineAckedSeq >= offlineEditSeq && !localDirtyInRoom ? "clean" : "dirty");
            return;
          }
          // Live, the socket carries the restoration; staging or solo, the
          // file already holds these bytes. Nothing queued either way.
          offlineAckedSeq = offlineEditSeq;
          if (pending) {
            pending = 0;
            opts.onPending?.(0);
          }
          roomCaughtUp();
          setState("clean");
        } catch {
          setState("error");
        } finally {
          asking = null;
        }
      },
    };
    opts.onShrinkRefused(asking);
  };

  /* In a hub-held room nobody here saves, so the hub's 422 never comes back
     to a client: the room just stops being written. The same rule is checked
     on the document instead, once the typing pauses, against the last text
     the file was known to hold. */
  let shrinkTimer: ReturnType<typeof setTimeout> | null = null;
  const watchShrink = () => {
    if (shrinkTimer) clearTimeout(shrinkTimer);
    shrinkTimer = setTimeout(() => {
      const text = current();
      if (sharpShrink(byteLength(saved), byteLength(text))) askShrink(text);
    }, SAVE_IDLE_MS);
  };

  /* Somebody else's write landed on this path while we were editing, and the
     two versions cannot be reconciled — merge3 found the same words changed
     two different ways. So neither version is dropped: theirs is the file,
     ours goes beside it as `<name>.bdrive-conflict-<who>-<utc>`, which the
     reader already knows how to explain (lib/conflict.ts, ConflictBanner).

     The copy is written create-only (If-None-Match: *). Another tab of the
     same account names its copies on its own clock, so the name may already
     hold ITS words: that is a 409, and the next second is tried. A failure
     with no answer keeps the name, because the write may have landed: the
     retry sends the same bytes to the same name, which the hub answers 200
     unchanged rather than writing a second copy. */
  const preserve = async (
    text: string,
    theirs: string,
    copy = mintConflictPath(),
  ): Promise<{ err: unknown } | null> => {
    const seq = ++conflictSeq;
    let at = copy;
    for (let tries = 1; ; tries++) {
      const target = at;
      conflictPuts.set(target, text);
      // A newer park started since this one (a join's, over newer typing), or
      // the room took the buffer: such a landing settles nothing about `saved`.
      try {
        await putText(contentURL(target), text, undefined, false, false, false, true);
        if (seq === conflictSeq && !hubHoldsIt) {
          saved = text;
          savedSha = undefined;
          if (theirs) base = theirs;
          setState("clean");
        }
        opts.onConflictCopy?.(target);
        return null;
      } catch (e) {
        if (e instanceof HttpError && e.status === 409 && tries < MAX_CONFLICT_TRIES) {
          // Taken by other words. A retry of this text must follow the step.
          at = mintConflictPath();
          if (parkedName === target && parkedText === text) parkedName = at;
          continue;
        }
        // Could not even park it. Stay dirty and loud rather than pretend.
        if (seq === conflictSeq && !hubHoldsIt) setState("error");
        return { err: e };
      } finally {
        if (conflictPuts.get(target) === text) conflictPuts.delete(target);
      }
    }
  };

  /* A conflict copy written on the way out: keepalive, create-only, and
     asking the hub to step to the next free second itself — a discarded page
     never runs the JS that would try again. An older hub answers a taken name
     409; while the page still lives, the next second is tried from here. */
  const parkOnExit = (copy: string, text: string, key: string, landed?: (path: string) => void) => {
    const attempt = (target: string, tries: number) => {
      void putText(contentURL(target), text, undefined, false, true, true, true).then(
        (out) => landed?.(out.parked ?? target),
        (e: unknown) => {
          if (e instanceof HttpError && e.status === 409 && tries < MAX_EXIT_COPY_TRIES) {
            const next = mintConflictPath();
            if (parkedName === target && parkedText === text) parkedName = next;
            attempt(next, tries + 1);
            return;
          }
          exitFailed(key)();
        },
      );
    };
    attempt(copy, 1);
  };

  /* Park `parked` beside the file after a clash with head `theirSha`. One
     name per text: a retry re-writes the same copy. Until it lands the clash
     is unparked (`stagingConflict`), so leaving parks it keepalive, and a
     transient failure re-arms the save loop, which retries this park. */
  const parkConflict = async (parked: string, theirSha: string): Promise<"rebased" | "stop"> => {
    if (parkedName === null || parkedText !== parked) {
      parkedName = mintConflictPath();
      parkedText = parked;
    }
    const name = parkedName;
    parkedSha = theirSha;
    stagingConflict = true;
    const failed = await preserve(parked, theirSha, name);
    if (gone) return "stop"; // destroy() parked the buffer itself
    if (failed) {
      retryAfterFailure(failed.err);
      return "stop";
    }
    stagingConflict = false;
    // A newer park (a join's, over newer typing) keeps its own name. This
    // text's name may have stepped past a taken second: compare the text.
    if (parkedText === parked) {
      parkedName = null;
      parkedText = null;
      parkedSha = "";
    }
    // Typing that landed while the copy was on the wire is not in it: it is
    // sent next, built on what was parked.
    return current() !== parked ? "rebased" : "stop";
  };

  /* Edits made while a held room's socket is down live only in this tab's
     Y.Doc. If the socket stays away, hand them to the hub as a Yjs update:
     applying it to the room is idempotent, so the reconnect that eventually
     comes adds nothing twice, and a tab closed in the meantime loses nothing. */
  const offlineAcked = (targetSeq: number, sentRoomSeq: number) => {
    offlineAckedSeq = Math.max(offlineAckedSeq, targetSeq);
    roomHandedOver(sentRoomSeq);
    if (gone || live) return;
    pending = offlineEditSeq - offlineAckedSeq;
    opts.onPending?.(pending);
  };
  /* Refused or unwritten (a 503 while another process has not written it
     yet, quota, storage): the edits are still queued in the document and
     `pending` still counts them. The socket, the next edit, or this timer
     retries — the last so a tab that stays offline and idle does not wait
     forever for a nudge. */
  const retryOffline = () => {
    if (gone || live || asking || offlineTimer) return;
    offlineTimer = setTimeout(() => {
      offlineTimer = null;
      if (!live && !gone) void flushOffline(false);
    }, OFFLINE_FLUSH_MS);
  };
  /* A keepalive flush (the page is going) fires at once and rejects when it
     fails, so the exit path can offer it again. Any other is single-flight:
     concurrent callers share one loop, which sends until the hub has
     acknowledged every edit made so far. */
  const flushOffline = (keepalive: boolean): Promise<void> => {
    clearTimeout(offlineTimer ?? undefined);
    offlineTimer = null;
    if (keepalive) {
      if (offlineAckedSeq >= offlineEditSeq) return Promise.resolve();
      const targetSeq = offlineEditSeq;
      const sentRoomSeq = localRoomSeq;
      return syncCollab(syncURL, { update: collab.encodeState(), base }, true).then((out) => {
        rebase(out.sha);
        offlineAcked(targetSeq, sentRoomSeq);
      });
    }
    if (offlineFlushing) return offlineFlushing;
    const run = (async () => {
      try {
        while (!live && !gone && !asking && offlineAckedSeq < offlineEditSeq) {
          const targetSeq = offlineEditSeq;
          const sentRoomSeq = localRoomSeq;
          let out: { sha: string };
          try {
            out = await syncCollab(syncURL, { update: collab.encodeState(), base }, false);
          } catch (e) {
            /* Refused for sharply shrinking the file: no retry can land it
               until the user agrees, so ask now — Done waits on the answer. */
            if (e instanceof HttpError && e.status === 422 && isShrinkRefusal(e)) {
              clearTimeout(shrinkTimer ?? undefined);
              shrinkTimer = null;
              askShrink(current());
              if (asking) return;
            }
            retryOffline();
            return;
          }
          rebase(out.sha);
          offlineAcked(targetSeq, sentRoomSeq);
        }
      } finally {
        offlineFlushing = null;
      }
    })();
    offlineFlushing = run;
    return run;
  };

  /* The first sync: the hub's document has arrived. Whatever was typed into a
     staging buffer in the meantime is merged in three ways — (what it started
     from, what the room holds, what it holds now) — and spliced into the
     shared document as minimal hunks. A checkpoint that already reached the
     room is in both and lands once.

     When the same words changed two ways the buffer is parked beside the file
     as a conflict copy — and the editor stays on that buffer until the copy
     is durably on the hub. Swapping onto the shared document first would
     unmount the only place those words exist: a failed park would then lose
     them outright. A transient failure (5xx, 429, no answer) is retried on
     the save backoff; destroy() parks the buffer on the way out. */
  const finishJoin = (then?: () => void) => {
    pendingJoin = false;
    parkedName = null;
    parkedText = null;
    joinAttempt = 0;
    clearTimeout(joinTimer ?? undefined);
    joinTimer = null;
    roomNeedsFlush = false;
    hubHoldsIt = true;
    seeRoomText(collab.text.toString());
    then?.();
    if (readyWaiting) {
      readyWaiting = false;
      opts.onReady(collab);
    }
  };
  const join = async (): Promise<void> => {
    if (gone || hubHoldsIt || joining || waitingToJoin) return;
    clearTimeout(joinTimer ?? undefined);
    joinTimer = null;
    if (timer) clearTimeout(timer);
    timer = null;
    /* A staging checkpoint still on the wire is part of what the room will
       hold. Read the buffer and the room before it lands and the merge sees
       its lines as typing the room lacks, splices them in, and the room's
       own copy of them arrives after: every checkpointed line twice. So wait
       for it — the loop sends no further checkpoint meanwhile — and let it
       hand its items to the document first (sendRoom). */
    if (saving || exitSaving) {
      waitingToJoin = true;
      try {
        // The way-out checkpoint too: a page back from the bfcache joins
        // only once it has landed (or failed, and is named for the peek).
        while (saving || exitSaving) await (saving ?? exitSaving);
      } finally {
        waitingToJoin = false;
      }
      if (gone || hubHoldsIt) return;
      if (!live) {
        // The socket dropped while we waited: staging again, and the typing
        // the loop held back still has to reach the room.
        if (current() !== saved) scheduleSave();
        return;
      }
    }
    /* A checkpoint that failed after the hub may already have put it into
       the room (a 5xx, a 422, an answer that never came back) leaves the
       room's content unknown: its items may still be on their way down the
       socket. Merging from `saved` now would splice its lines in a second
       time under fresh item IDs — and when the room's own copy arrives,
       every one of them twice. So ask the room first (a peek writes
       nothing), take its items into the document, and if it holds the
       checkpoint, rebase the buffer over it before the join reads either. */
    if (ambiguousRoomCheckpoint !== null && !noRoomHttp) {
      let peek: { sha: string; text: string; update?: string };
      waitingToJoin = true; // no checkpoint goes out while the room is read
      try {
        peek = await syncCollab(syncURL, { peek: true });
      } catch {
        if (gone || hubHoldsIt) return;
        const delay = SAVE_RETRY_MS[Math.min(joinAttempt, SAVE_RETRY_MS.length - 1)];
        joinAttempt++;
        joinTimer = setTimeout(() => {
          joinTimer = null;
          void join();
        }, delay);
        return;
      } finally {
        waitingToJoin = false;
      }
      if (gone || hubHoldsIt) return;
      applyAmbiguousPeek(peek);
      if (!live) {
        if (current() !== saved || roomNeedsFlush) scheduleSave();
        return;
      }
    }
    /* The buffer the join is about to read may still be arriving (an
       iframe's patches over postMessage): bring it all in and hold the
       surface still until the join is done. No checkpoint goes out meanwhile:
       the join is what hands the buffer to the room. */
    let unlockSurface: (() => void) | undefined;
    if (opts.beforeJoin) {
      waitingToJoin = true;
      try {
        unlockSurface = await opts.beforeJoin();
      } catch {
        // A surface that cannot be drained joins with what it has.
      } finally {
        waitingToJoin = false;
      }
      if (gone || hubHoldsIt) {
        unlockSurface?.();
        return;
      }
      if (!live) {
        unlockSurface?.();
        if (current() !== saved || roomNeedsFlush) scheduleSave();
        return;
      }
    }
    try {
      const local = opts.soloText?.();
      if (local === undefined || local === saved) return finishJoin();
      const hub = collab.text.toString();
      const res = mergeForPath(saved, hub, local);
      if (res.ok) return finishJoin(() => collab.applySplices(diffSplices(hub, res.text)));
      pendingJoin = true;
      // One name for every attempt at the same text: a retry re-writes the same
      // copy, never a second. Newer text — or a staging park still on the wire —
      // gets a freshly minted name, so an older PUT never lands over it.
      if (parkedName === null || parkedText !== local) {
        parkedName = mintConflictPath();
        parkedText = local;
      }
      joining = true;
      let failed: { err: unknown } | null;
      try {
        failed = await preserve(local, base ?? "", parkedName);
      } finally {
        joining = false;
      }
      if (gone) return; // destroy() parked the buffer itself
      if (!failed) {
        // Typing that landed in the buffer while the copy was on the wire is
        // not in it: go round again from what was parked.
        if (opts.soloText?.() !== local) {
          parkedName = null;
          parkedText = null;
          return join();
        }
        // Parked: the editor now shows the room's text, which is what it is built on.
        return finishJoin(() => {
          saved = collab.text.toString();
        });
      }
      const e = failed.err;
      if (e instanceof HttpError && !(e.status >= 500 || e.status === 429)) return; // waits for the next edit
      const delay = SAVE_RETRY_MS[Math.min(joinAttempt, SAVE_RETRY_MS.length - 1)];
      joinAttempt++;
      joinTimer = setTimeout(() => {
        joinTimer = null;
        void join();
      }, delay);
    } finally {
      // Joined (onReady has run), parked, or waiting on a retry: the surface
      // takes typing again.
      unlockSurface?.();
    }
  };

  const collab = new CollabDoc(
    opts.apiBase + "ycollab?path=" + encodeURIComponent(opts.path),
    (s) => {
      live = s === "live";
      if (s === "solo") solo = true;
      if (live) {
        if (offlineTimer) clearTimeout(offlineTimer);
        offlineTimer = null;
        cancelRetry(); // the room holds the buffer now, and the hub writes it
        if (!hubHoldsIt) void join();
        offlineAckedSeq = offlineEditSeq;
        if (pending) {
          pending = 0;
          opts.onPending?.(0);
        }
      }
      opts.onCollab?.(s);
    },
    // The editor moves onto the shared document only once the join is done:
    // a buffer still waiting for its conflict copy to land stays mounted.
    () => {
      if (hubHoldsIt) opts.onReady(collab);
      else readyWaiting = true;
    },
    () => {
      if (!gone && !hubHoldsIt) opts.onStalled?.();
    },
    opts.me,
  );

  /* MY changes restart the idle timer. A peer's do not — and in a held room
     nothing here writes at all: N clients saving one shared document is N
     identical PUTs and N history entries every time the room goes quiet
     (docs/hub-load-prd.md Stage 4). The hub writes it, once. */
  const onDocChange = (_e: unknown, tx: { origin?: unknown }) => {
    const roomText = hubHoldsIt ? collab.text.toString() : null;
    if (roomText !== null) seeRoomText(roomText);
    if (collab.isRemote(tx?.origin)) return;
    setState("dirty");
    if (hubHoldsIt) {
      localDirtyInRoom = true;
      lastLocalRoomText = roomText;
      localRoomSeq += 1;
      if (!live) {
        // Counted whether or not the caller shows it (VisualEdit passes no
        // onPending): `pending` is what tells flushOffline there is anything
        // to hand over on the offline timer and on destroy().
        offlineEditSeq += 1;
        pending = offlineEditSeq - offlineAckedSeq;
        opts.onPending?.(pending);
        clearTimeout(offlineTimer ?? undefined);
        offlineTimer = setTimeout(() => {
          offlineTimer = null;
          if (!live && !gone) void flushOffline(false);
        }, OFFLINE_FLUSH_MS);
      }
      // "dirty" clears when the hub's write comes back through the change
      // stream and merge() finds the file equal to the document.
      watchShrink();
      return;
    }
    scheduleSave();
  };
  collab.text.observe(onDocChange);
  const onPeerChange = () => opts.onPeers?.(collab.peerCount());
  collab.awareness.on("change", onPeerChange);
  /* A server with no room route goes straight to solo; a route that is slow
     is retried for as long as this file is open (CollabDoc.connect). The
     callback is deferred because the caller's own handle to this file is not
     assigned until this returns. */
  if (hasCollabRoute(opts.apiBase)) collab.connect();
  else
    queueMicrotask(() => {
      if (gone) return;
      solo = true;
      collab.stop();
      opts.onSolo();
    });

  // The shared document once the hub has it; before that (or without a room
  // at all) the caller's own buffer — except a solo surface that keeps its
  // text in the Y.Text anyway (VisualEdit).
  const current = () => {
    if (finalText !== null) return finalText;
    if (onShared()) return collab.text.toString();
    return opts.soloText?.() ?? "";
  };
  /* Is the shared document the buffer? Once the hub holds it, yes — and for
     a solo surface that keeps its text in the Y.Text (VisualEdit). Never
     while staging or joining: the document then holds the ROOM's text (the
     socket's, or a checkpoint's answer, sendRoom) while the editor is still
     on the caller's own buffer, which is the only copy of the typing. */
  const onShared = () => hubHoldsIt || (solo && collab.text.length > 0);

  /* The file's current sha, learned from somewhere other than our own write.
     A co-editor's save moves the head; whoever learns the new sha tells us,
     so our next save is not refused for a conflict that does not exist. */
  const rebase = (sha?: string) => {
    if (sha) base = sha;
  };

  const merge = (next: string, nextSha?: string, how?: { held?: boolean }): MergeResult => {
    if (how?.held) {
      adopt(next, nextSha);
      return "same";
    }
    // Our own write coming back (possibly before its response), or nothing new.
    if (next === saved || next === inFlightSentText) {
      rebase(nextSha);
      return "same";
    }
    const cur = current();
    // Already in the buffer: a co-editor snapshotted the document we share.
    if (next === cur) {
      adopt(next, nextSha);
      // For a live room this IS the save landing — the hub's write, back
      // through the stream — so this is where "dirty" honestly ends.
      if (hubHoldsIt) {
        roomCaughtUp();
        setState("clean");
      }
      return "same";
    }

    if (hubHoldsIt) {
      /* In a room a peer's save — or the hub's own snapshot — is a PAST STATE
         of the shared document: its bytes are already in our CRDT, and the
         room has moved on since (peers kept typing, or deleting). Splicing it
         would apply it twice; refusing it would raise a banner over nothing.
         It is the file now, so it is the base the next write builds on. */
      const prevSaved = saved;
      const roomAlreadyHasIt = seenRoomTexts.includes(next) || retainsChanges(prevSaved, next, cur);
      if (roomAlreadyHasIt) {
        adopt(next, nextSha);
        if (
          localDirtyInRoom &&
          (lastLocalRoomText === null ||
            next === lastLocalRoomText ||
            retainsChanges(prevSaved, lastLocalRoomText, next))
        ) {
          roomCaughtUp();
          setState("clean");
        }
        return "same";
      }
      /* An outsider's write that never went through the room (an
         unconditional PUT): with unsaved typing or company it is the caller's
         banner; alone and clean it simply becomes the document. */
      if (cur !== saved || collab.peerCount() > 0) return "blocked";
      adopt(next, nextSha);
      applyText(next);
      roomCaughtUp();
      return "merged";
    }

    /* Solo or staging: three ways. Our unsaved typing is (cur − saved), their
       write is (next − saved); both survive unless they changed the same
       words differently, and only then is this the caller's banner. */
    if (!collab.text.length && !opts.soloApply) return "blocked";
    const res = mergeForPath(saved, next, cur);
    if (!res.ok) return "blocked";
    if (inFlightSentText !== null) disturbed = true;
    adopt(next, nextSha);
    applyText(res.text);
    // What is left over is our own typing, rebased: it still needs saving.
    if (res.text !== next) scheduleSave();
    else if (inFlightSentText === null) setState("clean");
    return res.text === cur ? "same" : "merged";
  };

  /* An explicit save (Done, Mod-s). In a live room idle typing never writes
     anything from here — the hub snapshots the room — but a user who asked to
     save and leave must not leave on a snapshot still pending: the room's
     state goes to the hub, which answers only once storage holds it.

     It sends until nothing is owed, so typing that lands while a pass is on
     the wire is saved (and shrink-checked) by the next pass before it
     answers. True: everything typed so far is saved and no question is open.
     False: a question is open, or a pass failed and the buffer is still
     unsaved — its retry stays armed, and Done stays on the editor. Clean, or
     a bystander: true at once. Concurrent calls share one loop. */
  const owesSave = (): boolean => {
    if (gone) return false;
    if (hubHoldsIt) return live ? localDirtyInRoom : offlineAckedSeq < offlineEditSeq;
    return pendingJoin || stagingConflict || current() !== saved || roomNeedsFlush;
  };
  // A sharp shrink against what the file is known to hold is asked about
  // before any request: the network is never waited on to learn it.
  const askIfShrunk = (): boolean => {
    if (confirmShrinkOnce || !opts.onShrinkRefused || !sharpShrink(byteLength(saved), byteLength(current()))) return false;
    clearTimeout(shrinkTimer ?? undefined);
    shrinkTimer = null;
    askShrink(current());
    return true;
  };
  // One live pass: the room's state to the hub, answered once storage holds it.
  const flushLive = async (): Promise<boolean> => {
    const sentRoomSeq = localRoomSeq;
    setState("saving");
    try {
      const out = await syncCollab(syncURL, { update: collab.encodeState() });
      if (gone) return false;
      if (out.update) collab.applyRemoteUpdate(out.update);
      adopt(out.text, out.sha);
      // Typing that landed while this was out is not in it: owed still.
      roomHandedOver(sentRoomSeq);
      setState(localDirtyInRoom ? "dirty" : "clean");
      opts.onSaved?.(saved);
      return true;
    } catch (e) {
      if (gone) return false;
      /* The hub refused to write a document this much smaller than the
         file without a word from the user: ask now (the caller stays put
         until it is answered), rather than wait for the idle check. */
      if (e instanceof HttpError && e.status === 422 && isShrinkRefusal(e)) {
        clearTimeout(shrinkTimer ?? undefined);
        shrinkTimer = null;
        askShrink(current());
        if (asking) return false;
      }
      // The room still holds every edit and the hub still snapshots it.
      setState(localDirtyInRoom ? "dirty" : "clean");
      return false;
    }
  };
  let explicitSaving: Promise<boolean> | null = null;
  const saveNow = (): Promise<boolean> => {
    if (explicitSaving) {
      // Typing since the pass on the wire: the save loop sends it next.
      if (saving) dirtyWhileSaving = true;
      return explicitSaving;
    }
    if (gone || asking) return Promise.resolve(false);
    if (!owesSave()) return Promise.resolve(true);
    const p = (async (): Promise<boolean> => {
      while (!gone && !asking && owesSave()) {
        if (askIfShrunk()) return false;
        if (hubHoldsIt) {
          if (live) {
            if (!(await flushLive())) return false;
            continue;
          }
          // flushOffline sends until every edit is acknowledged; stopping
          // short of that is a failure (its retry is armed) or a question.
          await flushOffline(false);
          if (gone || asking || (!live && offlineAckedSeq < offlineEditSeq)) return false;
          continue;
        }
        // Staging or solo. A pass that moved nothing failed (a 503, a
        // refused park, a join still waiting): its retry is armed, and
        // looping would only send it again now.
        const beforeText = current();
        const beforeSaved = saved;
        const beforeFlush = roomNeedsFlush;
        await save();
        if (gone || asking) return false;
        if (owesSave() && current() === beforeText && saved === beforeSaved && roomNeedsFlush === beforeFlush) {
          return false;
        }
      }
      return !gone && !asking && !owesSave();
    })();
    const run = p.finally(() => {
      if (explicitSaving === run) explicitSaving = null;
    });
    explicitSaving = run;
    return run;
  };

  /* Leaving mid-word must not lose the word — a raw tab close or reload
     (pagehide) or the surface unmounting (destroy), whichever comes first.
     Every request is keepalive so it outlives the page, and the second of the
     two finds the same state already on the wire and sends nothing. Nothing
     counts as handed over until the hub answers: a failed request, or a page
     restored from the back/forward cache (pageshow), may send it again.
     - held and live: an edit no snapshot is known to carry goes to the hub
       as the room's state; clean or a bystander sends nothing.
     - held, socket down: the queued update goes over HTTP.
     - staging: the buffer goes to the room as text.
     - joining, or a staging checkpoint the room refused for a clash, its
       conflict copy not yet landed: the buffer goes to that copy — the room
       already holds the file's other side.
     - solo: an unloading page PUTs with If-Match; an unmount runs the save
       loop (or, mid-flight, it sends it next).
     An unanswered shrink question means the user has not agreed to write
     this text, so it is not written on the way out either. */
  let exitSent: string | null = null;
  // A request that failed releases its key, so the state can be offered again.
  const exitFailed = (key: string) => () => {
    if (exitSent === key) exitSent = null;
  };
  const flushExit = (text: string, unloading: boolean) => {
    if (asking) return;
    if (hubHoldsIt) {
      const offline = !live && pending > 0;
      if (!offline && !(live && localDirtyInRoom)) return;
      const key = "room\u0000" + collab.text.toString();
      if (exitSent === key) return;
      exitSent = key;
      const sentRoomSeq = localRoomSeq;
      const req = offline
        ? flushOffline(true)
        : syncCollab(syncURL, { update: collab.encodeState() }, true).then((out) => rebase(out.sha));
      req.then(() => roomHandedOver(sentRoomSeq), exitFailed(key));
      return;
    }
    if (pendingJoin || stagingConflict) {
      // A blank buffer is never parked beside a file (the hub agrees:
      // upload.go refuses a whitespace-only conflict copy).
      if (!text.trim()) return;
      const key = "park\u0000" + text;
      if (exitSent === key) return;
      exitSent = key;
      // The same words as a park already named reuse its name (the same
      // bytes again are answered unchanged); newer words get a freshly
      // minted one, so no older park still on the wire can land over them.
      let copy = parkedName !== null && parkedText === text ? parkedName : null;
      for (const [path, held] of conflictPuts) if (copy === null && held === text) copy = path;
      copy ??= mintConflictPath();
      parkOnExit(copy, text, key);
      return;
    }
    // An emptied file is an edit like any other: only "nothing changed" is
    // nothing to send.
    if (text === saved && !roomNeedsFlush) return;
    const key = "text\u0000" + text;
    if (exitSent === key) return;
    if ((solo || noRoomHttp) && !unloading) {
      void save();
      return;
    }
    exitSent = key;
    const from = saved;
    const staging = !(solo || noRoomHttp);
    /* A staging checkpoint is recorded as ambiguous until answered, exactly
       as sendRoom records one: a page restored from the bfcache with this
       request unsettled (or failed after it reached the room) names it in
       `prior_sent`, and a join peeks, so it is never applied twice. */
    const before = ambiguousRoomCheckpoint;
    const mine: Ambiguous = { sent: text, from, prev: before };
    if (staging) ambiguousRoomCheckpoint = mine;
    /* The page is going and will not get to send a second request, so this
       one asks the hub to park a clash beside the file itself and answers
       with the copy (`parked`). An older hub refuses 409 instead: if its head
       already holds our edit there is nothing to keep; otherwise the words
       are parked from here, as long as the page lives to send it. */
    const onExitError = (e: unknown) => {
      // Refused before the room was touched: the checkpoint is not ambiguous.
      if (
        staging &&
        e instanceof HttpError &&
        [400, 401, 403, 404, 405, 409, 413].includes(e.status) &&
        ambiguousRoomCheckpoint === mine
      ) {
        ambiguousRoomCheckpoint = before;
      }
      // A page that lives on (bfcache, or no unload at all) asks the user,
      // as a save would have.
      if (e instanceof HttpError && e.status === 422 && isShrinkRefusal(e)) {
        exitFailed(key)();
        if (!gone) {
          clearTimeout(shrinkTimer ?? undefined);
          shrinkTimer = null;
          askShrink(current());
        }
        return;
      }
      if (!(e instanceof HttpError && e.status === 409)) return exitFailed(key)();
      let theirs: { text?: unknown; sha?: unknown } = {};
      try {
        const parsed: unknown = JSON.parse(e.body);
        if (parsed && typeof parsed === "object") theirs = parsed as typeof theirs;
      } catch {
        // an older hub, or a body we cannot read: park
      }
      if (typeof theirs.text === "string" && (theirs.text === text || retainsChanges(from, text, theirs.text))) {
        // Already in the hub's text: a page that lives on is built on it now.
        if (!gone && !hubHoldsIt) {
          const res = mergeForPath(text, theirs.text, current());
          if (res.ok && applyText(res.text)) {
            saved = theirs.text;
            savedSha = undefined;
            if (typeof theirs.sha === "string") rebase(theirs.sha);
          }
        }
        return;
      }
      if (!text.trim()) return; // a blank buffer is never parked
      stagingConflict = true;
      let copy = parkedName !== null && parkedText === text ? parkedName : null;
      for (const [path, held] of conflictPuts) if (copy === null && held === text) copy = path;
      copy ??= mintConflictPath();
      parkedName = copy;
      parkedText = text;
      parkOnExit(copy, text, key, (path) => {
        stagingConflict = false;
        opts.onConflictCopy?.(path);
      });
    };
    /* Answered: what landed is what a page restored from the bfcache builds
       on, so its next save diffs against it rather than against `from`. */
    const parkedHere = (parked: string, sha?: string) => {
      saved = text;
      savedSha = undefined;
      rebase(sha);
      if (!gone && current() === text) setState("clean");
      opts.onConflictCopy?.(parked);
    };
    let req: Promise<unknown>;
    if (!staging) {
      req = putText(contentURL(opts.path), text, base, confirmShrinkOnce, true, true).then((out) => {
        confirmShrinkOnce = false;
        if (out.parked) return parkedHere(out.parked, out.sha);
        if (out.merged) {
          // The hub folded it into a newer file: learn what the file is.
          if (!gone) void rebaseOnHead(text, true);
          else rebase(out.sha);
          return;
        }
        adopt(text, out.sha);
        if (!gone && current() === saved) setState("clean");
        opts.onSaved?.(text);
      }, onExitError);
    } else {
      req = syncCollab(syncURL, { ...roomPayload(text, before), park_on_conflict: true }, true).then((out) => {
        confirmShrinkOnce = false;
        if (ambiguousRoomCheckpoint === mine) ambiguousRoomCheckpoint = null;
        roomNeedsFlush = false;
        if (out.parked) return parkedHere(out.parked, out.sha);
        if (out.update) collab.applyRemoteUpdate(out.update);
        if (gone || hubHoldsIt || typeof out.text !== "string") {
          rebase(out.sha);
          return;
        }
        const res = mergeForPath(text, out.text, current());
        if (res.ok && applyText(res.text)) {
          adopt(out.text, out.sha);
          if (current() === saved) setState("clean");
        }
        opts.onSaved?.(out.text);
      }, onExitError);
    }
    // Settled either way: what waits on it only needs to know it is over.
    const settled: Promise<void> = req.then(
      () => {},
      () => {},
    );
    exitSaving = settled;
    void settled.then(() => {
      if (exitSaving === settled) exitSaving = null;
    });
  };
  const onPageHide = () => {
    if (!gone) flushExit(current(), true);
  };
  // Back from the back/forward cache: the page lives on, and whatever it
  // sent on the way "out" is no reason to send nothing on the next exit.
  const onPageShow = () => {
    exitSent = null;
  };
  if (typeof window !== "undefined") {
    window.addEventListener("pagehide", onPageHide);
    window.addEventListener("pageshow", onPageShow);
  }

  return {
    collab,
    current,
    merge,
    rebase,
    saveNow,
    askingShrink: () => asking !== null,
    destroy() {
      if (typeof window !== "undefined") {
        window.removeEventListener("pagehide", onPageHide);
        window.removeEventListener("pageshow", onPageShow);
      }
      clearTimeout(timer ?? undefined);
      clearTimeout(shrinkTimer ?? undefined);
      cancelRetry();
      clearTimeout(joinTimer ?? undefined);
      joinTimer = null;
      const text = current();
      gone = true;
      finalText = text;
      if (asking) opts.onShrinkWithdrawn?.(asking);
      else flushExit(text, false);
      clearTimeout(offlineTimer ?? undefined);
      collab.awareness.off("change", onPeerChange);
      collab.text.unobserve(onDocChange);
      collab.destroy();
    },
  };
}
