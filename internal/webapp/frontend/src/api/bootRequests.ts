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
  // How the answer is read: JSON (the default), or a file's bytes decoded
  // the way the ["text", url] query decodes them (useBlob.fetchBlobText).
  kind?: "json" | "blob";
}

// Bytes a text query would only refuse: images and PDFs render from their
// URL, never through a text read.
const NOT_TEXT = /\.(png|jpe?g|gif|webp|svg|avif|bmp|ico|pdf)$/i;
// An HTML file's read view is its framed page, not its source.
const HTML = /\.html?$/i;

export function bootRequests(route: BootRoute, isMarkdown: (path: string) => boolean): BootRequest[] {
  if (!route.project || !PROJECT_ID.test(route.project)) return [];
  const apiBase = "/api/p/" + route.project + "/";
  // In the order the page needs them: on HTTP/1.1 a browser opens six
  // connections to a host and the rest wait, so what gates the first paint
  // goes first. The app shell waits for the project list and the orgs, the
  // file for the tree and its document. Heat completes the meta line, and
  // comments arrive beside a document already on screen.
  const out: BootRequest[] = [
    { queryKey: ["projects"], url: "/api/projects" },
    { queryKey: ["orgs"], url: "/api/orgs" },
    { queryKey: ["tree", apiBase], url: apiBase + "tree?slim=1" },
  ];
  const heat: BootRequest = { queryKey: ["heat", apiBase], url: apiBase + "heat?days=30" };
  // Only the current file: a pinned version and the project views fetch
  // something else.
  const path = route.path;
  if (!path || route.view || route.version) return [...out, heat];
  const q = encodeURIComponent(path);
  const md = isMarkdown(path);
  const fileURL = apiBase + "file?path=" + q;
  // The key the reading views and the editor both read (textQueryOptions).
  const text: BootRequest = { kind: "blob", queryKey: ["text", fileURL], url: fileURL };
  const render: BootRequest = { queryKey: ["render", apiBase, path, ""], url: apiBase + "render?path=" + q };
  const comments: BootRequest = { queryKey: ["comments", apiBase, path], url: apiBase + "comments?status=all&path=" + q };
  if (route.editing) {
    // The editor is seeded from the bytes; Markdown's preview and rail want
    // the render and the threads beside it.
    const seed = NOT_TEXT.test(path) ? [] : [text];
    return md ? [...out, ...seed, render, comments, heat] : [...out, ...seed, heat];
  }
  // A Markdown read also warms its source, so Edit has nothing to wait for.
  if (md) return [...out, render, heat, comments, text];
  // A read view of anything else is a text read, which the page would only
  // start after React mounts. A last segment with no extension is far more
  // often a folder than a file, and a folder has no bytes to fetch.
  if (NOT_TEXT.test(path) || HTML.test(path) || !/\.[A-Za-z0-9]+$/.test(path.split("/").pop() || "")) {
    return [...out, heat];
  }
  return [...out, text, heat];
}
