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
     sync    — what the file holds / the document now, as the app saves
     disarm  — the editor reloaded or closed normally: nothing more to do
     rearm   — the page came back from the bfcache after an exit: a fresh
               port from the frame, and what the file holds now
   From the iframe (the port), once per arming: bd-edit:exit with every
   edited span. */

import { buildExitSaveRequest, type ExitPatch, type ExitWorkerState } from "./editbeacon";

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
type FromApp =
  | Arm
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
let pendingPatches: ExitPatch[] | null = null;
let pendingTimer: ReturnType<typeof setTimeout> | undefined;
let workerSaved: { fromText: string; text: string; sha?: string } | null = null;

/* A frame script cannot trigger a write on its own: both user interaction in
   the frame (`interacted`) and a real parent unload (`parentUnloading`) must
   be attested by VisualEdit on the trusted parent channel before any message
   from `childPort` can produce a network request. */
const tryFlush = () => {
  if (disarmed || exited || !state || !interacted || !parentUnloading || !pendingPatches) return;
  const patches = pendingPatches;
  pendingPatches = null;
  clearTimeout(pendingTimer);
  exited = true;
  childPort?.close();
  childPort = null;
  const curState = state;
  const req = buildExitSaveRequest(curState, patches);
  if (!req) return;
  const fromText = curState.savedText;
  void fetch(req.url, {
    method: req.method,
    credentials: "same-origin",
    headers: req.headers,
    body: req.body,
    keepalive: req.keepalive,
  })
    .then(async (res) => {
      if (!res.ok) return;
      const out = (await res.json().catch(() => null)) as {
        ok?: boolean;
        sha?: string;
        text?: string;
        parked?: string;
      } | null;
      if (!out || out.parked) return;
      const landedText = typeof out.text === "string" ? out.text : req.method === "PUT" ? req.body : null;
      if (landedText === null) return;
      workerSaved = { fromText, text: landedText, sha: out.sha };
      if (state === curState && state.savedText === fromText) {
        if (state.docText === state.savedText) state.docText = landedText;
        state.savedText = landedText;
        if (typeof out.sha === "string" && out.sha) state.baseSha = out.sha;
      }
    })
    .catch(() => {});
};
const onExit = (ev: MessageEvent) => {
  const msg = ev.data as { type?: unknown; patches?: unknown } | null;
  if (!msg || msg.type !== "bd-edit:exit" || !Array.isArray(msg.patches)) return;
  if (disarmed || exited || !state) return;
  pendingPatches = msg.patches as ExitPatch[];
  clearTimeout(pendingTimer);
  if (interacted && parentUnloading) {
    tryFlush();
    return;
  }
  // Without a parent unload alongside it, discard shortly so a forged message
  // sent while viewing cannot linger until a later close.
  pendingTimer = setTimeout(() => {
    pendingPatches = null;
  }, 500);
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
    workerSaved = null;
    parentUnloading = false;
    pendingPatches = null;
    clearTimeout(pendingTimer);
    childPort?.close();
    childPort = ev.ports[0] ?? null;
    exited = false;
    if (childPort) childPort.onmessage = onExit;
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
    pendingPatches = null;
    clearTimeout(pendingTimer);
    childPort?.close();
    childPort = ev.ports[0] ?? null;
    if (childPort) childPort.onmessage = onExit;
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
    pendingPatches = null;
    clearTimeout(pendingTimer);
    childPort?.close();
    childPort = null;
    self.close();
  }
};
