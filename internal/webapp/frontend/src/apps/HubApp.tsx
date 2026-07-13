import { useEffect, useMemo, useState } from "react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";
import { postJSON } from "../api/http";
import type { InviteAccepted, Project, ProjectCreated, ServerConfig } from "../api/types";
import { useOrgs, usePending, useProjects, useHubRefresh } from "../hooks/useHub";
import { parseRoute } from "../router";
import { AppShell, Topbar, VaultHeader } from "../components/shell";
import { ProjectNav } from "../components/ProjectNav";
import { OrgBar } from "../components/OrgBar";
import { EmptyState } from "../components/EmptyState";
import { toast } from "../toast";
import Browser from "./Browser";

export default function HubApp({ config }: { config: ServerConfig }) {
  const location = useLocation();
  const navigate = useNavigate();
  const refresh = useHubRefresh();
  // Org just joined via an invite this page-load: prefer its projects over
  // whatever happens to be first in the list.
  const [joinedOrgId, setJoinedOrgId] = useState<string | null>(null);

  const joinToken = useMemo(() => {
    const m = location.pathname.match(/^\/join\/([0-9a-f]+)\/?$/);
    return m ? m[1] : null;
  }, [location.pathname]);

  const { data: projects } = useProjects(!joinToken);
  const { data: orgs } = useOrgs(!joinToken);
  const isAdmin = !!config.auth.admin;
  const { data: pending } = usePending(isAdmin);

  const route = useMemo(
    () => parseRoute(location.pathname, "hub"),
    [location.pathname],
  );

  const current: Project | null = useMemo(() => {
    if (!projects) return null;
    return (
      projects.find((p) => p.id === route.project) ||
      (joinedOrgId && projects.find((p) => p.org === joinedOrgId)) ||
      projects[0] ||
      null
    );
  }, [projects, route.project, joinedOrgId]);

  useEffect(() => {
    document.title = current
      ? current.name + " — BearDrive"
      : config.brand || config.volume || "BearDrive";
  }, [current, config]);

  if (joinToken) {
    return (
      <JoinInvite
        token={joinToken}
        onDone={async (orgId) => {
          setJoinedOrgId(orgId);
          await refresh();
          navigate("/", { replace: true });
        }}
      />
    );
  }

  const brand = config.brand || config.volume || "BearDrive";
  const org = (current && orgs?.find((o) => o.id === current.org)) || null;
  const ownedOrg = orgs?.find((o) => o.role === "owner") || null;
  // The top-of-sidebar gear is the always-visible admin entry point: any
  // account that owns an org (or is a hub admin) gets it, whatever project
  // is open. The panels it opens arrive in Phase 4.
  const gearTarget = org && org.role === "owner" ? org : ownedOrg;
  // Insights (embedded on the project home and behind the ⋯ menu) is for
  // hub admins and owners of the project's org.
  const canInsights = isAdmin || (org ? org.role === "owner" : false);

  const vault = (
    <VaultHeader
      name={projects ? (current ? current.name : brand) : "…"}
      onHome={current ? () => navigate("/" + current.id) : undefined}
      showSignout={config.auth.enabled}
      admin={isAdmin ? { pending: pending?.length || 0, onClick: () => {} } : undefined}
      gear={gearTarget ? { onClick: () => {} } : undefined}
    />
  );

  if (!projects || !orgs) {
    return (
      <AppShell vault={vault} topbar={<Topbar />}>
        <div className="empty">Loading…</div>
      </AppShell>
    );
  }

  if (!current) {
    return (
      <AppShell
        vault={vault}
        projectsNav={<ProjectNav projects={projects} />}
        topbar={<Topbar />}
        contentClass="view"
      >
        <EmptyState
          authEnabled={config.auth.enabled}
          onCreate={async (name) => {
            if (!name) {
              toast("Give the project a name.", true);
              return;
            }
            try {
              const out = await postJSON<ProjectCreated>("/api/projects", { name });
              await refresh();
              navigate("/" + out.project.id);
              toast(`Created “${out.project.name}”.`);
            } catch (e) {
              toast("Could not create the project: " + (e as Error).message, true);
            }
          }}
        />
      </AppShell>
    );
  }

  // Landing ("/") and unknown project ids both resolve to a real project
  // URL; replace so back/forward never bounces through the redirect.
  if (route.project !== current.id) {
    return <Navigate to={"/" + current.id} replace />;
  }

  return (
    <Browser
      key={current.id} // fresh tree/fold state per project
      config={config}
      apiBase={"/api/p/" + current.id + "/"}
      route={route}
      hub
      project={current}
      projects={projects}
      canInsights={canInsights}
      sidebar={{
        vault,
        projectsNav: <ProjectNav projects={projects} currentId={current.id} />,
        orgBar: <OrgBar org={org} onManage={() => {}} />,
      }}
    />
  );
}

/* Opening "/join/<token>" joins the invite's org. If the visitor isn't
   signed in yet, the 401 handler sends them to /auth/login with the /join
   path intact in `next`, so after signing in the server re-serves the app
   here and the join completes — the token is never lost. */
function JoinInvite({ token, onDone }: { token: string; onDone: (orgId: string | null) => void }) {
  useEffect(() => {
    let cancelled = false;
    postJSON<InviteAccepted>("/api/invites/" + token)
      .then((out) => {
        if (cancelled) return;
        toast(`Welcome — you joined the “${out.org.name}” team. Opening its projects…`);
        onDone(out.org.id);
      })
      .catch((e) => {
        if (cancelled || String((e as Error).message).includes("signing in")) return;
        toast("Could not accept the invite: " + (e as Error).message, true);
        onDone(null);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token]);
  return (
    <AppShell vault={<VaultHeader name="…" showSignout />} topbar={<Topbar />}>
      <div className="empty">Joining…</div>
    </AppShell>
  );
}
