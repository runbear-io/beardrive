import { useEffect, useState } from "react";
import { navigate } from "../nav";
import { Icon, ProjectIcon, RailButton } from "./shell";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Project } from "../api/types";
import { closeSidebarOnMobile } from "./shell";
import { requestSearch } from "../search";
import { isMac } from "../util";
import { ACTIVITY_CAP, agoText, useActivityCount, useSyncLine } from "../hooks/useSidebar";
import {
  activateFilterSel,
  clearFilter,
  moveFilterSel,
  setFilterQuery,
  useSidebarSearch,
} from "../lib/sidebarSearch";

// Deterministic accent for a project's letter-mark, so each project keeps a
// stable color across reloads without any server state.
const PROJ_COLORS = ["#5b8def", "#f5a623", "#4cc38a", "#e0679b", "#8b7bf0", "#3ec8c8", "#e6934a"];
export function projColor(s: string): string {
  let h = 0;
  for (const c of s) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return PROJ_COLORS[h % PROJ_COLORS.length];
}

export type NavKey = "home" | "activity" | "insights" | "settings";

export interface ProjectMenu {
  active: NavKey | null;
  onNav: (key: NavKey) => void;
}

const NAV: ReadonlyArray<readonly [NavKey, string, string]> = [
  ["home", "Home", "home"],
  ["activity", "Activity", "activity"],
  ["insights", "Insights", "insights"],
  ["settings", "Settings", "gear"],
];

const countText = (n: number) => (n >= ACTIVITY_CAP ? ACTIVITY_CAP - 1 + "+" : String(n));

export function ProjectNav({
  projects,
  currentId,
  menu,
  onNew,
  readsEnabled = false,
}: {
  projects: Project[];
  currentId?: string;
  menu?: ProjectMenu;
  // Opening the create dialog belongs to HubApp: three things ask for it now
  // (this button, the empty state's button, and the auto-open when a signed-in
  // account has no projects at all), and one owner beats three copies.
  onNew: () => void;
  readsEnabled?: boolean; // the hub keeps read telemetry, so /heat?by=device answers
}) {
  const current = projects.find((p) => p.id === currentId);
  const fresh = useActivityCount(menu ? currentId : undefined);

  return (
    <nav id="projects" aria-label="Projects">
      <div className="nav-head">
        <span>Projects</span>
        <button
          className="nav-add"
          title="New project"
          aria-label="New project"
          onClick={onNew}
        >
          +
        </button>
      </div>
      <div className="proj-row">
        <Select
          value={currentId || ""}
          onValueChange={(v) => {
            if (v && v !== currentId) {
              navigate("/" + v);
              closeSidebarOnMobile();
            }
          }}
        >
          <SelectTrigger
            id="project-select"
            aria-label={`Switch project — current: ${current?.name ?? "none"}`}
            title={current?.name}
            className="proj-trigger"
          >
            {current && (
              <span
                className="proj-mark"
                aria-hidden="true"
                style={{ background: projColor(current.name) }}
              >
                <ProjectIcon name={current.icon} />
              </span>
            )}
            {/* SelectValue mirrors the selected item's text into the trigger —
                which, now that every item carries its own mark, would draw a
                second one here. Render the name ourselves when we have it,
                keeping the [data-slot="select-value"] styling hook. */}
            {current ? (
              <span className="proj-text">
                <span data-slot="select-value">{current.name}</span>
                {menu && <SyncLine pid={current.id} readsEnabled={readsEnabled} />}
              </span>
            ) : (
              <SelectValue placeholder="Select a project" />
            )}
          </SelectTrigger>
          <SelectContent className="proj-menu" position="popper" sideOffset={4}>
            {projects.map((p) => (
              <SelectItem key={p.id} value={p.id}>
                <span
                  className="proj-mark"
                  aria-hidden="true"
                  style={{ background: projColor(p.name) }}
                >
                  <ProjectIcon name={p.icon} />
                </span>
                {p.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      {menu && <SidebarSearch />}
      {menu && (
        <ul className="nav-menu" aria-label="Project pages">
          {NAV.map(([key, label, icon]) => (
            <li key={key}>
              <div
                id={"nav-" + key}
                className={"row" + (menu.active === key ? " active" : "")}
                role="button"
                tabIndex={0}
                onClick={() => menu.onNav(key)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    menu.onNav(key);
                  }
                }}
              >
                <Icon name={icon} />
                <span className="label">{label}</span>
                {key === "activity" && !!fresh && (
                  <span className="nav-pill" title={`${countText(fresh)} changes since your last visit`}>
                    {countText(fresh)}
                  </span>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
    </nav>
  );
}

// Re-renders once a minute so "synced 2m ago" does not freeze.
function useMinuteTick() {
  const [, set] = useState(0);
  useEffect(() => {
    const id = window.setInterval(() => set((n) => n + 1), 60_000);
    return () => window.clearInterval(id);
  }, []);
}

function SyncLine({ pid, readsEnabled }: { pid: string; readsEnabled: boolean }) {
  useMinuteTick();
  const { devices, synced } = useSyncLine(pid, readsEnabled);
  const parts: string[] = [];
  if (devices) parts.push(devices + (devices === 1 ? " device" : " devices"));
  if (synced) parts.push("synced " + agoText(synced));
  if (!parts.length) return null;
  return (
    <span id="sync-line">
      <span className="sync-dot" aria-hidden="true" />
      {parts.join(" · ")}
    </span>
  );
}

/* The always-visible search field. A click on the empty field opens the ⌘K
   palette, which searches files, projects and actions. Typing into it from
   the keyboard (Tab, then type) filters the tree in place (FileTree's
   TreeFilter); once a filter is showing, clicks edit it like any input, and
   ⌘↵ hands the query to the palette. */
export function SidebarSearch() {
  const { query } = useSidebarSearch();
  return (
    <div id="sb-search" className={query ? "filtering" : undefined}>
      <Icon name="search" />
      <input
        id="sb-search-input"
        type="text"
        value={query}
        placeholder="Search or jump to…"
        aria-label="Filter files, or press Enter to search everything"
        autoComplete="off"
        spellCheck={false}
        onMouseDown={(e) => {
          // Primary button only: right and middle clicks keep the browser's
          // context menu, autoscroll and paste.
          if (query || e.button !== 0) return;
          e.preventDefault();
          requestSearch();
          closeSidebarOnMobile();
        }}
        onChange={(e) => setFilterQuery(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown" || e.key === "ArrowUp") {
            e.preventDefault();
            moveFilterSel(e.key === "ArrowDown" ? 1 : -1);
          } else if (e.key === "Enter") {
            e.preventDefault();
            if (!query.trim() || e.metaKey || e.ctrlKey) requestSearch(query.trim());
            else activateFilterSel();
          } else if (e.key === "Escape") {
            e.preventDefault();
            e.stopPropagation();
            if (query) clearFilter();
            else e.currentTarget.blur();
          }
        }}
      />
      {query ? (
        <button className="sb-kbd" aria-label="Clear filter" onClick={clearFilter}>
          esc
        </button>
      ) : (
        <button
          className="sb-kbd"
          aria-label="Open search"
          title="Search everything"
          onClick={() => {
            requestSearch();
            closeSidebarOnMobile();
          }}
        >
          {isMac ? "⌘K" : "Ctrl K"}
        </button>
      )}
    </div>
  );
}

/* The project's nav as icon-only rail buttons, badges kept. */
export function ProjectRailNav({ pid, menu }: { pid: string; menu: ProjectMenu }) {
  const fresh = useActivityCount(pid);
  return (
    <>
      {NAV.map(([key, label, icon]) => (
        <RailButton
          key={key}
          id={"rail-" + key}
          icon={icon}
          label={label}
          hint={key === "activity" && fresh ? countText(fresh) + " new" : undefined}
          badge={key === "activity" && fresh ? countText(fresh) : undefined}
          active={menu.active === key}
          onClick={() => menu.onNav(key)}
        />
      ))}
    </>
  );
}
