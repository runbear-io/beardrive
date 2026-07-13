import type { Org } from "../api/types";

// The sidebar footer names the project's org; clicking it opens the org
// admin panel, and owners get a Manage button that does the same. The
// panel itself arrives with the admin surfaces (Phase 4).
export function OrgBar({ org, onManage }: { org: Org | null; onManage: (org: Org) => void }) {
  if (!org) return null;
  return (
    <footer id="orgbar">
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
      {org.role === "owner" && (
        <button id="invite-btn" title="Manage this organization" onClick={() => onManage(org)}>
          Manage
        </button>
      )}
    </footer>
  );
}
