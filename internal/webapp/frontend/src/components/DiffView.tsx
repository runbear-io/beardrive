import { useMemo } from "react";
import { diffText } from "../lib/diff";
import { blobURL, useBlobText } from "../hooks/useBlob";

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
