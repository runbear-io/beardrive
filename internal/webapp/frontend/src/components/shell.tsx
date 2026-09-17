import type { ReactNode } from "react";
import { requestSearch } from "../search";
import { isMac } from "../util";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  Beaker,
  BookOpen,
  Briefcase,
  Bug,
  Calendar,
  Check,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Clock,
  Code,
  Compass,
  Copy,
  CreditCard,
  Database,
  Download,
  Ellipsis,
  FileText,
  Flag,
  Folder,
  Gauge,
  Heart,
  Image,
  LayoutDashboard,
  Lightbulb,
  Gavel,
  Globe,
  GraduationCap,
  History,
  Link,
  Lock,
  LogOut,
  Maximize2,
  Megaphone,
  Menu,
  Minimize2,
  Music,
  Package,
  PanelLeft,
  PenLine,
  Plus,
  Printer,
  Rocket,
  Search,
  Settings,
  Share2,
  Shield,
  SquareTerminal,
  Star,
  Trash2,
  TriangleAlert,
  Upload,
  Users,
  Plug,
  Wrench,
  X,
  type LucideIcon,
} from "lucide-react";

// The app's fixed layout: off-canvas sidebar (mobile: body.sb-open toggles
// it), topbar, and the content pane. Ids and classes match the classic app
// so style.css applies unchanged.

export const SIDEBAR_COLLAPSED_KEY = "bdrive.sbCollapsed";

// The sidebar has two hidden states, one per layout. Below the breakpoint it
// is a modal off-canvas drawer (body.sb-open); at desktop widths it is a fixed
// column that collapses for a distraction-free reading width
// (body.sb-collapsed, remembered per browser). One control and one shortcut
// (⌘B / Ctrl+B) drive whichever applies at the current width.
export function toggleSidebar() {
  if (window.innerWidth <= SIDEBAR_BREAKPOINT) toggleDrawer();
  else setCollapsed(!document.body.classList.contains("sb-collapsed"));
}

function toggleDrawer() {
  const opening = !document.body.classList.contains("sb-open");
  document.body.classList.toggle("sb-open");
  syncSidebarInert();
  // The drawer is modal for the mouse (scrim, click-to-dismiss); make it modal
  // for the keyboard too. Without this, Tab walks from the drawer into the
  // content BEHIND the scrim, where clicks do nothing — and because the menu
  // button follows the sidebar in DOM order, the nav was only reachable by
  // tabbing backwards.
  if (opening) {
    document.getElementById("sidebar")?.querySelector<HTMLElement>(FOCUSABLE)?.focus();
  } else {
    document.getElementById("menu-btn")?.focus();
  }
}

function setCollapsed(collapsed: boolean) {
  document.body.classList.toggle("sb-collapsed", collapsed);
  // A preference is never worth a broken page: storage throws in private modes
  // and wherever it is disabled, so swallow (see util.lastProject).
  try {
    localStorage.setItem(SIDEBAR_COLLAPSED_KEY, collapsed ? "1" : "0");
  } catch {
    /* ignore */
  }
  syncSidebarInert();
}

const FOCUSABLE = 'a[href], button:not(:disabled), select, input, [tabindex]:not([tabindex="-1"])';
export function closeSidebarOnMobile() {
  const wasOpen = document.body.classList.contains("sb-open");
  document.body.classList.remove("sb-open");
  syncSidebarInert();
  // Closing returns focus to the control that opened it — otherwise focus
  // falls to <body> and the next Tab restarts from the top of the document.
  if (wasOpen && window.innerWidth <= SIDEBAR_BREAKPOINT) {
    document.getElementById("menu-btn")?.focus();
  }
}

// ⌘B is Bold inside a text editor, so the shortcut yields whenever the event
// originates in an editable surface: a form field, the ProseMirror visual
// editor (contentEditable), or the CodeMirror source editor.
function isEditingTarget(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null;
  if (!el || typeof el.tagName !== "string") return false;
  const tag = el.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return true;
  if (el.isContentEditable) return true;
  return !!el.closest?.(".cm-editor");
}

// The sidebar is off-canvas below the breakpoint — translated out of view, but
// still in the DOM, so its nine controls stayed in the tab order with no
// visible focus indicator: nine dead stops before the first thing on screen
// (WCAG 2.4.7). `inert` removes focusability and AT exposure together, which
// is exactly the "it isn't there right now" that the transform — or, collapsed
// at desktop width, the display:none — already implies.
const SIDEBAR_BREAKPOINT = 900; // must match the off-canvas media query in style.css

export function syncSidebarInert() {
  const el = document.getElementById("sidebar");
  if (!el) return;
  const mobile = window.innerWidth <= SIDEBAR_BREAKPOINT;
  const drawerOpen = document.body.classList.contains("sb-open");
  const collapsed = document.body.classList.contains("sb-collapsed");
  const hidden = mobile ? !drawerOpen : collapsed;
  if (hidden) el.setAttribute("inert", "");
  else el.removeAttribute("inert");
  // The mirror: while the drawer is over the page, the page is not reachable.
  const main = document.getElementById("main");
  if (main) {
    if (drawerOpen && mobile) main.setAttribute("inert", "");
    else main.removeAttribute("inert");
  }
  el.setAttribute("aria-modal", String(drawerOpen && mobile));
  // The trigger's state lives in body classes rather than React state, so it
  // is declared from here — the one place that always runs when it changes.
  // aria-expanded answers "is the sidebar on screen": the drawer when narrow,
  // the collapse state when wide.
  document.getElementById("menu-btn")?.setAttribute("aria-expanded", String(!hidden));
}

if (typeof window !== "undefined") {
  // Restore the remembered collapse before React mounts, so the first paint at
  // desktop width is already correct (the mobile drawer always loads closed).
  try {
    if (document.body && localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === "1") {
      document.body.classList.add("sb-collapsed");
    }
  } catch {
    /* ignore */
  }
  window.addEventListener("resize", syncSidebarInert);
  window.addEventListener("keydown", (e) => {
    // Escape closes the drawer, the way every other overlay in the app does.
    if (e.key === "Escape" && document.body.classList.contains("sb-open")) {
      closeSidebarOnMobile();
      return;
    }
    // ⌘B / Ctrl+B toggles the sidebar — the VS Code convention, and the one
    // Windows users already know (Ctrl+B), since there is no ⌘ key there.
    if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === "b") {
      if (isEditingTarget(e.target)) return; // ⌘B is Bold in an editor
      e.preventDefault();
      toggleSidebar();
    }
  });
}

// Icons are lucide (lucide.dev) components behind the historical sprite
// names, so call sites keep the tiny `<Icon name>` API and style.css's
// `.ico` sizing/stroke rules apply unchanged.
const ICONS: Record<string, LucideIcon> = {
  alert: TriangleAlert,
  card: CreditCard,
  check: Check,
  chev: ChevronRight,
  chevd: ChevronDown,
  chevl: ChevronLeft,
  clock: Clock,
  copy: Copy,
  doc: FileText,
  dots: Ellipsis,
  download: Download,
  expand: Maximize2,
  folder: Folder,
  dashboard: LayoutDashboard,
  gear: Settings,
  globe: Globe,
  hist: History,
  link: Link,
  lock: Lock,
  menu: Menu,
  plug: Plug,
  plus: Plus,
  power: LogOut,
  printer: Printer,
  search: Search,
  share: Share2,
  sidebar: PanelLeft,
  shield: Shield,
  shrink: Minimize2,
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

// The icons a project may choose from, keyed by their real lucide name (so
// what lands in the database is a public, portable identifier — not one of
// ICONS' historical sprite aliases). Deliberately a curated shortlist: these
// are named imports, which is what lets Vite tree-shake the other ~1500
// lucide icons out of the bundle. Adding one here is the whole change; the
// server only ever validates the shape of the string.
export const PROJECT_ICONS: Record<string, LucideIcon> = {
  folder: Folder,
  "book-open": BookOpen,
  "file-text": FileText,
  "pen-line": PenLine,
  users: Users,
  briefcase: Briefcase,
  megaphone: Megaphone,
  rocket: Rocket,
  lightbulb: Lightbulb,
  flag: Flag,
  star: Star,
  heart: Heart,
  code: Code,
  "square-terminal": SquareTerminal,
  bug: Bug,
  wrench: Wrench,
  database: Database,
  package: Package,
  beaker: Beaker,
  gauge: Gauge,
  shield: Shield,
  lock: Lock,
  gavel: Gavel,
  globe: Globe,
  compass: Compass,
  calendar: Calendar,
  clock: Clock,
  "graduation-cap": GraduationCap,
  image: Image,
  music: Music,
};

// ProjectIcon renders a project's chosen glyph. One fallback, one place:
// no icon set, or a name this build doesn't know (hand-written into storage,
// or dropped from the list later) → the folder placeholder.
export function ProjectIcon({ name, className }: { name?: string; className?: string }) {
  // Object.hasOwn, not `?? Folder`: the server validates an icon's SHAPE only
  // (iconRe, deliberately — adding an icon needs no server change), and
  // "constructor" has that shape while resolving through Object.prototype to a
  // function. The nullish fallback therefore never fired and React was handed
  // Object as a component, which white-screens the whole SPA for every member
  // of the org on every route, with no way back through the UI.
  const key = name ?? "";
  const C = Object.hasOwn(PROJECT_ICONS, key) ? PROJECT_ICONS[key] : Folder;
  return <C className={className} aria-hidden="true" />;
}

/* The BearDrive mark: a rail and two blocks — the letter B built from
   rectangles only, and the same shape as the product (a spine with volumes
   hanging off it). One fill, so `currentColor` themes it everywhere: the
   sidebar, the favicon, flat ink. Kept identical to the landing page's
   Mark.astro; if one changes, change both. */
export function Mark({ size = 22 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 32 32"
      fill="currentColor"
      role="img"
      aria-label="BearDrive"
    >
      <rect x="4" y="4" width="5.6" height="24" />
      <rect x="11.2" y="4" width="14.4" height="11.2" />
      <rect x="11.2" y="16.8" width="16.8" height="11.2" />
    </svg>
  );
}

/* The column system, in one place. `#content` owns scrolling and the page
   gutter; `<Page>` owns width and centering — nothing else may set either.
   Three widths cover every view: `read` for rendered files only (markdown
   prose), `app` for every structured view (guide, listings, history,
   dashboard, settings, admin), `wide` for content that is itself a
   page (a rendered HTML file in its frame). `read` and `app` both resolve
   to Tailwind's md (768px); they stay separate classes because the file
   view carries markdown typography and the widths may diverge again.
   Views used to declare their own
   max-width (560px to unbounded, half of them uncentered), so no two routes
   shared a column.

   The line: <Page> sets the COLUMN, a view may still cap its own MEASURE
   (`.nf-sub`, a chart's design width). What a view must never do is declare
   a page-level width — that is how the tiers drifted apart the first time.
   Widening a column also never means scaling content up: Insights sits at
   `app` and its charts cap themselves, because at `wide` the viewBox SVGs
   just zoomed (a 10.5px label painted at 21px). */
export type PageWidth = "read" | "app" | "wide";

export function Page(props: {
  width?: PageWidth;
  className?: string; // a view's own styling hook (e.g. markdown typography)
  children: ReactNode;
}) {
  const cls = ["page", props.width ?? "app", props.className].filter(Boolean).join(" ");
  return <div className={cls}>{props.children}</div>;
}

// Applies the initial inert state as soon as the element exists, so the first
// paint at a small width is already correct.
function sidebarInert(el: HTMLElement | null) {
  if (el) syncSidebarInert();
}

export function AppShell(props: {
  vault: ReactNode;
  projectsNav?: ReactNode;
  tree?: ReactNode;
  orgBar?: ReactNode;
  topbar: ReactNode;
  // Fullscreen's Exit control. It lives here, outside the topbar it hides,
  // and is positioned over the content — the HTML sandbox and the browser's
  // PDF viewer swallow every key event, so a control painted on top of them
  // is the only exit that works from inside one.
  exit?: ReactNode;
  contentRef?: React.Ref<HTMLElement>;
  onContentScroll?: () => void;
  children: ReactNode;
}) {
  return (
    <>
      <div id="sb-backdrop" onClick={closeSidebarOnMobile} />
      <aside id="sidebar" ref={sidebarInert}>
        {props.vault}
        {props.projectsNav}
        {props.tree ?? <nav id="tree" aria-label="Files" />}
        {props.orgBar}
      </aside>
      <main id="main">
        {props.topbar}
        {props.exit}
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
  /* BearDrive is pre-1.0 and says so next to its own wordmark. Callers pass
     this only when the lockup IS the BearDrive brand — a hub that set its own
     `brand` is labelling somebody else's product, and "Acme Docs Beta" is a
     claim we have no business making for them. */
  beta?: boolean;
}) {
  const { name, onHome, showSignout, search, beta } = props;
  return (
    <header id="vault">
      <span id="vault-badge">
        <Mark size={22} />
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
      {beta && <span id="vault-beta">Beta</span>}
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

export function Topbar(props: { crumb?: ReactNode; meta?: ReactNode; actions?: ReactNode; nav?: ReactNode }) {
  return (
    <header id="topbar">
      <Tooltip delayDuration={150}>
        <TooltipTrigger asChild>
          <button
            id="menu-btn"
            className="icon-btn"
            aria-label="Toggle sidebar"
            aria-controls="sidebar"
            aria-expanded="false"
            onClick={toggleSidebar}
          >
            <Icon name="sidebar" />
          </button>
        </TooltipTrigger>
        <TooltipContent className="tipcard" sideOffset={6}>
          Toggle sidebar <kbd>{isMac ? "⌘B" : "Ctrl+B"}</kbd>
        </TooltipContent>
      </Tooltip>
      {props.nav}
      <span id="crumb">{props.crumb}</span>
      <span id="meta">{props.meta}</span>
      {props.actions}
    </header>
  );
}
