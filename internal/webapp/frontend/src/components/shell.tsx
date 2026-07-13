import type { ReactNode } from "react";

// The app's fixed layout: off-canvas sidebar (mobile: body.sb-open toggles
// it), topbar, and the content pane. Ids and classes match the classic app
// so style.css applies unchanged.

export function toggleSidebar() {
  document.body.classList.toggle("sb-open");
}
export function closeSidebarOnMobile() {
  document.body.classList.remove("sb-open");
}

export function Icon({ name }: { name: string }) {
  return (
    <svg className="ico" aria-hidden="true">
      <use href={`#i-${name}`} />
    </svg>
  );
}

export function AppShell(props: {
  vault: ReactNode;
  projectsNav?: ReactNode;
  tree?: ReactNode;
  orgBar?: ReactNode;
  topbar: ReactNode;
  contentClass?: string;
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
        <article id="content" className={props.contentClass ?? "markdown"}>
          {props.children}
        </article>
      </main>
    </>
  );
}

export function VaultHeader(props: {
  name: string;
  onHome?: () => void; // hub: the project name doubles as a home link
  showSignout: boolean;
  admin?: { pending: number; onClick: () => void }; // hub admins only
  gear?: { onClick: () => void }; // org owners: manage organization
}) {
  const { name, onHome, showSignout, admin, gear } = props;
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
        {gear && (
          <button
            id="settings-btn"
            className="icon-btn2"
            title="Manage organization"
            aria-label="Manage organization"
            onClick={gear.onClick}
          >
            <Icon name="users" />
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
