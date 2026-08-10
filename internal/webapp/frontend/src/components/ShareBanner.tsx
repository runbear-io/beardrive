import { Button } from "@/components/ui/button";
import type { ShareInfo } from "../api/types";
import { copyText } from "../util";
import { toast } from "../toast";
import { Icon } from "./shell";
import { OPENS_NOTE, revokeShare, shareDetail } from "./SharesTable";

/* A file that is publicly reachable says so while you are reading it. The
   Share dialog used to be the only place the link — and its Revoke button —
   existed, so once it closed the undo lived three clicks away in the org
   panel while the action was one click away here.

   Anyone with read sees the banner (a member should know the folder they
   rely on is exposed); Revoke is offered where the Share button already is,
   and the server checks again anyway.

   Same decision, one surface wider: project Settings shows a read-only member
   every active public link and who created it (BEA-69 — the owner's call,
   benchmarked against Google Drive showing a viewer who has access). Neither
   this banner nor that list is a leak to hide later; both routes stay
   PermRead. */
export function ShareBanner({
  shares,
  canRevoke,
  onChanged,
}: {
  shares: ShareInfo[];
  canRevoke: boolean;
  onChanged: () => void;
}) {
  if (shares.length === 0) return null;
  return (
    <div className="share-banner" role="status">
      <div className="sb-head">
        <Icon name="share" />
        <b>Publicly shared</b>
        <span className="sb-count">
          {shares.length} active link{shares.length > 1 ? "s" : ""}
        </span>
      </div>
      {/* Same words as the Share dialog: it is what the user already read. */}
      <p className="sb-note">
        <b>Anyone with this link can view this file</b> — no account needed. It always shows the
        latest version until you revoke it.
        {/* Same gate as the settings table: say nothing about opens on a hub
            that does not measure them. */}
        {shares.some((s) => s.opens !== undefined) && <> {OPENS_NOTE}</>}
      </p>
      {shares.map((s) => (
        <div className="sb-link" key={s.token}>
          <span className="sb-url mono" title={s.url}>
            {s.url}
          </span>
          <span className="sb-meta">{shareDetail(s, false)}</span>
          <span className="sb-actions">
            <Button
              variant="subtle"
              onClick={() =>
                copyText(s.url).then((ok) => toast(ok ? "Copied." : "Select and copy the link."))
              }
            >
              Copy link
            </Button>
            <Button variant="subtle" onClick={() => window.open(s.url, "_blank")}>
              Open
            </Button>
            {canRevoke && (
              <button
                className="ai-del"
                aria-label={`Revoke the share of ${s.path}`}
                onClick={() => revokeShare(s, onChanged)}
              >
                Revoke
              </button>
            )}
          </span>
        </div>
      ))}
    </div>
  );
}
