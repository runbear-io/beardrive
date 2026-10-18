import { useEffect, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Archive, BellOff, Users } from "lucide-react";
import { getJSON } from "../api/http";
import type { HeatMap, Node } from "../api/types";
import { heatTotal } from "../hooks/useBrowse";
import {
  HEAT_DISCLOSURE,
  HOT_READS,
  STALE_DAYS,
  ageRange,
  ageSpanLabel,
  isFlatRange,
  orphanPaths,
  placeLabels,
} from "../lib/heat";
import { linkProps } from "../nav";

/* ---- Insights: what the team and its agents actually read ----
   Every file joined from two sources: reads over 30 days (the heat API) and
   the file's mtime, size and last editor (the tree). A hot file nobody has
   changed in a while is the danger zone; a file that was emptied and is still
   being read is an anomaly worth restoring. /heat returns counts only, never
   actor identities (reads.go); the coverage matrix names DEVICES, which
   History already shows every member. */

export interface DeviceHeat {
  id?: string;
  name?: string;
  folders?: Record<string, number>;
}

// /heat?by=device breakdown; older servers lack it — the coverage section
// simply doesn't render.
export function useInsightsDevices(apiBase: string, enabled: boolean) {
  const q = useQuery({
    queryKey: ["heatDevices", apiBase],
    queryFn: () =>
      getJSON<{ devices: DeviceHeat[] }>(apiBase + "heat?by=device&days=30"),
    enabled,
    retry: false,
    staleTime: 60_000,
  });
  return q.data?.devices ?? null;
}

interface Pt {
  path: string;
  reads: number; // under the current lens
  agent: number;
  human: number;
  share: number;
  total: number;
  days: number;
  editor: string;
  danger: boolean; // hot + stale under the current thresholds
  empty: boolean; // size 0 and still read
  // A heat row whose path is no longer in the tree: it has reads but no
  // mtime, so it is ranked but never plotted (any position would be invented).
  orphan?: boolean;
}

type Lens = "all" | "human" | "agent" | "share";
type Kind = "agent" | "human" | "share";

const LENS_ORDER = ["all", "human", "agent", "share"] as const;
const LENS_LABEL: Record<Lens, string> = {
  all: "All reads",
  human: "People",
  agent: "Agents",
  share: "Share links",
};
const KIND_LABEL: Record<Kind, string> = { agent: "agents", human: "people", share: "share" };

interface Thresholds {
  hot: number;
  stale: number;
}
/* Versioned so a saved value from an earlier default does not pin a user to
   it: a v1 save equal to that default is dropped, any other v1 save is a
   deliberate choice and carries over. */
const TH_KEY = "bd.insights.thresholds.v2";
const TH_KEY_V1 = "bd.insights.thresholds";
const V1_DEFAULT = { hot: 3, stale: 30 };
function parseThresholds(raw: string | null): Thresholds | null {
  const t = JSON.parse(raw || "null");
  return t && t.hot >= 1 && t.stale >= 1 ? { hot: +t.hot, stale: +t.stale } : null;
}
function loadThresholds(): Thresholds {
  try {
    const t = parseThresholds(localStorage.getItem(TH_KEY));
    if (t) return t;
    const v1 = parseThresholds(localStorage.getItem(TH_KEY_V1));
    localStorage.removeItem(TH_KEY_V1);
    if (v1 && (v1.hot !== V1_DEFAULT.hot || v1.stale !== V1_DEFAULT.stale)) return v1;
  } catch {
    /* storage unavailable or corrupt: defaults */
  }
  return { hot: HOT_READS, stale: STALE_DAYS };
}

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;
const daysLabel = (d: number) => {
  const n = Math.round(d);
  return n === 0 ? "today" : n === 1 ? "1 day ago" : `${n} days ago`;
};
const kindsText = (p: Pt) => `${p.human} people · ${p.agent} agents · ${p.share} share`;
function tip(p: Pt): string {
  const parts = [p.path, `${plural(p.reads, "read")}/30d (${kindsText(p)})`];
  if (p.orphan) parts.push("no longer in the project");
  else parts.push(`changed ${daysLabel(p.days)}${p.editor ? " by " + p.editor : ""}`);
  if (p.empty) parts.push("⛔ empty but still read");
  else if (p.danger) parts.push("⚠ hot + stale");
  return parts.join(" — ");
}

function StatusMark({ p }: { p: { danger: boolean; empty: boolean } }) {
  if (p.empty)
    return (
      <span className="in-flag in-flag-empty" role="img" aria-label="emptied">
        ⛔
      </span>
    );
  if (p.danger)
    return (
      <span className="in-flag in-flag-stale" role="img" aria-label="hot and stale">
        ⚠
      </span>
    );
  return null;
}

/* "Read, but now empty" means EMPTIED: 0 B now and had content before. The
   tree only knows today's size, so a file that was always 0 B (a placeholder,
   an empty robots.txt) looks the same; the hub answers which files are
   emptied in one pass over the whole history (GET history/emptied). */
function useEmptied(apiBase: string | undefined, files: Node[]): Set<string> {
  // Keyed on the tree's own 0-byte files: whatever refreshes the tree — a
  // change frame, a restore or remove here, the tree's fallback poll — also
  // refreshes this whenever a file becomes, or stops being, empty.
  const zero = files
    .filter((f) => !f.size)
    .map((f) => f.path + "@" + (f.time || ""))
    .join("\n");
  const { data } = useQuery({
    queryKey: ["emptied", apiBase, zero],
    queryFn: () => getJSON<{ paths: string[] }>(apiBase + "history/emptied"),
    enabled: !!apiBase && zero !== "",
    staleTime: 60_000,
  });
  return new Set(data?.paths ?? []);
}

export function Insights(props: {
  apiBase?: string; // checks emptied files against their history; omitted, none is flagged
  flatFiles: Node[];
  heatMap: HeatMap | null;
  devices: DeviceHeat[] | null;
  scope?: string; // "" = whole project; a folder scopes to its subtree, a file to itself
  loading?: boolean; // tree not in yet — "this project has no files" would be a lie for a frame
  installHref?: string; // omitted on the project home, which already leads with ConnectGuide
  onOpenFile: (path: string) => void;
  onOpenFolder: (path: string) => void;
  onOpenHistory: (path: string) => void;
  isFolder: (path: string) => boolean;
}) {
  const emptied = useEmptied(props.apiBase, props.flatFiles);
  const [lens, setLens] = useState<Lens>("all");
  const [th, setTh] = useState<Thresholds>(loadThresholds);
  useEffect(() => {
    try {
      localStorage.setItem(TH_KEY, JSON.stringify(th));
    } catch {
      /* not persisted; the control still works for this visit */
    }
  }, [th]);
  const { flatFiles, heatMap, devices, scope } = props;

  const inScope = (p: string) => !scope || p === scope || p.startsWith(scope + "/");
  const scoped = scope ? flatFiles.filter((f) => inScope(f.path)) : flatFiles;

  /* Gate on "no files", never on "no reads": files nobody has read still get
     a map and a self-explaining hot path. */
  if (!props.loading && !scoped.length)
    return (
      <div className="insights">
        <h1 className="in-title">
          Knowledge insights{scope ? <span className="in-scope"> · {scope}</span> : null}
        </h1>
        <div className="dl-empty in-blank">
          <p>{scope ? `Nothing in ${scope} to chart yet.` : "Nothing to chart yet."}</p>
          <p>
            {scope
              ? `No files under ${scope} are syncing here yet.`
              : "This project has no files. Once a device syncs files here, the map, the reads × freshness plot and the hot path fill in on their own."}
          </p>
          {props.installHref && (
            <a className="pbtn" {...linkProps(props.installHref)}>
              Set up a device →
            </a>
          )}
        </div>
      </div>
    );

  const scopedDevices =
    devices && scope
      ? devices
          .map((d) => {
            // Null-prototype bag: the keys are folder names off a peer's
            // journal, and "__proto__" on a plain object hits the prototype
            // setter and stores nothing.
            const folders: Record<string, number> = Object.create(null);
            for (const [f, n] of Object.entries(d.folders || {})) if (inScope(f)) folders[f] = n;
            return { ...d, folders };
          })
          .filter((d) => Object.keys(d.folders).length > 0)
      : devices;

  const now = Date.now();
  const lensReads = (e: { agent?: number; human?: number; share?: number }) =>
    lens === "all" ? heatTotal(e) : e[lens] || 0;
  const pts: Pt[] = scoped.map((f) => {
    const e = (heatMap && heatMap[f.path]) || {};
    const days = f.time ? Math.max(0, (now - new Date(f.time).getTime()) / 86400000) : 0;
    const reads = lensReads(e);
    const empty = emptied.has(f.path) && reads > 0;
    return {
      path: f.path,
      reads,
      agent: e.agent || 0,
      human: e.human || 0,
      share: e.share || 0,
      total: heatTotal(e),
      days,
      editor: f.user_name || f.user || f.author || "",
      empty,
      danger: !empty && reads >= th.hot && days >= th.stale,
    };
  });

  /* Reads whose file left the project. Neither plot can position them (no
     mtime), so the hot path ranks them and the plots carry a count. */
  const orphans: Pt[] = orphanPaths(heatMap, new Set(flatFiles.map((f) => f.path)))
    .filter(inScope)
    .map((path) => {
      const e = heatMap![path];
      return {
        path,
        reads: lensReads(e),
        agent: e.agent || 0,
        human: e.human || 0,
        share: e.share || 0,
        total: heatTotal(e),
        days: 0,
        editor: "",
        danger: false,
        empty: false,
        orphan: true,
      };
    })
    .filter((p) => p.reads > 0);
  const orphanNote =
    orphans.length > 0 ? (
      <p className="in-legend in-orphan-note">
        {plural(orphans.length, "file")} with reads {orphans.length === 1 ? "is" : "are"} no
        longer in the project — see Hot path.
      </p>
    ) : null;

  const kinds: Kind[] = lens === "all" ? ["agent", "human", "share"] : [lens];

  return (
    <div className="insights">
      <div className="in-top">
        <h1 className="in-title">
          What the team and its agents actually read
          {scope ? <span className="in-scope"> · {scope}</span> : null}
        </h1>
        <div className="in-lens" role="group" aria-label="Reader lens">
          {LENS_ORDER.map((l) => (
            <button
              key={l}
              className={"in-lens-btn" + (l === lens ? " active" : "")}
              aria-pressed={l === lens}
              onClick={() => setLens(l)}
            >
              {l !== "all" && <span className={"in-sw " + l} aria-hidden />}
              {LENS_LABEL[l]}
            </button>
          ))}
        </div>
      </div>
      <p className="dl-sub">
        {scope
          ? `Reads over the last 30 days × freshness, for ${scope} and everything in it. ${HEAT_DISCLOSURE}`
          : "Last 30 days · one read = one visit. Hot but stale knowledge — read a lot, maintained by nobody — is the danger zone. " +
            HEAT_DISCLOSURE}
      </p>

      <Kpis pts={pts} orphans={orphans} lens={lens} th={th} />

      <FixFirst
        pts={pts}
        th={th}
        setTh={setTh}
        lens={lens}
        onOpenFile={props.onOpenFile}
        onOpenHistory={props.onOpenHistory}
      />

      <div className="in-split">
        <section className="in-sec in-sec-map">
          <Treemap
            pts={pts}
            scope={scope || ""}
            onOpenFile={props.onOpenFile}
            onOpenFolder={props.onOpenFolder}
            isFolder={props.isFolder}
          />
          {orphanNote}
        </section>
        <section className="in-sec in-sec-hot">
          <h2 className="in-h2">Hot path</h2>
          <p className="in-sub">
            Agents and people live on different files — so each gets its own chart.
          </p>
          <HotPath
            pts={[...pts, ...orphans]}
            kinds={kinds}
            th={th}
            onOpenFile={props.onOpenFile}
            onOpenHistory={props.onOpenHistory}
          />
        </section>
      </div>

      <Scatter pts={pts} th={th} onOpenFile={props.onOpenFile} />
      {orphanNote}

      {scopedDevices && scopedDevices.length > 0 && (
        <CoverageMatrix devices={scopedDevices} lens={lens} />
      )}
    </div>
  );
}

/* ---- section frame: title, caption, and the View as table toggle ---- */
function Frame(props: {
  id: string;
  title: string;
  sub?: ReactNode;
  aside?: ReactNode;
  table: () => ReactNode;
  children: ReactNode;
}) {
  const [asTable, setAsTable] = useState(false);
  return (
    <section className="in-sec" data-sec={props.id}>
      <div className="in-sec-head">
        <div>
          <h2 className="in-h2">{props.title}</h2>
          {props.sub && <p className="in-sub">{props.sub}</p>}
        </div>
        <div className="in-sec-tools">
          {props.aside}
          <button className="in-astable" aria-pressed={asTable} onClick={() => setAsTable(!asTable)}>
            {asTable ? "View as chart" : "View as table"}
          </button>
        </div>
      </div>
      {asTable ? <div className="in-table-wrap">{props.table()}</div> : props.children}
    </section>
  );
}

/* ---- 1. KPI row ---- */
function Kpis({ pts, orphans, lens, th }: { pts: Pt[]; orphans: Pt[]; lens: Lens; th: Thresholds }) {
  const all = [...pts, ...orphans];
  const sum = (k: Kind) => all.reduce((s, p) => s + (lens === "all" || lens === k ? p[k] : 0), 0);
  const split = { human: sum("human"), agent: sum("agent"), share: sum("share") };
  const reads = split.human + split.agent + split.share;
  const readFiles = pts.filter((p) => p.reads > 0).length;
  const pct = pts.length ? Math.round((readFiles / pts.length) * 100) : 0;
  const stale = pts.filter((p) => p.danger).length;
  const empty = pts.filter((p) => p.empty);
  const emptyReads = empty.reduce((s, p) => s + p.reads, 0);
  return (
    <div className="in-kpis">
      <div className="in-kpi" data-kpi="reads">
        <div className="in-kpi-l">Reads</div>
        <div className="in-kpi-v">{reads.toLocaleString("en")}</div>
        <div className="in-kpi-bar" aria-hidden>
          {(["human", "agent", "share"] as Kind[]).map((k) =>
            split[k] ? <span key={k} className={"in-hp-" + k} style={{ flexGrow: split[k] }} /> : null,
          )}
        </div>
        <div className="in-kpi-n">
          {(["human", "agent", "share"] as Kind[]).map((k) => (
            <span key={k} data-kind={k}>
              <span className={"in-sw " + k} aria-hidden /> {split[k].toLocaleString("en")} {KIND_LABEL[k]}
            </span>
          ))}
        </div>
      </div>
      <div className="in-kpi" data-kpi="coverage">
        <div className="in-kpi-l">Files read at least once</div>
        <div className="in-kpi-v">
          {pct}% <small>{readFiles.toLocaleString("en")} of {pts.length.toLocaleString("en")}</small>
        </div>
        <div className="in-kpi-bar in-kpi-meter" aria-hidden>
          <span className="in-kpi-fill" style={{ width: pct + "%" }} />
        </div>
      </div>
      <div className="in-kpi" data-kpi="stale">
        <div className="in-kpi-l">Hot but stale</div>
        <div className="in-kpi-v">
          {stale}
          {stale > 0 && <span className="in-status in-status-warn">⚠ Review</span>}
        </div>
        <div className="in-kpi-n">
          ≥ {plural(th.hot, "read")} and unchanged for {th.stale}+ days
        </div>
      </div>
      <div className="in-kpi" data-kpi="empty">
        <div className="in-kpi-l">Read, but now empty</div>
        <div className="in-kpi-v">
          {empty.length}
          {empty.length > 0 && <span className="in-status in-status-crit">⛔ Restore</span>}
        </div>
        <div className="in-kpi-n">
          {empty.length ? `0-byte files opened ${plural(emptyReads, "time")}` : "no empty file is being read"}
        </div>
      </div>
    </div>
  );
}

/* ---- 2. Fix these first ---- */
function reasonFor(p: Pt): string {
  if (p.empty) return "Emptied, still opened";
  const t = p.total || 1;
  if (p.share / t >= 0.5) return "Mostly read via a public share link";
  if (p.agent / t >= 0.95) return "Only agents read it";
  if (p.agent / t >= 0.6) return "Agents rely on it, nobody updates it";
  if (p.human / t >= 0.95) return "Only people read it";
  return "Read a lot, maintained by nobody";
}

function ThresholdControl({ th, setTh }: { th: Thresholds; setTh: (t: Thresholds) => void }) {
  const num = (v: string, fallback: number) => {
    const n = Math.round(Number(v));
    return Number.isFinite(n) && n >= 1 ? n : fallback;
  };
  return (
    <div className="in-thresholds" role="group" aria-label="Thresholds">
      <label>
        Stale after{" "}
        <input
          type="number"
          min={1}
          aria-label="Stale after days"
          value={th.stale}
          onChange={(e) => setTh({ ...th, stale: num(e.target.value, th.stale) })}
        />{" "}
        days
      </label>
      <span aria-hidden>·</span>
      <label>
        hot ≥{" "}
        <input
          type="number"
          min={1}
          aria-label="Hot at reads"
          value={th.hot}
          onChange={(e) => setTh({ ...th, hot: num(e.target.value, th.hot) })}
        />{" "}
        reads
      </label>
    </div>
  );
}

function KindBar({ p, max, lens }: { p: Pt; max: number; lens: Lens }) {
  const ks: Kind[] = lens === "all" ? ["human", "agent", "share"] : [lens];
  return (
    <span className="in-fx-bar" style={{ width: Math.max(4, (p.reads / max) * 100) + "%" }}>
      {ks.map((k) => (p[k] ? <span key={k} className={"in-hp-" + k} style={{ flexGrow: p[k] }} /> : null))}
    </span>
  );
}

const FIX_MAX = 8;
function FixFirst(props: {
  pts: Pt[];
  th: Thresholds;
  setTh: (t: Thresholds) => void;
  lens: Lens;
  onOpenFile: (p: string) => void;
  onOpenHistory: (p: string) => void;
}) {
  const rows = props.pts
    .filter((p) => p.danger || p.empty)
    .sort(
      (a, b) =>
        Number(b.empty) - Number(a.empty) ||
        b.reads * Math.max(1, b.days) - a.reads * Math.max(1, a.days),
    );
  const shown = rows.slice(0, FIX_MAX);
  const max = Math.max(1, ...shown.map((p) => p.reads));
  return (
    <section className="in-sec in-fix" data-sec="fix">
      <div className="in-sec-head">
        <div>
          <h2 className="in-h2">Fix these first</h2>
          <p className="in-sub">
            Files people or agents keep relying on that are emptied or no longer maintained. Ranked
            by reads × days since last change.
          </p>
        </div>
        <ThresholdControl th={props.th} setTh={props.setTh} />
      </div>
      {shown.length === 0 ? (
        <div className="in-fix-none">Nothing hot and stale, and no empty file is being read.</div>
      ) : (
        <table className="in-fix-table">
          <thead>
            <tr>
              <th>File</th>
              <th>Reads · 30d</th>
              <th>Last change</th>
              <th>Why it's here</th>
              <th>
                <span className="sr-only">Action</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {shown.map((p) => {
              const slash = p.path.lastIndexOf("/");
              return (
                <tr key={p.path} className="in-fix-row" data-path={p.path} data-kind={p.empty ? "empty" : "stale"} title={tip(p)}>
                  <td className="in-fx-file">
                    <StatusMark p={p} />
                    <span className="in-fx-name">
                      <span>{p.path.slice(slash + 1)}</span>
                      {slash > 0 && <small>{p.path.slice(0, slash + 1)}</small>}
                    </span>
                  </td>
                  <td className="in-fx-reads">
                    <span className="in-fx-track">
                      <KindBar p={p} max={max} lens={props.lens} />
                    </span>
                    <span className="in-fx-n">{p.reads}</span>
                  </td>
                  <td className={"in-fx-when" + (p.empty ? " empty" : "")}>
                    {daysLabel(p.days)}
                    {p.empty ? " → 0 B" : ""}
                  </td>
                  <td className="in-fx-why">{reasonFor(p)}</td>
                  <td className="in-fx-act">
                    {p.empty ? (
                      <button className="pbtn in-fx-restore" onClick={() => props.onOpenHistory(p.path)}>
                        Restore
                      </button>
                    ) : (
                      <button className="in-fx-review" onClick={() => props.onOpenFile(p.path)}>
                        Review
                      </button>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
      {rows.length > shown.length && (
        <p className="in-legend">+ {rows.length - shown.length} more below the cut</p>
      )}
    </section>
  );
}

/* ---- 3. Treemap ---- */

/* Squarified treemap (Bruls et al.), dependency-free: items sorted by value
   fill a rect in rows along the shorter side, keeping cells near-square. */
interface TmItem {
  value: number;
}
function squarify<T extends TmItem>(
  items: T[],
  x: number,
  y: number,
  w: number,
  h: number,
): Array<{ item: T; x: number; y: number; w: number; h: number }> {
  const total = items.reduce((s, it) => s + it.value, 0);
  if (!total || w <= 0 || h <= 0) return [];
  const rest = items
    .slice()
    .sort((a, b) => b.value - a.value)
    .map((it) => ({ it, a: (it.value / total) * w * h }));
  const worst = (row: Array<{ a: number }>, side: number) => {
    const sum = row.reduce((t, r) => t + r.a, 0);
    const d = sum / side;
    let m = 0;
    for (const r of row) {
      const l = r.a / d;
      m = Math.max(m, l / d, d / l);
    }
    return m;
  };
  const out: Array<{ item: T; x: number; y: number; w: number; h: number }> = [];
  while (rest.length) {
    const horiz = w >= h;
    const side = horiz ? h : w;
    const row = [rest.shift()!];
    while (rest.length && worst(row.concat(rest[0]), side) <= worst(row, side)) {
      row.push(rest.shift()!);
    }
    const d = row.reduce((t, r) => t + r.a, 0) / side;
    let off = 0;
    for (const r of row) {
      const l = r.a / d;
      if (horiz) out.push({ item: r.it, x, y: y + off, w: d, h: l });
      else out.push({ item: r.it, x: x + off, y, w: l, h: d });
      off += l;
    }
    if (horiz) {
      x += d;
      w -= d;
    } else {
      y += d;
      h -= d;
    }
  }
  return out;
}

/* Freshness bin edges (upper bounds of bins 1–3, in days). A project whose
   oldest file is past 60 days gets the fixed ≤3 / 4–14 / 15–30 / >30 edges;
   a younger one splits its own range, so a 49-day project is not painted one
   colour. */
export function freshEdges(maxDays: number): [number, number, number] {
  if (maxDays >= 60) return [3, 14, 30];
  const m = Math.max(4, maxDays);
  const a = Math.max(1, Math.round(m * 0.1));
  const b = Math.max(a + 1, Math.round(m * 0.3));
  const c = Math.max(b + 1, Math.round(m * 0.6));
  return [a, b, c];
}
const binOf = (d: number, e: [number, number, number]) => (d <= e[0] ? 1 : d <= e[1] ? 2 : d <= e[2] ? 3 : 4);

function median(xs: number[]): number {
  const s = [...xs].sort((a, b) => a - b);
  return s.length ? s[Math.floor(s.length / 2)] : 0;
}

interface Cell {
  key: string; // folder path ("" = root) or file path
  label: string;
  file: boolean;
  value: number;
  reads: number;
  days: number;
  files: Pt[];
  danger: boolean;
  empty: boolean;
}

/* The folder a file is drawn under at the default level: its directory cut
   to two segments below `base`. */
function groupKey(path: string, base: string): string {
  const depth = base ? base.split("/").length : 0;
  return path.split("/").slice(0, -1).slice(0, depth + 2).join("/");
}

function folderCells(files: Pt[], base: string): Cell[] {
  const groups = new Map<string, Pt[]>();
  for (const p of files) {
    const key = groupKey(p.path, base);
    const g = groups.get(key);
    if (g) g.push(p);
    else groups.set(key, [p]);
  }
  return [...groups.entries()].map(([key, fs]) => {
    const read = fs.filter((p) => p.reads > 0);
    return {
      key,
      label: !key ? "(root)" : key === base ? "(files here)" : key.slice(base ? base.length + 1 : 0),
      file: false,
      value: fs.reduce((s, p) => s + p.reads + 1, 0),
      reads: read.reduce((s, p) => s + p.reads, 0),
      days: median((read.length ? read : fs).map((p) => p.days)),
      files: fs,
      danger: fs.some((p) => p.danger),
      empty: fs.some((p) => p.empty),
    };
  });
}

function fileCells(files: Pt[], base: string): Cell[] {
  return files.map((p) => ({
    key: p.path,
    label: base ? p.path.slice(base.length + 1) : p.path,
    file: true,
    value: p.reads + 1,
    reads: p.reads,
    days: p.days,
    files: [p],
    danger: p.danger,
    empty: p.empty,
  }));
}

function cellTip(c: Cell): string {
  if (c.file) return tip(c.files[0]);
  const k = (key: Kind) => c.files.reduce((s, p) => s + p[key], 0);
  const flags = [
    c.danger ? `⚠ ${c.files.filter((p) => p.danger).length} hot + stale` : "",
    c.empty ? `⛔ ${c.files.filter((p) => p.empty).length} emptied` : "",
  ].filter(Boolean);
  return [
    `${c.key || "(root)"}/`,
    `${plural(c.reads, "read")}/30d (${k("human")} people · ${k("agent")} agents · ${k("share")} share)`,
    `${plural(c.files.length, "file")} · read files changed ~${daysLabel(c.days)}`,
    ...flags,
  ].join(" — ");
}

const TM_W = 600,
  TM_H = 420;

const TM_OUTLINES = 8;

function Treemap({
  pts,
  scope,
  onOpenFile,
  onOpenFolder,
  isFolder,
}: {
  pts: Pt[];
  scope: string;
  onOpenFile: (p: string) => void;
  onOpenFolder: (p: string) => void;
  isFolder: (p: string) => boolean;
}) {
  const [drill, setDrill] = useState<string | null>(null);
  useEffect(() => setDrill(null), [scope]);
  const scopeDir = scope && isFolder(scope) ? scope : "";
  // `pts` is every file in scope, read or not, so the range is per-scope and
  // independent of the lens.
  const range = ageRange(pts.map((p) => p.days));
  const flat = !!range && isFlatRange(range.min, range.max);
  const edges = freshEdges(range ? range.max : 0);

  const inDrill = drill === null ? pts : pts.filter((p) => groupKey(p.path, scopeDir) === drill);
  const cells =
    drill === null ? folderCells(pts, scopeDir) : fileCells(inDrill, drill || scopeDir);
  const placed = squarify(cells, 0, 0, TM_W, TM_H);
  // Only the worst few hot+stale cells get the outline; when every cell is
  // outlined none of them stands out.
  const outlined = new Set(
    cells
      .filter((c) => c.danger && !c.empty)
      .sort((a, b) => b.reads * b.days - a.reads * a.days)
      .slice(0, TM_OUTLINES)
      .map((c) => c.key),
  );

  const crumbs = (
    <nav className="in-tm-crumbs" aria-label="Treemap level">
      <button className={drill === null ? "cur" : ""} onClick={() => setDrill(null)}>
        {scopeDir || "All folders"}
      </button>
      {drill !== null && (
        <>
          <span aria-hidden>›</span>
          <span className="cur">{drill || "(root)"}</span>
          {drill && isFolder(drill) && (
            <button className="in-tm-open" onClick={() => onOpenFolder(drill)}>
              Open folder
            </button>
          )}
        </>
      )}
      {drill === null && <span className="in-tm-hint">click a folder to see its files</span>}
    </nav>
  );

  const table = () => (
    <table className="in-data-table">
      <thead>
        <tr>
          <th>{drill === null ? "Folder" : "File"}</th>
          <th>Reads</th>
          <th>Files</th>
          <th>Last change</th>
          <th>Flag</th>
        </tr>
      </thead>
      <tbody>
        {[...cells]
          .sort((a, b) => b.reads - a.reads)
          .map((c) => (
            <tr key={c.key}>
              <td>{c.label}</td>
              <td>{c.reads}</td>
              <td>{c.files.length}</td>
              <td>{daysLabel(c.days)}</td>
              <td>{c.empty ? "⛔ emptied" : c.danger ? "⚠ hot + stale" : ""}</td>
            </tr>
          ))}
      </tbody>
    </table>
  );

  return (
    <Frame
      id="treemap"
      title="Where reads go"
      sub="Area = reads · colour = how recently the read files changed"
      table={table}
    >
      {crumbs}
      <svg
        viewBox={`0 0 ${TM_W} ${TM_H}`}
        className="in-chart in-treemap"
        style={{ maxWidth: TM_W }}
        data-level={drill === null ? "folder" : "file"}
        onClick={(e) => {
          const t = (e.target as Element).closest("[data-path], [data-dir]");
          if (!t) return;
          const path = t.getAttribute("data-path");
          if (path !== null) return onOpenFile(path);
          const dir = t.getAttribute("data-dir");
          if (dir !== null) setDrill(dir);
        }}
      >
        {placed.map(({ item: c, x, y, w, h }) => {
          const bin = binOf(c.days, edges);
          const cls =
            "in-tm-cell " +
            (flat ? "in-tm-flat" : "in-tm-b" + bin) +
            (c.empty ? " flag-empty" : outlined.has(c.key) ? " flag-stale" : "");
          const glyph = c.empty ? "⛔ " : outlined.has(c.key) ? "⚠ " : "";
          const fit = Math.floor((w - 10) / 6.2);
          const name = glyph + c.label;
          const label = name.length > fit ? name.slice(0, Math.max(1, fit - 1)) + "…" : name;
          const attr = c.file ? { "data-path": c.key } : { "data-dir": c.key };
          return (
            <g key={c.key}>
              <rect
                x={x + 1}
                y={y + 1}
                width={Math.max(0.4, w - 2)}
                height={Math.max(0.4, h - 2)}
                rx={3}
                className={cls}
                data-bin={flat ? 0 : bin}
                {...attr}
              >
                <title>{cellTip(c)}</title>
              </rect>
              {w > 46 && h > 22 && fit >= 4 && (
                <text
                  x={x + 7}
                  y={y + 16}
                  className={"in-tm-label" + (!flat && bin >= 3 ? " on-dark" : "")}
                  {...attr}
                >
                  {label}
                  {h > 36 && (
                    <tspan x={x + 7} dy={13} className="in-tm-sub">
                      {c.reads}
                      {c.file ? "" : ` · ${plural(c.files.length, "file")}`}
                    </tspan>
                  )}
                </text>
              )}
            </g>
          );
        })}
      </svg>
      <FreshnessLegend range={range} flat={flat} edges={edges} />
    </Frame>
  );
}

function FreshnessLegend({
  range: r,
  flat,
  edges,
}: {
  range: { min: number; max: number } | null;
  flat: boolean;
  edges: [number, number, number];
}) {
  if (!r) return null;
  const span = ageSpanLabel(r.min, r.max);
  const labels = [
    `≤ ${edges[0]}d`,
    `${edges[0] + 1}–${edges[1]}d`,
    `${edges[1] + 1}–${edges[2]}d`,
    `> ${edges[2]}d`,
  ];
  return (
    <p className="in-legend in-tm-legend">
      Last change
      <span className={"in-sw-age" + (flat ? " in-sw-flat" : "")}>
        {labels.map((l, i) => (
          <span key={l} className="in-tm-key">
            <span className={"in-sw in-tm-b" + (i + 1)} aria-hidden /> {l}
          </span>
        ))}
      </span>
      <span className="in-tm-key">
        <span className="in-flag in-flag-empty" aria-hidden>⛔</span> emptied
      </span>
      <span className="in-tm-key">
        <span className="in-flag in-flag-stale" aria-hidden>⚠</span> hot + stale (top {TM_OUTLINES})
      </span>
      <span className="in-tm-range">
        {flat
          ? `all files here: ${span} old — colour off, not enough range to rank`
          : `observed: ${span} old — bins fit this range`}
      </span>
    </p>
  );
}

/* ---- 4. Hot path: one single-series chart per reader kind ---- */
const HOT_TITLE: Record<Kind, string> = {
  agent: "What agents load",
  human: "What people open",
  share: "What share links serve",
};
const HP_MAX = 8;

function HotPath({
  pts,
  kinds,
  th,
  onOpenFile,
  onOpenHistory,
}: {
  pts: Pt[];
  kinds: Kind[];
  th: Thresholds;
  onOpenFile: (p: string) => void;
  onOpenHistory: (p: string) => void;
}) {
  const lists = kinds
    .map((k) => ({
      k,
      total: pts.reduce((s, p) => s + p[k], 0),
      top: pts
        .filter((p) => p[k] > 0)
        .sort((a, b) => b[k] - a[k] || b.days - a.days)
        .slice(0, HP_MAX),
    }))
    // Share links are a minority reader: their chart appears only when any exist.
    .filter((l) => l.top.length > 0 || (kinds.length > 1 && l.k !== "share"));
  if (!lists.some((l) => l.top.length)) return <div className="dl-empty">No reads in the window yet.</div>;
  return (
    <>
      {lists.map(({ k, total, top }) => (
        <HotList key={k} k={k} total={total} top={top} th={th} onOpenFile={onOpenFile} onOpenHistory={onOpenHistory} />
      ))}
    </>
  );
}

function HotList(props: {
  k: Kind;
  total: number;
  top: Pt[];
  th: Thresholds;
  onOpenFile: (p: string) => void;
  onOpenHistory: (p: string) => void;
}) {
  const { k, top } = props;
  const max = top.length ? top[0][k] : 1;
  const table = () => (
    <table className="in-data-table">
      <thead>
        <tr>
          <th>File</th>
          <th>{KIND_LABEL[k]} reads</th>
          <th>Last change</th>
        </tr>
      </thead>
      <tbody>
        {top.map((p) => (
          <tr key={p.path}>
            <td>{p.path}</td>
            <td>{p[k]}</td>
            <td>{p.orphan ? "deleted" : daysLabel(p.days)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
  return (
    <Frame
      id={"hot-" + k}
      title={HOT_TITLE[k]}
      aside={<span className="in-hp-total">{plural(props.total, "read")}</span>}
      table={table}
    >
      <div className={"in-hotpath in-hp-" + k + "-list"} data-kind={k}>
        <span className={"in-sw " + k} aria-hidden />
        {top.length === 0 && <div className="in-hp-none">No {KIND_LABEL[k]} reads in the window.</div>}
        {top.map((p) => {
          // The file view would land on not-found for a path that left the
          // project; History still has the content.
          const open = () => (p.orphan ? props.onOpenHistory(p.path) : props.onOpenFile(p.path));
          const flagged = { danger: p[k] >= props.th.hot && p.danger, empty: p.empty };
          return (
            <div
              key={p.path}
              className="in-hp-row"
              tabIndex={0}
              role="button"
              title={tip(p)}
              onClick={open}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  open();
                }
              }}
            >
              <span className="in-hp-line">
                <span className={"in-hp-name" + (p.danger || p.empty ? " danger" : "")}>
                  <StatusMark p={{ danger: p.danger || flagged.danger, empty: p.empty }} />
                  {p.path}
                </span>
                {p.orphan && <span className="in-hp-gone">· no longer in the project</span>}
                <span className="in-hp-count">{p[k]}</span>
              </span>
              <span className="in-hp-bar">
                <span className={"in-hp-" + k} style={{ width: ((p[k] / max) * 100).toFixed(1) + "%" }} />
              </span>
            </div>
          );
        })}
      </div>
    </Frame>
  );
}

/* ---- 5. Scatter, emphasis form ---- */
function Scatter({ pts, th, onOpenFile }: { pts: Pt[]; th: Thresholds; onOpenFile: (p: string) => void }) {
  const W = 960,
    H = 340,
    M = { l: 44, r: 16, t: 20, b: 34 };
  const read = pts.filter((p) => p.reads > 0);
  const maxDays = Math.max(th.stale * 1.5, ...read.map((p) => p.days), 1);
  const maxReads = Math.max(th.hot * 3, ...read.map((p) => p.reads));
  const rMax = 5;
  // x linear and fitted to the observed range; y log, since reads are long-tailed.
  const X = (d: number) => M.l + rMax + (d / maxDays) * (W - M.l - M.r - 2 * rMax);
  const Y = (r: number) =>
    H - M.b - rMax - (Math.log10(r + 1) / Math.log10(maxReads + 1)) * (H - M.t - M.b - 2 * rMax);
  const flagged = read.filter((p) => p.danger || p.empty);
  const dangerN = read.filter((p) => p.danger).length;
  const labels = placeLabels(
    flagged.map((p) => ({ path: p.path, reads: p.reads, cx: X(p.days), cy: Y(p.reads), r: 4.5 })),
    { right: W - M.r, top: M.t + 8, bottom: H - M.b - 4 },
  );
  const xTicks = [0, 0.25, 0.5, 0.75, 1].map((f) => Math.round(f * maxDays));
  const yTicks = [1, 3, 10, 30, 100, 300, 1000, 3000].filter((t) => t <= maxReads);
  const table = () => (
    <table className="in-data-table">
      <thead>
        <tr>
          <th>File</th>
          <th>Reads</th>
          <th>Days since change</th>
          <th>Verdict</th>
        </tr>
      </thead>
      <tbody>
        {[...read]
          .sort((a, b) => Number(b.empty) - Number(a.empty) || Number(b.danger) - Number(a.danger) || b.reads - a.reads)
          .map((p) => (
            <tr key={p.path}>
              <td>{p.path}</td>
              <td>{p.reads}</td>
              <td>{Math.round(p.days)}</td>
              <td>{p.empty ? "⛔ emptied" : p.danger ? "⚠ hot + stale" : ""}</td>
            </tr>
          ))}
      </tbody>
    </table>
  );
  return (
    <Frame
      id="scatter"
      title="Reads × days since last change"
      sub={`One dot per file read in the window (${read.length}). Grey = fine; only the files that need you are coloured and named.`}
      aside={<span className="in-cap">Hover a dot for path, reads by type, last editor</span>}
      table={table}
    >
      <svg viewBox={`0 0 ${W} ${H}`} className="in-chart in-scatter" style={{ maxWidth: W }}>
        <rect
          x={X(th.stale)}
          y={M.t}
          width={Math.max(0, W - M.r - X(th.stale))}
          height={Math.max(0, Y(th.hot) - M.t)}
          className="in-danger-zone"
        />
        {xTicks.map((t) => (
          <text key={"x" + t} x={X(t)} y={H - M.b + 14} className="in-tick" textAnchor="middle">
            {t}d
          </text>
        ))}
        {yTicks.map((t) => (
          <text key={"y" + t} x={M.l - 6} y={Y(t) + 3} className="in-tick" textAnchor="end">
            {t}
          </text>
        ))}
        <line x1={X(th.stale)} y1={M.t} x2={X(th.stale)} y2={H - M.b} className="in-threshold" data-th="stale" />
        <line x1={M.l} y1={Y(th.hot)} x2={W - M.r} y2={Y(th.hot)} className="in-threshold" data-th="hot" />
        <line x1={M.l} y1={H - M.b} x2={W - M.r} y2={H - M.b} className="in-axis" />
        <line x1={M.l} y1={M.t} x2={M.l} y2={H - M.b} className="in-axis" />
        <text x={W - M.r} y={H - 6} className="in-label" textAnchor="end">
          days since last change →
        </text>
        <text x={12} y={(M.t + H - M.b) / 2} className="in-label" transform={`rotate(-90 12 ${(M.t + H - M.b) / 2})`}>
          reads / 30d (log) →
        </text>
        <text x={X(th.stale) + 6} y={M.t + 14} className="in-quad in-quad-danger">
          hot + stale
        </text>
        <text x={X(th.stale) + 92} y={M.t + 14} className="in-quad-count">
          · {dangerN}
        </text>
        <text x={M.l + 6} y={M.t + 14} className="in-quad">
          hot + fresh
        </text>
        <text x={W - M.r - 6} y={H - M.b - 8} className="in-quad" textAnchor="end">
          cold + stale
        </text>
        <text x={M.l + 6} y={H - M.b - 8} className="in-quad">
          cold + fresh
        </text>
        {read.map((p) => (
          <circle
            key={p.path}
            cx={Number(X(p.days).toFixed(1))}
            cy={Number(Y(p.reads).toFixed(1))}
            r={p.danger || p.empty ? 4.5 : 3.5}
            className={"in-pt" + (p.empty ? " empty" : p.danger ? " danger" : "")}
            onClick={() => onOpenFile(p.path)}
          >
            <title>{tip(p)}</title>
          </circle>
        ))}
        {labels.map((l) => (
          <text
            key={l.path}
            x={Number(l.x.toFixed(1))}
            y={Number(l.y.toFixed(1))}
            textAnchor={l.anchor}
            className="in-pt-label"
          >
            {l.name}
          </text>
        ))}
      </svg>
      <p className="in-legend">
        <span className="in-sw in-sw-other" aria-hidden /> other files{" "}
        <span className="in-sw in-sw-stale" aria-hidden /> ⚠ hot + stale{" "}
        <span className="in-sw in-sw-empty" aria-hidden /> ⛔ emptied
      </p>
    </Frame>
  );
}

/* ---- 6. Coverage: which agents read which folders ---- */
const covBin = (v: number) => (v <= 0 ? 0 : v <= 5 ? 1 : v <= 20 ? 2 : v <= 50 ? 3 : 4);

export function coverageTakeaways(
  rows: Array<{ name: string; total: number }>,
  cols: Array<{ name: string; total: number }>,
): Array<{ icon: string; head: string; body: string }> {
  const out: Array<{ icon: string; head: string; body: string }> = [];
  const sum = rows.reduce((s, r) => s + r.total, 0);
  if (!sum) return out;
  if (rows.length >= 3) {
    const top2 = rows[0].total + rows[1].total;
    out.push({
      icon: "users",
      head: `2 devices do ${Math.round((top2 / sum) * 100)}% of agent reading`,
      body: `${rows[0].name} and ${rows[1].name}: their agents' context is the team's de-facto canon.`,
    });
  }
  const low = rows.filter((r) => r.total < rows[0].total * 0.1);
  if (low.length)
    out.push({
      icon: "off",
      head: `${plural(low.length, "device")} barely load${low.length === 1 ? "s" : ""} team context`,
      body: `Under a tenth of the busiest device's agent reads — check that the BearDrive hooks are installed there.`,
    });
  if (cols.length >= 2) {
    const last = cols[cols.length - 1];
    out.push({
      icon: "archive",
      head: `${last.name || "(root)"}${last.name ? "/" : ""} is the least-read folder by agents`,
      body: `${plural(last.total, "read")} in 30 days — a candidate to exclude from agent search, or to point agents at.`,
    });
  }
  return out.slice(0, 3);
}

function TakeawayIcon({ name }: { name: string }) {
  const I = name === "users" ? Users : name === "off" ? BellOff : Archive;
  return <I className="in-tk-ico" aria-hidden size={15} />;
}

function CoverageMatrix({ devices, lens }: { devices: DeviceHeat[]; lens: Lens }) {
  const totals = new Map<string, number>();
  for (const d of devices) {
    for (const [f, n] of Object.entries(d.folders || {})) totals.set(f, (totals.get(f) || 0) + n);
  }
  const cols = [...totals.entries()]
    .sort((a, b) => b[1] - a[1])
    .slice(0, 10)
    .map(([name, total]) => ({ name, total }));
  const val = (d: DeviceHeat, c: string) =>
    d.folders && Object.prototype.hasOwnProperty.call(d.folders, c) ? d.folders[c] : 0;
  const rows = devices
    .map((d) => ({
      d,
      name: d.name || d.id || "",
      total: Object.values(d.folders || {}).reduce((s, n) => s + n, 0),
    }))
    .sort((a, b) => b.total - a.total)
    .slice(0, 12);
  const takeaways = coverageTakeaways(rows, cols);
  const offLens = lens === "human" || lens === "share";
  const table = () => (
    <table className="in-data-table">
      <thead>
        <tr>
          <th>Device</th>
          {cols.map((c) => (
            <th key={c.name}>{c.name || "(root)"}</th>
          ))}
          <th>Total</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => (
          <tr key={r.d.id || r.name}>
            <td>{r.name}</td>
            {cols.map((c) => (
              <td key={c.name}>{val(r.d, c.name)}</td>
            ))}
            <td>{r.total}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
  // The matrix counts agent reads only; under another lens neither the chart
  // nor its table may show it.
  const offNote = (
    <p className="in-legend in-cov-off">
      This matrix counts agent reads only — the {LENS_LABEL[lens]} lens has nothing to show here.
    </p>
  );
  return (
    <Frame
      id="coverage"
      title="Which agents read which folders"
      sub="Agent reads per device × top-level folder · folders sorted by total agent reads"
      table={offLens ? () => offNote : table}
    >
      {offLens && offNote}
      {!offLens && (
        <div className="in-cov">
          <div className="in-matrix-wrap">
            <table className="in-matrix">
              <thead>
                <tr>
                  <th />
                  {cols.map((c) => (
                    <th key={c.name} scope="col">
                      {c.name || "(root)"}
                    </th>
                  ))}
                  <th scope="col" className="in-cv-total">
                    total
                  </th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.d.id || r.name}>
                    <th scope="row" title={r.name}>
                      {r.name}
                    </th>
                    {cols.map((c) => {
                      const v = val(r.d, c.name);
                      return (
                        <td key={c.name} className={"in-cv in-cv-" + covBin(v)} title={`${r.name} × ${c.name || "(root)"}: ${plural(v, "agent read")}/30d`}>
                          {v || "·"}
                        </td>
                      );
                    })}
                    <td className="in-cv-total">{r.total}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <p className="in-legend in-cv-legend">
              Agent reads{" "}
              {["0", "1–5", "6–20", "21–50", "50+"].map((l, i) => (
                <span key={l} className="in-tm-key">
                  <span className={"in-sw in-cv-" + i} aria-hidden /> {l}
                </span>
              ))}
            </p>
          </div>
          {takeaways.length > 0 && (
            <aside className="in-takeaways">
              <h3>What this tells you</h3>
              {takeaways.map((t) => (
                <div key={t.head} className="in-takeaway">
                  <TakeawayIcon name={t.icon} />
                  <div>
                    <b>{t.head}</b>
                    <p>{t.body}</p>
                  </div>
                </div>
              ))}
            </aside>
          )}
        </div>
      )}
    </Frame>
  );
}
