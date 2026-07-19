import { api } from "../api/http";
import { Button } from "@/components/ui/button";
import { copyText } from "../util";
import { toast } from "../toast";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";

/* A clear, explicitly-public share confirmation: warns that anyone with the
   link can view, and offers copy / open / revoke. */
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
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="modal" showCloseButton={false}>
        <DialogTitle asChild>
          <h3>Public link created</h3>
        </DialogTitle>
        <p>
          <b>Anyone with this link can view this file</b> — no account needed. It always shows the
          latest version until you revoke it.
        </p>
        <div className="modal-url">{url}</div>
        <div className="modal-actions">
          <Button
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
          <Button
            variant="subtle" className="ai-del"
            onClick={async () => {
              try {
                await api("DELETE", "/api/shares/" + token);
                toast("Link revoked — it no longer works.");
                onClose();
              } catch (e) {
                toast((e as Error).message, true);
              }
            }}
          >
            Revoke
          </Button>
          <Button variant="subtle" onClick={onClose}>
            Done
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
