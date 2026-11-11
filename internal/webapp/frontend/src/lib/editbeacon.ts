/* The editing iframe's last word, when the app may never hear it.

   An edit reaches the app as a postMessage, and the app acknowledges each
   patch once it is in the document. A tab closed between the keystroke and
   that acknowledgement leaves an edit only the iframe holds — and the iframe
   is sandboxed without allow-same-origin and runs the synced file's own
   scripts, so it must never hold a session or any reusable write credential.

   So the save is made somewhere else: a dedicated worker the app starts on
   its own origin (editexit.worker.ts), which outlives the closing tab long
   enough to send one request with the app's own cookie. The iframe holds only
   a MessagePort to it, and on the way out posts the spans it edited. The
   worker trusts none of that: it splices them onto the render it was armed
   with, only at ranges the app anchored, refuses script and sharp shrinks,
   and merges the result against what the app last knew the file held.

   Pure, so it is tested without a DOM or a worker. */

import { conflictName } from "./conflict";
import { hasCollabRoute } from "./collabroute";
import { merge3ForPath } from "./merge3";
import { byteLength, sharpShrink } from "./meeting";

export type EditedSpan = {
  start: number;
  end: number;
  html: string;
  seq: number;
  acked?: boolean;
  ackedHtml?: string;
  history?: Map<number, string>;
};

/* Marks the span edit `seq` acknowledged, recording the HTML that `seq`
   carried (`ackedHtml`) as the ancestor already present in the app's document.
   A later edit to a different span never clears an earlier unapplied span, and
   an older ack arriving after a newer edit to the same span advances `ackedHtml`
   to what landed while keeping the newer edit unacknowledged. */
export function ackEditedSpan(spans: Iterable<EditedSpan>, seq: number): void {
  for (const s of spans) {
    const h = s.history?.get(seq);
    if (h !== undefined) {
      s.ackedHtml = h;
      if (s.seq === seq) s.acked = true;
    } else if (s.seq === seq) {
      s.ackedHtml = s.html;
      s.acked = true;
    }
  }
}

export type ExitPatch = { start: number; end: number; html: string; ackedHtml?: string };

/* What the frame posts on the way out, or null when nothing is owed (every
   span acknowledged, or nothing edited). Every span edited on this render
   goes once any is unacknowledged, carrying `ackedHtml` for spans the app
   already applied so the worker merges from the acknowledged document rather
   than re-merging an edit that already landed. */
export function unackedExitPatches(spans: Iterable<EditedSpan>): ExitPatch[] | null {
  const all = [...spans];
  if (!all.some((s) => !s.acked)) return null;
  return all
    .sort((a, b) => a.start - b.start)
    .map(({ start, end, html, ackedHtml }) => ({
      start,
      end,
      html,
      ...(ackedHtml !== undefined ? { ackedHtml } : {}),
    }));
}

// What the worker was armed with, kept current by the app as it saves.
export type ExitWorkerState = {
  apiBase: string;
  path: string;
  who?: string;
  // The source the frame rendered, which every patch range indexes.
  renderText: string;
  renderLen: number;
  // The ranges the app anchored ("start,end"): the only ones a patch may name.
  allowedRanges: ReadonlySet<string>;
  // The version the file is known to hold, and its sha.
  savedText: string;
  baseSha?: string;
  // The app's document as it stands (staging buffer or shared document).
  docText: string;
};

// Far more than one render has elements; a body claiming more is not one.
const MAX_EXIT_PATCHES = 256;
// The hub's largest seed (maxSeedBytes): nothing bigger is spliced here.
const MAX_EXIT_BYTES = 1 << 20;
// Browsers cap the keepalive bodies in flight; past it, an ordinary request.
const KEEPALIVE_MAX_BYTES = 60_000;

const inSurrogatePair = (s: string, i: number) =>
  i > 0 &&
  i < s.length &&
  s.charCodeAt(i) >= 0xdc00 &&
  s.charCodeAt(i) <= 0xdfff &&
  s.charCodeAt(i - 1) >= 0xd800 &&
  s.charCodeAt(i - 1) <= 0xdbff;

/* The render with the frame's patches spliced in, or null if any patch is
   not one the app could have received: a range it did not anchor, overlapping
   or out of order, splitting a character, carrying a script or a NUL — the
   frame runs the page's own scripts, so its word is checked, not taken. */
export function applyRenderPatches(
  renderText: string,
  renderLen: number,
  allowedRanges: ReadonlySet<string>,
  patches: ReadonlyArray<ExitPatch>,
): string | null {
  if (renderText.length !== renderLen) return null;
  if (patches.length < 1 || patches.length > MAX_EXIT_PATCHES) return null;
  let want = "";
  let prev = 0;
  for (const { start, end, html } of patches) {
    if (!Number.isInteger(start) || !Number.isInteger(end)) return null;
    if (start <= 0 || start > end || end >= renderLen || start < prev) return null;
    if (!allowedRanges.has(start + "," + end)) return null;
    if (inSurrogatePair(renderText, start) || inSurrogatePair(renderText, end)) return null;
    if (typeof html !== "string" || html.includes("\0") || /<\s*script\b/i.test(html)) return null;
    want += renderText.slice(prev, start) + html;
    prev = end;
  }
  want += renderText.slice(prev);
  return byteLength(want) < MAX_EXIT_BYTES ? want : null;
}

export type ExitSaveRequest = {
  url: string;
  method: "POST" | "PUT";
  headers: Record<string, string>;
  body: string;
  keepalive: boolean;
};

/* The one request the worker sends, or null when there is nothing to send
   or nothing it may: patches it refuses, an unconfirmed sharp shrink, or a
   result the file already holds.

   The frame's edits are on the RENDER; the file may have moved since (the
   app's own autosaves, a co-editor). They are merged three ways onto what
   the app last knew the document held. Merged: the result goes as an
   ordinary save from `savedText`/`baseSha`. A same-word clash: the frame's
   own document goes beside the file, as every conflict does.

   In a project (a room): a checkpoint asking the hub to park a clash itself.
   In a plain folder (no room): a conditional PUT on the latest base — the
   render's version is gone from a folder once an autosave replaced it, so
   the base has to move with the saves — or, for a clash, a create-only
   conflict copy. */
export function buildExitSaveRequest(
  state: ExitWorkerState,
  patches: ReadonlyArray<ExitPatch>,
): ExitSaveRequest | null {
  const renderedWant = applyRenderPatches(state.renderText, state.renderLen, state.allowedRanges, patches);
  if (renderedWant === null) return null;
  const ackedPatches = patches
    .filter((p) => typeof p.ackedHtml === "string")
    .map((p) => ({ start: p.start, end: p.end, html: p.ackedHtml as string }));
  const ancestor =
    ackedPatches.length > 0
      ? applyRenderPatches(state.renderText, state.renderLen, state.allowedRanges, ackedPatches)
      : state.renderText;
  if (ancestor === null) return null;
  const latest = state.docText || state.savedText;
  let mergedOk = true;
  let want = renderedWant;
  if (latest !== ancestor && latest !== renderedWant) {
    const m = merge3ForPath(state.path, ancestor, latest, renderedWant);
    if (m.ok) want = m.text;
    else mergedOk = false;
  }
  if (state.savedText.length > 0 && sharpShrink(byteLength(state.savedText), byteLength(want))) return null;
  if (mergedOk && want === state.savedText) return null;

  if (hasCollabRoute(state.apiBase)) {
    const payload = mergedOk
      ? { base: state.baseSha, base_text: state.savedText, text: want, park_on_conflict: true }
      : { base_text: state.renderText, text: renderedWant, park_on_conflict: true };
    const body = JSON.stringify(payload);
    return {
      url: state.apiBase + "ycollab/sync?path=" + encodeURIComponent(state.path),
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body,
      keepalive: byteLength(body) <= KEEPALIVE_MAX_BYTES,
    };
  }

  const text = "text/plain; charset=utf-8";
  if (mergedOk) {
    const headers: Record<string, string> = {
      "Content-Type": text,
      "X-Bdrive-Park-On-Conflict": "1",
    };
    if (state.baseSha) headers["If-Match"] = `"${state.baseSha}"`;
    return {
      url: state.apiBase + "upload/content?path=" + encodeURIComponent(state.path),
      method: "PUT",
      headers,
      body: want,
      keepalive: byteLength(want) <= KEEPALIVE_MAX_BYTES,
    };
  }
  return {
    url:
      state.apiBase +
      "upload/content?path=" +
      encodeURIComponent(conflictName(state.path, state.who || "browser", new Date())),
    method: "PUT",
    headers: { "Content-Type": text, "If-None-Match": "*", "X-Bdrive-Park-On-Conflict": "1" },
    body: renderedWant,
    keepalive: byteLength(renderedWant) <= KEEPALIVE_MAX_BYTES,
  };
}
