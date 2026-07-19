import { useEffect, useRef, useState } from "react";
import type { Org } from "../api/types";
import { Icon } from "./shell";
import { projColor } from "./ProjectNav";

// The sidebar footer is the account row: avatar, name, email. Clicking it
// opens a popover with the workspace (org) and account actions — settings,
// hub administration for admins, and sign-out.
export function AccountBar({
  me,
  org,
  admin,
  onOrgSettings,
}: {
  me: { email: string; name: string };
  org: Org | null;
  admin?: { pending: number; onClick: () => void }; // hub admins only
  onOrgSettings: (org: Org) => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const display = me.name || me.email;
  return (
    <footer id="accountbar" ref={ref}>
      {open && (
        <div id="account-menu" role="menu" aria-label="Account menu">
          {org && (
            <>
              <div className="menu-sec">Organization</div>
              <button
                id="menu-org-settings"
                role="menuitem"
                onClick={() => {
                  setOpen(false);
                  onOrgSettings(org);
                }}
              >
                <Icon name="gear" />
                <span>
                  <b>{org.name}</b> Settings
                </span>
              </button>
            </>
          )}
          {admin && (
            <>
              <div className="menu-sec">Hub</div>
              <button
                id="menu-hub-admin"
                role="menuitem"
                onClick={() => {
                  setOpen(false);
                  admin.onClick();
                }}
              >
                <Icon name="shield" />
                <span>Signup &amp; access{admin.pending ? ` · ${admin.pending}` : ""}</span>
              </button>
            </>
          )}
          <div className="menu-sec">Account</div>
          <a id="signout" role="menuitem" href="/auth/logout">
            <Icon name="power" />
            <span>Log out</span>
          </a>
        </div>
      )}
      <button
        id="account-btn"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        <span className="avatar" style={{ background: projColor(me.email) }} aria-hidden="true">
          {(display.trim()[0] || "?").toUpperCase()}
        </span>
        <span className="acct">
          <b>{display}</b>
          {me.name && <small>{me.email}</small>}
        </span>
        <Icon name="chev" />
      </button>
    </footer>
  );
}
