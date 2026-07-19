import { useRef, useSyncExternalStore } from "react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogTitle,
} from "@/components/ui/dialog";

// In-app modal prompt/confirm replacing native prompt()/confirm().
// Imperative promise-based API (awaitable from event handlers, like the
// classic app); <ModalHost/> (mounted once in App) renders the active one
// inside a Radix Dialog, which owns Escape/overlay dismissal and focus.

type Prompt = {
  kind: "prompt";
  title: string;
  label: string;
  value: string;
  okLabel: string;
  resolve: (v: string | null) => void;
};
type Confirm = {
  kind: "confirm";
  title: string;
  message: string;
  confirmLabel: string;
  danger: boolean;
  resolve: (v: boolean) => void;
};
type Modal = Prompt | Confirm;

let current: Modal | null = null;
let listeners: Array<() => void> = [];
function emit(next: Modal | null) {
  current = next;
  listeners.forEach((l) => l());
}

export function modalPrompt(
  title: string,
  label: string,
  value = "",
  okLabel = "OK",
): Promise<string | null> {
  return new Promise((resolve) =>
    emit({ kind: "prompt", title, label, value, okLabel, resolve }),
  );
}

export function modalConfirm(
  title: string,
  message: string,
  confirmLabel = "Confirm",
  danger = false,
): Promise<boolean> {
  return new Promise((resolve) =>
    emit({ kind: "confirm", title, message, confirmLabel, danger, resolve }),
  );
}

export function ModalHost() {
  const m = useSyncExternalStore(
    (l) => {
      listeners.push(l);
      return () => {
        listeners = listeners.filter((x) => x !== l);
      };
    },
    () => current,
  );
  if (!m) return null;

  const cancel = () => {
    emit(null);
    if (m.kind === "prompt") m.resolve(null);
    else m.resolve(false);
  };

  return (
    <Dialog open onOpenChange={(open) => !open && cancel()}>
      <DialogContent className="modal" showCloseButton={false}>
        {m.kind === "prompt" ? <PromptBody m={m} /> : <ConfirmBody m={m} />}
      </DialogContent>
    </Dialog>
  );
}

function PromptBody({ m }: { m: Prompt }) {
  const input = useRef<HTMLInputElement>(null);
  const done = (v: string | null) => {
    emit(null);
    m.resolve(v);
  };
  const ok = () => done(input.current!.value.trim() || null);
  return (
    <>
      <DialogTitle asChild>
        <h3>{m.title}</h3>
      </DialogTitle>
      <label className="modal-label">{m.label}</label>
      <input
        className="modal-input"
        type="text"
        autoComplete="off"
        defaultValue={m.value}
        ref={input}
        autoFocus
        onFocus={(e) => e.currentTarget.select()}
        onKeyDown={(e) => e.key === "Enter" && ok()}
      />
      <div className="modal-actions">
        <Button variant="subtle" onClick={() => done(null)}>
          Cancel
        </Button>
        <Button variant="primary" onClick={ok}>
          {m.okLabel}
        </Button>
      </div>
    </>
  );
}

function ConfirmBody({ m }: { m: Confirm }) {
  const done = (v: boolean) => {
    emit(null);
    m.resolve(v);
  };
  return (
    <>
      <DialogTitle asChild>
        <h3>{m.title}</h3>
      </DialogTitle>
      <p className="modal-msg">{m.message}</p>
      <div className="modal-actions">
        <Button variant="subtle" onClick={() => done(false)}>
          Cancel
        </Button>
        <Button variant={m.danger ? "danger" : "primary"} onClick={() => done(true)} autoFocus>
          {m.confirmLabel}
        </Button>
      </div>
    </>
  );
}
