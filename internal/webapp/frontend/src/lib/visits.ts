import { useSyncExternalStore } from "react";

// Per-browser sidebar memory, keyed by project: when this reader last looked
// (the baseline every "N new" count is measured from) and which paths they
// pinned. Client-side on purpose — it is one person's view of a shared
// project, and nothing else needs to agree with it.
//
// Storage throws in private modes and wherever it is disabled; a preference
// is never worth a broken page, so every access degrades to "nothing stored".

const VISIT_KEY = (pid: string) => "bdrive.lastVisit." + pid;
const PINS_KEY = (pid: string) => "bdrive.pins." + pid;

function read(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}
function write(key: string, value: string) {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* ignore */
  }
}

const listeners = new Set<() => void>();
function emit() {
  for (const l of listeners) l();
}
function subscribe(l: () => void) {
  listeners.add(l);
  return () => {
    listeners.delete(l);
  };
}

/* ---- last visit ----
   The baseline is fixed for the length of a page session: it is what was
   stored when the project was first opened, so counts do not drain away while
   the reader is still looking at them. The stored marker moves forward when
   the reader leaves (pagehide, project switch) and when they open Activity —
   which is also the one moment the baseline itself moves, since that page IS
   the list of what was new. A project never visited before starts at "now":
   a first visit has nothing to be new relative to. */
const baselines = new Map<string, number>();

export function visitBaseline(pid: string): number {
  let b = baselines.get(pid);
  if (b === undefined) {
    const stored = Number(read(VISIT_KEY(pid)));
    b = Number.isFinite(stored) && stored > 0 ? stored : Date.now();
    baselines.set(pid, b);
    if (!(stored > 0)) write(VISIT_KEY(pid), String(b));
  }
  return b;
}

// Records "seen up to now" for the NEXT session without moving this one's
// baseline. Leaving ends the session, so the cached baseline is dropped and a
// return to this project in the same tab starts from the stored marker.
export function noteLeaving(pid: string) {
  write(VISIT_KEY(pid), String(Date.now()));
  baselines.delete(pid);
}

// The reader looked at everything (opened Activity): both the stored marker
// and this session's baseline move to now.
export function markAllSeen(pid: string) {
  const now = Date.now();
  baselines.set(pid, now);
  write(VISIT_KEY(pid), String(now));
  emit();
}

export function useVisitBaseline(pid: string | undefined): number {
  return useSyncExternalStore(subscribe, () => (pid ? visitBaseline(pid) : 0));
}

/* ---- pins ---- */
const pinCache = new Map<string, string[]>();
const NO_PINS: string[] = [];

export function pinsOf(pid: string): string[] {
  let p = pinCache.get(pid);
  if (!p) {
    try {
      const v = JSON.parse(read(PINS_KEY(pid)) || "[]");
      p = Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
    } catch {
      p = [];
    }
    pinCache.set(pid, p);
  }
  return p;
}

export function togglePin(pid: string, path: string) {
  const cur = pinsOf(pid);
  const next = cur.includes(path) ? cur.filter((p) => p !== path) : [...cur, path];
  pinCache.set(pid, next);
  write(PINS_KEY(pid), JSON.stringify(next));
  emit();
}

export function usePins(pid: string | undefined): string[] {
  return useSyncExternalStore(subscribe, () => (pid ? pinsOf(pid) : NO_PINS));
}

/* ---- tree order ----
   How the sidebar tree sorts each folder, one choice per browser. */

const ORDER_KEY = "bdrive.treeOrder";

export function useTreeOrder(): "name" | "recent" {
  return useSyncExternalStore(subscribe, () => (read(ORDER_KEY) === "recent" ? "recent" : "name"));
}

export function setTreeOrder(order: "name" | "recent") {
  write(ORDER_KEY, order);
  emit();
}
