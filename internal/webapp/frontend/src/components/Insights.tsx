import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HeatMap, Node } from "../api/types";
import { heatTotal } from "../hooks/useBrowse";

/* ---- insights: the read×write matrix ----
   Every file plotted by how much it is read (30 days, from the heat API)
   against how long since it last changed (from the tree). The hot-but-stale
   quadrant is the danger zone: knowledge people still rely on that nobody
   maintains. Admin/org-owner only — members get the ambient heat dots. */

const HOT_READS = 3; // ≥ this many reads/30d = hot
const STALE_DAYS = 30; // ≥ this many days since last write = stale

interface DeviceHeat {
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
  reads: number;
  agent: number;
  total: number;
  days: number;
  danger: boolean;
}

type Lens = "all" | "human" | "agent";

export function Insights(props: {
  flatFiles: Node[];
  heatMap: HeatMap | null;
  devices: DeviceHeat[] | null;
  onOpenFile: (path: string) => void;
  onOpenFolder: (path: string) => void;
  isFolder: (path: string) => boolean;
}) {
  const [lens, setLens] = useState<Lens>("all");
  const { flatFiles, heatMap, devices } = props;

  const now = Date.now();
  const pts: Pt[] = flatFiles.map((f) => {
    const e = (heatMap && heatMap[f.path]) || {};
    const days = f.time ? Math.max(0, (now - new Date(f.time).getTime()) / 86400000) : 0;
    const reads = lens === "all" ? heatTotal(e) : e[lens] || 0;
    return {
      path: f.path,
      reads,
      agent: e.agent || 0,
      total: heatTotal(e),
      days,
      danger: reads >= HOT_READS && days >= STALE_DAYS,
    };
  });

  return (
    <div className="insights">
      <h1 className="in-title">Knowledge insights</h1>
      <p className="dl-sub">
        Reads over the last 30 days × how long since each file changed. Hot but stale knowledge —
        read a lot, maintained by nobody — is the danger zone.
      </p>
      <div className="in-lens">
        {(["all", "human", "agent"] as const).map((l) => (
          <button
            key={l}
            className={"in-lens-btn" + (l === lens ? " active" : "")}
            onClick={() => setLens(l)}
          >
            {l === "all" ? "All reads" : l === "human" ? "Human reads" : "Agent reads"}
          </button>
        ))}
      </div>

      <h3 className="dl-h3">Map — cell size = reads, color = freshness</h3>
      <Treemap pts={pts} onOpenFile={props.onOpenFile} onOpenFolder={props.onOpenFolder} isFolder={props.isFolder} />

      <h3 className="dl-h3">Reads × freshness</h3>
      <Scatter pts={pts} onOpenFile={props.onOpenFile} />

      <h3 className="dl-h3">Hot path — top files by reads</h3>
      <HotPath pts={pts} lens={lens} onOpenFile={props.onOpenFile} />

      {devices && devices.length > 0 && (
        <>
          <h3 className="dl-h3">Agent coverage — which agents read which areas</h3>
          <CoverageMatrix devices={devices} />
        </>
      )}
    </div>
  );
}

/* Staleness color: fresh green → amber → red over 0..300 days. */
function staleColor(days: number): string {
  const stops = [
    [76, 195, 138],
    [232, 196, 84],
    [224, 93, 93],
  ];
  const t = Math.min(1, Math.max(0, days / 300)) * (stops.length - 1);
  const i = Math.min(stops.length - 2, Math.floor(t)),
    f = t - i;
  const c = stops[i].map((v, k) => Math.round(v + (stops[i + 1][k] - v) * f));
  return `rgb(${c[0]},${c[1]},${c[2]})`;
}

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
    const horiz = w >= h; // row = a strip along the shorter side
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

const TM_HEADER = 15; // group label strip height

function Treemap({
  pts,
  onOpenFile,
  onOpenFolder,
  isFolder,
}: {
  pts: Pt[];
  onOpenFile: (p: string) => void;
  onOpenFolder: (p: string) => void;
  isFolder: (p: string) => boolean;
}) {
  const W = 720,
    H = 480;
  // Two levels: top-level folder groups, files within each.
  const groups = new Map<string, { name: string; files: Pt[]; value: number }>();
  for (const p of pts) {
    const top = p.path.includes("/") ? p.path.split("/")[0] : "/";
    let g = groups.get(top);
    if (!g) groups.set(top, (g = { name: top, files: [], value: 0 }));
    g.files.push(p);
    g.value += p.reads + 1; // +1: unread files still occupy a sliver
  }
  const cells: React.ReactNode[] = [];
  for (const gc of squarify([...groups.values()], 0, 0, W, H)) {
    const g = gc.item;
    const dir = g.name === "/" ? "" : g.name;
    cells.push(
      <rect
        key={"g" + g.name}
        x={gc.x + 1}
        y={gc.y + 1}
        width={Math.max(0, gc.w - 2)}
        height={Math.max(0, gc.h - 2)}
        rx={3}
        className="in-tm-group"
        data-dir={dir}
      />,
    );
    if (gc.w > 46 && gc.h > TM_HEADER + 10) {
      let label = g.name === "/" ? "(root)" : g.name;
      const fit = Math.floor((gc.w - 8) / 6);
      if (label.length > fit) label = label.slice(0, Math.max(1, fit - 1)) + "…";
      cells.push(
        <text key={"gl" + g.name} x={gc.x + 5} y={gc.y + 12} className="in-tm-glabel" data-dir={dir}>
          {label}
        </text>,
      );
    }
    const fcells = squarify(
      g.files.map((f) => ({ ...f, name: f.path.split("/").pop()!, value: f.reads + 1 })),
      gc.x + 2,
      gc.y + TM_HEADER,
      Math.max(0, gc.w - 4),
      Math.max(0, gc.h - TM_HEADER - 2),
    );
    for (const c of fcells) {
      cells.push(
        <rect
          key={c.item.path}
          x={c.x + 0.6}
          y={c.y + 0.6}
          width={Math.max(0.4, c.w - 1.2)}
          height={Math.max(0.4, c.h - 1.2)}
          rx={1.5}
          fill={staleColor(c.item.days)}
          className="in-tm-cell"
          data-path={c.item.path}
        >
          <title>
            {`${c.item.path} — ${c.item.reads} read${c.item.reads === 1 ? "" : "s"}/30d · changed ${Math.round(c.item.days)}d ago`}
          </title>
        </rect>,
      );
      if (c.w > 54 && c.h > 16) {
        const fit = Math.floor((c.w - 8) / 6);
        let label = (c.item.danger ? "⚠ " : "") + c.item.name;
        if (label.length > fit) label = label.slice(0, Math.max(1, fit - 1)) + "…";
        if (fit >= 5)
          cells.push(
            <text key={"l" + c.item.path} x={c.x + 4.5} y={c.y + 12.5} className="in-tm-label" data-path={c.item.path}>
              {label}
            </text>,
          );
      }
    }
  }
  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      className="in-chart in-treemap"
      onClick={(e) => {
        // One delegated click handler for thousands of cells.
        const t = (e.target as Element).closest("[data-path], [data-dir]");
        if (!t) return;
        const path = t.getAttribute("data-path");
        if (path) return onOpenFile(path);
        const dir = t.getAttribute("data-dir");
        if (dir && isFolder(dir)) onOpenFolder(dir);
      }}
    >
      {cells}
    </svg>
  );
}

/* Dependency-free SVG scatter: x = days since last write, y = reads, both
   log-scaled; threshold lines split the quadrants. */
function Scatter({ pts, onOpenFile }: { pts: Pt[]; onOpenFile: (p: string) => void }) {
  const W = 720,
    H = 360,
    M = { l: 44, r: 16, t: 20, b: 34 };
  const maxDays = Math.max(STALE_DAYS * 2, ...pts.map((p) => p.days));
  const maxReads = Math.max(HOT_READS * 2, ...pts.map((p) => p.reads));
  const lx = (d: number) => Math.log10(d + 1) / Math.log10(maxDays + 1);
  const ly = (r: number) => Math.log10(r + 1) / Math.log10(maxReads + 1);
  const X = (d: number) => M.l + lx(d) * (W - M.l - M.r);
  const Y = (r: number) => H - M.b - ly(r) * (H - M.t - M.b);

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="in-chart">
      <rect
        x={X(STALE_DAYS)}
        y={M.t}
        width={W - M.r - X(STALE_DAYS)}
        height={Y(HOT_READS) - M.t}
        className="in-danger-zone"
      />
      <line x1={X(STALE_DAYS)} y1={M.t} x2={X(STALE_DAYS)} y2={H - M.b} className="in-threshold" />
      <line x1={M.l} y1={Y(HOT_READS)} x2={W - M.r} y2={Y(HOT_READS)} className="in-threshold" />
      <line x1={M.l} y1={H - M.b} x2={W - M.r} y2={H - M.b} className="in-axis" />
      <line x1={M.l} y1={M.t} x2={M.l} y2={H - M.b} className="in-axis" />
      <text x={(M.l + W - M.r) / 2} y={H - 8} className="in-label">
        days since last change →
      </text>
      <text
        x={12}
        y={(M.t + H - M.b) / 2}
        className="in-label"
        transform={`rotate(-90 12 ${(M.t + H - M.b) / 2})`}
      >
        reads / 30d →
      </text>
      <text x={W - M.r - 6} y={M.t + 14} className="in-quad in-quad-danger" textAnchor="end">
        hot + stale
      </text>
      <text x={M.l + 6} y={M.t + 14} className="in-quad">
        hot + fresh
      </text>
      <text x={W - M.r - 6} y={H - M.b - 8} className="in-quad" textAnchor="end">
        cold + stale
      </text>
      <text x={W - M.r - 6} y={M.t + 28} className="in-label" textAnchor="end">
        dot size = agent share of reads
      </text>
      {pts.map((p) => {
        // Radius encodes the agent share of the file's reads; translucent
        // dots keep the cloud readable at hundreds of files.
        const share = p.total ? (p.agent || 0) / p.total : 0;
        return (
          <circle
            key={p.path}
            cx={Number(X(p.days).toFixed(1))}
            cy={Number(Y(p.reads).toFixed(1))}
            r={Number((3 + 4 * share).toFixed(1))}
            className={"in-pt" + (p.danger ? " danger" : p.reads ? "" : " cold")}
            onClick={() => onOpenFile(p.path)}
          >
            <title>
              {`${p.path} — ${p.reads} read${p.reads === 1 ? "" : "s"} / 30d · changed ${Math.round(p.days)}d ago`}
            </title>
          </circle>
        );
      })}
    </svg>
  );
}

/* ---- hot path: top-20 files by reads, agent/human split ---- */
function HotPath({
  pts,
  lens,
  onOpenFile,
}: {
  pts: Pt[];
  lens: Lens;
  onOpenFile: (p: string) => void;
}) {
  const top = pts
    .filter((p) => p.reads > 0)
    .sort((a, b) => b.reads - a.reads || b.days - a.days)
    .slice(0, 20);
  if (!top.length) return <div className="dl-empty">No reads in the window yet.</div>;
  const max = top[0].reads;
  return (
    <>
      <div className="in-hotpath">
        {top.map((p) => {
          // Split of the lens reads: pure lenses are single-color by definition.
          const aFrac = lens === "agent" ? 1 : lens === "human" ? 0 : p.total ? p.agent / p.total : 0;
          const pct = (p.reads / max) * 100;
          return (
            <div
              key={p.path}
              className="in-hp-row"
              tabIndex={0}
              role="button"
              title={
                p.danger
                  ? `${p.reads} read${p.reads === 1 ? "" : "s"}/30d · unchanged ${Math.round(p.days)}d — review this file`
                  : p.path
              }
              onClick={() => onOpenFile(p.path)}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  onOpenFile(p.path);
                }
              }}
            >
              <span className={"in-hp-name" + (p.danger ? " danger" : "")}>
                {p.path + (p.danger ? " ⚠" : "")}
              </span>
              <span className="in-hp-bar">
                <span className="in-hp-agent" style={{ width: (pct * aFrac).toFixed(1) + "%" }} />
                <span className="in-hp-human" style={{ width: (pct * (1 - aFrac)).toFixed(1) + "%" }} />
              </span>
              <span className="in-hp-count">{p.reads}</span>
            </div>
          );
        })}
      </div>
      <p className="in-legend">
        <span className="in-sw agent" /> agent reads <span className="in-sw human" /> human reads
      </p>
    </>
  );
}

/* ---- agent coverage matrix: devices × top-level folders ---- */
function CoverageMatrix({ devices }: { devices: DeviceHeat[] }) {
  const totals = new Map<string, number>();
  for (const d of devices) {
    for (const [f, n] of Object.entries(d.folders || {})) totals.set(f, (totals.get(f) || 0) + n);
  }
  const cols = [...totals.entries()]
    .sort((a, b) => b[1] - a[1])
    .slice(0, 12)
    .map((e) => e[0]);
  const rows = devices.slice(0, 12); // server sorts by total desc
  const left = 140,
    top = 6,
    cw = Math.min(76, Math.max(34, (720 - left - 8) / cols.length)),
    ch = 26;
  const W = 720,
    H = top + rows.length * ch + 58;
  const max = Math.max(1, ...rows.flatMap((d) => cols.map((c) => (d.folders || {})[c] || 0)));
  const shade = (t: number) => {
    // #17191f → amber by intensity
    const a = [23, 25, 31],
      b = [245, 166, 35];
    const c = a.map((v, i) => Math.round(v + (b[i] - v) * t));
    return `rgb(${c[0]},${c[1]},${c[2]})`;
  };
  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="in-chart in-matrix">
      {rows.map((d, i) => {
        let label = d.name || d.id || "";
        if (label.length > 20) label = label.slice(0, 19) + "…";
        return (
          <g key={d.id || i}>
            <text x={left - 8} y={top + i * ch + 17} textAnchor="end" className="in-label">
              {label}
            </text>
            {cols.map((c, j) => {
              const v = (d.folders || {})[c] || 0;
              return (
                <rect
                  key={c}
                  x={left + j * cw}
                  y={top + i * ch}
                  width={cw - 4}
                  height={ch - 4}
                  rx={3}
                  fill={shade(Math.sqrt(v / max))}
                >
                  <title>{`${d.name || d.id} × ${c || "(root)"}: ${v} read${v === 1 ? "" : "s"}/30d`}</title>
                </rect>
              );
            })}
          </g>
        );
      })}
      {cols.map((c, j) => {
        const cx = left + j * cw + (cw - 4) / 2,
          cy = top + rows.length * ch + 14;
        return (
          <text
            key={c}
            x={cx}
            y={cy}
            className="in-label"
            textAnchor="end"
            transform={`rotate(-28 ${cx} ${cy})`}
          >
            {c || "(root)"}
          </text>
        );
      })}
    </svg>
  );
}
