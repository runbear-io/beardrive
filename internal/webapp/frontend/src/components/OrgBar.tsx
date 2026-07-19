import type { Org } from "../api/types";
import { Icon } from "./shell";

// The sidebar footer is the workspace (org) row: the org name, a gear that
// opens the org admin panel (owners), and sign-out. Project-scoped actions
// live in the header; workspace-scoped ones end here.
export function OrgBar({
  org,
  onManage,
  showSignout,
}: {
  org: Org | null;
  onManage: (org: Org) => void;
  showSignout?: boolean;
}) {
  if (!org && !showSignout) return null;
  return (
    <footer id="orgbar">
      {org && (
        <span
          id="org-name"
          title="Manage organization"
          role="button"
          tabIndex={0}
          onClick={() => onManage(org)}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") {
              e.preventDefault();
              onManage(org);
            }
          }}
        >
          {org.name}
        </span>
      )}
      {org && org.role === "owner" && (
        <button
          id="org-settings-btn"
          className="icon-btn2"
          title="Manage this organization"
          aria-label="Manage this organization"
          onClick={() => onManage(org)}
        >
          <Icon name="gear" />
        </button>
      )}
      {showSignout && (
        <a id="signout" href="/auth/logout" title="Sign out" aria-label="Sign out">
          <Icon name="power" />
        </a>
      )}
    </footer>
  );
}
