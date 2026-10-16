import { useEffect, useMemo, useState, useSyncExternalStore } from "react";
import type { FrontmatterPair, HeatEntry, RenderDoc } from "../api/types";
import { HEAT_DISCLOSURE, agoLabel, daysSince, heatTotal, staleNote } from "../lib/heat";
import { FM_RAIL, humanSize, whoChanged } from "../util";
import { Icon } from "./shell";

/* The file page's document header and right rail (markdown files).

   Everything here is built from data the page already holds: the render
   response (provenance, size, frontmatter, the HTML the TOC is read from) and
   the project's 30-day heat map. The heat API answers per path with totals
   only — no per-day series and no reader identities — so the reads card shows
   the split and the distinct-reader count, and nothing it would have to
   invent. */

export type TocItem = { id: string; text: string; level: number };

// h2/h3 only: the h1 is the page title, and h4+ is too fine for a rail.
export function tocFrom(html: string): TocItem[] {
  if (!html) return [];
  const doc = new DOMParser().parseFromString(html, "text/html");
  const out: TocItem[] = [];
  for (const h of doc.querySelectorAll("h2[id], h3[id]")) {
    const text = h.textContent?.trim() ?? "";
    if (text) out.push({ id: h.id, text, level: h.tagName === "H2" ? 2 : 3 });
  }
  return out;
}

function fm(pairs: FrontmatterPair[] | undefined, ...keys: string[]): string {
  for (const k of keys) {
    const hit = pairs?.find((p) => p.key.toLowerCase() === k);
    if (hit && !hit.code && hit.value.trim()) return hit.value.trim();
  }
  return "";
}

const useWide = () =>
  useSyncExternalStore(
    (cb) => {
      const mq = window.matchMedia(FM_RAIL);
      mq.addEventListener("change", cb);
      return () => mq.removeEventListener("change", cb);
    },
    () => window.matchMedia(FM_RAIL).matches,
  );

export function DocHeader({ doc, heat }: { doc: RenderDoc; heat: HeatEntry | null }) {
  const type = fm(doc.frontmatter, "type");
  const okf = fm(doc.frontmatter, "okf_version", "okf-version", "okf");
  const reads = heat ? heatTotal(heat) : 0;
  const stale = staleNote(heat, doc.time);
  const days = daysSince(doc.time);
  const who = doc.user_name || doc.user || doc.author ? whoChanged(doc) : "";
  // The display name alone reads as a person; the full identity rides along
  // as hover text for whoever needs the address.
  const short = doc.user_name || doc.user || doc.author || "";
  return (
    <header className="fv-head" data-bd-ignore="">
      <div className="fv-chips">
        {type && (
          <span className="fv-chip accent">
            <Icon name="shield" /> {type}
          </span>
        )}
        {okf && (
          <span className="fv-chip">
            <Icon name="tag" /> OKF v{okf.replace(/^v/i, "")}
          </span>
        )}
        <span className="fv-chip">
          <Icon name="doc" /> Markdown · {humanSize(doc.size)}
        </span>
      </div>
      <div className="fv-meta">
        {stale && (
          <span className="meta-stale" title={stale}>
            <span aria-hidden="true">⚠ </span>
            {stale}
          </span>
        )}
        {who && (
          <span className="fv-who" title={who}>
            <span className="fv-avatar" aria-hidden="true">
              {short.trim()[0]?.toUpperCase()}
            </span>
            {short} edited{days !== null ? " " + (days < 1 ? "today" : agoLabel(days)) : ""}
          </span>
        )}
        {doc.device && (
          <span>
            <Icon name="laptop" /> {doc.device}
          </span>
        )}
        {doc.time && (
          <span>
            <Icon name="calendar" /> {new Date(doc.time).toLocaleString()}
          </span>
        )}
        {reads > 0 && (
          <span className="fv-reads" title={HEAT_DISCLOSURE}>
            <Icon name="eye" /> {reads} {reads === 1 ? "read" : "reads"} / 30d
            <span className="sr-only"> — {HEAT_DISCLOSURE}</span>
          </span>
        )}
      </div>
    </header>
  );
}

export function FileRail(props: {
  html: string;
  frontmatter?: FrontmatterPair[];
  heat: HeatEntry | null;
  insightsHref?: string;
}) {
  const { html, frontmatter, heat, insightsHref } = props;
  const toc = useMemo(() => tocFrom(html), [html]);
  const wide = useWide();
  const [open, setOpen] = useState(false);
  const active = useActiveHeading(toc);
  const reads = heat ? heatTotal(heat) : 0;

  if (!toc.length && !frontmatter?.length && !reads) return null;

  const body = (
    <>
      {toc.length > 0 && (
        <section className="fv-sec fv-toc" aria-label="On this page">
          <h4>On this page</h4>
          <ul>
            {toc.map((t) => (
              <li key={t.id} className={(t.level === 3 ? "sub" : "") + (t.id === active ? " on" : "")}>
                <a
                  href={"#" + t.id}
                  aria-current={t.id === active ? "location" : undefined}
                  onClick={(e) => {
                    e.preventDefault();
                    document.getElementById(t.id)?.scrollIntoView({ block: "start", behavior: "smooth" });
                  }}
                >
                  {t.text}
                </a>
              </li>
            ))}
          </ul>
        </section>
      )}
      {heat && reads > 0 && <ReadsCard heat={heat} insightsHref={insightsHref} />}
      {frontmatter?.length ? (
        <section className="fv-sec fv-props" aria-label="Properties">
          <h4>Properties</h4>
          <dl>
            {frontmatter.map((p) => (
              <div key={p.key}>
                <dt>{p.key}</dt>
                <dd>{p.code ? <code>{p.value}</code> : p.value}</dd>
              </div>
            ))}
          </dl>
        </section>
      ) : null}
    </>
  );

  if (wide) {
    return (
      <aside className="fv-rail" data-bd-ignore="" aria-label="About this file">
        <div className="fv-rail-in">{body}</div>
      </aside>
    );
  }
  return (
    <details className="fv-rail narrow" data-bd-ignore="" open={open}>
      <summary
        onClick={(e) => {
          e.preventDefault();
          setOpen(!open);
        }}
      >
        Page info
      </summary>
      <div className="fv-rail-in">{body}</div>
    </details>
  );
}

function ReadsCard({ heat, insightsHref }: { heat: HeatEntry; insightsHref?: string }) {
  const total = heatTotal(heat);
  const parts = [
    { k: "agent", n: heat.agent || 0, label: "agent" },
    { k: "human", n: heat.human || 0, label: "human" },
    { k: "share", n: heat.share || 0, label: "shared" },
  ].filter((p) => p.n > 0);
  return (
    <section className="fv-sec fv-readcard" aria-label="Reads, last 30 days">
      <h4>
        Reads · 30 days
        {insightsHref && (
          <a className="fv-sec-link" href={insightsHref}>
            Insights →
          </a>
        )}
      </h4>
      <div className="fv-big" title={HEAT_DISCLOSURE}>
        {total}
        <span className="sr-only"> — {HEAT_DISCLOSURE}</span>
      </div>
      <div className="fv-split" role="img" aria-label={parts.map((p) => `${p.n} ${p.label}`).join(", ")}>
        {parts.map((p) => (
          <span key={p.k} className={"fv-split-" + p.k} style={{ flexGrow: p.n }} />
        ))}
      </div>
      <div className="fv-legend">
        {parts.map((p) => (
          <span key={p.k}>
            <i className={"fv-sw fv-split-" + p.k} /> {p.n} {p.label}
          </span>
        ))}
      </div>
      {(heat.readers || heat.last) && (
        <p className="fv-readnote">
          {heat.readers ? `${heat.readers} distinct reader${heat.readers === 1 ? "" : "s"}` : ""}
          {heat.readers && heat.last ? " · " : ""}
          {heat.last ? "last read " + new Date(heat.last).toLocaleDateString() : ""}
        </p>
      )}
    </section>
  );
}

// The heading the reader is in: the last one whose top has scrolled past the
// upper fifth of the scroller.
function useActiveHeading(toc: TocItem[]): string | null {
  const [active, setActive] = useState<string | null>(toc[0]?.id ?? null);
  useEffect(() => {
    const content = document.getElementById("content");
    if (!content || !toc.length) return;
    let raf = 0;
    const update = () => {
      raf = 0;
      const line = content.getBoundingClientRect().top + content.clientHeight * 0.2;
      let cur = toc[0].id;
      for (const t of toc) {
        const el = document.getElementById(t.id);
        if (el && el.getBoundingClientRect().top <= line) cur = t.id;
      }
      // The bottom of the page: whatever is last on screen is where you are.
      if (content.scrollTop > 0 && content.scrollTop + content.clientHeight >= content.scrollHeight - 2) cur = toc[toc.length - 1].id;
      setActive(cur);
    };
    const onScroll = () => {
      if (!raf) raf = requestAnimationFrame(update);
    };
    update();
    content.addEventListener("scroll", onScroll, { passive: true });
    return () => {
      content.removeEventListener("scroll", onScroll);
      cancelAnimationFrame(raf);
    };
  }, [toc]);
  return active;
}
