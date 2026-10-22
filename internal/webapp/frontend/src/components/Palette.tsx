import { useEffect, useMemo, useState } from "react";
import { Icon } from "./shell";
import { Marked } from "./Marked";
import { Command, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { prepare, search, type Range } from "@/lib/fuzzy";

/* ---- command palette (⌘K / Ctrl+K) ----
   One box for everything: jump to any file or folder, go to the project's
   pages, switch projects, and run actions on the open file. Empty, it lists
   the files you opened last and what you can do, in sections. Typing ranks
   everything in one list, best match first. */

export interface PaletteItem {
  icon: string;
  // What a query matches. For a file or folder this is its path: the name is
  // painted first and the folder after it, dim, so "docs cli" can match
  // across both.
  label: string;
  hint?: string; // painted dim after the label, never matched
  kind: "file" | "folder" | "project" | "page" | "action";
  // The empty palette's section. An item without one only answers a query.
  group?: string;
  run: () => void;
}

const LIMIT = 40;
const RECENT = 5;

function Row({ item, ranges, hint }: { item: PaletteItem; ranges: readonly Range[]; hint?: string }) {
  const path = item.kind === "file" || item.kind === "folder";
  const cut = path ? item.label.lastIndexOf("/") + 1 : 0;
  return (
    <>
      <span className="picon">
        <Icon name={item.icon} />
      </span>
      <span className="ptext">
        <Marked className="plabel" text={item.label.slice(cut)} ranges={ranges} offset={cut} />
        {path && cut > 0 && <Marked className="ploc" text={item.label.slice(0, cut - 1)} ranges={ranges} />}
        {!path && hint && <span className="ploc">{hint}</span>}
      </span>
      {!path && <span className="pkind">{item.kind}</span>}
    </>
  );
}

export function Palette({
  open,
  onClose,
  candidates,
  recent = [],
  initialQuery = "",
}: {
  open: boolean;
  onClose: () => void;
  candidates: () => PaletteItem[];
  recent?: string[]; // file paths, newest first
  initialQuery?: string; // what the sidebar's filter already had typed
}) {
  const [query, setQuery] = useState("");
  useEffect(() => {
    if (open) setQuery(initialQuery);
  }, [open, initialQuery]);

  // Built once per opening, not per keystroke: a tree can hold tens of
  // thousands of paths. Preparing them waits for the first character typed,
  // so the palette itself opens at once.
  const items = useMemo(() => (open ? candidates() : []), [open, candidates]);
  const typing = query.trim() !== "";
  const prepared = useMemo(() => (typing ? items.map((i) => prepare(i.label)) : []), [items, typing]);
  const hits = useMemo(() => search(query, prepared, { limit: LIMIT }).hits, [query, prepared]);

  const sections = useMemo(() => {
    const out = new Map<string, PaletteItem[]>();
    const files = new Map<string, PaletteItem>();
    for (const i of items) if (i.kind === "file") files.set(i.label, i);
    const recentItems = recent.flatMap((p) => files.get(p) ?? []).slice(0, RECENT);
    if (recentItems.length) out.set("Recent", recentItems);
    for (const i of items) if (i.group) (out.get(i.group) ?? out.set(i.group, []).get(i.group)!).push(i);
    return [...out];
  }, [items, recent]);

  const run = (item: PaletteItem) => {
    onClose();
    item.run();
  };

  // cmdk owns keyboard nav/selection/aria; our matcher owns matching and
  // order (shouldFilter=false).
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent id="palette" className="palette" showCloseButton={false} aria-describedby={undefined}>
        <DialogTitle className="sr-only">Search file names, projects and actions</DialogTitle>
        <Command shouldFilter={false} loop>
          <div id="palette-inputwrap">
            <Icon name="search" />
            {/* No id here (nor on CommandList): cmdk writes its own after our
                props, so one never reaches the DOM. Style off #palette /
                #palette-inputwrap or [cmdk-*] instead. (BEA-54) */}
            <CommandInput
              placeholder="Search file names, projects, actions…"
              autoComplete="off"
              spellCheck={false}
              value={query}
              onValueChange={setQuery}
            />
          </div>
          <CommandList>
            {typing ? (
              hits.length === 0 ? (
                <div className="pempty">No matches — search covers file names, projects, and actions</div>
              ) : (
                hits.map((h) => {
                  const item = items[h.index];
                  return (
                    <CommandItem key={h.index} value={String(h.index)} data-kind={item.kind} onSelect={() => run(item)}>
                      <Row item={item} ranges={h.ranges} hint={item.hint} />
                    </CommandItem>
                  );
                })
              )
            ) : (
              sections.map(([name, rows]) => (
                <CommandGroup key={name} heading={name}>
                  {rows.map((item) => (
                    <CommandItem
                      key={item.kind + ":" + item.label}
                      value={name + ":" + item.kind + ":" + item.label}
                      data-kind={item.kind}
                      onSelect={() => run(item)}
                    >
                      {/* A section named after the file already says what its actions act on. */}
                      <Row item={item} ranges={[]} hint={item.hint === name ? undefined : item.hint} />
                    </CommandItem>
                  ))}
                </CommandGroup>
              ))
            )}
          </CommandList>
          <footer id="palette-hint">↑↓ navigate · ↵ open · esc close</footer>
        </Command>
      </DialogContent>
    </Dialog>
  );
}
