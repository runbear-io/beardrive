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
   From the iframe (the port), once: bd-edit:exit with every edited span. */

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
};
type Sync = { type: "sync"; savedText?: string; baseSha?: string; hasBaseSha?: boolean; docText?: string };
type FromApp = Arm | Sync | { type: "disarm" };

let state: ExitWorkerState | null = null;
let childPort: MessagePort | null = null;
let disarmed = false;
let exited = false;

const onExit = (ev: MessageEvent) => {
  const msg = ev.data as { type?: unknown; patches?: unknown } | null;
  if (!msg || msg.type !== "bd-edit:exit" || !Array.isArray(msg.patches)) return;
  if (disarmed || exited || !state) return;
  exited = true;
  childPort?.close();
  childPort = null;
  // Shapes are checked spliced (applyRenderPatches): a bad one sends nothing.
  const req = buildExitSaveRequest(state, msg.patches as ExitPatch[]);
  if (!req) return;
  void fetch(req.url, {
    method: req.method,
    credentials: "same-origin",
    headers: req.headers,
    body: req.body,
    keepalive: req.keepalive,
  }).catch(() => {});
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
    // A re-arm (a new render) replaces the iframe's channel; nothing sent yet.
    childPort?.close();
    childPort = ev.ports[0] ?? null;
    exited = false;
    if (childPort) childPort.onmessage = onExit;
    return;
  }
  if (msg.type === "sync") {
    if (!state) return;
    if (typeof msg.savedText === "string") state.savedText = msg.savedText;
    if (msg.hasBaseSha) state.baseSha = msg.baseSha;
    if (typeof msg.docText === "string") state.docText = msg.docText;
    return;
  }
  if (msg.type === "disarm") {
    disarmed = true;
    state = null;
    childPort?.close();
    childPort = null;
    self.close();
  }
};
