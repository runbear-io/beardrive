import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HeatEntry, HeatMap, HistoryEntry, Node } from "../api/types";

// The volume's file tree, polled so synced changes appear without a
// reload. react-query's structural sharing keeps identical polls from
// re-rendering (the classic app compared JSON strings for the same
// reason).
export function useTree(apiBase: string, enabled = true) {
  const q = useQuery({
    queryKey: ["tree", apiBase],
    queryFn: () => getJSON<Node>(apiBase + "tree"),
    enabled,
    refetchInterval: 15_000,
  });
  // Flattened lookups: every file (wikilink resolution, palette) and every
  // directory (folder listings, path-kind dispatch).
  const index = useMemo(() => {
    const flatFiles: Node[] = [];
    const dirIndex = new Map<string, Node>();
    const walk = (n: Node) => {
      for (const c of n.children || []) {
        if (c.dir) {
          dirIndex.set(c.path, c);
          walk(c);
        } else {
          flatFiles.push(c);
        }
      }
    };
    if (q.data) walk(q.data);
    return { flatFiles, dirIndex };
  }, [q.data]);
  return { tree: q.data, ...index, loaded: !!q.data };
}

/* ---- read heat ----
   30-day read counts per path from the heat API (hub only). Counts only —
   the server never says who read what. */
export function useHeat(apiBase: string, enabled: boolean) {
  const q = useQuery({
    queryKey: ["heat", apiBase],
    queryFn: () => getJSON<{ entries: HeatMap }>(apiBase + "heat?days=30"),
    enabled,
    staleTime: 60_000,
    refetchInterval: 60_000,
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

/* Heat for one listing entry: a file's own bucket, or the subtree sum for a
   folder. Null when there is nothing to show. */
export function heatFor(heatMap: HeatMap | null, path: string, isDir: boolean): HeatEntry | null {
  if (!heatMap) return null;
  if (!isDir) return heatMap[path] || null;
  const agg = { human: 0, agent: 0, share: 0 };
  for (const [p, e] of Object.entries(heatMap)) {
    if (!p.startsWith(path + "/")) continue;
    agg.human += e.human || 0;
    agg.agent += e.agent || 0;
    agg.share += e.share || 0;
  }
  return agg.human || agg.agent || agg.share ? agg : null;
}

export function heatTotal(e: HeatEntry): number {
  return (e.human || 0) + (e.agent || 0) + (e.share || 0);
}

export function heatText(e: HeatEntry): string {
  const total = heatTotal(e);
  if (!total) return "";
  let s = total + (total === 1 ? " read" : " reads");
  if (e.agent) s += " (" + e.agent + " agent)";
  return s;
}

/* Dot intensity 1–4, log-ish steps: 1–2, 3–9, 10–29, 30+ reads. */
export function heatLevel(e: HeatEntry): number {
  const total = heatTotal(e);
  if (!total) return 0;
  if (total < 3) return 1;
  if (total < 10) return 2;
  if (total < 30) return 3;
  return 4;
}
