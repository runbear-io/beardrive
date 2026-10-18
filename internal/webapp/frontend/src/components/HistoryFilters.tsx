import { useEffect, useRef, useState } from "react";
import { Input } from "@/components/ui/input";
import { Icon } from "./shell";
import { hasHistoryFilters, type HistoryFilters as Filters } from "../router";
import { RANGE_LABEL } from "../lib/activity";

/* ---- history filters ----
   The whole feed is one flat scroll, and agents write far more than people
   do — so a month-old project is unreadable without a way to narrow it.
   Every filter is applied SERVER-side (?q=/?user=/?actor=/?device=/?since=/
   ?until=), never over the loaded page: filtering what happens to be on
   screen would lie about everything below the fold and break paging. State
   lives in the URL, so a narrowed feed is linkable and Back undoes a filter
   like any other navigation.

   Dates are the reader's own calendar days. The URL keeps the day (or the
   preset), and the feed turns it into instants at the reader's midnight
   (lib/activity.ts) — so "today" ends when the reader's day does. */

export type DeviceOption = { id: string; name: string };

const SEGMENTS: [string, string][] = [
  ["", "Everyone"],
  ["agent", "Agents"],
  ["human", "People"],
];

// The reader's zone, named in the date controls so a day is never ambiguous.
export const localZone = (): string => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "local time";
  } catch {
    return "local time";
  }
};

export function HistoryFilters(props: {
  filters?: Filters;
  authors: string[]; // accounts seen in the loaded window
  devices: DeviceOption[]; // devices seen in the loaded window
  onChange: (f: Filters) => void;
}) {
  const { filters, authors, devices, onChange } = props;
  const set = (patch: Partial<Filters>) => {
    const next: Filters = { ...filters, ...patch };
    for (const k of Object.keys(next) as (keyof Filters)[]) if (!next[k]) delete next[k];
    onChange(next);
  };

  // The path box is typed into, so it keeps its own state and pushes a URL
  // only once typing pauses — a navigation per keystroke would stack a
  // history entry per letter and refetch on each one.
  const [q, setQ] = useState(filters?.q ?? "");
  const typed = useRef(false);
  useEffect(() => {
    if (!typed.current) setQ(filters?.q ?? ""); // external change (Back, Clear, deep link)
  }, [filters?.q]);
  useEffect(() => {
    if (!typed.current) return;
    const t = setTimeout(() => {
      typed.current = false;
      if (q !== (filters?.q ?? "")) set({ q });
    }, 250);
    return () => clearTimeout(t);
  }, [q]); // eslint-disable-line react-hooks/exhaustive-deps

  // Custom is a mode of the date control, not a URL value: dates in the URL
  // are what make it custom.
  const [customOpen, setCustomOpen] = useState(false);
  const custom = customOpen || !!filters?.since || !!filters?.until;
  const range = filters?.range ?? (custom ? "custom" : "");

  // The URL is authoritative: an author or device filtered from a page that
  // is no longer loaded still has to show as the current selection.
  const options = filters?.user && !authors.includes(filters.user) ? [filters.user, ...authors] : authors;
  const devOptions =
    filters?.device && !devices.some((d) => d.id === filters.device)
      ? [{ id: filters.device, name: filters.device }, ...devices]
      : devices;
  // One control narrows by machine or by account: both answer "whose
  // changes". A URL carrying both shows the device; the chips name each.
  const who = filters?.device ? "d:" + filters.device : filters?.user ? "u:" + filters.user : "";
  const active = hasHistoryFilters(filters);
  const zone = localZone();
  return (
    <div className="hfilters">
      <label className="hf-search">
        <Icon name="search" />
        <Input
          type="search"
          value={q}
          placeholder="Filter by path or file name…"
          aria-label="Filter by path"
          onChange={(e) => {
            typed.current = true;
            setQ(e.target.value);
          }}
        />
      </label>
      <div className="hf-seg" role="group" aria-label="Who made the change">
        {SEGMENTS.map(([v, label]) => (
          <button
            key={label}
            type="button"
            aria-pressed={(filters?.actor ?? "") === v}
            onClick={() => set({ actor: v })}
          >
            {label}
          </button>
        ))}
      </div>
      <span className="hf-pick">
        <Icon name="laptop" />
        <select
          className="hf-device"
          value={who}
          aria-label="Filter by device or person"
          onChange={(e) => {
            const v = e.target.value;
            set({
              device: v.startsWith("d:") ? v.slice(2) : undefined,
              user: v.startsWith("u:") ? v.slice(2) : undefined,
            });
          }}
        >
          <option value="">All devices</option>
          {devOptions.length > 0 && (
            <optgroup label="Devices">
              {devOptions.map((d) => (
                <option key={d.id} value={"d:" + d.id}>
                  {d.name}
                </option>
              ))}
            </optgroup>
          )}
          {options.length > 0 && (
            <optgroup label="People">
              {options.map((a) => (
                <option key={a} value={"u:" + a}>
                  {a}
                </option>
              ))}
            </optgroup>
          )}
        </select>
      </span>
      <span className="hf-pick">
        <Icon name="clock" />
        <select
          className="hf-range"
          value={range}
          aria-label={`Date range (${zone})`}
          onChange={(e) => {
            const v = e.target.value;
            if (v === "custom") {
              setCustomOpen(true);
              if (filters?.range) set({ range: undefined });
              return;
            }
            setCustomOpen(false);
            set({ range: v, since: undefined, until: undefined });
          }}
        >
          <option value="">All time</option>
          {Object.entries(RANGE_LABEL).map(([k, label]) => (
            <option key={k} value={k}>
              {label}
            </option>
          ))}
          <option value="custom">Custom…</option>
        </select>
      </span>
      {custom && (
        <span className="hf-dates">
          <Input
            type="date"
            className="hf-date"
            value={filters?.since ?? ""}
            aria-label={`From date (${zone})`}
            onChange={(e) => set({ since: e.target.value, range: undefined })}
          />
          <span className="hf-dash">–</span>
          <Input
            type="date"
            className="hf-date"
            value={filters?.until ?? ""}
            aria-label={`To date (${zone})`}
            onChange={(e) => set({ until: e.target.value, range: undefined })}
          />
          <span className="hf-lbl" title="Days are your own calendar days">
            {zone}
          </span>
        </span>
      )}
      {active && (
        <button
          type="button"
          className="hf-clear"
          onClick={() => {
            setCustomOpen(false);
            onChange({});
          }}
        >
          Clear
        </button>
      )}
    </div>
  );
}

// Each active filter as a removable chip: [label, the filters without it].
export function filterChips(f: Filters | undefined, devices: DeviceOption[]): [string, Filters][] {
  if (!f) return [];
  const without = (...ks: (keyof Filters)[]) => {
    const n = { ...f };
    for (const k of ks) delete n[k];
    return n;
  };
  const out: [string, Filters][] = [];
  if (f.q) out.push(["path: " + f.q, without("q")]);
  if (f.actor) out.push([f.actor === "agent" ? "Agents" : "People", without("actor")]);
  if (f.device) out.push([devices.find((d) => d.id === f.device)?.name || f.device, without("device")]);
  if (f.user) out.push([f.user, without("user")]);
  if (f.range) out.push([RANGE_LABEL[f.range] || f.range, without("range")]);
  if (f.since || f.until) out.push([(f.since || "…") + " – " + (f.until || "…"), without("since", "until")]);
  return out;
}

// The accounts present in a loaded feed, in first-seen order.
export function authorsOf(entries: { user?: string }[]): string[] {
  const seen = new Set<string>();
  for (const e of entries) if (e.user) seen.add(e.user);
  return [...seen].sort();
}
