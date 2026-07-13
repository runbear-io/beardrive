import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getJSON, postJSON } from "../api/http";
import type { AdminPolicy } from "../api/types";
import { usePending } from "../hooks/useHub";
import { toast } from "../toast";

/* Hub-admin settings: signup/access policy. Verification & approval are
   live toggles; the domain allowlist and admin list are shown read-only
   (they're server-config owned, deliberately not browser-editable).
   Pending approvals live here too, so this is the single admin home. */
export function HubSettings() {
  const qc = useQueryClient();
  const { data: pol, error } = useQuery({
    queryKey: ["admin", "policy"],
    queryFn: () => getJSON<AdminPolicy>("/api/admin/policy"),
  });
  const { data: pending } = usePending(true);
  const [ver, setVer] = useState(false);
  const [app, setApp] = useState(false);
  useEffect(() => {
    if (pol) {
      setVer(pol.require_verification && pol.mailer);
      setApp(pol.require_approval);
    }
  }, [pol]);
  useEffect(() => {
    if (error) toast((error as Error).message, true);
  }, [error]);
  if (!pol) return null;

  const act = async (id: string, verb: "approve" | "deny", email: string) => {
    try {
      await postJSON(`/api/admin/pending/${id}/${verb}`);
      toast((verb === "approve" ? "Approved " : "Denied ") + email);
      qc.invalidateQueries({ queryKey: ["admin", "pending"] });
    } catch (e) {
      toast((e as Error).message, true);
    }
  };

  return (
    <div className="admin">
      <h1>Signup &amp; access</h1>
      <p className="admin-sub">
        Who can create an account on this hub, and how new accounts are vetted.
      </p>

      <h3>New-account vetting</h3>
      <div className="admin-list">
        <PolicyToggle
          label="Require email verification"
          desc={
            pol.mailer
              ? "New accounts must click an emailed link before they can sign in — proves they control the address."
              : "Configure SMTP on the server (auth.smtp) to enable email verification."
          }
          checked={ver}
          disabled={!pol.mailer}
          onChange={setVer}
        />
        <PolicyToggle
          label="Require admin approval"
          desc="New accounts wait for a hub admin to approve them before they gain access."
          checked={app}
          onChange={setApp}
        />
      </div>
      <button
        className="pbtn"
        style={{ marginTop: 14 }}
        onClick={async () => {
          try {
            await postJSON("/api/admin/policy", { require_verification: ver, require_approval: app });
            toast("Signup policy saved.");
            qc.invalidateQueries({ queryKey: ["admin", "policy"] });
          } catch (e) {
            toast((e as Error).message, true);
          }
        }}
      >
        Save policy
      </button>

      <h3>Who can sign up</h3>
      <div className="admin-list">
        <div className="admin-item">
          <span className="ai-main">Allowed email domains</span>
          <span className="ai-tag">
            {pol.allowed_domains && pol.allowed_domains.length
              ? pol.allowed_domains.map((d) => "@" + d).join(", ")
              : "any"}
          </span>
        </div>
        <div className="admin-item">
          <span className="ai-main">Self-signup</span>
          <span className="ai-tag">{pol.allow_signup ? "open" : "invite-only"}</span>
        </div>
        <div className="admin-item">
          <span className="ai-main">Hub admins</span>
          <span className="ai-tag">
            {pol.admins && pol.admins.length ? pol.admins.join(", ") : "none"}
          </span>
        </div>
      </div>
      <p className="admin-sub">
        Domains and admins are set in the server config file (they can't be widened from the
        browser).
      </p>

      <h3>Pending signups</h3>
      <div className="admin-list">
        {(!pending || pending.length === 0) && (
          <div className="admin-empty">No one is waiting for approval.</div>
        )}
        {(pending || []).map((u) => (
          <div className="admin-item" key={u.id}>
            <span className="ai-main">{(u.name ? u.name + "  ·  " : "") + u.email}</span>
            <button className="pbtn" onClick={() => act(u.id, "approve", u.email)}>
              Approve
            </button>
            <button className="ai-del" onClick={() => act(u.id, "deny", u.email)}>
              Deny
            </button>
          </div>
        ))}
      </div>
    </div>
  );
}

function PolicyToggle({
  label,
  desc,
  checked,
  disabled,
  onChange,
}: {
  label: string;
  desc: string;
  checked: boolean;
  disabled?: boolean;
  onChange: (v: boolean) => void;
}) {
  return (
    <label className="admin-item toggle" style={disabled ? { opacity: 0.55 } : undefined}>
      <span className="ai-main">
        <div className="tg-label">{label}</div>
        <div className="tg-desc">{desc}</div>
      </span>
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
      />
    </label>
  );
}
