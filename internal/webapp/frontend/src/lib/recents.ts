import { useSyncExternalStore } from "react";

// The files this browser opened in a project, newest first, for Home's
// "Jump back in". Per-browser like the visit baseline (visits.ts): the hub
// keeps no record of who opened what, and a reader's own recents need none.
// Storage failures degrade to "nothing remembered".

export type Recent = { path: string; at: number };

const KEY = (pid: string) => "bdrive.recent." + pid;
export const RECENT_KEEP = 12;

// Moves `path` to the front, dropping its older entry and anything past the cap.
export function pushRecent(list: Recent[], path: string, at: number, keep = RECENT_KEEP): Recent[] {
  return [{ path, at }, ...list.filter((r) => r.path !== path)].slice(0, keep);
}

export function parseRecents(raw: string | null): Recent[] {
  try {
    const v = JSON.parse(raw || "[]");
    if (!Array.isArray(v)) return [];
    return v.filter(
      (r): r is Recent => !!r && typeof r.path === "string" && typeof r.at === "number" && Number.isFinite(r.at),
    );
  } catch {
    return [];
  }
}

const cache = new Map<string, Recent[]>();
const NONE: Recent[] = [];
const listeners = new Set<() => void>();
function subscribe(l: () => void) {
  listeners.add(l);
  return () => {
    listeners.delete(l);
  };
}

export function recentsOf(pid: string): Recent[] {
  let r = cache.get(pid);
  if (!r) {
    let raw: string | null = null;
    try {
      raw = localStorage.getItem(KEY(pid));
    } catch {
      /* ignore */
    }
    r = parseRecents(raw);
    cache.set(pid, r);
  }
  return r;
}

export function noteOpened(pid: string, path: string) {
  const next = pushRecent(recentsOf(pid), path, Date.now());
  cache.set(pid, next);
  try {
    localStorage.setItem(KEY(pid), JSON.stringify(next));
  } catch {
    /* ignore */
  }
  for (const l of listeners) l();
}

export function useRecents(pid: string | undefined): Recent[] {
  return useSyncExternalStore(subscribe, () => (pid ? recentsOf(pid) : NONE));
}
