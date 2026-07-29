import { useMemo, useState } from "react";
import {
  createColumnHelper,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type SortingState,
} from "@tanstack/react-table";
import { api } from "../api/http";
import type { ShareInfo } from "../api/types";
import { modalConfirm } from "../modal";
import { toast } from "../toast";
import { linkProps } from "../nav";
import { urlForPath } from "../router";
import { AdminTable } from "./AdminTable";

/* The one live-public-links table in the product. The org panel passes
   org-wide rows (showProject) as the cross-project audit; a project's own
   Settings passes just its rows. Revoking is DELETE /api/shares/{token}
   either way — the server does its own PermWrite check, so canRevoke only
   decides whether to offer the control. */

// The one place a share's lifetime gets worded — the audit row and the share
// dialog must not drift on what "no expiry" looks like.
export function expiryLabel(expires?: string): string {
  return expires ? "expires " + new Date(expires).toLocaleDateString() : "no expiry";
}

export function shareDetail(s: ShareInfo, showProject: boolean): string {
  const bits: string[] = [];
  if (showProject && s.project_name) bits.push(s.project_name);
  if (s.creator) bits.push("by " + s.creator);
  if (s.created) bits.push(new Date(s.created).toLocaleDateString());
  bits.push(expiryLabel(s.expires));
  return bits.join(" · ");
}

export function SharesTable({
  shares,
  onChanged,
  showProject = false,
  canRevoke = true,
  empty = "No public shares.",
}: {
  shares: ShareInfo[];
  onChanged: () => void;
  showProject?: boolean;
  canRevoke?: boolean;
  empty?: string;
}) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const col = useMemo(() => createColumnHelper<ShareInfo>(), []);
  const columns = useMemo(
    () => [
      col.accessor("path", {
        header: "Path",
        // Links to the file in the hub, not to /s/<token>: from an audit row
        // the question is always "what is this?", and the public URL is one
        // click away on the file itself.
        cell: (c) => (
          <a
            className="ai-main mono"
            title={c.getValue()}
            {...linkProps(urlForPath(c.getValue(), c.row.original.project))}
          >
            {c.getValue()}
          </a>
        ),
      }),
      col.accessor((s) => shareDetail(s, showProject), {
        id: "detail",
        header: showProject ? "Project" : "Shared",
        cell: (c) => <span className="ai-tag">{c.getValue()}</span>,
      }),
      col.display({
        id: "actions",
        header: "",
        cell: (c) =>
          canRevoke ? (
            <button
              className="ai-del"
              aria-label={`Revoke the share of ${c.row.original.path}`}
              onClick={() => revokeShare(c.row.original, onChanged)}
            >
              Revoke
            </button>
          ) : null,
      }),
    ],
    [col, onChanged, showProject, canRevoke],
  );

  const table = useReactTable({
    data: shares,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  });

  if (shares.length === 0)
    return (
      <div className="admin-list">
        <div className="admin-empty">{empty}</div>
      </div>
    );

  return <AdminTable table={table} className="shares-table" />;
}

// Shared by the table and the file-page banner so "revoke" means the same
// thing (confirm, delete, tell the caller to refetch) wherever it is offered.
export async function revokeShare(sh: ShareInfo, onChanged: () => void) {
  if (
    !(await modalConfirm(
      "Revoke share link",
      `Revoke the public link to “${sh.path}”? Anyone with the URL will lose access.`,
      "Revoke",
      true,
    ))
  )
    return;
  try {
    await api("DELETE", "/api/shares/" + sh.token);
    toast("Share revoked.");
    onChanged();
  } catch (e) {
    toast((e as Error).message, true);
  }
}
