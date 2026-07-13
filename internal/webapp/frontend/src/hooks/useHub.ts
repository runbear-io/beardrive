import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { OrgList, PendingList, ProjectList } from "../api/types";

// Hub-wide server state: the project list (polled — new projects appear
// without a reload, matching the classic app's 30s refresh) and the orgs
// the signed-in account belongs to.

export function useProjects(enabled: boolean) {
  return useQuery({
    queryKey: ["projects"],
    queryFn: () => getJSON<ProjectList>("/api/projects"),
    enabled,
    refetchInterval: 30_000,
    select: (d) => d.projects || [],
  });
}

export function useOrgs(enabled: boolean) {
  return useQuery({
    queryKey: ["orgs"],
    queryFn: () => getJSON<OrgList>("/api/orgs"),
    enabled,
    select: (d) => d.orgs || [],
  });
}

// Pending signups; only fetched for hub admins (the admin bar shows the
// count).
export function usePending(enabled: boolean) {
  return useQuery({
    queryKey: ["admin", "pending"],
    queryFn: () => getJSON<PendingList>("/api/admin/pending"),
    enabled,
    select: (d) => d.pending || [],
  });
}

// Resolves once the refetches land — await it before navigating to a
// just-created project, or the router's unknown-id fallback will bounce
// off the stale list.
export function useHubRefresh() {
  const qc = useQueryClient();
  return () =>
    Promise.all([
      qc.invalidateQueries({ queryKey: ["projects"] }),
      qc.invalidateQueries({ queryKey: ["orgs"] }),
    ]).then(() => {});
}
