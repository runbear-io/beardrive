import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createColumnHelper,
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type SortingState,
} from "@tanstack/react-table";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { api, getJSON, postJSON } from "../api/http";
import type { Org, OrgInviteInfo, OrgShareInfo, Project } from "../api/types";
import { modalConfirm, modalPrompt } from "../modal";
import { toast } from "../toast";
import { copyText } from "../util";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

/* The org admin panel: members (owners can change roles / remove), rename,
   projects (rename / delete), invite links (create / revoke), and an
   org-wide audit of public shares. Members and shares render through
   react-table (sortable); the rename is an RHF+zod form. */

const renameSchema = z.object({
  name: z.string().trim().min(1, "Give the organization a name.").max(60, "Keep it under 60 characters."),
});
type RenameForm = z.infer<typeof renameSchema>;

type Member = Org["members"][number];

export function OrgAdmin({
  org,
  projects,
  myEmail,
  onProjectsChanged,
}: {
  org: Org;
  projects: Project[];
  myEmail: string;
  onProjectsChanged: () => Promise<void>;
}) {
  const qc = useQueryClient();
  const owner = org.role === "owner";

  const refreshOrgs = () => qc.invalidateQueries({ queryKey: ["orgs"] });
  const refreshInvites = () => qc.invalidateQueries({ queryKey: ["invites", org.id] });
  const refreshShares = () => qc.invalidateQueries({ queryKey: ["orgShares", org.id] });

  const renameForm = useForm<RenameForm>({
    resolver: zodResolver(renameSchema),
    values: { name: org.name },
  });

  const { data: invites } = useQuery({
    queryKey: ["invites", org.id],
    queryFn: () => getJSON<{ invites: OrgInviteInfo[] }>(`/api/orgs/${org.id}/invites`),
    enabled: owner,
    select: (d) => d.invites || [],
  });
  const { data: shares } = useQuery({
    queryKey: ["orgShares", org.id],
    queryFn: () => getJSON<{ shares: OrgShareInfo[] }>(`/api/orgs/${org.id}/shares`),
    enabled: owner,
    select: (d) => d.shares || [],
  });

  const orgProjects = projects.filter((p) => p.org === org.id);

  return (
    <div className="admin">
      <h1 id="org-title">{org.name + (owner ? "" : "  ·  member")}</h1>

      {owner && (
        <form
          className="admin-row"
          onSubmit={renameForm.handleSubmit(async ({ name }) => {
            try {
              await api("PATCH", "/api/orgs/" + org.id, { name });
              toast("Renamed.");
              refreshOrgs();
            } catch (e) {
              toast((e as Error).message, true);
            }
          })}
        >
          <input id="org-rename" type="text" {...renameForm.register("name")} />
          <Button variant="primary" id="org-rename-btn" type="submit">
            Rename org
          </Button>
          {renameForm.formState.errors.name && (
            <span className="field-err">{renameForm.formState.errors.name.message}</span>
          )}
        </form>
      )}

      <h3>Members</h3>
      <MembersTable org={org} owner={owner} myEmail={myEmail} onChanged={refreshOrgs} />

      {owner && (
        <>
          <h3>Projects</h3>
          <div className="admin-list">
            {orgProjects.length === 0 && <div className="admin-empty">No projects yet.</div>}
            {orgProjects.map((p) => (
              <div className="admin-item" key={p.id}>
                <span className="ai-main">{p.name}</span>
                <Button
                  variant="subtle"
                  onClick={async () => {
                    const name = await modalPrompt("Rename project", "New name", p.name, "Rename");
                    if (!name || name === p.name) return;
                    try {
                      await api("PATCH", "/api/projects/" + p.id, { name });
                      toast("Renamed.");
                      await onProjectsChanged();
                    } catch (e) {
                      toast((e as Error).message, true);
                    }
                  }}
                >
                  Rename
                </Button>
                <button
                  className="ai-del"
                  onClick={async () => {
                    if (
                      !(await modalConfirm(
                        "Delete project",
                        `Delete “${p.name}”? Its files stay in storage, but it's removed from the hub.`,
                        "Delete",
                        true,
                      ))
                    )
                      return;
                    try {
                      await api("DELETE", "/api/projects/" + p.id);
                      toast(`Deleted “${p.name}”.`);
                      await onProjectsChanged();
                    } catch (e) {
                      toast((e as Error).message, true);
                    }
                  }}
                >
                  Delete
                </button>
              </div>
            ))}
          </div>

          <div className="admin-h">
            <h3>Invite links</h3>
            <Button
              variant="primary"
              onClick={async () => {
                try {
                  const out = await postJSON<{ url: string }>(`/api/orgs/${org.id}/invites`);
                  const ok = await copyText(out.url);
                  toast(ok ? "Invite link copied to clipboard." : "Invite created — copy it from the list below.");
                  refreshInvites();
                } catch (e) {
                  toast((e as Error).message, true);
                }
              }}
            >
              New invite
            </Button>
          </div>
          <div className="admin-list">
            {invites && invites.length === 0 && (
              <div className="admin-empty">No active invite links.</div>
            )}
            {(invites || []).map((inv) => (
              <div className="admin-item" key={inv.token}>
                <span
                  className="ai-main mono"
                  style={{ cursor: "pointer" }}
                  title="Copy"
                  onClick={() =>
                    copyText(inv.url).then((ok) => toast(ok ? "Copied." : "Select and copy the link."))
                  }
                >
                  {inv.url}
                </span>
                <span className="ai-tag">
                  {(inv.creator ? "by " + inv.creator + " · " : "") +
                    (inv.uses ? inv.uses + " joined · " : "unused · ") +
                    "expires " +
                    new Date(inv.expires).toLocaleDateString()}
                </span>
                <button
                  className="ai-del"
                  onClick={async () => {
                    if (
                      !(await modalConfirm(
                        "Revoke invite",
                        "Revoke this invite link? Anyone still holding it won't be able to join.",
                        "Revoke",
                        true,
                      ))
                    )
                      return;
                    try {
                      await api("DELETE", `/api/orgs/${org.id}/invites/${inv.token}`);
                      toast("Revoked.");
                      refreshInvites();
                    } catch (e) {
                      toast((e as Error).message, true);
                    }
                  }}
                >
                  Revoke
                </button>
              </div>
            ))}
          </div>

          <h3>Public share links</h3>
          <SharesTable shares={shares || []} onChanged={refreshShares} />
        </>
      )}
    </div>
  );
}

/* ---- members (react-table: sortable email/role) ---- */

function MembersTable({
  org,
  owner,
  myEmail,
  onChanged,
}: {
  org: Org;
  owner: boolean;
  myEmail: string;
  onChanged: () => void;
}) {
  const [sorting, setSorting] = useState<SortingState>([{ id: "email", desc: false }]);
  const col = useMemo(() => createColumnHelper<Member>(), []);

  const columns = useMemo(
    () => [
      col.accessor("email", {
        id: "email",
        header: "Member",
        cell: (c) => {
          const isSelf = !!myEmail && c.getValue().toLowerCase() === myEmail.toLowerCase();
          return <span className="ai-main">{c.getValue() + (isSelf ? "  (you)" : "")}</span>;
        },
      }),
      col.accessor("role", {
        id: "role",
        header: "Role",
        cell: (c) => {
          const m = c.row.original;
          const isSelf = !!myEmail && m.email.toLowerCase() === myEmail.toLowerCase();
          if (!owner || isSelf) return <span className="ai-tag">{m.role}</span>;
          return (
            <>
              <select
                value={m.role}
                onChange={async (e) => {
                  try {
                    await api("PATCH", `/api/orgs/${org.id}/members/${encodeURIComponent(m.email)}`, {
                      role: e.target.value,
                    });
                    toast("Role updated.");
                  } catch (err) {
                    toast((err as Error).message, true);
                  }
                  onChanged();
                }}
              >
                <option value="owner">owner</option>
                <option value="member">member</option>
              </select>
              <button
                className="ai-del"
                onClick={async () => {
                  if (
                    !(await modalConfirm("Remove member", `Remove ${m.email} from ${org.name}?`, "Remove", true))
                  )
                    return;
                  try {
                    await api("DELETE", `/api/orgs/${org.id}/members/${encodeURIComponent(m.email)}`);
                    toast("Removed.");
                    onChanged();
                  } catch (err) {
                    toast((err as Error).message, true);
                  }
                }}
              >
                Remove
              </button>
            </>
          );
        },
      }),
    ],
    [col, org.id, org.name, owner, myEmail],
  );

  const table = useReactTable({
    data: org.members,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  });

  return (
    <Table className="admin-table">
      <TableHeader>
        {table.getHeaderGroups().map((hg) => (
          <TableRow key={hg.id}>
            {hg.headers.map((h) => (
              <TableHead
                key={h.id}
                onClick={h.column.getToggleSortingHandler()}
                data-sort={h.column.getIsSorted() || undefined}
              >
                {flexRender(h.column.columnDef.header, h.getContext())}
                {h.column.getIsSorted() === "asc" ? " ↑" : h.column.getIsSorted() === "desc" ? " ↓" : ""}
              </TableHead>
            ))}
          </TableRow>
        ))}
      </TableHeader>
      <TableBody>
        {table.getRowModel().rows.map((r) => (
          <TableRow key={r.id} className="admin-item">
            {r.getVisibleCells().map((c) => (
              <TableCell key={c.id}>{flexRender(c.column.columnDef.cell, c.getContext())}</TableCell>
            ))}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

/* ---- shares audit (react-table: sortable path/project) ---- */

function SharesTable({
  shares,
  onChanged,
}: {
  shares: OrgShareInfo[];
  onChanged: () => void;
}) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const col = useMemo(() => createColumnHelper<OrgShareInfo>(), []);
  const columns = useMemo(
    () => [
      col.accessor("path", {
        header: "Path",
        cell: (c) => (
          <span
            className="ai-main mono"
            style={{ cursor: "pointer" }}
            title={c.row.original.url}
            onClick={() => window.open(c.row.original.url, "_blank")}
          >
            {c.getValue()}
          </span>
        ),
      }),
      col.accessor((s) => s.project_name || "", {
        id: "project",
        header: "Project",
        cell: (c) => (
          <span className="ai-tag">
            {(c.getValue() || "") +
              (c.row.original.creator ? " · by " + c.row.original.creator : "") +
              (c.row.original.created ? " · " + new Date(c.row.original.created).toLocaleDateString() : "")}
          </span>
        ),
      }),
      col.display({
        id: "actions",
        header: "",
        cell: (c) => (
          <button
            className="ai-del"
            onClick={async () => {
              const sh = c.row.original;
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
            }}
          >
            Revoke
          </button>
        ),
      }),
    ],
    [col, onChanged],
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
        <div className="admin-empty">No public shares.</div>
      </div>
    );

  return (
    <Table className="admin-table">
      <TableHeader>
        {table.getHeaderGroups().map((hg) => (
          <TableRow key={hg.id}>
            {hg.headers.map((h) => (
              <TableHead key={h.id} onClick={h.column.getToggleSortingHandler()}>
                {flexRender(h.column.columnDef.header, h.getContext())}
              </TableHead>
            ))}
          </TableRow>
        ))}
      </TableHeader>
      <TableBody>
        {table.getRowModel().rows.map((r) => (
          <TableRow key={r.id} className="admin-item">
            {r.getVisibleCells().map((c) => (
              <TableCell key={c.id}>{flexRender(c.column.columnDef.cell, c.getContext())}</TableCell>
            ))}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
