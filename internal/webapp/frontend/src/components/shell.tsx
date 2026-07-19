import type { ReactNode } from "react";
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
  trash: Trash2,
  upload: Upload,
  users: Users,
  x: X,
};

export function Icon({ name }: { name: string }) {
  const C = ICONS[name];
  return C ? <C className="ico" aria-hidden="true" /> : null;
}

export function AppShell(props: {
  vault: ReactNode;
  projectsNav?: ReactNode;
  tree?: ReactNode;
  orgBar?: ReactNode;
  topbar: ReactNode;
  contentClass?: string;
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
          className={props.contentClass ?? "markdown"}
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
  showSignout?: boolean; // volume mode: sign-out stays in the header (no org bar)
  admin?: { pending: number; onClick: () => void }; // hub admins only
  projectSettings?: { onClick: () => void }; // hub: settings for the open project
}) {
  const { name, onHome, showSignout, admin, projectSettings } = props;
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
        {admin && (
          <button
            id="adminbar"
            className="adminbar"
            title={
              "Hub administration — signup policy" +
              (admin.pending ? " and pending approvals" : "")
            }
            onClick={admin.onClick}
          >
            <Icon name="shield" />
            <span>Admin{admin.pending ? " · " + admin.pending : ""}</span>
          </button>
        )}
        {projectSettings && (
          <button
            id="project-settings-btn"
            className="icon-btn2"
            title="Project settings"
            aria-label="Project settings"
            onClick={projectSettings.onClick}
          >
            <Icon name="gear" />
          </button>
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
