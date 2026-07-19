import { useEffect, useMemo, useState } from "react";
import { postJSON } from "../api/http";
import type { InviteAccepted, Project, ProjectCreated, ServerConfig } from "../api/types";
import { useOrgs, usePending, useProjects, useHubRefresh } from "../hooks/useHub";
import { parseRoute, urlForView } from "../router";
import { navigate, Redirect, useLocationPath } from "../nav";
import { AppShell, Page, Topbar, VaultHeader, closeSidebarOnMobile } from "../components/shell";
import { OrgAdmin } from "../components/OrgAdmin";
import { HubSettings } from "../components/HubSettings";
import { ProjectNav } from "../components/ProjectNav";
import { AccountBar } from "../components/AccountBar";
import { ProjectSettings } from "../components/ProjectSettings";
import { ConnectGuide } from "../components/ConnectGuide";
import { EmptyState } from "../components/EmptyState";
import { toast } from "../toast";
import Browser from "./Browser";

export default function HubApp({ config }: { config: ServerConfig }) {
  const pathname = useLocationPath();
  const refresh = useHubRefresh();
  // Org just joined via an invite this page-load: prefer its projects over
  // whatever happens to be first in the list.
  const [joinedOrgId, setJoinedOrgId] = useState<string | null>(null);
  // Admin panels replace the content pane without touching the URL (they
  // were never routes in the classic app); any navigation closes them.
  const [panel, setPanel] = useState<null | { kind: "hub" } | { kind: "org"; orgId: string }>(null);
  useEffect(() => setPanel(null), [pathname]);

  const joinToken = useMemo(() => {
    const m = pathname.match(/^\/join\/([0-9a-f]+)\/?$/);
    return m ? m[1] : null;
  }, [pathname]);

  const { data: projects } = useProjects(!joinToken);
  const { data: orgs } = useOrgs(!joinToken);
  const isAdmin = !!config.auth.admin;
  const { data: pending } = usePending(isAdmin);

  const route = useMemo(() => parseRoute(pathname, "hub"), [pathname]);

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
  // Insights (embedded on the project home and behind the ⋯ menu) is for
  // hub admins and owners of the project's org.
  const canInsights = isAdmin || (org ? org.role === "owner" : false);

  // Top of the sidebar is the brand; project and account actions live in
  // their own sections below (PropelAuth-style layout).
  const vault = <VaultHeader name={brand} onHome={() => navigate("/")} search={!!current} />;

  const accountBar = config.me ? (
    <AccountBar
      me={config.me}
      org={org}
      admin={
        isAdmin
          ? {
              pending: pending?.length || 0,
              onClick: () => {
                setPanel({ kind: "hub" });
                closeSidebarOnMobile();
              },
            }
          : undefined
      }
      onOrgSettings={(o) => {
        setPanel({ kind: "org", orgId: o.id });
        closeSidebarOnMobile();
      }}
    />
  ) : undefined;

  if (!projects || !orgs) {
    return (
      <AppShell vault={vault} topbar={<Topbar />}>
        <Page>
          <div className="empty">Loading…</div>
        </Page>
      </AppShell>
    );
  }

  if (!current) {
    return (
      <AppShell
        vault={vault}
        projectsNav={<ProjectNav projects={projects} />}
        orgBar={accountBar}
        topbar={<Topbar />}
      >
        <Page>
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
        </Page>
      </AppShell>
    );
  }

  const panelOrg = panel?.kind === "org" ? orgs.find((o) => o.id === panel.orgId) : null;
  const activePanel =
    panel?.kind === "hub"
      ? { crumb: "Signup & access", body: <HubSettings /> }
      : panelOrg
        ? {
            crumb: panelOrg.name,
            body: (
              <OrgAdmin
                org={panelOrg}
                projects={projects}
                myEmail={config.me?.email || ""}
                onProjectsChanged={refresh}
              />
            ),
          }
        : null;

  const routePage =
    route.view === "settings"
      ? { crumb: "Project settings", body: <ProjectSettings project={current} org={org} /> }
      : route.view === "install"
        ? {
            // The same guide the project home shows, in the same column —
            // it used to sit in the .onboard card, 320px narrower and 90px
            // lower than home, two sidebar items apart.
            crumb: "Installation",
            body: <ConnectGuide project={current} />,
          }
        : null;

  // Landing ("/") and unknown project ids both resolve to a real project
  // URL; replace so back/forward never bounces through the redirect.
  if (route.project !== current.id) {
    return <Redirect to={"/" + current.id} />;
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
        projectsNav: (
          <ProjectNav
            projects={projects}
            currentId={current.id}
            menu={{
              // Scoped views (/insights/<path>, /history/<path>) belong to
              // the file/folder — the tree carries the selection, no menu
              // item lights up.
              active: panel
                ? null
                : route.view === "insights" && !route.viewTarget
                  ? "dashboard"
                  : route.view === "install"
                    ? "install"
                    : route.view === "history" && !route.viewTarget
                      ? "history"
                      : route.view === "settings"
                        ? "settings"
                        : null,
              // Each page is a URL; explicitly close overlay panels because
              // same-path navigation doesn't change pathname.
              onDashboard: () => {
                setPanel(null);
                navigate(urlForView("insights", current.id));
                closeSidebarOnMobile();
              },
              onInstall: () => {
                setPanel(null);
                navigate(urlForView("install", current.id));
                closeSidebarOnMobile();
              },
              onHistory: () => {
                setPanel(null);
                navigate(urlForView("history", current.id));
                closeSidebarOnMobile();
              },
              onSettings: () => {
                setPanel(null);
                navigate(urlForView("settings", current.id));
                closeSidebarOnMobile();
              },
            }}
          />
        ),
        orgBar: accountBar,
      }}
      panel={activePanel || routePage}
      onClosePanel={() => setPanel(null)}
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
    <AppShell vault={<VaultHeader name="BearDrive" />} topbar={<Topbar />}>
      <Page>
        <div className="empty">Joining…</div>
      </Page>
    </AppShell>
  );
}
