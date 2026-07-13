import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useLocation, useNavigate, useNavigationType } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import type { Project, ServerConfig } from "../api/types";
import { useHeat, useTree } from "../hooks/useBrowse";
import { urlForPath, urlForView, type Route } from "../router";
import { uploadFile } from "../upload";
import { copyText } from "../util";
import { toast } from "../toast";
import { AppShell, Icon, Topbar, closeSidebarOnMobile } from "../components/shell";
import { FileTree, ancestorsOf } from "../components/FileTree";
import { Breadcrumbs } from "../components/Breadcrumbs";
import { FolderListing } from "../components/FolderListing";
import { FileView } from "../components/FileView";
import { ShareDialog } from "../components/ShareDialog";
import { Palette, type PaletteItem } from "../components/Palette";

// The browsing surface shared by hub projects and single-volume mode: the
// file tree, folder listings, file views, and every topbar action. Sidebar
// chrome (vault header, project nav, org bar) is injected by the caller;
// key this component by project id so tree state resets on project switch.
export default function Browser(props: {
  config: ServerConfig;
  apiBase: string;
  route: Route;
  hub: boolean;
  project?: Project;
  projects?: Project[];
  canInsights?: boolean;
  sidebar: { vault: ReactNode; projectsNav?: ReactNode; orgBar?: ReactNode };
  home?: ReactNode; // hub landing view (project home); default prompt otherwise
}) {
  const { config, apiBase, route, hub, project } = props;
  const navigate = useNavigate();
  const location = useLocation();
  const navType = useNavigationType();
  const qc = useQueryClient();

  const { tree, flatFiles, dirIndex, loaded } = useTree(apiBase, !hub || !!project);
  const heatMap = useHeat(apiBase, hub && !!project && !!config.reads?.enabled);

  const path = route.path;
  const isDir = !!path && dirIndex.has(path);
  const isFile = !!path && loaded && !dirIndex.has(path);
  const listingShowing = isDir && !route.view;

  /* ---- tree expansion ---- */
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set());
  const firstLoad = useRef(true);
  useEffect(() => {
    // First render of the tree: every folder starts closed, except a lone
    // root folder — opening it spares the user a single shut folder.
    if (!tree || !firstLoad.current) return;
    firstLoad.current = false;
    const rootDirs = (tree.children || []).filter((c) => c.dir);
    if (rootDirs.length === 1) setExpanded((s) => new Set(s).add(rootDirs[0].path));
  }, [tree]);
  useEffect(() => {
    // Opening any path (tree click, palette, wikilink, deep link) unfolds
    // the way to it; a selected folder itself opens too.
    if (!path || !loaded) return;
    setExpanded((s) => {
      const next = new Set(s);
      for (const a of ancestorsOf(path)) next.add(a);
      if (dirIndex.has(path)) next.add(path);
      return next;
    });
    const row = document.querySelector(`#tree .row[data-path="${CSS.escape(path)}"]`);
    if (row) row.scrollIntoView({ block: "nearest" });
  }, [path, loaded, dirIndex]);
  const onToggle = useCallback((p: string) => {
    setExpanded((s) => {
      const next = new Set(s);
      if (next.has(p)) next.delete(p);
      else next.add(p);
      return next;
    });
  }, []);

  /* ---- per-route scroll restoration ----
     Back/forward returns to where the reader was; fresh navigations start
     at the top. Views call onRendered when their content lands (and again
     when async sections grow), and we re-apply the target until it fits. */
  const contentRef = useRef<HTMLElement>(null);
  const memo = useRef(new Map<string, number>());
  const scrollGoal = useRef({ key: "", want: 0, attempts: 0 });
  useEffect(() => {
    scrollGoal.current = {
      key: location.key,
      want: navType === "POP" ? (memo.current.get(location.key) ?? 0) : 0,
      attempts: 0,
    };
  }, [location.key, navType]);
  const onRendered = useCallback(() => {
    const c = contentRef.current;
    const g = scrollGoal.current;
    if (!c || g.key !== location.key || g.attempts >= 3) return;
    g.attempts++;
    c.scrollTo({ top: g.want, behavior: "instant" });
  }, [location.key]);
  const onScroll = useCallback(() => {
    if (contentRef.current) memo.current.set(location.key, contentRef.current.scrollTop);
  }, [location.key]);

  /* ---- navigation ---- */
  const openPath = useCallback(
    (p: string) => {
      navigate(urlForPath(p, project?.id));
      closeSidebarOnMobile();
    },
    [navigate, project?.id],
  );
  const openHistory = useCallback(
    (target: string) => navigate(urlForView("history", project?.id, target)),
    [navigate, project?.id],
  );

  /* ---- topbar state + actions ---- */
  const [meta, setMeta] = useState("");
  const [uploadStatus, setUploadStatus] = useState("");
  const [share, setShare] = useState<{ url: string; copied: boolean } | null>(null);
  const [moreOpen, setMoreOpen] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const uploadInput = useRef<HTMLInputElement>(null);
  const downloadRef = useRef<HTMLAnchorElement>(null);

  const canShare = hub && !!project && isFile;
  const canHistory = hub && !!project;
  const canUpload = !!config.upload?.enabled && (!hub || !!project);
  const canDownload = isFile;
  const canMore = isFile || (hub && !!project && isDir);
  const downloadURL = apiBase + "download?path=" + encodeURIComponent(path);

  const shareNow = useCallback(async () => {
    // Shares are per-file; a selected folder has nothing to mint.
    try {
      const r = await fetch(apiBase + "shares", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path }),
      });
      if (!r.ok) throw new Error(await r.text());
      const s = await r.json();
      const copied = await copyText(s.url);
      setShare({ url: s.url, copied });
    } catch (err) {
      toast("Share failed: " + (err as Error).message, true);
    }
  }, [apiBase, path]);

  const historyNow = useCallback(() => {
    if (!path) return openHistory("");
    openHistory(isDir ? path + "/" : path);
  }, [path, isDir, openHistory]);

  const uploadNow = useCallback(() => uploadInput.current?.click(), []);
  const onUploadPick = async () => {
    const input = uploadInput.current!;
    const file = input.files?.[0];
    input.value = "";
    if (!file) return;
    // A selected folder receives the upload; a selected file means "next
    // to it".
    const dir = !path ? "" : isDir ? path : path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
    const dest = dir ? dir + "/" + file.name : file.name;
    try {
      setUploadStatus(`Uploading ${dest}…`);
      await uploadFile(apiBase, dest, file);
      setUploadStatus(`Uploaded ${dest}`);
      await qc.invalidateQueries({ queryKey: ["tree", apiBase] });
      openPath(dest);
    } catch (err) {
      setUploadStatus("Upload failed: " + (err as Error).message);
    }
  };

  useEffect(() => {
    // Any navigation clears a stale upload status from the meta slot.
    setUploadStatus("");
  }, [location.key]);

  /* ---- ⌘K palette ---- */
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPaletteOpen((v) => !v);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const paletteCandidates = useCallback((): PaletteItem[] => {
    const items: PaletteItem[] = [];
    const add = (icon: string, label: string, kind: string, run: () => void) =>
      items.push({ icon, label, kind, run });
    if (hub && project && path) {
      if (isFile) add("share", "Share: " + path, "action", shareNow);
      add("hist", "History: " + path, "action", historyNow);
      if (isFile) add("download", "Download: " + path, "action", () => downloadRef.current?.click());
    }
    if (hub && project) add("hist", "History: whole project", "action", () => openHistory(""));
    if (canUpload) add("upload", "Upload a file…", "action", uploadNow);
    if (hub) {
      for (const p of props.projects || []) {
        if (!project || p.id !== project.id) {
          add("folder", "Switch to project: " + p.name, "project", () => navigate("/" + p.id));
        }
      }
    }
    if (config.auth?.enabled) {
      add("power", "Sign out", "action", () => (window.location.href = "/auth/logout"));
    }
    for (const d of dirIndex.keys()) add("folder", d, "folder", () => openPath(d));
    for (const f of flatFiles) add("doc", f.path, "file", () => openPath(f.path));
    return items;
  }, [hub, project, path, isFile, canUpload, config.auth?.enabled, dirIndex, flatFiles, props.projects, shareNow, historyNow, uploadNow, openHistory, openPath, navigate]);

  /* ---- "⋯ More" menu (secondary actions on narrow screens) ---- */
  useEffect(() => {
    if (!moreOpen) return;
    const close = () => setMoreOpen(false);
    document.addEventListener("click", close);
    return () => document.removeEventListener("click", close);
  }, [moreOpen]);

  /* ---- content view ---- */
  let contentClass = "markdown";
  let view: ReactNode;
  if (route.view === "insights" || route.view === "history") {
    // Phase 3 delivers these views.
    contentClass = "view";
    view = <div className="empty">{route.view} view is on its way.</div>;
  } else if (path) {
    if (!loaded) {
      view = <div className="empty">Loading…</div>;
    } else if (isDir) {
      contentClass = "view";
      view = (
        <FolderListing
          node={dirIndex.get(path)!}
          heatMap={heatMap}
          hub={hub && !!project}
          apiBase={apiBase}
          onOpen={openPath}
          onFullHistory={openHistory}
          onRendered={onRendered}
        />
      );
    } else {
      view = (
        <FileView
          apiBase={apiBase}
          path={path}
          heatMap={heatMap}
          flatFiles={flatFiles}
          onOpenFile={openPath}
          onMeta={setMeta}
          onRendered={onRendered}
        />
      );
    }
  } else if (props.home) {
    contentClass = "view";
    view = props.home;
  } else {
    view = <div className="empty">Select a file to read it.</div>;
  }

  const crumb = path ? <Breadcrumbs path={path} onOpenFolder={openPath} /> : null;

  const topbar = (
    <Topbar
      crumb={crumb}
      meta={uploadStatus || meta}
      actions={
        <>
          <button className="btn ghost" title="Search (⌘K)" onClick={() => setPaletteOpen(true)}>
            <Icon name="search" /> <span className="lbl">Search</span> <kbd>⌘K</kbd>
          </button>
          {canShare && (
            <button id="share-btn" className="btn" onClick={shareNow}>
              <Icon name="share" /> <span className="lbl">Share</span>
            </button>
          )}
          {canHistory && (
            <button id="history-btn" className="btn" onClick={historyNow}>
              <Icon name="hist" /> <span className="lbl">History</span>
            </button>
          )}
          {canUpload && (
            <button id="upload-btn" className="btn" onClick={uploadNow}>
              <Icon name="upload" /> <span className="lbl">Upload</span>
            </button>
          )}
          <input type="file" hidden ref={uploadInput} onChange={onUploadPick} />
          {canDownload && (
            <a id="download" className="btn" download href={downloadURL} ref={downloadRef}>
              <Icon name="download" /> <span className="lbl">Download</span>
            </a>
          )}
          {canMore && (
            <button
              id="more-btn"
              className="btn icon-only"
              title="More actions"
              aria-label="More actions"
              onClick={(e) => {
                e.stopPropagation();
                setMoreOpen(!moreOpen);
              }}
            >
              <Icon name="dots" />
            </button>
          )}
          {moreOpen && (
            <div id="more-menu" role="menu">
              {canHistory && (
                <button className="more-item" onClick={historyNow}>
                  History
                </button>
              )}
              {canUpload && (
                <button className="more-item" onClick={uploadNow}>
                  Upload
                </button>
              )}
              {canDownload && (
                <button className="more-item" onClick={() => downloadRef.current?.click()}>
                  Download
                </button>
              )}
              {props.canInsights && (
                <button
                  className="more-item"
                  onClick={() => navigate(urlForView("insights", project?.id))}
                >
                  Insights
                </button>
              )}
            </div>
          )}
        </>
      }
    />
  );

  return (
    <>
      <AppShell
        vault={props.sidebar.vault}
        projectsNav={props.sidebar.projectsNav}
        orgBar={props.sidebar.orgBar}
        tree={
          <FileTree
            root={tree}
            expanded={expanded}
            onToggle={onToggle}
            currentPath={path}
            listingShowing={listingShowing}
            onOpen={openPath}
          />
        }
        topbar={topbar}
        contentClass={contentClass}
        contentRef={contentRef}
        onContentScroll={onScroll}
      >
        {view}
      </AppShell>
      {share && <ShareDialog url={share.url} copied={share.copied} onClose={() => setShare(null)} />}
      <Palette open={paletteOpen} onClose={() => setPaletteOpen(false)} candidates={paletteCandidates} />
    </>
  );
}
