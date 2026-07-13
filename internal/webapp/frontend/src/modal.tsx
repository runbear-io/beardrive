import { useEffect, useRef, useSyncExternalStore } from "react";

// In-app modal prompt/confirm replacing native prompt()/confirm().
// Imperative promise-based API (awaitable from event handlers, like the
// classic app); <ModalHost/> (mounted once in App) renders the active one.

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
  return m.kind === "prompt" ? <PromptModal m={m} /> : <ConfirmModal m={m} />;
}

function close() {
  emit(null);
}

function PromptModal({ m }: { m: Prompt }) {
  const input = useRef<HTMLInputElement>(null);
  const done = (v: string | null) => {
    close();
    m.resolve(v);
  };
  const ok = () => done(input.current!.value.trim() || null);
  useEffect(() => {
    input.current!.focus();
    input.current!.select();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") done(null);
      if (e.key === "Enter") ok();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return (
    <div className="modal-back" onClick={(e) => e.target === e.currentTarget && done(null)}>
      <div className="modal">
        <h3>{m.title}</h3>
        <label className="modal-label">{m.label}</label>
        <input className="modal-input" type="text" autoComplete="off" defaultValue={m.value} ref={input} />
        <div className="modal-actions">
          <button className="ai-btn" onClick={() => done(null)}>
            Cancel
          </button>
          <button className="pbtn" onClick={ok}>
            {m.okLabel}
          </button>
        </div>
      </div>
    </div>
  );
}

function ConfirmModal({ m }: { m: Confirm }) {
  const okBtn = useRef<HTMLButtonElement>(null);
  const done = (v: boolean) => {
    close();
    m.resolve(v);
  };
  useEffect(() => {
    okBtn.current!.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") done(false);
      if (e.key === "Enter") done(true);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return (
    <div className="modal-back" onClick={(e) => e.target === e.currentTarget && done(false)}>
      <div className="modal">
        <h3>{m.title}</h3>
        <p className="modal-msg">{m.message}</p>
        <div className="modal-actions">
          <button className="ai-btn" onClick={() => done(false)}>
            Cancel
          </button>
          <button className={m.danger ? "danger-btn" : "pbtn"} onClick={() => done(true)} ref={okBtn}>
            {m.confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
