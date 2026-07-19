import type { ReactNode } from "react";
import { requestSearch } from "../search";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  Check,
  ChevronDown,
  ChevronRight,
  Clock,
  Copy,
  Download,
  Ellipsis,
  FileText,
  Folder,
  LayoutDashboard,
  Globe,
  History,
  Link,
  Lock,
  LogOut,
  Menu,
  Plus,
  Search,
  Settings,
  Share2,
  Shield,
  SquareTerminal,
  Trash2,
  TriangleAlert,
  Upload,
  Users,
  X,
  type LucideIcon,
} from "lucide-react";

// The app's fixed layout: off-canvas sidebar (mobile: body.sb-open toggles
// it), topbar, and the content pane. Ids and classes match the classic app
// so style.css applies unchanged.

export function toggleSidebar() {
  document.body.classList.toggle("sb-open");
}
export function closeSidebarOnMobile() {
  document.body.classList.remove("sb-open");
}

// Icons are lucide (lucide.dev) components behind the historical sprite
// names, so call sites keep the tiny `<Icon name>` API and style.css's
// `.ico` sizing/stroke rules apply unchanged.
const ICONS: Record<string, LucideIcon> = {
  alert: TriangleAlert,
  check: Check,
  chev: ChevronRight,
  chevd: ChevronDown,
  clock: Clock,
  copy: Copy,
  doc: FileText,
  dots: Ellipsis,
  download: Download,
  folder: Folder,
  dashboard: LayoutDashboard,
  gear: Settings,
  globe: Globe,
  hist: History,
  link: Link,
  lock: Lock,
  menu: Menu,
  plus: Plus,
  power: LogOut,
  search: Search,
  share: Share2,
  shield: Shield,
  terminal: SquareTerminal,
  trash: Trash2,
  upload: Upload,
  users: Users,
  x: X,
};

export function Icon({ name }: { name: string }) {
  const C = ICONS[name];
  return C ? <C className="ico" aria-hidden="true" /> : null;
}

/* The column system, in one place. `#content` owns scrolling and the page
   gutter; `<Page>` owns width and centering — nothing else may set either.
   Three widths cover every view: `read` for prose and listings (line length
   rules), `app` for structured views, `wide` for data-dense ones. Views used
   to declare their own max-width (560px to unbounded, half of them
   uncentered), so no two routes shared a column. */
export type PageWidth = "read" | "app" | "wide";

export function Page(props: {
  width?: PageWidth;
  className?: string; // a view's own styling hook (e.g. markdown typography)
  children: ReactNode;
}) {
  const cls = ["page", props.width ?? "app", props.className].filter(Boolean).join(" ");
  return <div className={cls}>{props.children}</div>;
}

export function AppShell(props: {
  vault: ReactNode;
  projectsNav?: ReactNode;
  tree?: ReactNode;
  orgBar?: ReactNode;
  topbar: ReactNode;
  contentRef?: React.Ref<HTMLElement>;
  onContentScroll?: () => void;
  children: ReactNode;
}) {
  return (
    <>
      <div id="sb-backdrop" onClick={closeSidebarOnMobile} />
      <aside id="sidebar">
        {props.vault}
        {props.projectsNav}
        {props.tree ?? <nav id="tree" aria-label="Files" />}
        {props.orgBar}
      </aside>
      <main id="main">
        {props.topbar}
        <article
          id="content"
          ref={props.contentRef}
          onScroll={props.onContentScroll}
        >
          {props.children}
        </article>
      </main>
    </>
  );
}

export function VaultHeader(props: {
  name: string;
  onHome?: () => void; // hub: the project name doubles as a home link
  showSignout?: boolean; // volume mode: sign-out stays in the header (no account bar)
  search?: boolean; // icon-only ⌘K search trigger beside the brand
}) {
  const { name, onHome, showSignout, search } = props;
  return (
    <header id="vault">
      <span id="vault-badge" aria-hidden="true">
        🐻
      </span>
      <span
        id="vault-name"
        className={onHome ? "vault-link" : undefined}
        onClick={onHome}
        role={onHome ? "button" : undefined}
        tabIndex={onHome ? 0 : undefined}
        onKeyDown={(e) => {
          if (onHome && (e.key === "Enter" || e.key === " ")) {
            e.preventDefault();
            onHome();
          }
        }}
      >
        {name}
      </span>
      <div className="vault-actions">
        {search && (
          <Tooltip delayDuration={150}>
            <TooltipTrigger asChild>
              <button
                id="search-btn"
                className="icon-btn2"
                aria-label="Search"
                onClick={() => {
                  requestSearch();
                  closeSidebarOnMobile();
                }}
              >
                <Icon name="search" />
              </button>
            </TooltipTrigger>
            <TooltipContent className="tipcard" sideOffset={6}>
              Search <kbd>⌘K</kbd>
            </TooltipContent>
          </Tooltip>
        )}
        {showSignout && (
          <a id="signout" href="/auth/logout" title="Sign out" aria-label="Sign out">
            <Icon name="power" />
          </a>
        )}
      </div>
    </header>
  );
}

export function Topbar(props: { crumb?: ReactNode; meta?: ReactNode; actions?: ReactNode }) {
  return (
    <header id="topbar">
      <button id="menu-btn" className="icon-btn" title="Menu" aria-label="Menu" onClick={toggleSidebar}>
        <Icon name="menu" />
      </button>
      <span id="crumb">{props.crumb}</span>
      <span id="meta">{props.meta}</span>
      {props.actions}
    </header>
  );
}
