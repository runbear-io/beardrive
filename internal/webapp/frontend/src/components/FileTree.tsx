import { useEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import type { Node } from "../api/types";
import { Icon, closeSidebarOnMobile } from "./shell";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuShortcut,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuShortcut,
  ContextMenuTrigger,
} from "@/components/ui/context-menu";
import { buildRows, filterTree, parentOf, systemKey, type FilterHit, type TreeOrder, type TreeStats } from "../lib/treeview";
import { registerFilterResults, setFilterSel, useSidebarSearch } from "../lib/sidebarSearch";
import { requestSearch } from "../search";
import { isMac } from "../util";

// The sidebar file tree, virtualized: only the visible window of rows is in
// the DOM (collapsed subtrees aren't rendered at all), so 10k-file projects
// scroll smoothly. The chevron only folds; the row selects (opens a
// folder's listing / a file). Clicking the folder whose listing is already
// showing folds/unfolds it, like a plain tree. Which rows exist at all —
// conflict copies folded into badges, system files grouped, nested folders
// capped — is lib/treeview.

// Every action the row menu can offer. An absent callback is an action this
// reader cannot take here, and its item is not drawn.
export interface TreeActions {
  open: (path: string) => void;
  newFile?: (folder: string) => void;
  /** Whether a new file may be created in this folder — folder rules can
      make a subtree read-only for someone who may write the project. */
  canCreateIn?: (folder: string) => boolean;
  pinned?: (path: string) => boolean;
  togglePin?: (path: string) => void;
  copyPath: (path: string) => void;
  copyShareLink?: (path: string) => void;
  history?: (path: string) => void;
  insights?: (path: string) => void;
  remove?: (path: string) => void;
}

// The two row menus, ⋯ (a DropdownMenu anchored to its button) and right-click
// (a ContextMenu at the pointer), draw this one list through their own parts.
interface MenuKit {
  Item: typeof DropdownMenuItem | typeof ContextMenuItem;
  Separator: typeof DropdownMenuSeparator | typeof ContextMenuSeparator;
  Shortcut: typeof DropdownMenuShortcut | typeof ContextMenuShortcut;
}
const DROPDOWN_KIT: MenuKit = { Item: DropdownMenuItem, Separator: DropdownMenuSeparator, Shortcut: DropdownMenuShortcut };
const CONTEXT_KIT: MenuKit = { Item: ContextMenuItem, Separator: ContextMenuSeparator, Shortcut: ContextMenuShortcut };

function RowMenuItems({ path, dir, actions, kit }: {
  path: string;
  dir: boolean;
  actions: TreeActions;
  kit: MenuKit;
}) {
  const { Item, Separator, Shortcut } = kit;
  const folder = dir ? path : parentOf(path).replace(/\/$/, "");
  return (
    <>
      <Item onSelect={() => actions.open(path)}>
        <Icon name={dir ? "folderopen" : "doc"} /> {dir ? "Open folder" : "Open"}
        <Shortcut>↵</Shortcut>
      </Item>
      {actions.newFile && (actions.canCreateIn?.(folder) ?? true) && (
        <Item onSelect={() => actions.newFile!(folder)}>
          <Icon name="fileplus" /> New file
        </Item>
      )}
      {actions.togglePin && (
        <Item onSelect={() => actions.togglePin!(path)}>
          <Icon name="star" /> {actions.pinned?.(path) ? "Unpin" : "Pin to sidebar"}
        </Item>
      )}
      <Separator />
      <Item onSelect={() => actions.copyPath(path)}>
        <Icon name="copy" /> Copy path
      </Item>
      {actions.copyShareLink && !dir && (
        <Item onSelect={() => actions.copyShareLink!(path)}>
          <Icon name="link" /> Copy share link
        </Item>
      )}
      {(actions.history || actions.insights) && <Separator />}
      {actions.history && (
        <Item onSelect={() => actions.history!(path)}>
          <Icon name="hist" /> Show history
        </Item>
      )}
      {actions.insights && (
        <Item onSelect={() => actions.insights!(path)}>
          <Icon name="insights" /> View in Insights
        </Item>
      )}
      {actions.remove && !dir && (
        <>
          <Separator />
          <Item variant="destructive" onSelect={() => actions.remove!(path)}>
            <Icon name="trash" /> Delete…
          </Item>
        </>
      )}
    </>
  );
}

// Shift+F10 and the Menu key open the context menu at the focused row, just
// below its label, rather than wherever the browser would place it.
function openContextMenuAt(row: HTMLElement) {
  const r = row.getBoundingClientRect();
  row.dispatchEvent(
    new MouseEvent("contextmenu", { bubbles: true, cancelable: true, clientX: r.left + 24, clientY: r.bottom }),
  );
}

function activateOnKey(fn: () => void) {
  return (e: React.KeyboardEvent) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      fn();
    }
  };
}

export function FileTree(props: {
  root: Node | undefined;
  expanded: Set<string>;
  onToggle: (path: string) => void;
  currentPath: string;
  listingShowing: boolean; // current view is a folder listing
  /* Prefixes of folders carrying an access rule. The tree is the ONLY place a
     top-level folder appears — the project root renders the connect guide and
     the dashboard, not a listing — so without a mark here a restricted
     top-level folder has nowhere to announce itself. A glyph, not the listing's
     text pill: these rows are 28px and already carry chevron, guides, icon and
     label. */
  restricted: Set<string>;
  stats: TreeStats;
  order?: TreeOrder;
  actions: TreeActions;
}) {
  const { root, expanded, onToggle, currentPath, listingShowing, restricted, stats, order, actions } = props;
  const scrollRef = useRef<HTMLElement>(null);
  const [showAll, setShowAll] = useState<Set<string>>(() => new Set());
  // The row whose ⋯ menu / right-click menu is open, for the row highlight.
  const [menuFor, setMenuFor] = useState<string | null>(null);
  const [contextFor, setContextFor] = useState<string | null>(null);
  const menuScrollTop = useRef(0);
  // A touch long-press opens the context menu, and lifting the finger still
  // fires the row's click: a click this soon after a menu opened is that
  // release, not a request to open the row under the menu.
  const menuOpenedAt = useRef(0);
  const noteMenuOpen = () => {
    menuScrollTop.current = scrollRef.current?.scrollTop ?? 0;
    menuOpenedAt.current = Date.now();
  };
  const fromMenuRelease = () => Date.now() - menuOpenedAt.current < 700;

  const rows = useMemo(
    () => buildRows(root, { expanded, showAll, stats, keep: currentPath, order }),
    [root, expanded, showAll, stats, currentPath, order],
  );

  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => (window.matchMedia("(max-width: 768px)").matches ? 44 : 28),
    overscan: 12,
    getItemKey: (i) => {
      const r = rows[i];
      return r.kind === "node" ? r.node.path : r.kind + ":" + r.parent;
    },
  });

  // Keep the subject visible: when the selection changes (tree click, deep
  // link, scoped view), scroll its row into the window.
  useEffect(() => {
    if (!currentPath) return;
    const i = rows.findIndex((r) => r.kind === "node" && r.node.path === currentPath);
    if (i >= 0) virtualizer.scrollToIndex(i, { align: "auto" });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currentPath, rows]);

  const guides = (depth: number) =>
    Array.from({ length: depth }, (_, i) => (
      <span key={i} className="tguide" style={{ left: 8 + i * 13 + 5 }} aria-hidden="true" />
    ));
  const place = (start: number, depth: number) => ({
    position: "absolute" as const,
    top: 0,
    left: 0,
    right: 0,
    transform: `translateY(${start}px)`,
    paddingLeft: 8 + depth * 13,
  });

  return (
    <nav
      id="tree"
      aria-label="Files"
      ref={scrollRef}
      onScroll={(e) => {
        // The virtualizer recycles rows as the tree scrolls, so a menu would
        // be left pointing at a row that is gone or now shows another path.
        // Focus returning to a row nudges the scroll by a few pixels; only a
        // real scroll closes the menu.
        if (!menuFor && !contextFor) return;
        if (Math.abs(e.currentTarget.scrollTop - menuScrollTop.current) < 28) return;
        if (menuFor) setMenuFor(null);
        if (contextFor) document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
      }}
    >
      <div style={{ height: virtualizer.getTotalSize(), position: "relative" }}>
        {virtualizer.getVirtualItems().map((vi) => {
          const r = rows[vi.index];
          if (r.kind === "more") {
            const more = () => setShowAll((s) => new Set(s).add(r.parent));
            return (
              <div
                key={vi.key}
                className="row tmore"
                role="button"
                tabIndex={0}
                data-more={r.parent}
                style={place(vi.start, r.depth)}
                onClick={more}
                onKeyDown={activateOnKey(more)}
              >
                {guides(r.depth)}
                <span className="label">Show {r.hidden} more</span>
              </div>
            );
          }
          if (r.kind === "system") {
            const toggle = () => onToggle(systemKey(r.parent));
            return (
              <div
                key={vi.key}
                className={"row dir tsystem" + (r.open ? "" : " collapsed")}
                role="button"
                tabIndex={0}
                aria-expanded={r.open}
                data-system={r.parent}
                style={place(vi.start, r.depth)}
                onClick={toggle}
                onKeyDown={activateOnKey(toggle)}
              >
                {guides(r.depth)}
                <span className="chev">
                  <Icon name="chevd" />
                </span>
                <span className="ticon">
                  <Icon name="system" />
                </span>
                <span className="label">System files</span>
                <span className="tmeta">{r.count}</span>
              </div>
            );
          }
          const { node: n, depth, conflicts, fresh } = r;
          const open = n.dir ? expanded.has(n.path) : false;
          const click = () => {
            if (fromMenuRelease()) return;
            if (n.dir && currentPath === n.path && listingShowing) {
              // Folding beats re-opening when this folder's listing is up.
              onToggle(n.path);
              return;
            }
            actions.open(n.path);
            if (!n.dir) closeSidebarOnMobile();
          };
          return (
            <ContextMenu
              key={vi.key}
              modal={false}
              onOpenChange={(o) => {
                if (o) noteMenuOpen();
                setContextFor((p) => (o ? n.path : p === n.path ? null : p));
              }}
            >
            <DropdownMenu
              open={menuFor === n.path}
              onOpenChange={(o) => {
                if (o) noteMenuOpen();
                setMenuFor(o ? n.path : null);
              }}
            >
              <ContextMenuTrigger asChild>
                <div
                  className={"row " + (n.dir ? "dir" : "file") + (currentPath === n.path ? " active" : "") + (n.dir && !open ? " collapsed" : "") + (menuFor === n.path || contextFor === n.path ? " menu-open" : "")}
                  data-path={n.path}
                  tabIndex={0}
                  role="button"
                  title={n.name}
                  aria-expanded={n.dir ? open : undefined}
                  style={place(vi.start, depth)}
                  onClick={click}
                  onKeyDown={(e) => {
                    if (e.target !== e.currentTarget) return;
                    if ((e.key === "F10" && e.shiftKey) || e.key === "ContextMenu") {
                      e.preventDefault();
                      openContextMenuAt(e.currentTarget);
                      return;
                    }
                    activateOnKey(click)(e);
                  }}
                >
                  {guides(depth)}
                  <span
                    className="chev"
                    onClick={(e) => {
                      if (!n.dir) return;
                      e.stopPropagation();
                      onToggle(n.path);
                    }}
                  >
                    <Icon name="chevd" />
                  </span>
                  <span className="ticon">
                    <Icon name={n.dir ? "folder" : n.link ? "link" : "doc"} />
                  </span>
                  <span className="label" title={n.link ? `${n.name} → ${n.link}` : undefined}>
                    {n.name}
                  </span>
                  {conflicts > 0 && (
                    <span className="tconflicts">
                      · {conflicts} conflict{conflicts === 1 ? "" : "s"}
                    </span>
                  )}
                  {n.dir && fresh > 0 && <span className="tmeta tnew">{fresh} new</span>}
                  {n.dir && restricted.has(n.path) && (
                    <span
                      className="trestricted"
                      role="img"
                      aria-label={n.name + " is a restricted folder"}
                      title="Restricted — not everyone in the workspace has the same access here"
                    >
                      <Icon name="lock" />
                    </span>
                  )}
                  <span className="thover">
                    {n.dir && actions.newFile && (actions.canCreateIn?.(n.path) ?? true) && (
                      <button
                        className="tact"
                        aria-label={"New file in " + n.name}
                        title="New file"
                        tabIndex={-1}
                        onClick={(e) => {
                          e.stopPropagation();
                          actions.newFile!(n.path);
                        }}
                      >
                        <Icon name="plus" />
                      </button>
                    )}
                    <DropdownMenuTrigger asChild>
                      <button
                        className="tact"
                        aria-label={"Actions for " + n.name}
                        title="More actions"
                        tabIndex={-1}
                        onClick={(e) => e.stopPropagation()}
                      >
                        <Icon name="dots" />
                      </button>
                    </DropdownMenuTrigger>
                  </span>
                </div>
              </ContextMenuTrigger>
              <DropdownMenuContent className="row-menu" align="start" onClick={(e) => e.stopPropagation()}>
                <RowMenuItems path={n.path} dir={!!n.dir} actions={actions} kit={DROPDOWN_KIT} />
              </DropdownMenuContent>
            </DropdownMenu>
            <ContextMenuContent className="row-menu" onClick={(e) => e.stopPropagation()}>
              <RowMenuItems path={n.path} dir={!!n.dir} actions={actions} kit={CONTEXT_KIT} />
            </ContextMenuContent>
            </ContextMenu>
          );
        })}
      </div>
    </nav>
  );
}

/* ---- filter mode ----
   Typing in the sidebar search replaces the tree with matches: folders, then
   files, each with the folder it lives in underneath so two folders with one
   name are told apart. The last row hands the query to the palette. */
const FOLDER_LIMIT = 5;
const FILE_LIMIT = 8;

export function TreeFilter(props: {
  query: string;
  dirs: Iterable<string>;
  files: Node[];
  onOpen: (path: string) => void;
}) {
  const { query, onOpen } = props;
  const { sel } = useSidebarSearch();
  const [moreFiles, setMoreFiles] = useState(false);
  useEffect(() => setMoreFiles(false), [query]);
  const { folders, files } = useMemo(
    () => filterTree(query, props.dirs, props.files),
    [query, props.dirs, props.files],
  );
  const shownFolders = folders.slice(0, FOLDER_LIMIT);
  const shownFiles = moreFiles ? files : files.slice(0, FILE_LIMIT);
  const flat = [...shownFolders, ...shownFiles];
  const q = query.trim();

  useEffect(() => {
    registerFilterResults(flat.length + 1, (i) => {
      if (i < flat.length) {
        onOpen(flat[i].path);
        if (!flat[i].dir) closeSidebarOnMobile();
      } else requestSearch(q);
    });
  });
  const listRef = useRef<HTMLElement>(null);
  useEffect(() => {
    listRef.current?.querySelector(".fhit.sel")?.scrollIntoView({ block: "nearest" });
  }, [sel]);

  const row = (h: FilterHit, i: number) => (
    <div
      key={(h.dir ? "d:" : "f:") + h.path}
      className={"fhit" + (i === sel ? " sel" : "")}
      role="option"
      aria-selected={i === sel}
      data-path={h.path}
      onMouseEnter={() => setFilterSel(i)}
      onClick={() => {
        onOpen(h.path);
        if (!h.dir) closeSidebarOnMobile();
      }}
    >
      <span className="ticon">
        <Icon name={h.dir ? "folder" : "doc"} />
      </span>
      <span className="fhit-text">
        <span className="fhit-name">
          {h.name.slice(0, h.at)}
          <mark>{h.name.slice(h.at, h.at + q.length)}</mark>
          {h.name.slice(h.at + q.length)}
        </span>
        <span className="fhit-path">{parentOf(h.path) || "/"}</span>
      </span>
      {i === sel && <span className="fhit-enter" aria-hidden="true">↵</span>}
    </div>
  );

  return (
    <nav id="tree-filter" aria-label="Matching files" ref={listRef} role="listbox">
      {shownFolders.length > 0 && <div className="fgroup">Folders · {folders.length}</div>}
      {shownFolders.map((h, i) => row(h, i))}
      {shownFiles.length > 0 && <div className="fgroup">Files · {files.length}</div>}
      {shownFiles.map((h, i) => row(h, shownFolders.length + i))}
      {!moreFiles && files.length > FILE_LIMIT && (
        <button className="fmore" onClick={() => setMoreFiles(true)}>
          Show {files.length - FILE_LIMIT} more files
        </button>
      )}
      {flat.length === 0 && <div className="fempty">No file or folder names match</div>}
      <div
        className={"fhit fcontents" + (sel === flat.length ? " sel" : "")}
        role="option"
        aria-selected={sel === flat.length}
        onMouseEnter={() => setFilterSel(flat.length)}
        onClick={() => requestSearch(q)}
      >
        <span className="ticon">
          <Icon name="search" />
        </span>
        <span className="fhit-name">Search contents for “{q}”</span>
        <kbd>{isMac ? "⌘↵" : "Ctrl+↵"}</kbd>
      </div>
    </nav>
  );
}

/* Every ancestor folder of a path (for unfolding the way to it). */
export function ancestorsOf(filePath: string): string[] {
  const parts = filePath.split("/");
  const out: string[] = [];
  let acc = "";
  for (let i = 0; i < parts.length - 1; i++) {
    acc = acc ? acc + "/" + parts[i] : parts[i];
    out.push(acc);
  }
  return out;
}
