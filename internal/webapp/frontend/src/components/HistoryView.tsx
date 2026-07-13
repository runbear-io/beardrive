import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HistoryEntry } from "../api/types";
import { HistoryRow } from "./HistoryRow";

/* ---- history ----
   Every change ever made, straight from the journals: who (account), when,
   from which device (name, OS, IP as the server saw it). The route stores
   one target; the tree says whether it is a folder (subtree feed) or a
   file (version list). */
export function HistoryView(props: {
  apiBase: string;
  target: string; // "" = whole project
  isFolder: (p: string) => boolean;
  onOpen: (path: string) => void;
  onMeta: (meta: string) => void;
  onRendered?: () => void;
}) {
  const { apiBase, target, isFolder, onMeta, onRendered } = props;
  const q = !target
    ? { prefix: "" }
    : isFolder(target)
      ? { prefix: target + "/" }
      : { path: target };
  const qs =
    "path" in q && q.path !== undefined
      ? "path=" + encodeURIComponent(q.path)
      : "prefix=" + encodeURIComponent(q.prefix ?? "");
  const { data, error } = useQuery({
    queryKey: ["history", apiBase, qs, 200],
    queryFn: () => getJSON<{ entries: HistoryEntry[] }>(apiBase + "history?" + qs + "&n=200"),
    staleTime: 15_000,
  });

  useEffect(() => {
    if (error) onMeta("History unavailable: " + (error as Error).message);
  }, [error, onMeta]);
  useEffect(() => {
    if (data) onRendered?.();
  }, [data, onRendered]);

  if (!data) return null;
  const entries = data.entries || [];
  return (
    <div className="history">
      {entries.length === 0 && <div className="empty">No history yet.</div>}
      {entries.map((e, i) => (
        <HistoryRow key={i} entry={e} onOpen={props.onOpen} />
      ))}
    </div>
  );
}

// The crumb title for a history route target.
export function historyTitle(target: string, isFolder: (p: string) => boolean): string {
  if (!target) return "all changes";
  return isFolder(target) ? target + "/ (folder)" : target;
}
