/* The visual editor's exit save, in a dedicated worker on the app's origin.

   A tab closed before the editing iframe's last patch reached the app leaves
   that edit only in the iframe — which runs the synced file's own scripts,
   so it is never given a session or any credential (lib/editbeacon.ts). This
   worker is started by VisualEdit on the app's origin, outlives the closing
   tab long enough for one request, and sends it with the app's own cookie.
   The iframe holds nothing but a MessagePort to it.

   Messages from the app:
     arm     — the render, its anchored ranges, what the file holds; the
               transferred port is the iframe's end of the channel
     ack     — the app applied patch `seq` into its document
     sync    — what the file holds / the document now, as the app saves
     disarm  — the editor reloaded or closed normally: nothing more to do
     rearm   — the page came back from the bfcache after an exit: a fresh
               port from the frame, and what the file holds now
   From the iframe (the port): `bd-edit:span` as each edit happens, and
   `bd-edit:exit` once on unload. All span/ack state lives in this worker's
   isolated JS realm rather than in the iframe. */

import { hasCollabRoute } from "./collabroute";
import {
  ackEditedSpan,
  buildExitSaveRequest,
  unackedExitPatches,
  type EditedSpan,
  type ExitWorkerState,
} from "./editbeacon";
import { retainsChanges } from "./merge3";
type Arm = {
  type: "arm";
  apiBase: string;
  path: string;
  who?: string;
  renderText: string;
  renderLen: number;
  allowedRanges: string[];
  savedText: string;
  baseSha?: string;
  docText: string;
  interacted?: boolean;
};
type Sync = { type: "sync"; savedText?: string; baseSha?: string; hasBaseSha?: boolean; docText?: string };
type Rearm = {
  type: "rearm";
  savedText: string;
  baseSha?: string;
  hasBaseSha?: boolean;
  docText: string;
  interacted?: boolean;
};
type Ack = { type: "ack"; seq: number };
type FromApp =
  | Arm
  | Ack
  | Sync
  | Rearm
  | { type: "interacted" }
  | { type: "parent-unload" }
  | { type: "disarm" };

let state: ExitWorkerState | null = null;
let childPort: MessagePort | null = null;
let disarmed = false;
let exited = false;
let interacted = false;
let parentUnloading = false;
let childExited = false;
let pendingTimer: ReturnType<typeof setTimeout> | undefined;
let retryTimer: ReturnType<typeof setTimeout> | undefined;
let workerSaved: { fromText: string; text: string; sha?: string } | null = null;
let ambiguousAttempt: { fromText: string; sentText: string; seqs: number[] } | null = null;
const priorSentTexts: string[] = [];
const editedSpans = new Map<string, EditedSpan>();
const ackedSeqs = new Set<number>();

const confirmSeqs = (seqs: ReadonlyArray<number>) => {
  for (const seq of seqs) {
    ackedSeqs.add(seq);
    ackEditedSpan(editedSpans.values(), seq);
  }
};

const recordAmbiguous = (curState: ExitWorkerState, fromText: string, sentText: string, seqs: number[]) => {
  ambiguousAttempt = { fromText, sentText, seqs };
  if (!priorSentTexts.includes(sentText)) {
    priorSentTexts.unshift(sentText);
    if (priorSentTexts.length > 4) priorSentTexts.length = 4;
  }
  curState.priorSent = priorSentTexts;
};

const scheduleRearmRetry = () => {
  clearTimeout(retryTimer);
  if (disarmed || exited || !state || !unackedExitPatches(editedSpans.values())) return;
  retryTimer = setTimeout(() => {
    retryTimer = undefined;
    if (disarmed || exited || !state) return;
    sendUnacked(false);
  }, 1_500);
};

const reconcileAfterRearm = () => {
  if (disarmed || exited || !state || !unackedExitPatches(editedSpans.values())) return;
  const curState = state;
  const amb = ambiguousAttempt;
  if (amb && hasCollabRoute(curState.apiBase)) {
    void fetch(curState.apiBase + "ycollab/sync?path=" + encodeURIComponent(curState.path), {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ peek: true }),
    })
      .then(async (res) => {
        if (!res.ok || disarmed || state !== curState) {
          scheduleRearmRetry();
          return;
        }
        const peek = (await res.json().catch(() => null)) as { ok?: boolean; sha?: string; text?: string } | null;
        if (peek?.ok && typeof peek.text === "string" && retainsChanges(amb.fromText, amb.sentText, peek.text)) {
          // A peek proves the live room holds the edit, NOT that storage does:
          // advance `ackedHtml` + `docText` so a retry merges from the room's
          // text without duplicating the insertion, while keeping `s.acked`
          // false and `priorSentTexts` intact until a real sync answers 200 OK.
          for (const s of editedSpans.values()) {
            for (const seq of amb.seqs) {
              const h = s.history?.get(seq);
              if (h !== undefined) s.ackedHtml = h;
            }
          }
          if (curState.docText === amb.fromText) curState.docText = peek.text;
          ambiguousAttempt = null;
          scheduleRearmRetry();
          return;
        }
        ambiguousAttempt = null;
        scheduleRearmRetry();
      })
      .catch(() => {
        scheduleRearmRetry();
      });
    return;
  }
  scheduleRearmRetry();
};

const sendUnacked = (asUnload: boolean) => {
  if (disarmed || !state) return;
  const patches = unackedExitPatches(editedSpans.values());
  if (!patches) return;
  const sentSeqs: number[] = [];
  for (const s of editedSpans.values()) sentSeqs.push(s.seq);
  const curState = state;
  curState.priorSent = priorSentTexts.length > 0 ? priorSentTexts : undefined;
  const req = buildExitSaveRequest(curState, patches);
  if (!req) return;
  const fromText = curState.savedText;
  void fetch(req.url, {
    method: req.method,
    credentials: "same-origin",
    headers: req.headers,
    body: req.body,
    keepalive: asUnload && req.keepalive,
  })
    .then(async (res) => {
      if (!res.ok) {
        // A 5xx (notably 503 when syncDurable failed after Apply) may already
        // have spliced the edit into the live room: record it as ambiguous so
        // rearm peeks first and a retry carries prior_sent.
        if (res.status >= 500) {
          recordAmbiguous(curState, fromText, req.sentText, sentSeqs);
          if (!exited && !disarmed && state === curState) reconcileAfterRearm();
        } else if (res.status === 429 && !exited && !disarmed && state === curState) {
          scheduleRearmRetry();
        }
        return;
      }
      const out = (await res.json().catch(() => null)) as {
        ok?: boolean;
        sha?: string;
        text?: string;
        parked?: string;
      } | null;
      if (!out || out.ok !== true) {
        recordAmbiguous(curState, fromText, req.sentText, sentSeqs);
        if (!exited && !disarmed && state === curState) reconcileAfterRearm();
        return;
      }
      confirmSeqs(sentSeqs);
      ambiguousAttempt = null;
      priorSentTexts.length = 0;
      curState.priorSent = undefined;
      if (out.parked) {
        if (state === curState && typeof out.sha === "string" && out.sha) state.baseSha = out.sha;
        return;
      }
      const landedText = typeof out.text === "string" ? out.text : req.method === "PUT" ? req.body : null;
      if (landedText === null) return;
      workerSaved = { fromText, text: landedText, sha: out.sha };
      if (state === curState && state.savedText === fromText) {
        if (state.docText === state.savedText) state.docText = landedText;
        state.savedText = landedText;
        if (typeof out.sha === "string" && out.sha) state.baseSha = out.sha;
      }
    })
    .catch(() => {
      recordAmbiguous(curState, fromText, req.sentText, sentSeqs);
      if (!exited && !disarmed && state === curState) reconcileAfterRearm();
    });
};

/* A frame script cannot trigger a write on its own: both user interaction in
   the frame (`interacted`) and a real parent unload (`parentUnloading`) must
   be attested by VisualEdit on the trusted parent channel before `bd-edit:exit`
   from `childPort` can produce a network request. */
const tryFlush = () => {
  if (disarmed || exited || !state || !interacted || !parentUnloading || !childExited) return;
  childExited = false;
  clearTimeout(pendingTimer);
  clearTimeout(retryTimer);
  exited = true;
  childPort?.close();
  childPort = null;
  sendUnacked(true);
};
const onChildMessage = (ev: MessageEvent) => {
  const msg = ev.data as {
    type?: unknown;
    seq?: unknown;
    start?: unknown;
    end?: unknown;
    html?: unknown;
  } | null;
  if (!msg || typeof msg !== "object" || disarmed || exited || !state) return;
  if (
    msg.type === "bd-edit:span" &&
    typeof msg.seq === "number" &&
    typeof msg.start === "number" &&
    typeof msg.end === "number" &&
    typeof msg.html === "string"
  ) {
    const k = msg.start + "," + msg.end;
    if (!state.allowedRanges.has(k)) return;
    const prev = editedSpans.get(k);
    const history = prev?.history ?? new Map<number, string>();
    history.set(msg.seq, msg.html);
    let ackedHtml = prev?.ackedHtml;
    let maxAcked = -1;
    for (const [hSeq, hHtml] of history) {
      if (ackedSeqs.has(hSeq) && hSeq > maxAcked) {
        maxAcked = hSeq;
        ackedHtml = hHtml;
      }
    }
    const latestSeq = prev && prev.seq > msg.seq ? prev.seq : msg.seq;
    const latestHtml = prev && prev.seq > msg.seq ? prev.html : msg.html;
    editedSpans.set(k, {
      start: msg.start,
      end: msg.end,
      html: latestHtml,
      seq: latestSeq,
      acked: ackedSeqs.has(latestSeq),
      ackedHtml,
      history,
    });
    return;
  }
  if (msg.type === "bd-edit:exit") {
    childExited = true;
    clearTimeout(pendingTimer);
    if (interacted && parentUnloading) {
      tryFlush();
      return;
    }
    pendingTimer = setTimeout(() => {
      childExited = false;
    }, 500);
  }
};

self.onmessage = (ev: MessageEvent) => {
  const msg = ev.data as FromApp | null;
  if (!msg || typeof msg !== "object" || disarmed) return;
  if (msg.type === "arm") {
    state = {
      apiBase: msg.apiBase,
      path: msg.path,
      who: msg.who,
      renderText: msg.renderText,
      renderLen: msg.renderLen,
      allowedRanges: new Set(msg.allowedRanges),
      savedText: msg.savedText,
      baseSha: msg.baseSha,
      docText: msg.docText,
    };
    if (msg.interacted) interacted = true;
    editedSpans.clear();
    ackedSeqs.clear();
    ambiguousAttempt = null;
    priorSentTexts.length = 0;
    workerSaved = null;
    parentUnloading = false;
    childExited = false;
    clearTimeout(pendingTimer);
    clearTimeout(retryTimer);
    childPort?.close();
    childPort = ev.ports[0] ?? null;
    exited = false;
    if (childPort) childPort.onmessage = onChildMessage;
    return;
  }
  /* Back from the bfcache: the first exit already went (and closed its
     port), but the page lives on and may be closed again with a new edit the
     app has not heard. Same render, so the ranges stand; if the app itself
     never learned of the worker's own exit save (frozen in bfcache), keep the
     worker's landed version rather than rolling `savedText` back to before it. */
  if (msg.type === "rearm") {
    if (!state) return;
    if (workerSaved && msg.savedText === workerSaved.fromText) {
      state.savedText = workerSaved.text;
      if (workerSaved.sha) state.baseSha = workerSaved.sha;
      state.docText = msg.docText === workerSaved.fromText ? workerSaved.text : msg.docText;
    } else {
      workerSaved = null;
      state.savedText = msg.savedText;
      if (msg.hasBaseSha) state.baseSha = msg.baseSha;
      state.docText = msg.docText;
    }
    if (msg.interacted) interacted = true;
    exited = false;
    parentUnloading = false;
    childExited = false;
    clearTimeout(pendingTimer);
    clearTimeout(retryTimer);
    childPort?.close();
    childPort = ev.ports[0] ?? null;
    if (childPort) childPort.onmessage = onChildMessage;
    reconcileAfterRearm();
    return;
  }
  if (msg.type === "ack") {
    if (typeof msg.seq === "number") {
      ackedSeqs.add(msg.seq);
      ackEditedSpan(editedSpans.values(), msg.seq);
    }
    return;
  }
  if (msg.type === "interacted") {
    interacted = true;
    tryFlush();
    return;
  }
  if (msg.type === "parent-unload") {
    parentUnloading = true;
    tryFlush();
    return;
  }
  if (msg.type === "sync") {
    if (!state) return;
    if (typeof msg.savedText === "string") {
      if (!workerSaved || msg.savedText !== workerSaved.fromText) {
        workerSaved = null;
        state.savedText = msg.savedText;
        if (msg.hasBaseSha) state.baseSha = msg.baseSha;
      }
    } else if (msg.hasBaseSha && !workerSaved) {
      state.baseSha = msg.baseSha;
    }
    if (typeof msg.docText === "string") {
      state.docText = workerSaved && msg.docText === workerSaved.fromText ? workerSaved.text : msg.docText;
    }
    return;
  }
  if (msg.type === "disarm") {
    disarmed = true;
    state = null;
    editedSpans.clear();
    ackedSeqs.clear();
    ambiguousAttempt = null;
    priorSentTexts.length = 0;
    childExited = false;
    clearTimeout(pendingTimer);
    clearTimeout(retryTimer);
    childPort?.close();
    childPort = null;
    self.close();
  }
};
