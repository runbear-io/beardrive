import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import { diffBase } from "../lib/activity";
import { diffText } from "../lib/diff";
import { blobURL, useBlobText } from "../hooks/useBlob";
import type { Node } from "../api/types";
import { handleLinkClick, transformHTML } from "./FileView";

/* What actually changed between one version of a file and the one before
   it: a line diff, computed in the browser from the two blobs the history
   response already named. Mounted only once a row is expanded — nothing
   here fetches until then. */

function basename(path: string): string {
  return path.slice(path.lastIndexOf("/") + 1);
}

// Both versions, side by side, for the cases we cannot diff.
function Downloads({ apiBase, path, prev, cur }: { apiBase: string; path: string; prev: string; cur: string }) {
  const name = basename(path);
  return (
    <span className="dv-dl">
      <a href={blobURL(apiBase, prev, name, true)}>download previous</a>
      <a href={blobURL(apiBase, cur, name, true)}>download this version</a>
    </span>
  );
}

export function DiffView({
  apiBase,
  path,
  prev,
  cur,
}: {
  apiBase: string;
  path: string;
  prev: string; // sha of the previous version of this path
  cur: string; // sha of the version this row describes
}) {
  const a = useBlobText(apiBase, prev, true);
  const b = useBlobText(apiBase, cur, true);
  const ready = a.data?.kind === "text" && b.data?.kind === "text";
  const result = useMemo(
    () =>
      a.data?.kind === "text" && b.data?.kind === "text"
        ? diffText(a.data.text, b.data.text)
        : null,
    [a.data, b.data],
  );

  if (a.error || b.error) {
    return <div className="dv dv-msg">Could not load one of the versions.</div>;
  }
  if (!a.data || !b.data) return <div className="dv dv-msg">Loading changes…</div>;
  if (!ready) {
    const tooLarge = a.data.kind === "too-large" || b.data.kind === "too-large";
    return (
      <div className="dv dv-msg">
        {tooLarge ? "Too large to diff — download to compare." : "Binary file — no diff available."}
        <Downloads apiBase={apiBase} path={path} prev={prev} cur={cur} />
      </div>
    );
  }
  const { lines, add, del } = result!;
  return (
    <div className="dv">
      <div className="dv-head">
        <span className="dv-stat">
          <span className="dv-add">+{add}</span> <span className="dv-del">−{del}</span>
        </span>
        {add === 0 && del === 0 && <span className="dv-same">No line changes</span>}
      </div>
      <div className="dv-body">
        {lines.map((l, i) => (
          <div key={i} className={"dv-line dv-" + (l.op === "=" ? "ctx" : l.op === "+" ? "ins" : "rm")}>
            <span className="dv-n">{l.an ?? ""}</span>
            <span className="dv-n">{l.bn ?? ""}</span>
            <span className="dv-mark">{l.op === "=" ? " " : l.op}</span>
            <span className="dv-text">{l.line || " "}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

/* The +N −M of one change, for a row that wants the size of an edit without
   its lines. Shares DiffView's blob cache, so opening the diff afterwards
   costs nothing. `base` is diffBase's answer: "" counts every line as added
   (a first version), undefined counts nothing (a predecessor nobody named). */
export function useDiffStat(apiBase: string, base: string | undefined, cur: string | undefined, enabled: boolean) {
  const a = useBlobText(apiBase, base, enabled && !!base);
  const b = useBlobText(apiBase, cur, enabled && base !== undefined);
  return useMemo(() => {
    if (base === undefined || b.data?.kind !== "text") return null;
    if (!base) return diffText("", b.data.text);
    if (a.data?.kind !== "text") return null;
    return diffText(a.data.text, b.data.text);
  }, [a.data, b.data, base]);
}

export function DiffStat({ add, del }: { add: number; del: number }) {
  return (
    <span className="hstat">
      {add > 0 && <span className="dv-add">+{add}</span>}
      {del > 0 && <span className="dv-del">−{del}</span>}
    </span>
  );
}

const TABS = [
  ["changes", "Changes"],
  ["rendered", "Rendered"],
  ["full", "Full file"],
] as const;
type Tab = (typeof TABS)[number][0];

/* The diff side panel's body: the same change three ways. Rendered asks the
   hub to render that exact version (markdown) or shows it as an image;
   everything else has no rendered form and says so. */
export function DiffTabs({
  apiBase,
  path,
  kind,
  prev,
  cur,
  flatFiles,
  projectId,
  onOpenFile,
}: {
  apiBase: string;
  path: string;
  kind: string;
  prev?: string;
  cur?: string; // absent for a delete: the panel then shows what was removed
  flatFiles: Node[];
  projectId?: string;
  onOpenFile: (path: string) => void;
}) {
  const [tab, setTab] = useState<Tab>("changes");
  const shown = cur || prev;
  return (
    <div className="dtabs">
      <div className="dtabs-bar" role="tablist">
        {TABS.map(([k, label]) => (
          <button key={k} type="button" role="tab" aria-selected={tab === k} onClick={() => setTab(k)}>
            {label}
          </button>
        ))}
      </div>
      <div className="dtabs-body">
        {tab === "changes" &&
          (cur && prev ? (
            <DiffView apiBase={apiBase} path={path} prev={prev} cur={cur} />
          ) : (
            <div className="dv dv-msg">
              {!cur
                ? "This change deleted the file."
                : diffBase(kind, prev) === ""
                  ? "First version — nothing to compare against."
                  : "Earlier version not loaded — nothing to compare against here."}
            </div>
          ))}
        {tab === "rendered" && shown && (
          <Rendered apiBase={apiBase} path={path} sha={shown} flatFiles={flatFiles} projectId={projectId} onOpenFile={onOpenFile} />
        )}
        {tab === "full" && shown && <FullFile apiBase={apiBase} sha={shown} />}
      </div>
    </div>
  );
}

/* The same string transform and delegated link handling as the file page's
   ?v= view, so links and [[wikilinks]] in a past version resolve against the
   file's path and open in-app. Like that view, relative images point at the
   files as they are now — a version pins only this file's own bytes. */
function Rendered({
  apiBase,
  path,
  sha,
  flatFiles,
  projectId,
  onOpenFile,
}: {
  apiBase: string;
  path: string;
  sha: string;
  flatFiles: Node[];
  projectId?: string;
  onOpenFile: (path: string) => void;
}) {
  const md = /\.(md|markdown)$/i.test(path);
  const img = /\.(png|jpe?g|gif|webp|svg)$/i.test(path);
  const { data, error } = useQuery({
    queryKey: ["render", apiBase, path, sha],
    queryFn: () => getJSON<{ html: string }>(apiBase + "render?path=" + encodeURIComponent(path) + "&sha=" + sha),
    enabled: md,
    retry: false,
  });
  const html = useMemo(
    () => (data ? transformHTML(data.html, path, apiBase, flatFiles, projectId) : ""),
    [data, path, apiBase, flatFiles, projectId],
  );
  if (img) return <img className="dtabs-img" alt={basename(path)} src={blobURL(apiBase, sha, basename(path))} />;
  if (!md) return <div className="dv dv-msg">No rendered view for this file type — see Full file.</div>;
  if (error) return <div className="dv dv-msg">Could not render this version.</div>;
  if (!data) return <div className="dv dv-msg">Rendering…</div>;
  return (
    <div
      className="md dtabs-md"
      dangerouslySetInnerHTML={{ __html: html }}
      onClick={(e) => handleLinkClick(e, path, onOpenFile)}
    />
  );
}

function FullFile({ apiBase, sha }: { apiBase: string; sha: string }) {
  const t = useBlobText(apiBase, sha, true);
  if (t.error) return <div className="dv dv-msg">Could not load this version.</div>;
  if (!t.data) return <div className="dv dv-msg">Loading…</div>;
  if (t.data.kind !== "text")
    return <div className="dv dv-msg">{t.data.kind === "too-large" ? "Too large to show." : "Binary file."}</div>;
  return (
    <div className="dv">
      <div className="dv-body">
        {t.data.text.split("\n").map((l, i) => (
          <div key={i} className="dv-line dv-ctx">
            <span className="dv-n">{i + 1}</span>
            <span className="dv-text">{l || " "}</span>
          </div>
        ))}
      </div>
    </div>
  );
}
