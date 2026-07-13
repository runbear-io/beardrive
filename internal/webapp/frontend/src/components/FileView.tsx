import { useEffect, useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HeatMap, Node, RenderDoc } from "../api/types";
import { heatTotal, heatText } from "../hooks/useBrowse";
import { IMG_EXT, MD_EXT, TEXT_EXT, joinPath } from "../util";

export function FileView(props: {
  apiBase: string;
  path: string;
  heatMap: HeatMap | null;
  flatFiles: Node[];
  onOpenFile: (path: string) => void;
  onMeta: (meta: string) => void;
  onRendered?: () => void;
}) {
  const { apiBase, path, onMeta } = props;
  const fileURL = apiBase + "file?path=" + encodeURIComponent(path);

  useEffect(() => () => onMeta(""), [path, onMeta]); // leaving a file clears its meta line

  if (MD_EXT.test(path)) return <MarkdownView {...props} />;
  if (IMG_EXT.test(path)) {
    return <ImgView src={fileURL} alt={path} onRendered={props.onRendered} />;
  }
  if (TEXT_EXT.test(path)) return <TextView {...props} fileURL={fileURL} />;
  return (
    <div className="filecard">
      <div className="name">{path.split("/").pop()}</div>
      <p>No preview for this file type.</p>
      <a className="btn" download href={apiBase + "download?path=" + encodeURIComponent(path)}>
        Download
      </a>
    </div>
  );
}

function MarkdownView(props: Parameters<typeof FileView>[0]) {
  const { apiBase, path, heatMap, flatFiles, onOpenFile, onMeta, onRendered } = props;
  const { data: doc, error } = useQuery({
    queryKey: ["render", apiBase, path],
    queryFn: () => getJSON<RenderDoc>(apiBase + "render?path=" + encodeURIComponent(path)),
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
    const he = heatMap && heatMap[doc.path];
    if (he && heatTotal(he)) parts.push(heatText(he) + " / 30d");
    onMeta(parts.join(" · "));
    onRendered?.();
  }, [doc, heatMap, onMeta, onRendered]);

  if (error) return <div className="empty">Could not load file: {(error as Error).message}</div>;
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

function ImgView({ src, alt, onRendered }: { src: string; alt: string; onRendered?: () => void }) {
  return <img src={src} alt={alt} onLoad={onRendered} />;
}

function TextView(props: Parameters<typeof FileView>[0] & { fileURL: string }) {
  const { path, fileURL, onRendered } = props;
  const { data, error } = useQuery({
    queryKey: ["text", fileURL],
    queryFn: async () => {
      const r = await fetch(fileURL);
      if (!r.ok) throw new Error(await r.text());
      return r.text();
    },
  });
  useEffect(() => {
    if (data != null) onRendered?.();
  }, [data, onRendered]);
  if (error) return <div className="empty">Could not load file: {(error as Error).message}</div>;
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
