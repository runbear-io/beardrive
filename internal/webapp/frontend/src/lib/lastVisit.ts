// The last-visit marker behind "What's new" (/<project-id>/since) — the one
// storage-touching module in an otherwise pure lib/, and the only
// localStorage in the frontend. Every access is wrapped: Safari private mode
// and disabled site storage throw on get and on set alike, and a catch-up
// view is not worth a white screen.
//
// Keyed by account as well as project: two people sharing a laptop (or a demo
// switching personas) would otherwise steal each other's last-visit time.
// Per browser, not per account on the server — a laptop and a phone keep
// separate markers. That is the price of the small version.

const key = (project: string, account?: string) =>
  "bdrive.lastVisit." + (account || "anon") + "." + project;

const WINDOW_DAYS = 7;

// The baseline for one mount, read exactly once. No marker (first visit,
// fresh browser, storage off) falls back to the last 7 days, flagged so the
// view can say so rather than pretending it knows when you were last here.
export function lastVisit(project: string, account?: string): { since: string; first: boolean } {
  let v: string | null = null;
  try {
    v = localStorage.getItem(key(project, account));
  } catch {
    /* storage off — treat as a first visit */
  }
  return v
    ? { since: v, first: false }
    : { since: new Date(Date.now() - WINDOW_DAYS * 864e5).toISOString(), first: true };
}

export function stampVisit(project: string, account?: string): void {
  try {
    localStorage.setItem(key(project, account), new Date().toISOString());
  } catch {
    /* storage off — the view degrades to "always the last 7 days" */
  }
}
