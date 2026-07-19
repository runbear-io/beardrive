import { useEffect, useMemo, useState } from "react";
import { Icon } from "./shell";
import { Command, CommandInput, CommandItem, CommandList } from "@/components/ui/command";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";

/* ---- command palette (⌘K / Ctrl+K) ----
   One box for everything: fuzzy-jump to any file, switch projects, and run
   quick actions (share, history, upload, download, sign out). */

export interface PaletteItem {
  icon: string;
  label: string;
  kind: string; // action | project | folder | file
  run: () => void;
}

/* Subsequence fuzzy match. Returns a score (higher = better), plus the
   matched positions for highlighting; null when it doesn't match. */
function fuzzy(query: string, text: string): { score: number; hits: number[] } | null {
  if (!query) return { score: 0, hits: [] };
  const q = query.toLowerCase();
  const t = text.toLowerCase();
  let ti = 0,
    score = 0,
    streak = 0;
  const hits: number[] = [];
  for (let qi = 0; qi < q.length; qi++) {
    const found = t.indexOf(q[qi], ti);
    if (found === -1) return null;
    streak = found === ti ? streak + 1 : 1;
    score += streak * 3; // consecutive runs
    if (found === 0 || "/ -_.".includes(t[found - 1])) score += 8; // word starts
    hits.push(found);
    ti = found + 1;
  }
  score -= Math.floor(t.length / 8); // mild preference for short targets
  return { score, hits };
}

/* Match a query against a label, tolerating a simple English plural so
   "ideas" still finds idea.md. Tries the raw query first, then a lightly
   de-pluralized form (…ies→…y, …es→…, …s→…). */
function fuzzyStemmed(query: string, label: string) {
  const m = fuzzy(query, label);
  if (m) return m;
  const q = query.toLowerCase();
  let stem: string | null = null;
  if (q.length > 3 && q.endsWith("ies")) stem = q.slice(0, -3) + "y";
  else if (q.length > 3 && q.endsWith("es")) stem = q.slice(0, -2);
  else if (q.length > 2 && q.endsWith("s")) stem = q.slice(0, -1);
  return stem ? fuzzy(stem, label) : null;
}

function Highlight({ text, hits }: { text: string; hits: number[] }) {
  const out: React.ReactNode[] = [];
  let last = 0;
  hits.forEach((h, i) => {
    if (h > last) out.push(text.slice(last, h));
    out.push(<b key={i}>{text[h]}</b>);
    last = h + 1;
  });
  out.push(text.slice(last));
  return <span className="plabel">{out}</span>;
}

export function Palette({
  open,
  onClose,
  candidates,
}: {
  open: boolean;
  onClose: () => void;
  candidates: () => PaletteItem[];
}) {
  const [query, setQuery] = useState("");

  const items = useMemo(() => {
    if (!open) return [];
    const scored: Array<PaletteItem & { score: number; hits: number[] }> = [];
    for (const c of candidates()) {
      const m = fuzzyStemmed(query, c.label);
      if (m) scored.push({ ...c, score: m.score, hits: m.hits });
    }
    scored.sort((a, b) => b.score - a.score);
    return scored.slice(0, 40);
  }, [open, query, candidates]);

  useEffect(() => {
    if (open) setQuery("");
  }, [open]);

  const run = (item: PaletteItem) => {
    onClose();
    item.run();
  };

  // cmdk owns keyboard nav/selection/aria; our fuzzy scorer owns matching
  // and order (shouldFilter=false), so search behavior is unchanged.
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent id="palette" className="palette" showCloseButton={false} aria-describedby={undefined}>
        <DialogTitle className="sr-only">Search and quick actions</DialogTitle>
        <Command shouldFilter={false} loop>
          <div id="palette-inputwrap">
            <Icon name="search" />
            <CommandInput
              id="palette-input"
              placeholder="Search file names, projects, actions…"
              autoComplete="off"
              spellCheck={false}
              value={query}
              onValueChange={setQuery}
            />
          </div>
          <CommandList id="palette-results">
            {items.length === 0 ? (
              <div className="pempty">No matches — search covers file names, projects, and actions</div>
            ) : (
              items.map((item) => (
                <CommandItem
                  key={item.kind + ":" + item.label}
                  value={item.kind + ":" + item.label}
                  onSelect={() => run(item)}
                >
                  <span className="picon">
                    <Icon name={item.icon} />
                  </span>
                  <Highlight text={item.label} hits={item.hits} />
                  <span className="pkind">{item.kind}</span>
                </CommandItem>
              ))
            )}
          </CommandList>
          <footer id="palette-hint">↑↓ navigate · ⏎ select · esc close</footer>
        </Command>
      </DialogContent>
    </Dialog>
  );
}
