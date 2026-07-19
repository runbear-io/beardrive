import type { Org } from "../api/types";
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
  const display = me.name || me.email;
  return (
    <footer id="accountbar">
      <DropdownMenu modal={false}>
        <DropdownMenuTrigger asChild>
          <button id="account-btn" aria-label="Account menu">
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
              <DropdownMenuItem id="menu-org-settings" onSelect={() => onOrgSettings(org)}>
                <Icon name="gear" />
                <span>
                  <b>{org.name}</b> Settings
                </span>
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
