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

// SortableHead is both tables' header cell: a real button inside the th so
// sorting is reachable by keyboard, with the direction announced rather than
// carried by a glyph alone. Two tables ten centimetres apart had opposite
// accessibility contracts before this existed.
function SortableHead({ header }: { header: any }) {
  const sorted = header.column.getIsSorted();
  if (!header.column.getCanSort()) {
    // The actions column has no header text and nothing to sort; a button
    // here is a dead tab stop with an empty accessible name.
    return <TableHead>{flexRender(header.column.columnDef.header, header.getContext())}</TableHead>;
  }
  return (
    <TableHead
      data-sort={sorted || undefined}
      aria-sort={sorted === "asc" ? "ascending" : sorted === "desc" ? "descending" : "none"}
    >
      <button type="button" className="th-sort" onClick={header.column.getToggleSortingHandler()}>
        {flexRender(header.column.columnDef.header, header.getContext())}
        {sorted === "asc" ? " ↑" : sorted === "desc" ? " ↓" : ""}
      </button>
    </TableHead>
  );
}

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
      <h1 id="org-title">{org.name}</h1>
      {!owner && <p className="role-chip-row"><span className="ai-tag role-chip">Member</span></p>}
      {!owner && (
        <p className="admin-sub">
          Only owners can rename this organization, manage members, or issue invite links.
        </p>
      )}

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
          <label className="admin-lbl" htmlFor="org-rename">
            Organization name
          </label>
          <input
            id="org-rename"
            type="text"
            aria-invalid={!!renameForm.formState.errors.name}
            aria-describedby={renameForm.formState.errors.name ? "org-rename-err" : undefined}
            {...renameForm.register("name")}
          />
          <Button
            variant="subtle"
            id="org-rename-btn"
            type="submit"
            disabled={!renameForm.formState.isDirty}
          >
            Rename org
          </Button>
          {renameForm.formState.errors.name && (
            <span id="org-rename-err" role="alert" className="field-err">
              {renameForm.formState.errors.name.message}
            </span>
          )}
        </form>
      )}

      <h3>Members</h3>
      <MembersTable org={org} owner={owner} myEmail={myEmail} onChanged={refreshOrgs} />

      {!owner && (
        <>
          <h3>Projects</h3>
          <div className="admin-list">
            {orgProjects.length === 0 && <div className="admin-empty">No projects yet.</div>}
            {orgProjects.map((p) => (
              <div className="admin-item" key={p.id}>
                <span className="ai-main" title={p.name}>{p.name}</span>
              </div>
            ))}
          </div>
        </>
      )}

      {owner && (
        <>
          <h3>Projects</h3>
          <div className="admin-list">
            {orgProjects.length === 0 && <div className="admin-empty">No projects yet.</div>}
            {orgProjects.map((p) => (
              <div className="admin-item" key={p.id}>
                <span className="ai-main" title={p.name}>{p.name}</span>
                <Button
                  variant="subtle"
                  aria-label={`Rename ${p.name}`}
                  onClick={async () => {
                    const name = await modalPrompt("Rename project", "New name", p.name, "Rename");
                    if (name === null || name.trim() === p.name) return;
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
                  aria-label={`Delete ${p.name}`}
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
                <button
                  type="button"
                  className="ai-main mono ai-copy"
                  aria-label={`Copy invite link ${inv.url}`}
                  title={inv.url}
                  onClick={() =>
                    copyText(inv.url).then((ok) => toast(ok ? "Copied." : "Select and copy the link."))
                  }
                >
                  {inv.url}
                </button>
                <span className="ai-tag">
                  {(inv.creator ? "by " + inv.creator + " · " : "") +
                    (inv.uses ? inv.uses + " joined · " : "unused · ") +
                    "expires " +
                    new Date(inv.expires).toLocaleDateString()}
                </span>
                <button
                  className="ai-del"
                  aria-label={`Revoke invite ${inv.token.slice(0, 8)}`}
                  onClick={async () => {
                    if (
                      !(await modalConfirm(
                        "Revoke invite",
                        `Revoke the link starting ${inv.token.slice(0, 8)}…? Anyone still holding it won't be able to join.`,
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
          return (
            <span className="ai-main" title={c.getValue()}>
              {c.getValue() + (isSelf ? "  (you)" : "")}
            </span>
          );
        },
      }),
      col.accessor("role", {
        id: "role",
        header: "Role",
        cell: (c) => {
          const m = c.row.original;
          const isSelf = !!myEmail && m.email.toLowerCase() === myEmail.toLowerCase();
          if (!owner || isSelf) return <span className="ai-tag role-static">{m.role}</span>;
          return (
            <span className="role-cell">
              <select
                aria-label={`Role for ${m.email}`}
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
                aria-label={`Remove ${m.email}`}
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
            </span>
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
    <div className="admin-list admin-card-table">
    <Table className="admin-table">
      <TableHeader>
        {table.getHeaderGroups().map((hg) => (
          <TableRow key={hg.id}>
            {hg.headers.map((h) => (
              <SortableHead key={h.id} header={h} />
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
    </div>
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
          <a
            className="ai-main mono"
            href={c.row.original.url}
            target="_blank"
            rel="noopener noreferrer"
            title={c.getValue()}
          >
            {c.getValue()}
          </a>
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
            aria-label={`Revoke the share of ${c.row.original.path}`}
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
    <div className="admin-list admin-card-table">
    <Table className="admin-table">
      <TableHeader>
        {table.getHeaderGroups().map((hg) => (
          <TableRow key={hg.id}>
            {hg.headers.map((h) => (
              <SortableHead key={h.id} header={h} />
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
    </div>
  );
}
