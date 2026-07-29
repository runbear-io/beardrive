import { useEffect, useMemo, useState } from "react";
import { postJSON } from "../api/http";
import type { InviteAccepted, Project, ServerConfig } from "../api/types";
import { useOrgs, usePending, useProjects, useHubRefresh } from "../hooks/useHub";
import { parseRoute, urlForPath, urlForView } from "../router";
import { linkProps, navigate, Redirect, useLocationPath } from "../nav";
import { AppShell, Page, Topbar, VaultHeader, closeSidebarOnMobile } from "../components/shell";
import { OrgAdmin } from "../components/OrgAdmin";
import { HubSettings } from "../components/HubSettings";
import { ProjectNav } from "../components/ProjectNav";
import { AccountBar } from "../components/AccountBar";
import { BillingView } from "../components/BillingView";
import { ProjectSettings } from "../components/ProjectSettings";
import { ConnectGuide } from "../components/ConnectGuide";
import { EmptyState } from "../components/EmptyState";
import { toast } from "../toast";
import Browser from "./Browser";

export default function HubApp({ config }: { config: ServerConfig }) {
  const loc = useLocationPath(); // pathname + search
  const refresh = useHubRefresh();
  // Org just joined via an invite this page-load: prefer its projects over
  // whatever happens to be first in the list.
  const [joinedOrgId, setJoinedOrgId] = useState<string | null>(null);
  // The hub-admin panel still replaces the content pane without touching the
  // URL (the last of the classic app's URL-less surfaces); any navigation
  // closes it. Org administration is a real route — see /orgs/<id> below.
  const [panel, setPanel] = useState<null | { kind: "hub" }>(null);
  useEffect(() => setPanel(null), [loc]);

  const joinToken = useMemo(() => {
    const m = loc.split("?")[0].match(/^\/join\/([0-9a-f]+)\/?$/);
    return m ? m[1] : null;
  }, [loc]);

  const { data: projects } = useProjects(!joinToken);
  const { data: orgs } = useOrgs(!joinToken);
  const isAdmin = !!config.auth.admin;
  const { data: pending } = usePending(isAdmin);

  const route = useMemo(() => parseRoute(loc, "hub"), [loc]);

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
      : config.brand || "BearDrive";
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

  const brand = config.brand || "BearDrive";
  const org = (current && orgs?.find((o) => o.id === current.org)) || null;

  // Top of the sidebar is the brand; project and account actions live in
  // their own sections below (PropelAuth-style layout).
  const vault = <VaultHeader name={brand} onHome={() => navigate("/")} search={!!current} />;

  const accountBar = config.me ? (
    <AccountBar
      me={config.me}
      org={org}
      orgActive={!!route.org}
      billing={config.billing}
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
          <EmptyState />
        </Page>
      </AppShell>
    );
  }

  const activePanel = panel?.kind === "hub" ? { crumb: "Signup & access", body: <HubSettings /> } : null;

  const routeOrg = route.org ? orgs.find((o) => o.id === route.org) : null;
  // A stale link, a revoked membership, or a typo: say so. Rendering the
  // project view at /orgs/<id> told the user nothing and survived a reload.
  const orgMissing = route.org && !routeOrg;
  const orgPage = orgMissing
    ? {
        crumb: "Organization",
        body: (
          <div className="empty">
            <h3>Organization not found</h3>
            <p>This organization doesn't exist, or you're no longer a member.</p>
            <p>
              <a {...linkProps("/" + current.id)}>Back to {current.name}</a>
            </p>
          </div>
        ),
      }
    : routeOrg
    ? {
        crumb: "Organization",
        body: (
          <OrgAdmin
            org={routeOrg}
            projects={projects}
            myEmail={config.me?.email || ""}
          />
        ),
      }
    : null;

  // Billing is hub-level (the managed deployment's surface), not
  // project-scoped — like the org route it borrows whichever project the
  // sidebar is showing. An OSS hub has no billing block; a hand-typed
  // /billing there says so instead of silently showing files.
  const billingPage = route.billing
    ? {
        crumb: "Billing",
        body: config.billing ? (
          <BillingView url={config.billing.url} />
        ) : (
          <div className="empty">
            <h3>No billing on this hub</h3>
            <p>This BearDrive hub doesn't have a billing surface.</p>
          </div>
        ),
      }
    : null;

  const routePage =
    route.view === "settings"
      ? {
          crumb: "Project settings",
          body: (
            <ProjectSettings
              project={current}
              org={org}
              onDeleted={async () => {
                // The id is dead now: refresh drops it from the list, and
                // navigating home lands on whatever project is left (or the
                // empty state) in one hop instead of via the stale-route
                // redirect below.
                await refresh();
                navigate("/");
              }}
            />
          ),
        }
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
  // URL; replace so back/forward never bounces through the redirect. The
  // org route is not project-scoped, so it is exempt — it borrows whichever
  // project the sidebar is showing.
  if (!route.org && !route.billing && route.project !== current.id) {
    return <Redirect to={"/" + current.id} />;
  }

  // A renamed view URL (/insights) still resolves; swap it for the current
  // one so there is one live URL per page.
  if (route.legacyView && route.view) {
    return <Redirect to={urlForView(route.view, current.id, route.viewTarget)} />;
  }

  // /notes/ is the same page as /notes — resolve it, then take the slash off
  // the address bar. After the rewrite the flag is false, so there is no
  // second hop. Must stay below the unknown-project redirect above, or a bad
  // project id would be normalized on the path and keep the wrong project.
  if (route.trailingSlash && route.path) {
    return <Redirect to={urlForPath(route.path, current.id, route.version)} />;
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
      sidebar={{
        vault,
        projectsNav: (
          <ProjectNav
            projects={projects}
            currentId={current.id}
            menu={{
              // Scoped views (/dashboard/<path>, /history/<path>) belong to
              // the file/folder — the tree carries the selection, no menu
              // item lights up.
              active: panel
                ? null
                : route.view === "dashboard" && !route.viewTarget
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
                navigate(urlForView("dashboard", current.id));
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
      panel={activePanel || orgPage || billingPage || routePage}
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
