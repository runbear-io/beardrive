import { useEffect, useState, type CSSProperties, type ReactNode } from "react";
import { ChevronsUpDown, Monitor, Moon, Sun } from "lucide-react";
import { linkProps, navigate } from "../nav";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Icon, ProjectIcon, RailButton, closeSidebarOnMobile } from "./shell";
import type { Org, Project } from "../api/types";
import { requestSearch } from "../search";
import { isMac } from "../util";
import { useTheme, type ThemePref } from "@/lib/theme";
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

// A .proj-mark is tinted from its color (style.css), so the glyph and the
// square read on both themes without a per-theme palette.
export function markStyle(name: string): CSSProperties {
  return { "--mk": projColor(name) } as CSSProperties;
}

export type NavKey = "home" | "activity" | "insights" | "settings";

export interface ProjectMenu {
  active: NavKey | null;
  onNav: (key: NavKey) => void;
}

// Settings is a page like the others (it has a route and an active state) but
// it lives in the project menu: it is the least visited of the four, and the
// sidebar's rows are kept for the pages people return to.
const NAV: ReadonlyArray<readonly [NavKey, string, string]> = [
  ["home", "Home", "home"],
  ["activity", "Activity", "activity"],
  ["insights", "Insights", "insights"],
];

const countText = (n: number) => (n >= ACTIVITY_CAP ? ACTIVITY_CAP - 1 + "+" : String(n));

// Everything about the signed-in session that the project menu shows below the
// project list: the org, hub administration, billing, theme and sign-out.
export interface AccountMenu {
  me: { email: string; name: string };
  org: Org | null;
  orgActive?: boolean; // the org page is the open surface
  admin?: { pending: number; onClick: () => void }; // hub admins only
  billing?: { plan: string; url: string }; // managed deployments only
  mcp?: boolean; // the hub serves /mcp, so agent connections can be managed
  // Desktop app: sign-out is a sidecar call, not the hub's /auth/logout page.
  signOut?: () => void;
  /* BearDrive is pre-1.0 and says so. Callers pass beta only when the brand
     IS BearDrive — a hub that set its own `brand` is labelling somebody
     else's product, and "Acme Docs Beta" is a claim we have no business
     making for them. */
  brand: string;
  beta?: boolean;
}

export function ProjectNav({
  projects,
  currentId,
  menu,
  account,
  onNew,
}: {
  projects: Project[];
  currentId?: string;
  menu?: ProjectMenu;
  account?: AccountMenu;
  // Opening the create dialog belongs to HubApp: three things ask for it now
  // (this menu, the empty state's button, and the auto-open when a signed-in
  // account has no projects at all), and one owner beats three copies.
  onNew: () => void;
}) {
  const current = projects.find((p) => p.id === currentId);
  const fresh = useActivityCount(menu ? currentId : undefined);

  return (
    <nav id="projects" aria-label="Projects">
      <SidebarMenu
        projects={projects}
        current={current}
        menu={menu}
        account={account}
        onNew={onNew}
        trigger={
          <button
            id="project-select"
            className="proj-trigger"
            aria-label={`Project and account menu — current project: ${current?.name ?? "none"}`}
            title={current?.name}
          >
            <span className="proj-mark" aria-hidden="true" style={current ? markStyle(current.name) : undefined}>
              <ProjectIcon name={current?.icon} />
            </span>
            <span className="proj-name">{current ? current.name : projects.length ? "Select a project" : "No projects yet"}</span>
            <ChevronsUpDown className="ico updown" aria-hidden="true" />
          </button>
        }
      />
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
                aria-current={menu.active === key ? "page" : undefined}
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
                  <span className="nav-count" title={`${countText(fresh)} changes since your last visit`}>
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

// The one menu behind the project name (and the rail's project mark): switch
// or create a project, this project's and this org's settings, then the
// session — ordered from the narrowest scope to the widest, so what is used
// most sits nearest the trigger and sign-out sits at the far end.
//
// Radix owns open/dismiss (Escape, outside click, focus). The open state is
// ours because several entries are links: linkProps calls preventDefault to
// route internally, and Radix composes its own select handler with
// checkForDefaultPrevented, so that handler never runs and the menu would stay
// open on top of the page it just opened.
export function SidebarMenu({
  projects,
  current,
  menu,
  account,
  onNew,
  trigger,
  side = "bottom",
}: {
  projects: Project[];
  current?: Project;
  menu?: ProjectMenu;
  account?: AccountMenu;
  onNew: () => void;
  trigger: ReactNode;
  side?: "bottom" | "right";
}) {
  const [open, setOpen] = useState(false);
  const close = () => setOpen(false);
  const { pref, setPref } = useTheme();
  const org = account?.org ?? null;
  const orgLink = org ? linkProps(org.manage_url) : null;
  const billingLink = account?.billing ? linkProps(account.billing.url) : null;
  const connectionsLink = linkProps("/connections");
  const orgExternal = !!org && !org.manage_url.startsWith("/");

  return (
    <DropdownMenu modal={false} open={open} onOpenChange={setOpen}>
      <DropdownMenuTrigger asChild>{trigger}</DropdownMenuTrigger>
      <DropdownMenuContent
        id="project-menu"
        className="sb-menu"
        side={side}
        align="start"
        sideOffset={side === "right" ? 8 : 4}
      >
        <div className="sb-menu-projects">
          {projects.map((p) => (
            <DropdownMenuItem
              key={p.id}
              data-project={p.id}
              aria-current={p.id === current?.id ? "true" : undefined}
              onSelect={() => {
                if (p.id !== current?.id) navigate("/" + p.id);
                closeSidebarOnMobile();
              }}
            >
              <span className="proj-mark" aria-hidden="true" style={markStyle(p.name)}>
                <ProjectIcon name={p.icon} />
              </span>
              <span className="label">{p.name}</span>
              {p.id === current?.id && <Icon name="check" />}
            </DropdownMenuItem>
          ))}
        </div>
        <DropdownMenuItem id="menu-new-project" onSelect={onNew}>
          <span className="proj-mark add" aria-hidden="true">
            <Icon name="plus" />
          </span>
          <span className="label dim">New project</span>
        </DropdownMenuItem>

        {(menu || org || account?.billing || account?.admin) && <DropdownMenuSeparator />}
        {menu && current && (
          <DropdownMenuItem
            id="nav-settings"
            aria-current={menu.active === "settings" ? "page" : undefined}
            onSelect={() => menu.onNav("settings")}
          >
            <Icon name="gear" />
            <span className="label">Project settings</span>
          </DropdownMenuItem>
        )}
        {org && (
          <DropdownMenuItem asChild>
            {/* org.manage_url: this hub's own org page when it owns its orgs,
                the identity provider's page when it does not. The server
                decides; nothing here branches on the answer. */}
            <a
              id="menu-org-settings"
              aria-current={account?.orgActive ? "page" : undefined}
              {...orgLink}
              onClick={(e) => {
                orgLink?.onClick?.(e);
                close();
              }}
            >
              <Icon name="building" />
              <span className="label">Organization settings</span>
              <span className="meta">{org.name}</span>
              {orgExternal && (
                <>
                  <span className="ext" aria-hidden="true">↗</span>
                  <span className="sr-only"> (opens in a new tab)</span>
                </>
              )}
            </a>
          </DropdownMenuItem>
        )}
        {account?.billing && (
          <DropdownMenuItem asChild>
            <a
              id="menu-billing"
              {...billingLink}
              onClick={(e) => {
                billingLink?.onClick?.(e);
                close();
              }}
            >
              <Icon name="card" />
              <span className="label">Billing</span>
              <span className="ps-chip plan-chip">{account.billing.plan}</span>
            </a>
          </DropdownMenuItem>
        )}
        {account?.admin && (
          <DropdownMenuItem id="menu-hub-admin" onSelect={account.admin.onClick}>
            <Icon name="shield" />
            <span className="label">Signup &amp; access</span>
            {!!account.admin.pending && <span className="meta">{account.admin.pending}</span>}
          </DropdownMenuItem>
        )}

        <DropdownMenuSeparator />
        {account && <DropdownMenuLabel className="sb-menu-label">{account.me.email}</DropdownMenuLabel>}
        <div className="sb-theme">
          <span className="label">Theme</span>
          <DropdownMenuRadioGroup
            className="seg"
            aria-label="Theme"
            value={pref}
            onValueChange={(v) => setPref(v as ThemePref)}
          >
            {THEMES.map(({ value, label, Glyph }) => (
              // preventDefault keeps the menu open: the page re-themes behind
              // it, so the choice is visible before the user commits to it.
              <DropdownMenuRadioItem
                key={value}
                id={`menu-theme-${value}`}
                className="seg-item"
                value={value}
                aria-label={label}
                title={label}
                onSelect={(e) => e.preventDefault()}
              >
                <Glyph className="ico" aria-hidden="true" />
              </DropdownMenuRadioItem>
            ))}
          </DropdownMenuRadioGroup>
        </div>
        {account?.mcp && (
          <DropdownMenuItem asChild>
            {/* An in-app route (/connections), account-level: one grant can
                span several projects, so it has no project to live under. */}
            <a
              id="menu-connections"
              {...connectionsLink}
              onClick={(e) => {
                connectionsLink.onClick?.(e);
                close();
              }}
            >
              <Icon name="plug" />
              <span className="label">Connected agents</span>
            </a>
          </DropdownMenuItem>
        )}
        {account &&
          (account.signOut ? (
            <DropdownMenuItem id="signout" onSelect={account.signOut}>
              <Icon name="power" />
              <span className="label">Log out</span>
            </DropdownMenuItem>
          ) : (
            <DropdownMenuItem asChild>
              <a id="signout" href="/auth/logout">
                <Icon name="power" />
                <span className="label">Log out</span>
              </a>
            </DropdownMenuItem>
          ))}
        {account && (
          <div id="menu-brand" className="sb-menu-brand">
            {account.brand}
            {account.beta && <span className="beta">Beta</span>}
          </div>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

const THEMES: { value: ThemePref; label: string; Glyph: typeof Monitor }[] = [
  { value: "system", label: "System", Glyph: Monitor },
  { value: "light", label: "Light", Glyph: Sun },
  { value: "dark", label: "Dark", Glyph: Moon },
];

// Re-renders once a minute so "synced 2m ago" does not freeze.
function useMinuteTick() {
  const [, set] = useState(0);
  useEffect(() => {
    const id = window.setInterval(() => set((n) => n + 1), 60_000);
    return () => window.clearInterval(id);
  }, []);
}

/* The sidebar's last row: this project's sync state, and the way to the
   install page. With nothing to report yet it says what to do instead
   ("Connect a device") — the state a new project is in, and the reader who
   most needs the page. */
export function SyncFooter({
  pid,
  readsEnabled,
  active,
  onOpen,
}: {
  pid: string;
  readsEnabled: boolean;
  active: boolean;
  onOpen: () => void;
}) {
  useMinuteTick();
  const { devices, synced } = useSyncLine(pid, readsEnabled);
  const parts: string[] = [];
  if (devices) parts.push(devices + (devices === 1 ? " device" : " devices"));
  if (synced) parts.push("synced " + agoText(synced));
  const row = (
    <button id="nav-install" className={"row sb-sync" + (active ? " active" : "")} onClick={onOpen}>
      {parts.length ? <span className="sync-dot" aria-hidden="true" /> : <Icon name="laptop" />}
      <span id="sync-line" className="label">
        {parts.length ? parts.join(" · ") : "Connect a device"}
      </span>
    </button>
  );
  if (!parts.length) return <div className="sb-foot">{row}</div>;
  return (
    <div className="sb-foot">
      <Tooltip delayDuration={300}>
        <TooltipTrigger asChild>{row}</TooltipTrigger>
        <TooltipContent className="tipcard" side="top" sideOffset={6}>
          Connect a device
        </TooltipContent>
      </Tooltip>
    </div>
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
        placeholder="Search"
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
        <Tooltip delayDuration={150}>
          <TooltipTrigger asChild>
            <button
              id="search-btn"
              className="sb-kbd"
              aria-label="Search"
              onClick={() => {
                requestSearch();
                closeSidebarOnMobile();
              }}
            >
              {isMac ? "⌘K" : "Ctrl K"}
            </button>
          </TooltipTrigger>
          <TooltipContent className="tipcard" sideOffset={6}>
            Search everything <kbd>{isMac ? "⌘K" : "Ctrl+K"}</kbd>
          </TooltipContent>
        </Tooltip>
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

/* The rail's top slot: the current project's mark, opening the same menu as
   the expanded sidebar's project name. */
export function ProjectRailMenu(props: {
  projects: Project[];
  current: Project;
  menu: ProjectMenu;
  account?: AccountMenu;
  onNew: () => void;
}) {
  return (
    <SidebarMenu
      {...props}
      side="right"
      trigger={
        <button id="rail-project" className="rail-btn rail-proj" aria-label={`Project and account menu — ${props.current.name}`}>
          <span className="proj-mark" aria-hidden="true" style={markStyle(props.current.name)}>
            <ProjectIcon name={props.current.icon} />
          </span>
        </button>
      }
    />
  );
}
