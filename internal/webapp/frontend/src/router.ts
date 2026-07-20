// Native path routing (no hash, no %2F):
//   volume mode:  /<path>
//   hub mode:     /<project-id>/<path>
//   invite:       /join/<token>
//   org admin:    /orgs/<org-id>
// Each path segment is percent-encoded for odd characters, but the "/"
// separators stay literal so the URL reads like a real file path. This is
// why routes are parsed by hand instead of with a route-matching library:
// encoded slashes must survive.

export function encodePath(p: string): string {
  return p.split("/").map(encodeURIComponent).join("/");
}
export function decodePath(p: string): string {
  return p.split("/").map(decodeURIComponent).join("/");
}

// Special views are RESTful routes under the project — the first segment
// after the project id is reserved when it names a view:
//   /<project-id>/insights[/<path>]   the Insights dashboard (optionally scoped)
//   /<project-id>/history[/<path>]    change feed (project / subtree / file)
//   /<project-id>/install             connect-a-device guide
//   /<project-id>/settings            project settings
// Rule: every page gets its own URL path (see CLAUDE.md) — new surfaces are
// view routes here, not ephemeral panel state. (Root-level files literally
// named like a view lose the URL shortcut and remain reachable via the tree.)
export const VIEW_ROUTES = new Set(["insights", "history", "install", "settings"]);

export type ViewName = "insights" | "history" | "install" | "settings";

export interface Route {
  // Org administration is not project-scoped, so it is a top-level route
  // rather than a view under a project. The server hands out this URL (see
  // manage_url on /api/orgs), which is why it is reserved here.
  org?: string;
  project?: string;
  path: string;
  view?: ViewName;
  viewTarget?: string;
}

export function parseRoute(pathname: string, mode: "volume" | "hub"): Route {
  const raw = pathname.replace(/^\/+/, "");
  if (mode !== "hub") return { path: raw ? decodePath(raw) : "" };
  if (raw === "orgs" || raw.startsWith("orgs/")) {
    return { org: raw.slice(5).replace(/\/+$/, ""), path: "" };
  }
  const slash = raw.indexOf("/");
  if (slash === -1) return { project: raw, path: "" };
  const r: Route = { project: raw.slice(0, slash), path: decodePath(raw.slice(slash + 1)) };
  const seg = r.path.indexOf("/");
  const head = seg === -1 ? r.path : r.path.slice(0, seg);
  if (VIEW_ROUTES.has(head)) {
    r.view = head as ViewName;
    r.viewTarget = seg === -1 ? "" : r.path.slice(seg + 1).replace(/\/+$/, "");
    r.path = "";
  }
  return r;
}

// The URL for a file within a project (hub) or the volume (no project id).
export function urlForPath(path: string, projectId?: string): string {
  const enc = encodePath(path);
  if (projectId) return "/" + projectId + (enc ? "/" + enc : "");
  return "/" + enc;
}

// The URL for a special view of a project.
export function urlForView(view: ViewName, projectId?: string, target?: string): string {
  let s = (projectId ? "/" + projectId : "") + "/" + view;
  if (target) s += "/" + encodePath(target.replace(/\/+$/, ""));
  return s;
}
