import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { postJSON, putText } from "../api/http";
import type { Conflict } from "../lib/conflict";
import { fetchBlobText, fileURLFor, textQueryOptions } from "../hooks/useBlob";
import { toast } from "../toast";
import { DiffView } from "./DiffView";
import { Icon } from "./shell";

/* A conflict copy is an ordinary file with an alarming name, and the README
   is the only place that ever explained it. This banner is what makes the
   guarantee visible where a user actually meets the file: two devices edited
   at once, nothing was dropped, and the other version is one click away.

   The name carries everything (see lib/conflict.ts), so the explanation needs
   no request. Comparing and resolving do, and only on demand: a diff of the
   two versions, keeping this one (written over the original, then the copy
   removed), or removing the copy. Reuses VersionBanner's .vbanner styles:
   same shape, same job. */
export function ConflictBanner(props: {
  conflict: Conflict;
  apiBase?: string;
  // The path of this conflict copy — what Remove deletes and Keep reads.
  conflictPath?: string;
  canEdit?: boolean;
  /* The recovered original only when the project actually holds it:
     conflictName truncates a long base name before appending the suffix, so
     the name we recover can be one that never existed. The explanation is
     the point; the link is the bonus. */
  originalHref?: () => void;
  onResolved?: (targetPath: string) => void;
}) {
  const { conflict, apiBase, conflictPath, canEdit, originalHref, onResolved } = props;
  const qc = useQueryClient();
  const [showDiff, setShowDiff] = useState(false);
  const [busy, setBusy] = useState(false);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const who = conflict.device || "another device";
  const actionable = !!(apiBase && conflictPath);
  const compare = actionable && !!originalHref && showDiff;
  const orig = useQuery({ ...textQueryOptions(fileURLFor(apiBase || "", conflict.original)), enabled: compare });
  const copy = useQuery({ ...textQueryOptions(fileURLFor(apiBase || "", conflictPath || "")), enabled: compare });

  const removeCopy = async () => {
    if (!apiBase || !conflictPath) return;
    await postJSON(apiBase + "remove", { path: conflictPath });
    qc.invalidateQueries({ queryKey: ["tree", apiBase] });
    qc.invalidateQueries({ queryKey: ["history", apiBase] });
    qc.invalidateQueries({ queryKey: ["history-summary", apiBase] });
  };

  const keep = async () => {
    if (!apiBase || !conflictPath) return;
    setBusy(true);
    try {
      const mine = await fetchBlobText(fileURLFor(apiBase, conflictPath));
      if (mine.kind !== "text") throw new Error("Only a text file can be kept from the browser — download it instead.");
      // If-Match the original we are replacing, so an edit that lands there
      // meanwhile is merged rather than erased. With no original, create only.
      let base: string | undefined;
      if (originalHref) {
        const cur = await fetchBlobText(fileURLFor(apiBase, conflict.original));
        base = cur.kind === "text" ? cur.sha : undefined;
      }
      const res = await putText(
        apiBase + "upload/content?path=" + encodeURIComponent(conflict.original),
        mine.text,
        base,
        true, // the user picked this version knowingly, even if it is shorter
        false,
        false,
        !originalHref,
      );
      qc.invalidateQueries({ queryKey: ["text"] });
      qc.invalidateQueries({ queryKey: ["render", apiBase, conflict.original] });
      if (res.merged) {
        // Someone edited the original meanwhile: the file now holds both, so
        // the copy stays until a person has looked.
        qc.invalidateQueries({ queryKey: ["tree", apiBase] });
        qc.invalidateQueries({ queryKey: ["history", apiBase] });
        qc.invalidateQueries({ queryKey: ["history-summary", apiBase] });
        toast("The original changed meanwhile — merged this version in and kept the copy", true);
        return;
      }
      await removeCopy();
      toast("Kept this version and removed conflict copy");
      onResolved?.(conflict.original);
    } catch (e) {
      toast("Could not keep this version: " + (e as Error).message, true);
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!conflictPath) return;
    setBusy(true);
    try {
      await removeCopy();
      toast("Removed conflict copy");
      onResolved?.(originalHref ? conflict.original : conflictPath.split("/").slice(0, -1).join("/"));
    } catch (e) {
      toast("Could not remove the conflict copy: " + (e as Error).message, true);
    } finally {
      setBusy(false);
      setConfirmRemove(false);
    }
  };

  const o = orig.data;
  const c = copy.data;
  return (
    <>
      <div className="vbanner" role="status">
        <span className="vb-icon">
          <Icon name="alert" />
        </span>
        <div className="vb-text">
          <b>Conflict copy — a concurrent edit, preserved</b>
          <span>
            {who} edited this file at the same time as someone else on{" "}
            {conflict.when.toLocaleString()}. Rather than drop either version,
            beardrive kept that one here.{" "}
            {originalHref ? (
              <>The other version lives at <code>{conflict.original}</code></>
            ) : (
              <>The original file is no longer at its former path.</>
            )}
          </span>
        </div>
        {(originalHref || (actionable && canEdit)) && (
          <div className="vb-actions">
            {originalHref && (
              <button className="ai-btn" onClick={originalHref}>
                Open the other version
              </button>
            )}
            {actionable && originalHref && (
              <button className="ai-btn" aria-expanded={showDiff} onClick={() => setShowDiff((v) => !v)}>
                {showDiff ? "Hide diff" : "Compare"}
              </button>
            )}
            {actionable && canEdit && (
              <>
                <button className="ai-btn" disabled={busy} onClick={keep}>
                  Keep this version
                </button>
                {confirmRemove ? (
                  <>
                    <button className="ai-btn" disabled={busy} onClick={remove}>
                      Confirm remove
                    </button>
                    <button className="ai-btn" disabled={busy} onClick={() => setConfirmRemove(false)}>
                      Cancel
                    </button>
                  </>
                ) : (
                  <button className="ai-btn" disabled={busy} onClick={() => setConfirmRemove(true)}>
                    Remove copy
                  </button>
                )}
              </>
            )}
          </div>
        )}
      </div>
      {compare &&
        apiBase &&
        conflictPath &&
        (orig.isError || copy.isError ? (
          <div className="dv dv-msg">Could not load one of the versions.</div>
        ) : !o || !c ? (
          <div className="dv dv-msg">Loading changes…</div>
        ) : o.kind === "text" && c.kind === "text" && o.sha && c.sha ? (
          <DiffView apiBase={apiBase} path={conflictPath} prev={o.sha} cur={c.sha} />
        ) : (
          <div className="dv dv-msg">These versions cannot be compared here — open each to see them.</div>
        ))}
    </>
  );
}
