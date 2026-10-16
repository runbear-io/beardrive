import type { HeatMap, HistoryEntry, Node } from "../api/types";
import { agentName, isAgent } from "./activity.ts";
import { daysSince, heatFor, heatTotal, isDanger } from "./heat.ts";
import { parseConflict } from "./conflict.ts";

/* ---- project Home ----
   Pure derivations for the Home view, no React: everything is computed from
   responses other surfaces already fetch (history, tree, heat), so Home adds
   no server aggregate. Unit-tested in home.test.ts (`npm test`). */

export function greeting(name: string | undefined, now = new Date()): string {
  const h = now.getHours();
  const part = h < 5 ? "Good evening" : h < 12 ? "Good morning" : h < 18 ? "Good afternoon" : "Good evening";
  const first = (name || "").trim().split(/\s+/)[0];
  return first ? part + ", " + first : part;
}

export type SinceSummary = { changes: number; files: number; agent: number; capped: boolean };

// What changed since the last visit, from a page the server already filtered
// with ?since= — re-checking here would compare whole-second times against a
// millisecond baseline and drop changes made in that second. `capped` says
// the page was full, so the counts are floors.
export function sinceSummary(entries: HistoryEntry[], pageFull: boolean): SinceSummary {
  const files = new Set<string>();
  let changes = 0;
  let agent = 0;
  for (const e of entries) {
    changes++;
    files.add(e.path);
    if (isAgent(e)) agent++;
  }
  return { changes, files: files.size, agent, capped: pageFull };
}

// Two newest-first pages as one, each change once.
export function mergeEntries(a: HistoryEntry[], b: HistoryEntry[]): HistoryEntry[] {
  // device + seq is one op; the fallback (an older hub with no seq) can merge
  // two same-second edits of one path from one device.
  const key = (e: HistoryEntry) =>
    e.seq ? (e.device?.id ?? "") + "\0" + e.seq : e.time + "\0" + e.path + "\0" + e.kind + "\0" + (e.device?.id ?? "");
  const seen = new Set<string>();
  const out: HistoryEntry[] = [];
  for (const e of [...a, ...b]) {
    const k = key(e);
    if (seen.has(k)) continue;
    seen.add(k);
    out.push(e);
  }
  return out.sort((x, y) => Date.parse(y.time) - Date.parse(x.time));
}

export type DeviceRow = { id: string; name: string; last: number; agent?: string; you: boolean };

/* The devices that wrote in a window of history, most recent first. The hub
   exposes no device presence, so "last change" is the only recency there is;
   the agent is the one the device's newest session-stamped change names. */
export function devicesFrom(entries: HistoryEntry[], me?: string): DeviceRow[] {
  const out = new Map<string, DeviceRow>();
  for (const e of entries) {
    const id = e.device?.id || e.device?.name;
    if (!id) continue;
    let d = out.get(id);
    if (!d) {
      d = { id, name: e.device.name || id, last: Date.parse(e.time) || 0, you: false };
      out.set(id, d);
    }
    if (!d.agent && isAgent(e)) d.agent = agentName(e.note);
    if (me && e.user === me) d.you = true;
  }
  return [...out.values()].sort((a, b) => b.last - a.last);
}

export const ACTIVE_MS = 15 * 60_000;

export type Attention =
  | { kind: "conflict"; path: string; original: string; device: string }
  | { kind: "comments"; path: string; count: number }
  | { kind: "stale"; path: string; reads: number; days: number };

export function conflictsIn(files: Node[]): Attention[] {
  const out: Attention[] = [];
  for (const f of files) {
    const c = parseConflict(f.path);
    if (c) out.push({ kind: "conflict", path: f.path, original: c.original, device: c.device });
  }
  return out;
}

// Hot-but-stale files, most-read first.
export function staleHot(files: Node[], heat: HeatMap | null, now = Date.now(), max = 3): Attention[] {
  if (!heat) return [];
  const out: { kind: "stale"; path: string; reads: number; days: number }[] = [];
  for (const f of files) {
    const e = heat[f.path];
    const days = daysSince(f.time, now);
    if (!e || days === null) continue;
    const reads = heatTotal(e);
    if (isDanger(reads, days)) out.push({ kind: "stale", path: f.path, reads, days });
  }
  return out.sort((a, b) => b.reads - a.reads).slice(0, max);
}

export type Freshness = "fresh" | "aging" | "stale";
export function freshness(days: number | null): Freshness {
  if (days === null || days > 30) return "stale";
  return days > 7 ? "aging" : "fresh";
}

export type FolderReads = { name: string; reads: number; fresh: Freshness };

/* 30-day reads per top-level folder, busiest first, coloured by the newest
   change anywhere under it. Folders nobody read are left out. */
export function readsByFolder(root: Node | undefined, heat: HeatMap | null, now = Date.now()): FolderReads[] {
  if (!root || !heat) return [];
  const newest = (n: Node): number => {
    let t = n.dir ? 0 : Date.parse(n.time || "") || 0;
    for (const c of n.children || []) t = Math.max(t, newest(c));
    return t;
  };
  const out: FolderReads[] = [];
  for (const c of root.children || []) {
    if (!c.dir) continue;
    const e = heatFor(heat, c.path, true);
    const reads = e ? heatTotal(e) : 0;
    if (!reads) continue;
    const t = newest(c);
    out.push({ name: c.name, reads, fresh: freshness(t ? (now - t) / 86400000 : null) });
  }
  return out.sort((a, b) => b.reads - a.reads);
}
