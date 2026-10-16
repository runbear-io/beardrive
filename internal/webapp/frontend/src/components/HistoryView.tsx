import { useEffect, useMemo, useRef, useState } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HistoryEntry, Node } from "../api/types";
import { HistoryRow, NoteText, type RemoveAction, type RestoreAction } from "./HistoryRow";
import { Icon } from "./shell";
import { copyText, whoChanged } from "../util";
import { toast } from "../toast";
import { groupRuns, runFileCount, type Run } from "../lib/runs";
import { HistoryFilters, authorsOf, filterChips } from "./HistoryFilters";
import { hasHistoryFilters, type HistoryFilters as Filters } from "../router";
import {
  agentName,
  dayLabel,
  historyApiQuery,
  isAgent,
  localDay,
  spanLabel,
  summaryDays,
  summaryLabel,
} from "../lib/activity";
import { DiffTabs } from "./DiffView";

// Undoing a WHOLE run — the run-wide form of restore/remove, and the only
// action the card header carries. Absent when the viewer can't write, like
// its two per-row siblings, so a read-only member never sees a button that
// 403s. The card hands over the run itself, not a file list: which paths are
// reverted is worked out server-side, because this window is paged and
// filtered and a client-computed list is wrong exactly when the run is old.
export type UndoRunAction = {
  onUndoRun: (run: Run) => void;
  busy?: string; // the session (or note) currently in flight
};

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
  // Every file the project still has, so a run card can mark a path it read
  // that has since been deleted — the same label the Dashboard uses.
  flatFiles: Node[];
  onOpen: (path: string, version?: string) => void;
  onMeta: (meta: string) => void;
  onRendered?: () => void;
  restore?: RestoreAction;
  remove?: RemoveAction;
  undoRun?: UndoRunAction;
  // Reader filters, straight from the URL. Applied server-side, so they
  // narrow the whole feed and not just the loaded page.
  filters?: Filters;
  onFilters?: (f: Filters) => void;
  // The shareable URL of a file (at a version), for the panel's Copy link.
  linkFor?: (path: string, version?: string) => string;
}) {
  const { apiBase, target, isFolder, onMeta, onRendered, restore, remove, undoRun, filters } = props;
  // One set for the whole feed, not one per card: every run card asks the
  // same question of the same tree.
  const known = useMemo(() => new Set(props.flatFiles.map((f) => f.path)), [props.flatFiles]);
  const folder = !target || isFolder(target);
  const scope = !target
    ? "prefix="
    : folder
      ? "prefix=" + encodeURIComponent(target + "/")
      : "path=" + encodeURIComponent(target);
  // The date window is resolved against the reader's clock once per filter
  // change: "today" in the key would otherwise shift under an open page.
  const qs = useMemo(() => scope + historyApiQuery(filters), [scope, filters]);
  // Paged: the server hands back a cursor while entries remain, so a project
  // with thousands of changes is reachable to its first one. Pages
  // accumulate into one array
  // — groupRuns and prevBlob both work over the whole window, so a run
  // straddling a page boundary becomes one card when its second page lands,
  // and the oldest loaded row shows no diff base rather than a wrong one.
  const queryClient = useQueryClient();
  const { data, error, isPending, fetchNextPage, hasNextPage, isFetchingNextPage } = useInfiniteQuery({
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
    retry: 1,
  });

  // A request still pending after 10 s is reported as a failure the reader
  // can retry, rather than a "Loading…" that never resolves.
  const [slow, setSlow] = useState(false);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    setSlow(false);
    if (!isPending) return;
    const t = setTimeout(() => setSlow(true), HISTORY_TIMEOUT_MS);
    return () => clearTimeout(t);
  }, [isPending, qs, attempt]);
  const retry = () => {
    setAttempt((n) => n + 1);
    // Reset, not refetch: the hung request is abandoned rather than awaited.
    queryClient.resetQueries({ queryKey: ["history", apiBase, qs] });
  };

  const seenAuthors = useRef(new Set<string>());
  const seenDevices = useRef(new Map<string, string>());

  useEffect(() => {
    if (error) onMeta("History unavailable: " + (error as Error).message);
  }, [error, onMeta]);
  useEffect(() => {
    if (data) onRendered?.();
  }, [data, onRendered]);

  const entries = useMemo(() => (data ? data.pages.flatMap((p) => p.entries || []) : []), [data]);
  // The author and device lists accumulate and never shrink: filtering BY
  // one leaves only its rows loaded, so a list rebuilt from the current feed
  // would drop every other choice and strand the reader on "Anyone" as the
  // only way out.
  for (const a of authorsOf(entries)) seenAuthors.current.add(a);
  for (const e of entries)
    if (e.device?.id && !seenDevices.current.has(e.device.id))
      seenDevices.current.set(e.device.id, e.device.name || e.device.id);
  const devices = [...seenDevices.current].map(([id, name]) => ({ id, name })).sort((a, b) => a.name.localeCompare(b.name));
  const bar = props.onFilters && (
    <HistoryFilters
      filters={filters}
      authors={[...seenAuthors.current].sort()}
      devices={devices}
      onChange={props.onFilters}
    />
  );
  const summary = folder && <ActivitySummary apiBase={apiBase} scope={scope} filters={filters} />;

  const items = useMemo(() => groupRuns(entries), [entries]);
  // Keyboard order: every row as it sits on screen, run rows in card order.
  const order = useMemo(() => items.flatMap((it) => (it.run ? it.run.idx : [it.i])), [items]);
  const [sel, setSel] = useState<number | null>(null);
  useEffect(() => setSel(null), [qs]);
  useEffect(() => {
    const onKey = (ev: KeyboardEvent) => {
      if (ev.metaKey || ev.ctrlKey || ev.altKey || ev.defaultPrevented) return;
      const t = ev.target as HTMLElement | null;
      if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
      if (ev.key === "Escape" && sel !== null) {
        setSel(null);
        return;
      }
      if (ev.key !== "j" && ev.key !== "k") return;
      if (!order.length) return;
      ev.preventDefault();
      const at = sel === null ? -1 : order.indexOf(sel);
      const next = ev.key === "j" ? Math.min(order.length - 1, at + 1) : Math.max(0, at === -1 ? 0 : at - 1);
      setSel(order[next]);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [order, sel]);
  useEffect(() => {
    if (sel === null) return;
    document.querySelector(`.history [data-hidx="${sel}"]`)?.scrollIntoView({ block: "nearest" });
  }, [sel]);

  // Nothing loaded yet. The filters are in the query key, so every keystroke
  // in the path box drops `data` back to undefined — returning a bare filter
  // bar here made a pending request pixel-identical to "nothing matched", and
  // a reader twice concluded a file had no history when it had three entries
  // (BEA-131). The shell always renders so the bar stays interactive; the
  // skeleton holds the newest day's place until it lands.
  if (!data)
    return (
      <div className="activity">
        <div className="history">
          {bar}
          {summary}
          {error || slow ? (
            <HistoryError onRetry={retry} timedOut={!error} />
          ) : (
            isPending && <HistorySkeleton />
          )}
        </div>
      </div>
    );
  // Entries arrive newest-first, so a row's predecessor is the next entry
  // below it on the same path that still has content. This keeps scanning the
  // flat list, never a group: it is a per-path lookup, and grouping must not
  // change what a diff compares against.
  const prevBlob = (i: number) => {
    if (entries[i].prev) return entries[i].prev;
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
  // "" is the marker headBlob already uses for a path whose newest op is a
  // delete — so this restore brings the file back, which nothing in the
  // browser can walk back afterwards. The confirm copy says so (BEA-129).
  const recreates = (i: number) => headBlob.get(entries[i].path) === "";
  // The version from before a change, when putting it back is a real change.
  const restorePrev = (i: number) => {
    const sha = prevBlob(i);
    return sha && sha !== headBlob.get(entries[i].path) ? sha : undefined;
  };
  const chips = filterChips(filters, devices);
  // Day headers sit between rows as siblings, never wrappers, so every row
  // stays a direct child of the feed.
  let lastDay = "";
  const dayCount = new Map<string, number>();
  for (const e of entries) {
    const d = localDay(new Date(e.time));
    dayCount.set(d, (dayCount.get(d) ?? 0) + 1);
  }
  const selEntry = sel !== null ? entries[sel] : undefined;
  const at = sel === null ? -1 : order.indexOf(sel);
  return (
    <div className={"activity" + (selEntry ? " has-panel" : "")}>
      <div className="history">
        {bar}
        {summary}
        {entries.length === 0 &&
          (hasHistoryFilters(filters) ? (
            <div className="empty hempty">
              <span className="hempty-icon">
                <Icon name="search" />
              </span>
              <b>No changes match these filters.</b>
              <span className="hchips">
                {chips.map(([label, rest]) => (
                  <button
                    key={label}
                    type="button"
                    className="hf-chip"
                    title={"Remove " + label}
                    onClick={() => props.onFilters?.(rest)}
                  >
                    {label}
                    <Icon name="x" />
                  </button>
                ))}
              </span>
              <span className="hempty-hint">
                Try widening the date range, or check whether this folder is excluded in .bdriveignore.
              </span>
              <span className="hempty-actions">
                <button type="button" className="btn hf-clear-empty" onClick={() => props.onFilters?.({})}>
                  Clear filters
                </button>
                {filters?.range !== "30d" && (filters?.range || filters?.since || filters?.until) && (
                  <button
                    type="button"
                    className="pbtn hf-widen"
                    onClick={() => props.onFilters?.({ ...filters, range: "30d", since: undefined, until: undefined })}
                  >
                    Show last 30 days
                  </button>
                )}
              </span>
            </div>
          ) : (
            <div className="empty">No history yet.</div>
          ))}
        {items.map((item, n) => {
          const day = localDay(new Date(entries[item.i].time));
          const head =
            day !== lastDay ? (
              <div className="hday" key={"d" + day}>
                <b>{dayLabel(day)}</b>
                <span>
                  · {dayCount.get(day)} change{dayCount.get(day) === 1 ? "" : "s"}
                  {hasNextPage && day === localDay(new Date(entries[entries.length - 1].time)) ? "+" : ""}
                </span>
              </div>
            ) : null;
          lastDay = day;
          const row = item.run ? (
            <RunGroup
              key={"g" + n}
              run={item.run}
              known={known}
              onOpen={props.onOpen}
              apiBase={apiBase}
              prevBlob={prevBlob}
              restoreSha={restoreSha}
              recreates={recreates}
              restore={restore}
              remove={remove}
              undoRun={undoRun}
              sel={sel}
              onSelect={setSel}
            />
          ) : (
            <HistoryRow
              key={"r" + item.i}
              idx={item.i}
              entry={entries[item.i]}
              apiBase={apiBase}
              onOpen={props.onOpen}
              /* Every feed, not just the per-file one (BEA-58): prevBlob is a
                 per-path lookup, so a mixed-path feed diffs each row against its
                 own predecessor. A review-focused reader opens the whole-project
                 feed first, and finding no diff there read as "this product has
                 none". */
              diff={{ apiBase, prev: prevBlob(item.i) }}
              restore={restore}
              restoreSha={restoreSha(item.i)}
              recreates={recreates(item.i)}
              selected={sel === item.i}
              onSelect={() => setSel(item.i)}
              /* no `remove`: an add outside a run card isn't attributable to a
                 run, so the undo has nothing to claim (follow-up issue). */
            />
          );
          return head ? [head, row] : row;
        })}
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
      {selEntry && sel !== null && (
        <ActivityPanel
          key={sel}
          entry={selEntry}
          apiBase={apiBase}
          prev={prevBlob(sel)}
          restorePrev={restorePrev(sel)}
          recreates={recreates(sel)}
          restore={restore}
          linkFor={props.linkFor}
          onOpen={props.onOpen}
          onClose={() => setSel(null)}
          onPrev={at > 0 ? () => setSel(order[at - 1]) : undefined}
          onNext={at < order.length - 1 ? () => setSel(order[at + 1]) : undefined}
        />
      )}
    </div>
  );
}

const HISTORY_TIMEOUT_MS = 10_000;

function HistorySkeleton() {
  return (
    <div className="hskel" aria-busy="true" aria-label="Loading activity">
      <div className="hday">
        <b>Today</b>
      </div>
      {[62, 74, 48, 68, 56].map((w, i) => (
        <div className="hskel-row" key={i}>
          <span className="hskel-dot" />
          <span className="hskel-lines">
            <span style={{ width: w + "%" }} />
            <span style={{ width: w / 2 + "%" }} />
          </span>
          <span className="hskel-time" />
        </div>
      ))}
      <div className="hskel-note">Loading changes…</div>
    </div>
  );
}

function HistoryError({ onRetry, timedOut }: { onRetry: () => void; timedOut: boolean }) {
  return (
    <div className="empty hempty herror" role="alert">
      <span className="hempty-icon herror-icon">
        <Icon name="alert" />
      </span>
      <b>Couldn't load activity</b>
      <span className="hempty-hint">
        {timedOut ? "The history service didn't respond within 10 seconds." : "The history service returned an error."}{" "}
        Your files are still syncing — only this view is affected.
      </span>
      <span className="hempty-actions">
        <button type="button" className="pbtn herror-retry" onClick={onRetry}>
          <Icon name="hist" />
          Try again
        </button>
      </span>
    </div>
  );
}

/* Totals for the window the date filter names (seven days when it names
   none, the days a custom range touches when it is one): one aggregate
   request with the feed's own filters, never a client-side walk over pages. */
function ActivitySummary({ apiBase, scope, filters }: { apiBase: string; scope: string; filters?: Filters }) {
  const days = summaryDays(filters);
  const rest = historyApiQuery(filters);
  let tz = "";
  try {
    tz = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  } catch {
    /* offset alone */
  }
  const qs =
    scope + rest + "&days=" + days + "&offset=" + -new Date().getTimezoneOffset() + (tz ? "&tz=" + encodeURIComponent(tz) : "");
  const { data } = useQuery({
    queryKey: ["history-summary", apiBase, qs],
    queryFn: () => getJSON<HistorySummary>(apiBase + "history/summary?" + qs),
    staleTime: 15_000,
    retry: false,
  });
  if (!data) return <div className="hsum hsum-wait" />;
  const todayKey = localDay(new Date());
  const max = Math.max(1, ...data.days.map((d) => d.agent + d.human));
  const fmt = (d: string) => {
    const [y, m, dd] = d.split("-").map(Number);
    return new Date(y, m - 1, dd);
  };
  const short = (d: Date) => d.toLocaleDateString([], { month: "short", day: "numeric" });
  const n = data.days.length;
  const span = !n ? "" : n === 1 ? short(fmt(data.days[0].date)) : short(fmt(data.days[0].date)) + " – " + short(fmt(data.days[n - 1].date));
  const share = data.total ? Math.round((100 * data.agent) / data.total) : 0;
  const label = summaryLabel(filters);
  return (
    <div className="hsum">
      <div className="hsum-head">
        <div>
          <div className="hsum-total">
            {data.total} change{data.total === 1 ? "" : "s"}
          </div>
          <div className="hsum-sub">
            {span && span + " · "}
            {share}% by agents · {data.devices} device
            {data.devices === 1 ? "" : "s"}
            <span className="hsum-range"> · {label}</span>
          </div>
        </div>
        <div className="hsum-legend">
          <span className="agent">Agents</span>
          <span className="human">People</span>
        </div>
      </div>
      <div className={"hsum-bars" + (data.days.length > 14 ? " dense" : "")}>
        {data.days.map((d, i) => {
          const day = fmt(d.date);
          const today = d.date === todayKey;
          return (
            <div
              className={"hsum-col" + (today ? " today" : "")}
              key={d.date}
              title={`${short(day)}: ${d.agent} by agents, ${d.human} by people`}
            >
              <div className="hsum-stack">
                <span className="agent" style={{ height: (100 * d.agent) / max + "%" }} />
                <span className="human" style={{ height: (100 * d.human) / max + "%" }} />
              </div>
              <span className="hsum-day">
                {data.days.length === 1
                  ? dayLabel(d.date)
                  : data.days.length > 14
                    ? i % 5 === 0 || today
                      ? day.getDate()
                      : ""
                    : day.toLocaleDateString([], { weekday: "short" })}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}

type HistorySummary = {
  total: number;
  agent: number;
  human: number;
  devices: number;
  days: { date: string; agent: number; human: number }[];
};

/* The selected change, beside the feed: what it was, the three ways to read
   it, and what to do about it. J/K move the selection; Esc closes. */
function ActivityPanel(props: {
  entry: HistoryEntry;
  apiBase: string;
  prev?: string;
  restorePrev?: string;
  recreates: boolean;
  restore?: RestoreAction;
  linkFor?: (path: string, version?: string) => string;
  onOpen: (path: string, version?: string) => void;
  onClose: () => void;
  onPrev?: () => void;
  onNext?: () => void;
}) {
  const { entry: e, apiBase, restore } = props;
  const deleted = e.kind === "delete";
  const base = e.path.split("/").pop() || e.path;
  const who = isAgent(e) ? agentName(e.note) : whoChanged(e);
  const dev = e.device.name || e.device.id;
  const when = new Date(e.time);
  const busy = !!restore?.busy && !!props.restorePrev && restore.busy === e.path + props.restorePrev;
  return (
    <aside className="apanel" aria-label={"Change to " + e.path}>
      <div className="ap-head">
        <Icon name="doc" />
        <span className="ap-name">{base}</span>
        <button type="button" className="ap-nav" title="Previous change (K)" disabled={!props.onPrev} onClick={props.onPrev}>
          <Icon name="chev" />
        </button>
        <button type="button" className="ap-nav down" title="Next change (J)" disabled={!props.onNext} onClick={props.onNext}>
          <Icon name="chevd" />
        </button>
        <button type="button" className="ap-close" title="Close (Esc)" onClick={props.onClose}>
          <Icon name="x" />
        </button>
      </div>
      <div className="ap-path">{e.path}</div>
      <div className="ap-who">
        <Icon name={isAgent(e) ? "terminal" : "users"} />
        {who}
        {dev ? " on " + dev : ""} · {dayLabel(localDay(when))}{" "}
        {when.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
      </div>
      <div className="ap-actions">
        {!deleted && e.blob && (
          <button type="button" className="pbtn ap-open" onClick={() => props.onOpen(e.path, e.blob)}>
            <Icon name="expand" />
            Open file
          </button>
        )}
        {restore && props.restorePrev && (
          <button
            type="button"
            className="btn ap-restore"
            disabled={busy}
            onClick={() => restore.onRestore(e.path, props.restorePrev!, props.recreates)}
          >
            <Icon name="hist" />
            {busy ? "Restoring…" : deleted ? "Restore file" : "Restore previous"}
          </button>
        )}
        {props.linkFor && (
          <button
            type="button"
            className="btn ap-copy"
            onClick={async () => {
              const url = location.origin + props.linkFor!(e.path, deleted ? props.prev : e.blob);
              toast((await copyText(url)) ? "Link copied" : url, false);
            }}
          >
            <Icon name="link" />
            Copy link
          </button>
        )}
      </div>
      <DiffTabs apiBase={apiBase} path={e.path} kind={e.kind} prev={props.prev} cur={deleted ? undefined : e.blob} />
      <div className="ap-foot">J / K to move between changes · esc close</div>
    </aside>
  );
}

function RunGroup({
  run,
  known,
  onOpen,
  apiBase,
  prevBlob,
  restoreSha,
  recreates,
  restore,
  remove,
  undoRun,
  sel,
  onSelect,
}: {
  run: Run;
  known: Set<string>;
  onOpen: (path: string, version?: string) => void;
  apiBase: string;
  prevBlob: (i: number) => string | undefined;
  restoreSha: (i: number) => string | undefined;
  recreates: (i: number) => boolean;
  restore?: RestoreAction;
  remove?: RemoveAction;
  undoRun?: UndoRunAction;
  sel: number | null;
  onSelect: (i: number) => void;
}) {
  const [open, setOpen] = useState(true);
  const first = run.entries[0];
  const who = whoChanged(first);
  const dev = [first.device.name || first.device.id, first.device.os].filter(Boolean).join(" · ");
  // What this run READ, joined on the session id its own ops carry — never on
  // the note, which anyone can set to anything. Both the session and the
  // device are required by the server, so a card only ever shows reads its
  // own device reported.
  const sid = first.session;
  const did = first.device?.id;
  const { data: reads } = useQuery({
    queryKey: ["session-reads", apiBase, sid, did],
    queryFn: () =>
      getJSON<{ paths: string[] }>(
        apiBase + "heat?session=" + encodeURIComponent(sid!) + "&device=" + encodeURIComponent(did!),
      ),
    enabled: !!sid && !!did,
    staleTime: 30_000,
  });
  const readPaths = new Set(reads?.paths ?? []);
  const written = new Set(run.entries.map((e) => e.path));
  // Read but never written: the half of the run that History could not show
  // before, and usually the half that answers "what did it look at?".
  const readOnly = [...readPaths].filter((p) => !written.has(p)).sort();
  const times = run.entries.map((e) => new Date(e.time).getTime());
  const span = fmtSpan(Math.min(...times), Math.max(...times));
  const lasted = spanLabel(Math.max(...times) - Math.min(...times));
  // Distinct paths, not ops: repeat edits to one file must not inflate the
  // one number that sizes a run (BEA-39). Every op is still a row below.
  const n = runFileCount(run);
  const undoing = !!undoRun?.busy && undoRun.busy === (run.session || run.note);
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
        <span className="havatar agent" aria-hidden="true">
          <Icon name="terminal" />
        </span>
        <span className="hrun-title">
          <span className="hrun-what">
            <b>{run.session ? agentName(run.note) : who.split(" <")[0]}</b> edited {n} file{n === 1 ? "" : "s"}
            {lasted ? " · " + lasted : ""}
          </span>
          {/* The note is a link when the agent left one — clicking it opens the
              session, so it can't live inside the collapse button. */}
          <span className="hrun-sub">
            <span className="hrun-note">
              <NoteText text={run.note} />
            </span>
            <span className="hrun-meta">
              {readPaths.size > 0 ? `read ${readPaths.size} · changed ${n}` : `${n} file${n === 1 ? "" : "s"}`} ·{" "}
              {who}
              {dev ? " · " + dev : ""}
            </span>
          </span>
        </span>

        <span className="hrun-time">{span}</span>
        {/* The one action the header carries. Every row inside the card
            already has its own; this is the verb the card was grouped for —
            reverting a run file by file and hoping you got them all is what
            it replaces. onUndoRun confirms before anything is written. */}
        {undoRun && (
          <button
            type="button"
            className="hrun-undo"
            disabled={undoing}
            title="Put every file this run touched back the way it was"
            onClick={() => undoRun.onUndoRun(run)}
          >
            <Icon name="hist" />
            {undoing ? "undoing…" : "undo this run"}
          </button>
        )}
      </div>
      {open && (
        <div className="hrun-body">
          {run.entries.map((e, k) => (
            <HistoryRow
              key={k}
              entry={e}
              apiBase={apiBase}
              onOpen={onOpen}
              /* run.idx[k] indexes back into the flat list, so a row inside a
                 card compares against its own predecessor, not its neighbour
                 in the card. */
              diff={{ apiBase, prev: prevBlob(run.idx[k]) }}
              restore={restore}
              remove={remove}
              restoreSha={restoreSha(run.idx[k])}
              recreates={recreates(run.idx[k])}
              inRun
              read={readPaths.has(e.path)}
              idx={run.idx[k]}
              stat
              selected={sel === run.idx[k]}
              onSelect={() => onSelect(run.idx[k])}
            />
          ))}
          {readOnly.length > 0 && (
            <div className="hrun-reads">
              <div className="hrun-reads-head">Read, not changed</div>
              {readOnly.map((p) => (
                <button key={p} type="button" className="hrun-read" onClick={() => onOpen(p)}>
                  <span className="hkind">read</span>
                  <span className="hpath">{p}</span>
                  {/* Word for word what the Dashboard says about the same
                      file (Insights.tsx) — two surfaces reading one ledger
                      must not each invent their own vocabulary for it. */}
                  {!known.has(p) && <span className="in-hp-gone">· no longer in the project</span>}
                </button>
              ))}
            </div>
          )}
          {/* Not decoration: this card is one session on one device, so its
              read count is smaller than the project's totals for the same
              files — and that gap read as a bug. Reads of a deleted file are
              kept and labelled, never dropped: the ledger records what the
              agent did, and an audit surface reports it. */}
          {sid && (
            <div className="hrun-foot">
              Reads shown are what this device reported for this session — a narrower set than the
              project's read totals.
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// "14:02 – 14:04" within a day (the day header names it), the full stamp
// when a run straddles days.
function fmtSpan(from: number, to: number): string {
  const a = new Date(from);
  const b = new Date(to);
  const t = (d: Date) => d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  if (a.toDateString() !== b.toDateString()) return a.toLocaleString() + " – " + b.toLocaleString();
  return from === to ? t(b) : t(a) + " – " + t(b);
}

// The crumb title for a history route target.
export function historyTitle(target: string, isFolder: (p: string) => boolean): string {
  if (!target) return "all changes";
  return isFolder(target) ? target + "/ (folder)" : target;
}
