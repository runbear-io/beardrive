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
   second while the first is open.

   The answers belong to the editor that asked, and that editor can go away
   while the question is on screen — another file opened, edit mode left. It
   withdraws its own ask as it is destroyed (withdrawShrink), which closes the
   dialog without running either answer; a different editor's ask replaces
   this one wholesale and never runs its callbacks. */

type Ask = ShrinkAsk & { path: string; from: ShrinkAsk };

let current: Ask | null = null;
let listeners: Array<() => void> = [];
const emit = (next: Ask | null) => {
  current = next;
  listeners.forEach((l) => l());
};

export function askShrink(path: string, ask: ShrinkAsk) {
  emit({ ...ask, path, from: ask });
}

// Close the dialog if it is still showing this ask; a later ask is untouched.
export function withdrawShrink(ask: ShrinkAsk) {
  if (current?.from === ask) emit(null);
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
  // Keyed to the ask, so an answer still running for one never disables the
  // buttons of the next.
  const [busyFor, setBusyFor] = useState<Ask | null>(null);
  if (!a) return null;
  const busy = busyFor === a;
  const name = a.path.split("/").pop() || a.path;
  const run = async (f: () => Promise<void>) => {
    setBusyFor(a);
    try {
      await f();
    } finally {
      setBusyFor((b) => (b === a ? null : b));
      if (current === a) emit(null);
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
