// What a page load can ask the hub for before React mounts: the requests that
// depend on the URL alone. See prefetch.ts for why they start early.
//
// Import-free on purpose, so the node test runner can load it.

const PROJECT_ID = /^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|p-[0-9a-f]{8})$/;

export interface BootRoute {
  project?: string;
  path: string;
  view?: string;
  version?: string;
  editing?: boolean;
}

export interface BootRequest {
  queryKey: unknown[];
  url: string;
}

export function bootRequests(route: BootRoute, isMarkdown: (path: string) => boolean): BootRequest[] {
  if (!route.project || !PROJECT_ID.test(route.project)) return [];
  const apiBase = "/api/p/" + route.project + "/";
  // The app shell waits for the project list and the orgs together, and the
  // tree waits for the shell. Heat completes the file's meta line, which
  // otherwise grows a read count after the document is already on screen.
  const out: BootRequest[] = [
    { queryKey: ["projects"], url: "/api/projects" },
    { queryKey: ["orgs"], url: "/api/orgs" },
    { queryKey: ["tree", apiBase], url: apiBase + "tree?slim=1" },
    { queryKey: ["heat", apiBase], url: apiBase + "heat?days=30" },
  ];
  // Only the plain reading view of a markdown file: a pinned version, the
  // editor and the project views fetch something else.
  const path = route.path;
  if (!path || route.view || route.version || route.editing || !isMarkdown(path)) return out;
  const q = encodeURIComponent(path);
  out.push(
    { queryKey: ["render", apiBase, path, ""], url: apiBase + "render?path=" + q },
    { queryKey: ["comments", apiBase, path], url: apiBase + "comments?status=all&path=" + q },
  );
  return out;
}
