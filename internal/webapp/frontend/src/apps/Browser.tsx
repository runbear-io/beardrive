import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { atLeast } from "../api/types";
import { postJSON } from "../api/http";
import type { Project, ServerConfig } from "../api/types";
import { useHeat, useTree } from "../hooks/useBrowse";
import { useShares } from "../hooks/useHub";
import { urlForPath, urlForView, type Route } from "../router";
import { currentNavType, navigate, useLocationPath } from "../nav";
import { HTML_EXT, PDF_EXT, copyText } from "../util";
import { toast } from "../toast";
import { onSearchRequest } from "../search";
import { track } from "../analytics";
import { AppShell, Icon, Page, Topbar, closeSidebarOnMobile, type PageWidth } from "../components/shell";
import { FileTree, ancestorsOf } from "../components/FileTree";
import { Breadcrumbs } from "../components/Breadcrumbs";
import { FolderListing } from "../components/FolderListing";
import { FileView } from "../components/FileView";
import { ShareDialog } from "../components/ShareDialog";
import { ShareBanner } from "../components/ShareBanner";
import { Palette, type PaletteItem } from "../components/Palette";
import { ConnectGuide } from "../components/ConnectGuide";
import { Insights, useInsightsDevices } from "../components/Insights";
import { HistoryView, historyTitle } from "../components/HistoryView";
import { VersionBanner } from "../components/VersionBanner";

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
  sidebar: { vault: ReactNode; projectsNav?: ReactNode; orgBar?: ReactNode };
  // Admin panels (org admin, hub settings) replace the content pane without
  // touching the URL — matching the classic app, where they were never
  // routes. Any navigation closes them (the caller owns that state).
  panel?: { crumb: string; body: ReactNode } | null;
  onClosePanel?: () => void; // panels are not routes: same-path navigation needs an explicit close
}) {
  const { config, apiBase, route, hub, project } = props;
  const routeKey = useLocationPath(); // scroll memo key, one slot per URL
  const qc = useQueryClient();

  const { tree, flatFiles, dirIndex, loaded } = useTree(apiBase, !hub || !!project);
  const heatMap = useHeat(apiBase, hub && !!project && !!config.reads?.enabled);
  // Dashboard data: the per-device breakdown, plus a fresh heat fetch when
  // a dashboard surface opens (the ambient heat cache may be a minute old).
  const isHome = hub && !!project && !route.path && !route.view;
  const insightsOpen = route.view === "dashboard" || isHome;
  const devices = useInsightsDevices(apiBase, insightsOpen);
  useEffect(() => {
    if (insightsOpen) qc.invalidateQueries({ queryKey: ["heat", apiBase] });
  }, [insightsOpen, apiBase, qc]);

  const path = route.path;
  // ?v= belongs to the file page; a view route or folder ignores it.
  const version = !route.view ? route.version : undefined;
  // On scoped view routes (/dashboard/<p>, /history/<p>) the subject of the
  // page is the target — the tree highlights it, not a menu item.
  const treePath = path || (route.view === "dashboard" || route.view === "history" ? route.viewTarget || "" : "");
  const isDir = !!path && dirIndex.has(path);
  // A file only counts as one when the tree actually contains it — a
  // missing path gets the not-found view, not a broken file view.
  const isFile = !!path && loaded && !isDir && flatFiles.some((f) => f.path === path);
  const isMissing = !!path && loaded && !isDir && !isFile;
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
    // Opening any path (tree click, palette, wikilink, deep link — or a
    // scoped dashboard/history view of it) unfolds the way to it; a selected
    // folder itself opens too.
    if (!treePath || !loaded) return;
    setExpanded((s) => {
      const next = new Set(s);
      for (const a of ancestorsOf(treePath)) next.add(a);
      if (dirIndex.has(treePath)) next.add(treePath);
      return next;
    });
  }, [treePath, loaded, dirIndex]);
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
      key: routeKey,
      want: currentNavType() === "POP" ? (memo.current.get(routeKey) ?? 0) : 0,
      attempts: 0,
    };
  }, [routeKey]);
  const onRendered = useCallback(() => {
    const c = contentRef.current;
    const g = scrollGoal.current;
    if (!c || g.key !== routeKey || g.attempts >= 3) return;
    g.attempts++;
    c.scrollTo({ top: g.want, behavior: "instant" });
  }, [routeKey]);
  const onScroll = useCallback(() => {
    if (contentRef.current) memo.current.set(routeKey, contentRef.current.scrollTop);
  }, [routeKey]);

  /* ---- navigation ---- */
  const openPath = useCallback(
    // A version (a history row's content hash) pins the file page to those
    // exact bytes; without one the page is the current file.
    (p: string, v?: string) => {
      navigate(urlForPath(p, project?.id, v));
      closeSidebarOnMobile();
    },
    [project?.id],
  );
  const openHistory = useCallback(
    (target: string) => navigate(urlForView("history", project?.id, target)),
    [project?.id],
  );

  /* ---- topbar state + actions ---- */
  const [meta, setMeta] = useState("");
  const [share, setShare] = useState<{ url: string; copied: boolean } | null>(null);
  const [moreOpen, setMoreOpen] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  useEffect(() => onSearchRequest(() => setPaletteOpen(true)), []);
  const downloadRef = useRef<HTMLAnchorElement>(null);

  const panel = props.panel ?? null;
  // Minting a public link is a write. A read-only member sees no Share
  // button rather than a button that 403s.
  const canShare = !panel && hub && !!project && isFile && atLeast(project.perm, "write");
  // The project's live public links, filtered to the open file. One query
  // for the whole project (Settings reads the same cache entry), so opening
  // a file costs no extra request.
  const { data: shares } = useShares(project?.id, hub && !!project);
  const refreshShares = useCallback(
    () => void qc.invalidateQueries({ queryKey: ["shares", project?.id] }),
    [qc, project?.id],
  );
  const fileShares = isFile ? (shares || []).filter((s) => s.path === path) : [];
  const canHistory = !panel && hub && !!project;
  // Browser upload is deliberately absent (for now): content enters through
  // local sync only; the web app is a read/share/history surface.
  const canDownload = !panel && isFile;
  const canMore = !panel && (isFile || (hub && !!project && isDir));
  // Downloading while a version is open gives you THAT version — the ⋯ menu
  // offering the current bytes under a page framed as historical was half of
  // what made old versions unreachable.
  const downloadURL = version
    ? apiBase + "blob?sha=" + version + "&name=" + encodeURIComponent(path) + "&download=1"
    : apiBase + "download?path=" + encodeURIComponent(path);

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
      // Fired here rather than by the table in api/http.ts, because this is
      // the one write in the app that goes out as a raw fetch.
      track("share_created");
      const copied = await copyText(s.url);
      setShare({ url: s.url, copied });
      refreshShares(); // the banner appears (or stays) without a reload
    } catch (err) {
      toast("Share failed: " + (err as Error).message, true);
    }
  }, [apiBase, path, refreshShares]);

  /* ---- restore ----
     Putting an old version back is a write, so a read-only member sees no
     Restore button rather than one that 403s. The restore itself is a new
     change: the tree, the file, and the history feed all move. */
  const [restoring, setRestoring] = useState("");
  const canRestore = hub && !!project && atLeast(project?.perm, "write");
  const onRestore = useCallback(
    async (p: string, sha: string) => {
      setRestoring(p + sha);
      try {
        await postJSON(apiBase + "restore", { path: p, sha });
        qc.invalidateQueries({ queryKey: ["history", apiBase] });
        qc.invalidateQueries({ queryKey: ["tree", apiBase] });
        qc.invalidateQueries({ queryKey: ["render", apiBase, p] });
        qc.invalidateQueries({ queryKey: ["text"] });
        toast("Restored " + p + " — it syncs to every device like any other change.");
      } catch (err) {
        toast("Restore failed: " + (err as Error).message, true);
      } finally {
        setRestoring("");
      }
    },
    [apiBase, qc],
  );

  const historyNow = useCallback(() => {
    if (!path) return openHistory("");
    openHistory(isDir ? path + "/" : path);
  }, [path, isDir, openHistory]);

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
  }, [hub, project, path, isFile, config.auth?.enabled, dirIndex, flatFiles, props.projects, shareNow, historyNow, openHistory, openPath]);

  /* ---- "⋯ More" menu (secondary actions on narrow screens) ---- */
  useEffect(() => {
    if (!moreOpen) return;
    const close = () => setMoreOpen(false);
    document.addEventListener("click", close);
    return () => document.removeEventListener("click", close);
  }, [moreOpen]);

  /* ---- content view ---- */
  const isFolderFn = useCallback((p: string) => dirIndex.has(p), [dirIndex]);
  // One column decision per route (see <Page> in shell.tsx). File views also
  // carry the markdown typography class, which used to sit on #content itself
  // — putting the width there made the gutter eat into the reading column, so
  // .md pages ran 80px narrower than every other page.
  let pageWidth: PageWidth = "app";
  let pageClass: string | undefined;
  let view: ReactNode;
  if (panel) {
    view = panel.body;
  } else if (route.view === "dashboard") {
    view = (
      <Insights
        flatFiles={flatFiles}
        heatMap={heatMap}
        devices={devices}
        scope={route.viewTarget || ""}
        onOpenFile={openPath}
        onOpenFolder={openPath}
        isFolder={isFolderFn}
      />
    );
  } else if (route.view === "history") {
    // structured view — default app column, like the folder listing it shares rows with
    view = (
      <HistoryView
        apiBase={apiBase}
        target={route.viewTarget || ""}
        isFolder={isFolderFn}
        onOpen={openPath}
        onMeta={setMeta}
        onRendered={onRendered}
        restore={canRestore ? { onRestore, busy: restoring } : undefined}
      />
    );
  } else if (path) {
    if (!loaded) {
      view = <div className="empty">Loading…</div>;
    } else if (isMissing) {
      // The tree polls every few seconds, so a file that's mid-upload (or
      // mid-sync from a teammate's device) appears here on its own.
      view = (
        <div className="notfound">
          <h1>Couldn't find that</h1>
          <p>
            <code>{path}</code> isn't in this project right now.
          </p>
          <p className="nf-sub">
            If it was just created, it may still be uploading or syncing
            from a teammate's device — this page checks again automatically
            every few seconds, so refresh or come back in a moment.
          </p>
          <button
            className="pbtn"
            onClick={() => qc.invalidateQueries({ queryKey: ["tree", apiBase] })}
          >
            Check again
          </button>
        </div>
      );
    } else if (isDir) {
      view = ( // structured view — default app column; read is for rendered files only
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
      // A PDF page is unreadable squeezed into the 768px reading column.
      pageWidth = HTML_EXT.test(path) || PDF_EXT.test(path) ? "wide" : "read";
      pageClass = "markdown";
      view = (
        <>
          {version && (
            <VersionBanner
              apiBase={apiBase}
              path={path}
              version={version}
              onViewCurrent={() => openPath(path)}
            />
          )}
          <FileView
            apiBase={apiBase}
            path={path}
            version={version}
            heatMap={heatMap}
            flatFiles={flatFiles}
            onOpenFile={openPath}
            onMeta={setMeta}
            onRendered={onRendered}
          />
        </>
      );
    }
  } else if (isHome) {
    // The project's index page: the connect-an-agent guide, with the
    // dashboard below it.
    view = (
      <>
        <ConnectGuide project={project!} existing={route.connect === "existing"} />
        <div className="home-insights">
          <Insights
            flatFiles={flatFiles}
            heatMap={heatMap}
            devices={devices}
            onOpenFile={openPath}
            onOpenFolder={openPath}
            isFolder={isFolderFn}
          />
        </div>
      </>
    );
  } else {
    view = <div className="empty">Select a file to read it.</div>;
  }

  const crumb = panel ? (
    panel.crumb
  ) : path ? (
    <Breadcrumbs path={path} onOpenFolder={openPath} />
  ) : route.view === "dashboard" ? (
    "Dashboard — " + (route.viewTarget || project?.name || "")
  ) : route.view === "history" ? (
    "History — " + historyTitle(route.viewTarget || "", isFolderFn)
  ) : isHome ? (
    project!.name
  ) : null;

  const topbar = (
    <Topbar
      crumb={crumb}
      meta={meta}
      actions={
        <>
          {canShare && (
            <Button id="share-btn" variant="toolbar" className="icon-only" title="Share" aria-label="Share" onClick={shareNow}>
              <Icon name="share" />
            </Button>
          )}
          {canHistory && !path && !route.view && (
            <Button id="history-btn" variant="toolbar" onClick={historyNow}>
              <Icon name="hist" /> <span className="lbl">History</span>
            </Button>
          )}
          {canDownload && (
            <a id="download" hidden download href={downloadURL} ref={downloadRef}>
              Download
            </a>
          )}
          {canMore && (
            <Button
              id="more-btn"
              variant="toolbar"
              className="icon-only"
              title="More actions"
              aria-label="More actions"
              onClick={(e) => {
                e.stopPropagation();
                setMoreOpen(!moreOpen);
              }}
            >
              <Icon name="dots" />
            </Button>
          )}
          {moreOpen && (
            <div id="more-menu" role="menu">
              {canHistory && (
                <button className="more-item" onClick={historyNow}>
                  History
                </button>
              )}
              {canDownload && (
                <button className="more-item" onClick={() => downloadRef.current?.click()}>
                  Download
                </button>
              )}
              {hub && !!project && (
                <button
                  className="more-item"
                  onClick={() => {
                    props.onClosePanel?.();
                    navigate(urlForView("dashboard", project?.id, path));
                  }}
                >
                  Dashboard
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
            currentPath={treePath}
            listingShowing={listingShowing}
            onOpen={openPath}
          />
        }
        topbar={topbar}
        contentRef={contentRef}
        onContentScroll={onScroll}
      >
        <Page width={pageWidth} className={pageClass}>
          {!panel && isFile && (
            <ShareBanner
              shares={fileShares}
              canRevoke={!!project && atLeast(project.perm, "write")}
              onChanged={refreshShares}
            />
          )}
          {view}
        </Page>
      </AppShell>
      {share && (
        <ShareDialog
          url={share.url}
          copied={share.copied}
          onClose={() => {
            setShare(null);
            refreshShares(); // the dialog can revoke; the banner must agree
          }}
        />
      )}
      <Palette open={paletteOpen} onClose={() => setPaletteOpen(false)} candidates={paletteCandidates} />
    </>
  );
}
