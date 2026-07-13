import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, getJSON, postJSON } from "../api/http";
import type { Org, OrgInviteInfo, OrgShareInfo, Project } from "../api/types";
import { modalConfirm, modalPrompt } from "../modal";
import { toast } from "../toast";
import { copyText } from "../util";

/* The org admin panel: members (owners can change roles / remove), rename,
   projects (rename / delete), invite links (create / revoke), and an
   org-wide audit of public shares. */
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
  const [renameVal, setRenameVal] = useState(org.name);

  const refreshOrgs = () => qc.invalidateQueries({ queryKey: ["orgs"] });
  const refreshInvites = () => qc.invalidateQueries({ queryKey: ["invites", org.id] });
  const refreshShares = () => qc.invalidateQueries({ queryKey: ["orgShares", org.id] });

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
        <div className="admin-row">
          <input
            id="org-rename"
            type="text"
            value={renameVal}
            onChange={(e) => setRenameVal(e.target.value)}
          />
          <button
            className="pbtn"
            id="org-rename-btn"
            onClick={async () => {
              try {
                await api("PATCH", "/api/orgs/" + org.id, { name: renameVal.trim() });
                toast("Renamed.");
                refreshOrgs();
              } catch (e) {
                toast((e as Error).message, true);
              }
            }}
          >
            Rename org
          </button>
        </div>
      )}

      <h3>Members</h3>
      <div className="admin-list">
        {org.members.map((m) => {
          const isSelf = !!myEmail && m.email.toLowerCase() === myEmail.toLowerCase();
          return (
            <div className="admin-item" key={m.email}>
              <span className="ai-main">{m.email + (isSelf ? "  (you)" : "")}</span>
              {owner && !isSelf ? (
                <>
                  <select
                    value={m.role}
                    onChange={async (e) => {
                      try {
                        await api(
                          "PATCH",
                          `/api/orgs/${org.id}/members/${encodeURIComponent(m.email)}`,
                          { role: e.target.value },
                        );
                        toast("Role updated.");
                      } catch (err) {
                        toast((err as Error).message, true);
                      }
                      refreshOrgs();
                    }}
                  >
                    <option value="owner">owner</option>
                    <option value="member">member</option>
                  </select>
                  <button
                    className="ai-del"
                    onClick={async () => {
                      if (
                        !(await modalConfirm(
                          "Remove member",
                          `Remove ${m.email} from ${org.name}?`,
                          "Remove",
                          true,
                        ))
                      )
                        return;
                      try {
                        await api("DELETE", `/api/orgs/${org.id}/members/${encodeURIComponent(m.email)}`);
                        toast("Removed.");
                        refreshOrgs();
                      } catch (err) {
                        toast((err as Error).message, true);
                      }
                    }}
                  >
                    Remove
                  </button>
                </>
              ) : (
                <span className="ai-tag">{m.role}</span>
              )}
            </div>
          );
        })}
      </div>

      {owner && (
        <>
          <h3>Projects</h3>
          <div className="admin-list">
            {orgProjects.length === 0 && <div className="admin-empty">No projects yet.</div>}
            {orgProjects.map((p) => (
              <div className="admin-item" key={p.id}>
                <span className="ai-main">{p.name}</span>
                <button
                  className="ai-btn"
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
                </button>
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
            <button
              className="pbtn"
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
            </button>
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
          <div className="admin-list">
            {shares && shares.length === 0 && <div className="admin-empty">No public shares.</div>}
            {(shares || []).map((sh) => (
              <div className="admin-item" key={sh.token}>
                <span
                  className="ai-main mono"
                  style={{ cursor: "pointer" }}
                  title={sh.url}
                  onClick={() => window.open(sh.url, "_blank")}
                >
                  {sh.path}
                </span>
                <span className="ai-tag">
                  {(sh.project_name || "") +
                    (sh.creator ? " · by " + sh.creator : "") +
                    (sh.created ? " · " + new Date(sh.created).toLocaleDateString() : "")}
                </span>
                <button
                  className="ai-del"
                  onClick={async () => {
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
                      refreshShares();
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
        </>
      )}
    </div>
  );
}
