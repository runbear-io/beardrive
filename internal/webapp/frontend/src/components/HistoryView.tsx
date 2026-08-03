import { useEffect, useState } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HistoryEntry } from "../api/types";
import { HistoryRow, NoteText, type RemoveAction, type RestoreAction } from "./HistoryRow";
import { Icon } from "./shell";
import { whoChanged } from "../util";
import { groupRuns, runFileCount, type Run } from "../lib/runs";

/* ---- history ----
   Every change ever made, straight from the journals: who (account), when,
   from which device (name, OS, IP as the server saw it). The route stores
   one target; the tree says whether it is a folder (subtree feed) or a
   file (version list).

   One agent run is one card. A run identity is already on the wire — the
   sync hook stamps "claude-code session <id>" into the op's note — so
   grouping is a pure group-by here, with no journal or API change: same
   (note, device), one group. Note-less changes (browser uploads, plain
   daemon scans, anything from before the feature) stay bare rows, exactly
   as they were. */
export function HistoryView(props: {
  apiBase: string;
  target: string; // "" = whole project
  isFolder: (p: string) => boolean;
  onOpen: (path: string, version?: string) => void;
  onMeta: (meta: string) => void;
  onRendered?: () => void;
  restore?: RestoreAction;
  remove?: RemoveAction;
}) {
  const { apiBase, target, isFolder, onMeta, onRendered, restore, remove } = props;
  const q = !target
    ? { prefix: "" }
    : isFolder(target)
      ? { prefix: target + "/" }
      : { path: target };
  const qs =
    "path" in q && q.path !== undefined
      ? "path=" + encodeURIComponent(q.path)
      : "prefix=" + encodeURIComponent(q.prefix ?? "");
  // Paged: the server hands back a cursor while entries remain, so a project
  // with thousands of changes is reachable to its first one. Pages accumulate
  // into one array — groupRuns and prevBlob both work over the whole window,
  // so a run straddling a page boundary becomes one card when its second page
  // lands, and the oldest loaded row shows no diff base rather than a wrong one.
  const { data, error, fetchNextPage, hasNextPage, isFetchingNextPage } = useInfiniteQuery({
    queryKey: ["history", apiBase, qs],
    queryFn: ({ pageParam }) =>
      getJSON<{ entries: HistoryEntry[]; next_cursor?: string }>(
        apiBase +
          "history?" +
          qs +
          "&n=100" +
          (pageParam ? "&cursor=" + encodeURIComponent(pageParam) : ""),
      ),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor,
    staleTime: 15_000,
  });

  useEffect(() => {
    if (error) onMeta("History unavailable: " + (error as Error).message);
  }, [error, onMeta]);
  useEffect(() => {
    if (data) onRendered?.();
  }, [data, onRendered]);

  if (!data) return null;
  const entries = data.pages.flatMap((p) => p.entries || []);
  // Diffs are a per-file affair: the subtree feed mixes paths, and each row
  // there would need its own predecessor lookup for no review benefit.
  const perFile = !!target && !isFolder(target);
  // Entries arrive newest-first, so a row's predecessor is the next entry
  // below it on the same path that still has content. This keeps scanning the
  // flat list, never a group: it is a per-path lookup, and grouping must not
  // change what a diff compares against.
  const prevBlob = (i: number) => {
    for (let j = i + 1; j < entries.length; j++) {
      if (entries[j].path === entries[i].path && entries[j].kind !== "delete") return entries[j].blob;
    }
  };
  // A path's current content, from the loaded window: entries are strictly
  // newest-first, so a path's FIRST occurrence decides. A newest-delete leaves
  // the path out — the file is gone, so putting its content back is a real
  // change and its rows stay restorable.
  const headBlob = new Map<string, string>();
  for (const e of entries) {
    if (headBlob.has(e.path)) continue;
    headBlob.set(e.path, e.kind === "delete" ? "" : (e.blob ?? "")); // deleted: nothing is current
  }
  // What a row restores: its own bytes, or — for a delete — the content it
  // removed, which is how a deleted file comes back. Unlike diffs, this is
  // available in every feed, so the predecessor lookup runs regardless.
  // Nothing, when those bytes ARE the file's current content: that restore
  // could only write a +0 −0 change to every device, and the server 409s it
  // (restore.go), so offering the button would only produce an error. The rule
  // is content equality, not row index — a hand-reverted older row is just as
  // much of a no-op — which is exactly what the server checks.
  const restoreSha = (i: number) => {
    const sha = entries[i].kind === "delete" ? prevBlob(i) : entries[i].blob;
    return sha && sha === headBlob.get(entries[i].path) ? undefined : sha;
  };
  return (
    <div className="history">
      {entries.length === 0 && <div className="empty">No history yet.</div>}
      {groupRuns(entries).map((item, n) =>
        item.run ? (
          <RunGroup
            key={"g" + n}
            run={item.run}
            onOpen={props.onOpen}
            apiBase={apiBase}
            perFile={perFile}
            prevBlob={prevBlob}
            restoreSha={restoreSha}
            restore={restore}
            remove={remove}
          />
        ) : (
          <HistoryRow
            key={"r" + item.i}
            entry={entries[item.i]}
            apiBase={apiBase}
            onOpen={props.onOpen}
            diff={perFile ? { apiBase, prev: prevBlob(item.i) } : undefined}
            restore={restore}
            restoreSha={restoreSha(item.i)}
            /* no `remove`: an add outside a run card isn't attributable to a
               run, so the undo has nothing to claim (follow-up issue). */
          />
        ),
      )}
      {/* A button, not an IntersectionObserver: keyboard-reachable, and it
          says out loud that there is more rather than hiding it behind a
          scroll gesture. */}
      {hasNextPage && (
        <button
          type="button"
          className="btn hmore"
          onClick={() => fetchNextPage()}
          disabled={isFetchingNextPage}
        >
          {isFetchingNextPage ? "Loading…" : "Load more"}
        </button>
      )}
    </div>
  );
}

function RunGroup({
  run,
  onOpen,
  apiBase,
  perFile,
  prevBlob,
  restoreSha,
  restore,
  remove,
}: {
  run: Run;
  onOpen: (path: string, version?: string) => void;
  apiBase: string;
  perFile: boolean;
  prevBlob: (i: number) => string | undefined;
  restoreSha: (i: number) => string | undefined;
  restore?: RestoreAction;
  remove?: RemoveAction;
}) {
  const [open, setOpen] = useState(true);
  const first = run.entries[0];
  const who = whoChanged(first);
  const dev = [first.device.name || first.device.id, first.device.os].filter(Boolean).join(" · ");
  const times = run.entries.map((e) => new Date(e.time).getTime());
  const span = fmtSpan(Math.min(...times), Math.max(...times));
  // Distinct paths, not ops: repeat edits to one file must not inflate the
  // one number that sizes a run (BEA-39). Every op is still a row below.
  const n = runFileCount(run);
  return (
    <div className={"hrun" + (open ? " open" : "")}>
      <div className="hrun-head">
        <button
          type="button"
          className="hrun-toggle"
          aria-expanded={open}
          title={open ? "Collapse this run" : "Expand this run"}
          onClick={() => setOpen(!open)}
        >
          <Icon name={open ? "chevd" : "chev"} />
        </button>
        {/* The note is a link when the agent left one — clicking it opens the
            session, so it can't live inside the collapse button. */}
        <span className="hrun-note">
          <NoteText text={run.note} />
        </span>
        <span className="hrun-meta">
          {n} file{n === 1 ? "" : "s"} · {who}
          {dev ? " · " + dev : ""}
        </span>
        <span className="hrun-time">{span}</span>
      </div>
      {open && (
        <div className="hrun-body">
          {run.entries.map((e, k) => (
            <HistoryRow
              key={k}
              entry={e}
              apiBase={apiBase}
              onOpen={onOpen}
              diff={perFile ? { apiBase, prev: prevBlob(run.idx[k]) } : undefined}
              restore={restore}
              remove={remove}
              restoreSha={restoreSha(run.idx[k])}
              inRun
            />
          ))}
        </div>
      )}
    </div>
  );
}

// "14:02 – 14:04" within a day, the full stamp when a run straddles days.
function fmtSpan(from: number, to: number): string {
  const a = new Date(from);
  const b = new Date(to);
  const t = (d: Date) => d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  if (a.toDateString() !== b.toDateString()) return a.toLocaleString() + " – " + b.toLocaleString();
  const day = b.toLocaleDateString();
  return from === to ? day + " " + t(b) : day + " " + t(a) + " – " + t(b);
}

// The crumb title for a history route target.
export function historyTitle(target: string, isFolder: (p: string) => boolean): string {
  if (!target) return "all changes";
  return isFolder(target) ? target + "/ (folder)" : target;
}
