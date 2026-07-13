import { useSyncExternalStore } from "react";

// Transient toast, replacing blocking alert(). Imperative `toast()` from
// anywhere; <Toaster/> (mounted once in App) renders it.

type ToastState = { msg: string; err: boolean; shown: boolean };
let state: ToastState = { msg: "", err: false, shown: false };
let listeners: Array<() => void> = [];
let timer: ReturnType<typeof setTimeout> | undefined;

function emit(next: ToastState) {
  state = next;
  listeners.forEach((l) => l());
}

export function toast(msg: string, isErr = false) {
  emit({ msg, err: isErr, shown: true });
  clearTimeout(timer);
  timer = setTimeout(() => emit({ ...state, shown: false }), 3200);
}

export function Toaster() {
  const s = useSyncExternalStore(
    (l) => {
      listeners.push(l);
      return () => {
        listeners = listeners.filter((x) => x !== l);
      };
    },
    () => state,
  );
  return (
    <div id="toast" className={s.shown ? "show" + (s.err ? " err" : "") : ""}>
      {s.msg}
    </div>
  );
}
