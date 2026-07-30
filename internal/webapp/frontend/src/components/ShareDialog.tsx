import { useRef, useState } from "react";
import { api } from "../api/http";
import { Button } from "@/components/ui/button";
import { copyText } from "../util";
import { toast } from "../toast";
import { expiryLabel } from "./SharesTable";
import type { ShareInfo } from "../api/types";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";

/* Expiry is offered AFTER minting, never before: the one-click share is the
   flow this product is fast at, and ~90% of links are permanent. PATCHing the
   token we just handed out also keeps the URL already on the clipboard valid —
   minting with a TTL would create a second link, since ShareDB.Create only
   reuses permanent ones. */
const EXPIRY_PRESETS = [
  { value: "", label: "Never" },
  { value: "24h", label: "In 24 hours" },
  { value: "168h", label: "In 7 days" },
  { value: "720h", label: "In 30 days" },
];

/* A clear, explicitly-public share confirmation: warns that anyone with the
   link can view, and offers copy / open / expiry. The title says "Public
   link", not "created": ShareDB.Create hands back the file's existing live
   link when there is one, so a second Share click is not a second link.

   Revoke lives on the ShareBanner, not here: this modal is a transient
   handoff of the URL, the banner is what is still on the file after it
   closes. Two Revokes for one link is a destructive control shown twice. */
export function ShareDialog({
  url,
  copied,
  onClose,
}: {
  url: string;
  copied: boolean;
  onClose: () => void;
}) {
  const token = url.split("/s/")[1];
  // The dialog only ever opens on a freshly minted link, which is permanent.
  const [preset, setPreset] = useState("");
  const [expires, setExpires] = useState<string | undefined>();
  const [busy, setBusy] = useState(false);
  // The expiry control now sits above the buttons, so Radix's "focus the
  // first tabbable child" would land on it. Copy link keeps the focus: the
  // dialog exists to hand over a URL, expiry is the exception.
  const copyRef = useRef<HTMLButtonElement>(null);

  async function setExpiry(next: string) {
    const prev = preset;
    setPreset(next);
    setBusy(true);
    try {
      const s = await api<ShareInfo>("PATCH", "/api/shares/" + token, { expires_in: next });
      setExpires(s.expires);
    } catch (e) {
      // Never leave the control claiming a lifetime the server didn't take.
      toast((e as Error).message, true);
      setPreset(prev);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent
        className="modal"
        showCloseButton={false}
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          copyRef.current?.focus();
        }}
      >
        <DialogTitle asChild>
          <h3>Public link</h3>
        </DialogTitle>
        <p>
          <b>Anyone with this link can view this file</b> — no account needed. It always shows the
          latest version until it expires or you revoke it.
        </p>
        <div className="modal-url">{url}</div>
        <div className="modal-expiry">
          <label htmlFor="share-expiry">Expires</label>
          <select
            id="share-expiry"
            value={preset}
            disabled={busy}
            onChange={(e) => setExpiry(e.target.value)}
          >
            {EXPIRY_PRESETS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
          <span className="modal-expiry-note">{expiryLabel(expires)}</span>
        </div>
        <div className="modal-actions">
          <Button
            ref={copyRef}
            variant="primary"
            onClick={() =>
              copyText(url).then((ok) => toast(ok ? "Copied." : "Select and copy the link above."))
            }
          >
            {copied ? "Copied ✓" : "Copy link"}
          </Button>
          <Button variant="subtle" onClick={() => window.open(url, "_blank")}>
            Open
          </Button>
          <Button variant="subtle" onClick={onClose}>
            Done
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
