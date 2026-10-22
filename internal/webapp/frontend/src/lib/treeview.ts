import type { Node } from "../api/types";
import { parseConflict } from "./conflict.ts";
import { prepare, search, type Range } from "./fuzzy.ts";

// What the sidebar tree shows, as pure functions over the project tree so the
// rules are testable without React (treeview.test.ts):
//   - conflict copies never appear as siblings; their original carries a count
//   - housekeeping files are folded into one collapsed "System files" group
//   - a nested folder lists its few most recently changed children first
//   - a folder reports how many files under it changed since a baseline

// Names that are plumbing rather than content. Matched against the base name.
export const SYSTEM_FILE_PATTERNS: RegExp[] = [/^\./, /^_lint\.py$/];

export function isSystemName(name: string): boolean {
  return SYSTEM_FILE_PATTERNS.some((re) => re.test(name));
}

// How many children a nested folder shows before "Show N more". The top level
// is never capped: it is the map of the project.
export const CHILD_CAP = 5;

// The expansion key of a folder's System files group. NUL cannot occur in a
// path, so it never collides with a real folder in the same expanded set.
export function systemKey(parent: string): string {
  return "\u0000sys:" + parent;
}

export type TreeRow =
  | { kind: "node"; node: Node; depth: number; conflicts: number; fresh: number }
  | { kind: "system"; parent: string; depth: number; count: number; open: boolean }
  | { kind: "more"; parent: string; depth: number; hidden: number };

export interface TreeStats {
  // original path → number of conflict copies beside it
  conflicts: Map<string, number>;
  // folder path → files under it changed after the baseline
  fresh: Map<string, number>;
  // any path → newest change time at or under it (ms)
  latest: Map<string, number>;
}

const t = (s?: string) => (s ? Date.parse(s) || 0 : 0);

export function treeStats(root: Node | undefined, since: number): TreeStats {
  const conflicts = new Map<string, number>();
  const fresh = new Map<string, number>();
  const latest = new Map<string, number>();
  const files = new Set<string>();
  const copies: string[] = [];
  const walk = (n: Node): [number, number] => {
    let f = 0;
    let l = 0;
    for (const c of n.children || []) {
      if (c.dir) {
        const [cf, cl] = walk(c);
        f += cf;
        l = Math.max(l, cl);
      } else {
        files.add(c.path);
        if (parseConflict(c.path)) copies.push(c.path);
        const ct = t(c.time);
        latest.set(c.path, ct);
        if (since > 0 && ct > since) f++;
        l = Math.max(l, ct);
      }
    }
    if (n.path) {
      fresh.set(n.path, f);
      latest.set(n.path, l);
    }
    return [f, l];
  };
  if (root) walk(root);
  for (const p of copies) {
    const orig = parseConflict(p)!.original;
    if (files.has(orig)) conflicts.set(orig, (conflicts.get(orig) || 0) + 1);
  }
  return { conflicts, fresh, latest };
}

// A conflict copy is folded into its original only when that original is
// still there to carry the badge; an orphaned copy stays visible.
function isFoldedCopy(path: string, stats: TreeStats): boolean {
  const c = parseConflict(path);
  return !!c && stats.conflicts.has(c.original);
}

export type TreeOrder = "name" | "recent";

export function buildRows(
  root: Node | undefined,
  opts: {
    expanded: Set<string>;
    showAll: Set<string>; // folders whose "Show N more" was clicked
    stats: TreeStats;
    keep?: string; // the selected path: always reachable, never capped away
    cap?: number;
    // "recent" lists each level newest change first; "name" keeps the
    // tree's own order (folders first, then by name).
    order?: TreeOrder;
  },
): TreeRow[] {
  const { expanded, showAll, stats, keep = "" } = opts;
  const cap = opts.cap ?? CHILD_CAP;
  const out: TreeRow[] = [];
  const byLatest = (a: Node, b: Node) => (stats.latest.get(b.path) || 0) - (stats.latest.get(a.path) || 0);
  const onKeepPath = (p: string) => keep === p || keep.startsWith(p + "/");

  const emit = (nodes: Node[], depth: number) => {
    for (const n of nodes) {
      out.push({
        kind: "node",
        node: n,
        depth,
        conflicts: n.dir ? 0 : stats.conflicts.get(n.path) || 0,
        fresh: n.dir ? stats.fresh.get(n.path) || 0 : 0,
      });
      if (n.dir && expanded.has(n.path)) level(n, depth + 1);
    }
  };

  const level = (parent: Node, depth: number) => {
    const kids = (parent.children || []).filter((c) => !isFoldedCopy(c.path, stats));
    const plain = kids.filter((c) => !isSystemName(c.name));
    if (opts.order === "recent") plain.sort(byLatest);
    const system = kids.filter((c) => isSystemName(c.name));

    if (depth > 0 && plain.length > cap && !showAll.has(parent.path)) {
      // WHICH children show is always the most recently changed (HANDOFF
      // §3-A); the order they show in is the reader's chosen order.
      const byRecent = [...plain].sort(byLatest);
      const pick = new Set([...byRecent.slice(0, cap), ...byRecent.slice(cap).filter((c) => onKeepPath(c.path))]);
      const shown = byRecent.slice(0, cap);
      const held = byRecent.slice(cap).filter((c) => onKeepPath(c.path));
      emit(plain.filter((c) => pick.has(c)), depth);
      out.push({ kind: "more", parent: parent.path, depth, hidden: plain.length - shown.length - held.length });
    } else {
      emit(plain, depth);
    }

    if (system.length) {
      const key = systemKey(parent.path);
      const open = expanded.has(key) || system.some((c) => onKeepPath(c.path));
      out.push({ kind: "system", parent: parent.path, depth, count: system.length, open });
      if (open) emit(system, depth + 1);
    }
  };

  if (root) level(root, 0);
  return out;
}

/* ---- filter mode ---- */

export interface FilterHit {
  path: string;
  name: string;
  dir: boolean;
  ranges: Range[]; // inside `name`
}

export interface FilterResult {
  folders: FilterHit[];
  files: FilterHit[];
  folderCount: number;
  fileCount: number;
}

const FILTER_FOLDERS = 5;
const FILTER_FILES = 100;

// The palette's matcher, held to names: the sidebar lists files and folders
// by name, so a token must match inside the name, not across the folders
// above it. Same ranking, same forgiven typo, same NFC fold as ⌘K, so the
// two never disagree about a name.
export function filterTree(
  query: string,
  dirs: Iterable<string>,
  files: Iterable<{ path: string; name: string }>,
): FilterResult {
  const run = (paths: string[], dir: boolean, limit: number) => {
    const { hits, total } = search(query, paths.map(prepare), { limit, nameOnly: true });
    const list = hits.map((h): FilterHit => {
      const path = paths[h.index];
      const cut = path.lastIndexOf("/") + 1;
      return { path, name: path.slice(cut), dir, ranges: h.ranges.map(([a, b]): Range => [a - cut, b - cut]) };
    });
    return { list, total };
  };
  const d = run([...dirs], true, FILTER_FOLDERS);
  const f = run(
    [...files].map((x) => x.path).filter((p) => !parseConflict(p)),
    false,
    FILTER_FILES,
  );
  return { folders: d.list, files: f.list, folderCount: d.total, fileCount: f.total };
}

// The folder a path lives in, with a trailing slash ("" at the root).
export function parentOf(path: string): string {
  const i = path.lastIndexOf("/");
  return i < 0 ? "" : path.slice(0, i + 1);
}
