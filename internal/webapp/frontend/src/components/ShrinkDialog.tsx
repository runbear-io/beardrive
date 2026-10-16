import { useState, useSyncExternalStore } from "react";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog";
import type { ShrinkAsk } from "../lib/sharedfile";
import { humanSize } from "../util";
import { Icon } from "./shell";

/* The empty-save guard's question, as a dialog that blocks the editor until
   it is answered. Keeping the saved version is the default: Escape, the
   overlay and the focused button all keep it, and only "Save anyway" writes
   the shrunken document. One question at a time — sharedfile.ts never asks a
   second while the first is open. */

type Ask = ShrinkAsk & { path: string };

let current: Ask | null = null;
let listeners: Array<() => void> = [];
const emit = (next: Ask | null) => {
  current = next;
  listeners.forEach((l) => l());
};

export function askShrink(path: string, ask: ShrinkAsk) {
  emit({ ...ask, path });
}

export function ShrinkDialogHost() {
  const a = useSyncExternalStore(
    (l) => {
      listeners.push(l);
      return () => {
        listeners = listeners.filter((x) => x !== l);
      };
    },
    () => current,
  );
  const [busy, setBusy] = useState(false);
  if (!a) return null;
  const name = a.path.split("/").pop() || a.path;
  const run = async (f: () => Promise<void>) => {
    setBusy(true);
    try {
      await f();
    } finally {
      setBusy(false);
      emit(null);
    }
  };
  const keep = () => void run(a.keep);
  return (
    <Dialog open onOpenChange={(open) => !open && !busy && keep()}>
      <DialogContent className="modal shrink-dialog" showCloseButton={false} id="shrink-dialog">
        <div className="sd-head">
          <span className="sd-icon" aria-hidden="true">
            <Icon name="shield" />
          </span>
          <DialogTitle asChild>
            <h3>{a.now === 0 ? "This save would erase the note" : "This save would cut most of the note"}</h3>
          </DialogTitle>
        </div>
        <DialogDescription className="modal-msg">
          {name} would go from {a.was.toLocaleString()} B to {a.now.toLocaleString()} B. This usually means the
          editor lost its connection before the content loaded. Nothing has been written yet.
        </DialogDescription>
        <div className="sd-compare">
          <div>
            <span>Saved version</span>
            <b className="sd-ok">{humanSize(a.was)}</b>
          </div>
          <div>
            <span>Your editor</span>
            <b className="sd-bad">{a.now === 0 ? "0 B · empty" : humanSize(a.now)}</b>
          </div>
        </div>
        <div className="modal-actions sd-actions">
          <Button id="shrink-keep" variant="primary" autoFocus disabled={busy} onClick={keep}>
            Keep saved version
          </Button>
          <Button id="shrink-confirm" variant="danger" disabled={busy} onClick={() => void run(a.confirm)}>
            Save anyway
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
