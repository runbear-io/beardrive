import { useState } from "react";
import type { Org } from "../api/types";
import { linkProps } from "../nav";
import { Icon } from "./shell";
import { projColor } from "./ProjectNav";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";

// The sidebar footer is the account row: avatar, name, email. Clicking it
// opens a menu with the workspace (org) and account actions — settings,
// hub administration for admins, and sign-out. Radix owns open/dismiss
// behavior (Escape, outside click, focus).
//
// The org entry is a plain link to org.manage_url: this hub's own org page
// when it owns its orgs, the identity provider's page when it does not. The
// server decides; nothing here branches on the answer.
export function AccountBar({
  me,
  org,
  admin,
  orgActive,
}: {
  me: { email: string; name: string };
  org: Org | null;
  admin?: { pending: number; onClick: () => void }; // hub admins only
  orgActive?: boolean; // the org page is the open surface
}) {
  const display = me.name || me.email;
  // The menu's open state is ours because the org entry is a link: linkProps
  // calls preventDefault to route internally, and Radix composes its own
  // select handler with checkForDefaultPrevented, so that handler never runs
  // and the menu stays open on top of the page it just opened. Closing it
  // here works for both destinations without giving up a real <a>
  // (middle-click, copy link address).
  const [menuOpen, setMenuOpen] = useState(false);
  const orgLink = org ? linkProps(org.manage_url) : null;
  return (
    <footer id="accountbar">
      <DropdownMenu modal={false} open={menuOpen} onOpenChange={setMenuOpen}>
        <DropdownMenuTrigger asChild>
          <button id="account-btn" className={orgActive ? "active" : undefined} aria-label="Account menu">
            <span className="avatar" style={{ background: projColor(me.email) }} aria-hidden="true">
              {(display.trim()[0] || "?").toUpperCase()}
            </span>
            <span className="acct">
              <b>{display}</b>
              {me.name && <small>{me.email}</small>}
            </span>
            <Icon name="chev" />
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent id="account-menu" side="top" align="start" sideOffset={6} className="acct-menu">
          {org && (
            <>
              <DropdownMenuLabel className="menu-sec">Organization</DropdownMenuLabel>
              <DropdownMenuItem asChild>
                <a
                  id="menu-org-settings"
                  aria-current={orgActive ? "page" : undefined}
                  {...orgLink}
                  onClick={(e) => {
                    orgLink?.onClick?.(e);
                    setMenuOpen(false);
                  }}
                >
                  <Icon name="gear" />
                  <span>
                    <b>{org.name}</b> Settings
                  </span>
                  {!org.manage_url.startsWith("/") && (
                    <>
                      <span className="ext" aria-hidden="true">↗</span>
                      <span className="sr-only"> (opens in a new tab)</span>
                    </>
                  )}
                </a>
              </DropdownMenuItem>
            </>
          )}
          {admin && (
            <>
              <DropdownMenuLabel className="menu-sec">Hub</DropdownMenuLabel>
              <DropdownMenuItem id="menu-hub-admin" onSelect={admin.onClick}>
                <Icon name="shield" />
                <span>Signup &amp; access{admin.pending ? ` · ${admin.pending}` : ""}</span>
              </DropdownMenuItem>
            </>
          )}
          <DropdownMenuLabel className="menu-sec">Account</DropdownMenuLabel>
          <DropdownMenuItem asChild>
            <a id="signout" href="/auth/logout">
              <Icon name="power" />
              <span>Log out</span>
            </a>
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </footer>
  );
}
