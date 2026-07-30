import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HeatMap, Node, RenderDoc } from "../api/types";
import { heatTotal, heatText } from "../hooks/useBrowse";
import { useTextAt } from "../hooks/useBlob";
import { HTML_EXT, IMG_EXT, MD_EXT, PDF_EXT, TEXT_EXT, humanSize, joinPath } from "../util";

export function FileView(props: {
  apiBase: string;
  path: string;
  // Pinned to one past version by content hash (?v=), otherwise current.
  version?: string;
  heatMap: HeatMap | null;
  flatFiles: Node[];
  onOpenFile: (path: string) => void;
  onMeta: (meta: string) => void;
  onRendered?: () => void;
}) {
  const { apiBase, path, version, onMeta } = props;
  // A version is served by content hash; ?name= is what makes the server
  // set a real Content-Type, so images and text render instead of
  // downloading as octet-stream.
  const fileURL = version
    ? apiBase + "blob?sha=" + version + "&name=" + encodeURIComponent(path)
    : apiBase + "file?path=" + encodeURIComponent(path);

  useEffect(() => () => onMeta(""), [path, onMeta]); // leaving a file clears its meta line

  if (MD_EXT.test(path)) return <MarkdownView {...props} />;
  if (HTML_EXT.test(path)) {
    // Rendered, not shown as source — inside a sandboxed iframe so synced
    // HTML never runs with the hub origin's session (the server also
    // stamps the response with a sandbox CSP; this is belt and braces).
    return (
      <iframe
        className="htmlview"
        sandbox="allow-scripts"
        src={fileURL}
        title={path}
        onLoad={props.onRendered}
      />
    );
  }
  if (PDF_EXT.test(path)) {
    // The browser's own viewer, streaming — no byte cap needed, nothing is
    // held in JS memory. Deliberately NOT sandboxed: the PDF viewer is not
    // this page's JS realm, so it can't reach the hub API or its cookies,
    // and sandbox without allow-same-origin breaks Firefox's pdf.js.
    return <iframe className="pdfview" src={fileURL} title={path} onLoad={props.onRendered} />;
  }
  if (IMG_EXT.test(path)) {
    return <ImgView src={fileURL} alt={path} version={version} onRendered={props.onRendered} />;
  }
  if (TEXT_EXT.test(path)) return <TextView {...props} fileURL={fileURL} />;
  // No extension we recognize: decide on the bytes instead of giving up.
  return <SniffView {...props} fileURL={fileURL} />;
}

/* The fallthrough: one fetch, then text / binary / too-large. Only files
   that used to show the dead "No preview" card get here, so nothing that
   already previewed pays for the extra request. */
function SniffView(props: Parameters<typeof FileView>[0] & { fileURL: string }) {
  const { apiBase, path, version, fileURL, onRendered } = props;
  // The ["text", url] family is what a restore invalidates (Browser.tsx);
  // an immutable ["blob", …] key on a live path would go stale after a
  // teammate's edit. A ?v= URL is content-addressed, so it can be pinned.
  const { data, error } = useTextAt(fileURL, ["text", fileURL], true, !!version);
  useEffect(() => {
    if (data) onRendered?.();
  }, [data, onRendered]);
  if (error) return <LoadError version={version} err={error as Error} />;
  if (!data) return null;
  if (data.kind === "text")
    return (
      <pre className="plain" key={path}>
        {data.text}
      </pre>
    );
  return (
    <FileCard apiBase={apiBase} path={path} version={version} fileURL={fileURL}>
      {data.kind === "too-large"
        ? `Too large to preview (${humanSize(data.size)}).`
        : "No preview for this file type."}
    </FileCard>
  );
}

function FileCard(props: {
  apiBase: string;
  path: string;
  version?: string;
  fileURL: string;
  children: React.ReactNode;
}) {
  const { apiBase, path, version, fileURL } = props;
  return (
    <div className="filecard">
      <div className="name">{path.split("/").pop()}</div>
      <p>{props.children}</p>
      <a
        className="btn"
        download
        href={
          version ? fileURL + "&download=1" : apiBase + "download?path=" + encodeURIComponent(path)
        }
      >
        Download
      </a>
    </div>
  );
}

function MarkdownView(props: Parameters<typeof FileView>[0]) {
  const { apiBase, path, version, heatMap, flatFiles, onOpenFile, onMeta, onRendered } = props;
  const { data: doc, error } = useQuery({
    queryKey: ["render", apiBase, path, version || ""],
    queryFn: () =>
      getJSON<RenderDoc>(
        apiBase + "render?path=" + encodeURIComponent(path) + (version ? "&sha=" + version : ""),
      ),
    // A blob that isn't there will not appear on a retry, and the retry's
    // delay is a blank pane the reader has no explanation for.
    retry: version ? false : undefined,
  });

  // Rewrite the HTML BEFORE rendering (relative image sources, external
  // link targets) rather than patching the live DOM afterwards: React owns
  // the dangerouslySetInnerHTML subtree and may re-apply the markup on any
  // update, silently discarding post-commit DOM patches. Link navigation
  // is delegated on the container for the same reason.
  const html = useMemo(
    () => (doc ? transformHTML(doc.html, path, apiBase) : ""),
    [doc, path, apiBase],
  );

  useEffect(() => {
    if (!doc) return;
    const parts: string[] = [];
    if (doc.author) parts.push(doc.author + (doc.device ? " on " + doc.device : ""));
    if (doc.time) parts.push(new Date(doc.time).toLocaleString());
    // Read counts belong to the path, not to one version — showing them
    // beside content the banner just called historical reads as if they
    // counted views of these bytes.
    const he = version ? null : heatMap && heatMap[doc.path];
    if (he && heatTotal(he)) parts.push(heatText(he) + " / 30d");
    onMeta(parts.join(" · "));
    onRendered?.();
  }, [doc, version, heatMap, onMeta, onRendered]);

  if (error) return <LoadError version={version} err={error as Error} />;
  if (!doc) return null;
  // Server-rendered, server-sanitized markdown — same trust model as the
  // classic app assigning innerHTML.
  return (
    <div
      dangerouslySetInnerHTML={{ __html: html }}
      onClick={(e) => handleLinkClick(e, path, flatFiles, onOpenFile)}
    />
  );
}

/* Delegated click handling for rendered-markdown links: wiki: targets
   resolve by basename, relative links resolve against the current file's
   folder, everything else keeps its native behavior. */
function handleLinkClick(
  e: React.MouseEvent,
  p: string,
  flatFiles: Node[],
  openFile: (path: string) => void,
) {
  const a = (e.target as HTMLElement).closest("a");
  if (!a || !(e.currentTarget as HTMLElement).contains(a)) return;
  const href = a.getAttribute("href") || "";
  const dir = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
  if (href.startsWith("wiki:")) {
    e.preventDefault();
    openWikilink(decodeURIComponent(href.slice(5)), flatFiles, openFile);
  } else if (!/^([a-z]+:|\/|#)/i.test(href)) {
    e.preventDefault();
    openFile(joinPath(dir, decodeURIComponent(href)));
  }
}

/* String-level rewrite of the server's HTML: relative image sources point
   at the file API, external links open in a new tab. */
function transformHTML(html: string, p: string, apiBase: string): string {
  const dir = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
  const fileURL = (path: string) => apiBase + "file?path=" + encodeURIComponent(path);
  const parsed = new DOMParser().parseFromString(html, "text/html");
  for (const img of parsed.querySelectorAll("img")) {
    const src = img.getAttribute("src") || "";
    if (!/^([a-z]+:|\/)/i.test(src)) img.setAttribute("src", fileURL(joinPath(dir, src)));
  }
  for (const a of parsed.querySelectorAll("a")) {
    const href = a.getAttribute("href") || "";
    if (/^https?:/i.test(href)) {
      a.setAttribute("target", "_blank");
      a.setAttribute("rel", "noopener");
    }
  }
  return parsed.body.innerHTML;
}

function ImgView(props: { src: string; alt: string; version?: string; onRendered?: () => void }) {
  const [failed, setFailed] = useState(false);
  if (failed) return <LoadError version={props.version} err={new Error("could not be loaded")} />;
  return (
    <img src={props.src} alt={props.alt} onLoad={props.onRendered} onError={() => setFailed(true)} />
  );
}

/* A missing current file is a server problem worth quoting; a missing
   version is almost always a bad ?v= in a hand-edited or stale URL, which
   the server's "no such version" wording does not explain. */
function LoadError({ version, err }: { version?: string; err: Error }) {
  return (
    <div className="empty">
      {version ? "That version isn't available." : "Could not load file: " + err.message}
    </div>
  );
}

function TextView(props: Parameters<typeof FileView>[0] & { fileURL: string }) {
  const { path, version, fileURL, onRendered } = props;
  const { data, error } = useQuery({
    queryKey: ["text", fileURL],
    queryFn: async () => {
      const r = await fetch(fileURL);
      if (!r.ok) throw new Error(await r.text());
      return r.text();
    },
    retry: version ? false : undefined,
  });
  useEffect(() => {
    if (data != null) onRendered?.();
  }, [data, onRendered]);
  if (error) return <LoadError version={version} err={error as Error} />;
  if (data == null) return null;
  return (
    <pre className="plain" key={path}>
      {data}
    </pre>
  );
}

function openWikilink(target: string, flatFiles: Node[], openFile: (path: string) => void) {
  const want = target.toLowerCase();
  const hit =
    flatFiles.find((f) => f.path.toLowerCase() === want || f.path.toLowerCase() === want + ".md") ||
    flatFiles.find((f) => {
      const n = f.name.toLowerCase();
      return n === want || n === want + ".md";
    });
  if (hit) openFile(hit.path);
}
