import { useMemo, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { CommentThread, HeatMap, HistoryEntry, Node, Project } from "../api/types";
import { linkProps } from "../nav";
import { urlForPath, urlForView } from "../router";
import { agentName, isAgent } from "../lib/activity";
import { ACTIVITY_CAP, agoText, useSinceHistory } from "../hooks/useSidebar";
import { useVisitBaseline } from "../lib/visits";
import { useRecents } from "../lib/recents";
import { agoLabel } from "../lib/heat";
import { groupRuns, runFileCount } from "../lib/runs";
import {
  ACTIVE_MS,
  conflictsIn,
  devicesFrom,
  greeting,
  readsByFolder,
  mergeEntries,
  sinceSummary,
  staleHot,
  type Attention,
} from "../lib/home";
import { agentPrompt } from "../lib/prompt";
import { ConnectGuide, GuideCode } from "./ConnectGuide";
import { FolderListing } from "./FolderListing";
import type { FolderRule } from "../api/types";
import { Icon } from "./shell";

/* ---- project Home ----
   "What changed and what needs me?" — every section is derived from routes
   other surfaces already call: one page of history (summary, feed, devices),
   the tree (recents, conflicts, freshness), heat (stale files, where reads
   go) and the open-comments list the sidebar's pin badges share. An empty
   project shows the connect guide instead: there is nothing to summarise
   until a device has synced. */

const PAGE = 200;
const base = (pid: string) => "/api/p/" + pid + "/";
const dirOf = (p: string) => (p.includes("/") ? p.slice(0, p.lastIndexOf("/") + 1) : "");
const nameOf = (p: string) => p.slice(p.lastIndexOf("/") + 1);

type Actor = "" | "agent" | "human";

export function Home(props: {
  project: Project;
  tree: Node | undefined;
  flatFiles: Node[];
  loaded: boolean;
  heatMap: HeatMap | null;
  folders: FolderRule[];
  me?: { email: string; name: string };
  comments: boolean;
  existing?: boolean;
  onOpen: (path: string) => void;
  onFullHistory: (path: string) => void;
  onRendered?: () => void;
}) {
  const { project, flatFiles, heatMap, me } = props;
  const pid = project.id;
  const apiBase = base(pid);
  const since = useVisitBaseline(pid);
  const recents = useRecents(pid);
  const [actor, setActor] = useState<Actor>("");

  /* Two history pages. The sidebar's since-last-visit page is shared (same
     key, one request) and is the one the change stream refreshes, so a
     peer's write stays a one-or-two-request event (netbudget.spec.ts). The
     wider page for the feed and devices is not refreshed by writes; every
     write lands after the baseline, so merging the two keeps the feed live. */
  const recent = useSinceHistory(pid);
  const hist = useQuery({
    queryKey: ["homeHistory", apiBase],
    queryFn: () => getJSON<{ entries: HistoryEntry[] }>(apiBase + "history?n=" + PAGE),
    enabled: props.loaded && flatFiles.length > 0,
    retry: false,
    staleTime: 60_000,
  });
  // Same key and request as the sidebar's pin badges, so it is one fetch.
  const comments = useQuery({
    queryKey: ["comments", apiBase, "open-all"],
    queryFn: () => getJSON<{ comments: CommentThread[] }>(apiBase + "comments?status=open"),
    enabled: props.comments && flatFiles.length > 0,
    retry: false,
  });

  const fresh = useMemo(() => recent.data?.entries || [], [recent.data]);
  const entries = useMemo(() => mergeEntries(fresh, hist.data?.entries || []), [fresh, hist.data]);
  const summary = useMemo(() => sinceSummary(fresh, fresh.length >= ACTIVITY_CAP), [fresh]);
  const devices = useMemo(() => devicesFrom(entries, me?.email), [entries, me?.email]);
  const fileSet = useMemo(() => new Set(flatFiles.map((f) => f.path)), [flatFiles]);
  const jump = useMemo(() => recents.filter((r) => fileSet.has(r.path)).slice(0, 4), [recents, fileSet]);

  const attention = useMemo(() => {
    const out: Attention[] = conflictsIn(flatFiles);
    // "Your files": the latest change is yours, or you are in the thread.
    const mine = new Set(flatFiles.filter((f) => me && f.user === me.email).map((f) => f.path));
    const per = new Map<string, number>();
    for (const t of comments.data?.comments || []) {
      const involved = !!me && (t.author === me.email || t.messages.some((m) => m.author === me.email));
      if (!mine.has(t.path) && !involved) continue;
      per.set(t.path, (per.get(t.path) || 0) + 1);
    }
    for (const [path, count] of per) out.push({ kind: "comments", path, count });
    return out.concat(staleHot(flatFiles, heatMap));
  }, [flatFiles, comments.data, heatMap, me]);

  const reads = useMemo(() => readsByFolder(props.tree, heatMap), [props.tree, heatMap]);
  const feed = useMemo(() => {
    const list = actor ? entries.filter((e) => (actor === "agent") === isAgent(e)) : entries;
    return { list, items: groupRuns(list).slice(0, 6) };
  }, [entries, actor]);

  if (!props.loaded) return <div className="empty">Loading…</div>;
  if (flatFiles.length === 0) {
    return (
      <div className="home home-empty">
        <ConnectGuide project={project} existing={props.existing} />
      </div>
    );
  }

  const href = (p: string) => urlForPath(p, pid);
  const activityHref = urlForView("history", pid, "", actor ? { actor } : undefined);

  return (
    <div className="home">
      <header className="home-head">
        <h1 className="home-greet">{greeting(me?.name)}</h1>
        <p className="home-since">
          {recent.data
            ? summary.changes === 0
              ? `Nothing changed since your last visit ${agoText(since)}.`
              : `Since your last visit ${agoText(since)} — ${summary.changes}${summary.capped ? "+" : ""} ` +
                `change${summary.changes === 1 ? "" : "s"} across ${summary.files} file${summary.files === 1 ? "" : "s"}` +
                (summary.agent ? `, ${summary.agent} of them by agents.` : ".")
            : project.description || ""}
        </p>
      </header>

      {jump.length > 0 && (
        <section className="home-sec home-jump" aria-label="Jump back in">
          <h2 className="home-eyebrow">Jump back in</h2>
          <div className="home-cards">
            {jump.map((r) => (
              <a key={r.path} className="home-card" {...linkProps(href(r.path))}>
                <span className="hc-name">
                  <Icon name="doc" />
                  {nameOf(r.path)}
                </span>
                <span className="hc-dir">{dirOf(r.path) || "/"}</span>
                <span className="hc-when">Viewed {agoText(r.at)}</span>
              </a>
            ))}
          </div>
        </section>
      )}

      <div className="home-grid">
        <div className="home-main">
          <section className="home-sec home-attn" aria-label="Needs your attention">
            <h2 className="home-h">
              Needs your attention
              {attention.length > 0 && <span className="home-count">{attention.length}</span>}
            </h2>
            {attention.length === 0 ? (
              <p className="home-none">Nothing needs you right now.</p>
            ) : (
              <ul className="home-list">
                {attention.map((a) => (
                  <AttentionRow key={a.kind + a.path} a={a} href={href} />
                ))}
              </ul>
            )}
          </section>

          <section className="home-sec home-activity" aria-label="Recent activity">
            <div className="home-row">
              <h2 className="home-h">Recent activity</h2>
              <a className="home-more" {...linkProps(activityHref)}>
                View all activity →
              </a>
            </div>
            <div className="hf-seg" role="group" aria-label="Who made the change">
              {(
                [
                  ["", "Everyone"],
                  ["agent", "Agents"],
                  ["human", "People"],
                ] as const
              ).map(([v, label]) => (
                <button key={v} aria-pressed={actor === v} onClick={() => setActor(v)}>
                  {label}
                </button>
              ))}
            </div>
            {hist.isError ? (
              <p className="home-none">Activity is unavailable right now.</p>
            ) : !hist.data ? (
              <p className="home-none">Loading…</p>
            ) : feed.items.length === 0 ? (
              <p className="home-none">No changes yet.</p>
            ) : (
              <ul className="home-feed">
                {feed.items.map((it) => {
                  const e = feed.list[it.i];
                  const run = it.run;
                  const agent = isAgent(e);
                  const who = agent ? agentName(e.note) : e.user_name || e.author || "Someone";
                  const what = run
                    ? `${e.kind === "delete" ? "changed" : "updated"} ${runFileCount(run)} files`
                    : e.kind === "add"
                      ? "created"
                      : e.kind === "delete"
                        ? "deleted"
                        : "edited";
                  return (
                    <li key={it.i} className="home-fe">
                      <span className={"fe-avatar " + (agent ? "agent" : "human")} aria-hidden="true">
                        <Icon name={agent ? "sparkle" : "users"} />
                      </span>
                      <div className="fe-body">
                        <div>
                          <b>{who}</b> <span className="fe-what">{what}</span>
                        </div>
                        {run ? (
                          <span className="fe-path">{dirOf(e.path) || nameOf(e.path)}</span>
                        ) : e.kind === "delete" ? (
                          <span className="fe-path del">{e.path}</span>
                        ) : (
                          <a className="fe-path" {...linkProps(href(e.path))}>
                            {e.path}
                          </a>
                        )}
                        {e.device?.name && <span className="fe-dev">{e.device.name}</span>}
                      </div>
                      <span className="fe-when">{agoText(Date.parse(e.time))}</span>
                    </li>
                  );
                })}
              </ul>
            )}
          </section>
        </div>

        <aside className="home-side">
          <section className="home-sec home-devices" aria-label="Devices">
            <h2 className="home-h">Devices</h2>
            {devices.length === 0 ? (
              <p className="home-none">No device has synced a change recently.</p>
            ) : (
              <ul className="home-devlist">
                {devices.slice(0, 5).map((d) => {
                  const active = Date.now() - d.last < ACTIVE_MS;
                  return (
                    <li key={d.id}>
                      <span className={"dev-dot" + (active ? " on" : "")} aria-hidden="true" />
                      <div className="dev-text">
                        <span className="dev-name">{d.name}</span>
                        <span className="dev-sub">
                          {active ? "Active now" : "Last change " + agoText(d.last)}
                          {d.you ? " · you" : ""}
                        </span>
                      </div>
                      {d.agent && <span className="dev-agent">{d.agent}</span>}
                    </li>
                  );
                })}
                {devices.length > 5 && <li className="home-none">+ {devices.length - 5} more devices</li>}
              </ul>
            )}
          </section>

          <section className="home-sec home-connect" aria-label="Connect another device">
            <h2 className="home-h">
              <Icon name="terminal" />
              Connect another device
            </h2>
            <p className="gd-desc">Paste this into your coding agent in the folder you want synced.</p>
            <GuideCode code={agentPrompt(window.location.origin, { project })} />
            <a className="home-more" {...linkProps(urlForView("install", pid))}>
              Full setup guide →
            </a>
          </section>

          {reads.length > 0 && (
            <section className="home-sec home-reads" aria-label="Where reads go">
              <div className="home-row">
                <h2 className="home-h">Where reads go</h2>
                <a className="home-more" {...linkProps(urlForView("dashboard", pid))}>
                  Insights →
                </a>
              </div>
              <ul className="home-bars">
                {reads.slice(0, 6).map((r) => (
                  <li key={r.name} className={"fresh-" + r.fresh}>
                    <a className="hb-label" {...linkProps(href(r.name))}>
                      <span>{r.name}</span>
                      <span className="hb-n">{r.reads}</span>
                    </a>
                    <span className="hb-track">
                      <span className="hb-fill" style={{ width: (100 * r.reads) / reads[0].reads + "%" }} />
                    </span>
                  </li>
                ))}
              </ul>
              <div className="home-legend">
                <span className="fresh-fresh">fresh</span>
                <span className="fresh-aging">aging</span>
                <span className="fresh-stale">stale</span>
              </div>
            </section>
          )}
        </aside>
      </div>

      {props.tree && (
        <section className="home-sec home-files" aria-label="Browse files" id="home-files">
          {/* No folder feed under it: Recent activity above is that feed. */}
          <FolderListing
            node={{ ...props.tree, name: project.name, path: "" }}
            heatMap={heatMap}
            folders={props.folders}
            hub={false}
            apiBase={apiBase}
            onOpen={props.onOpen}
            onFullHistory={props.onFullHistory}
            onRendered={props.onRendered}
          />
        </section>
      )}
    </div>
  );
}

function AttentionRow({ a, href }: { a: Attention; href: (p: string) => string }) {
  let icon: string, title: ReactNode, sub: string, action: string, cls: string;
  if (a.kind === "conflict") {
    icon = "alert";
    cls = "conflict";
    title = (
      <>
        Sync conflict on <b>{a.original}</b>
      </>
    );
    sub = `A diverged copy from ${a.device || "another device"} was kept beside it`;
    action = "Resolve";
  } else if (a.kind === "comments") {
    icon = "comment";
    cls = "comments";
    title = (
      <>
        {a.count} open comment{a.count === 1 ? "" : "s"} on <b>{nameOf(a.path)}</b>
      </>
    );
    sub = a.path;
    action = "Reply";
  } else {
    icon = "clock";
    cls = "stale";
    title = (
      <>
        <b>{nameOf(a.path)}</b> is read a lot but going stale
      </>
    );
    sub = `${a.reads} reads in 30d · last changed ${agoLabel(a.days)}`;
    action = "Review";
  }
  return (
    <li className={"home-attn-row " + cls}>
      <span className="ha-icon" aria-hidden="true">
        <Icon name={icon} />
      </span>
      <div className="ha-text">
        <span className="ha-title">{title}</span>
        <span className="ha-sub">{sub}</span>
      </div>
      <a className={a.kind === "conflict" ? "pbtn" : "btn"} {...linkProps(href(a.path))}>
        {action}
      </a>
    </li>
  );
}
