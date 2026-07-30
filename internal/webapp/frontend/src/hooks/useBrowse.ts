import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HeatMap, HistoryEntry, Node } from "../api/types";

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

/* The heat arithmetic itself is pure and lives in lib/heat.ts (unit-tested
   without React); re-exported here so import sites don't care. */
export { heatFor, heatLevel, heatText, heatTotal, hotPathSplit } from "../lib/heat";
