// Fetch helpers shared by every API hook. All URLs are root-absolute: a
// deep path like /<project>/<dir>/<file> must never break relative
// resolution.

import { track } from "../analytics";

// Product events, derived from the write that just succeeded rather than
// hand-fired at each button. Every mutating call in the app goes through
// api() or postJSON(), so this table is the whole instrumentation surface —
// a new write shows up here or nowhere, instead of silently going unmeasured
// because someone forgot a capture() call. The one write that bypasses these
// helpers (share creation, a raw fetch in Browser.tsx) fires its own.
//
// Nothing here carries a path, project name, email or token: the event says
// what kind of thing happened, never to which customer object.
//
// Two writes are deliberately absent. POST .../reads is read telemetry, not a
// user action — counting it would mean the analytics of our analytics. The
// /store/* routes are device replication, which is a sync cycle rather than
// anything a person did.
const PRODUCT_EVENTS: [RegExp, string][] = [
  [/^POST \/api\/projects$/, "project_created"],
  [/^DELETE \/api\/projects\//, "project_deleted"],
  [/^POST \/api\/p\/[^/]+\/restore$/, "file_restored"],
  [/^DELETE \/api\/shares\//, "share_revoked"],
  [/^PATCH \/api\/shares\//, "share_expiry_changed"],
  [/^POST \/api\/orgs\/[^/]+\/invites$/, "invite_created"],
  [/^DELETE \/api\/orgs\/[^/]+\/invites\//, "invite_revoked"],
  [/^POST \/api\/invites\//, "invite_accepted"],
  [/^PUT \/api\/p\/[^/]+\/permissions\/./, "project_access_granted"],
  [/^DELETE \/api\/p\/[^/]+\/permissions\/./, "project_access_revoked"],
  [/^POST \/api\/p\/[^/]+\/comments$/, "comment_created"],
  [/^POST \/api\/p\/[^/]+\/comments\/[^/]+$/, "comment_updated"],
  [/^DELETE \/api\/p\/[^/]+\/comments\/[^/]+$/, "comment_deleted"],
];

function trackWrite(method: string, url: string) {
  // The query string can hold a path; match on the path alone.
  const key = method + " " + url.split("?")[0];
  const hit = PRODUCT_EVENTS.find(([re]) => re.test(key));
  if (hit) track(hit[1]);
}

/* Bouncing to the login page is a FULL PAGE LOAD, so it must never be
   something the app can do twice in a row.

   It did, on 2026-09-22. The hub's database ran out of connections, session
   lookups started failing intermittently, and a signed-in user got a 401.
   This function sent the browser to /auth/login, which saw a perfectly valid
   session and sent it straight back — and the reload issued the same requests
   again, against the pool whose exhaustion caused the 401 in the first place.
   The page reloaded about once a second, indefinitely, and every lap made the
   underlying problem worse.

   The redirect is still right for someone who is genuinely signed out. What
   is wrong is doing it again after it has just failed to help. So: one bounce
   per window, recorded where a page load can see it, and the second time we
   surface the error instead. A visible failure is recoverable; a reload loop
   is not — the user cannot even open devtools on a page that keeps dying. */
const BOUNCE_KEY = "bdrive:login-bounce";
const BOUNCE_WINDOW_MS = 30_000;

/* There is deliberately no "auth worked, clear the marker" call.

   The first version of this had one, called when /api/config came back with
   a user — and it defeated the whole guard, because in the loop /api/config
   SUCCEEDS. Only the project calls were 401ing. So every lap cleared the
   marker that was supposed to stop the next lap, and the e2e caught it:
   234 navigations in twenty seconds with the guard supposedly in place.

   The marker therefore just expires. The cost is narrow and acceptable: a
   second genuine session expiry within the window shows an error instead of
   redirecting, and the user reloads. The alternative cost is a page that
   reloads forever, which a user cannot even open devtools on. */

function toLogin(): never {
  let bounced = 0;
  try {
    bounced = Number(sessionStorage.getItem(BOUNCE_KEY) || 0);
  } catch {
    /* no storage: fall through and redirect, as before */
  }
  if (bounced && Date.now() - bounced < BOUNCE_WINDOW_MS) {
    throw new HttpError(
      401,
      "The server rejected your session, and signing in again did not help. " +
        "It is probably having a bad moment — wait a few seconds and reload.",
    );
  }
  try {
    sessionStorage.setItem(BOUNCE_KEY, String(Date.now()));
  } catch {
    /* as above */
  }
  // Auth required: sign in, then come back to the current route.
  location.href =
    "/auth/login?next=" +
    encodeURIComponent(location.pathname + location.search);
  throw new Error("signing in…");
}

// Server messages are written for operators and CLIs — lowercase, unpunctuated,
// occasionally naming internals ("forbidden: seat limit reached for plan free").
// They surface verbatim in toasts, so the ones we can predict get product copy
// and everything else falls back to the server's own words, which is still
// better than a generic apology when the cause is specific.
function errorFor(status: number, body: string): string {
  const raw = body.trim();
  switch (status) {
    case 403:
      if (raw.includes("seat"))
        return "This plan is out of seats. Upgrade to add more people.";
      if (raw.includes("owner")) return "Only owners can do that.";
      return "You don't have access to that.";
    case 409:
      // The 409 body carries the URL that owns the thing, which is the whole
      // point of the message — keep it, but capitalize like the rest.
      return raw
        ? raw[0].toUpperCase() + raw.slice(1)
        : "That is managed outside this hub.";
    case 404:
      return "That is gone — it may have been removed already.";
    case 413:
      return "This project is over its plan limit.";
    case 429:
      return "Too many requests. Give it a moment.";
    default:
      if (status >= 500) return "The server had a problem. Try again.";
      return raw
        ? raw[0].toUpperCase() + raw.slice(1)
        : "Something went wrong.";
  }
}

/* The status travels with the message.

   Callers used to get a bare Error, so anything wanting to tell a 429 from a
   404 had to match on prose — which is how a retry policy ends up keyed on
   copy that a product decision then rewrites. `.message` is unchanged, so
   every existing toast and error branch reads exactly as before. */
export class HttpError extends Error {
  constructor(
    readonly status: number,
    message: string,
    // The server's own body, unprocessed. A 409 from upload/content carries
    // the sha that replaced ours, and a caller recovering from it needs the
    // value, not the sentence errorFor built out of it.
    readonly body = "",
  ) {
    super(message);
    this.name = "HttpError";
  }
}

async function fail(r: Response): Promise<never> {
  const body = await r.text();
  throw new HttpError(r.status, errorFor(r.status, body), body);
}

/* Causal receipts: read-your-own-writes across hub processes.

   A hub is more than one process, and each one folds the project's journals
   on its own clock. A write answered by one can be followed by a read served
   by another whose snapshot predates it — the file you just saved reads back
   as it was, the comment you just posted is missing from the list. Every
   write answers with a receipt naming what it stored (X-Bdrive-Receipt: the
   journal op `<device>:<seq>`; X-Bdrive-Comment-Receipt: the comment event's
   object key), and the reads that could show it carry the recent ones back,
   so whichever process answers folds far enough to include them first.

   In memory and short-lived on purpose: a receipt only has to outlive the
   gap between processes catching up, which is seconds, and a reload starts
   from reads that are fresh anyway. Per project, because a receipt names an
   op in that project's journals and means nothing to another. */
const RECEIPT_TTL_MS = 30_000;
const MAX_JOURNAL_RECEIPTS = 4;
const MAX_COMMENT_RECEIPTS = 8;
const JOURNAL_RECEIPT = /^[A-Za-z0-9._-]+:[1-9][0-9]*$/;
const COMMENT_RECEIPT = /^comments\/[0-9a-f]{12}\/([0-9]{19}-[0-9a-f]{8}|deleted)\.json$/;
// The reads a journal receipt can change the answer of.
const JOURNAL_READS: Record<string, true> = { tree: true, file: true, render: true, resolve: true, frame: true };

type Held = { token: string; at: number };
const journalReceipts = new Map<string, Held[]>();
const commentReceipts = new Map<string, Held[]>();

// `/api/p/<id>/` and the route under it, or null for anything else.
function projectRoute(url: string): { base: string; route: string } | null {
  let path = url;
  if (!url.startsWith("/")) {
    try {
      const u = new URL(url, location.href);
      if (u.origin !== location.origin) return null;
      path = u.pathname + u.search;
    } catch {
      return null;
    }
  }
  const m = /^(\/api\/p\/[^/?#]+\/)([^/?#]*)/.exec(path);
  return m ? { base: m[1], route: m[2] } : null;
}

function hold(map: Map<string, Held[]>, base: string, token: string, max: number) {
  const now = Date.now();
  const list = (map.get(base) || []).filter((h) => now - h.at < RECEIPT_TTL_MS && h.token !== token);
  list.push({ token, at: now });
  map.set(base, list.slice(-max));
}

function held(map: Map<string, Held[]>, base: string): string[] {
  const list = map.get(base);
  if (!list) return [];
  const now = Date.now();
  const live = list.filter((h) => now - h.at < RECEIPT_TTL_MS);
  if (live.length) map.set(base, live);
  else map.delete(base);
  return live.map((h) => h.token);
}

/* Record a journal receipt for a project. Exported for a write whose answer
   does not come back through these helpers. */
export function recordProjectReceipt(apiBase: string, receipt: string | null | undefined) {
  if (receipt && JOURNAL_RECEIPT.test(receipt)) hold(journalReceipts, apiBase, receipt, MAX_JOURNAL_RECEIPTS);
}

export function recordCommentReceipt(apiBase: string, key: string | null | undefined) {
  if (key && COMMENT_RECEIPT.test(key)) hold(commentReceipts, apiBase, key, MAX_COMMENT_RECEIPTS);
}

// Take whatever receipts a project response carries.
function noteReceipts(url: string, r: Response) {
  const p = projectRoute(url);
  if (!p) return;
  recordProjectReceipt(p.base, r.headers.get("X-Bdrive-Receipt"));
  recordCommentReceipt(p.base, r.headers.get("X-Bdrive-Comment-Receipt"));
}

// The receipts a request to `url` should carry, as headers.
function receiptHeaders(url: string): Record<string, string> {
  const p = projectRoute(url);
  if (!p) return {};
  const out: Record<string, string> = {};
  // Comments carry journal receipts too: a thread on a file created a moment
  // ago is checked against that file, on whichever process answers.
  if (JOURNAL_READS[p.route] || p.route === "comments") {
    const t = held(journalReceipts, p.base);
    if (t.length) out["X-Bdrive-Receipt"] = t.join(",");
  }
  if (p.route === "comments") {
    const t = held(commentReceipts, p.base);
    if (t.length) out["X-Bdrive-Comment-Receipt"] = t.join(",");
  }
  return out;
}

// fetch, carrying and collecting receipts on project routes.
async function projectFetch(url: string, init: RequestInit = {}): Promise<Response> {
  const extra = receiptHeaders(url);
  const r = await fetch(
    url,
    Object.keys(extra).length ? { ...init, headers: { ...(init.headers as Record<string, string>), ...extra } } : init,
  );
  noteReceipts(url, r);
  return r;
}

export async function getJSON<T>(url: string): Promise<T> {
  // Explicit Accept so endpoints that content-negotiate (e.g. /billing:
  // JSON data vs the app shell) know this is a data fetch.
  const r = await projectFetch(url, { headers: { Accept: "application/json" } });
  if (r.status === 401) toLogin();
  if (!r.ok) await fail(r);
  return r.json();
}

// Same auth/error handling as getJSON, but hands back the raw Response —
// for endpoints that serve bytes rather than JSON (blob?sha=).
export async function getResponse(url: string): Promise<Response> {
  const r = await projectFetch(url);
  if (r.status === 401) toLogin();
  if (!r.ok) await fail(r);
  return r;
}

/* fetch wrapper for methods without a body-returning helper */
export async function api<T = unknown>(
  method: string,
  url: string,
  body?: unknown,
): Promise<T> {
  const opt: RequestInit = { method };
  if (body !== undefined) {
    opt.headers = { "Content-Type": "application/json" };
    opt.body = JSON.stringify(body);
  }
  const r = await projectFetch(url, opt);
  if (!r.ok) await fail(r);
  trackWrite(method, url);
  return r.status === 204 ? ({} as T) : r.json();
}

/* The desktop sidecar's own POSTs, in one place instead of three.
   X-Bdrive-Desktop is the sidecar's CSRF guard (cmd/bdrive/desktop.go
   `guarded`): it forces a preflight the server never approves, so a random
   web page cannot fire one of these at loopback.

   Deliberately NOT routed through api(): its 401 handler sends the window to
   /auth/login, and the sidecar registers no /auth/* routes at all — a 401
   from a proxied hub call would navigate the app to a 404 on loopback. The
   caller gets the raw Response and reads the body, which is what all three
   call sites already did by hand. What they gain is the product-event table:
   the invite this posts was going unmeasured. */
export async function desktopPost(path: string, body?: unknown): Promise<Response> {
  const r = await fetch(path, {
    method: "POST",
    headers: { "X-Bdrive-Desktop": "1", "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (r.ok) trackWrite("POST", path);
  return r;
}

export async function postJSON<T>(url: string, body?: unknown): Promise<T> {
  const r = await projectFetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
  if (r.status === 401) toLogin();
  if (!r.ok) await fail(r);
  trackWrite("POST", url);
  return r.json();
}

/* putText writes a file's whole contents. It is the browser editor's save
   (Editor.tsx), and rides the same upload/content route the CLI and the
   desktop app already use — which is why the desktop gets editing for free:
   its sidecar proxies that route to the hub, so the write is journaled by the
   hub under this account exactly like any other. */
/* Write text, optionally only if the file is still the version we read.

   `base` is the sha the caller's buffer was built on. With it, the hub
   refuses (409) rather than letting this body erase a write that landed in
   between — see handleUploadContent. Without it, the write is
   unconditional, which is what every other caller still does. */
export async function putText(
  url: string,
  text: string,
  base?: string,
  // The user confirmed a save the hub refused for emptying or sharply
  // shrinking the file (422, upload.go sharpShrink).
  confirmShrink = false,
  // Outlive the page (a closing tab parking its buffer). Past the browser's
  // keepalive cap the request goes as an ordinary one, as syncCollab's does.
  keepalive = false,
  // Leaving for good: a body the hub cannot merge is parked beside the file
  // in this same request (`parked` names the copy), since a closing page
  // never gets to send a second one.
  parkOnConflict = false,
  // Create only: a path that already holds other bytes is refused 409 (a
  // conflict copy another tab named in the same second), the same bytes are
  // answered 200 unchanged (a retry whose first answer was lost).
  ifNoneMatch = false,
  // `merged`: the hub found the file had moved past `base` and folded this
  // body into it three ways (upload.go) — the file is NOT exactly `text`.
): Promise<{ sha?: string; merged?: boolean; parked?: string; path?: string; unchanged?: boolean; receipt?: string }> {
  const r = await projectFetch(url, {
    method: "PUT",
    headers: {
      "Content-Type": "text/plain; charset=utf-8",
      ...(base ? { "If-Match": base } : {}),
      ...(ifNoneMatch ? { "If-None-Match": "*" } : {}),
      ...(confirmShrink ? { "X-Bdrive-Confirm-Shrink": "1" } : {}),
      ...(parkOnConflict ? { "X-Bdrive-Park-On-Conflict": "1" } : {}),
    },
    body: text,
    keepalive: Boolean(keepalive && new TextEncoder().encode(text).byteLength <= KEEPALIVE_MAX_BYTES),
  });
  // A page being torn down must not navigate.
  if (r.status === 401 && !keepalive) toLogin();
  if (!r.ok) await fail(r);
  trackWrite("PUT", url);
  return (await r.json().catch(() => ({}))) as {
    sha?: string;
    merged?: boolean;
    parked?: string;
    path?: string;
    unchanged?: boolean;
    receipt?: string;
  };
}

/* Write into a file's co-editing room over plain HTTP, for a client that is
   not (or not yet) on the room's websocket: either a Yjs update this client's
   document holds (queued offline edits), or a text the hub merges three ways
   against the room — `base_text` is what the client started from, `text` what
   it has now. The hub answers with the room's text and the sha it wrote.

   `keepalive` lets the request outlive the page: it is what a closing tab
   uses to hand over edits the socket never got to carry. Browsers cap the
   keepalive bodies in flight at 64 KiB and REJECT the fetch outright past it
   (a TypeError, so nothing is sent at all), which is why a larger body goes
   as an ordinary request instead: it may still finish before the page does.
   Deliberately no login bounce — a page being torn down must not navigate. */
const KEEPALIVE_MAX_BYTES = 60_000;

export async function syncCollab(
  url: string,
  payload: {
    update?: string;
    base?: string;
    base_text?: string;
    text?: string;
    confirm_shrink?: boolean;
    peek?: boolean;
    prior_sent?: string[];
    // A text that clashes with the room is parked beside the file by the hub
    // in this same request, answered with `parked` (a closing page's flush).
    park_on_conflict?: boolean;
  },
  keepalive = false,
): Promise<{ sha: string; text: string; merged?: boolean; update?: string; parked?: string; receipt?: string }> {
  const body = JSON.stringify(payload);
  const bodyBytes = new TextEncoder().encode(body).byteLength;
  // Through projectFetch so the receipt of what the room wrote is held for
  // the reads that follow it (Done re-reads the file and its render).
  const r = await projectFetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
    keepalive: Boolean(keepalive && bodyBytes <= KEEPALIVE_MAX_BYTES),
  });
  if (!r.ok) await fail(r);
  return (await r.json()) as { sha: string; text: string; merged?: boolean; update?: string; parked?: string; receipt?: string };
}
