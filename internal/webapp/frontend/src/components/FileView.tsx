import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HeatMap, Node, RenderDoc } from "../api/types";
import { fetchBlobText, fileURLFor, isTransientBlobError, textQueryOptions, useFrameURL, type BlobText } from "../hooks/useBlob";
import type { SharedFile } from "../lib/sharedfile";
import {
  CSV_EXT,
  HTML_EXT,
  IMG_EXT,
  MD_EXT,
  PDF_EXT,
  TEXT_EXT,
  humanSize,
  joinPath,
  resolveWiki,
} from "../util";
import { urlForPath, urlForView } from "../router";
import { DocHeader, FileRail, useWideRail } from "./FileRail";
import { CSV_ROWS, parseDelimited, type Csv } from "../lib/csv";
import { DARK, LIGHT, hasMermaid, renderMermaid } from "../lib/mermaid";
import { useTheme } from "../lib/theme";
import { secretsBadge, type SecretFinding } from "../lib/secrets";
import { Icon } from "./shell";
import { toast } from "../toast";
import type { EditMode, EditorInfo, SaveState } from "./Editor";
import { EditRail, type RestoreAnswer } from "./EditRail";
import { Avatar } from "./PresenceBar";
import { agoShort, byteLength, isMeetingNote, sectionAt, sections, wordCount } from "../lib/meeting";
import type { CollabStatus } from "../lib/collab";

/* The editors carry CodeMirror, TipTap/ProseMirror and the Yjs collaboration
   stack, none of which a reader needs, so they load when someone edits.

   Loaded by hand rather than with React.lazy: lazy suspends on its first
   render even when the module is already here, and React then holds the
   Suspense fallback for at least 300 ms. A module fetched ahead of time (see
   preloadTextEditor / preloadVisualEditor) is rendered on the first pass
   instead. */
type EditorModule = typeof import("./Editor");
type VisualEditModule = typeof import("./VisualEdit");
let editorModule: EditorModule | null = null;
let visualModule: VisualEditModule | null = null;
let editorLoad: Promise<EditorModule> | null = null;
let visualLoad: Promise<VisualEditModule> | null = null;
// A failed import is forgotten rather than cached, so the next attempt tries
// the network again (a deploy can replace the chunk under an open tab).
const loadEditor = () =>
  (editorLoad ??= import("./Editor")
    .then((m) => (editorModule = m))
    .catch((e) => {
      editorLoad = null;
      throw e;
    }));
const loadVisualEdit = () =>
  (visualLoad ??= import("./VisualEdit")
    .then((m) => (visualModule = m))
    .catch((e) => {
      visualLoad = null;
      throw e;
    }));
// The loader for a surface this page does not show: never settles, so the
// module stays unloaded and no failure is reported for it.
const notNeeded = () => new Promise<never>(() => {});

function useModule<T>(have: T | null, load: () => Promise<T>): { mod: T | null; failed: boolean; retry: () => void } {
  const [m, setM] = useState(have);
  const [failed, setFailed] = useState(false);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    if (m) return;
    let live = true;
    load().then(
      (got) => live && setM(got),
      () => live && setFailed(true),
    );
    return () => {
      live = false;
    };
  }, [m, load, attempt]);
  return {
    mod: m,
    failed,
    retry: () => {
      setFailed(false);
      setAttempt((a) => a + 1);
    },
  };
}

// What the editing page has to load before it can show anything: the editor
// code, and the file's text under the key EditView reads. Started ahead of the
// click so pressing Edit has nothing left to wait for. One chunk per surface:
// a Markdown or code file never needs TipTap, and an HTML page never needs
// the source editor unless its reader asks for the markup.
export function preloadTextEditor() {
  void loadEditor();
}

export function preloadVisualEditor() {
  void loadVisualEdit();
}

export function preloadEditorForPath(path: string) {
  if (HTML_EXT.test(path)) preloadVisualEditor();
  else preloadTextEditor();
}

/* Live file URLs a read was started for ahead of editing — hovers, row
   prefetches, a read view warming its source. Opening the editor must not
   ride such a read: it was asked before the click, perhaps before a peer's
   write. The boot prefetch of an /edit/ link is not among them: that read IS
   the edit's own, and restarting it would only double a cold open. */
const readsBeforeEdit = new Set<string>();

export function prefetchEditText(qc: QueryClient, apiBase: string, path: string) {
  const url = fileURLFor(apiBase, path);
  if (readsBeforeEdit.size >= 128) {
    const oldest = readsBeforeEdit.values().next().value;
    if (oldest !== undefined) readsBeforeEdit.delete(oldest);
  }
  readsBeforeEdit.add(url);
  void qc.prefetchQuery(textQueryOptions(url));
}

/* What opening a file will read, started from a hover or press on its row:
   the rendered page for Markdown, and the bytes for anything read as text
   (Markdown's too, so Edit is instant from there). Images, PDFs and HTML
   load from their own URL in the page and need nothing here. Both go through
   the queries the views read, so a fresh copy costs nothing. */
export function prefetchFile(qc: QueryClient, apiBase: string, path: string) {
  if (MD_EXT.test(path)) void qc.prefetchQuery(renderQuery(apiBase, path));
  if (IMG_EXT.test(path) || PDF_EXT.test(path) || HTML_EXT.test(path)) return;
  prefetchEditText(qc, apiBase, path);
}

export function FileView(props: {
  apiBase: string;
  path: string;
  // Pinned to one past version by content hash (?v=), otherwise current.
  version?: string;
  heatMap: HeatMap | null;
  flatFiles: Node[];
  // Hub mode only; absent in volume mode, where file URLs are "/<path>".
  projectId?: string;
  onOpenFile: (path: string) => void;
  onMeta: (meta: ReactNode) => void;
  onRendered?: () => void;
  // Who is editing, for the label and colour on this account's caret in
  // everyone else's editor.
  me?: { name: string; colour: string };
  // Editing is a write, so the caller decides whether this reader may: never
  // on a pinned past version (that would silently fork history at ?v=), and
  // never below write permission.
  editing?: boolean;
  // Editing an HTML file's MARKUP rather than its rendered page. The escape
  // hatch for the structural work click-to-edit deliberately cannot express:
  // adding a section, fixing a tag, repairing something the visual editor
  // refuses to open. Ignored for every other file type, which has only ever
  // had the one surface.
  editSource?: boolean;
  // An HTML file renders its commentable variant (comments.go's ?comment=1),
  // which carries the bootstrap Comments.tsx talks to.
  commentable?: boolean;
  // Filled while editing with "save the open file now": what Done awaits
  // before it leaves, so the last words are on the hub, not a pending idle
  // timer or room snapshot. False: the save is waiting on a question to the
  // user (a sharp shrink), which leaving would withdraw unanswered.
  flushRef?: { current: (() => Promise<boolean>) | null };
}) {
  const { apiBase, path, version, onMeta } = props;
  const fileURL = fileURLFor(apiBase, path, version);

  useEffect(() => () => onMeta(""), [path, onMeta]); // leaving a file clears its meta line

  // A symlink has no content of its own: show the link and its target, never
  // resolve it. This takes precedence over every reader and the editor below —
  // there are no bytes here to render or edit.
  const link = props.flatFiles.find((f) => f.path === path)?.link;
  if (link) {
    return (
      <SymlinkView
        path={path}
        target={link}
        flatFiles={props.flatFiles}
        onOpenFile={props.onOpenFile}
        onRendered={props.onRendered}
      />
    );
  }

  // One editor for every text kind: markdown, csv, code, plain. The rendered
  // views below are what you get when you are reading rather than writing.
  if (props.editing) return <EditView {...props} />;
  if (MD_EXT.test(path)) return <MarkdownView {...props} />;
  if (HTML_EXT.test(path)) return <HtmlView {...props} fileURL={fileURL} />;
  if (PDF_EXT.test(path)) {
    // The browser's own viewer, streaming — no byte cap needed, nothing is
    // held in JS memory. Deliberately NOT sandboxed: the PDF viewer is not
    // this page's JS realm, so it can't reach the hub API or its cookies,
    // and sandbox without allow-same-origin breaks Firefox's pdf.js.
    return (
      <iframe
        className="pdfview"
        src={fileURL}
        title={path}
        onLoad={props.onRendered}
      />
    );
  }
  if (IMG_EXT.test(path)) {
    return (
      <ImgView
        src={fileURL}
        alt={path}
        version={version}
        onRendered={props.onRendered}
      />
    );
  }
  // Same component as plain text on purpose: the fallback for a file the
  // parser can't make a table of is then the very JSX it already renders,
  // not a second code path to keep in sync.
  if (CSV_EXT.test(path)) {
    return (
      <TextView
        {...props}
        fileURL={fileURL}
        delim={/\.tsv$/i.test(path) ? "\t" : ","}
      />
    );
  }
  if (TEXT_EXT.test(path)) return <TextView {...props} fileURL={fileURL} />;
  // No extension we recognize: decide on the bytes instead of giving up.
  return <SniffView {...props} fileURL={fileURL} />;
}

/* A rendered HTML file, and it FOLLOWS the file.

   Rendered, not shown as source — inside a sandboxed iframe so synced HTML
   never runs with the hub origin's session (the server also stamps the
   response with a sandbox CSP; this is belt and braces). allow-downloads is
   on BOTH sides, and has to be: the browser applies the intersection, so a
   flag missing from either one blocks the page's own export buttons
   (server.go's inlineSandbox says what it does and does not grant).

   The reload is the part worth explaining. An iframe loads once and then sits
   there, so the page a reader is looking at is the file as it was when they
   opened it. Leaving the visual editor made that obvious and awkward: the read
   view mounts the instant Done is pressed, which is BEFORE the save lands, so
   it rendered the pre-edit file and stayed that way — the edit was on the hub
   and nowhere on screen. Following the change stream fixes that case and a
   quieter one with it: a teammate's edit now appears instead of the page going
   stale under you. */
function HtmlView(props: Parameters<typeof FileView>[0] & { fileURL: string }) {
  const { apiBase, path, version, onRendered } = props;
  const frame = useFrameURL(apiBase, path, props.fileURL, version);
  const fileURL = frame.url;
  const fresh = useRef(frame.fresh);
  fresh.current = frame.fresh;
  const [rev, setRev] = useState(0);
  useEffect(() => {
    const onChanged = (e: Event) => {
      if (!(e as CustomEvent<string[]>).detail?.includes(path)) return;
      void fresh.current().finally(() => setRev((r) => r + 1));
    };
    window.addEventListener("bdrive:changed", onChanged);
    return () => window.removeEventListener("bdrive:changed", onChanged);
  }, [path]);
  if (!fileURL) return null;
  return (
    <iframe
      // Re-mounted rather than re-pointed: an iframe whose src is unchanged
      // does not reload, and the src IS unchanged — it is the same file.
      key={rev}
      className="htmlview"
      sandbox="allow-scripts allow-downloads"
      src={props.commentable ? fileURL + (fileURL.includes("?") ? "&" : "?") + "comment=1" : fileURL}
      title={path}
      onLoad={onRendered}
    />
  );
}

/* The fallthrough: one fetch, then text / binary / too-large. Only files
   that used to show the dead "No preview" card get here, so nothing that
   already previewed pays for the extra request. */
function SniffView(
  props: Parameters<typeof FileView>[0] & { fileURL: string },
) {
  const { apiBase, path, version, fileURL, onRendered } = props;
  // The ["text", url] family is what a restore invalidates (Browser.tsx);
  // an immutable ["blob", …] key on a live path would go stale after a
  // teammate's edit. A ?v= URL is content-addressed, so it can be pinned.
  const { data, error } = useQuery(textQueryOptions(fileURL, !!version));
  useEffect(() => {
    if (data) onRendered?.();
  }, [data, onRendered]);
  if (error) return <LoadError version={version} err={error as Error} />;
  if (!data) return <FileSkeleton />;
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
          version
            ? fileURL + "&download=1"
            : apiBase + "download?path=" + encodeURIComponent(path)
        }
      >
        Download
      </a>
    </div>
  );
}

// resolveLink resolves a symlink target against the link's own directory, the
// way a filesystem would, and returns the in-project path — or null if the
// target is absolute or climbs above the project root, i.e. points outside.
function resolveLink(fromPath: string, target: string): string | null {
  if (target.startsWith("/")) return null; // absolute: outside the project
  const dir = fromPath.split("/").slice(0, -1);
  for (const seg of target.split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
      if (!dir.length) return null; // climbs above the root
      dir.pop();
    } else dir.push(seg);
  }
  return dir.join("/");
}

/* A symbolic link: shown, never followed. It has no content, so this is the
   whole view — the target string, and a click-through when the target lands
   on a file inside the project. A link pointing outside is shown as inert
   text, because BearDrive never resolves it. */
function SymlinkView(props: {
  path: string;
  target: string;
  flatFiles: Node[];
  onOpenFile: (path: string) => void;
  onRendered?: () => void;
}) {
  const { path, target, flatFiles, onOpenFile, onRendered } = props;
  useEffect(() => onRendered?.(), [onRendered]);
  const resolved = resolveLink(path, target);
  const inProject = resolved !== null && flatFiles.some((f) => f.path === resolved);
  return (
    <div className="filecard">
      <div className="sb-icon" style={{ justifyContent: "center", color: "var(--text-dim)", marginBottom: 6 }}>
        <Icon name="link" />
      </div>
      <div className="name">{path.split("/").pop()}</div>
      <p>
        This is a symbolic link. BearDrive shows the link and never follows it.
      </p>
      <p>
        Target:{" "}
        {inProject ? (
          <a
            href="#"
            onClick={(e) => {
              e.preventDefault();
              onOpenFile(resolved!);
            }}
          >
            <code>{target}</code>
          </a>
        ) : (
          <code>{target}</code>
        )}
        {!inProject && (
          <>
            <br />
            <span style={{ color: "var(--text-dim)" }}>
              Points outside this project — no content is served here.
            </span>
          </>
        )}
      </p>
    </div>
  );
}

/* The editing surface. Loads the file's current bytes once and hands them to
   CodeMirror; from then on the buffer is the truth until a save lands.

   It still does NOT re-seed itself from the change stream — that would reset
   the document under your cursor. An outside write is offered to the open
   document as a SPLICE instead (sharedfile.merge), which leaves every
   character it does not change, and the cursor sitting in one, alone. The
   banner is what is left: the cases merge refuses, because folding the write
   in would have overwritten unsaved edits or landed twice. */
function EditView(props: Parameters<typeof FileView>[0]) {
  const { apiBase, path, onMeta } = props;
  // An HTML file edits as the page it renders as, unless the reader asked for
  // its markup instead. Everything else has only ever had one surface.
  const visual = HTML_EXT.test(path) && !props.editSource;
  // Live path, never immutable: the buffer is seeded from whatever the file
  // says right now. Shares the ["text", url] family the rest of the app uses,
  // so a restore or a peer write invalidates it like any other read.
  const fileURL = fileURLFor(apiBase, path);
  // Only the visual editor frames the page; a source editor minting a frame
  // URL it never shows was a wasted request on every Markdown and code edit.
  const frameURL = useFrameURL(apiBase, path, fileURL, undefined, visual).url;
  /* The buffer opens on the cached copy when there is one — a read view, a
     hover, the boot prefetch, Done's own save — so the editor is on screen in
     the first frame. That copy may be older than a peer's write, so it is
     always read again on the way in ("always", not the stale check), and
     whatever that read brings is folded into the open document as a splice
     (Editor's initial/baseSha → sharedfile.merge), never a re-seed: the
     cursor stays where it is. Only a cold load waits, behind the skeleton. */
  const qcEarly = useQueryClient();
  /* "always" can still join a read that was already out — a hover prefetch
     sent before a peer's write — and then no read is made after the click at
     all. So an open that found a copy, or a pre-edit read still in flight,
     starts a read of its own: the editor is up on the cached copy (or the
     skeleton holds), `data` stays defined throughout, and the answer is
     folded in like any other. Decided BEFORE useQuery subscribes, which would
     otherwise make every open look like it had a read in flight. A cold
     /edit/ link keeps its boot read, the one request it needs. */
  const ownRead = useMemo(() => {
    const s = qcEarly.getQueryState(textQueryOptions(fileURL).queryKey);
    return s?.data !== undefined || (s?.fetchStatus === "fetching" && readsBeforeEdit.has(fileURL));
  }, [qcEarly, fileURL]);
  const text = useQuery({ ...textQueryOptions(fileURL), refetchOnMount: "always" });
  const { error, data, refetch } = text;
  useEffect(() => {
    readsBeforeEdit.delete(fileURL);
    if (!ownRead) return;
    // Cancelled first: refetch's own cancelRefetch only replaces a read when
    // the query already has data, so a cold pre-click read still in flight
    // would otherwise be joined, not replaced. With data, cancelling keeps
    // it and only aborts the read.
    void qcEarly
      .cancelQueries({ queryKey: textQueryOptions(fileURL).queryKey })
      .then(() => refetch({ cancelRefetch: true }));
  }, [ownRead, refetch, fileURL, qcEarly]);
  /* Except a copy already known to be behind: the change stream (or a
     restore) invalidated it because a newer write landed after it was read.
     Opening on it would paint text the read in flight is about to replace, so
     that read is waited for instead. Only before the editor is up — once it
     holds a buffer, a later invalidation is a splice, never an unmount. */
  const textState = qcEarly.getQueryState(textQueryOptions(fileURL).queryKey);
  const seededFor = useRef<string | null>(null);
  const behind = seededFor.current !== fileURL && !!textState?.isInvalidated && text.isFetching;
  // Only the surface this file edits with: a Markdown edit has no use for
  // TipTap, nor an HTML page's visual edit for CodeMirror.
  const editorMod = useModule(editorModule, visual ? notNeeded : loadEditor);
  const visualMod = useModule(visualModule, visual ? loadVisualEdit : notNeeded);
  const Editor = editorMod.mod?.Editor;
  const VisualEdit = visualMod.mod?.VisualEdit;
  const loadFailed = visual ? visualMod : editorMod;
  const [state, setState] = useState<SaveState>("clean");
  const [collab, setCollab] = useState<CollabStatus>("connecting");
  const md = MD_EXT.test(path);
  const meeting = isMeetingNote(path);
  // Write and Markdown are one CodeMirror reconfigured; Preview hides it
  // without unmounting, so the room, the cursor and the undo history all
  // survive a switch. The /edit/ URL stays the invitation either way.
  const [mode, setMode] = useState<EditMode | "preview">(md ? "write" : "markdown");
  const [info, setInfo] = useState<EditorInfo | null>(null);
  const [pending, setPending] = useState(0);
  const [savedAt, setSavedAt] = useState<number | null>(null);
  const [, setTick] = useState(0);
  // Who is in the room, and which sections a remote caret is in.
  const [editors, setEditors] = useState<string[]>([]);
  const [typing, setTyping] = useState<Set<string>>(new Set());
  useEffect(() => {
    const t = setInterval(() => setTick((n) => n + 1), 5_000);
    return () => clearInterval(t);
  }, []);
  // Typed into during this visit: Done then fetches the render it opens on.
  const edited = useRef(false);
  useEffect(() => {
    if (state === "clean") setSavedAt(Date.now());
    else edited.current = true;
  }, [state]);
  const roomDoc = info?.collab ?? null;
  const textRef = useRef("");
  textRef.current = info?.text ?? "";
  useEffect(() => {
    if (!roomDoc) return;
    const read = () => {
      const names: string[] = [];
      roomDoc.awareness.getStates().forEach((st) => {
        const n = (st as { user?: { name?: string } }).user?.name;
        if (n) names.push(n);
      });
      setEditors([...new Set(names)]);
      const secs = sections(textRef.current);
      const on = new Set<string>();
      for (const c of roomDoc.remoteCursors()) {
        const sec = sectionAt(secs, c.pos);
        if (sec) on.add(sec.name);
      }
      setTyping(on);
    };
    read();
    roomDoc.awareness.on("change", read);
    return () => roomDoc.awareness.off("change", read);
  }, [roomDoc]);
  // A peer's write that arrived while this buffer was open. Advisory only —
  // and only interesting when it did NOT come through the shared document:
  // a co-editor's keystrokes are already in the buffer.
  const [peerWrote, setPeerWrote] = useState(false);
  /* Writes of MY OWN that the change stream has not accounted for yet.

     A count, not a flag. Each save is announced before it goes out and claimed
     by the event it produces; a flag loses that pairing the moment two saves
     are in flight — which the visual editor does routinely, since clicking
     from one paragraph to the next saves each of them. The second event then
     had nothing to claim it and raised the banner on the user's own edit. */
  const mine = useRef(0);
  const qc = useQueryClient();
  const fileHandle = useRef<SharedFile | null>(null);
  /* The visual editor reads its seed once, so a fresher read of the file
     than the copy it opened on is folded in here instead — the same splice
     the source editor makes from its own props. */
  const visualSeed = useRef<{ url: string; text: string; sha?: string } | null>(null);
  useEffect(() => {
    if (!visual || data?.kind !== "text") return;
    const prev = visualSeed.current;
    visualSeed.current = { url: fileURL, text: data.text, sha: data.sha };
    if (!prev || prev.url !== fileURL || (prev.text === data.text && prev.sha === data.sha)) return;
    const f = fileHandle.current;
    if (f && f.merge(data.text, data.sha) === "blocked") setPeerWrote(true);
  }, [visual, data, fileURL]);
  /* A restore from the rail, run and reconciled here rather than left to the
     change stream: a missed frame would leave the open document on the text
     from before it, and the next save would write that back over the
     restore. A live room already holds the restored text — the hub folded it
     in — so only the version is recorded; anything else takes it as an
     outside write.

     The whole request is fenced. No read of the file may reach the editor's
     own fold (its initial/baseSha props) until the restored head has been
     merged with `held`: one would splice into the room a text the room's
     restore frame then brings again. Two reads could: one already in flight
     before the restore (it would land after, with the old bytes), and the
     change stream's refetch, published before POST /restore even answers.
     So reads are cancelled before and after the request, the editor's props
     are frozen on what it already holds for the duration, and the cache is
     given exactly the head just merged — which the fold then recognises as
     nothing new when the props thaw. A failed restore just thaws them. */
  const restoring = useRef(0);
  const [frozen, setFrozen] = useState<{ text: string; sha?: string } | null>(null);
  /* The document this view has open right now, for a restore's follow-up
     read to tell it is still the one it restored. */
  const openURL = useRef(fileURL);
  openURL.current = fileURL;
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  // Each restore's number, and the newest one whose head is merged: an older
  // restore still retrying its read stops once a newer one has landed.
  const restoreSeq = useRef(0);
  const restoreMerged = useRef(0);
  /* A committed restore whose head could not be read. The props stay frozen
     and the next read of the file to land is merged here, with `held`, before
     they thaw — so that read never reaches the editor's fold without it. */
  const pendingHeld = useRef<{ url: string; folded: boolean; blob?: string; at: number } | null>(null);
  const { dataUpdatedAt } = text;
  // Mirrors pendingHeld for rendering: the banner and the retry listeners.
  const [hasPendingRestore, setHasPendingRestore] = useState(false);
  const setPendingRestore = (p: typeof pendingHeld.current) => {
    pendingHeld.current = p;
    setHasPendingRestore(p !== null);
  };
  /* The text query's own retries end; a pending restore must not end with
     them, or the props stay frozen until something else happens to read the
     file. So it reads again when the network comes back or the tab regains
     focus, and the banner offers it by hand. */
  useEffect(() => {
    if (!hasPendingRestore) return;
    const again = () => void refetch();
    window.addEventListener("online", again);
    window.addEventListener("focus", again);
    return () => {
      window.removeEventListener("online", again);
      window.removeEventListener("focus", again);
    };
  }, [hasPendingRestore, refetch]);
  useEffect(() => {
    const p = pendingHeld.current;
    if (!p) return;
    if (p.url !== fileURL) {
      // Another document now: the restore's is gone with its editor.
      setPendingRestore(null);
      if (restoring.current === 0) setFrozen(null);
      return;
    }
    if (data?.kind !== "text" || dataUpdatedAt <= p.at) return;
    setPendingRestore(null);
    const f = fileHandle.current;
    // Held only if this read IS the restored version: a write that landed
    // after the restore is an outside write, which the room does not hold.
    const held = p.folded && !!p.blob && data.sha === p.blob;
    if (f) setPeerWrote(f.merge(data.text, data.sha, { held }) === "blocked");
    if (restoring.current === 0) setFrozen(null);
  }, [data, dataUpdatedAt, fileURL]);
  /* Throws only when the restore itself failed. `reconciled: false` means it
     WAS committed but the open document could not be brought to it yet —
     the next read of the file does that (pendingHeld). */
  const onRestore = async (
    post: () => Promise<RestoreAnswer>,
  ): Promise<{ reconciled: boolean }> => {
    const textKey = textQueryOptions(fileURL).queryKey;
    const mine = ++restoreSeq.current;
    if (restoring.current++ === 0 && !pendingHeld.current) {
      const held = qc.getQueryData(textKey);
      setFrozen(held?.kind === "text" ? { text: held.text, sha: held.sha } : null);
    }
    try {
      await qc.cancelQueries({ queryKey: textKey });
      // A refusal thaws at once (finally): nothing changed, so the props may
      // flow again.
      const out = await post();
      const folded = !!out?.folded;
      /* From here the file HAS changed, and the props stay frozen until the
         restored head is merged: thawing on a failed read would let the next
         refetch reach the editor's fold without `held`. `held` itself only
         when the head read IS the restored blob — anything newer is an
         outside write the room does not hold. A transient failure is
         retried, backing off (1, 2, 4, 8 s); a refusal (403, 404) is an
         answer and is not. Giving up hands the merge to the next read that
         lands (pendingHeld). The loop also ends when this document is gone,
         or a newer restore has already merged its own head. */
      let reconciled = true;
      for (let attempt = 0; ; attempt++) {
        if (!mounted.current || openURL.current !== fileURL || restoreMerged.current > mine) break;
        await qc.cancelQueries({ queryKey: textKey });
        try {
          const head = await fetchBlobText(fileURL);
          if (!mounted.current || openURL.current !== fileURL || restoreMerged.current > mine) break;
          const f = fileHandle.current;
          if (head.kind === "text" && f) {
            const held = folded && !!out?.blob && head.sha === out.blob;
            setPeerWrote(f.merge(head.text, head.sha, { held }) === "blocked");
          }
          qc.setQueryData(textKey, head);
          restoreMerged.current = Math.max(restoreMerged.current, mine);
          setPendingRestore(null);
          break;
        } catch (err) {
          if (attempt < 4 && isTransientBlobError(err)) {
            await new Promise((r) => setTimeout(r, 1_000 * 2 ** attempt));
            continue;
          }
          if (mounted.current && openURL.current === fileURL && restoreMerged.current <= mine) {
            setPendingRestore({ url: fileURL, folded, blob: out?.blob, at: Date.now() });
            void qc.invalidateQueries({ queryKey: textKey });
          }
          reconciled = false;
          break;
        }
      }
      void qc.invalidateQueries({ queryKey: ["render", apiBase, path] });
      return { reconciled };
    } finally {
      // Still frozen while a pending merge waits for its read.
      if (--restoring.current === 0 && !pendingHeld.current) setFrozen(null);
    }
  };
  const { flushRef } = props;
  useEffect(() => {
    if (!flushRef) return;
    /* Save and leave, only once everything typed is saved. A question the
       user must answer (a shrink, however late the hub reports it) keeps
       them here, and so does a save that failed: leaving would destroy the
       only copy of the buffer and the retry that is still trying to land it.
       Clean or a bystander settles at once.

       And the page Done opens is ready before it opens. The saved bytes go
       into the ["text", url] query the reading views read, so a text, code or
       CSV page paints them in its first frame; a Markdown page's render is
       fetched now, while the editor is still on screen (it carries this
       save's receipt, so whichever hub process answers has folded it), so the
       read view opens on the new HTML instead of a skeleton or the pre-edit
       page. A render that fails does not hold anyone here: the read view
       retries it as it always has. */
    const flush = async () => {
      let f: SharedFile;
      for (;;) {
        const cur = fileHandle.current;
        if (!cur) return true;
        let saved = false;
        try {
          saved = await cur.saveNow();
        } catch {
          return false;
        }
        if (!saved || cur.askingShrink()) return false;
        if (fileHandle.current && fileHandle.current !== cur) continue;
        f = cur;
        break;
      }
      const now = f.settled();
      const textKey = textQueryOptions(fileURL).queryKey;
      const before = qc.getQueryData(textKey);
      const changed = edited.current || !(before?.kind === "text" && before.text === now.text);
      const seeded: BlobText = now.sha ? { kind: "text", text: now.text, sha: now.sha } : { kind: "text", text: now.text };
      qc.setQueryData(textKey, seeded);
      if (MD_EXT.test(path)) {
        const render = renderQuery(apiBase, path);
        const held = qc.getQueryState(render.queryKey);
        if (changed || !held?.data || held.isInvalidated) {
          try {
            await qc.fetchQuery({ ...render, staleTime: 0, retry: false });
          } catch {
            // MarkdownView retries it.
          }
        }
      }
      // Confirmed in the background; the cache already shows the answer.
      void qc.invalidateQueries({ queryKey: ["text", fileURL], refetchType: "none" });
      return true;
    };
    flushRef.current = flush;
    return () => {
      if (flushRef.current === flush) flushRef.current = null;
    };
  }, [flushRef, qc, apiBase, path, fileURL]);
  // Co-editors in this document right now. A write while somebody else is in
  // the room is their snapshot of the document I already have — warning about
  // it would fire on every co-editing save. Alone, the same event means
  // somebody edited outside the room (a CLI, another device), which is the
  // case the banner exists for.
  const peers = useRef(0);

  /* A concurrent edit that could not be merged. Nothing was dropped: this
     account's version was written beside the file under the name the sync
     path has always used, so the reader meets it as an ordinary file with an
     alarming name and ConflictBanner explains it there. */
  const onConflictCopy = (copy: string) => {
    toast(
      "Someone else saved this file first. Your version is beside it as " +
        (copy.split("/").pop() ?? copy),
    );
  };

  useEffect(() => {
    const onPeer = (e: Event) => {
      const p = (e as CustomEvent<string[]>).detail;
      if (!p?.includes(path)) return;
      // The source editor decides from the file's own bytes once the refetch
      // this same event triggers arrives (Editor's onExternal). That verdict
      // knows whether the change could be merged; this event cannot, and
      // raising a banner ahead of it only flashes the wrong answer.
      if (!visual) return;
      // Our own save comes back through the stream; that is not a peer.
      if (mine.current > 0) {
        mine.current--;
        return;
      }
      if (peers.current > 0) return; // a co-editor's snapshot, already in my buffer
      setPeerWrote(true);
    };
    window.addEventListener("bdrive:changed", onPeer);
    return () => window.removeEventListener("bdrive:changed", onPeer);
  }, [path, visual]);

  const offline = collab === "offline";
  const label =
    offline && pending > 0
      ? `Offline · ${pending} ${pending === 1 ? "edit" : "edits"} pending`
      : offline
        ? "Offline · reconnecting"
        : state === "saving"
          ? "Saving…"
          : state === "dirty"
            ? collab === "live"
              ? "Live · unsaved"
              : "Unsaved"
            : state === "error"
              ? "Not saved — still trying"
              : (collab === "live" ? "Live · " : "") + "saved" + (savedAt ? " " + agoShort(savedAt) : "");
  useEffect(() => {
    onMeta(
      <>
        <span
          id="editor-state"
          className="ed-state"
          data-state={state}
          data-collab={collab}
          data-pending={pending}
        >
          <i aria-hidden="true" />
          {label}
        </span>
        {editors.length > 0 && (
          <span className="ed-editors" aria-label="People editing this file">
            {editors.slice(0, 5).map((n) => (
              <Avatar key={n} name={n} title={n + " is editing"} />
            ))}
            {editors.length > 5 && <span className="ed-more">+{editors.length - 5}</span>}
          </span>
        )}
      </>,
    );
    // collab belongs in the deps: without it the status line keeps whatever
    // connection state it was first rendered with, so a room that came up
    // fine still reads "connecting" forever.
  }, [state, collab, label, pending, editors, onMeta]);
  // Leaving edit mode keeps FileView mounted on the same path, so the editor
  // clears its own status rather than leaving it on a read-only page.
  useEffect(() => () => onMeta(""), [onMeta]);

  /* A read that failed is only fatal before there is anything to lose.

     This used to be `if (error)`, which unmounted a LIVE editor the moment a
     refetch failed — taking the buffer, its idle save timer and the co-editing
     stream with it, and leaving "Could not open … for editing" over work that
     was still perfectly intact. One 429 from the hub's rate limiter was enough,
     and the only way back was a reload. React Query keeps the last good `data`
     across a failed refetch, so the editor keeps running and the failure is a
     banner (BEA network-efficiency stage 5). */
  if (error && !data)
    return <div className="empty">Could not open {path} for editing.</div>;
  // Nothing cached (a cold load), or a cached copy known to be behind: the
  // only cases that wait for the read.
  if (!data || behind) return <FileSkeleton />;
  if (data.kind !== "text") {
    return (
      <div className="empty">
        This file is not text, so it cannot be edited here.
      </div>
    );
  }
  seededFor.current = fileURL;

  return (
    <>
      {(error || hasPendingRestore) && (
        <div id="read-stale" className="banner">
          {hasPendingRestore
            ? "The restore is saved, but this editor could not read it back yet — your work is untouched and still saving. "
            : "Could not check this file for changes just now — your work is untouched and still saving. Retrying. "}
          <button type="button" className="btn-link" onClick={() => void refetch()}>
            Retry now
          </button>
        </div>
      )}
      {peerWrote && (
        <div id="peer-wrote" className="banner">
          Someone else changed this file while you were editing. Your buffer is
          unchanged — saving keeps your version and theirs stays in history.
        </div>
      )}
      {offline && (
        <div id="ed-offline" className="banner ed-offline" role="status">
          <b>Reconnecting…</b>{" "}
          {pending > 0
            ? `Your ${pending} ${pending === 1 ? "edit is" : "edits are"} kept in this tab and will merge when you're back.`
            : "Edits you make are kept in this tab and merge when you're back."}
          {meeting && " Only your own section can be edited until then."}
        </div>
      )}
      {!visual && md && (
        <div className="ed-modes" role="tablist" aria-label="Editor mode">
          {(["write", "markdown", "preview"] as const).map((m) => (
            <button
              key={m}
              type="button"
              role="tab"
              id={"mode-" + m}
              aria-selected={mode === m}
              className={mode === m ? "on" : ""}
              onClick={() => setMode(m)}
            >
              <Icon name={m === "write" ? "pencil" : m === "markdown" ? "code" : "eye"} />
              {m === "write" ? "Write" : m === "markdown" ? "Markdown" : "Preview"}
            </button>
          ))}
        </div>
      )}
      {md && !visual && (
        <EditRail
          apiBase={apiBase}
          path={path}
          text={info?.text ?? data.text}
          meeting={meeting}
          editors={editors}
          me={props.me?.name ?? ""}
          typing={typing}
          versions={!!props.projectId}
          onRestore={onRestore}
        />
      )}
      {loadFailed.failed ? (
        <div className="empty">
          Could not load the editor.{" "}
          <Button variant="toolbar" onClick={loadFailed.retry}>
            Try again
          </Button>
        </div>
      ) : visual ? (
        !VisualEdit || !frameURL ? (
          <FileSkeleton />
        ) : (
        /* HTML gets the rendered page, clicked directly. Same file, same
           shared document, same save — a different surface onto it. Source
           editing stays one click away for the structural work this cannot
           express (Browser.tsx's "Edit source"). */
        <VisualEdit
          apiBase={apiBase}
          fileURL={frameURL}
          path={path}
          initial={data.text}
          baseSha={data.sha}
          onConflictCopy={onConflictCopy}
          onWriting={() => {
            mine.current++;
          }}
          onStateChange={setState}
          onCollab={setCollab}
          onPeers={(n) => {
            peers.current = n;
          }}
          me={props.me}
          onRendered={props.onRendered}
          handle={fileHandle}
        />
        )
      ) : !Editor ? (
        <FileSkeleton />
      ) : (
        <div className="ed-surface" hidden={mode === "preview"}>
        <Editor
          apiBase={apiBase}
          mode={mode === "preview" ? "write" : mode}
          meeting={meeting}
          files={props.flatFiles.map((f) => f.path)}
          onInfo={setInfo}
          onPending={setPending}
          handle={fileHandle}
          path={path}
          initial={frozen ? frozen.text : data.text}
          baseSha={frozen ? frozen.sha : data.sha}
          onConflictCopy={onConflictCopy}
          /* Someone wrote the file while it was open here. Merged, it is
             already on screen and only worth a word; refused, it is the
             banner — and either way this is the only thing that raises or
             clears it for this surface. */
          onExternal={(r) => {
            setPeerWrote(r === "blocked");
            if (r === "merged") toast("Folded in a change from outside this editor.");
          }}
          onWriting={() => {
            mine.current++;
          }}
          onStateChange={setState}
          onCollab={setCollab}
          onPeers={(n) => {
            peers.current = n;
          }}
          me={props.me}
        />
        </div>
      )}
      {mode === "preview" && !visual && <EditPreview {...props} />}
      {!visual && (
        <div className="ed-statusbar" id="ed-statusbar">
          <span>
            {md ? "Markdown" : "Text"} · {humanSize(byteLength(info?.text ?? data.text))} ·{" "}
            {wordCount(info?.text ?? data.text).toLocaleString()} words
          </span>
          <span className="ed-guard" title="A save that would empty the file or cut most of it asks first">
            <Icon name="shield" /> Empty-save protection on
          </span>
          {info && (
            <span className="ed-pos">
              Ln {info.line}, Col {info.col}
            </span>
          )}
        </div>
      )}
    </>
  );
}

/* The markdown page's content query. The file page prefetches it while the
   tree is still loading, so the two requests run side by side. */
export function renderQuery(apiBase: string, path: string, version?: string) {
  return {
    queryKey: ["render", apiBase, path, version || ""],
    queryFn: () =>
      getJSON<RenderDoc>(
        apiBase + "render?path=" + encodeURIComponent(path) + (version ? "&sha=" + version : ""),
      ),
    // A blob that isn't there will not appear on a retry, and the retry's
    // delay is a blank pane the reader has no explanation for.
    retry: version ? false : undefined,
  };
}

/* The note as it reads, from the hub's renderer. It shows the file as last
   written — the room snapshots within a second of a pause — while the editor
   stays mounted, hidden, so going back to Write is the same session. */
function EditPreview(props: Parameters<typeof FileView>[0]) {
  const { apiBase, path, flatFiles, projectId, onOpenFile } = props;
  const { data: doc } = useQuery(renderQuery(apiBase, path));
  const html = useMemo(
    () => (doc ? transformHTML(doc.html, path, apiBase, flatFiles, projectId) : ""),
    [doc, path, apiBase, flatFiles, projectId],
  );
  if (!doc) return <div className="empty">Loading preview…</div>;
  return (
    <div
      id="ed-preview"
      className="fv-prose"
      dangerouslySetInnerHTML={{ __html: html }}
      onClick={(e) => handleLinkClick(e, path, onOpenFile)}
    />
  );
}

function MarkdownView(props: Parameters<typeof FileView>[0]) {
  const wideRail = useWideRail();
  const {
    apiBase,
    path,
    version,
    heatMap,
    flatFiles,
    projectId,
    onOpenFile,
    onRendered,
  } = props;
  const { data: doc, error } = useQuery(renderQuery(apiBase, path, version));
  // Once the page is up, warm its source in the background: pressing Edit
  // then mounts the editor on bytes already in the cache, in one frame.
  const qc = useQueryClient();
  const docLoaded = !!doc;
  useEffect(() => {
    if (docLoaded && !version) prefetchEditText(qc, apiBase, path);
  }, [docLoaded, version, qc, apiBase, path]);

  // Rewrite the HTML BEFORE rendering (relative image sources, external
  // link targets) rather than patching the live DOM afterwards: React owns
  // the dangerouslySetInnerHTML subtree and may re-apply the markup on any
  // update, silently discarding post-commit DOM patches. Link navigation
  // is delegated on the container for the same reason.
  const html = useMemo(
    () =>
      doc ? transformHTML(doc.html, path, apiBase, flatFiles, projectId) : "",
    [doc, path, apiBase, flatFiles, projectId],
  );

  // Diagrams are rendered into a NEW html string and fed back through state,
  // so the one dangerouslySetInnerHTML below re-mounts with the SVG already
  // in it — the same reason transformHTML runs before the mount and not after
  // it. Runs on transformHTML's output, so a diagram's SVG never goes through
  // the img/link rewriting pass. The palette follows the current theme, and a
  // theme switch re-renders: the SVG bakes its colours in, so CSS alone
  // cannot recolour it.
  const { theme } = useTheme();
  const [diagrams, setDiagrams] = useState<string | null>(null);
  const diagramsFor = useRef<string | null>(null);
  useEffect(() => {
    // A new document drops the old diagrams at once; a theme switch keeps
    // them on screen until the recoloured ones are ready, rather than
    // flashing the raw fences in between.
    if (diagramsFor.current !== html) setDiagrams(null);
    diagramsFor.current = html;
    if (!hasMermaid(html)) return; // no fence: mermaid is never downloaded
    let cancelled = false;
    renderMermaid(html, theme === "light" ? LIGHT : DARK).then((out) => {
      // A slow render of the file we just left must not paint over this one.
      if (!cancelled) setDiagrams(out);
    });
    return () => {
      cancelled = true;
    };
  }, [html, theme]);

  // Rendered content, and nothing else, counts as "rendered" — the scroll
  // restorer re-applies its goal on this call, and a read-count refresh
  // (heatMap, polled every 60s) used to ride along on the meta effect above
  // and yank the reader back to the top mid-read (BEA-155). html and diagrams
  // are exactly what the mounted subtree is made of; a diagram landing late
  // now reports itself too, which is the case the retries were written for.
  useEffect(() => {
    if (html) onRendered?.();
  }, [html, diagrams, onRendered]);

  if (error) return <LoadError version={version} err={error as Error} />;
  if (!doc) return <FileSkeleton />;
  // Read counts belong to the path, not to one version — showing them beside
  // content the banner just called historical reads as if they counted views
  // of these bytes.
  const heat = version ? null : (heatMap && heatMap[doc.path]) || null;
  // The rail is a SIBLING of the prose, not a wrapper around it: CSS hangs it
  // in the right margin of .page.read (style.css, .fv-rail) without a third
  // column in AppShell. Wide, it follows the document so assistive heading
  // navigation meets the page title first; narrow, its disclosure sits above. Server-rendered, server-sanitized markdown — same
  // trust model as the classic app assigning innerHTML.
  const rail = (
    <FileRail
      html={html}
      frontmatter={doc.frontmatter}
      heat={heat}
      insightsHref={projectId ? urlForView("dashboard", projectId, path) : undefined}
    />
  );
  return (
    <>
      <SecretBadge findings={doc.findings} />
      {!wideRail && rail}
      <DocHeader doc={doc} heat={heat} />
      <div
        className="fv-prose"
        dangerouslySetInnerHTML={{ __html: diagrams ?? html }}
        onClick={(e) => handleLinkClick(e, path, onOpenFile)}
      />
      {wideRail && rail}
    </>
  );
}

/* Holds the file page's shape while its content loads: title, meta row, six
   lines of prose, and the rail beside them. */
export function FileSkeleton() {
  return (
    <div className="fskel" aria-busy="true" aria-label="Loading file" data-bd-ignore="">
      <div className="fskel-main">
        <span className="fskel-title" />
        <span className="fskel-meta">
          <span style={{ width: 120 }} />
          <span style={{ width: 90 }} />
          <span style={{ width: 150 }} />
        </span>
        {[96, 88, 92, 70, 84, 52].map((w, i) => (
          <span className="fskel-line" key={i} style={{ width: w + "%" }} />
        ))}
      </div>
      <div className="fskel-rail" aria-hidden="true">
        <span style={{ width: "40%" }} />
        <span style={{ width: "80%" }} />
        <span style={{ width: "65%" }} />
        <span style={{ width: "72%" }} />
      </div>
    </div>
  );
}

/* The share gate could already name the rule and the line well enough to
   refuse to publish this file, while the file view rendered the same key as
   ordinary prose (BEA-147). Advisory only, in VersionBanner's shape: a strip
   above the content, role="status", no actions. Nothing is blocked and
   nothing is redacted — a reader who can open the file could already read
   the key, and the point is that they now know it is in there. */
function SecretBadge({ findings }: { findings?: SecretFinding[] }) {
  if (!findings?.length) return null;
  return (
    <div className="sbadge" role="status">
      <span className="sb-icon">
        <Icon name="shield" />
      </span>
      <div className="sb-text">
        <b>{secretsBadge(findings)}</b>
        <span>
          Checked when this page loaded. Sharing the file asks you to confirm
          first.
        </span>
      </div>
    </div>
  );
}

/* Delegated click handling for rendered-markdown links: wikilinks (already
   carrying a real in-app href, see transformHTML) and relative links route
   in-app on a plain click, everything else keeps its native behavior. */
export function handleLinkClick(
  e: React.MouseEvent,
  p: string,
  openFile: (path: string) => void,
) {
  const a = (e.target as HTMLElement).closest("a");
  if (!a || !(e.currentTarget as HTMLElement).contains(a)) return;
  // Same rule as nav.ts:linkProps — a modified or non-primary click belongs
  // to the browser (new tab, new window, download), which is the entire
  // point of these anchors carrying real URLs.
  if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0)
    return;
  const href = a.getAttribute("href") || "";
  const dir = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
  // data-wiki, not "any root-absolute href": an author's own [x](/somewhere)
  // keeps its native behavior instead of quietly becoming an SPA route, and
  // the target path is read back directly instead of re-parsed out of a URL.
  const wiki = a.getAttribute("data-wiki");
  if (wiki !== null) {
    e.preventDefault();
    openFile(wiki);
  } else if (!/^([a-z]+:|\/|#)/i.test(href)) {
    e.preventDefault();
    openFile(joinPath(dir, decodeURIComponent(href)));
  }
}

/* String-level rewrite of the server's HTML: relative image sources point
   at the file API, external links open in a new tab, and wikilinks get the
   real in-app URL of their target. */
export function transformHTML(
  html: string,
  p: string,
  apiBase: string,
  files: Node[],
  projectId?: string,
): string {
  const dir = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
  const fileURL = (path: string) =>
    apiBase + "file?path=" + encodeURIComponent(path);
  const parsed = new DOMParser().parseFromString(html, "text/html");
  for (const img of parsed.querySelectorAll("img")) {
    const src = img.getAttribute("src") || "";
    // An inline SVG is a document, not a picture. goldmark's IsDangerousURL
    // admits any `data:image/…` URL for an image, and svg+xml is the one
    // member of that family a browser parses as markup — the same property
    // the server's sandboxInline walls off. Raster data: URLs stay: they are
    // inert and people do paste them into markdown.
    if (/^\s*data:image\/svg/i.test(src)) img.removeAttribute("src");
    else if (!/^([a-z]+:|\/)/i.test(src))
      img.setAttribute("src", fileURL(joinPath(dir, src)));
  }
  for (const a of parsed.querySelectorAll("a")) {
    const href = a.getAttribute("href") || "";
    // The server has no file tree, so it leaves [[target]] as the marker
    // href="wiki:<target>" (markdown.go). Resolving it HERE — before the
    // mount — is what makes the anchor an ordinary link: copy-link,
    // middle-click and open-in-new-tab all read the attribute, and only a
    // plain click ever reaches the handler above.
    if (href.startsWith("wiki:")) {
      const hit = resolveWiki(decodeURIComponent(href.slice(5)), files);
      if (hit) {
        a.setAttribute("href", urlForPath(hit.path, projectId));
        a.setAttribute("data-wiki", hit.path);
      } else {
        // No file matches: an unusable "wiki:" string must not survive the
        // mount, so the anchor loses its href and says why on hover.
        a.removeAttribute("href");
        a.classList.add("wiki-missing");
        a.setAttribute("title", "No file matches this wikilink");
      }
      continue;
    }
    // goldmark's data: allowance exists for IMAGES and is applied to <a> as
    // well, so a rendered document can mount a link whose target is an
    // attacker-authored document. Browsers refuse a top-level data:
    // navigation today, which is the only reason this is defence in depth
    // rather than a live exploit — and it is not a defence this app should be
    // borrowing. A markdown link is never legitimately a data: URL.
    if (/^\s*data:/i.test(href)) a.removeAttribute("href");
    else if (/^https?:/i.test(href)) {
      a.setAttribute("target", "_blank");
      a.setAttribute("rel", "noopener");
    }
  }
  return parsed.body.innerHTML;
}

function ImgView(props: {
  src: string;
  alt: string;
  version?: string;
  onRendered?: () => void;
}) {
  const [failed, setFailed] = useState(false);
  if (failed)
    return (
      <LoadError
        version={props.version}
        err={new Error("could not be loaded")}
      />
    );
  return (
    <img
      src={props.src}
      alt={props.alt}
      onLoad={props.onRendered}
      onError={() => setFailed(true)}
    />
  );
}

/* A missing current file is a server problem worth quoting; a missing
   version is almost always a bad ?v= in a hand-edited or stale URL, which
   the server's "no such version" wording does not explain. */
function LoadError({ version, err }: { version?: string; err: Error }) {
  return (
    <div className="empty">
      {version
        ? "That version isn't available."
        : "Could not load file: " + err.message}
    </div>
  );
}

function TextView(
  props: Parameters<typeof FileView>[0] & { fileURL: string; delim?: string },
) {
  const { apiBase, path, version, fileURL, delim, onRendered } = props;
  // The same BlobText query the editor, Copy and SniffView read: one shape
  // under ["text", url], whoever filled it.
  const { data, error } = useQuery(textQueryOptions(fileURL, !!version));
  useEffect(() => {
    if (data) onRendered?.();
  }, [data, onRendered]);
  const text = data?.kind === "text" ? data.text : null;
  // null = not usefully delimited (or no delimiter asked for): fall through
  // to the plain-text view below.
  const csv = useMemo(
    () => (delim && text != null ? parseDelimited(text, delim, CSV_ROWS) : null),
    [text, delim],
  );
  if (error) return <LoadError version={version} err={error as Error} />;
  if (!data) return <FileSkeleton />;
  if (data.kind !== "text") {
    return (
      <FileCard apiBase={apiBase} path={path} version={version} fileURL={fileURL}>
        {data.kind === "too-large"
          ? `Too large to preview (${humanSize(data.size)}).`
          : "No preview for this file type."}
      </FileCard>
    );
  }
  if (csv) return <CsvTable csv={csv} key={path} />;
  return (
    <pre className="plain" key={path}>
      {data.text}
    </pre>
  );
}

/* Plain <table> — no sorting, filtering or search, so @tanstack/react-table
   would only be weight. Every row is padded to the widest one so a ragged
   row renders empty trailing cells instead of shifting its neighbours. */
function CsvTable({ csv }: { csv: Csv }) {
  const [head, ...body] = csv.rows;
  const cols = csv.rows.reduce((m, r) => Math.max(m, r.length), 0);
  const idx = Array.from({ length: cols }, (_, i) => i);
  return (
    <>
      <div className="csvbox">
        <table className="csvview">
          <thead>
            <tr>
              {idx.map((i) => (
                <th key={i}>{head[i] ?? ""}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {body.map((r, i) => (
              <tr key={i}>
                {idx.map((j) => (
                  <td key={j}>{r[j] ?? ""}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {csv.truncated > 0 && (
        <p className="csvnote">
          showing {csv.rows.length.toLocaleString()} of{" "}
          {(csv.rows.length + csv.truncated).toLocaleString()} rows — Download
          for the rest
        </p>
      )}
    </>
  );
}
