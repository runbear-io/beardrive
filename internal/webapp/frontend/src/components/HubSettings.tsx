import { useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { getJSON, postJSON } from "../api/http";
import type { AdminPolicy } from "../api/types";
import { usePending } from "../hooks/useHub";
import { toast } from "../toast";
import { Button } from "@/components/ui/button";

const policySchema = z.object({
  require_verification: z.boolean(),
  require_approval: z.boolean(),
});
type PolicyForm = z.infer<typeof policySchema>;

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
  const form = useForm<PolicyForm>({
    resolver: zodResolver(policySchema),
    values: pol
      ? { require_verification: pol.require_verification && pol.mailer, require_approval: pol.require_approval }
      : { require_verification: false, require_approval: false },
  });
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
      <form
        onSubmit={form.handleSubmit(async (v) => {
          try {
            await postJSON("/api/admin/policy", v);
            toast("Signup policy saved.");
            qc.invalidateQueries({ queryKey: ["admin", "policy"] });
          } catch (e) {
            toast((e as Error).message, true);
          }
        })}
      >
        <div className="admin-list">
          <PolicyToggle
            label="Require email verification"
            desc={
              pol.mailer
                ? "New accounts must click an emailed link before they can sign in — proves they control the address."
                : "Configure SMTP on the server (auth.smtp) to enable email verification."
            }
            disabled={!pol.mailer}
            inputProps={form.register("require_verification")}
          />
          <PolicyToggle
            label="Require admin approval"
            desc="New accounts wait for a hub admin to approve them before they gain access."
            inputProps={form.register("require_approval")}
          />
        </div>
        <Button variant="primary" type="submit" style={{ marginTop: 14 }}>
          Save policy
        </Button>
      </form>

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
            <Button variant="primary" onClick={() => act(u.id, "approve", u.email)}>
              Approve
            </Button>
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
  disabled,
  inputProps,
}: {
  label: string;
  desc: string;
  disabled?: boolean;
  inputProps: ReturnType<ReturnType<typeof useForm<PolicyForm>>["register"]>;
}) {
  return (
    <label className="admin-item toggle" style={disabled ? { opacity: 0.55 } : undefined}>
      <span className="ai-main">
        <div className="tg-label">{label}</div>
        <div className="tg-desc">{desc}</div>
      </span>
      <input type="checkbox" disabled={disabled} {...inputProps} />
    </label>
  );
}
