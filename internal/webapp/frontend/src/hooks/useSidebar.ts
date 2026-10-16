import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { CommentThread, HistoryEntry } from "../api/types";
import type { DeviceHeat } from "../components/Insights";
import { useVisitBaseline } from "../lib/visits";
import { useTree } from "./useBrowse";

// The sidebar's ambient numbers, all derived from routes the hub already
// serves. Each one is optional: a hub that cannot answer (reads off, an older
// server, a 403) leaves its slot empty rather than showing a guess. A peer's
// write must stay a one-or-two-request event for every open tab and an idle
// tab must ask for nothing (netbudget.spec.ts), so there is exactly one
// history query here, under the prefix the live-event stream invalidates, and
// "synced Xm ago" reads the tree the sidebar already holds.

const apiOf = (pid: string) => "/api/p/" + pid + "/";

// Changes since this reader's last visit, capped at what one page returns.
export const ACTIVITY_CAP = 100;
export function useActivityCount(pid: string | undefined): number | null {
  const since = useVisitBaseline(pid);
  const apiBase = pid ? apiOf(pid) : "";
  const q = useQuery({
    queryKey: ["history", apiBase, "since", since],
    queryFn: () =>
      getJSON<{ entries: HistoryEntry[] }>(
        apiBase + "history?n=" + ACTIVITY_CAP + "&since=" + encodeURIComponent(new Date(since).toISOString()),
      ),
    enabled: !!pid && since > 0,
    retry: false,
    staleTime: 15_000,
  });
  return q.data ? (q.data.entries || []).length : null;
}

// "N devices · synced Xm ago": agent devices seen reading in the last 30 days
// (the same breakdown Insights shows) and the newest change in the tree.
export function useSyncLine(pid: string | undefined, readsEnabled: boolean) {
  const apiBase = pid ? apiOf(pid) : "";
  const devices = useQuery({
    queryKey: ["heatDevices", apiBase],
    queryFn: () => getJSON<{ devices: DeviceHeat[] }>(apiBase + "heat?by=device&days=30"),
    enabled: !!pid && readsEnabled,
    retry: false,
    staleTime: 60_000,
  });
  const { flatFiles } = useTree(apiBase, !!pid);
  const synced = useMemo(() => {
    let max = 0;
    for (const f of flatFiles) max = Math.max(max, (f.time && Date.parse(f.time)) || 0);
    return max || undefined;
  }, [flatFiles]);
  const n = devices.data?.devices?.length;
  return { devices: n || undefined, synced };
}

// Open review threads per path, for the badges on pinned files.
export function useOpenCommentCounts(apiBase: string, enabled: boolean): Map<string, number> {
  const q = useQuery({
    queryKey: ["comments", apiBase, "open-all"],
    queryFn: () => getJSON<{ comments: CommentThread[] }>(apiBase + "comments?status=open"),
    enabled,
    retry: false,
    refetchInterval: 60_000,
  });
  return useMemo(() => {
    const m = new Map<string, number>();
    for (const t of q.data?.comments || []) m.set(t.path, (m.get(t.path) || 0) + 1);
    return m;
  }, [q.data]);
}

export function agoText(ms: number, now = Date.now()): string {
  const s = Math.max(0, Math.round((now - ms) / 1000));
  if (s < 60) return "just now";
  const m = Math.round(s / 60);
  if (m < 60) return m + "m ago";
  const h = Math.round(m / 60);
  if (h < 48) return h + "h ago";
  return Math.round(h / 24) + "d ago";
}
