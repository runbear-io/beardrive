import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { ChangeFileDelta, HeatMap, HistoryEntry, Node } from "../api/types";

/* The volume's file tree.

   NOT polled for freshness: useProjectEvents holds an SSE stream that names
   every changed path, and this query is invalidated from it. The 15s poll
   this used to carry predates that stream and was never retired — on a real
   project the tree is 1.65 MB, so it was re-sending the whole project four
   times a minute to a client that already knew nothing had changed
   (docs/network-efficiency-prd.md).

   The 5 minute interval that remains is not freshness, it is insurance: a
   stream that dies quietly on a tab nobody touches would otherwise leave the
   tree wrong until navigation. Long enough to be a rounding error, short
   enough that nobody stares at a stale listing. */
export function useTree(apiBase: string, enabled = true) {
  const q = useQuery({
    queryKey: ["tree", apiBase],
    // ?slim=1: every node's path is its ancestors' names plus its own, and
    // the walk below has to visit every node anyway — so asking the hub to
    // re-send 480 KB of derivable strings (22% of this response even after
    // gzip) was paying for the same information twice. An older hub ignores
    // the parameter and sends them, which costs nothing but the bytes.
    queryFn: () => getJSON<Node>(apiBase + "tree?slim=1"),
    enabled,
    refetchInterval: 300_000,
  });
  // Flattened lookups: every file (wikilink resolution, palette) and every
  // directory (folder listings, path-kind dispatch).
  const index = useMemo(() => {
    const flatFiles: Node[] = [];
    const dirIndex = new Map<string, Node>();
    /* Rebuilds `path` on the way down, where it is a string concat rather
       than a payload. Written back onto the node so every consumer — file
       tree, folder listing, palette, wikilink resolution — keeps reading
       `c.path` exactly as before; only the wire changed. A hub that still
       sends paths simply gets the same value assigned over itself. */
    const walk = (n: Node, prefix: string) => {
      for (const c of n.children || []) {
        c.path = prefix ? prefix + "/" + c.name : c.name;
        if (c.dir) {
          dirIndex.set(c.path, c);
          walk(c, c.path);
        } else {
          flatFiles.push(c);
        }
      }
    };
    if (q.data) walk(q.data, "");
    return { flatFiles, dirIndex };
  }, [q.data]);
  return { tree: q.data, ...index, loaded: !!q.data };
}
const cloneTreeNode = (n: Node): Node => ({
  ...n,
  children: n.children?.map(cloneTreeNode),
});

const sortTreeNodes = (n: Node): void => {
  if (!n.children) return;
  n.children.sort((a, b) => {
    if (!!a.dir !== !!b.dir) return a.dir ? -1 : 1;
    const an = a.name.toLowerCase();
    const bn = b.name.toLowerCase();
    return an < bn ? -1 : an > bn ? 1 : 0;
  });
  for (const c of n.children) if (c.dir) sortTreeNodes(c);
};

const refreshDirectoryTimes = (n: Node): string | undefined => {
  if (!n.dir) return n.time;
  let newest = "";
  for (const c of n.children || []) {
    const time = refreshDirectoryTimes(c);
    if (!time) continue;
    if (!newest || (Date.parse(time) || 0) > (Date.parse(newest) || 0)) newest = time;
  }
  n.time = newest || undefined;
  return n.time;
};

/** Apply a complete small SSE change frame without fetching /tree. */
export function applyTreeDeltas(root: Node, deltas: ChangeFileDelta[]): Node {
  const next = cloneTreeNode(root);
  if (!next.children) next.children = [];

  for (const delta of deltas) {
    const parts = delta.path.split("/").filter(Boolean);
    if (!parts.length) continue;
    const leaf = parts[parts.length - 1];
    if (delta.op === "put") {
      let parent = next;
      for (let i = 0; i < parts.length - 1; i++) {
        const name = parts[i];
        const path = parts.slice(0, i + 1).join("/");
        let child = parent.children!.find((c) => c.name === name);
        if (!child || !child.dir) {
          child = { name, path, dir: true, children: [] };
          parent.children = parent.children!.filter((c) => c.name !== name);
          parent.children!.push(child);
        } else if (!child.children) {
          child.children = [];
        }
        parent = child;
      }
      const file: Node = {
        name: leaf,
        path: delta.path,
        size: delta.size,
        time: delta.time,
        user: delta.user,
        user_name: delta.user_name,
        author: delta.author,
        device: delta.device,
        link: delta.link,
      };
      parent.children = (parent.children || []).filter((c) => c.name !== leaf);
      parent.children.push(file);
    } else if (delta.op === "delete") {
      const remove = (dir: Node, at: number): boolean => {
        if (!dir.children) return false;
        const name = parts[at];
        if (at === parts.length - 1) {
          const before = dir.children.length;
          dir.children = dir.children.filter((c) => c.name !== name || c.dir);
          return before !== dir.children.length && dir.children.length === 0;
        }
        const child = dir.children.find((c) => c.name === name && c.dir);
        if (!child || !remove(child, at + 1)) return false;
        dir.children = dir.children.filter((c) => c !== child);
        return dir.children.length === 0;
      };
      remove(next, 0);
    }
    sortTreeNodes(next);
    refreshDirectoryTimes(next);
  }
  return next;
}

/* ---- read heat ----
   30-day read counts per path from the heat API (hub only). Counts only —
   the server never says who read what. */
export function useHeat(apiBase: string, enabled: boolean) {
  const q = useQuery({
    queryKey: ["heat", apiBase],
    queryFn: () => getJSON<{ entries: HeatMap }>(apiBase + "heat?days=30"),
    enabled,
    // Read counts move when somebody READS, which nothing here can observe —
    // so this is refreshed when a surface that shows it opens (Browser.tsx),
    // not on a timer that re-sent 133 KB every minute forever.
    staleTime: 60_000,
  });
  return q.data?.entries ?? null;
}

// The folder's change feed, straight from the journals (hub only).
export function useFolderHistory(apiBase: string, prefix: string, enabled: boolean) {
  const q = useQuery({
    queryKey: ["history", apiBase, "prefix", prefix, 20],
    queryFn: () =>
      getJSON<{ entries: HistoryEntry[] }>(
        apiBase + "history?prefix=" + encodeURIComponent(prefix) + "&n=20",
      ),
    enabled,
    staleTime: 15_000,
  });
  return q.data?.entries ?? null;
}

/* The heat arithmetic itself is pure and lives in lib/heat.ts (unit-tested
   without React); re-exported here so import sites don't care. */
export { heatFor, heatLevel, heatText, heatTotal, hotPathSplit } from "../lib/heat";
