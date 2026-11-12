/* Per-route scroll restoration, as a pure state machine.

   Back/forward returns the reader to where they were; a fresh navigation
   starts at the top. A route change arms a goal, and views re-apply it when
   their content lands — up to MAX_APPLY times, because content grows after
   first paint (mermaid SVGs, a folder's "Recent changes" feed) and a restored
   offset may only fit on a later paint.

   The goal retires itself the moment the reader scrolls somewhere of their
   own accord (noteScroll). That is the choke point: any view that calls
   onRendered for something that isn't a render — a metadata refresh, a poll —
   still cannot move a reader who has taken over. */

export type Goal = { key: string; want: number; attempts: number; anchor?: string };

export const MAX_APPLY = 3;

/** How far off `want` still counts as "that was our own scrollTo". */
const SLOP = 2;

/** `anchor`: an element id to land on instead of `want` once it renders
    (a `#heading` link, or Back/Forward to one). */
export function armGoal(key: string, want: number, anchor?: string): Goal {
  return anchor ? { key, want, attempts: 0, anchor } : { key, want, attempts: 0 };
}

/** The element id a location hash names, or undefined. A malformed escape
    is taken literally rather than thrown. */
export function anchorFromHash(hash: string): string | undefined {
  const raw = hash.startsWith("#") ? hash.slice(1) : hash;
  if (!raw) return undefined;
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

/** Point a live anchor goal at where its element now sits. Returns false
    when the goal is retired, for another route, or has no anchor. */
export function retarget(g: Goal, key: string, want: number): boolean {
  if (g.key !== key || !g.anchor) return false;
  g.want = want;
  return true;
}

/** The offset to scroll to, or null for "do nothing". Counts the attempt. */
export function applyGoal(g: Goal, key: string): number | null {
  if (g.key !== key || g.attempts >= MAX_APPLY) return null;
  g.attempts++;
  return g.want;
}

/** Record a scroll at `top`; `max` is scrollHeight - clientHeight. */
export function noteScroll(g: Goal, key: string, top: number, max: number): void {
  if (g.key !== key) return;
  // Our own scrollTo fires a scroll event, so "the reader moved" is measured
  // against the goal, never against zero.
  if (Math.abs(top - g.want) <= SLOP) return;
  // A page shorter than the last one makes the browser CLAMP the carried-over
  // offset, and a clamp always lands at the very bottom. Before the goal has
  // ever been applied that is the old page's offset arriving; after it, it is
  // a goal that doesn't fit yet — the exact case the retries exist for.
  if (g.attempts === 0 || (top >= max - SLOP && top < g.want)) return;
  g.key = ""; // retired: no later onRendered can move this reader
}
