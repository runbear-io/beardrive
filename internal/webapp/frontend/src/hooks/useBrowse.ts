import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HeatMap, HistoryEntry, Node } from "../api/types";

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
    queryFn: () => getJSON<Node>(apiBase + "tree"),
    enabled,
    refetchInterval: 300_000,
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
