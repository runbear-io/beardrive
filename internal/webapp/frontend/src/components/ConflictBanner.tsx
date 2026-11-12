import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { postJSON, putText } from "../api/http";
import type { Conflict } from "../lib/conflict";
import { fetchBlobText, fetchFileHeadSha, fileURLFor, textQueryOptions } from "../hooks/useBlob";
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
  const [viewedCopy, setViewedCopy] = useState<{ url: string; sha: string }>();
  const copyURL = apiBase && conflictPath ? fileURLFor(apiBase, conflictPath) : "";
  const viewedCopySha = viewedCopy?.url === copyURL ? viewedCopy.sha : undefined;
  useEffect(() => {
    setViewedCopy(undefined);
    setConfirmRemove(false);
    if (!copyURL) return;
    let active = true;
    void fetchFileHeadSha(copyURL).then((head) => {
      if (active && head.exists) {
        setViewedCopy((current) => current?.url === copyURL ? current : { url: copyURL, sha: head.sha });
      }
    }).catch(() => {
      // Opening Remove retries verification; confirmation never deletes blindly.
    });
    return () => { active = false; };
  }, [copyURL]);
  const who = conflict.device || "another device";
  const actionable = !!(apiBase && conflictPath);
  const compare = actionable && !!originalHref && showDiff;
  const orig = useQuery({ ...textQueryOptions(fileURLFor(apiBase || "", conflict.original)), enabled: compare });
  const copy = useQuery({ ...textQueryOptions(fileURLFor(apiBase || "", conflictPath || "")), enabled: compare });

  const invalidateChanges = () => {
    qc.invalidateQueries({ queryKey: ["tree", apiBase] });
    qc.invalidateQueries({ queryKey: ["history", apiBase] });
    qc.invalidateQueries({ queryKey: ["history-summary", apiBase] });
  };

  const removeCopy = async (sha: string) => {
    if (!apiBase || !conflictPath) throw new Error("No conflict copy selected.");
    await postJSON(apiBase + "remove", { path: conflictPath, if_match: sha });
    invalidateChanges();
  };

  const beginRemove = async () => {
    setBusy(true);
    try {
      if (!viewedCopySha) {
        const head = await fetchFileHeadSha(copyURL);
        if (!head.exists) throw new Error("The conflict copy no longer exists.");
        setViewedCopy({ url: copyURL, sha: head.sha });
      }
      setConfirmRemove(true);
    } catch (e) {
      toast("Could not verify the conflict copy: " + (e as Error).message, true);
    } finally {
      setBusy(false);
    }
  };

  const keep = async () => {
    if (!apiBase || !conflictPath) return;
    setBusy(true);
    try {
      const mine = await fetchBlobText(fileURLFor(apiBase, conflictPath));
      if (mine.kind !== "text") throw new Error("Only a text file can be kept from the browser — download it instead.");
      if (!mine.sha) throw new Error("Could not verify this conflict copy's version.");
      if (viewedCopySha && mine.sha !== viewedCopySha) {
        setViewedCopy({ url: copyURL, sha: mine.sha });
        qc.invalidateQueries({ queryKey: ["text", copyURL] });
        invalidateChanges();
        toast("The conflict copy changed since you opened it — review the updated version before keeping it", true);
        return;
      }
      // HEAD verifies every original, including binary and oversized files.
      const origHead = await fetchFileHeadSha(fileURLFor(apiBase, conflict.original));
      const res = await putText(
        apiBase + "upload/content?path=" + encodeURIComponent(conflict.original),
        mine.text,
        origHead.exists ? origHead.sha : undefined,
        true, // the user knowingly picked this version, even if it is shorter
        false,
        false,
        !origHead.exists,
      );
      qc.invalidateQueries({ queryKey: ["text"] });
      qc.invalidateQueries({ queryKey: ["render", apiBase, conflict.original] });
      if (res.merged || res.parked || (res.path && res.path !== conflict.original)) {
        invalidateChanges();
        toast(
          res.parked
            ? `The original changed meanwhile — saved as ${res.parked} and kept this copy`
            : "The original changed meanwhile — merged this version in and kept the copy",
          true,
        );
        return;
      }
      try {
        await removeCopy(mine.sha);
      } catch {
        invalidateChanges();
        toast("Updated the original, but kept the conflict copy because it changed meanwhile", true);
        return;
      }
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
      if (!viewedCopySha) throw new Error("Could not verify this conflict copy's version.");
      await removeCopy(viewedCopySha);
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
                {confirmRemove && viewedCopySha ? (
                  <>
                    <button className="ai-btn" disabled={busy} onClick={remove}>
                      Confirm remove
                    </button>
                    <button className="ai-btn" disabled={busy} onClick={() => setConfirmRemove(false)}>
                      Cancel
                    </button>
                  </>
                ) : (
                  <button className="ai-btn" disabled={busy} onClick={beginRemove}>
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
