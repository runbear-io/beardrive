import { useEffect, useRef, useState } from "react";
import { Input } from "@/components/ui/input";
import { Icon } from "./shell";
import { hasHistoryFilters, type HistoryFilters as Filters } from "../router";

/* ---- history filters ----
   The whole feed is one flat scroll, and agents write far more than people
   do — so a month-old project is unreadable without a way to narrow it.
   Every filter is applied SERVER-side (?q=/?user=/?since=/?until=), never
   over the loaded page: filtering what happens to be on screen would lie
   about everything below the fold and break paging. State lives in the URL,
   so a narrowed feed is linkable and Back undoes a filter like any other
   navigation.

   Dates are bare YYYY-MM-DD and the server reads them as UTC days — the
   label says so, because a native date input speaks the reader's local
   calendar and a silent conversion would quietly drop an evening's changes
   for anyone east of UTC. */
export function HistoryFilters(props: {
  filters?: Filters;
  authors: string[]; // accounts seen in the loaded window
  onChange: (f: Filters) => void;
}) {
  const { filters, authors, onChange } = props;
  const set = (k: keyof Filters, v: string) => onChange({ ...filters, [k]: v || undefined });

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
      if (q !== (filters?.q ?? "")) set("q", q);
    }, 250);
    return () => clearTimeout(t);
  }, [q]); // eslint-disable-line react-hooks/exhaustive-deps

  // The URL is authoritative: an author filtered from a page that is no
  // longer loaded still has to show as the current selection.
  const options = filters?.user && !authors.includes(filters.user) ? [filters.user, ...authors] : authors;
  const active = hasHistoryFilters(filters);
  return (
    <div className="hfilters">
      <label className="hf-search">
        <Icon name="search" />
        <Input
          type="search"
          value={q}
          placeholder="path contains…"
          aria-label="Filter by path"
          onChange={(e) => {
            typed.current = true;
            setQ(e.target.value);
          }}
        />
      </label>
      <select
        className="hf-user"
        value={filters?.user ?? ""}
        aria-label="Filter by author"
        onChange={(e) => set("user", e.target.value)}
      >
        <option value="">Anyone</option>
        {options.map((a) => (
          <option key={a} value={a}>
            {a}
          </option>
        ))}
      </select>
      <span className="hf-dates">
        <span className="hf-lbl">UTC</span>
        <Input
          type="date"
          className="hf-date"
          value={filters?.since ?? ""}
          aria-label="From date (UTC)"
          onChange={(e) => set("since", e.target.value)}
        />
        <span className="hf-dash">–</span>
        <Input
          type="date"
          className="hf-date"
          value={filters?.until ?? ""}
          aria-label="To date (UTC)"
          onChange={(e) => set("until", e.target.value)}
        />
      </span>
      {active && (
        <button type="button" className="hf-clear" onClick={() => onChange({})}>
          Clear
        </button>
      )}
    </div>
  );
}

// The accounts present in a loaded feed, in first-seen order.
export function authorsOf(entries: { user?: string }[]): string[] {
  const seen = new Set<string>();
  for (const e of entries) if (e.user) seen.add(e.user);
  return [...seen].sort();
}
