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
//   /<project-id>/dashboard[/<path>]  the read×staleness dashboard (optionally scoped)
//   /<project-id>/history[/<path>]    change feed (project / subtree / file)
//   /<project-id>/since               the change feed since your last visit
//   /<project-id>/install             connect-a-device guide
//   /<project-id>/settings            project settings
// Rule: every page gets its own URL path (see CLAUDE.md) — new surfaces are
// view routes here, not ephemeral panel state. (Root-level files literally
// named like a view lose the URL shortcut and remain reachable via the tree.)
export const VIEW_ROUTES = new Set(["dashboard", "history", "since", "install", "settings"]);

// Shipped URLs that were renamed. Parsed into the new view and normalized
// away on arrival, so bookmarks resolve without a second live name.
const LEGACY_VIEWS: Record<string, ViewName> = { insights: "dashboard" };

export type ViewName = "dashboard" | "history" | "since" | "install" | "settings";

export interface Route {
  // Org administration is not project-scoped, so it is a top-level route
  // rather than a view under a project. The server hands out this URL (see
  // manage_url on /api/orgs), which is why it is reserved here.
  org?: string;
  // Billing (managed hubs) is hub-level like the org route; the URL comes
  // from /api/config's billing block. Reserved only in hub mode — project
  // ids are UUIDs (or legacy p-…), so the segment can't collide with one.
  billing?: boolean;
  project?: string;
  path: string;
  view?: ViewName;
  viewTarget?: string;
  // The URL used a renamed segment (e.g. /insights): the app replaces it
  // with the canonical one instead of leaving two URLs for one page.
  legacyView?: boolean;
  // The URL carried a trailing separator (/notes/ — what a browser hands you
  // when you copy a folder URL). Same treatment as legacyView: it resolves,
  // then the app replaces it with the slash-free URL.
  trailingSlash?: boolean;
  // A past version of `path`, by content hash (?v=<sha>). Not a view route:
  // the first segment after the project id is reserved for view names, and a
  // version is the same page pinned to older bytes, so it rides as a query
  // param on the file route.
  version?: string;
  // How the creator said they'd fill this project ("existing" = they already
  // have a folder). Rides as a query param rather than living on the project
  // record on purpose: it is one person's intent for their next five minutes,
  // not a property of the project — a teammate connecting next week has their
  // own answer, and would be told the wrong thing by a persisted flag.
  connect?: string;
}

// `url` is pathname + search (what useLocationPath hands back).
export function parseRoute(url: string, mode: "volume" | "hub"): Route {
  const qi = url.indexOf("?");
  const q = qi === -1 ? null : new URLSearchParams(url.slice(qi));
  const version = q?.get("v") || "";
  const connect = q?.get("connect") || "";
  const r = parsePath(qi === -1 ? url : url.slice(0, qi), mode);
  if (version) r.version = version;
  if (connect) r.connect = connect;
  return r;
}

// Trailing separators are stripped off the raw (still-encoded) slice so a
// percent-encoded slash inside a segment survives, and flagged only when
// stripping actually changed the string — a bare "/p-1/" has nothing to
// strip, so it never asks for a redirect.
function withPath(r: Route, raw: string): Route {
  const p = raw.replace(/\/+$/, "");
  if (p !== raw) r.trailingSlash = true;
  r.path = p ? decodePath(p) : "";
  return r;
}

function parsePath(pathname: string, mode: "volume" | "hub"): Route {
  const raw = pathname.replace(/^\/+/, "");
  if (mode !== "hub") return withPath({ path: "" }, raw);
  if (raw === "orgs" || raw.startsWith("orgs/")) {
    return { org: raw.slice(5).replace(/\/+$/, ""), path: "" };
  }
  if (raw === "billing" || raw.startsWith("billing/")) {
    return { billing: true, path: "" };
  }
  const slash = raw.indexOf("/");
  if (slash === -1) return { project: raw, path: "" };
  const r = withPath({ project: raw.slice(0, slash), path: "" }, raw.slice(slash + 1));
  const seg = r.path.indexOf("/");
  const head = seg === -1 ? r.path : r.path.slice(0, seg);
  if (VIEW_ROUTES.has(head) || LEGACY_VIEWS[head]) {
    r.view = LEGACY_VIEWS[head] || (head as ViewName);
    if (LEGACY_VIEWS[head]) r.legacyView = true;
    r.viewTarget = seg === -1 ? "" : r.path.slice(seg + 1).replace(/\/+$/, "");
    r.path = "";
  }
  return r;
}

// The URL for a file within a project (hub) or the volume (no project id),
// optionally pinned to one past version by content hash.
export function urlForPath(path: string, projectId?: string, version?: string): string {
  const enc = encodePath(path);
  const q = version ? "?v=" + version : "";
  if (projectId) return "/" + projectId + (enc ? "/" + enc : "") + q;
  return "/" + enc + q;
}

// The URL for a special view of a project.
export function urlForView(view: ViewName, projectId?: string, target?: string): string {
  let s = (projectId ? "/" + projectId : "") + "/" + view;
  if (target) s += "/" + encodePath(target.replace(/\/+$/, ""));
  return s;
}
